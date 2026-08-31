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
	"path"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
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
	// 二轮审校 #17：owner 级 API（命令/SSE/宿主注册表）无认证——非回环监听必须显式豁免
	allowRemote := flag.Bool("allow-remote", false, "allow non-loopback listen (owner APIs are unauthenticated)")
	// 开发者模式（M1 v1.8）：debug 日志级别 + /v1/debug 只读端点 + UI 调试面板。
	// 主线排障入口前置——定位手段先于功能面铺开。
	dev := flag.Bool("dev", false, "developer mode: debug logs + read-only /v1/debug endpoints + UI debug panel")
	flag.Parse()

	// 信号处理必须先于一切：listening 日志出现后 ST 立即投递 SIGINT，
	// 注册晚于日志会出现"信号走默认动作 → 非零码退出"的竞态（CI 双核实证）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 日志级别：-dev 下放开 debug（轮链路/分发/命令各环节的 ids 全程可查）；
	// 常规模式维持 info——debug 埋点在级别门控下零输出。
	logLevel := new(slog.LevelVar)
	if *dev {
		logLevel.Set(slog.LevelDebug)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	if host, _, err := net.SplitHostPort(*addr); err == nil {
		// 复审 #4：空 host（":7420"）与通配地址 = 全接口监听，不是回环——
		// 逐项字符串白名单会把空 host 误判为回环，绕过远程监听门禁。
		ip := net.ParseIP(host)
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if !loopback && !*allowRemote {
			logger.Error("拒绝非回环监听（owner/harness API 无认证；个人版本地形态）",
				"addr", *addr, "豁免方式", "显式传 -allow-remote 并自行承担暴露面")
			os.Exit(1)
		}
		if !loopback {
			logger.Warn("非回环监听已豁免：API 无认证，请确保处于受信网络", "addr", *addr)
		}
	}

	// 二轮审校 #19：数据目录 owner-only（组/其他不可读——事件日志含全部讨论内容）
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		logger.Error("mkdir data failed", "dir", *dataDir, "err", err)
		os.Exit(1)
	}
	// 复审 #17：既有目录权限补收紧（MkdirAll 只在新建时生效；忽略不支持的平台）
	if err := os.Chmod(*dataDir, 0o700); err != nil {
		logger.Warn("数据目录权限收紧失败（POSIX 语义平台外为 no-op）", "dir", *dataDir, "err", err)
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

	// 预算上限：引擎 admission 与调试面水位共用同一份配置（口径不得分叉）。
	budgetLimits := contextx.Limits{ // M1 防失控默认（宽裕）：预算只作 admission 不进排序（RFC-0003）
		MaxRounds: 500, MaxUtterances: 1500, MaxTokens: 20_000_000,
	}

	// 引擎指针先于装配声明：httpapi 调试面的座位快照经其惰性读取
	// （引擎在宿主扫描完成后才构造，调试端点不得阻塞启动序）。
	var enginePtr atomic.Pointer[room.Engine]

	mux := httpapi.New(httpapi.Deps{
		SVC:         svc,
		Reader:      store,
		Hub:         hub,
		Actor:       room.Actor{ParticipantID: "par_owner", Kind: "human"}, // 本地 owner（ADR-0009）
		Harness:     harnessRegistry,
		ProbeRunner: harness.NewHostRunner(),
		Logger:      logger,
		Dev:         *dev,
		Budget:      budgetLimits,
		Outbox:      store,
		Seats: func() []room.AgentSeat {
			if engine := enginePtr.Load(); engine != nil {
				return engine.Seats()
			}
			return nil
		},
	})

	// 先 Listen 再 Serve：暴露实际绑定地址（支持 -addr 127.0.0.1:0；裸 ":port"
	// 是全接口监听，须经 -allow-remote 豁免——复审 #4）。
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	logger.Info("mosaic-server listening", "addr", ln.Addr().String())
	if *dev {
		logger.Info("开发者模式已启用", "debug日志", "已放开", "调试端点", "/v1/debug/rooms/{room_id}/state|events", "UI面板", "已注入")
	}

	// 房间引擎 + 提交后分发：echo 恒在（conformance 基线）；宿主扫描完成后，
	// 已启用的真实适配器（如 codex）动态注册并加入座位；此后周期 resync——
	// 运行时经 /v1/harness/*/enable 启用的适配器无需重启即可入座（二轮审校 #1）。
	// 分发循环晚于扫描启动——扫描期间的命令照常受理，事件在分发启动后依序投递（outbox 不丢）。
	go func() {
		<-scanDone
		hostRunner := harness.NewHostRunner()
		// agent 工作目录隔离（二轮审校 #18）：不给服务工作目录与 owner 文件——
		// 专用空目录 + -C + read-only 沙箱三层约束。
		// 复审 #1：准备失败 fail closed（不再回退服务器 cwd——隔离失效胜过静默降级）。
		workRoot := filepath.Join(*dataDir, "agent-work")
		if err := os.MkdirAll(workRoot, 0o700); err != nil {
			logger.Error("agent work root 准备失败：codex 座位不注册（fail closed）", "dir", workRoot, "err", err)
			workRoot = ""
		}
		wslHomeCache := map[string]string{}
		syncSeats := func() []room.AgentSeat {
			seats := []room.AgentSeat{{
				ParticipantID: "par_echo",
				Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
			}}
			for _, exe := range harnessRegistry.EnabledList() {
				switch exe.Adapter {
				case "codex":
					profileID := "prof_codex_" + exe.Runtime
					if exe.Distro != "" {
						profileID += "_" + strings.ReplaceAll(exe.Distro, ".", "_")
					}
					cfg := codexadapter.Config{CodexPath: exe.Path, Timeout: 180 * time.Second}
					if harness.Runtime(exe.Runtime) == harness.RuntimeWSL {
						// WSL 运行面（复审 #2）：工作目录必须是发行版内 Linux 路径——
						// Windows 路径交给发行版内的 codex 即坏；HOME 同理在发行版内解析。
						home, ok := wslHomeCache[exe.Distro]
						if !ok {
							home = hostRunner.Home(ctx, harness.RuntimeWSL, exe.Distro)
							wslHomeCache[exe.Distro] = home
						}
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = home
						dir := path.Join(strings.TrimSuffix(home, "/"), ".mosaic", "agent-work", profileID)
						if err := hostRunner.MkdirAll(ctx, harness.RuntimeWSL, exe.Distro, dir); err != nil {
							logger.Error("codex WSL 工作目录准备失败：座位不注册（fail closed）", "profile", profileID, "err", err)
							continue
						}
						cfg.WorkDir = dir
					} else {
						// native：宿主侧专用空目录；任一失败即跳过座位（复审 #1：fail closed）
						if workRoot == "" {
							continue
						}
						dir := filepath.Join(workRoot, profileID)
						if err := os.MkdirAll(dir, 0o700); err != nil {
							logger.Error("codex 工作目录准备失败：座位不注册（fail closed）", "profile", profileID, "err", err)
							continue
						}
						cfg.WorkDir = dir
					}
					// 复审 #3：按 Profile 键登记——多 executable（native + WSL 两份 codex）
					// 各自成适配器实例，不再折叠到首个 "codex"。
					if err := supervisor.RegisterFor(profileID, codexadapter.New(cfg)); err != nil {
						logger.Warn("codex adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: "par_" + strings.TrimPrefix(profileID, "prof_"),
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "codex", DisplayName: "Codex"},
					})
				}
			}
			return seats
		}
		engine := room.NewEngine(room.EngineConfig{
			Store:  store,
			Reader: store,
			Agents: supervisor,
			Seats:  syncSeats(),
			Policy: attention.Policy{
				Mode:        "open_floor",
				MaxSpeakers: 3,
				Lambda:      0.30, // M1 默认；OQ-04 校准前可配（RFC-0003 §3.1.5）
				Weights:     attention.DefaultWeights,
			},
			Budget:   budgetLimits,
			Receipts: store,                      // Context Receipt 落库（RFC-0007）
			Claims:   store,                      // durable handoff（二轮审校 #9）
			OnDraft:  httpapi.DraftConsumer(hub), // DraftUpdate 安全子集 → SSE 瞬态帧
			Logger:   logger,
			Clock:    clock,
			Now:      time.Now,
			NewID:    newID,
			Tenant:   "ten_local",
		})
		enginePtr.Store(engine)
		engine.RecoverClaims() // 崩溃窗口重驱动：已声明未开轮的刺激（二轮审校 #9）
		// 周期 resync（二轮审校 #1）：运行时启用/禁用 → 座位与适配器热更新
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					engine.SetSeats(syncSeats())
				}
			}
		}()
		dispatcher := outbox.NewDispatcher(store, []outbox.Consumer{
			httpapi.HubConsumer(hub),
			engine,
		}, 20*time.Millisecond).WithLogger(logger)
		dispatcher.Run(ctx)
	}()

	// 复审 #20：请求读取期限——慢速滴灌 body 不得无限占用连接（大小上限之外的时限防线）。
	// SSE 为无 body GET，不受 ReadTimeout 影响；WriteTimeout 不设（SSE 长流按连接存活）。
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       65 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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
	// 引擎先于 supervisor 关停：取消在途轮 → 适配器经 ctx 击杀子进程组（防孤儿）。
	// 已提交事件构成可恢复状态（RFC-0003 3.4）；短暂宽限让击杀信号送达。
	if engine := enginePtr.Load(); engine != nil {
		engine.Close()
		time.Sleep(200 * time.Millisecond)
	}
	supervisor.Shutdown()
	logger.Info("mosaic-server stopped")
}
