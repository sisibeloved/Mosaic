// Package run：单 Agent Mission 执行（0.6.5 / ADR-0028）——Run 域（一次
// 执行尝试）+ 持久调度器 + 执行器端口。0.6.2–0.6.4 三件底座（存档清单 /
// 环境与运行时 / 租约与围栏）在此首次被完整消费：ready Mission → 领取
// 租约 → 装配存档清单 → 创建 Run（Mission → running）→ 提交执行 →
// 围栏核对 → Run 终态 + 租约释放（Mission 回 ready 可再执行）。
//
// 与房间 Run（internal/room/runs.go，M4-1 讨论面任务）不是同一对象：
// 这是 Mission 执行域的独立事实（RFC §6"任务事实放 Mission 流"的执行段）。
package run

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Run 状态（pending 为事务内瞬态——落库即 running；unknown/暂停随 0.6.7/0.6.6）。
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
)

// 事件类型（run_events 流）。
const (
	EventAttemptRequested = "attempt_requested"
	EventStarted          = "started"
	EventCompleted        = "completed"
	EventFailed           = "failed"
	EventFenceRejected    = "fence_rejected"
)

// 领域错误。
var (
	// ErrRunNotFound Run 不存在。
	ErrRunNotFound = errors.New("run: Run 不存在")
	// ErrMissionNotFound Mission 不存在。
	ErrMissionNotFound = errors.New("run: Mission 不存在")
	// ErrMissionNotReady Mission 非 ready（running/canceled/draft）。
	ErrMissionNotReady = errors.New("run: Mission 非 ready")
	// ErrNoExecutor 无可用执行面（owner 无 active Binding / 无能力满足的 ready Runtime）。
	ErrNoExecutor = errors.New("run: 无可用执行面")
	// ErrSchedulerBusy 资源被占（租约冲突——本轮跳过，下轮再试）。
	ErrSchedulerBusy = errors.New("run: 资源被占")
	// ErrManifestBlocked 存档清单装配 blocked（原因在清单——返回给调用方）。
	ErrManifestBlocked = errors.New("run: 执行清单 blocked")
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
}

// Event run_events 流事件。
type Event struct {
	Seq        int64          `json:"seq"`
	RunID      string         `json:"run_id"`
	EventType  string         `json:"event_type"`
	Payload    map[string]any `json:"payload"`
	OccurredAt string         `json:"occurred_at"`
}

// ExecutionInput 执行器输入（存档清单的关键内容块经 app 适配转 Inline）。
type ExecutionInput struct {
	RunID      string
	MissionID  string
	AgentID    string
	BindingID  string
	RuntimeID  string
	ManifestID string
	// Inline 执行语境（契约层内容块 + 近期消息窗——由服务层从存档清单提取）。
	Inline map[string]any
}

// Executor 执行面端口（阻塞至完成；app 注入 agent.Supervisor 适配）。
type Executor interface {
	Submit(ctx context.Context, input ExecutionInput) (block string, err error)
}

// ---- 调度器依赖端口（域自定义小端口，app 装配注入）----

// Missions 契约面端口：ready 候选 + 状态投影。
type Missions interface {
	// ListReady 返回 ready 且无未终态 Run 的 Mission 摘要（调度候选）。
	ListReady(ctx context.Context) ([]MissionRef, error)
	// SetRunning / SetReadyFromRunning Mission 状态投影（running ↔ ready）。
	SetRunning(ctx context.Context, missionID string) error
	SetReadyFromRunning(ctx context.Context, missionID, reason string) error
}

// MissionRef 调度候选摘要。
type MissionRef struct {
	MissionID    string
	OwnerAgentID string
}

// Bindings owner 的 active Binding 解析（执行者事实）。
type Bindings interface {
	// ActiveOf 返回 agent 当前 active 的 BindingFacts（无 active → ok=false）。
	ActiveOf(ctx context.Context, agentID string) (BindingFacts, bool, error)
}

