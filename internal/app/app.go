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
	"encoding/json"
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
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/claude"
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/codex"
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/kimi"
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/minimax"
	"github.com/sisibeloved/Mosaic/internal/agent/adapter/zcode"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/agent/slowrun"
	"github.com/sisibeloved/Mosaic/internal/attach"
	"github.com/sisibeloved/Mosaic/internal/backup"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/dispatch"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/execctx"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
	mosaicruntime "github.com/sisibeloved/Mosaic/internal/runtime"
	"github.com/sisibeloved/Mosaic/internal/settings"
	"github.com/sisibeloved/Mosaic/internal/slot"
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
		logger = NewLogger(os.Stdout, level) // v1.49：人类可读时间（TextHandler）
	}

	// 二轮审校 #19：数据目录 owner-only；四轮复审 #6：收紧失败 fail closed。
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("app: mkdir data: %w", err)
	}
	if err := os.Chmod(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("app: chmod data (fail closed): %w", err)
	}
	// M4-0 恢复启动段：pending 标记在开库之前换库（活动句柄不可替换——Windows
	// 下结构性约束）。失败不阻断启动：以当前数据继续（标记已转 .failed 留痕）。
	if applied, err := backup.ApplyPendingRestore(opts.DataDir); err != nil {
		logger.Error("待恢复备份应用失败（以当前数据继续启动）", "err", err)
	} else if applied != nil {
		logger.Info("备份恢复已应用（原库移入安全副本目录，可回滚）",
			"backup_id", applied.BackupID, "safety_dir", applied.SafetyDir)
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

	// OQ-B 设置族（M4-1 切片 B 首员）：run_timeout 持久化 + 引擎活读——设置页
	// 变更无须重启（下次 run 拉起即生效）。文件损坏 fail safe 回缺省（错误可见，
	// 不阻断启动；首次保存会以合法文档覆写）。
	settingsStore, err := settings.Open(filepath.Join(opts.DataDir, "settings.json"))
	if err != nil {
		logger.Error("设置文件读取失败（回缺省继续）", "err", err)
		settingsStore = settings.Default()
	}

	// ID：时间有序前缀 + 随机后缀（uuidv7 语义）。
	newID := func(prefix string) string {
		var b [8]byte
		_, _ = rand.Read(b[:])
		return prefix + "_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
	}
	clock := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }

	// RFC-0013 附件面（M4-0 文件上传）：数据目录内存储；上传令牌定稿与
	// 删除级联走服务端口，语境摘录注入走 contextx 渲染器。
	attachStore := &attach.Store{Root: opts.DataDir}
	attachExcerpt := func(info contextx.AttachmentInfo) string {
		head := make([]byte, 64<<10) // 摘录上限 8k runes——64KiB 头部足够多字节场景
		if f, err := os.Open(filepath.Join(opts.DataDir, filepath.FromSlash(info.StoragePath))); err == nil {
			n, _ := f.Read(head)
			head = head[:n]
			f.Close()
		}
		return attach.RenderForContext(attach.Descriptor{
			AttachmentID: info.AttachmentID, Name: info.Name, MIME: info.MIME,
			SizeBytes: info.SizeBytes, StoragePath: info.StoragePath, SHA256: info.SHA256,
		}, agent.RedactSecrets, head) +
			// OQ-1 裁定（2026-09-07）：路径注入——agent 用自身文件工具按路径读
			// 全文/原图（图像可读性取决于各 CLI 工具能力，如实标注）。
			"\n" + attachmentPathHint(opts.DataDir, info.StoragePath)
	}

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
		Attachments: attachStore, // 令牌定稿（描述子入事件载荷）+ 删除级联
		Docs:        store,       // RFC-0014 §2.4：message.posted refs 与 attach_doc_to_room 的文档存在性校验
		// M4-1 能力门：assignee 的适配器须声明 TaskRuns（echo 等测试桩拒绝）。
		RunCapable: func(assignee string) bool {
			if engine := enginePtr.Load(); engine != nil {
				return engine.CanRun(assignee)
			}
			return false
		},
	})

	supervisor := agent.NewSupervisor()
	if err := supervisor.Register(echo.Adapter{}); err != nil {
		store.Close()
		return nil, fmt.Errorf("app: register echo: %w", err)
	}

	// RFC-0014 文档面：per-doc 事件流与房间日志并列（同一 SQLite 库、不同表）。
	// doc_id 服务端分配（doc_<12hex> 形态 Schema 锁定——6 随机字节 hex，
	// 与 newID 的时间有序形态不同源）。
	docSvc := doc.NewService(doc.Config{
		Store:    store,
		Reader:   store,
		Searcher: store,
		Lister:   store,
		Clock:    clock,
		NewID:    newID,
		NewDocID: func() string {
			var b [6]byte
			_, _ = rand.Read(b[:])
			return "doc_" + hex.EncodeToString(b[:])
		},
		Tenant: "ten_local",
	})
	// 受控执行桩（-dev 门）：M4-1 ST 与本地验证的 TaskRuns 能力面；生产装配
	//（无 -dev）不注册、不入席——能力面如实。
	if opts.Dev {
		if err := supervisor.Register(slowrun.Adapter{}); err != nil {
			store.Close()
			return nil, fmt.Errorf("app: register slowrun: %w", err)
		}
	}

	// M4-5 监控面：数据目录内 monitors.json；调度刻 10s（随主 ctx 终止）。
	monitorMgr, err := startMonitor(ctx, opts.DataDir, svc, store, logger, newID)
	if err != nil {
		logger.Error("监控面装配失败（监控端点 404 继续）", "err", err)
		monitorMgr = nil
	}

	// RFC-0016 外部云文档面（多 provider：腾讯/飞书）：per-provider 凭据目录
	// （含 RFC-0015 txdocs-token 单文件迁移）+ provider 注册表。
	// 装配失败不阻断启动（extdocs 端点 404 继续——monitor 先例）。
	extMgr, err := extdocs.LoadManager(filepath.Join(opts.DataDir, "extdocs"))
	if err != nil {
		logger.Error("外部文档面装配失败（extdocs 端点 404 继续）", "err", err)
		extMgr = nil
	}
	var extSvc *extdocs.Service
	if extMgr != nil {
		extSvc = extdocs.NewService(store)
		extSvc.Logger = logger
		tencent := extdocs.NewTencentProvider(extMgr, "")
		tencent.Logger = logger
		feishu := extdocs.NewFeishuProvider(extMgr, "")
		feishu.Logger = logger
		extSvc.Register(tencent)
		extSvc.Register(feishu)
	}

	// RFC-0017 M2 表格手动交换：provider 注册表 + doc 落库端口 + 交换记录表。
	// 启动收敛崩溃残留的 running 记录（M2 同步执行——running 只属中断窗口）。
	var sheetTransfers *extdocs.SheetTransferService
	if extSvc != nil {
		sheetTransfers = extdocs.NewSheetTransferService(extSvc, docSvc, store, newID)
		sheetTransfers.Logger = logger
		if n, err := store.MarkStaleRunningTransfers(ctx, time.Now().Format(time.RFC3339)); err != nil {
			logger.Error("表格交换残留收敛失败", "err", err.Error())
		} else if n > 0 {
			logger.Warn("表格交换存在中断残留，已标记 unknown（历史可查）", "count", n)
		}
	}

	hub := sse.NewHub()

	// 宿主层：启动自动扫描（探测失败不阻塞启动，注册表持久化合并）。
	harnessRegistry, err := harness.LoadOrCreate(filepath.Join(opts.DataDir, "harness-registry.json"))
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("app: harness registry: %w", err)
	}
	// 独立 Agent 身份域（RFC-0018 / ADR-0017，0.5.0）：SQLite 权威（v7 表）+
	// 注册表单向同步。装载后先迁移磁盘存量（先于扫描），扫描完成后增量同步；
	// 身份 API 读前另有惰性幂等同步兜底（手动登记/删除最终一致）。
	identitySvc := personalagent.New(store, personalagent.NewRegistryAdapter(harnessRegistry),
		personalagent.WithNewAgentID(func() string { return newID("ag") }), personalagent.WithNow(clock))
	if rep, err := identitySvc.SyncFromHarness(ctx); err != nil {
		logger.Warn("agent 身份迁移失败（读面前惰性重试）", "err", err)
	} else {
		logger.Info("agent 身份迁移完成", "created", rep.Created, "aliases", rep.AliasesAdded)
		for _, c := range rep.Conflicts {
			logger.Warn("agent 身份别名冲突（不合并，待人工处置）", "detail", c)
		}
	}
	// 职责域（RFC-0018 §5 / ADR-0019–0022，0.5.2–0.5.5）：模板/实例/任职/准入。
	// AgentLookup 适配身份服务（任职需要 Agent 存在且 active）；准入门实装
	// GrantReadinessChecker（0.5.5：授权档案 × 模板要求 → blocked/降级）。
	// 依赖方向：app 装配注入，slot 不 import personalagent。
	slotSvc := slot.New(store, slot.WithNow(clock),
		slot.WithAgentLookup(identityAgentLookup{identitySvc}),
		slot.WithReadiness(slot.NewGrantReadinessChecker(store)))
	// Mission 契约域（RFC-0018 §6.1 / ADR-0023，0.6.0）：独立事实源 + CAS 修订。
	// owner 存在性经 AgentLookup 端口（AgentByID——任意生命周期可指派）。
	roomTasks := roomTaskLookup{reader: store}
	missionSvc := mission.New(store, mission.WithNow(clock),
		mission.WithAgentLookup(missionAgentLookup{identitySvc}),
		mission.WithSourceLookup(roomTasks))
	// 网关首切片（ADR-0024 / 0.6.1）：确定性解析 → 复核投递 → 回执；
	// caller/scope 服务端推导（owner/workspace），幂等键库级唯一。
	dispatchSvc := dispatch.New(store, missionSvc, dispatch.Lookups{
		Agents:  missionAgentLookup{identitySvc},
		Sources: roomTasks,
		Mapped:  missionSvc.SourceMapped,
	}, dispatch.WithNow(clock))
	// 执行上下文清单域（RFC-0018 §7.3 / ADR-0025，0.6.2）：确定性装配存档
	// （digest 即身份——重建重放，状态变化追加）；任职事实（binding → 实例
	// → 钉版本模板行）与房间事件经端口适配注入。readiness 经 ADR-0022
	// 交集单源纯函数（EvaluateReadiness）与工具清单/域授权共用同一授权
	// 快照（审校 P4——单次装配只读一份授权）。授权过滤先于排序计数在
	// execctx 域内执行并测试钉住。
	execCtxSvc := execctx.New(store, execctx.Ports{
		Missions: missionSvc,
		Bindings: bindingFactsAdapter{slots: slotSvc},
		Grants:   store,
		Rooms:    execRoomEvents{reader: store},
	}, execctx.WithNow(clock))
	// 本地 Environment / Runtime 域（RFC-0018 §3/§6 / ADR-0026，0.6.3）：
	// 发现源与探测面复用 harness 注册表（sync 单向搬运不回写；Reprobe 只读）；
	// 默认环境启动确保（全库恰一，部分唯一索引兜底）。
	runtimeSvc := mosaicruntime.New(store,
		harnessProber{registry: harnessRegistry, runner: harness.NewHostRunner()},
		osDirFS{},
		mosaicruntime.WithNow(clock),
		mosaicruntime.WithDiscoverySource(harnessDiscoveryOf(harnessRegistry)),
		// 租约占用主体端口（0.6.4 / ADR-0027）：missionSvc 单源——存在性
		// 与 canceled 拒领，域不 import mission 包。
		mosaicruntime.WithMissions(missionStateLookup{missionSvc}))
	if _, err := runtimeSvc.EnsureDefaultEnvironment(ctx, "默认工作区", filepath.Join(opts.DataDir, "workspace")); err != nil {
		logger.Warn("默认环境确保失败（读面前惰性重试）", "err", err)
	}
	// Mission 执行域（RFC-0018 §10.2 / ADR-0028，0.6.5）：三件底座
	// （清单/运行时/租约）首次被完整消费——调度器周期扫描 ready Mission，
	// 领取租约 → 装配存档清单 → 创建 Run → supervisor 执行 → 围栏核对 →
	// 终态 + 释放（Mission 回 ready 可再执行）。
	runSvc := missionrun.New(store,
		runMissionsAdapter{missionSvc},
		runBindingsAdapter{slots: slotSvc},
		runRuntimesAdapter{runtimes: runtimeSvc},
		runLeasesAdapter{runtimes: runtimeSvc},
		runManifestsAdapter{execctx: execCtxSvc},
		supervisorExecutor{supervisor: supervisor},
		missionrun.WithNow(clock))
	if n, err := runSvc.RecoverUnfinished(ctx); err != nil {
		logger.Warn("Run 恢复失败", "err", err)
	} else if n > 0 {
		logger.Info("Run 恢复：未终态置 failed（结果未知，不自动重跑）", "count", n)
	}
	runSvc.Start(ctx)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second) // 随主 ctx 取消
		defer cancel()
		scanOpts := harness.ScanOptions{IncludeWSL: goruntime.GOOS == "windows"}
		if err := harnessRegistry.Scan(scanCtx, harness.NewHostRunner(), harness.BuiltinProbes, scanOpts); err != nil {
			logger.Warn("harness scan partial failure", "err", err)
		}
		for _, note := range harnessRegistry.DrainNotes() {
			logger.Info("harness 身份迁移", "note", note)
		}
		if rep, err := identitySvc.SyncFromHarness(scanCtx); err != nil {
			logger.Warn("agent 身份同步失败（读面前惰性重试）", "err", err)
		} else if rep.Created > 0 || len(rep.Conflicts) > 0 {
			logger.Info("agent 身份同步", "created", rep.Created, "conflicts", len(rep.Conflicts))
			for _, c := range rep.Conflicts {
				logger.Warn("agent 身份别名冲突（不合并，待人工处置）", "detail", c)
			}
		}
		for _, exe := range harnessRegistry.List() {
			logger.Info("harness discovered",
				"adapter", exe.Adapter, "runtime", exe.Runtime, "distro", exe.Distro,
				"path", exe.Path, "version", exe.Version, "login", exe.Login)
		}
		// 运行时发现同步（ADR-0026：扫描后自动——发现幂等，既有行只刷事实）
		if created, refreshed, err := runtimeSvc.SyncFromSource(scanCtx); err != nil {
			logger.Warn("运行时同步失败（读面前惰性重试）", "err", err)
		} else if created > 0 || refreshed > 0 {
			logger.Info("运行时同步", "created", created, "refreshed", refreshed)
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

	// M4-0 备份面（数据目录内 backups/；VACUUM INTO 一致快照）与自诊断 bundle。
	backupMgr := &backup.Manager{DB: store, Dir: opts.DataDir}
	deps := httpapi.Deps{
		SVC:              svc,
		Reader:           store,
		Hub:              hub,
		Actor:            room.Actor{ParticipantID: "par_owner", Kind: "human"}, // 本地 owner（ADR-0009）
		Harness:          harnessRegistry,
		ProbeRunner:      harness.NewHostRunner(),
		Identity:         identitySvc,
		Slots:            slotSvc,
		Missions:         missionSvc,
		Dispatch:         dispatchSvc,
		Execctx:          execCtxSvc,
		Runtimes:         runtimeSvc,
		MissionRuns:      runSvc,
		Logger:           logger,
		Dev:              opts.Dev,
		Budget:           budgetLimits,
		Outbox:           store,
		Searcher:         store, // M3-3 按需平面：FTS5 trigram（SQLite 实现端口）
		ReceiptLister:    store, // v1.70 展示对齐：回执流水正式面
		ExtraOriginHosts: opts.ExtraOriginHosts,
		OwnerToken:       ownerToken,
		UI:               ui,
		Backups:          backupMgr,
		Attachments:      attachStore,
		Settings:         settingsStore,
		Monitors:         monitorMgr,
		DocSVC:           docSvc,
		DocReader:        store, // doc:{id} 频道 SSE 追平读路径
		ExtDocs:          extSvc,
		SheetTransfers:   sheetTransfers,
		Diagnostics: diagnosticsBundle(opts.DataDir,
			func() (int, error) { return countRooms(store) },
			func() int {
				if engine := enginePtr.Load(); engine != nil {
					return len(engine.Seats())
				}
				return 0
			},
			func() []map[string]any {
				out := make([]map[string]any, 0, len(harnessRegistry.List()))
				for _, exe := range harnessRegistry.List() {
					out = append(out, map[string]any{
						"adapter": exe.Adapter, "runtime": exe.Runtime, "distro": exe.Distro,
						"enabled": exe.Enabled, "login": exe.Login, "version": exe.Version,
					})
				}
				return out
			},
		),
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

	// doc outbox 分发（RFC-0014 §2.6）：独立于房间分发器（不同表），消费者只有
	// doc:{id} SSE 频道——引擎不消费 doc 事件（人类编辑不触发反应波，§2.7 裁定）。
	// 不等宿主扫描：文档事件分发与引擎就绪无依赖。
	go func() {
		docDispatcher := outbox.NewDispatcher(sqlite.DocOutboxStore{Store: store}, []outbox.Consumer{
			httpapi.DocHubConsumer(hub),
		}, 20*time.Millisecond).WithLogger(logger)
		docDispatcher.Run(ctx)
	}()

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
		// zcode 桌面渠道托管缓存根（bundle+provider 副本，见 zcode 适配器
		// ensureDesktopCache）：与工作根同级——同在 mosaic 缓存位之下，随其清理。
		zcodeCacheRoot := filepath.Join(filepath.Dir(workRoot), "zcode-desktop")
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
		// 注册签名（ADR-0013/M4-4）：resync 每 10s 重算座位，但适配器只在签名变化时
		// 重注册——RegisterFor 会驱逐该 Profile 的全部会话（实例替换语义），无条件
		// 重注册等于每 10 秒清一次会话续接句柄（kimi -S / mcode --session）。
		registered := map[string]string{}
		registerAdapter := func(profileID, signature string, mk func() error) error {
			if prev, ok := registered[profileID]; ok && prev == signature {
				return nil // 配置未变：不驱逐会话
			}
			if err := mk(); err != nil {
				return err
			}
			registered[profileID] = signature
			return nil
		}
		syncSeats := func() []room.AgentSeat {
			seats := []room.AgentSeat{{
				ParticipantID: "par_echo",
				Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
			}}
			if opts.Dev {
				seats = append(seats, room.AgentSeat{
					ParticipantID: "par_slowrun",
					Profile:       agent.Profile{ProfileID: "prof_slowrun", Adapter: "slowrun", DisplayName: "SlowRun"},
				})
			}
			for _, exe := range harnessRegistry.EnabledList() {
				// 身份从稳定 BotID 派生（ADR-0013：路径变化经重绑保持身份；四轮复审 #3
				// 的多实例不折叠语义不变）；工作目录随 BotID 稳定（重绑后目录沿用）。
				profileID := personalagent.ProfileIDOf(exe.Adapter, exe.BotIDOf())
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
					cfg := codex.Config{CodexPath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir,
						EvalModel: exe.EvalModel, Model: exe.Model, ReasoningEffort: exe.ReasoningEffort}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					// 四轮复审 #9：重登/换实例驱逐旧会话——经签名门控（同配置不驱逐）。
					if err := registerAdapter(profileID,
						strings.Join([]string{exe.Path, dir, exe.Distro, wslHome, exe.Model, exe.EvalModel, exe.ReasoningEffort}, "\x1f"),
						func() error { return supervisor.RegisterFor(profileID, codex.New(cfg)) }); err != nil {
						logger.Warn("codex adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: personalagent.ParticipantIDOf("codex", exe.BotIDOf()),
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "codex", DisplayName: "Codex", Channel: channel},
					})
				case "kimi":
					// C 轨：第二个真实适配器（kimi -p stream-json + -S 会话恢复）。
					cfg := kimi.Config{KimiPath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir,
						EvalModel: exe.EvalModel, Model: exe.Model}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					if err := registerAdapter(profileID,
						strings.Join([]string{exe.Path, dir, exe.Distro, wslHome, exe.Model, exe.EvalModel}, "\x1f"),
						func() error { return supervisor.RegisterFor(profileID, kimi.New(cfg)) }); err != nil {
						logger.Warn("kimi adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: personalagent.ParticipantIDOf("kimi", exe.BotIDOf()),
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "kimi", DisplayName: "Kimi", Channel: channel},
					})
				case "minimax":
					// M3-1 观测基座：第三个真实适配器（mcode exec stream-json + --session 恢复，
					// 提示词走 stdin——无 argv 上限；三 agent 同房构成真实并行/竞争场景）。
					cfg := minimax.Config{McodePath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir,
						EvalModel: exe.EvalModel, Model: exe.Model}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					if err := registerAdapter(profileID,
						strings.Join([]string{exe.Path, dir, exe.Distro, wslHome, exe.Model, exe.EvalModel}, "\x1f"),
						func() error { return supervisor.RegisterFor(profileID, minimax.New(cfg)) }); err != nil {
						logger.Warn("minimax adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: personalagent.ParticipantIDOf("minimax", exe.BotIDOf()),
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "minimax", DisplayName: "MiniMax", Channel: channel},
					})
				case "zcode":
					// 0.3.0 Agent 兼容性扩充首员：第四个真实适配器（zcode -p stream-json
					// --mode yolo + --resume 恢复；提示词经 sh $(cat) 走 stdin——-p 只认
					// argv 同 kimi 面；模型覆盖走 provider 配置 overlay，无 --model flag）。
					// 桌面渠道（exe.Bundle 非空）：ELECTRON_RUN_AS_NODE 驱动缓存 bundle
					// 副本（无感适配 2026-09-23，见 zcode 适配器包注释）。
					cfg := zcode.Config{ZcodePath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir,
						EvalModel: exe.EvalModel, Model: exe.Model,
						Bundle: exe.Bundle, CacheRoot: zcodeCacheRoot}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					if err := registerAdapter(profileID,
						strings.Join([]string{exe.Path, exe.Bundle, dir, exe.Distro, wslHome, exe.Model, exe.EvalModel}, "\x1f"),
						func() error { return supervisor.RegisterFor(profileID, zcode.New(cfg)) }); err != nil {
						logger.Warn("zcode adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: personalagent.ParticipantIDOf("zcode", exe.BotIDOf()),
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "zcode", DisplayName: "ZCode", Channel: channel},
					})
				case "claude":
					// 0.4.0 Agent 兼容性扩充：第五个真实适配器（claude -p stream-json
					// --verbose（实证必需）+ --resume 恢复 + --dangerously-skip-permissions
					// YOLO 等价；提示词走 stdin 一等通道——无 argv 上限；模型覆盖原生
					// --model flag；无 --cwd——工作目录经 Execer dir 传递）。
					cfg := claude.Config{ClaudePath: exe.Path, Timeout: 180 * time.Second, WorkDir: dir,
						EvalModel: exe.EvalModel, Model: exe.Model}
					if wslHome != "" {
						cfg.WSLDistro = exe.Distro
						cfg.WSLHome = wslHome
					}
					if err := registerAdapter(profileID,
						strings.Join([]string{exe.Path, dir, exe.Distro, wslHome, exe.Model, exe.EvalModel}, "\x1f"),
						func() error { return supervisor.RegisterFor(profileID, claude.New(cfg)) }); err != nil {
						logger.Warn("claude adapter register failed", "profile", profileID, "err", err)
						continue
					}
					seats = append(seats, room.AgentSeat{
						ParticipantID: personalagent.ParticipantIDOf("claude", exe.BotIDOf()),
						Profile:       agent.Profile{ProfileID: profileID, Adapter: "claude", DisplayName: "Claude", Channel: channel},
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
			AttachExcerpt:    attachExcerpt,
			Docs:             docSvc, // RFC-0014 §2.7：doc_ops 引擎代写面（actor 记为该 bot）
			Sheets:           docSvc, // RFC-0017 §4.3：sheet_ops 引擎代写面（同一 doc.Service）
			// §2.7 读面①：文档摘录注入（8k runes 有界 + DLP 秘密形状剔除——附件摘录同纪律）。
			DocExcerpt: func(docID string) string {
				state, err := docSvc.GetDoc(ctx, docID)
				if err != nil {
					return "（文档 " + docID + " 不存在或已删除）"
				}
				return doc.RenderExcerpt(state, doc.ExcerptRunes, agent.RedactSecrets)
			},
			// OQ-B 设置族活读面：每次 run 拉起时读当前设置（变更无须重启引擎）
			RunTimeoutFunc: settingsStore.RunTimeout,
			// 附录 K：发言资格闸活读（settings → 引擎配置映射；变更对下一波生效）
			SpeakGateFunc: func() room.SpeakGateSettings {
				doc := settingsStore.Snapshot()
				return room.SpeakGateSettings{
					Enabled:         !doc.SpeakGateOff,
					Threshold:       doc.SpeakGateThresh,
					CooldownPenalty: doc.SpeakGateCooldown,
				}
			},
			// M4-3：ROP 门（reply_or_pass_mode）+ 路径指标（A/B 测量面，JSONL 落盘）
			ReplyOrPassEnabled: settingsStore.ReplyOrPassEnabled,
			OnPathMetric:       pathMetricSink(opts.DataDir),
			OnDraft:            httpapi.DraftConsumer(hub),
			OnWaveSkip:         httpapi.WaveSkipConsumer(hub),
			OnSeatStatus:       httpapi.SeatStatusConsumer(hub),
			Logger:             logger,
			Clock:              clock,
			Now:                time.Now,
			NewID:              newID,
			Tenant:             "ten_local",
		})
		enginePtr.Store(engine)
		engine.RecoverClaims() // 崩溃窗口重驱动（二轮审校 #9）
		engine.RecoverRuns()   // M4-1：running 态无活进程 → run.unknown（结果未知，不自动重跑）
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

