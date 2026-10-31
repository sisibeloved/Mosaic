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
	mu      sync.Mutex
	ready   []MissionRef
	status  map[string]string
	setRuns []string
}

func (f *fakeMissions) ListReady(_ context.Context) ([]MissionRef, error) {
	return f.ready, nil
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

func (f *fakeMissions) SetReadyFromRunning(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = "ready"
	f.setRuns = append(f.setRuns, id+":ready:"+reason)
	return nil
}

type fakeBindings struct{ active map[string]BindingFacts }

func (f fakeBindings) ActiveOf(_ context.Context, agentID string) (BindingFacts, bool, error) {
	b, ok := f.active[agentID]
	return b, ok, nil
}

type fakeRuntimes struct{ ready RuntimeRef }

func (f *fakeRuntimes) FirstReady(context.Context) (RuntimeRef, bool, error) {
	return f.ready, f.ready.RuntimeID != "", nil
}

type fakeLeases struct {
	mu       sync.Mutex
	acquired int
	released int
	fenceOK  bool
	conflict bool
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
func (f *fakeLeases) Release(_ context.Context, _ string) error { f.released++; return nil }
func (f *fakeLeases) CheckFence(_ context.Context, _, _ string) (bool, string, error) {
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
	missions := &fakeMissions{status: map[string]string{}}
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
	if _, err := f.svc.store.CreateRun(ctx, seeded); err != nil {
		t.Fatalf("播种: %v", err)
	}
	f.missions.status["mi_a"] = "running"

	n, err := f.svc.RecoverUnfinished(ctx)
	if err != nil || n != 1 {
		t.Fatalf("恢复: %d %v", n, err)
	}
	got, _, _ := f.svc.GetRun(ctx, "run_seed")
	if got.State != StateFailed || !strings.Contains(got.ErrorText, "结果未知") {
		t.Fatalf("恢复事实: %+v", got)
	}
	// #6：Mission 停在 running（不回 ready）——扫描不自动调度
	if f.missions.status["mi_a"] != "running" {
		t.Fatalf("恢复后 Mission 须停在 running: %v", f.missions.status)
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
