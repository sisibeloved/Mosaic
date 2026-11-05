// Package run：单 Agent Mission 执行（0.6.5 / ADR-0028）——Run 域（一次
// 执行尝试）+ 持久调度器 + 执行器端口。0.6.2–0.6.4 三件底座（存档清单 /
// 环境与运行时 / 租约与围栏）在此首次被完整消费：ready Mission → 领取
// 租约 → 装配存档清单 → 创建 Run（Mission → running）→ 提交执行 →
// 围栏核对 → Run 终态 + 租约释放（Mission 回 ready 可再执行）。
//
// 编排安全面（v0.2 审校修复）：
//   - 提交前围栏复核（#3——失效/检查错误拒启动）
//   - 围栏核对与终态写入同事务（#4——防释放后旧结果落库）
//   - 请求断开后独立清理上下文（#5——不遗留 running + 活跃租约）
//   - 恢复后不自动调度（#6——Mission 停在 running 等待用户决定）
//   - canceled 后拒启动（#7——条件更新校验实际修改行数）
//
// 0.6.6 / ADR-0029 控制停止面：执行脱离请求上下文（Background 派生每
// Run 取消上下文 + 停止注册表）；SignalStopByMission 发信号 → 执行侧
// 终理化 Run → canceled（stop_requested 审计 + 租约释放 + Mission 投影
// 不动）；迟到执行器结果丢弃；恢复按停止意图分派（canceling 兑现收尾 /
// paused 停留 / running 照旧 failed 未知）。
package run

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Run 状态（pending 为事务内瞬态——落库即 running；canceled 为 0.6.6 控制停
// 止终态；unknown 随 0.6.7）。
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
	// StateCanceled 控制停止终态（0.6.6 / ADR-0029）：暂停/取消的停止信号
	// 使然——结果不落（迟到结果只审计），attempt 已消耗。
	StateCanceled = "canceled"
	// StateUnknown 结果不可判定终态（0.6.7 / ADR-0030）：进程重启时在执行且
	// Mission 无停止意图——系统未观测到完成也未观测到失败。终态（出未终态
	// 口径）但**可核验处置**（resolution），处置不改写状态（用户 attest 非
	// 系统事实）；Mission 同步进 waiting(reconciliation) 直至处置。
	StateUnknown = "unknown"
)

// 处置结论（unknown Run 的核验结果——用户 attest）。
const (
	ResolutionVerifiedCompleted = "verified_completed"
	ResolutionVerifiedFailed    = "verified_failed"
	ResolutionDiscarded         = "discarded"
)

// WaitingReconciliation Mission 等待原因：有未处置 unknown Run。
const WaitingReconciliation = "reconciliation"

// legacyResiduePrefix 0.6.5/0.6.6 恢复面写给崩溃残留的 error_text 前缀
// （failed 形态——升级收敛认领口径；与停止路径 canceled 的"结果未知"文案
// 不同串，不混淆）。
const legacyResiduePrefix = "进程重启时在执行——结果未知"

// 事件类型（run_events 流）。
const (
	EventAttemptRequested = "attempt_requested"
	EventStarted          = "started"
	EventCompleted        = "completed"
	EventFailed           = "failed"
	EventFenceRejected    = "fence_rejected"
	// EventStopRequested 控制停止信号审计（0.6.6 / ADR-0029）：payload.cause
	// = pause | cancel | recovery（停止意图来源）。
	EventStopRequested = "stop_requested"
	// EventCanceled Run canceled 终态事件（payload.cause 同上）。
	EventCanceled = "canceled"
	// EventUnknown 结果不可判定终态事件（0.6.7 / ADR-0030；payload.recovery=true）。
	EventUnknown = "unknown"
	// EventLateResult 停止后迟到结果审计（0.6.7——AT-12"旧结果只审计"）：
	// payload {outcome: ok|error, preview, generation, published:false, phase}。
	// Run 行不变、结果不落、不发布。
	EventLateResult = "late_result"
	// CauseWallClockExceeded 墙钟到期停止原因（0.6.13 / ADR-0036 §3——进入
	// stop_requested / canceled 事件 payload 的 cause）。
	CauseWallClockExceeded = "wall_clock_exceeded"
	// EventResolved unknown 处置事件（payload {outcome, note}）。
	EventResolved = "resolved"
)

// 领域错误。
var (
	ErrRunNotFound     = errors.New("run: Run 不存在")
	ErrMissionNotFound = errors.New("run: Mission 不存在")
	// ErrMissionNotReady Mission 非 ready（running/canceled/draft）。
	ErrMissionNotReady = errors.New("run: Mission 非 ready")
	// ErrNoExecutor 无可用执行面（owner 无 active Binding / 无能力满足的 ready Runtime）。
	ErrNoExecutor = errors.New("run: 无可用执行面")
	// ErrSchedulerBusy 资源被占（租约冲突——本轮跳过，下轮再试）。
	ErrSchedulerBusy = errors.New("run: 资源被占")
	// ErrManifestBlocked 存档清单装配 blocked（原因在清单——返回给调用方）。
	ErrManifestBlocked = errors.New("run: 执行清单 blocked")
	// ErrRunExists 未终态 Run 已存在（每 Mission 至多一个）。
	ErrRunExists = errors.New("run: 已有未终态 Run")
	// ErrFenceRejected 提交前围栏复核失败（租约已失效——执行不启动）。
	ErrFenceRejected = errors.New("run: 围栏复核失败")
	// ErrTerminalWrite 终态落库失败（结果有但库未收——返回错误给调用方）。
	ErrTerminalWrite = errors.New("run: 终态落库失败")
	// ErrRunNotUnknown 处置目标非 unknown（0.6.7）。
	ErrRunNotUnknown = errors.New("run: Run 非 unknown，不可处置")
	// ErrRunAlreadyResolved 已以不同结论处置（同结论幂等返回当前）。
	ErrRunAlreadyResolved = errors.New("run: Run 已处置")
	// ErrInvalidResolution 处置结论/备注不合法。
	ErrInvalidResolution = errors.New("run: 处置不合法")
	// ErrBudgetExhausted 该 Mission 已创建 Run 总数达 budget.max_runs 上限
	//（0.6.13 / ADR-0036 §3——不再派发，任务停留 ready，诊断面计数）。
	ErrBudgetExhausted = errors.New("run: budget.max_runs 已用尽——不再派发")
	// ErrOwnerNotActive 执行者身份门复核失败（复审 #4：领取 Run 的建行事务
	// 内核对——负责人身份非 active（暂停/退役不自动解除任职）或装配所钉
	// 任职已失效/换人；任务停留 ready，安全拒绝启动）。
	ErrOwnerNotActive = errors.New("run: 执行者身份门复核失败（负责人非 active 或任职失效）")
	// ErrContractDrift 装配清单与现行契约版本漂移（复审 #10：RefOf/装配后
	// 契约被修订——释放租约重新装配，下一轮以新版本快照执行；预算与清单
	// 不得混用两个版本）。
	ErrContractDrift = errors.New("run: 契约版本漂移——释放租约重新装配")
	// ErrGrantDrift 清单授权快照漂移（四审 #3 / RFC §11.4：装配后授权档案
	// 变化（含可选上下文/工具撤销）——旧清单不得交付，释放租约重新装配按
	// 剩余授权降级执行）。
	ErrGrantDrift = errors.New("run: 授权快照漂移——释放租约重新装配")
	// ErrResidueUnresolved 存在未处置 unknown 残留——再执行只发生在显式处置
	// 后（ADR-0030 兑现的执行面围栏：waiting 挡调度列之外的兜底——旧库
	// ready+残留收敛形态，0.6.7 发布前补齐）。
	ErrResidueUnresolved = errors.New("run: 存在未处置的 unknown 残留——先处置（resolve）")
	// ErrMissionTerminal Mission 已终态（accepted / canceled）——处置入口封闭
	//（0.6.9 审校 #2：验收完成后不得改写处置事实）。
	ErrMissionTerminal = errors.New("run: Mission 已终态，不可处置")
)

