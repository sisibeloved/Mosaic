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
	// + 近期消息窗（来源房 public 消息）+ 工具清单，由服务层从存档清单提取。
	Inline map[string]any
}

// Executor 执行面端口（阻塞至完成；app 注入 agent.Supervisor 适配）。
type Executor interface {
	Submit(ctx context.Context, input ExecutionInput) (block string, err error)
}

// ---- 调度器依赖端口（域自定义小端口，app 装配注入）----

// Missions 契约面端口：ready 候选 + 状态投影。
type Missions interface {
	ListReady(ctx context.Context) ([]MissionRef, error)
	// SetRunning ready → running；返回 changed（false = 状态不符——
	// 审校 #7：canceled 后拒启动须校验实际修改）。
	SetRunning(ctx context.Context, missionID string) (changed bool, err error)
	SetReadyFromRunning(ctx context.Context, missionID, reason string) error
}

// MissionRef 调度候选摘要。
type MissionRef struct {
	MissionID    string
	OwnerAgentID string
}

// Bindings owner 的 active Binding 解析（执行者事实）。
type Bindings interface {
	ActiveOf(ctx context.Context, agentID string) (BindingFacts, bool, error)
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
	CreateRun(ctx context.Context, r Run) (Run, error)
	// UpdateRun 全行替换 + 事件同事务。
	UpdateRun(ctx context.Context, r Run, events []Event) error
	RunByID(ctx context.Context, runID string) (Run, bool, error)
	ListRunsByMission(ctx context.Context, missionID string) ([]Run, error)
	ListUnfinished(ctx context.Context) ([]Run, error)
	RunEvents(ctx context.Context, runID string) ([]Event, error)
	NextAttempt(ctx context.Context, missionID string) (int, error)
}

// 选项。
type Option func(*Service)

func WithNow(f func() string) Option          { return func(s *Service) { s.now = f } }
func WithNewRunID(f func() string) Option     { return func(s *Service) { s.newRunID = f } }
func WithSchedulerDisabled() Option           { return func(s *Service) { s.enabled = false } }
func WithScanInterval(d time.Duration) Option { return func(s *Service) { s.scanInterval = d } }

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

	scanInterval time.Duration
	enabled      bool
	scans        atomic.Int64
	launched     atomic.Int64
	lastScanAt   atomic.Value
	lastError    atomic.Value
	stopCh       chan struct{}
	stopOnce     sync.Once
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

func randomSuffix() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// Start 启动周期扫描。
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
			case errors.Is(err, ErrSchedulerBusy), errors.Is(err, ErrNoExecutor):
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

