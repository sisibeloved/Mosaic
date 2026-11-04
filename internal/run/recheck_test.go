package run

// 0.6.13 复审修复回归（run 面）：#9 结果分支的期限判定（监控未及发停时完成
// 不得发布）、#10 预算单快照自清单契约层（RefOf 后修订收紧即时生效）。

import (
	"context"
	"errors"
	"testing"
	"time"
)

// releaseExec 阻塞至测试放行（先于任何停止信号返回正文——复现"响应已到、
// 监控未发停"的交错）。
type releaseExec struct {
	release chan struct{}
	started chan struct{}
}

func (e *releaseExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	close(e.started)
	select {
	case <-e.release:
		return "完成正文", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestDeadlinePassedCompletionRace 复审 #9：执行器在期限已过后返回正文（独立
// 监控尚未发停——注册表无信号）——完成提交前的终态判定按**行上期限**兜底：
// 终态 canceled + wall_clock_exceeded 事件，正文只审计不发布。
func TestDeadlinePassedCompletionRace(t *testing.T) {
	missions := &fakeMissions{refs: map[string]MissionRef{}, status: map[string]string{}, waitingReason: map[string]string{}}
	missions.refs["mi_dr"] = MissionRef{MissionID: "mi_dr", OwnerAgentID: "ag_x", WallClockSeconds: 60}
	exec := &releaseExec{release: make(chan struct{}), started: make(chan struct{})}
	clock := &mutableClock{now: "2026-11-03T12:00:00.000000000Z"}
	var n int
	svc := New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_x": {BindingID: "bd_x", AgentID: "ag_x"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		&fakeLeases{fenceOK: true},
		fakeManifests{state: "assembled", layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{"objective": "期限竞态", "acceptance_criteria": []string{"a"}}},
		}},
		exec,
		WithNow(clock.Now),
		WithNewRunID(func() string { n++; return "run_dr" + itoa(n) }),
		WithSchedulerDisabled())
	ctx := context.Background()

	done := make(chan Run, 1)
	go func() {
		r, _ := svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_dr", OwnerAgentID: "ag_x"})
		done <- r
	}()
	<-exec.started
	// 期限已过（监控未驱动——无任何停止信号）→ 放行执行器返回正文
	clock.Set("2026-11-03T12:01:30.000000000Z")
	close(exec.release)
	select {
	case r := <-done:
		if r.State != StateCanceled {
			t.Fatalf("期限已过的完成须按停止终理化: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("执行未返回")
	}
	runID := latestRunID(t, svc, "mi_dr")
	if r, ok, _ := svc.store.RunByID(ctx, runID); !ok || r.ResultBlock != "" {
		t.Fatalf("过期正文不得发布: %+v", r)
	}
	evs, _ := svc.store.RunEvents(ctx, runID)
	causeSeen := false
	for _, e := range evs {
		if e.EventType == EventCanceled && evStr(e.Payload, "cause") == CauseWallClockExceeded {
			causeSeen = true
		}
	}
	if !causeSeen {
		t.Fatalf("停止事件须携 wall_clock_exceeded: %+v", evs)
	}
	if st := missions.status["mi_dr"]; st != "ready" {
		t.Fatalf("停止后 Mission 回 ready: %v", st)
	}
}

func evStr(p map[string]any, key string) string {
	if v, ok := p[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// TestBudgetFromManifestSnapshot 复审 #10：预算单快照取**清单契约层**（与执行
// 输入同一版本）——RefOf/调用方携带的陈旧放宽值不生效：墙钟按契约层 60s 落
// 快照、次数按契约层上限拒绝。
func TestBudgetFromManifestSnapshot(t *testing.T) {
	missions := &fakeMissions{refs: map[string]MissionRef{}, status: map[string]string{}, waitingReason: map[string]string{}}
	// 陈旧放宽快照（RefOf 后契约已收紧——清单层为准）
	missions.refs["mi_ms"] = MissionRef{MissionID: "mi_ms", OwnerAgentID: "ag_x", MaxRuns: 99, WallClockSeconds: 0}
	var n int
	svc := New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_x": {BindingID: "bd_x", AgentID: "ag_x"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		&fakeLeases{fenceOK: true},
		fakeManifests{state: "assembled", layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{
				"objective": "预算快照", "acceptance_criteria": []string{"a"},
				"budget": map[string]any{"max_runs": 1, "max_wall_clock_seconds": 60},
			}},
		}},
		&fakeExecutor{block: "ok"},
		WithNow(func() string { return "2026-11-03T12:00:00.000000000Z" }),
		WithNewRunID(func() string { n++; return "run_ms" + itoa(n) }),
		WithSchedulerDisabled())
	ctx := context.Background()

	r1, err := svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_ms", OwnerAgentID: "ag_x", MaxRuns: 99})
	if err != nil {
		t.Fatalf("首次执行: %v", err)
	}
	if r1.Deadline == "" {
		t.Fatalf("墙钟按契约层快照落 deadline（陈旧 0 不得生效）: %+v", r1)
	}
	if _, err := svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_ms", OwnerAgentID: "ag_x", MaxRuns: 99}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("上限按契约层拒绝（陈旧 99 不得生效）: %v", err)
	}
}