// countRooms 房间数（诊断面数据统计；ListRooms 是既有读路径）。
func countRooms(store *sqlite.Store) (int, error) {
	rooms, err := store.ListRooms(context.Background())
	if err != nil {
		return 0, err
	}
	return len(rooms), nil
}

// roomTaskLookup mission.SourceLookup 的房间域适配（ADR-0024 / 0.6.1）：
// EventReader 分页读全量事件 → room.TasksOf 纯函数重建任务清单 → 存在性。
type roomTaskLookup struct{ reader room.EventReader }

func (l roomTaskLookup) RoomTaskExists(ctx context.Context, roomID, taskID string) (bool, error) {
	// 跨页累积全量有序历史后统一重建（审校 P2#3）：TasksOf 是因果序单遍
	// 重建——参与者索引（@指派解析）与既有申报状态都可能在前页，逐页
	// 重建会把真实任务判不存在/派生错 ID。
	var all []room.StoredEvent
	cursor := ""
	for {
		events, next, err := l.reader.EventsAfter(ctx, roomID, cursor, 500)
		if err != nil {
			return false, err
		}
		all = append(all, events...)
		if next == "" {
			break
		}
		cursor = next
	}
	for _, t := range room.TasksOf(all) {
		if t.TaskID == taskID {
			return true, nil
		}
	}
	return false, nil
}

