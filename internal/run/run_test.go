// UT 层：单 Agent Mission 执行（0.6.5 / ADR-0028 v0.2）。闭环、attempt
// 递增、围栏（提交前复核——#3 + 迟到拒绝——#4）、失败路径、恢复不
// 自动调度（#6）、canceled 拒启动（#7）、执行输入含存档内容（#1）。
package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeMissions struct {
	mu                sync.Mutex
	ready             []MissionRef
	status            map[string]string
	waitingReason     map[string]string     // 0.6.7：等待原因（waiting 态一等事实）
	refs              map[string]MissionRef // 0.6.13 审校 #8：权威快照（未注册回落 ag_x 缺省）
	setRuns           []string
	finishCancelCalls int
}

func (f *fakeMissions) ListReady(_ context.Context) ([]MissionRef, error) {
	return f.ready, nil
}

// RefOf 权威快照（0.6.13 审校 #8）：注册值优先；未注册回落缺省 owner（直呼
// ExecuteMission 的既有测试面——预算/意图为 0 保持原语义）。
func (f *fakeMissions) RefOf(_ context.Context, id string) (MissionRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.refs[id]; ok {
		return r, true, nil
	}
	for _, r := range f.ready {
		if r.MissionID == id {
			return r, true, nil
		}
	}
	return MissionRef{MissionID: id, OwnerAgentID: "ag_x"}, true, nil
}

func (f *fakeMissions) SetRunning(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.status[id]; ok && cur != "ready" {
		return false, nil
	}
	f.status[id] = "running"
	f.setRuns = append(f.setRuns, id+":running")
	return true, nil
}

// SetReadyFromRunning 条件投影（对齐 mission 域：仅 running 可回 ready——
// paused/canceling 上幂等无操作）。
func (f *fakeMissions) SetReadyFromRunning(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.status[id]; !ok || cur != "running" {
		return nil // 幂等无操作
	}
	f.status[id] = "ready"
	f.setRuns = append(f.setRuns, id+":ready:"+reason)
	return nil
}

func (f *fakeMissions) StatusOf(_ context.Context, id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.status[id]
	return st, ok, nil
}

// ListCanceling 枚举 canceling（0.6.6 审校 P2：恢复面收尾兜底——Run 已终态
// 的滞留 canceling 不经未终态扫描可见）。
func (f *fakeMissions) ListCanceling(_ context.Context) ([]MissionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []MissionRef{}
	for id, st := range f.status {
		if st == "canceling" {
			out = append(out, MissionRef{MissionID: id})
		}
	}
	return out, nil
}

// FinishCanceling 收尾（对齐存储/服务语义：仅 canceling 可收尾，幂等不计数）。
func (f *fakeMissions) FinishCanceling(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status[id] != "canceling" {
		return nil
	}
	f.finishCancelCalls++
	f.status[id] = "canceled"
	return nil
}

// SetWaiting running → waiting(reason)（0.6.7 / ADR-0030：恢复分派结果不可
// 判定——对齐 mission 域条件投影：仅 running 可进 waiting）。
func (f *fakeMissions) SetWaiting(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 对齐 mission 域（0.6.9 审校 #2）：running | ready 皆可进 waiting
	if f.status[id] != "running" && f.status[id] != "ready" {
		return nil
	}
	f.status[id] = "waiting"
	if f.waitingReason == nil {
		f.waitingReason = map[string]string{}
	}
	f.waitingReason[id] = reason
	return nil
}

// ClearWaiting waiting → ready（处置后回调度列；fake 恒视为完备）+ 清等待原因。
func (f *fakeMissions) ClearWaiting(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status[id] != "waiting" {
		return nil
	}
	f.status[id] = "ready"
	if f.waitingReason != nil {
		f.waitingReason[id] = ""
	}
	return nil
}

type fakeBindings struct {
	active map[string]BindingFacts
	// byInstance 按实例固定任职（0.6.13 审校 #1：slot 意图装配面）。
	byInstance map[string]BindingFacts
}

func (f fakeBindings) ActiveOf(_ context.Context, agentID string) (BindingFacts, bool, error) {
	b, ok := f.active[agentID]
	return b, ok, nil
}

// ActiveOfInstance 按职责实例解析 active 任职（0.6.13 审校 #1）。
func (f fakeBindings) ActiveOfInstance(_ context.Context, instanceID string) (BindingFacts, bool, error) {
	bf, ok := f.byInstance[instanceID]
	return bf, ok, nil
}

type fakeRuntimes struct{ ready RuntimeRef }