// BindingFacts 执行者事实（与 execctx.BindingFacts 同构——域间不互相 import）。
type BindingFacts struct {
	BindingID  string
	AgentID    string
	RuntimeID  string // 0.6.5：暂由 Runtime 候选解析而非 Binding 携带
	SlotSpecID string
}

// Runtimes ready 候选（单 Agent：发现序首个 ready 且无活跃租约的运行时）。
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
	// Build 装配并存档；blocked 也返回（replayed=false）——调用方判 State。
	Build(ctx context.Context, missionID, bindingID string) (Manifest, error)
}

// Manifest 清单摘要。
type Manifest struct {
	ManifestID string
	State      string // assembled | blocked
	ReasonCode string
}

// Store 持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
type Store interface {
	// CreateRun 建 Run（attempt = 该 Mission 既有最大 +1）+ attempt_requested
	// 事件；未终态行存在（部分唯一索引兜底）→ ErrRunExists。
	CreateRun(ctx context.Context, r Run) (Run, error)
	// UpdateRun 全行替换 + 事件同事务。
	UpdateRun(ctx context.Context, r Run, events []Event) error
	RunByID(ctx context.Context, runID string) (Run, bool, error)
	ListRunsByMission(ctx context.Context, missionID string) ([]Run, error)
	ListUnfinished(ctx context.Context) ([]Run, error)
	RunEvents(ctx context.Context, runID string) ([]Event, error)
	// NextAttempt 该 Mission 下一 attempt 序号（无 Run → 1）。
	NextAttempt(ctx context.Context, missionID string) (int, error)
}

// 选项。
type Option func(*Service)

// WithNow 注入时钟。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewRunID 注入 Run ID 生成器（测试定序）。
func WithNewRunID(f func() string) Option { return func(s *Service) { s.newRunID = f } }

// Service 执行域服务（调度 + 执行编排）。
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

	// 调度器
	scanInterval time.Duration
	enabled      bool
	scans        atomic.Int64
	launched     atomic.Int64
	lastScanAt   atomic.Value // string
	lastError    atomic.Value // string
	stopCh       chan struct{}
	stopOnce     sync.Once
	inflight     sync.Map // run_id → struct{}（防同 Run 双执行）
}

// New 装配服务。
func New(store Store, missions Missions, bindings Bindings, runtimes Runtimes,
	leases Leases, manifests Manifests, executor Executor, opts ...Option) *Service {
	s := &Service{
		store: store, missions: missions, bindings: bindings, runtimes: runtimes,
		leases: leases, manifests: manifests, executor: executor,
		now:          func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newRunID:     func() string { return "run_" + randomSuffix() },
		scanInterval: 5 * time.Second,
		enabled:      true,
		stopCh:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithSchedulerDisabled 关周期扫描（测试装配）。
func WithSchedulerDisabled() Option { return func(s *Service) { s.enabled = false } }

// WithScanInterval 注入扫描间隔（测试加速）。
func WithScanInterval(d time.Duration) Option { return func(s *Service) { s.scanInterval = d } }

func randomSuffix() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// Start 启动周期扫描（持久调度——全部事实在库，调度器自身无状态）。
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
}

// Stop 停扫描（在途执行随 ctx 取消）。
func (s *Service) Stop() { s.stopOnce.Do(func() { close(s.stopCh) }) }

// Status 调度器诊断面。
func (s *Service) Status(ctx context.Context) (map[string]any, error) {
	unfinished, err := s.store.ListUnfinished(ctx)
	if err != nil {
		return nil, err
	}
	lastScan, _ := s.lastScanAt.Load().(string)
	lastErr, _ := s.lastError.Load().(string)
	return map[string]any{
		"enabled": s.enabled, "scan_interval_seconds": int(s.scanInterval.Seconds()),
		"scans": s.scans.Load(), "launched_runs": s.launched.Load(),
		"active_runs": len(unfinished), "last_scan_at": lastScan, "last_error": lastErr,
	}, nil
}

// ScanOnce 扫一轮：对每个 ready 候选尝试调度（错误不夺整轮）。
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
			case errors.Is(err, ErrSchedulerBusy), errors.Is(err, ErrNoExecutor):
				// 资源面常态：下轮再试，不记错误
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
}