// runMissionsAdapter missionrun.Missions 的 Mission 域适配（ADR-0028）。
type runMissionsAdapter struct{ svc *mission.Service }

func (a runMissionsAdapter) ListReady(ctx context.Context) ([]missionrun.MissionRef, error) {
	list, err := a.svc.ListMissions(ctx)
	if err != nil {
		return nil, err
	}
	var out []missionrun.MissionRef
	for _, m := range list {
		if m.Status == mission.StatusReady {
			out = append(out, missionrun.MissionRef{MissionID: m.MissionID, OwnerAgentID: m.OwnerAgentID})
		}
	}
	return out, nil
}

func (a runMissionsAdapter) SetRunning(ctx context.Context, missionID string) error {
	return a.svc.MarkRunning(ctx, missionID)
}

func (a runMissionsAdapter) SetReadyFromRunning(ctx context.Context, missionID, reason string) error {
	return a.svc.MarkReadyFromRunning(ctx, missionID, reason)
}

// runBindingsAdapter missionrun.Bindings 的职责域适配：owner 的 active
// Binding 解析（slot.Service 单源）。
type runBindingsAdapter struct{ slots *slot.Service }

func (a runBindingsAdapter) ActiveOf(ctx context.Context, agentID string) (missionrun.BindingFacts, bool, error) {
	instances, err := a.slots.ListInstances(ctx)
	if err != nil {
		return missionrun.BindingFacts{}, false, err
	}
	for _, inst := range instances {
		bindings, err := a.slots.ListBindings(ctx, inst.SlotInstanceID)
		if err != nil {
			continue
		}
		for _, b := range bindings {
			if b.AgentID == agentID && b.Status == slot.BindingActive {
				return missionrun.BindingFacts{
					BindingID:  b.BindingID,
					AgentID:    b.AgentID,
					RuntimeID:  "", // 0.6.5：Runtime 候选由 Runtimes 端口解析（单 Agent 首个 ready）
					SlotSpecID: inst.SlotSpecID,
				}, true, nil
			}
		}
	}
	return missionrun.BindingFacts{}, false, nil
}