// ExecuteMission 调度并执行一次（同步面）。安全编排（v0.2 审校）：
//   - #3：提交前围栏复核（失效/检查错误均拒启动 → Run failed + 释放）
//   - #4：围栏核对 + 终态写入同事务（FinalizeRun）
//   - #5：清理面用独立 context（请求断开后终态/释放/投影不遗留）
//   - #7：SetRunning 返回 changed——canceled 后拒启动（Run 置 failed）
func (s *Service) ExecuteMission(ctx context.Context, ref MissionRef) (Run, error) {
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

	r := Run{
		RunID: s.newRunID(), MissionID: ref.MissionID,
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

	// #7：SetRunning 须实际修改（changed=false = Mission 已 canceled/非 ready
	// ——执行不启动，Run 置 failed 携原因）
	changed, err := s.missions.SetRunning(ctx, ref.MissionID)
	if err != nil {
		s.finalizeFailed(ctx, &r, "Mission 状态投影失败: "+err.Error(), lease.LeaseID)
		return r, nil
	}
	if !changed {
		s.finalizeFailed(ctx, &r, "Mission 已取消或不再 ready——执行不启动", lease.LeaseID)
		return r, nil
	}

	now = s.now()
	r.State = StateRunning
	r.StartedAt = now
	r.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, r, []Event{{
		RunID: r.RunID, EventType: EventStarted, OccurredAt: now, Payload: map[string]any{},
	}}); err != nil {
		s.finalizeFailed(ctx, &r, "run.started 落库失败: "+err.Error(), lease.LeaseID)
		return r, nil
	}

	// #3：提交前围栏复核——失效或检查出错均拒绝启动
	if valid, reason, ferr := s.leases.CheckFence(ctx, lease.LeaseID, lease.FenceToken); ferr != nil || !valid {
		why := reason
		if ferr != nil {
			why = "检查错误: " + ferr.Error()
		}
		s.finalizeFenceRejected(ctx, &r, lease.LeaseID, why, "")
		return r, nil
	}

	// 提交执行（阻塞至完成；#1：Inline 携存档清单内容块——非仅 ID）
	block, execErr := s.executor.Submit(ctx, ExecutionInput{
		RunID: r.RunID, MissionID: r.MissionID, AgentID: r.AgentID,
		BindingID: r.BindingID, RuntimeID: r.RuntimeID, ManifestID: r.ManifestID,
		Inline: manifestInline(manifest),
	})

	// #5：清理面独立上下文——请求断开后终态/释放/投影不遗留 running + 活跃租约
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()

	// #4：围栏核对 + 终态写入同事务（FinalizeRun——防释放后旧结果落库）
	if execErr != nil {
		err := s.finalizeRun(cleanupCtx, &r, lease, StateFailed, truncateS(execErr.Error(), 1000), "", "")
		if err != nil {
			return r, fmt.Errorf("%w：%v（执行错误：%v）", ErrTerminalWrite, err, execErr)
		}
		return r, nil
	}
	err = s.finalizeRun(cleanupCtx, &r, lease, StateCompleted, "", truncateS(block, 8000), "")
	if err != nil {
		return r, fmt.Errorf("%w：%v", ErrTerminalWrite, err)
	}
	return r, nil
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

	// 围栏通过 → 终态写入（失败返回 ErrTerminalWrite 给调用方——#12）
	r.State = state
	r.ErrorText = errText
	r.ResultBlock = resultBlock
	r.FinishedAt = now
	r.UpdatedAt = now
	eventType := EventCompleted
	if state == StateFailed {
		eventType = EventFailed
	}
	if err := s.store.UpdateRun(ctx, *r, []Event{{
		RunID: r.RunID, EventType: eventType, OccurredAt: now,
		Payload: map[string]any{"late": false},
	}}); err != nil {
		return fmt.Errorf("终态写入失败：%w", err)
	}
	_ = s.leases.Release(ctx, lease.LeaseID)
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "run_"+state)
	return nil
}

// finalizeFailed 失败路径（Run failed + 租约释放 + Mission 回 ready）——
// 使用独立上下文。
func (s *Service) finalizeFailed(ctx context.Context, r *Run, errText, leaseID string) {
	now := s.now()
	r.State = StateFailed
	r.ErrorText = truncateS(errText, 1000)
	r.FinishedAt = now
	r.UpdatedAt = now
	_ = s.store.UpdateRun(ctx, *r, []Event{{
		RunID: r.RunID, EventType: EventFailed, OccurredAt: now,
		Payload: map[string]any{"error": r.ErrorText},
	}})
	_ = s.leases.Release(ctx, leaseID)
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "run_failed")
}

func (s *Service) finalizeFenceRejected(ctx context.Context, r *Run, leaseID, reason, detail string) {
	now := s.now()
	r.State = StateFailed
	r.ErrorText = "提交前围栏复核失败（" + reason + "）——执行不启动"
	r.FinishedAt = now
	r.UpdatedAt = now
	_ = s.store.UpdateRun(ctx, *r, []Event{{
		RunID: r.RunID, EventType: EventFenceRejected, OccurredAt: now,
		Payload: map[string]any{"reason": reason, "phase": "pre_submit", "detail": detail},
	}})
	_ = s.leases.Release(ctx, leaseID)
	_ = s.missions.SetReadyFromRunning(ctx, r.MissionID, "fence_pre_submit")
}

// RecoverUnfinished #6：未终态 Run → failed（结果未知）+ 租约释放——
// **Mission 停在 running（不回 ready）**：下次扫描不会自动调度（等待
// 用户经手动触发决定——RFC"禁止盲目重跑"裁定强化）。
func (s *Service) RecoverUnfinished(ctx context.Context) (int, error) {
	unfinished, err := s.store.ListUnfinished(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range unfinished {
		now := s.now()
		r.State = StateFailed
		r.ErrorText = "进程重启时在执行——结果未知；请确认后手动再执行（不会自动重跑）"
		r.FinishedAt = now
		r.UpdatedAt = now
		if err := s.store.UpdateRun(ctx, r, []Event{{
			RunID: r.RunID, EventType: EventFailed, OccurredAt: now,
			Payload: map[string]any{"error": r.ErrorText, "recovery": true},
		}}); err == nil {
			// #6：Mission 停在 running——不回 ready（禁止自动调度）
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

func truncateS(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
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