func (f *fakeRuntimes) FirstReady(context.Context) (RuntimeRef, bool, error) {
	return f.ready, f.ready.RuntimeID != "", nil
}

type fakeLeases struct {
	mu         sync.Mutex
	acquired   int
	released   int
	fenceOK    bool
	conflict   bool
	releaseLog *eventLog // 可选：释放序审计（0.6.6 审校 P1 停止序断言）
	onCheck    func()    // 可选：围栏回调钩子（三审 #5——围栏期推进时钟/发停交错）
}

func (f *fakeLeases) Acquire(_ context.Context, _, _ string, _ int, _ map[string]int) (Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflict {
		return Lease{}, errors.New("runtime: 运行时已被租约占用")
	}
	f.acquired++
	return Lease{LeaseID: "lse_t", FenceToken: "fnc_t", Generation: f.acquired, EnvironmentID: "env_t"}, nil
}

// Release / CheckFence 对过期上下文报错（对齐真实存储：过期 ctx 的 DB 操作
// 失败——0.6.6 审校 P1 收尾窗口测试的失真防线）。
func (f *fakeLeases) Release(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.released++
	f.mu.Unlock()
	if f.releaseLog != nil {
		f.releaseLog.add("release")
	}
	return nil
}
func (f *fakeLeases) CheckFence(ctx context.Context, _, _ string) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	if f.onCheck != nil {
		f.onCheck() // 三审 #5：围栏期交错注入
	}
	if f.fenceOK {
		return true, "", nil
	}
	return false, "lease_not_active", nil
}

type fakeManifests struct {
	state  string
	reason string
	layers []ManifestLayer
}

func (f fakeManifests) Build(context.Context, string, string) (Manifest, error) {
	return Manifest{ManifestID: "cmf_t", State: f.state, ReasonCode: f.reason, Layers: f.layers}, nil
}

type fakeExecutor struct {
	block string
	err   error
	calls int
}

func (f *fakeExecutor) Submit(_ context.Context, _ ExecutionInput) (string, error) {
	f.calls++
	return f.block, f.err
}

func itoa(n int) string { return string(rune('0' + n)) }

func newFixture(t *testing.T) fixture {
	t.Helper()
	missions := &fakeMissions{refs: map[string]MissionRef{}, status: map[string]string{}, waitingReason: map[string]string{}}
	leases := &fakeLeases{fenceOK: true}
	exec := &fakeExecutor{block: "执行结果正文"}
	var n int
	svc := New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_x": {BindingID: "bd_x", AgentID: "ag_x"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		leases,
		fakeManifests{state: "assembled", layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{"objective": "测试目标", "acceptance_criteria": []string{"a"}}},
			{Name: "recent_messages", Content: map[string]any{"messages": []any{}}},
		}},
		exec,
		WithNow(func() string { return "2026-10-31T12:00:00.000000000Z" }),
		WithNewRunID(func() string { n++; return "run_t" + itoa(n) }),
		WithSchedulerDisabled(),
	)
	return fixture{svc: svc, missions: missions, leases: leases, exec: exec}
}

type fixture struct {
	svc      *Service
	missions *fakeMissions
	leases   *fakeLeases
	exec     *fakeExecutor
}

func TestExecuteMissionHappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("执行: %v", err)
	}
	if r.State != StateCompleted || r.ResultBlock != "执行结果正文" || r.Attempt != 1 {
		t.Fatalf("Run 事实: %+v", r)
	}
	if f.exec.calls != 1 || f.leases.released < 1 {
		t.Fatalf("执行=%d 释放=%d", f.exec.calls, f.leases.released)
	}
	if f.missions.status["mi_a"] != "ready" {
		t.Fatalf("Mission 投影: %v", f.missions.status)
	}
	r2, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil || r2.Attempt != 2 {
		t.Fatalf("再执行: %+v %v", r2, err)
	}
	evs, _, _ := f.svc.RunEventsByRun(ctx, r.RunID)
	if len(evs) != 3 {
		t.Fatalf("事件流: %d", len(evs))
	}
}

func TestExecuteMissionFailurePath(t *testing.T) {
	f := newFixture(t)
	f.exec.err = errors.New("适配器崩溃：exit 1")
	r, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("失败也是终态结果: %v", err)
	}
	if r.State != StateFailed || !strings.Contains(r.ErrorText, "适配器崩溃") || f.leases.released < 1 {
		t.Fatalf("失败事实: %+v 释放=%d", r, f.leases.released)
	}
	if f.missions.status["mi_a"] != "ready" {
		t.Fatalf("失败后回 ready: %v", f.missions.status)
	}
}

