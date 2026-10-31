package run

// 0.6.6 控制面（ADR-0029）：执行脱离请求上下文、停止信号终理化
// （Run → canceled + stop_requested/canceled 事件 + 租约释放 + Mission
// 投影不动）、迟到执行器结果不落、恢复按停止意图分派。测试先行。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// pauseExec 模拟长执行：阻塞至 ctx 取消后返回（真实停止面）。
type pauseExec struct {
	mu       sync.Mutex
	started  chan struct{}
	returned chan struct{}
	late     bool // 取消后仍"跑完"返回结果（迟到结果面）
}

func newPauseExec() *pauseExec {
	return &pauseExec{started: make(chan struct{}), returned: make(chan struct{})}
}

func (p *pauseExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	close(p.started)
	<-ctx.Done()
	p.mu.Lock()
	late := p.late
	p.mu.Unlock()
	if late {
		// 模拟真实 CLI 不理会取消跑完全程——结果迟到
		go func() { time.Sleep(30 * time.Millisecond); close(p.returned) }()
		return "迟到结果正文", nil
	}
	close(p.returned)
	return "", ctx.Err()
}

// waitForRunState 轮询至 Mission 出现目标状态 Run（或超时失败）。
func waitForRunState(t *testing.T, s *Service, missionID, state string) Run {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := s.ListRuns(context.Background(), missionID)
		for _, r := range runs {
			if r.State == state {
				return r
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 Run %s 超时", state)
	return Run{}
}

// TestExecutionDetachedFromRequestCtx 执行脱离请求上下文（ADR-0029 决策 2
// 行为变更）：请求 ctx 中途取消，执行照完、终态照落。
func TestExecutionDetachedFromRequestCtx(t *testing.T) {
	f := newFixture(t)
	f.svc.executor = &slowOkExec{delay: 60 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Run, 1)
	go func() {
		r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		if err != nil {
			t.Errorf("执行: %v", err)
		}
		done <- r
	}()
	time.Sleep(15 * time.Millisecond)
	cancel() // 客户端断开——执行不得中止
	select {
	case r := <-done:
		if r.State != StateCompleted || r.ResultBlock == "" {
			t.Fatalf("断开后执行须照完: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("执行未返回")
	}
	if f.missions.status["mi_a"] != "ready" {
		t.Fatalf("终态后投影: %v", f.missions.status)
	}
}

type slowOkExec struct{ delay time.Duration }

func (s *slowOkExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	select {
	case <-time.After(s.delay):
		return "完成正文", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestPauseStopsInFlightRun 暂停在途：持久 paused 先行 → 停止信号 →
// Run canceled（stop_requested{pause} + canceled 事件）+ 租约释放 +
// Mission 停 paused（不回 ready）。
func TestPauseStopsInFlightRun(t *testing.T) {
	f := newFixture(t)
	f.svc.executor = newPauseExec()
	ctx := context.Background()
	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		done <- r
	}()
	waitForRunState(t, f.svc, "mi_a", StateRunning)

	// 控制编排：mission CAS → paused（fake 对齐真实条件投影语义）
	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "paused"
	f.missions.mu.Unlock()

	n, err := f.svc.SignalStopByMission(ctx, "mi_a", "pause")
	if err != nil || n != 1 {
		t.Fatalf("停止信号: n=%d err=%v", n, err)
	}
	var r Run
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("停止后未返回")
	}
	if r.State != StateCanceled {
		t.Fatalf("Run 须 canceled: %+v", r)
	}
	got := waitForRunState(t, f.svc, "mi_a", StateCanceled)
	if got.ResultBlock != "" {
		t.Fatalf("迟到结果不得落: %+v", got)
	}
	evs, _, _ := f.svc.RunEventsByRun(ctx, got.RunID)
	var hasStop, hasCanceled bool
	for _, e := range evs {
		if e.EventType == EventStopRequested && e.Payload["cause"] == "pause" {
			hasStop = true
		}
		if e.EventType == EventCanceled {
			hasCanceled = true
		}
	}
	if !hasStop || !hasCanceled {
		t.Fatalf("事件面: stop=%v canceled=%v %+v", hasStop, hasCanceled, evs)
	}
	if f.leases.released < 1 {
		t.Fatal("租约须释放")
	}
	if f.missions.status["mi_a"] != "paused" {
		t.Fatalf("Mission 须停 paused（投影不动）: %v", f.missions.status)
	}
}

// TestCancelStopsInFlightRun 取消在途：canceling 先行 → 停止信号 → Run
// canceled；随后控制面 FinishCancel（fake 记录调用序）。
func TestCancelStopsInFlightRun(t *testing.T) {
	f := newFixture(t)
	f.svc.executor = newPauseExec()
	ctx := context.Background()
	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		done <- r
	}()
	waitForRunState(t, f.svc, "mi_a", StateRunning)

	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "canceling"
	f.missions.mu.Unlock()

	if n, _ := f.svc.SignalStopByMission(ctx, "mi_a", "cancel"); n != 1 {
		t.Fatalf("停止信号: %d", n)
	}
	select {
	case r := <-done:
		if r.State != StateCanceled {
			t.Fatalf("Run 须 canceled: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("停止后未返回")
	}
	// 控制面收尾（run 域不关心——但未终态清零面须可见）
	has, err := f.svc.HasUnfinished(ctx, "mi_a")
	if err != nil || has {
		t.Fatalf("停止后未终态须清零: has=%v err=%v", has, err)
	}
	if f.missions.finishCancelCalls != 0 {
		t.Fatalf("在途停止不代行收尾（控制面职责）: %d", f.missions.finishCancelCalls)
	}
}

// TestStopThenLateResultDropped 停止后迟到结果不落（缓冲通道丢弃——围栏
// 已失效，无第二写入者）。
func TestStopThenLateResultDropped(t *testing.T) {
	f := newFixture(t)
	exec := newPauseExec()
	exec.mu.Lock()
	exec.late = true
	exec.mu.Unlock()
	f.svc.executor = exec
	ctx := context.Background()
	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		done <- r
	}()
	waitForRunState(t, f.svc, "mi_a", StateRunning)
	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "canceling"
	f.missions.mu.Unlock()
	f.svc.SignalStopByMission(ctx, "mi_a", "cancel")

	select {
	case r := <-done:
		if r.State != StateCanceled || r.ResultBlock != "" {
			t.Fatalf("迟到结果不得落库: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("停止后未返回")
	}
	// 等迟到 goroutine 走完再核库
	select {
	case <-exec.returned:
	case <-time.After(1 * time.Second):
	}
	runs, _ := f.svc.ListRuns(ctx, "mi_a")
	for _, r := range runs {
		if r.ResultBlock != "" || r.State != StateCanceled {
			t.Fatalf("库面迟到污染: %+v", r)
		}
	}
}

// TestSignalStopNoInFlight 无在途：信号面返回 0（控制面幂等续驱用）。
func TestSignalStopNoInFlight(t *testing.T) {
	f := newFixture(t)
	n, err := f.svc.SignalStopByMission(context.Background(), "mi_none", "pause")
	if err != nil || n != 0 {
		t.Fatalf("无在途: n=%d err=%v", n, err)
	}
}

// TestRecoverDispatchByIntent 恢复分派（ADR-0029 决策 4）：canceling →
// Run canceled + 收尾兑现；paused → Run canceled + 停 paused；running →
// failed 结果未知 + 停 running（0.6.5 语义回归）。
func TestRecoverDispatchByIntent(t *testing.T) {
	for _, tc := range []struct {
		intent     string
		wantRun    string
		wantAfter  string
		wantFinish int
	}{
		{"canceling", StateCanceled, "canceled", 1},
		{"paused", StateCanceled, "paused", 0},
		{"running", StateFailed, "running", 0},
	} {
		f := newFixture(t)
		ctx := context.Background()
		seeded := Run{RunID: "run_seed", MissionID: "mi_a", Attempt: 1,
			LeaseID: "lse_seed", State: StateRunning, RequestedAt: "t", UpdatedAt: "t"}
		if _, err := f.svc.store.CreateRun(ctx, seeded); err != nil {
			t.Fatalf("播种: %v", err)
		}
		f.missions.status["mi_a"] = tc.intent

		if _, err := f.svc.RecoverUnfinished(ctx); err != nil {
			t.Fatalf("恢复: %v", err)
		}
		got, _, _ := f.svc.GetRun(ctx, "run_seed")
		if got.State != tc.wantRun {
			t.Fatalf("%s: Run 须 %s（得到 %s）", tc.intent, tc.wantRun, got.State)
		}
		if tc.wantRun == StateCanceled && !strings.Contains(got.ErrorText, "结果未知") {
			t.Fatalf("%s: 停止在途重启原因须留痕: %+v", tc.intent, got)
		}
		if f.missions.status["mi_a"] != tc.wantAfter {
			t.Fatalf("%s: Mission 须 %s（得到 %s）", tc.intent, tc.wantAfter, f.missions.status["mi_a"])
		}
		if f.missions.finishCancelCalls != tc.wantFinish {
			t.Fatalf("%s: 收尾兑现次数 %d ≠ %d", tc.intent, f.missions.finishCancelCalls, tc.wantFinish)
		}
		evs, _, _ := f.svc.RunEventsByRun(ctx, "run_seed")
		if tc.wantRun == StateCanceled {
			var hasCanceled bool
			for _, e := range evs {
				if e.EventType == EventCanceled {
					hasCanceled = true
				}
			}
			if !hasCanceled {
				t.Fatalf("%s: 须有 canceled 事件: %+v", tc.intent, evs)
			}
		}
	}
}

// TestStopRegistryCause 注册表因果面：信号携 cause 可读回（终理化前）。
func TestStopRegistryCause(t *testing.T) {
	reg := newStopRegistry()
	cause := ""
	reg.register("run_x", func() { cause = "fired" })
	if ok := reg.signal("run_x", "cancel"); !ok {
		t.Fatal("信号须命中")
	}
	if c, ok := reg.cause("run_x"); !ok || c != "cancel" {
		t.Fatalf("cause: %q %v", c, ok)
	}
	if cause != "fired" {
		t.Fatal("cancel 须被调用")
	}
	reg.unregister("run_x")
	if ok := reg.signal("run_x", "pause"); ok {
		t.Fatal("注销后信号不得命中")
	}
	if _, ok := reg.cause("run_missing"); ok {
		t.Fatal("未知 run 无 cause")
	}
}