// runRuntimesAdapter missionrun.Runtimes 适配：ready 首个且无活跃租约。
type runRuntimesAdapter struct{ runtimes *mosaicruntime.Service }

func (a runRuntimesAdapter) FirstReady(ctx context.Context) (missionrun.RuntimeRef, bool, error) {
	list, err := a.runtimes.ListRuntimes(ctx)
	if err != nil {
		return missionrun.RuntimeRef{}, false, err
	}
	leases, err := a.runtimes.ListLeases(ctx, mosaicruntime.LeaseStateActive, "")
	if err != nil {
		return missionrun.RuntimeRef{}, false, err
	}
	busy := map[string]bool{}
	for _, l := range leases {
		busy[l.RuntimeID] = true
	}
	for _, rt := range list {
		if rt.State == mosaicruntime.StateReady && !busy[rt.RuntimeID] {
			return missionrun.RuntimeRef{RuntimeID: rt.RuntimeID, EnvironmentID: rt.EnvironmentID}, true, nil
		}
	}
	return missionrun.RuntimeRef{}, false, nil
}

// runLeasesAdapter missionrun.Leases 适配（ADR-0027 服务同源——域间经
// 小端口转发，不直接依赖 runtime 包）。
type runLeasesAdapter struct{ runtimes *mosaicruntime.Service }