func TestPreSubmitFenceRejected(t *testing.T) {
	f := newFixture(t)
	f.leases.fenceOK = false
	r, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("提交前围栏拒绝: %v", err)
	}
	if r.State != StateFailed || !strings.Contains(r.ErrorText, "围栏") || f.exec.calls != 0 {
		t.Fatalf("围栏拒绝: %+v exec=%d", r, f.exec.calls)
	}
	evs, _, _ := f.svc.RunEventsByRun(context.Background(), r.RunID)
	found := false
	for _, e := range evs {
		if e.EventType == EventFenceRejected {
			found = true
		}
	}
	if !found {
		t.Fatalf("须有 fence_rejected: %+v", evs)
	}
}

func TestCanceledRejectsStart(t *testing.T) {
	f := newFixture(t)
	f.missions.status["mi_a"] = "canceled"
	r, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("canceled 后: %v", err)
	}
	if r.State != StateFailed || !strings.Contains(r.ErrorText, "取消") || f.exec.calls != 0 {
		t.Fatalf("canceled 拒启动: %+v exec=%d", r, f.exec.calls)
	}
}

func TestResourceBusy(t *testing.T) {
	f := newFixture(t)
	f.leases.conflict = true
	if _, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"}); err == nil {
		t.Fatal("冲突应错误")
	}
	runs, _ := f.svc.ListRuns(context.Background(), "mi_a")
	if len(runs) != 0 {
		t.Fatalf("资源忙不产生 Run: %+v", runs)
	}
}

func TestNoExecutor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.missions.refs["mi_a"] = MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_none"}
	if _, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_none"}); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("无任职: %v", err)
	}
	f.svc.runtimes = &fakeRuntimes{}
	if _, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"}); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("无运行时: %v", err)
	}
}

func TestManifestBlocked(t *testing.T) {
	f := newFixture(t)
	f.svc.manifests = fakeManifests{state: "blocked", reason: "readiness_not_met"}
	_, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if !errors.Is(err, ErrManifestBlocked) || f.leases.released < 1 {
		t.Fatalf("blocked: %v 释放=%d", err, f.leases.released)
	}
}

func TestRecoverNoAutoReschedule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seeded := Run{RunID: "run_seed", MissionID: "mi_a", Attempt: 1,
		LeaseID: "lse_seed", State: StateRunning, RequestedAt: "t", UpdatedAt: "t"}
	if _, err := f.svc.store.CreateRun(ctx, seeded, 0, 0, ""); err != nil {
		t.Fatalf("播种: %v", err)
	}
	f.missions.status["mi_a"] = "running"

	n, err := f.svc.RecoverUnfinished(ctx)
	if err != nil || n != 1 {
		t.Fatalf("恢复: %d %v", n, err)
	}
	got, _, _ := f.svc.GetRun(ctx, "run_seed")
	// 0.6.7 / ADR-0030：结果不可判定 → unknown（非 failed）
	if got.State != StateUnknown || !strings.Contains(got.ErrorText, "结果未知") {
		t.Fatalf("恢复事实: %+v", got)
	}
	// #6 → 0.6.7：Mission 进 waiting(reconciliation)（出调度列——扫描不自动调度）
	if f.missions.status["mi_a"] != "waiting" {
		t.Fatalf("恢复后 Mission 须 waiting: %v", f.missions.status)
	}
}

func TestScanOnceSchedulesReady(t *testing.T) {
	f := newFixture(t)
	f.missions.ready = []MissionRef{{MissionID: "mi_a", OwnerAgentID: "ag_x"}, {MissionID: "mi_b", OwnerAgentID: "ag_x"}}
	f.svc.ScanOnce(context.Background())
	if f.exec.calls != 2 {
		t.Fatalf("扫描两个: %d", f.exec.calls)
	}
}

// #1：执行输入含存档内容（非仅 manifest_id）。
func TestExecutionInputHasContent(t *testing.T) {
	f := newFixture(t)
	var captured ExecutionInput
	f.svc.executor = &captureExec{fn: func(in ExecutionInput) (string, error) {
		captured = in
		return "结果", nil
	}}
	_, _ = f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if captured.Inline == nil || captured.Inline["objective"] != "测试目标" {
		t.Fatalf("目标须传入: %v", captured.Inline)
	}
	if captured.Inline["acceptance_criteria"] == nil {
		t.Fatal("验收标准须传入")
	}
}

type captureExec struct {
	fn func(ExecutionInput) (string, error)
}

func (c *captureExec) Submit(_ context.Context, in ExecutionInput) (string, error) { return c.fn(in) }
