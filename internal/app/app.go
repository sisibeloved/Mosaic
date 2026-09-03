// Package app：Mosaic 服务装配（cmd/mosaic-server 与桌面壳共用的进程内形态）。
// 产品形态是"本地单进程桌面应用"（D-2/D-3）——TCP 服务（mosaic-server）与
// Wails 壳（assets Handler 直连 mux）经同一装配获得引擎/分发/宿主层。
// 装配纪律沿用各复审裁定：数据面 owner-only、agent 工作根出数据目录、
// 座位 fail closed、 Authority 注入真实绑定地址。
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sisibeloved/Mosaic/apps/web"
	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/codex"
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/kimi"
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/minimax"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/storage/sqlite"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// Options 装配选项（零值字段走默认）。
type Options struct {
	// Addr TCP 监听地址；空串 = 无监听的进程内形态（桌面壳经 Handler() 直连）。
	Addr string
	// DataDir 数据目录（SQLite/注册表；权限 owner-only fail closed）。
	DataDir string
	// AgentWork agent 工作根（空 = 用户缓存目录；保持在数据目录之外——四轮复审 #2）。
	AgentWork string
	// Logger 日志器（空 = stdout JSON，级别由 Dev 决定）。
	Logger *slog.Logger
	// Dev 开发者模式（M1 v1.8）：debug 级别 + /v1/debug 端点 + UI 面板注入。
	Dev bool
	// ExtraOriginHosts 跨源写门的壳集成信任源（如 wails.localhost——浏览器对
	// Origin 不可伪造，该主机名只能来自本应用自带的 WebView 页面）。
	ExtraOriginHosts []string
	// UI 前端产物根（空 = apps/web 构建产物）。
	UI fs.FS
}

// Server 装配产物。
type Server struct {
	addr       string // 实际绑定地址（进程内形态为空）
	handler    http.Handler
	shutdown   func()
	storeClose func()
}

// Addr 实际绑定地址（进程内形态为空串）。
func (s *Server) Addr() string { return s.addr }

// Handler 进程内直连形态：Wails 资产服务器以此服务 API 与 SPA（同源调用）。
func (s *Server) Handler() http.Handler { return s.handler }

// Shutdown 排空并释放（引擎先于 supervisor：在途轮取消 → 子进程组击杀）。
func (s *Server) Shutdown(ctx context.Context) {
	if s.shutdown != nil {
		s.shutdown()
	}
	_ = ctx
	if s.storeClose != nil {
		s.storeClose()
	}
}

