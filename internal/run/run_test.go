// UT 层：单 Agent Mission 执行（0.6.5 / ADR-0028）。闭环（租约+清单+Run+
// 执行+围栏+释放+投影回 ready）、attempt 递增、围栏拒绝审计、失败路径、
// 恢复不重跑、资源忙跳过、每 Mission 至多一未终态。
package run

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---- 测试件 ----

type fakeMissions struct {
	mu      sync.Mutex
	ready   []MissionRef
	status  map[string]string // mission_id → 投影状态
	setRuns []string          // SetRunning/SetReadyFromRunning 调用序
}

func (f *fakeMissions) ListReady(_ context.Context) ([]MissionRef, error) {
	return f.ready, nil
}

func (f *fakeMissions) SetRunning(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = "running"
	f.setRuns = append(f.setRuns, id+":running")
	return nil
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

type fakeRuntimes struct {
	ready   RuntimeRef
	hasRead bool
}

func (f *fakeRuntimes) FirstReady(context.Context) (RuntimeRef, bool, error) {
	f.hasRead = true
	return f.ready, f.ready.RuntimeID != "", nil
}

type fakeLeases struct {
	mu       sync.Mutex
	acquired []string // runtime_id 序列
	released []string
	fenceOK  bool
	fenceOf  map[string]string // lease_id → 有效与否（"true"/reason）
	conflict bool
}

func (f *fakeLeases) Acquire(_ context.Context, runtimeID, missionID string, _ int, _ map[string]int) (Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflict {
		return Lease{}, errors.New("runtime: 运行时已被租约占用")
	}
	f.acquired = append(f.acquired, runtimeID)
	return Lease{LeaseID: "lse_t", FenceToken: "fnc_t", Generation: 1, EnvironmentID: "env_t"}, nil
}

func (f *fakeLeases) Release(_ context.Context, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, leaseID)
	return nil
}

func (f *fakeLeases) CheckFence(_ context.Context, leaseID, token string) (bool, string, error) {
	if f.fenceOK {
		return true, "", nil
	}
	return false, "lease_not_active", nil
}

type fakeManifests struct {
	state  string // assembled | blocked
	reason string
}

func (f fakeManifests) Build(context.Context, string, string) (Manifest, error) {
	return Manifest{ManifestID: "cmf_t", State: f.state, ReasonCode: f.reason}, nil
}

type fakeExecutor struct {
	mu    sync.Mutex
	calls int
	block string
	err   error
	in    []ExecutionInput
}

func (f *fakeExecutor) Submit(_ context.Context, input ExecutionInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.in = append(f.in, input)
	return f.block, f.err
}

func newFixture(t *testing.T) (fixture, *fakeLeases, *fakeExecutor, *fakeMissions) {
	t.Helper()
	missions := &fakeMissions{status: map[string]string{}}
	leases := &fakeLeases{fenceOK: true}
	exec := &fakeExecutor{block: "执行结果正文"}
	var n int64
	svc := New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_x": {BindingID: "bd_x", AgentID: "ag_x"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		leases,
		fakeManifests{state: "assembled"},
		exec,
		WithNow(func() string { return "2026-10-31T12:00:00.000000000Z" }),
		WithNewRunID(func() string { n++; return "run_t" + strconv.Itoa(int(n)) }),
		WithSchedulerDisabled(),
	)
	return fixture{svc: svc, missions: missions}, leases, exec, missions
}

type fixture struct {
	svc      *Service
	missions *fakeMissions
}

func TestExecuteMissionHappyPath(t *testing.T) {
	f, leases, exec, missions := newFixture(t)
	ctx := context.Background()

	r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("执行: %v", err)
	}
	if r.State != StateCompleted || r.ResultBlock != "执行结果正文" || r.Attempt != 1 {
		t.Fatalf("Run 事实: %+v", r)
	}
	if r.LeaseID == "" || r.ManifestID != "cmf_t" || r.FenceToken == "" || r.Generation != 1 {
		t.Fatalf("绑定事实: %+v", r)
	}
	if exec.calls != 1 || exec.in[0].RunID != r.RunID || exec.in[0].MissionID != "mi_a" {
		t.Fatalf("执行输入: %+v", exec.in)
	}
	// 租约已释放、Mission 回 ready（可再执行 attempt+1）
	if len(leases.released) != 1 || leases.released[0] != "lse_t" {
		t.Fatalf("租约释放: %v", leases.released)
	}
	if missions.status["mi_a"] != "ready" {
		t.Fatalf("Mission 投影: %v", missions.status)
	}
	r2, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil || r2.Attempt != 2 {
		t.Fatalf("再执行 attempt 2: %+v %v", r2, err)
	}
	// 事件流：attempt_requested + started + completed（两轮共 6 条）
	evs, _, _ := f.svc.RunEventsByRun(ctx, r.RunID) // 首轮 Run 的事件流
	var types []string
	for _, e := range evs {
		types = append(types, e.EventType)
	}
	if len(types) != 3 || types[0] != EventAttemptRequested || types[1] != EventStarted || types[2] != EventCompleted {
		t.Fatalf("事件流: %v", types)
	}
}

