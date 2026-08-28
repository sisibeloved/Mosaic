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
	"strconv"
	"syscall"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
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

	mux := httpapi.New(httpapi.Deps{
		SVC:    svc,
		Reader: store,
		Hub:    hub,
		Actor:  room.Actor{ParticipantID: "par_owner", Kind: "human"}, // 本地 owner（ADR-0009）
		Logger: logger,
	})

	// 先 Listen 再 Serve：暴露实际绑定地址（支持 -addr :0），ST 据此发现端口。
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	logger.Info("mosaic-server listening", "addr", ln.Addr().String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 房间引擎：M1 服务全部已创建房间（房间级 seats/策略随 M2 房间生命周期完善）
	engine := room.NewEngine(room.EngineConfig{
		Store:  store,
		Agents: supervisor,
		Seats: []room.AgentSeat{{
			ParticipantID: "par_echo",
			Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
		}},
		Clock:  clock,
		Now:    time.Now,
		NewID:  newID,
		Tenant: "ten_local",
	})

	// 提交后分发：outbox → SSE Hub + 房间引擎
	dispatcher := outbox.NewDispatcher(store, []outbox.Consumer{
		httpapi.HubConsumer(hub),
		engine,
	}, 20*time.Millisecond)
	go dispatcher.Run(ctx)

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