func (a runLeasesAdapter) Acquire(ctx context.Context, runtimeID, missionID string, ttl int, limits map[string]int) (missionrun.Lease, error) {
	l, err := a.runtimes.Acquire(ctx, runtimeID, missionID, ttl, limits)
	if err != nil {
		return missionrun.Lease{}, err
	}
	return missionrun.Lease{LeaseID: l.LeaseID, FenceToken: l.FenceToken, Generation: l.Generation, EnvironmentID: l.EnvironmentID}, nil
}

func (a runLeasesAdapter) Release(ctx context.Context, leaseID string) error {
	_, err := a.runtimes.Release(ctx, leaseID)
	return err
}

func (a runLeasesAdapter) CheckFence(ctx context.Context, leaseID, token string) (bool, string, error) {
	return a.runtimes.CheckFence(ctx, leaseID, token)
}

// runManifestsAdapter missionrun.Manifests 适配（ADR-0025 Build 同源）。
type runManifestsAdapter struct{ execctx *execctx.Service }

func (a runManifestsAdapter) Build(ctx context.Context, missionID, bindingID string) (missionrun.Manifest, error) {
	res, err := a.execctx.Build(ctx, missionID, bindingID)
	if err != nil {
		return missionrun.Manifest{}, err
	}
	return missionrun.Manifest{ManifestID: res.Manifest.ManifestID, State: res.Manifest.State, ReasonCode: res.Manifest.ReasonCode}, nil
}