func TestExecuteMissionFailurePath(t *testing.T) {
	f, leases, exec, missions := newFixture(t)
	exec.err = errors.New("适配器崩溃：exit 1")
	ctx := context.Background()

	r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("失败也是终态结果（同步面）: %v", err)
	}
	if r.State != StateFailed || !strings.Contains(r.ErrorText, "适配器崩溃") || r.ResultBlock != "" {
		t.Fatalf("失败事实: %+v", r)
	}
	if len(leases.released) != 1 {
		t.Fatalf("失败须释放租约: %v", leases.released)
	}
	if missions.status["mi_a"] != "ready" {
		t.Fatalf("失败后 Mission 回 ready: %v", missions.status)
	}
	// 下轮 attempt+1（失败不自动重试——调度面再触发）
	r2, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil || r2.Attempt != 2 {
		t.Fatalf("再执行: %+v %v", r2, err)
	}
}

func TestExecuteMissionFenceRejected(t *testing.T) {
	f, leases, _, _ := newFixture(t)
	ctx := context.Background()
	// 首轮成功后，制造围栏失效（执行期间租约被释放/取代）
	leases.fenceOK = false
	exec := &fakeExecutor{block: "迟到结果"}
	f.svc.executor = exec

	r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("围栏拒绝也是终态结果: %v", err)
	}
	if r.State != StateFailed || !strings.Contains(r.ErrorText, "围栏拒绝") {
		t.Fatalf("围栏拒绝事实: %+v", r)
	}
	if r.ResultBlock != "" {
		t.Fatalf("迟到结果不得落: %q", r.ResultBlock)
	}
	evs, _, _ := f.svc.RunEventsByRun(ctx, r.RunID)
	found := false
	for _, e := range evs {
		if e.EventType == EventFenceRejected {
			found = true
		}
	}
	if !found {
		t.Fatalf("须有 fence_rejected 审计事件: %+v", evs)
	}
}

func TestExecuteMissionResourceBusy(t *testing.T) {
	f, leases, _, _ := newFixture(t)
	leases.conflict = true
	_, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err == nil {
		t.Fatal("租约冲突应返回错误（本轮跳过）")
	}
	// 无 Run 行产生
	runs, _ := f.svc.ListRuns(context.Background(), "mi_a")
	if len(runs) != 0 {
		t.Fatalf("资源忙不得产生 Run: %+v", runs)
	}
}

func TestExecuteMissionNoExecutor(t *testing.T) {
	f, _, _, _ := newFixture(t)
	ctx := context.Background()
	// owner 无 active Binding
	if _, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_none"}); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("无任职: %v", err)
	}
	// 无 ready Runtime
	f.svc.runtimes = &fakeRuntimes{}
	if _, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"}); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("无运行时: %v", err)
	}
}

func TestExecuteMissionManifestBlocked(t *testing.T) {
	f, leases, _, _ := newFixture(t)
	f.svc.manifests = fakeManifests{state: "blocked", reason: "readiness_not_met"}
	_, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if !errors.Is(err, ErrManifestBlocked) || !strings.Contains(err.Error(), "readiness_not_met") {
		t.Fatalf("清单 blocked: %v", err)
	}
	if len(leases.released) != 1 {
		t.Fatalf("blocked 须释放租约: %v", leases.released)
	}
	runs, _ := f.svc.ListRuns(context.Background(), "mi_a")
	if len(runs) != 0 {
		t.Fatalf("blocked 不得产生 Run: %+v", runs)
	}
}

func TestRecoverUnfinished(t *testing.T) {
	f, _, _, missions := newFixture(t)
	ctx := context.Background()
	// 播种一个 running 态 Run（模拟崩溃残留）
	seeded := Run{RunID: "run_seed", MissionID: "mi_a", Attempt: 1,
		LeaseID: "lse_seed", State: StateRunning, RequestedAt: "t", UpdatedAt: "t"}
	if _, err := f.svc.store.CreateRun(ctx, seeded); err != nil {
		t.Fatalf("播种: %v", err)
	}
	missions.status["mi_a"] = "running"

	n, err := f.svc.RecoverUnfinished(ctx)
	if err != nil || n != 1 {
		t.Fatalf("恢复: %d %v", n, err)
	}
	got, _, _ := f.svc.GetRun(ctx, "run_seed")
	if got.State != StateFailed || !strings.Contains(got.ErrorText, "结果未知") {
		t.Fatalf("恢复事实: %+v", got)
	}
	if missions.status["mi_a"] != "ready" {
		t.Fatalf("恢复后 Mission 回 ready: %v", missions.status)
	}
	// 恢复不重跑：无新 Run
	runs, _ := f.svc.ListRuns(ctx, "mi_a")
	if len(runs) != 1 {
		t.Fatalf("恢复不产生新 Run: %+v", runs)
	}
}

func TestScanOnceSchedulesReady(t *testing.T) {
	f, _, exec, missions := newFixture(t)
	missions.ready = []MissionRef{{MissionID: "mi_a", OwnerAgentID: "ag_x"}, {MissionID: "mi_b", OwnerAgentID: "ag_x"}}
	f.svc.ScanOnce(context.Background())
	if exec.calls != 2 {
		t.Fatalf("扫描应调度两个: %d", exec.calls)
	}
	st, _ := f.svc.Status(context.Background())
	if st["launched_runs"].(int64) != 2 || st["active_runs"].(int) != 0 {
		t.Fatalf("诊断面: %+v", st)
	}
}
