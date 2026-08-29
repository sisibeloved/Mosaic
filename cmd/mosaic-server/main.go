// mosaic-server 是 Mosaic 个人版的进程入口（M1：命令 API + SSE 订阅 + 房间引擎闭环）。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/codexadapter"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/storage/sqlite"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7420", "listen address")
	dataDir := flag.String("data", "./data", "data directory (SQLite + runtime files)")
	flag.Parse()

	// 信号处理必须先于一切：listening 日志出现后 ST 立即投递 SIGINT，
	// 注册晚于日志会出现"信号走默认动作 → 非零码退出"的竞态（CI 双核实证）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		logger.Error("mkdir data failed", "dir", *dataDir, "err", err)
		os.Exit(1)
	}
	store, err := sqlite.Open(filepath.Join(*dataDir, "mosaic.db"))
	if err != nil {
		logger.Error("open store failed", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// ID：时间有序前缀 + 随机后缀（uuidv7 语义；正式 uuidv7 库随 M1 迁移框架引入）
	newID := func(prefix string) string {
		var b [8]byte
		_, _ = rand.Read(b[:])
		return prefix + "_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
	}
	clock := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }

	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  clock,
		NewID:  newID,
		Tenant: "ten_local", // 个人版单租户常量（ADR-0008 机制映射）
	})

	// 适配器注册（M1：echo conformance；native-codex 随适配器切片接入）
	supervisor := agent.NewSupervisor()
	if err := supervisor.Register(echo.Adapter{}); err != nil {
		logger.Error("register echo adapter failed", "err", err)
		os.Exit(1)
	}

	hub := sse.NewHub()

	// 宿主层：启动自动扫描（负责人要求：Windows 必须覆盖 WSL 内安装的 CLI）。
	// 扫描超时独立于服务可用性——探测失败不阻塞启动，注册表持久化合并。
	harnessRegistry, err := harness.LoadOrCreate(filepath.Join(*dataDir, "harness-registry.json"))
	if err != nil {
		logger.Error("harness registry failed", "err", err)
		os.Exit(1)
	}
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second) // 随主 ctx 取消：退出不被扫描拖住
		defer cancel()
		opts := harness.ScanOptions{IncludeWSL: goruntime.GOOS == "windows"}
		if err := harnessRegistry.Scan(scanCtx, harness.NewHostRunner(), harness.BuiltinProbes, opts); err != nil {
			logger.Warn("harness scan partial failure", "err", err)
		}
		for _, exe := range harnessRegistry.List() {
			logger.Info("harness discovered",
				"adapter", exe.Adapter, "runtime", exe.Runtime, "distro", exe.Distro,
				"path", exe.Path, "version", exe.Version, "login", exe.Login)
		}
	}()

	mux := httpapi.New(httpapi.Deps{
		SVC:         svc,
		Reader:      store,
		Hub:         hub,
		Actor:       room.Actor{ParticipantID: "par_owner", Kind: "human"}, // 本地 owner（ADR-0009）
		Harness:     harnessRegistry,
		ProbeRunner: harness.NewHostRunner(),
		Logger:      logger,
	})

	// 先 Listen 再 Serve：暴露实际绑定地址（支持 -addr :0），ST 据此发现端口。
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	logger.Info("mosaic-server listening", "addr", ln.Addr().String())

	// 房间引擎 + 提交后分发：echo 恒在（conformance 基线）；宿主扫描完成后，
	// 已启用的真实适配器（如 codex）动态注册并加入座位。分发循环晚于扫描启动——
	// 扫描期间的命令照常受理，事件在分发启动后依序投递（outbox 不丢）。
	go func() {
		<-scanDone
		seats := []room.AgentSeat{{
			ParticipantID: "par_echo",
			Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
		}}
		for _, exe := range harnessRegistry.EnabledList() {
			switch exe.Adapter {
			case "codex":
				if err := supervisor.Register(codexadapter.New(codexadapter.Config{
					CodexPath: exe.Path,
					Timeout:   180 * time.Second,
				})); err != nil {
					logger.Warn("codex adapter register failed", "path", exe.Path, "err", err)
					continue
				}
				profileID := "prof_codex_" + exe.Runtime
				if exe.Distro != "" {
					profileID += "_" + strings.ReplaceAll(exe.Distro, ".", "_")
				}
				seats = append(seats, room.AgentSeat{
					ParticipantID: "par_codex",
					Profile:       agent.Profile{ProfileID: profileID, Adapter: "codex", DisplayName: "Codex"},
				})
				logger.Info("agent seat ready", "adapter", "codex", "runtime", exe.Runtime, "path", exe.Path)
			}
		}
		engine := room.NewEngine(room.EngineConfig{
			Store:  store,
			Reader: store,
			Agents: supervisor,
			Seats:  seats,
			Policy: attention.Policy{
				Mode:        "open_floor",
				MaxSpeakers: 3,
				Lambda:      0.30, // M1 默认；OQ-04 校准前可配（RFC-0003 §3.1.5）
				Weights:     attention.DefaultWeights,
			},
			Budget: contextx.Limits{ // M1 防失控默认（宽裕）：预算只作 admission 不进排序（RFC-0003）
				MaxRounds: 500, MaxUtterances: 1500, MaxTokens: 20_000_000,
			},
			Receipts: store,                      // Context Receipt 落库（RFC-0007）
			OnDraft:  httpapi.DraftConsumer(hub), // DraftUpdate 安全子集 → SSE 瞬态帧
			Clock:    clock,
			Now:      time.Now,
			NewID:    newID,
			Tenant:   "ten_local",
		})
		dispatcher := outbox.NewDispatcher(store, []outbox.Consumer{
			httpapi.HubConsumer(hub),
			engine,
		}, 20*time.Millisecond)
		dispatcher.Run(ctx)
	}()

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	supervisor.Shutdown()
	logger.Info("mosaic-server stopped")
}