// supervisorExecutor missionrun.Executor 适配：agent.Supervisor 提交
// （Kind=generate，存档清单引用入 Inline；工作目录由 Runtime 绑定环境
// 决定——0.6.3 已绑定）。阻塞至 Result 终值。
type supervisorExecutor struct{ supervisor *agent.Supervisor }

func (e supervisorExecutor) Submit(ctx context.Context, in missionrun.ExecutionInput) (string, error) {
	profile := agent.Profile{ProfileID: in.AgentID, Adapter: "echo", DisplayName: in.AgentID}
	h, err := e.supervisor.Submit(ctx, profile, agent.Task{
		TaskID: "mrun:" + in.RunID, Kind: agent.KindGenerate,
		ParticipantID: in.AgentID, RoomID: "mission:" + in.MissionID,
		Deadline: time.Now().Add(30 * time.Minute),
		Context:  agent.Context{Inline: in.Inline},
	})
	if err != nil {
		return "", err
	}
	res, err := h.Result()
	if err != nil {
		return "", err
	}
	body, _ := res.Data["body"].(string)
	return body, nil
}

// missionStateLookup runtime.Missions 的 Mission 域适配（ADR-0027）。
type missionStateLookup struct{ svc *mission.Service }

func (l missionStateLookup) MissionState(ctx context.Context, missionID string) (string, bool, error) {
	m, ok, err := l.svc.Get(ctx, missionID)
	if err != nil || !ok {
		return "", ok, err
	}
	return m.Status, true, nil
}