// ExecuteMission 调度并执行一次（手动触发与扫描共用；同步面——返回时
// Run 已终态）。资源忙/无执行面返回错误由调用方分类；其余错误也落 Run
// failed（attempt 已消耗）。
func (s *Service) ExecuteMission(ctx context.Context, ref MissionRef) (Run, error) {
	// 候选解析：owner active Binding → ready Runtime（单 Agent 首个）
	binding, ok, err := s.bindings.ActiveOf(ctx, ref.OwnerAgentID)
	if err != nil {
		return Run{}, fmt.Errorf("run: 解析执行者失败：%w", err)
	}
	if !ok {
		return Run{}, ErrNoExecutor
	}
	rt, ok, err := s.runtimes.FirstReady(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("run: 解析运行时失败：%w", err)
	}
	if !ok {
		return Run{}, ErrNoExecutor
	}

	now := s.now()
	// 领取租约（mission 为占用主体；容量/工作区冲突 → 资源忙下轮再试）
	lease, err := s.leases.Acquire(ctx, rt.RuntimeID, ref.MissionID, 0, nil)
	if err != nil {
		return Run{}, err // ErrLeaseConflict* → 调用方归 ErrSchedulerBusy 口径由 httpapi 映射
	}
	s.launched.Add(1)

	release := func(reason string) { _ = s.leases.Release(ctx, lease.LeaseID) }
	_ = release

	// 装配存档清单（执行存档输入——ADR-0025）；blocked → 记审计释放租约
	manifest, err := s.manifests.Build(ctx, ref.MissionID, binding.BindingID)
	if err != nil {
		_ = s.leases.Release(ctx, lease.LeaseID)
		return Run{}, fmt.Errorf("run: 装配清单失败：%w", err)
	}
	if manifest.State != "assembled" {
		_ = s.leases.Release(ctx, lease.LeaseID)
		return Run{}, fmt.Errorf("%w：%s", ErrManifestBlocked, manifest.ReasonCode)
	}

	// 创建 Run（attempt 递增）+ Mission → running（同事务面——两库操作
	// 按 mission 投影先行，失败即回滚面见下方 defer）
	attempt, err := s.store.NextAttempt(ctx, ref.MissionID)
	if err != nil {
		_ = s.leases.Release(ctx, lease.LeaseID)
		return Run{}, err
	}
	r := Run{
		RunID: s.newRunID(), MissionID: ref.MissionID, Attempt: attempt,
		LeaseID: lease.LeaseID, ManifestID: manifest.ManifestID,
		RuntimeID: rt.RuntimeID, BindingID: binding.BindingID, AgentID: ref.OwnerAgentID,
		State:       StatePending,
		FenceToken:  lease.FenceToken,
		Generation:  lease.Generation,
		RequestedAt: now, UpdatedAt: now,
	}
	r, err = s.store.CreateRun(ctx, r)
	if err != nil {
		_ = s.leases.Release(ctx, lease.LeaseID)
		return Run{}, err
	}
	if err := s.missions.SetRunning(ctx, ref.MissionID); err != nil {
		// 投影失败：Run 置 failed 携原因（不自动重试——下轮候选面重见）
		s.failRun(ctx, &r, "Mission 状态投影失败: "+err.Error(), lease.LeaseID)
		return r, nil
	}

	// started + running
	now = s.now()
	r.State = StateRunning
	r.StartedAt = now
	r.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, r, []Event{{
		RunID: r.RunID, EventType: EventStarted, OccurredAt: now, Payload: map[string]any{},
	}}); err != nil {
		s.failRun(ctx, &r, "run.started 落库失败: "+err.Error(), lease.LeaseID)
		return r, nil
	}

	// 提交执行（阻塞至完成）
	block, execErr := s.executor.Submit(ctx, ExecutionInput{
		RunID: r.RunID, MissionID: r.MissionID, AgentID: r.AgentID,
		BindingID: r.BindingID, RuntimeID: r.RuntimeID, ManifestID: r.ManifestID,
		Inline: manifestInline(manifest),
	})

	// 围栏核对（ADR-0027 四关——迟到旧实例提交被拒绝）
	now = s.now()
	valid, fenceReason, ferr := s.leases.CheckFence(ctx, lease.LeaseID, lease.FenceToken)
	if ferr != nil == false && !valid {
		// 围栏拒绝：审计事件 + Run failed（结果不落）
		ev := Event{RunID: r.RunID, EventType: EventFenceRejected, OccurredAt: now,
			Payload: map[string]any{"reason": fenceReason, "detail": truncateS(execErrText(execErr), 400)}}
		r.State = StateFailed
		r.ErrorText = "围栏拒绝（" + fenceReason + "）——迟到结果不落"
		r.FinishedAt = now
		r.UpdatedAt = now
		_ = s.store.UpdateRun(ctx, r, []Event{ev})
		_ = s.missions.SetReadyFromRunning(ctx, ref.MissionID, "fence_rejected")
		return r, nil
	}
	if execErr != nil {
		s.failRun(ctx, &r, execErr.Error(), lease.LeaseID)
		return r, nil
	}
	// completed：正文截断存档 + 租约释放 + Mission 回 ready
	r.State = StateCompleted
	r.ResultBlock = truncateS(block, 8000)
	r.FinishedAt = now
	r.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, r, []Event{{
		RunID: r.RunID, EventType: EventCompleted, OccurredAt: now,
		Payload: map[string]any{"late": false},
	}}); err != nil {
		s.lastError.Store(err.Error())
	}
	_ = s.leases.Release(ctx, lease.LeaseID)
	_ = s.missions.SetReadyFromRunning(ctx, ref.MissionID, "run_completed")
	return r, nil
}