// TestStopDuringFenceCheck 三审 #5：结果先于期限抵达，但围栏核对期间监控发停
// （此刻期限已过）——终态写入前的重新仲裁改按停止终理化，正文不发布。
func TestStopDuringFenceCheck(t *testing.T) {
	missions := &fakeMissions{refs: map[string]MissionRef{}, status: map[string]string{}, waitingReason: map[string]string{}}
	missions.refs["mi_fc"] = MissionRef{MissionID: "mi_fc", OwnerAgentID: "ag_x", WallClockSeconds: 60}
	clock := &mutableClock{now: "2026-11-03T12:00:00.000000000Z"}
	// 围栏回调内推进时钟越过期限（复现"围栏检查期间监控发停"交错）
	leases := &fakeLeases{fenceOK: true, onCheck: func() { clock.Set("2026-11-03T12:01:30.000000000Z") }}
	var n int
	svc := New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_x": {BindingID: "bd_x", AgentID: "ag_x"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		leases,
		fakeManifests{state: "assembled", layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{"objective": "围栏期停止", "acceptance_criteria": []string{"a"}}},
		}},
		&fakeExecutor{block: "完成正文"},
		WithNow(clock.Now),
		WithNewRunID(func() string { n++; return "run_fc" + itoa(n) }),
		WithSchedulerDisabled())
	ctx := context.Background()

	r, err := svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_fc", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("执行: %v", err)
	}
	if r.State != StateCanceled {
		t.Fatalf("围栏期发停须按停止终理化: %+v", r)
	}
	if r.ResultBlock != "" {
		t.Fatalf("正文不得发布: %+v", r)
	}
	evs, _ := svc.store.RunEvents(ctx, r.RunID)
	causeSeen := false
	for _, e := range evs {
		if e.EventType == EventCanceled && evStr(e.Payload, "cause") == CauseWallClockExceeded {
			causeSeen = true
		}
	}
	if !causeSeen {
		t.Fatalf("停止事件须携 wall_clock_exceeded: %+v", evs)
	}
	if st := missions.status["mi_fc"]; st != "ready" {
		t.Fatalf("停止后 Mission 回 ready: %v", st)
	}
}

// TestTerminalWriteArbitration 四审 #6：终态写入的**写锁后**墙钟裁决——
// completed/failed 在落库时刻期限已过即改按 canceled（迟到结果只审计）。
func TestTerminalWriteArbitration(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	clock := &mutableClock{now: "2026-11-03T12:00:00.000000000Z"}
	mk := func(id string) Run {
		return Run{RunID: id, MissionID: "mi_ta", Attempt: 1, LeaseID: "lse_t",
			State: StateRunning, RequestedAt: "t", UpdatedAt: "t",
			Deadline: "2026-11-03T12:01:00.000000000Z"}
	}
	// 播种 running 行（终写前态）——同一 mission 不能同时在途，逐个终写
	if _, err := store.CreateRun(ctx, mk("run_ta1"), 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 推进时钟越过期限（模拟终写取得写锁后的现行时刻）
	clock.Set("2026-11-03T12:01:30.000000000Z")
	// completed：落库时（nowFn——模拟锁后取值）已过期限 → canceled + 审计
	r1 := mk("run_ta1")
	r1.State, r1.ResultBlock = StateCompleted, "迟到正文"
	out1, err := store.UpdateRunTerminal(ctx, r1, nil, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if out1.State != StateCanceled || out1.ResultBlock != "" {
		t.Fatalf("期限后完成须改判 canceled: %+v", out1)
	}
	evs, _ := store.RunEvents(ctx, "run_ta1")
	lateSeen, causeSeen := false, false
	for _, e := range evs {
		if e.EventType == EventLateResult && evStr(e.Payload, "phase") == "terminal_write" {
			lateSeen = true
		}
		if e.EventType == EventCanceled && evStr(e.Payload, "cause") == CauseWallClockExceeded {
			causeSeen = true
		}
	}
	if !lateSeen || !causeSeen {
		t.Fatalf("停止/审计事件: %+v", evs)
	}
	// failed 同并入（四审：错误结果在停止后按停止归档）
	if _, err := store.CreateRun(ctx, mk("run_ta2"), 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	r2 := mk("run_ta2")
	r2.State, r2.ErrorText = StateFailed, "执行错误"
	out2, err := store.UpdateRunTerminal(ctx, r2, nil, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if out2.State != StateCanceled {
		t.Fatalf("期限后失败须并入停止: %+v", out2)
	}
}