// harnessDiscoveryOf runtime.DiscoverySource 的 harness 适配（ADR-0026 /
// 0.6.3）：注册表全量镜像（Enabled 是座位面关注，运行时记录全部发现）。
func harnessDiscoveryOf(registry *harness.Registry) mosaicruntime.DiscoverySource {
	return func(_ context.Context) ([]mosaicruntime.DiscoveredEntry, error) {
		exes := registry.List()
		out := make([]mosaicruntime.DiscoveredEntry, 0, len(exes))
		for _, e := range exes {
			out = append(out, mosaicruntime.DiscoveredEntry{
				Adapter: e.Adapter, Host: string(e.Runtime), Distro: e.Distro,
				Path: e.Path, Channel: e.Channel, Version: e.Version, Login: e.Login,
			})
		}
		return out, nil
	}
}

// harnessProber runtime.Prober 的 harness 适配：按 (adapter,host,distro,path)
// 定位登记行 → Reprobe（只读不回写）。成功 = 登记行在且版本探测非空；文件
// 消失 Reprobe 返回 ok=false → executable_gone。
type harnessProber struct {
	registry *harness.Registry
	runner   harness.Runner
}

func (a harnessProber) Probe(ctx context.Context, target mosaicruntime.ProbeTarget) (mosaicruntime.ProbeResult, error) {
	for _, e := range a.registry.List() {
		if e.Adapter != target.Adapter || string(e.Runtime) != target.Host ||
			e.Distro != target.Distro || e.Path != target.Path {
			continue
		}
		fresh, ok, err := a.registry.Reprobe(ctx, a.runner, e.ID)
		if err != nil {
			return mosaicruntime.ProbeResult{}, err
		}
		if !ok {
			return mosaicruntime.ProbeResult{OK: false, Login: "unknown", Note: "executable_gone"}, nil
		}
		if fresh.Version == "" {
			return mosaicruntime.ProbeResult{OK: false, Login: fresh.Login, Note: "version_probe_failed"}, nil
		}
		return mosaicruntime.ProbeResult{OK: true, Version: fresh.Version, Login: fresh.Login}, nil
	}
	return mosaicruntime.ProbeResult{OK: false, Login: "unknown", Note: "registry_entry_missing"}, nil
}