// failRun 失败路径：Run failed + failed 事件 + 租约释放 + Mission 回 ready。
func (s *Service) failRun(ctx context.Context, r *Run, errText string, leaseID string) {
	now := s.now()
	r.State = StateFailed
	r.ErrorText = truncateS(errText, 1000)
	r.FinishedAt = now
	r.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, *r, []Event{{
		RunID: r.RunID, EventType: EventFailed, OccurredAt: now,
		Payload: map[string]any{"error": r.ErrorText},
	}}); err != nil {
		s.lastError.Store(err.Error())
	}
	_ = s.leases.Release(ctx, leaseID)
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "run_failed")
}

// RecoverUnfinished 启动恢复：未终态 Run → failed（结果未知——0.6.7 的
// unknown 处理；本版不自动重跑，RFC"禁止盲目重跑"裁定）。
func (s *Service) RecoverUnfinished(ctx context.Context) (int, error) {
	unfinished, err := s.store.ListUnfinished(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range unfinished {
		now := s.now()
		r.State = StateFailed
		r.ErrorText = "进程重启时在执行——结果未知；不会自动重跑（可能已产生副作用）"
		r.FinishedAt = now
		r.UpdatedAt = now
		if err := s.store.UpdateRun(ctx, r, []Event{{
			RunID: r.RunID, EventType: EventFailed, OccurredAt: now,
			Payload: map[string]any{"error": r.ErrorText, "recovery": true},
		}}); err == nil {
			_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "restart_recovery")
			_ = s.leases.Release(ctx, r.LeaseID)
			n++
		}
	}
	return n, nil
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

// ErrRunExists 未终态 Run 已存在（每 Mission 至多一个）。
var ErrRunExists = errors.New("run: 已有未终态 Run")

func truncateS(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func execErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// manifestInline 存档清单 → 执行语境（本版最小面：契约标识与清单引用；
// 完整内容块消费面随 0.6.6+ 深化——先保通道语义正确）。
func manifestInline(m Manifest) map[string]any {
	return map[string]any{
		"manifest_id": m.ManifestID,
		"mission_run": true,
	}
}
