package run

// 0.6.13 / ADR-0036：墙钟运行时强制（§6.1 budget——Run 落 running 时按执行开始
// 契约快照 deadline，扫描过期走既有停止面）与 max_runs 派发上限。测试先行。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// mutableClock 可变时钟（墙钟测试：推进 now 越过 deadline）。
type mutableClock struct {
	mu  sync.Mutex
	now string
}

func (c *mutableClock) Now() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mutableClock) Set(s string) {
	c.mu.Lock()
	c.now = s
	c.mu.Unlock()
}

type wallFixture struct {
	svc      *Service
	missions *fakeMissions
	leases   *fakeLeases
}

func newWallFixture(t *testing.T) (wallFixture, *mutableClock, *stopRacingExec) {
	t.Helper()
	missions := &fakeMissions{status: map[string]string{}, waitingReason: map[string]string{}}
	leases := &fakeLeases{fenceOK: true}
	exec := &stopRacingExec{started: make(chan struct{})}
	clock := &mutableClock{now: "2026-11-03T12:00:00.000000000Z"}
	var n int
	svc := New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_x": {BindingID: "bd_x", AgentID: "ag_x"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		leases,
		fakeManifests{state: "assembled", layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{"objective": "墙钟测试", "acceptance_criteria": []string{"a"}}},
		}},
		exec,
		WithNow(clock.Now),
		WithNewRunID(func() string { n++; return "run_w" + itoa(n) }),
		WithSchedulerDisabled(),
	)
	return wallFixture{svc: svc, missions: missions, leases: leases}, clock, exec
}

// TestWallClockEnforced 墙钟到期：Run 落 running 时快照 deadline（started_at +
// 契约墙钟）；时钟越过 deadline → ScanOnce 触发既有停止面——run canceled（stop/
// canceled 事件携 cause=wall_clock_exceeded）+ Mission 回 ready。
func TestWallClockEnforced(t *testing.T) {
	f, clock, exec := newWallFixture(t)
	ctx := context.Background()
	ref := MissionRef{MissionID: "mi_w", OwnerAgentID: "ag_x", WallClockSeconds: 60}

	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(ctx, ref)
		done <- r
	}()
	<-exec.started
	waitForRunState(t, f.svc, "mi_w", StateRunning)

	// running 行快照了 deadline = started_at(12:00:00) + 60s
	run, ok, _ := f.svc.store.RunByID(ctx, latestRunID(t, f.svc, "mi_w"))
	dl, dlErr := time.Parse(time.RFC3339Nano, run.Deadline)
	if !ok || dlErr != nil || !dl.Equal(time.Date(2026, 11, 3, 12, 1, 0, 0, time.UTC)) {
		t.Fatalf("deadline 快照: ok=%v deadline=%q err=%v", ok, run.Deadline, dlErr)
	}

	// 未到期扫描不动
	clock.Set("2026-11-03T12:00:30.000000000Z")
	f.svc.ScanOnce(ctx)
	select {
	case r := <-done:
		t.Fatalf("未到期不得停止: %+v", r)
	default:
	}

	// 越过 deadline → 扫描触发停止信号 → 阻塞执行器被取消 → 终态 canceled
	clock.Set("2026-11-03T12:01:01.000000000Z")
	f.svc.ScanOnce(ctx)
	select {
	case r := <-done:
		if r.State != StateCanceled {
			t.Fatalf("墙钟到期终态须 canceled: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("墙钟停止后执行须返回")
	}
	evs, _ := f.svc.store.RunEvents(ctx, latestRunID(t, f.svc, "mi_w"))
	causeSeen := false
	for _, e := range evs {
		if (e.EventType == EventStopRequested || e.EventType == EventCanceled) &&
			strings.Contains(string(mustJSON(t, e.Payload)), CauseWallClockExceeded) {
			causeSeen = true
		}
	}
	if !causeSeen {
		t.Fatalf("停止事件须携 wall_clock_exceeded: %+v", evs)
	}
	if f.missions.status["mi_w"] != "ready" {
		t.Fatalf("停止后 Mission 回 ready: %v", f.missions.status)
	}
	// 再扫描幂等（已终态零信号）
	clock.Set("2026-11-03T12:05:00.000000000Z")
	f.svc.ScanOnce(ctx)
}

// TestWallClockUnsetNoDeadline 无墙钟（0 / 未设）不落 deadline、永不过期。
func TestWallClockUnsetNoDeadline(t *testing.T) {
	f, clock, exec := newWallFixture(t)
	ctx := context.Background()
	go func() { _, _ = f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_n", OwnerAgentID: "ag_x"}) }()
	<-exec.started
	waitForRunState(t, f.svc, "mi_n", StateRunning)
	run, _, _ := f.svc.store.RunByID(ctx, latestRunID(t, f.svc, "mi_n"))
	if run.Deadline != "" {
		t.Fatalf("无墙钟不得落 deadline: %+v", run)
	}
	clock.Set("2030-01-01T00:00:00.000000000Z")
	f.svc.ScanOnce(ctx)
	if _, ok, _ := f.svc.store.RunByID(ctx, latestRunID(t, f.svc, "mi_n")); !ok {
		t.Fatal("run 行不应消失")
	}
	f.svc.SignalStopByMission(ctx, "mi_n", "cleanup") // 收尾释放执行器
	time.Sleep(50 * time.Millisecond)
}

// TestMaxRunsDispatchCap max_runs 派发上限：已创建 Run 总数达上限 → 不再派发
// （ErrBudgetExhausted），Mission 停留 ready 不被静默吞。
func TestMaxRunsDispatchCap(t *testing.T) {
	f := newFixture(t) // 同步执行器（Submit 即返回）
	f.exec.block = "ok"
	ctx := context.Background()
	ref := MissionRef{MissionID: "mi_m", OwnerAgentID: "ag_x", MaxRuns: 2}
	if _, err := f.svc.ExecuteMission(ctx, ref); err != nil {
		t.Fatalf("第 1 次: %v", err)
	}
	if _, err := f.svc.ExecuteMission(ctx, ref); err != nil {
		t.Fatalf("第 2 次: %v", err)
	}
	if _, err := f.svc.ExecuteMission(ctx, ref); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("超上限须 ErrBudgetExhausted: %v", err)
	}
	if got, _ := f.svc.store.ListRunsByMission(ctx, "mi_m"); len(got) != 2 {
		t.Fatalf("恰 2 行: %d", len(got))
	}
	st, _ := f.svc.Status(ctx)
	if st["budget_exhausted_hits"] == nil {
		t.Fatalf("诊断面须计数: %v", st)
	}
}

func latestRunID(t *testing.T, s *Service, missionID string) string {
	t.Helper()
	runs, err := s.store.ListRunsByMission(context.Background(), missionID)
	if err != nil || len(runs) == 0 {
		t.Fatalf("runs: %v %d", err, len(runs))
	}
	return runs[len(runs)-1].RunID
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