// osDirFS runtime.FS 的 os 适配（工作区目录确保）。
type osDirFS struct{}

func (osDirFS) EnsureDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// bindingFactsAdapter execctx.Bindings 的职责域适配（ADR-0025 / 0.6.2）：
// 任职 → 实例（模板谱系）→ 钉住版本（SlotSpecVersion）的模板版本行——
// 职责层内容以任职钉版为准，不随模板最新版漂移。
type bindingFactsAdapter struct{ slots *slot.Service }

func (a bindingFactsAdapter) Facts(ctx context.Context, bindingID string) (execctx.BindingFacts, bool, error) {
	b, ok, err := a.slots.GetBinding(ctx, bindingID)
	if err != nil || !ok {
		return execctx.BindingFacts{}, false, err
	}
	inst, ok, err := a.slots.GetInstance(ctx, b.SlotInstanceID)
	if err != nil || !ok {
		return execctx.BindingFacts{}, false, err
	}
	versions, ok, err := a.slots.Versions(ctx, inst.SlotInstance.SlotSpecID)
	if err != nil || !ok {
		return execctx.BindingFacts{}, false, err
	}
	for _, spec := range versions {
		if spec.Version == b.SlotSpecVersion {
			return execctx.BindingFacts{Binding: b, Spec: spec}, true, nil
		}
	}
	return execctx.BindingFacts{}, false, nil
}

// execRoomEvents execctx.RoomReader 的房间域适配（ADR-0025 / 0.6.2）：
// EventReader 跨页累积全量有序事件（ADR-0024 P2#3 同型）→ 信封映射
// RoomEvent。可见性原样透传——授权过滤先于排序计数是 execctx 域内
// 不变量（域 UT 钉住），适配器预滤会让不变量失去测试钉子。
type execRoomEvents struct{ reader room.EventReader }

func (a execRoomEvents) RoomEvents(ctx context.Context, roomID string) ([]execctx.RoomEvent, error) {
	var all []room.StoredEvent
	cursor := ""
	for {
		events, next, err := a.reader.EventsAfter(ctx, roomID, cursor, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
		if next == "" {
			break
		}
		cursor = next
	}
	out := make([]execctx.RoomEvent, 0, len(all))
	for _, e := range all {
		evt := execctx.RoomEvent{
			Seq: e.Envelope.Seq, EventID: e.Envelope.EventID, OccurredAt: e.Envelope.OccurredAt,
			Type: e.Envelope.Type, Visibility: e.Envelope.Visibility.Kind,
			ParticipantID: e.Envelope.Actor.ParticipantID, ActorKind: e.Envelope.Actor.Kind,
		}
		if e.Envelope.Type == protocol.EventMessagePosted {
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(e.Envelope.Payload, &payload)
			evt.Body = payload.Body
		}
		out = append(out, evt)
	}
	return out, nil
}

// missionAgentLookup mission.AgentLookup 的身份域适配（owner 存在性）。
type missionAgentLookup struct{ svc *personalagent.Service }

func (l missionAgentLookup) AgentExists(ctx context.Context, agentID string) (bool, error) {
	_, ok, err := l.svc.AgentByID(ctx, agentID)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// identityAgentLookup slot.AgentLookup 的身份域适配（任职前置校验：
// Agent 须存在且 active；ADR-0021）。
type identityAgentLookup struct {
	svc *personalagent.Service
}

func (l identityAgentLookup) AgentStatus(ctx context.Context, agentID string) (string, bool, error) {
	a, ok, err := l.svc.AgentByID(ctx, agentID)
	if err != nil || !ok {
		return "", ok, err
	}
	return a.Status, true, nil
}