// Run 一次执行尝试（run_ 前缀；attempt 同 Mission 递增）。
type Run struct {
	RunID       string `json:"run_id"`
	MissionID   string `json:"mission_id"`
	Attempt     int    `json:"attempt"`
	LeaseID     string `json:"lease_id"`
	ManifestID  string `json:"manifest_id"`
	RuntimeID   string `json:"runtime_id"`
	BindingID   string `json:"binding_id"`
	AgentID     string `json:"agent_id"`
	State       string `json:"state"`
	ErrorText   string `json:"error_text"`
	ResultBlock string `json:"result_block"`
	FenceToken  string `json:"fence_token"`
	Generation  int    `json:"generation"`
	RequestedAt string `json:"requested_at"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	UpdatedAt   string `json:"updated_at"`
	// Deadline 墙钟期限（0.6.13 / ADR-0036 §3）：进入 running 时按执行开始
	// 契约快照 started_at + budget.max_wall_clock_seconds；空 = 无墙钟。修订
	// 不追改在途 Run 的 deadline（在途 Run 不静默换输入同族原则）。
	Deadline string `json:"deadline"`
	// 处置三列（0.6.7 / ADR-0030；非 unknown 恒空）：核验结论 / 备注 / 时刻。
	Resolution     string `json:"resolution"`
	ResolutionNote string `json:"resolution_note"`
	ResolvedAt     string `json:"resolved_at"`
}

// Event run_events 流事件。
type Event struct {
	Seq        int64          `json:"seq"`
	RunID      string         `json:"run_id"`
	EventType  string         `json:"event_type"`
	Payload    map[string]any `json:"payload"`
	OccurredAt string         `json:"occurred_at"`
}

// ExecutionInput 执行器输入：存档清单的完整内容块（契约/验收/近期消息——
// 审校 #1：执行器须拿到实际任务正文，非仅清单 ID）。
type ExecutionInput struct {
	RunID      string
	MissionID  string
	AgentID    string
	BindingID  string
	RuntimeID  string
	ManifestID string
	// Inline 执行语境——契约层内容块（目标/范围/验收标准/交付物/预算）
	// Inline 执行语境——契约层内容块（契约/验收/近期消息——审校 #1：执行器须拿到实际任务正文，非仅清单 ID）。
	Inline map[string]any
	// PreDeliver 交付前复核钩子（七审 #3：授权快照指纹——执行器须在**实际
	// 交付时点**（会话启动后、内容送出前）调用；非 nil 错误即放弃交付并把
	// 错误作为执行结果上报——停止/错误仲裁照常适用）。
	PreDeliver func() error
}

// Executor 执行面端口（阻塞至完成；app 注入 agent.Supervisor 适配）。
type Executor interface {
	Submit(ctx context.Context, input ExecutionInput) (block string, err error)
}

// ---- 调度器依赖端口（域自定义小端口，app 装配注入）----

// Missions 契约面端口：ready 候选 + 状态投影 + 控制恢复分派（0.6.6）。
type Missions interface {
	ListReady(ctx context.Context) ([]MissionRef, error)
	// RefOf 单任务权威快照（0.6.13 审校 #8：owner / slot 意图 / 预算自现行
	// 契约读——枚举与手动入口的 ref 可能陈旧（修订收紧/放宽、交接换人），
	// 启动前以此复核；查询失败中止当次派发，不用旧值启动）。
	RefOf(ctx context.Context, missionID string) (MissionRef, bool, error)
	// SetRunning ready → running；返回 changed（false = 状态不符——
	// 审校 #7：canceled 后拒启动须校验实际修改）。
	SetRunning(ctx context.Context, missionID string) (changed bool, err error)
	SetReadyFromRunning(ctx context.Context, missionID, reason string) error
	// StatusOf Mission 当前状态（恢复分派按停止意图定处置——ADR-0029 决策 4）。
	StatusOf(ctx context.Context, missionID string) (status string, ok bool, err error)
	// FinishCanceling canceling → canceled（重启时停止意图兑现收尾；幂等）。
	FinishCanceling(ctx context.Context, missionID, reason string) error
	// ListCanceling 枚举滞留 canceling 的 Mission（0.6.6 审校 P2：恢复面
	// 收尾兜底——"Run 已终态、收尾未兑现"不经未终态 Run 扫描可见）。
	ListCanceling(ctx context.Context) ([]MissionRef, error)
	// SetWaiting running → waiting(reason)（0.6.7 / ADR-0030：恢复分派结果不可
	// 判定——出调度列直至处置；幂等条件转移）。
	SetWaiting(ctx context.Context, missionID, reason string) error
	// ClearWaiting waiting → ready|draft（处置后回调度列，完备性重判，清等待
	// 原因；幂等条件转移——Mission 已取消/暂停时无操作）。
	ClearWaiting(ctx context.Context, missionID string) error
}

// MissionRef 调度候选摘要。
type MissionRef struct {
	MissionID    string
	OwnerAgentID string
	// SlotInstanceID 职责派发意图（0.6.13 审校 #1：slot 寻址派发的创建事实
	// ——执行装配按该实例固定 Binding（同人双任职不串用）；空 = 显式/无职责）。
	SlotInstanceID string
	// Budget 快照（0.6.13 / ADR-0036 §3，RefOf 权威读值；0 = 未设/不限）：
	// WallClockSeconds 随 Run 进入 running 落 deadline；MaxRuns 为该 Mission
	// 已创建 Run 总数上限（达限不再派发——任务停留 ready，诊断面计数；权威
	// 核对在 CreateRun 建行事务内，审校 #14）。
	WallClockSeconds int
	MaxRuns          int
}

// Bindings owner 的 active Binding 解析（执行者事实）。
type Bindings interface {
	ActiveOf(ctx context.Context, agentID string) (BindingFacts, bool, error)
	// ActiveOfInstance 按职责实例解析 active 任职（0.6.13 审校 #1：slot 意图
	// 任务的执行装配固定当时的 Binding——同人双任职不串用；无 active 任职
	// ok=false）。
	ActiveOfInstance(ctx context.Context, instanceID string) (BindingFacts, bool, error)
}

// BindingFacts 执行者事实（与 execctx.BindingFacts 同构——域间不互相 import）。
type BindingFacts struct {
	BindingID  string
	AgentID    string
	RuntimeID  string
	SlotSpecID string
}

// Runtimes ready 候选（按工作区排除占用——审校 #8）。
type Runtimes interface {
	FirstReady(ctx context.Context) (RuntimeRef, bool, error)
}

// RuntimeRef 运行时候选摘要。
type RuntimeRef struct {
	RuntimeID     string
	EnvironmentID string
}

// Leases 租约面（ADR-0027 服务同源语义）。
type Leases interface {
	Acquire(ctx context.Context, runtimeID, missionID string, ttlSeconds int, limits map[string]int) (Lease, error)
	Release(ctx context.Context, leaseID string) error
	CheckFence(ctx context.Context, leaseID, fenceToken string) (bool, string, error)
}

// Lease 租约摘要（run 域所需字段）。
type Lease struct {
	LeaseID       string
	FenceToken    string
	Generation    int
	EnvironmentID string
}

// Manifests 存档清单装配（ADR-0025 Build 同源）。
type Manifests interface {
	Build(ctx context.Context, missionID, bindingID string) (Manifest, error)
}

// Manifest 清单摘要。
type Manifest struct {
	ManifestID string
	State      string
	ReasonCode string
	// ContractVersion 清单钉住的契约版本（复审 #10：建行事务内核对现行版本
	// ——漂移即释放租约重新装配；预算自该版本契约层取，同快照不混版本）。
	ContractVersion int
	// GrantFingerprint 装配时授权快照指纹（四审 #3：建 Run 事务内与当前
	// 授权比对——可选撤销即漂移拒领，重新装配降级；空 = 测试件不核对）。
	GrantFingerprint string
	// Layers 内容块（审校 #1：执行输入取存档内容，非仅 ID）。
	Layers []ManifestLayer
}

// ManifestLayer 清单层摘要（服务层据此构造执行 Inline）。
type ManifestLayer struct {
	Name    string
	Content map[string]any
}

// Store 持久化端口。
type Store interface {
	// CreateRun 建 Run 行（attempt/事件同事务；0.6.13 审校 #14 + 复审 #4/#10：
	// maxRuns>0 时建行事务内核对已用次数（并发孪生恰一胜者，查询故障中止，
	// 超限 ErrBudgetExhausted）；manifestContractVersion>0 时核对现行契约版本
	//（漂移 ErrContractDrift——外层释放租约重新装配）；执行者门在建行事务
	// 内核对（负责人 active + 所钉任职仍 active 且未换人——ErrOwnerNotActive）。
	CreateRun(ctx context.Context, r Run, maxRuns, manifestContractVersion int, grantFingerprint string) (Run, error)
	// UpdateRun 全行替换 + 事件同事务。
	UpdateRun(ctx context.Context, r Run, events []Event) error
	// UpdateRunTerminal 终态写入 + **写锁后**墙钟裁决（四审 #6）：事务内以
	// nowFn() 现行时刻（锁后取值——等待写锁期间期限已过的窗口在此收口）
	// 核对行上 deadline——拟写 completed/failed 且期限已过 → 改按 canceled
	//（cause=wall_clock_exceeded）落终态，结果仅 late_result 审计不发布；
	// 否则按入参落。返回实际落库 Run（调用方以返回值为准）。
	UpdateRunTerminal(ctx context.Context, r Run, events []Event, nowFn func() string) (Run, error)
	RunByID(ctx context.Context, runID string) (Run, bool, error)
	ListRunsByMission(ctx context.Context, missionID string) ([]Run, error)
	ListUnfinished(ctx context.Context) ([]Run, error)
	RunEvents(ctx context.Context, runID string) ([]Event, error)
	// ListLegacyResidues 枚举旧版（0.6.5/0.6.6）崩溃残留终态行（failed 且
	// error_text 前缀匹配——升级收敛用；0.6.7 发布前补齐：此类行升级后无人
	// 认领，Mission 卡 running 无出口、ready 形态更被调度器自动重跑）。
	ListLegacyResidues(ctx context.Context, failedPrefix string) ([]Run, error)
	NextAttempt(ctx context.Context, missionID string) (int, error)
	// ResolveRun 条件处置（0.6.7）：state='unknown' AND resolution='' 才落三列
	// + 事件（单事务）；ok=false = 非 unknown/已处置（服务方重读分辨幂等）；
	// 未知 Run → ErrRunNotFound。
	ResolveRun(ctx context.Context, runID, resolution, note, resolvedAt string) (Run, bool, error)
	// AppendRunEvents 事件追加（不重写行——终态行只增审计）。
	AppendRunEvents(ctx context.Context, runID string, events []Event) error
	// ListUnresolvedUnknown 未处置 unknown Run（诊断计数）。
	ListUnresolvedUnknown(ctx context.Context) ([]Run, error)
	// ListExpiredRunning 墙钟过期 running Run（0.6.13 / ADR-0036 §3：
	// state=running 且 deadline 非空且 < now）。
	ListExpiredRunning(ctx context.Context, now string) ([]Run, error)
	// VerifyGrantFingerprint 交付前授权复核（六审基线：Run 建行后、输入交付前
	// 撤权——旧清单不得送达执行器；指纹不符 ErrGrantDrift。memstore 恒过）。
	VerifyGrantFingerprint(ctx context.Context, agentID, bindingID, fingerprint string) error
}

// 选项。
type Option func(*Service)

func WithNow(f func() string) Option          { return func(s *Service) { s.now = f } }
func WithNewRunID(f func() string) Option     { return func(s *Service) { s.newRunID = f } }
func WithSchedulerDisabled() Option           { return func(s *Service) { s.enabled = false } }
func WithScanInterval(d time.Duration) Option { return func(s *Service) { s.scanInterval = d } }

// WithWallClockInterval 墙钟监控独立 ticker 周期（0.6.13 审校 #4：监控面与
// 调度扫描分离——不被同步执行器阻塞；测试注入亚秒级）。
func WithWallClockInterval(d time.Duration) Option { return func(s *Service) { s.wallInterval = d } }

// WithCleanupBudget 清理面窗口（测试注入——窗口自收尾时刻起算，见
// cleanupContext；0.6.6 审校 P1）。
func WithCleanupBudget(d time.Duration) Option { return func(s *Service) { s.cleanupBudget = d } }

// WithStopGrace 停止后有界等待执行器退出窗口（测试注入；0.6.6 审校 P1）。
func WithStopGrace(d time.Duration) Option { return func(s *Service) { s.stopGrace = d } }

// Service 执行域服务。
type Service struct {
	store     Store
	missions  Missions
	bindings  Bindings
	runtimes  Runtimes
	leases    Leases
	manifests Manifests
	executor  Executor
	now       func() string
	newRunID  func() string
	stops     *stopRegistry // 控制停止注册表（0.6.6 / ADR-0029）

	scanInterval  time.Duration
	wallInterval  time.Duration // 墙钟监控独立 ticker（审校 #4——默认随 scanInterval）
	enabled       bool
	scans         atomic.Int64
	launched      atomic.Int64
	lastScanAt    atomic.Value
	lastError     atomic.Value
	stopCh        chan struct{}
	stopOnce      sync.Once
	cleanupBudget time.Duration // 清理面上下文窗口（收尾时刻起算——0.6.6 审校 P1）
	stopGrace     time.Duration // 停止后等执行器退出的有界窗口（0.6.6 审校 P1）
	budgetBlocked atomic.Int64  // 0.6.13 / ADR-0036 §3：max_runs 派发上限命中计数（诊断面）
}

// stopEntry 注册表项：每 Run 独立取消上下文的 cancel 与停止原因。
type stopEntry struct {
	cancel context.CancelFunc
	cause  string
}

// stopRegistry 控制停止注册表（进程内事实；意图已在库——mission paused/
// canceling——重启后由恢复分派兑现）。cause 在 signal 时写入（终理化前
// 可读回），cancel 幂等（context.CancelFunc 重复调用安全）。
type stopRegistry struct {
	mu sync.Mutex
	m  map[string]stopEntry
}

func newStopRegistry() *stopRegistry { return &stopRegistry{m: map[string]stopEntry{}} }

func (r *stopRegistry) register(runID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[runID] = stopEntry{cancel: cancel}
}

func (r *stopRegistry) unregister(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, runID)
}

// signal 记录 cause 并取消；runID 未注册（已终理化/未知）返回 false。
// first=true 表示本 Run 的首次信号（六审 #2：调用方据此把停止事实持久化到
// 该 Run 的事件流——resume 不清除，终态写事务据核对，防"暂停→恢复→旧 Run
// 完成发布"窗口）。
func (r *stopRegistry) signal(runID, cause string) (signalled, first bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[runID]
	if !ok {
		return false, false
	}
	first = e.cause == ""
	e.cause = cause
	r.m[runID] = e
	e.cancel()
	return true, first
}

func (r *stopRegistry) cause(runID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[runID]
	return e.cause, ok
}

// New 装配服务。
func New(store Store, missions Missions, bindings Bindings, runtimes Runtimes,
	leases Leases, manifests Manifests, executor Executor, opts ...Option) *Service {
	s := &Service{
		store: store, missions: missions, bindings: bindings, runtimes: runtimes,
		leases: leases, manifests: manifests, executor: executor,
		now:           func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newRunID:      func() string { return "run_" + randomSuffix() },
		stops:         newStopRegistry(),
		scanInterval:  5 * time.Second,
		wallInterval:  5 * time.Second,
		enabled:       true,
		stopCh:        make(chan struct{}),
		cleanupBudget: 30 * time.Second,
		stopGrace:     10 * time.Second,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func randomSuffix() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// Start 启动周期扫描 + 墙钟监控（0.6.13 审校 #4：**独立 ticker**——调度循环
// 同步串行执行 ExecuteMission，阻塞执行器会连带卡住尾部的过期扫描；监控面
// 与执行互不阻塞，按 RunID 直达停止）。
func (s *Service) Start(ctx context.Context) {
	if !s.enabled {
		return
	}
	go func() {
		ticker := time.NewTicker(s.scanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.ScanOnce(ctx)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(s.wallInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.enforceWallClock(ctx)
			}
		}
	}()
}

func (s *Service) Stop() { s.stopOnce.Do(func() { close(s.stopCh) }) }

// Status 调度器诊断面。
func (s *Service) Status(ctx context.Context) (map[string]any, error) {
	unfinished, err := s.store.ListUnfinished(ctx)
	if err != nil {
		return nil, err
	}
	unresolved, err := s.store.ListUnresolvedUnknown(ctx)
	if err != nil {
		return nil, err
	}
	lastScan, _ := s.lastScanAt.Load().(string)
	lastErr, _ := s.lastError.Load().(string)
	return map[string]any{
		"enabled": s.enabled, "scan_interval_seconds": int(s.scanInterval.Seconds()),
		"scans": s.scans.Load(), "launched_runs": s.launched.Load(),
		"active_runs": len(unfinished), "last_scan_at": lastScan, "last_error": lastErr,
		// 0.6.7 / RFC §11.5：记录 unknown 数（处置后归零）
		"unresolved_unknown_runs": len(unresolved),
		// 0.6.13 / ADR-0036 §3：max_runs 上限命中计数（诊断面）
		"budget_exhausted_hits": s.budgetBlocked.Load(),
	}, nil
}

// ScanOnce 扫一轮。
func (s *Service) ScanOnce(ctx context.Context) {
	s.scans.Add(1)
	s.lastScanAt.Store(s.now())
	candidates, err := s.missions.ListReady(ctx)
	if err != nil {
		s.lastError.Store(err.Error())
		return
	}
	var lastErr string
	for _, ref := range candidates {
		if _, err := s.ExecuteMission(ctx, ref); err != nil {
			switch {
			case errors.Is(err, ErrSchedulerBusy), errors.Is(err, ErrNoExecutor),
				errors.Is(err, ErrBudgetExhausted),                                  // 0.6.13：上限命中是常态（诊断面计数），不入扫描错误
				errors.Is(err, ErrOwnerNotActive), errors.Is(err, ErrContractDrift), // 复审 #4/#10：身份门/版本漂移——任务停留 ready，下轮重试
				errors.Is(err, ErrGrantDrift): // 四审 #3：授权漂移——重新装配降级，下轮重试
			default:
				lastErr = fmt.Sprintf("%s: %v", ref.MissionID, err)
			}
		}
	}
	if lastErr != "" {
		s.lastError.Store(lastErr)
	} else {
		s.lastError.Store("")
	}
	s.enforceWallClock(ctx)
}

// enforceWallClock 墙钟强制（0.6.13 / ADR-0036 §3；审校 #4 + 复核备注重写）：
// 扫描过期 running Run → **按 RunID 直达**停止注册表（signal → 在途 select 收
// 取消 → finalizeStopped：run canceled + cause 事件 + Mission 回 ready；迟到
// 结果只审计）。不经 Mission 再扫描——同 Mission 换发的新 Run 未到期不可误伤
// （审校"额外检查"指出的交错）；已终态 Run 注册表已注销（signal=false 自然
// 幂等）。由独立 ticker 驱动（Start），不随执行器阻塞。
func (s *Service) enforceWallClock(ctx context.Context) {
	expired, err := s.store.ListExpiredRunning(ctx, s.now())
	if err != nil {
		s.lastError.Store("wallclock scan: " + err.Error())
		return
	}
	// 七审 #7：信号路径**零持久写**——监控只取消进程内上下文（行上期限即权
	// 停止事实——终态写事务核对；控制停止的持久标记在暂停/取消**事务内**写
	// 入，见 sqlite transitionMissionIf）；两个 Run 同时超时不再被首条事件写
	// 拖住第二个的取消。
	for _, r := range expired {
		s.stops.signal(r.RunID, CauseWallClockExceeded)
	}
}

// ExecuteMission 调度并执行一次（同步面）。安全编排（v0.2 审校）：
//   - #3：提交前围栏复核（失效/检查错误均拒启动 → Run failed + 释放）
//   - #4：围栏核对 + 终态写入同事务（FinalizeRun）
//   - #5：清理面用独立 context（请求断开后终态/释放/投影不遗留）
//   - #7：SetRunning 返回 changed——canceled 后拒启动（Run 置 failed）
func (s *Service) ExecuteMission(ctx context.Context, ref MissionRef) (Run, error) {
	// 0.6.13 审校 #8：权威快照复核——枚举/手动入口的 ref 可能陈旧（修订
	// 收紧/放宽预算、交接换人），启动前一律重读现行契约（owner / slot 意图 /
	// 预算同一快照）；查询失败**中止**当次派发（不用旧值启动）。
	auth, found, err := s.missions.RefOf(ctx, ref.MissionID)
	if err != nil {
		return Run{}, fmt.Errorf("run: 权威快照复核失败（%s）：%w", ref.MissionID, err)
	}
	if !found {
		return Run{}, fmt.Errorf("%w（%s）", ErrMissionNotFound, ref.MissionID)
	}
	ref = auth
	// 执行面围栏（0.6.7 发布前补齐）：ready 形态下存在未处置 unknown 残留
	// （resolution 空）即拒启动——"再执行只发生在显式处置后"的兜底。waiting
	// 形态由调度列/条件投影自然拒（不在此拦，保持"手动触发 → Run failed 留
	// 档"的既有语义）；旧库 ready+残留收敛形态是唯一入口洞。查询失败中止
	//（审校 #14：不吞——围栏不可知不启动）。
	runs, err := s.store.ListRunsByMission(ctx, ref.MissionID)
	if err != nil {
		return Run{}, fmt.Errorf("run: 残留围栏读取失败（%s）：%w", ref.MissionID, err)
	}
	for _, r := range runs {
		if r.State == StateUnknown && r.Resolution == "" {
			if st, _, serr := s.missions.StatusOf(ctx, ref.MissionID); serr == nil && st == "ready" {
				return Run{}, fmt.Errorf("%w（mission %s，run %s）", ErrResidueUnresolved, ref.MissionID, r.RunID)
			}
			break
		}
	}
	// 0.6.13 / ADR-0036 §3：max_runs 派发上限——已创建 Run 总数（终态含）达
	// 契约上限即不再派发（任务停留 ready 不被静默吞，诊断面计数）。快路径
	// 省租约/清单；**权威核对在 CreateRun 建行事务内**（审校 #14：并发孪生
	// 与计数查询故障都在事务面收敛）。
	if ref.MaxRuns > 0 && len(runs) >= ref.MaxRuns {
		s.budgetBlocked.Add(1)
		return Run{}, fmt.Errorf("%w（mission %s：%d/%d）", ErrBudgetExhausted, ref.MissionID, len(runs), ref.MaxRuns)
	}
	// 执行者解析（0.6.13 审校 #1）：slot 意图任务按**实例**固定任职（同人双
	// 任职不串用——决策时的职责即执行职责，任职换人/失效拒启动重新派发）；
	// 显式/缺省 owner 保留原候选选择（首个 active 任职）。
	var binding BindingFacts
	if ref.SlotInstanceID != "" {
		b, bok, berr := s.bindings.ActiveOfInstance(ctx, ref.SlotInstanceID)
		if berr != nil {
			return Run{}, fmt.Errorf("run: 职责任职解析失败（%s）：%w", ref.SlotInstanceID, berr)
		}
		if !bok || b.AgentID != ref.OwnerAgentID {
			return Run{}, fmt.Errorf("%w：slot %s 任职失效/换人（预期 %s）——重新派发", ErrNoExecutor, ref.SlotInstanceID, ref.OwnerAgentID)
		}
		binding = b
	} else {
		b, bok, berr := s.bindings.ActiveOf(ctx, ref.OwnerAgentID)
		if berr != nil {
			return Run{}, fmt.Errorf("run: 解析执行者失败：%w", berr)
		}
		if !bok {
			return Run{}, ErrNoExecutor
		}
		binding = b
	}
	rt, ok, err := s.runtimes.FirstReady(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("run: 解析运行时失败：%w", err)
	}
	if !ok {
		return Run{}, ErrNoExecutor
	}

	now := s.now()
	lease, err := s.leases.Acquire(ctx, rt.RuntimeID, ref.MissionID, 0, nil)
	if err != nil {
		return Run{}, err
	}
	s.launched.Add(1)

	manifest, err := s.manifests.Build(ctx, ref.MissionID, binding.BindingID)
	if err != nil {
		_ = s.leases.Release(ctx, lease.LeaseID)
		return Run{}, fmt.Errorf("run: 装配清单失败：%w", err)
	}
	if manifest.State != "assembled" {
		_ = s.leases.Release(ctx, lease.LeaseID)
		return Run{}, fmt.Errorf("%w：%s", ErrManifestBlocked, manifest.ReasonCode)
	}
	// 复审 #10：预算单快照——自清单钉住版本的**契约层**取（墙钟 + max_runs），
	// 与执行输入同一版本；RefOf 的预算仅作快路径参考（漂移由 CreateRun 事务
	// 核对收敛：版本漂移释放租约重新装配，次数以现行契约为准）。
	mWallClock, mMaxRuns := budgetOfManifest(manifest)
	if mMaxRuns == 0 {
		mMaxRuns = ref.MaxRuns // 清单契约层缺省（测试件）回落 RefOf 快照
	}

	r := Run{
		RunID: s.newRunID(), MissionID: ref.MissionID,
		LeaseID: lease.LeaseID, ManifestID: manifest.ManifestID,
		RuntimeID: rt.RuntimeID, BindingID: binding.BindingID, AgentID: ref.OwnerAgentID,
		State:       StatePending,
		FenceToken:  lease.FenceToken,
		Generation:  lease.Generation,
		RequestedAt: now, UpdatedAt: now,
	}
	r, err = s.store.CreateRun(ctx, r, mMaxRuns, manifest.ContractVersion, manifest.GrantFingerprint)
	if err != nil {
		_ = s.leases.Release(ctx, lease.LeaseID)
		// 事务内权威上限命中（并发孪生败者——审校 #14）：计数并透传（手动
		// 入口 409 budget_exhausted，调度面常态跳过）。复审 #4/#10：身份门/
		// 版本漂移——任务停留 ready，释放租约后重试/重装配。
		if errors.Is(err, ErrBudgetExhausted) {
			s.budgetBlocked.Add(1)
		}
		return Run{}, err
	}

	// 0.6.6 审校 P1：停止句柄先于 Mission 投影注册——CreateRun 落库后 Run 即
	// 可被 SignalStopByMission 扫描可见；句柄若晚于 SetRunning 注册，其间落地
	// 的暂停（CAS 已翻转 paused）扫描得到 Run 而注册表无句柄——信号丢失、
	// 执行照启结果照落。注册先行后该窗口内暂停二选一：信号命中取消 runCtx，
	// 或 SetRunning changed=false 拒启动。
	// （0.6.6 / ADR-0029 决策 2：执行上下文脱离请求——Background 派生每 Run
	// 取消上下文。请求断开不中止执行；停止只经控制面或进程退出恢复面。）
	runCtx, runCancel := context.WithCancel(context.Background())
	s.stops.register(r.RunID, runCancel)
	defer func() {
		s.stops.unregister(r.RunID)
		runCancel()
	}()

	// #7：SetRunning 须实际修改（changed=false = Mission 已 canceled/非 ready
	// ——执行不启动，Run 置 failed 携原因）
	changed, err := s.missions.SetRunning(ctx, ref.MissionID)
	if err != nil {
		ferr := s.finalizeWithCleanup(func(cc context.Context) error {
			return s.finalizeFailed(cc, &r, "Mission 状态投影失败: "+err.Error(), lease.LeaseID)
		})
		if ferr != nil {
			return r, ferr
		}
		return r, nil
	}
	if !changed {
		ferr := s.finalizeWithCleanup(func(cc context.Context) error {
			return s.finalizeFailed(cc, &r, "Mission 已取消或不再 ready——执行不启动", lease.LeaseID)
		})
		if ferr != nil {
			return r, ferr
		}
		return r, nil
	}

	now = s.now()
	r.State = StateRunning
	r.StartedAt = now
	r.UpdatedAt = now
	// 0.6.13 / ADR-0036 §3 + 复审 #10：墙钟快照取**清单契约层**的
	// max_wall_clock_seconds（与执行输入同一版本——RefOf 后修订撤回的长期限
	// 不得沿用；清单缺省回落 RefOf 快照）。
	wallClock := mWallClock
	if wallClock == 0 {
		wallClock = ref.WallClockSeconds
	}
	if wallClock > 0 {
		if t, perr := time.Parse(time.RFC3339Nano, now); perr == nil {
			r.Deadline = t.Add(time.Duration(wallClock) * time.Second).UTC().Format(time.RFC3339Nano)
		}
	}
	if err := s.store.UpdateRun(ctx, r, []Event{{
		RunID: r.RunID, EventType: EventStarted, OccurredAt: now, Payload: map[string]any{},
	}}); err != nil {
		ferr := s.finalizeWithCleanup(func(cc context.Context) error {
			return s.finalizeFailed(cc, &r, "run.started 落库失败: "+err.Error(), lease.LeaseID)
		})
		if ferr != nil {
			return r, ferr
		}
		return r, nil
	}

	// #3：提交前围栏复核——失效或检查出错均拒绝启动
	if valid, reason, ferr := s.leases.CheckFence(ctx, lease.LeaseID, lease.FenceToken); ferr != nil || !valid {
		why := reason
		if ferr != nil {
			why = "检查错误: " + ferr.Error()
		}
		if uerr := s.finalizeWithCleanup(func(cc context.Context) error {
			return s.finalizeFenceRejected(cc, &r, lease.LeaseID, why, "")
		}); uerr != nil {
			return r, uerr
		}
		return r, nil
	}

	// 提交执行（阻塞至完成；#1：Inline 携存档清单内容块——非仅 ID）。
	// 被停止时放弃执行器 goroutine——其结果经缓冲通道丢弃（迟到结果不落，
	// 围栏已随释放失效）。
	// 七审 #3/#8：交付前授权复核挂**执行输入钩子**——goroutine 入口先核（执
	// 行器未实现钩子时的底线），真实适配器在会话启动后、内容送出前再核（实
	// 际交付时点）。失败经结果通道上报：停止意图在场（暂停/取消已信号）→
	// Done 分支按停止终理化（canceled，#8）；否则 failed → 终态事务再仲裁
	//（暂停映射 pause → canceled）。
	// 八审 #6：复核闭包用 **runCtx**（脱离请求上下文——客户端断开不使授权
	// 读取失败把 Run 记 failed）。
	preDeliver := func() error {
		if manifest.GrantFingerprint == "" {
			return nil
		}
		return s.store.VerifyGrantFingerprint(runCtx, r.AgentID, r.BindingID, manifest.GrantFingerprint)
	}
	done := make(chan execResult, 1)
	go func() {
		// 八审 #3：首次交付前核对该 Run 的**持久停止事实**（建行后、句柄注册
		// 前暂停再恢复——标记在册而注册表无信号；读取失败同样阻止交付）。
		// 命中即不交付，错误走结果通道 → 终态事务仲裁按标记改判 canceled。
		if stopped, cerr := s.readPersistedStopCause(runCtx, r.RunID); cerr != nil || stopped != "" {
			err := fmt.Errorf("停止事实在交付前已持久（%s）——不交付", stopped)
			if cerr != nil {
				err = fmt.Errorf("停止事实读取失败（交付阻止）: %w", cerr)
			}
			done <- execResult{err: err}
			return
		}
		if err := preDeliver(); err != nil {
			done <- execResult{err: fmt.Errorf("授权快照漂移（交付前复核）——重新装配: %w", err)}
			return
		}
		block, err := s.executor.Submit(runCtx, ExecutionInput{
			RunID: r.RunID, MissionID: r.MissionID, AgentID: r.AgentID,
			BindingID: r.BindingID, RuntimeID: r.RuntimeID, ManifestID: r.ManifestID,
			Inline: manifestInline(manifest), PreDeliver: preDeliver,
		})
		done <- execResult{block: block, err: err}
	}()

	select {
	case res := <-done:
		// #4：围栏核对 + 终态写入同事务（FinalizeRun——防释放后旧结果落库）。
		// 清理面上下文自此刻起算（0.6.6 审校 P1——窗口不再覆盖执行时长）。
		cleanupCtx, cleanupCancel := s.cleanupContext()
		defer cleanupCancel()
		// 停止与执行结果同时就绪的 select 竞态（0.6.10 复审 #1/#2 + 0.6.13 复审
		// #9）：终态判定在**完成提交前**原子进行——停止意图在场，或期限已过
		//（独立监控的墙钟信号必然伴随行上 deadline 已过——按期限判定不依赖
		// 注册表读取时序，结果分支读取与围栏检查之间发停不再漏网），一律按
		// 停止终理化：迟到正文只审计不发布（AT-12）。ok 仅表示已注册
		//（register 的 cause 为空串）——signal 过（非空 cause）才是已停止意图，
		// 纯执行结果/错误不误判。已取出的结果放回缓冲通道（恰容量 1，必不
		// 阻塞）交 finalizeStopped 等待并审计、释放恰一次——不留空通道让收尾
		// 白等 stopGrace（租约滞留到 TTL）。
		stopCause := ""
		if cause, ok := s.stops.cause(r.RunID); ok && cause != "" {
			stopCause = cause
		} else if deadlinePassed(r.Deadline, s.now()) {
			stopCause = CauseWallClockExceeded
		}
		if stopCause != "" {
			done <- res
			if err := s.finalizeStopped(cleanupCtx, &r, lease, stopCause, done); err != nil {
				return r, fmt.Errorf("%w：%v", ErrTerminalWrite, err)
			}
			return r, nil
		}
		finalize := func(state, errText, block string) error {
			err := s.finalizeRun(cleanupCtx, &r, lease, state, errText, block, "")
			if so, ok := err.(*stopOverrideError); ok {
				// 三审 #5：完成提交前的停止改判——正文回填缓冲通道交停止收尾
				// 审计（只审计不发布），按停止终理化。
				done <- res
				if ferr := s.finalizeStopped(cleanupCtx, &r, lease, so.cause, done); ferr != nil {
					return fmt.Errorf("%w：%v", ErrTerminalWrite, ferr)
				}
				return nil
			}
			return err
		}
		if res.err != nil {
			if err := finalize(StateFailed, truncateS(res.err.Error(), 1000), ""); err != nil {
				return r, fmt.Errorf("%w：%v（执行错误：%v）", ErrTerminalWrite, err, res.err)
			}
			return r, nil
		}
		if err := finalize(StateCompleted, "", truncateS(res.block, 8000)); err != nil {
			return r, fmt.Errorf("%w：%v", ErrTerminalWrite, err)
		}
		return r, nil
	case <-runCtx.Done():
		// 控制停止（Mission 已 paused/canceling——持久意图先行）：Run →
		// canceled + stop_requested 审计 + 租约释放；Mission 投影不动。
		cause, _ := s.stops.cause(r.RunID)
		if cause == "" {
			cause = "unknown"
		}
		cleanupCtx, cleanupCancel := s.cleanupContext()
		defer cleanupCancel()
		if err := s.finalizeStopped(cleanupCtx, &r, lease, cause, done); err != nil {
			return r, fmt.Errorf("%w：%v", ErrTerminalWrite, err)
		}
		return r, nil
	}
}

// execResult 执行器单次结果（缓冲通道传递——停止后迟到结果丢弃面）。
type execResult struct {
	block string
	err   error
}

// cleanupContext #5 清理面独立上下文：窗口自收尾时刻起算（0.6.6 审校 P1：
// 此前自执行开始起表——任务时长超过窗口预算（缺省 30s）即完成时已耗尽，
// 围栏检查/终态写入/租约释放全部随过期上下文静默失败，Run 滞留 running、
// 租约滞留 active）。
func (s *Service) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cleanupBudget)
}

// finalizeWithCleanup 前置失败路径的清理面入口（窗口同自此刻起算）。七审
// #9：f 的错误上抛——终态写入失败不得伪装成功（调用方返回 ErrTerminalWrite，
// 手动入口 5xx 而非 201；Run 滞留 running 由恢复面收敛）。
func (s *Service) finalizeWithCleanup(f func(ctx context.Context) error) error {
	ctx, cancel := s.cleanupContext()
	defer cancel()
	return f(ctx)
}

// readPersistedStopCause 读取该 Run 的持久停止标记 cause（八审 #3/#4：空串
// = 无标记；err = 读取失败——调用方按阻止交付/上抛处理，不得当不存在）。
func (s *Service) readPersistedStopCause(ctx context.Context, runID string) (string, error) {
	evs, err := s.store.RunEvents(ctx, runID)
	if err != nil {
		return "", err
	}
	cause := ""
	for _, e := range evs {
		if e.EventType == EventStopRequested {
			if c, ok := e.Payload["cause"].(string); ok && c != "" {
				cause = c
			}
		}
	}
	return cause, nil
}

// hasPersistedStopMarker 该 Run 是否已有持久 stop_requested（七审 #10：
// 终态事件组按存在性去重——恰一条停止审计）。
func (s *Service) hasPersistedStopMarker(ctx context.Context, runID string) bool {
	evs, err := s.store.RunEvents(ctx, runID)
	if err != nil {
		return false
	}
	for _, e := range evs {
		if e.EventType == EventStopRequested {
			return true
		}
	}
	return false
}

// SignalStopByMission 控制停止信号（ADR-0029 决策 2/3）：对 Mission 全部
// 未终态 Run 记录 cause 并取消其执行上下文。停止意图须已持久化（Mission
// paused/canceling——控制编排先行 CAS）；本方法只发信号，不等待、不代行
// 终理化（执行 goroutine 观察取消后自行终理化）。返回信号数（0 = 无在途
// ——无在途取消可同步直达终态）。
func (s *Service) SignalStopByMission(ctx context.Context, missionID, cause string) (int, error) {
	runs, err := s.store.ListRunsByMission(ctx, missionID)
	if err != nil {
		return 0, fmt.Errorf("run: 停止信号扫描失败：%w", err)
	}
	n := 0
	for _, r := range runs {
		if r.State != StatePending && r.State != StateRunning {
			continue
		}
		if signalled, _ := s.stops.signal(r.RunID, cause); signalled {
			n++
		}
	}
	return n, nil
}

// Unsettled Mission 的在途（pending/running）与未处置 unknown Run 计数
// （0.6.9 审校 #2：送验前置拦截——review.Runs 端口）。
func (s *Service) Unsettled(ctx context.Context, missionID string) (int, int, error) {
	runs, err := s.store.ListRunsByMission(ctx, missionID)
	if err != nil {
		return 0, 0, err
	}
	inflight, unresolved := 0, 0
	for _, r := range runs {
		switch {
		case r.State == StatePending || r.State == StateRunning:
			inflight++
		case r.State == StateUnknown && r.Resolution == "":
			unresolved++
		}
	}
	return inflight, unresolved, nil
}

// HasUnfinished Mission 是否仍有未终态 Run（控制面有界等待用）。
func (s *Service) HasUnfinished(ctx context.Context, missionID string) (bool, error) {
	runs, err := s.store.ListRunsByMission(ctx, missionID)
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		if r.State == StatePending || r.State == StateRunning {
			return true, nil
		}
	}
	return false, nil
}

// stopOverrideError 完成提交前的停止改判（三审 #5：围栏通过后、终态写入前
// 重新仲裁发现停止意图或期限已过——零写入返回，调用方改走 finalizeStopped，
// 迟到正文只审计不发布）。
type stopOverrideError struct{ cause string }

func (e *stopOverrideError) Error() string {
	return "run: 完成提交前停止改判（" + e.cause + "）"
}

// finalizeRun #4/#5：围栏核对 + 终态写入在同一存储事务内完成——核对不
// 通过或写入失败，结果不落库（fence_rejected 审计或返回 ErrTerminalWrite
// 给调用方，补偿面见调用方）。使用独立清理上下文（请求断开后不遗留）。
func (s *Service) finalizeRun(ctx context.Context, r *Run, lease Lease, state, errText, resultBlock, _ string) error {
	now := s.now()

	// #4：围栏核对在此（与终态写入紧邻——核对通过到写入之间无释放窗口；
	// 真正的原子化经存储事务面由 UpdateRun 承担——围栏失效写入 rejected 而非终态）
	valid, fenceReason, ferr := s.leases.CheckFence(ctx, lease.LeaseID, lease.FenceToken)
	if ferr != nil {
		// 检查出错：结果不落，Run failed 携原因（保守拒绝——不因检查故障放行）
		r.State = StateFailed
		r.ErrorText = "围栏检查错误（保守拒绝）: " + ferr.Error()
		r.FinishedAt = now
		r.UpdatedAt = now
		_ = s.store.UpdateRun(ctx, *r, []Event{{
			RunID: r.RunID, EventType: EventFenceRejected, OccurredAt: now,
			Payload: map[string]any{"reason": "check_error", "detail": ferr.Error()},
		}})
		_ = s.leases.Release(ctx, lease.LeaseID)
		_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "fence_check_error")
		return nil
	}
	if !valid {
		// 围栏拒绝：审计事件 + Run failed（结果不落）
		r.State = StateFailed
		r.ErrorText = "围栏拒绝（" + fenceReason + "）——迟到结果不落"
		r.FinishedAt = now
		r.UpdatedAt = now
		if err := s.store.UpdateRun(ctx, *r, []Event{{
			RunID: r.RunID, EventType: EventFenceRejected, OccurredAt: now,
			Payload: map[string]any{"reason": fenceReason, "detail": truncateS(errText, 400)},
		}}); err != nil {
			return fmt.Errorf("围栏拒绝事件落库失败：%w", err)
		}
		_ = s.leases.Release(ctx, lease.LeaseID)
		_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "fence_rejected")
		return nil
	}

	// 围栏通过 → 终态写入（失败返回 ErrTerminalWrite 给调用方——#12）。
	// 三审 #5：终态判定与提交同步——围栏检查期间监控发停（或此刻已过期限）
	// 的窗口在此收口：写入前**重新**读取停止意图与行上期限（fresh now），停
	// 止成立则改按停止终理化（迟到正文只审计）；残余窗口缩至判定与写入之间
	// 的数行（期限判定用行上权威值，监控信号必然伴随其已过）。
	{
		now2 := s.now()
		stopCause := ""
		if cause, ok := s.stops.cause(r.RunID); ok && cause != "" {
			stopCause = cause
		} else if deadlinePassed(r.Deadline, now2) {
			stopCause = CauseWallClockExceeded
		}
		if stopCause != "" {
			// 五审（既有 P1）：失败终态同样并入停止归档——停止意图在场时无论
			// 成功正文还是错误结果一律按停止终理化（迟到只审计）。
			return &stopOverrideError{cause: stopCause} // 零写入——调用方改走停止终理化
		}
	}
	r.State = state
	r.ErrorText = errText
	r.ResultBlock = resultBlock
	r.FinishedAt = now
	r.UpdatedAt = now
	eventType := EventCompleted
	if state == StateFailed {
		eventType = EventFailed
	}
	// 四审 #6：终态写入经 UpdateRunTerminal——**取得写锁后**以现行时刻核对
	// 行上期限（等待写锁期间期限已过/监控已发停的窗口在此收口）；期限已过
	// 改按 canceled 落库（迟到正文只审计），返回实际终态。
	finalRun, err := s.store.UpdateRunTerminal(ctx, *r, []Event{{
		RunID: r.RunID, EventType: eventType, OccurredAt: now,
		Payload: map[string]any{"late": false},
	}}, s.now)
	if err != nil {
		return fmt.Errorf("终态写入失败：%w", err)
	}
	*r = finalRun
	_ = s.leases.Release(ctx, lease.LeaseID)
	// 五审 #5：投影原因取**实际落库终态**（写锁内改判 canceled → run_canceled，
	// 与 finalizeStopped 同因——Run 行、事件流、Mission 状态原因三者重建同一结果）。
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "run_"+r.State)
	return nil
}

// finalizeFailed 失败路径（Run failed + 租约释放 + Mission 回 ready）——
// 使用独立上下文。
func (s *Service) finalizeFailed(ctx context.Context, r *Run, errText, leaseID string) error {
	now := s.now()
	r.State = StateFailed
	r.ErrorText = truncateS(errText, 1000)
	r.FinishedAt = now
	r.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, *r, []Event{{
		RunID: r.RunID, EventType: EventFailed, OccurredAt: now,
		Payload: map[string]any{"error": r.ErrorText},
	}}); err != nil {
		// 七审 #9：终态写失败上抛（租约/投影不重复施做——恢复面统一收尾）
		return fmt.Errorf("%w：%v", ErrTerminalWrite, err)
	}
	_ = s.leases.Release(ctx, leaseID)
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "run_failed")
	return nil
}

func (s *Service) finalizeFenceRejected(ctx context.Context, r *Run, leaseID, reason, detail string) error {
	now := s.now()
	r.State = StateFailed
	r.ErrorText = "提交前围栏复核失败（" + reason + "）——执行不启动"
	r.FinishedAt = now
	r.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, *r, []Event{{
		RunID: r.RunID, EventType: EventFenceRejected, OccurredAt: now,
		Payload: map[string]any{"reason": reason, "phase": "pre_submit", "detail": detail},
	}}); err != nil {
		return fmt.Errorf("%w：%v", ErrTerminalWrite, err) // 七审 #9：同上
	}
	_ = s.leases.Release(ctx, leaseID)
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "fence_pre_submit")
	return nil
}

// finalizeStopped 控制停止终理化（0.6.6 / ADR-0029 决策 2）：Run →
// canceled——单次 UpdateRun 同事务写 stop_requested{cause} + canceled 两
// 事件；围栏核对保留（租约在手——失效/检查错误则追加 fence_rejected 审计，
// 状态仍 canceled——停止事实不因围栏检查故障而悬置）。租约释放后于执行器
// 退出（0.6.6 审校 P1：停止信号只取消上下文，执行器可能仍在收尾写工作区
// ——先行释放即允许新执行同时改同一工作区；有界等待）。Mission 投影不动：
// 停止路径下 Mission 必为 paused/canceling（非 running）——
// SetReadyFromRunning 条件转移幂等无操作（ADR-0028 条件更新语义保证；
// 防御性保留调用以兜底非控制路径的取消）。使用独立清理上下文（自此刻
// 起算）。终态写库失败上抛 ErrTerminalWrite（0.6.6 审校 P2：不得吞错——
// Run 滞留 running + 租约滞留 active 的事实对齐，不释放，恢复面兜底）。
func (s *Service) finalizeStopped(ctx context.Context, r *Run, lease Lease, cause string, executorDone <-chan execResult) error {
	now := s.now()
	valid, fenceReason, ferr := s.leases.CheckFence(ctx, lease.LeaseID, lease.FenceToken)

	r.State = StateCanceled
	r.FinishedAt = now
	r.UpdatedAt = now
	events := []Event{
		{RunID: r.RunID, EventType: EventCanceled, OccurredAt: now,
			Payload: map[string]any{"cause": cause}},
	}
	// 七审 #10：stop_requested 恰一条——暂停/取消事务已持久写入标记（或此前
	// 已终理化）时不再追加（正常暂停不再产生两条审计）。
	if !s.hasPersistedStopMarker(ctx, r.RunID) {
		events = append([]Event{{
			RunID: r.RunID, EventType: EventStopRequested, OccurredAt: now,
			Payload: map[string]any{"cause": cause},
		}}, events...)
	}
	if ferr != nil || !valid {
		reason := fenceReason
		if ferr != nil {
			reason = "check_error: " + ferr.Error()
		}
		r.ErrorText = "停止时围栏核对未过（" + reason + "）——结果不落"
		events = append(events, Event{RunID: r.RunID, EventType: EventFenceRejected, OccurredAt: now,
			Payload: map[string]any{"reason": reason, "phase": "stop"}})
	}
	if err := s.store.UpdateRun(ctx, *r, events); err != nil {
		return fmt.Errorf("停止终态写入失败：%w", err)
	}
	// 租约释放后于执行器退出（0.6.6 审校 P1；0.6.7 发布前补齐修正）：真实面
	// ctx 取消即杀进程组（毫秒级退出）；超窗时**不得释放**——执行器可能仍在
	// 收尾写工作区，释放即允许新执行同写。持有租约 + 后台等执行器退出（先补
	// 迟到审计再释放——0.6.7 / ADR-0030 决策 4 AT-12"旧结果只审计"）；进程先
	// 退出由租约 TTL 兜底——独占性优先，可用性损失由 TTL 界。
	select {
	case res := <-executorDone:
		s.auditLateResult(ctx, r.RunID, r.Generation, res, "after_stop")
		_ = s.leases.Release(ctx, lease.LeaseID)
	case <-time.After(s.stopGrace):
		go func() {
			s.awaitLateResult(r.RunID, r.Generation, executorDone)
			rctx, rcancel := s.cleanupContext()
			defer rcancel()
			_ = s.leases.Release(rctx, lease.LeaseID)
		}()
	}
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "run_canceled")
	return nil
}

// auditLateResult 迟到结果审计事件（事件追加——不重写终态行；写失败 best-effort
// ——审计面无回报方，不影响已落的停止事实）。
func (s *Service) auditLateResult(ctx context.Context, runID string, generation int, res execResult, phase string) {
	outcome, preview := "ok", res.block
	if res.err != nil {
		outcome, preview = "error", res.err.Error()
	}
	_ = s.store.AppendRunEvents(ctx, runID, []Event{{
		RunID: runID, EventType: EventLateResult, OccurredAt: s.now(),
		Payload: map[string]any{
			"outcome": outcome, "preview": truncateS(preview, 400),
			"generation": generation, "published": false, "phase": phase,
		},
	}})
}

// awaitLateResult 宽限后的后台等待审计（每个被放弃的执行器一个 goroutine——
// 有界于被放弃执行数；进程退出即消亡，重启后无进程内迟到源）。
func (s *Service) awaitLateResult(runID string, generation int, executorDone <-chan execResult) {
	res := <-executorDone
	ctx, cancel := s.cleanupContext()
	defer cancel()
	s.auditLateResult(ctx, runID, generation, res, "after_stop")
}

// ResolveRun 核验处置（0.6.7 / ADR-0030 决策 3——禁止盲目重跑的兑现）：
// unknown Run 的用户 attest——**状态保持 unknown**（系统未观测到完成，不冒充
// 系统事实），处置事实落 resolution 三列 + resolved 事件（存储条件更新：
// state='unknown' AND resolution=”）；随后 Mission waiting → ready|draft
// （ClearWaiting 条件转移——已取消/暂停无操作，处置事实仍落 Run）。幂等：
// 同结论返回当前；异结论 ErrRunAlreadyResolved；非 unknown ErrRunNotUnknown。
func (s *Service) ResolveRun(ctx context.Context, runID, outcome, note string) (Run, error) {
	switch outcome {
	case ResolutionVerifiedCompleted, ResolutionVerifiedFailed, ResolutionDiscarded:
	default:
		return Run{}, fmt.Errorf("%w: outcome 须为 verified_completed | verified_failed | discarded", ErrInvalidResolution)
	}
	if utf8.RuneCountInString(note) > 500 {
		return Run{}, fmt.Errorf("%w: note ≤500 字", ErrInvalidResolution)
	}
	// 审校 #2：终态 Mission 封处置入口（先取 Run 归属再查 Mission 状态——未知
	// Run 仍按 ErrRunNotFound 报）
	if cur, found, err := s.store.RunByID(ctx, runID); err != nil {
		return Run{}, err
	} else if found {
		if st, ok, err := s.missions.StatusOf(ctx, cur.MissionID); err != nil {
			return Run{}, err
		} else if ok && (st == "accepted" || st == "canceled") {
			return Run{}, fmt.Errorf("%w（Mission %s）", ErrMissionTerminal, st)
		}
	}
	out, ok, err := s.store.ResolveRun(ctx, runID, outcome, note, s.now())
	if err != nil {
		return Run{}, err
	}
	if !ok {
		cur, found, err := s.store.RunByID(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		if !found {
			return Run{}, ErrRunNotFound
		}
		if cur.State != StateUnknown {
			return Run{}, fmt.Errorf("%w（当前 %s）", ErrRunNotUnknown, cur.State)
		}
		if cur.Resolution == outcome {
			// 幂等：处置事实已落，不重复记事件——但投影须补（首趟 ClearWaiting
			// 失败时 Mission 卡 waiting，幂等重试不得短路投影；0.6.7 发布前补齐）
			if err := s.missions.ClearWaiting(ctx, cur.MissionID); err != nil {
				return cur, fmt.Errorf("run: 处置已落库（%s）但 Mission 投影失败：%w（幂等重试已再试投影）", outcome, err)
			}
			return cur, nil
		}
		return Run{}, fmt.Errorf("%w（已 %s）", ErrRunAlreadyResolved, cur.Resolution)
	}
	// Mission 回调度列（waiting → ready|draft）——用户显式处置后方可再执行。
	// 投影失败上抛（0.6.7 发布前补齐）：处置已落库而 Mission 卡 waiting 无
	// 感知——调用方须见错误；同结论幂等重试会补投影。
	if err := s.missions.ClearWaiting(ctx, out.MissionID); err != nil {
		return out, fmt.Errorf("run: 处置已落库（%s）但 Mission 投影失败：%w（重试同结论将补投影）", outcome, err)
	}
	return out, nil
}

// RecoverUnfinished #6 + 0.6.6 / ADR-0029 决策 4：未终态 Run 按 Mission
// 停止意图分派——
//   - canceling：Run → canceled（停止请求在途时重启——结果未知）+ 收尾
//     兑现（canceling → canceled）
//   - paused：Run → canceled（同上）+ Mission 停 paused
//   - 其余（running，无停止意图）：Run → unknown（结果不可判定——0.6.7 /
//     ADR-0030；此前 failed）+ Mission → waiting(reconciliation)（出调度列，
//     等待用户核验处置——禁止盲目重跑）
func (s *Service) RecoverUnfinished(ctx context.Context) (int, error) {
	unfinished, err := s.store.ListUnfinished(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range unfinished {
		now := s.now()
		mStatus, _, _ := s.missions.StatusOf(ctx, r.MissionID)
		if mStatus == "canceling" || mStatus == "paused" {
			// 八审 #4 + 九审 #2：动作码映射（paused→pause / canceling→cancel）；
			// stop_requested 仅在无持久标记时写（标记**读取失败**上抛跳过——
			// 不得当不存在重复写）；canceled 写失败保留 running 可重试（恢复
			// 返回错误，不再静默报成功）。
			cause := "pause"
			if mStatus == "canceling" {
				cause = "cancel"
			}
			mk, mErr := s.readPersistedStopCause(ctx, r.RunID)
			if mErr != nil {
				return n, fmt.Errorf("run: 恢复读取停止标记失败（%s）：%w", r.RunID, mErr)
			}
			r.State = StateCanceled
			r.ErrorText = "停止请求在途时进程重启——结果未知"
			r.FinishedAt = now
			r.UpdatedAt = now
			evs := []Event{
				{RunID: r.RunID, EventType: EventCanceled, OccurredAt: now,
					Payload: map[string]any{"cause": cause, "recovery": true}},
			}
			if mk == "" { // 无持久标记才补写（去重）
				evs = append([]Event{{
					RunID: r.RunID, EventType: EventStopRequested, OccurredAt: now,
					Payload: map[string]any{"cause": cause, "recovery": true},
				}}, evs...)
			}
			if err := s.store.UpdateRun(ctx, r, evs); err != nil {
				return n, fmt.Errorf("run: 恢复兑现停止失败（%s，保留 running 可重试）：%w", r.RunID, err)
			}
			_ = s.leases.Release(ctx, r.LeaseID)
			if mStatus == "canceling" {
				_ = s.missions.FinishCanceling(ctx, r.MissionID, "recovery_stop")
			}
			n++
			continue
		}
		// 七审 #6 + 八审 #4/#5：Run 自身的持久停止标记（暂停/取消事务内写入
		//——此后恢复/重启不清除）→ 按停止兑现收尾（canceled 携**原停止动作码**
		// 与 recovery 标记，Mission 回调度列）。标记**读取失败**上抛并跳过该
		// Run（不得当不存在误判 unknown）；canceled 写失败保留 running 可重试
		//（不落 unknown/waiting，恢复返回错误）。
		if mCause, mErr := s.readPersistedStopCause(ctx, r.RunID); mErr != nil {
			return n, fmt.Errorf("run: 恢复读取停止标记失败（%s）：%w", r.RunID, mErr)
		} else if mCause != "" {
			r.State = StateCanceled
			r.ErrorText = "停止意图在重启前已持久（恢复面兑现）"
			r.FinishedAt = now
			r.UpdatedAt = now
			if err := s.store.UpdateRun(ctx, r, []Event{
				{RunID: r.RunID, EventType: EventCanceled, OccurredAt: now,
					Payload: map[string]any{"cause": mCause, "recovery": true}},
			}); err != nil {
				return n, fmt.Errorf("run: 恢复兑现停止失败（%s，保留 running 可重试）：%w", r.RunID, err)
			}
			_ = s.leases.Release(ctx, r.LeaseID)
			_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "run_canceled")
			n++
			continue
		}
		// 0.6.7 / ADR-0030 决策 1/2：无停止意图的在途 Run → unknown（结果不可
		// 判定——非 failed）+ 租约释放（进程已死）+ Mission → waiting
		// (reconciliation)（出调度列直至用户处置——禁止盲目重跑）。
		r.State = StateUnknown
		r.ErrorText = "进程重启时在执行——结果未知；须核验工作区副作用后处置（verified_completed / verified_failed / discarded），不会自动重跑"
		r.FinishedAt = now
		r.UpdatedAt = now
		if err := s.store.UpdateRun(ctx, r, []Event{{
			RunID: r.RunID, EventType: EventUnknown, OccurredAt: now,
			Payload: map[string]any{"recovery": true},
		}}); err == nil {
			_ = s.leases.Release(ctx, r.LeaseID)
			_ = s.missions.SetWaiting(ctx, r.MissionID, WaitingReconciliation)
			n++
		}
	}

	// 0.6.6 审校 P2：滞留 canceling 兜底——收尾兑现不只跟随未终态 Run：
	// "Run 已终态（停止写库成功）、进程在控制面 FinishCancel 前退出"的
	// Mission 无未终态 Run，须经状态枚举发现（幂等——未终态路径上面的循环
	// 已收过，此处只补无在途面）。
	canceling, err := s.missions.ListCanceling(ctx)
	if err != nil {
		return n, fmt.Errorf("run: 恢复枚举 canceling 失败：%w", err)
	}
	for _, ref := range canceling {
		has, herr := s.HasUnfinished(ctx, ref.MissionID)
		if herr != nil || has {
			continue // 枚举瞬变（上面的循环刚处置或并发新建）——下轮恢复再兜
		}
		if err := s.missions.FinishCanceling(ctx, ref.MissionID, "recovery_stop"); err == nil {
			n++
		}
	}

	// 0.6.7 发布前补齐：旧库（0.6.5/0.6.6）崩溃残留终态行收敛——failed+
	// "结果未知"文案行升级后无人认领（Mission 卡 running 无出口；ready 形态
	// 被调度器自动重跑——禁盲目重跑洞）。行转 unknown（与崩溃新语义一致，
	// 处置面即可认领；事件追加不抹历史），Mission running|ready → waiting
	// （ready 增列与 0.6.6"ready 可暂停"同理）；paused/canceling/canceled
	// 不动（处置后 ClearWaiting 条件跳过）。幂等：前缀只匹配旧文案，新语义
	// 行已是 unknown 不再命中。
	legacy, err := s.store.ListLegacyResidues(ctx, legacyResiduePrefix)
	if err != nil {
		return n, fmt.Errorf("run: 旧库残留枚举失败：%w", err)
	}
	for _, r := range legacy {
		now := s.now()
		r.State = StateUnknown
		r.ErrorText = "进程重启时在执行——结果未知（0.6.6 前形态，升级收敛）；须核验工作区副作用后处置，不会自动重跑"
		r.UpdatedAt = now
		if err := s.store.UpdateRun(ctx, r, []Event{{
			RunID: r.RunID, EventType: EventUnknown, OccurredAt: now,
			Payload: map[string]any{"recovery": true, "upgrade": true},
		}}); err != nil {
			continue
		}
		// running 形态进 waiting（出调度列）；ready 形态不动（RFC：waiting 挂
		// running）——执行面围栏挡自动重跑，处置后照常放行。
		_ = s.missions.SetWaiting(ctx, r.MissionID, WaitingReconciliation)
		n++
	}
	return n, nil
}

// AppendReceipt 外域回执追加（0.6.8 / ADR-0031：交付包提交审计
// delivery_submitted 落来源 Run 流——事件追加不改 Run 行）。
func (s *Service) AppendReceipt(ctx context.Context, runID, eventType string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	return s.store.AppendRunEvents(ctx, runID, []Event{{
		RunID: runID, EventType: eventType, OccurredAt: s.now(), Payload: payload,
	}})
}

// GetRun / ListRuns / RunEventsByRun 读面。
func (s *Service) GetRun(ctx context.Context, runID string) (Run, bool, error) {
	return s.store.RunByID(ctx, runID)
}

func (s *Service) ListRuns(ctx context.Context, missionID string) ([]Run, error) {
	return s.store.ListRunsByMission(ctx, missionID)
}

func (s *Service) RunEventsByRun(ctx context.Context, runID string) ([]Event, bool, error) {
	if _, ok, err := s.store.RunByID(ctx, runID); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, nil
	}
	evs, err := s.store.RunEvents(ctx, runID)
	return evs, true, err
}

// deadlinePassed 期限判定（复审 #9：完成提交前的原子终态判定用——行上
// deadline 已过即视为墙钟停止成立，不依赖停止注册表的读取时序）。
func deadlinePassed(deadline, nowRFC string) bool {
	if deadline == "" {
		return false
	}
	dl, err1 := time.Parse(time.RFC3339Nano, deadline)
	now, err2 := time.Parse(time.RFC3339Nano, nowRFC)
	if err1 != nil || err2 != nil {
		return false
	}
	return !now.Before(dl)
}

// budgetOfManifest 清单契约层预算（复审 #10：墙钟 + max_runs 与执行输入同
// 一版本快照——契约层嵌 Mission 全量，budget 键在层内容中）。
func budgetOfManifest(m Manifest) (wallClockSeconds, maxRuns int) {
	for _, l := range m.Layers {
		if l.Name != "contract" || l.Content == nil {
			continue
		}
		raw, ok := l.Content["budget"]
		if !ok {
			return 0, 0
		}
		b, ok := raw.(map[string]any)
		if !ok {
			return 0, 0
		}
		return toInt(b["max_wall_clock_seconds"]), toInt(b["max_runs"])
	}
	return 0, 0
}

// toInt 层内容数值（JSON 往返为 float64；测试件可能放 int/int64）。
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// truncateS 上限按 rune 计（0.6.7 发布前补齐：按字节截中文既提前丢内容
// （8000 字节 ≈ 2666 字）又产出无效 UTF-8；ASCII 快路径字节即 rune 不变）。
func truncateS(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// manifestInline #1：存档清单层内容 → 执行 Inline（契约/验收/近期消息/
// 工具清单——执行器拿到实际任务正文）。
func manifestInline(m Manifest) map[string]any {
	inline := map[string]any{
		"manifest_id": m.ManifestID,
		"mission_run": true,
	}
	for _, l := range m.Layers {
		if l.Content == nil {
			continue
		}
		switch l.Name {
		case "contract":
			inline["objective"] = l.Content["objective"]
			inline["scope"] = l.Content["scope"]
			inline["acceptance_criteria"] = l.Content["acceptance_criteria"]
			inline["deliverable_specs"] = l.Content["deliverable_specs"]
			inline["constraints"] = l.Content["constraints"]
			inline["budget"] = l.Content["budget"]
		case "responsibility":
			inline["responsibility"] = l.Content["responsibility"]
		case "recent_messages":
			if msgs, ok := l.Content["messages"]; ok {
				inline["recent_messages"] = msgs
			}
		case "tools":
			inline["tools_required"] = l.Content["required"]
			inline["tools_optional"] = l.Content["optional"]
		case "constraints":
			inline["readiness"] = l.Content["readiness"]
		}
	}
	return inline
}