// Start 装配并启动。返回即已完成 Listen（TCP 形态）——ST 依赖 listening 日志后
// 立即投递信号，故信号 ctx 由调用方先行注册。
func Start(ctx context.Context, opts Options) (*Server, error) {
	logger := opts.Logger
	if logger == nil {
		level := new(slog.LevelVar)
		if opts.Dev {
			level.Set(slog.LevelDebug)
		}
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}

	// 二轮审校 #19：数据目录 owner-only；四轮复审 #6：收紧失败 fail closed。
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("app: mkdir data: %w", err)
	}
	if err := os.Chmod(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("app: chmod data (fail closed): %w", err)
	}
	store, err := sqlite.Open(filepath.Join(opts.DataDir, "mosaic.db"))
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}

	// Owner token（M2，四轮复审 #15 残留收口）：首启生成、0600 持久化，重启不变
	//（会话连续性）。写端点凭据；第一方客户端经 /v1/owner/bootstrap（跨源门保护）获取。
	ownerToken, err := loadOrCreateOwnerToken(filepath.Join(opts.DataDir, "owner-token"))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("app: owner token: %w", err)
	}

	// ID：时间有序前缀 + 随机后缀（uuidv7 语义）。
	newID := func(prefix string) string {
		var b [8]byte
		_, _ = rand.Read(b[:])
		return prefix + "_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
	}
	clock := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }

	// 引擎指针先于服务构造声明：create_room 缺省选人要读当时在席座位做名单
	// 快照（v1.24：引擎在扫描完成后创建——首开窗口内为 nil，物化为空名单，
	// 与"扫描完成前无席"的实态一致）。
	var enginePtr atomic.Pointer[room.Engine]

	svc := room.NewService(room.Config{
		Store:  store,
		Lister: store, // GET /v1/rooms 房间列表读路径
		Clock:  clock,
		NewID:  newID,
		Tenant: "ten_local", // 个人版单租户常量（ADR-0008 机制映射）
		Seats: func() []room.AgentSeat {
			if engine := enginePtr.Load(); engine != nil {
				return engine.Seats()
			}
			return nil
		},
	})

	supervisor := agent.NewSupervisor()
	if err := supervisor.Register(echo.Adapter{}); err != nil {
		store.Close()
		return nil, fmt.Errorf("app: register echo: %w", err)
	}

	hub := sse.NewHub()

	// 宿主层：启动自动扫描（探测失败不阻塞启动，注册表持久化合并）。
	harnessRegistry, err := harness.LoadOrCreate(filepath.Join(opts.DataDir, "harness-registry.json"))
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("app: harness registry: %w", err)
	}
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second) // 随主 ctx 取消
		defer cancel()
		scanOpts := harness.ScanOptions{IncludeWSL: goruntime.GOOS == "windows"}
		if err := harnessRegistry.Scan(scanCtx, harness.NewHostRunner(), harness.BuiltinProbes, scanOpts); err != nil {
			logger.Warn("harness scan partial failure", "err", err)
		}
		for _, exe := range harnessRegistry.List() {
			logger.Info("harness discovered",
				"adapter", exe.Adapter, "runtime", exe.Runtime, "distro", exe.Distro,
				"path", exe.Path, "version", exe.Version, "login", exe.Login)
		}
	}()

	// 预算上限：引擎 admission 与调试面水位共用同一份配置（口径不得分叉）。
	budgetLimits := contextx.Limits{
		MaxRounds: 500, MaxUtterances: 1500, MaxTokens: 20_000_000,
	}

	ui := opts.UI
	if ui == nil {
		ui = web.Dist()
	}

	deps := httpapi.Deps{
		SVC:              svc,
		Reader:           store,
		Hub:              hub,
		Actor:            room.Actor{ParticipantID: "par_owner", Kind: "human"}, // 本地 owner（ADR-0009）
		Harness:          harnessRegistry,
		ProbeRunner:      harness.NewHostRunner(),
		Logger:           logger,
		Dev:              opts.Dev,
		Budget:           budgetLimits,
		Outbox:           store,
		ExtraOriginHosts: opts.ExtraOriginHosts,
		OwnerToken:       ownerToken,
		UI:               ui,
		Seats: func() []room.AgentSeat {
			if engine := enginePtr.Load(); engine != nil {
				return engine.Seats()
			}
			return nil
		},
	}

	var ln net.Listener
	if opts.Addr != "" {
		// 先 Listen 再 Serve：暴露实际绑定地址（支持 -addr 127.0.0.1:0）。
		ln, err = net.Listen("tcp", opts.Addr)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("app: listen %s: %w", opts.Addr, err)
		}
		// 四轮复审 #15：跨源写门以"配置的回环 authority"判定——真实绑定地址注入。
		deps.Authority = ln.Addr().String()
	}
	mux := httpapi.New(deps)

	// 房间引擎 + 提交后分发：echo 恒在（conformance 基线）；宿主扫描完成后，
	// 已启用的真实适配器动态注册并加入座位；此后周期 resync（二轮审校 #1）。
	go func() {
		<-scanDone
		hostRunner := harness.NewHostRunner()
		// agent 工作目录隔离（二轮审校 #18 / 四轮复审 #2）：工作根在数据目录之外；
		// 复审 #1：准备失败 fail closed（不回退服务器 cwd）。
		workRoot := opts.AgentWork
		if workRoot == "" {
			if cache, err := os.UserCacheDir(); err == nil {
				workRoot = filepath.Join(cache, "mosaic", "agent-work")
			} else {
				workRoot = filepath.Join(os.TempDir(), "mosaic-agent-work")
			}
		}
		if err := os.MkdirAll(workRoot, 0o700); err != nil {
			logger.Error("agent work root 准备失败：agent 座位不注册（fail closed）", "dir", workRoot, "err", err)
			workRoot = ""
		}
		wslHomeCache := map[string]string{}
		// resolveWorkDir 解析座位工作目录（fail closed）。WSL 面返回发行版内
		// Linux 路径与 HOME（四轮复审 #4：HOME 非空且绝对路径才可用，无效跳过不缓存）。
		resolveWorkDir := func(exe harness.Executable, profileID string) (dir, wslHome string, ok bool) {
			if harness.Runtime(exe.Runtime) == harness.RuntimeWSL {
				home, cached := wslHomeCache[exe.Distro]
				if !cached {
					home = hostRunner.Home(ctx, harness.RuntimeWSL, exe.Distro)
				}
				if home == "" || !strings.HasPrefix(home, "/") {
					logger.Error("WSL HOME 解析无效（座位不注册，fail closed）",
						"adapter", exe.Adapter, "distro", exe.Distro, "home", home)
					return "", "", false
				}
				wslHomeCache[exe.Distro] = home
				dir = path.Join(strings.TrimSuffix(home, "/"), ".mosaic", "agent-work", profileID)
				if err := hostRunner.MkdirAll(ctx, harness.RuntimeWSL, exe.Distro, dir); err != nil {
					logger.Error("WSL 工作目录准备失败：座位不注册（fail closed）", "adapter", exe.Adapter, "profile", profileID, "err", err)
					return "", "", false
				}
				return dir, home, true
			}
			if workRoot == "" {
				return "", "", false
			}
			dir = filepath.Join(workRoot, profileID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				logger.Error("agent 工作目录准备失败：座位不注册（fail closed）", "adapter", exe.Adapter, "profile", profileID, "err", err)
				return "", "", false
			}
			return dir, "", true
		}
		syncSeats := func() []room.AgentSeat {
			seats := []room.AgentSeat{{
				ParticipantID: "par_echo",
				Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
			}}
			for _, exe := range harnessRegistry.EnabledList() {
				// 四轮复审 #3：身份基于注册表唯一 ID 派生；C 轨多实例并存不折叠。
				exeKey := sanitizeProfileKey(exe.ID)
				profileID := "prof_" + exe.Adapter + "_" + exeKey
				// 渠道标签随座位进 Profile（快照参与者视图展示用）；注册表口径：空值按 cli 处理。
				channel := exe.Channel
				if channel == "" {
					channel = harness.ChannelCLI
				}
				dir, wslHome, ok := resolveWorkDir(exe, profileID)
				if !ok {
					continue
				}
				switch exe.Adapter {
				case "codex":
					cfg := codex.Config{CodexPath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir, EvalModel: exe.EvalModel}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					// 四轮复审 #9：重登驱逐旧会话。
					if err := supervisor.RegisterFor(profileID, codex.New(cfg)); err != nil {
						logger.Warn("codex adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: "par_codex_" + exeKey,
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "codex", DisplayName: "Codex", Channel: channel},
					})
				case "kimi":
					// C 轨：第二个真实适配器（kimi -p stream-json + -S 会话恢复）。
					cfg := kimi.Config{KimiPath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir, EvalModel: exe.EvalModel}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					if err := supervisor.RegisterFor(profileID, kimi.New(cfg)); err != nil {
						logger.Warn("kimi adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: "par_kimi_" + exeKey,
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "kimi", DisplayName: "Kimi", Channel: channel},
					})
				case "minimax":
					// M3-1 观测基座：第三个真实适配器（mcode exec stream-json + --session 恢复，
					// 提示词走 stdin——无 argv 上限；三 agent 同房构成真实并行/竞争场景）。
					cfg := minimax.Config{McodePath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir, EvalModel: exe.EvalModel}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					if err := supervisor.RegisterFor(profileID, minimax.New(cfg)); err != nil {
						logger.Warn("minimax adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: "par_minimax_" + exeKey,
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "minimax", DisplayName: "MiniMax", Channel: channel},
					})
				}
			}
			return seats
		}
		engine := room.NewEngine(room.EngineConfig{
			Store:    store,
			Reader:   store,
			Agents:   supervisor,
			Seats:    syncSeats(),
			Budget:   budgetLimits,
			Receipts: store,
			Claims:   store,
			// OQ-A 主动开口静默期：零值即禁用（scheduleProactive 直接返回）——
			// v1.36 曾漏配此处，主动波从未排上（dogfood 实证），勿再省略。
			ProactiveSilence: 5 * time.Minute,
			OnDraft:          httpapi.DraftConsumer(hub),
			OnWaveSkip:       httpapi.WaveSkipConsumer(hub),
			Logger:           logger,
			Clock:            clock,
			Now:              time.Now,
			NewID:            newID,
			Tenant:           "ten_local",
		})
		enginePtr.Store(engine)
		engine.RecoverClaims() // 崩溃窗口重驱动（二轮审校 #9）
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

	var srv *http.Server
	var serveErr = make(chan error, 1)
	if ln != nil {
		// 复审 #20：读取期限——慢速滴灌 body 不得无限占用连接；SSE 无 body GET 不受影响。
		srv = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       65 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}

	srvAddr := ""
	if ln != nil {
		srvAddr = ln.Addr().String()
		logger.Info("mosaic-server listening", "addr", srvAddr)
	}
	if opts.Dev {
		logger.Info("开发者模式已启用", "debug日志", "已放开", "调试端点", "/v1/debug/rooms/{room_id}/state|events", "UI面板", "已注入")
	}

	server := &Server{
		addr:    srvAddr,
		handler: mux,
		shutdown: func() {
			if srv != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}
			// 引擎先于 supervisor 关停：在途轮取消 → 适配器经 ctx 击杀子进程组（防孤儿）。
			if engine := enginePtr.Load(); engine != nil {
				engine.Close()
				time.Sleep(200 * time.Millisecond)
			}
			supervisor.Shutdown()
		},
		storeClose: func() { _ = store.Close() },
	}

	go func() {
		// TCP 形态：进程内形态由调用方显式 Shutdown。
		select {
		case <-ctx.Done():
			server.Shutdown(context.Background())
		case err := <-serveErr:
			if err != nil {
				logger.Error("server error", "err", err)
				server.Shutdown(context.Background())
			}
		}
	}()
	return server, nil
}

// loadOrCreateOwnerToken 读写端凭据：存在即复用（重启不变，会话连续），否则生成
// 32 字节随机 hex 并以 0600 落盘（数据目录已 owner-only；同用户本机进程等效 owner，
// token 的对手面是跨源/rebinding 页面，不是本机进程）。
func loadOrCreateOwnerToken(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(raw)); tok != "" {
			return tok, nil
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate: %w", err)
	}
	tok := hex.EncodeToString(b[:])
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persist: %w", err)
	}
	return tok, nil
}

// sanitizeProfileKey 注册表 ID → 身份/目录名安全字符（四轮复审 #3）。
func sanitizeProfileKey(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if out := b.String(); out != "" {
		return out
	}
	return "x"
}
