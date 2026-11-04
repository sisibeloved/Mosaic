package run

// 0.6.7 Run 崩溃恢复（ADR-0030）：重启恢复结果不可判定 → unknown（非 failed）
// + Mission waiting(reconciliation)；核验处置（resolve——状态保持 unknown、
// 处置事实落 resolution + resolved 事件、Mission 回调度列）；停止后迟到结果
// 只审计（late_result——宽限内/宽限后两形态）；诊断面 unknown 计数——测试先行。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// seedRunning 播种未终态 Run（模拟进程重启前在执行）。
func seedRunning(t *testing.T, f fixture, runID string) {
	t.Helper()
	if _, err := f.svc.store.CreateRun(context.Background(), Run{
		RunID: runID, MissionID: "mi_a", Attempt: 1, LeaseID: "lse_seed",
		State: StateRunning, Generation: 3, RequestedAt: "t", StartedAt: "t", UpdatedAt: "t",
	}, 0); err != nil {
		t.Fatalf("播种: %v", err)
	}
}

// TestRecoverRunningToUnknownWaiting 重启恢复：running Mission 的未终态 Run →
// unknown（非 failed）+ unknown 事件 + 租约释放 + Mission → waiting
// (reconciliation)。
func TestRecoverRunningToUnknownWaiting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedRunning(t, f, "run_seed")
	f.missions.status["mi_a"] = "running"

	n, err := f.svc.RecoverUnfinished(ctx)
	if err != nil || n != 1 {
		t.Fatalf("恢复: %d %v", n, err)
	}
	got, _, _ := f.svc.GetRun(ctx, "run_seed")
	if got.State != StateUnknown {
		t.Fatalf("结果不可判定须 unknown（非 failed）: %+v", got)
	}
	if !strings.Contains(got.ErrorText, "结果未知") || !strings.Contains(got.ErrorText, "核验") {
		t.Fatalf("原因须说明须核验处置: %q", got.ErrorText)
	}
	if got.Resolution != "" || got.ResolvedAt != "" {
		t.Fatalf("恢复时不得预置处置: %+v", got)
	}
	if f.leases.released < 1 {
		t.Fatal("进程已死——租约须释放")
	}
	if f.missions.status["mi_a"] != "waiting" || f.missions.waitingReason["mi_a"] != "reconciliation" {
		t.Fatalf("Mission 须 waiting(reconciliation): %v / %v", f.missions.status, f.missions.waitingReason)
	}
	evs, _, _ := f.svc.RunEventsByRun(ctx, "run_seed")
	var hasUnknown bool
	for _, e := range evs {
		if e.EventType == EventUnknown && e.Payload["recovery"] == true {
			hasUnknown = true
		}
		if e.EventType == EventFailed {
			t.Fatalf("不得记 failed 事件: %+v", evs)
		}
	}
	if !hasUnknown {
		t.Fatalf("须有 unknown{recovery} 事件: %+v", evs)
	}
}

// TestResolveUnknownRun 核验处置：状态保持 unknown、resolution 落、resolved
// 事件、Mission 回调度列（ClearWaiting）；幂等/拒绝族。
func TestResolveUnknownRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedRunning(t, f, "run_seed")
	f.missions.status["mi_a"] = "running"
	if _, err := f.svc.RecoverUnfinished(ctx); err != nil {
		t.Fatalf("恢复: %v", err)
	}

	// 非法结论 / 备注超限
	if _, err := f.svc.ResolveRun(ctx, "run_seed", "maybe", ""); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("非法结论: %v", err)
	}
	if _, err := f.svc.ResolveRun(ctx, "run_seed", ResolutionVerifiedCompleted, strings.Repeat("字", 501)); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("备注超限: %v", err)
	}
	// 未知 Run
	if _, err := f.svc.ResolveRun(ctx, "run_none", ResolutionDiscarded, ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("未知 Run: %v", err)
	}

	out, err := f.svc.ResolveRun(ctx, "run_seed", ResolutionVerifiedCompleted, "核对工作区：产出齐全")
	if err != nil {
		t.Fatalf("处置: %v", err)
	}
	if out.State != StateUnknown {
		t.Fatalf("处置不得改写状态（用户 attest 非系统事实）: %+v", out)
	}
	if out.Resolution != ResolutionVerifiedCompleted || out.ResolutionNote != "核对工作区：产出齐全" || out.ResolvedAt == "" {
		t.Fatalf("处置事实: %+v", out)
	}
	if f.missions.status["mi_a"] != "ready" || f.missions.waitingReason["mi_a"] != "" {
		t.Fatalf("处置后 Mission 须回调度列且清等待原因: %v / %v", f.missions.status, f.missions.waitingReason)
	}
	evs, _, _ := f.svc.RunEventsByRun(ctx, "run_seed")
	var resolved int
	for _, e := range evs {
		if e.EventType == EventResolved && e.Payload["outcome"] == ResolutionVerifiedCompleted {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("须恰一条 resolved 事件: %+v", evs)
	}

	// 幂等：同结论重复 → 当前，不重复记事件
	again, err := f.svc.ResolveRun(ctx, "run_seed", ResolutionVerifiedCompleted, "再按")
	if err != nil || again.Resolution != ResolutionVerifiedCompleted || again.ResolutionNote != "核对工作区：产出齐全" {
		t.Fatalf("幂等重复: %+v %v", again, err)
	}
	evs, _, _ = f.svc.RunEventsByRun(ctx, "run_seed")
	resolved = 0
	for _, e := range evs {
		if e.EventType == EventResolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("幂等不得重复记事件: %d", resolved)
	}
	// 异结论 → 已处置拒绝
	if _, err := f.svc.ResolveRun(ctx, "run_seed", ResolutionDiscarded, ""); !errors.Is(err, ErrRunAlreadyResolved) {
		t.Fatalf("异结论须拒绝: %v", err)
	}

	// 非 unknown（completed）→ 拒绝
	r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil || r.State != StateCompleted {
		t.Fatalf("处置后可再执行（attempt 2）: %+v %v", r, err)
	}
	if r.Attempt != 2 {
		t.Fatalf("attempt 递增: %d", r.Attempt)
	}
	if _, err := f.svc.ResolveRun(ctx, r.RunID, ResolutionDiscarded, ""); !errors.Is(err, ErrRunNotUnknown) {
		t.Fatalf("非 unknown 不可处置: %v", err)
	}
}

// TestResolveThreeOutcomesAllReleaseMission 三种结论均放回调度列（0.6.7 无
// Mission 完成态——ready = 可再执行）。
func TestResolveThreeOutcomesAllReleaseMission(t *testing.T) {
	for _, outcome := range []string{ResolutionVerifiedCompleted, ResolutionVerifiedFailed, ResolutionDiscarded} {
		f := newFixture(t)
		ctx := context.Background()
		seedRunning(t, f, "run_seed")
		f.missions.status["mi_a"] = "running"
		_, _ = f.svc.RecoverUnfinished(ctx)
		if _, err := f.svc.ResolveRun(ctx, "run_seed", outcome, ""); err != nil {
			t.Fatalf("%s: %v", outcome, err)
		}
		if f.missions.status["mi_a"] != "ready" {
			t.Fatalf("%s: Mission 须回 ready: %v", outcome, f.missions.status)
		}
	}
}

// TestUnknownBlocksScheduling waiting 出调度列：ListReady 不含（fake 以 status
// 过滤）；手动触发 SetRunning 条件不符拒启动（Run failed 不启动执行器）。
func TestUnknownBlocksScheduling(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedRunning(t, f, "run_seed")
	f.missions.status["mi_a"] = "running"
	_, _ = f.svc.RecoverUnfinished(ctx)

	r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("手动触发: %v", err)
	}
	if r.State != StateFailed || f.exec.calls != 0 {
		t.Fatalf("waiting 期间手动触发须拒启动（不进执行器）: %+v calls=%d", r, f.exec.calls)
	}
}

// TestStatusCountsUnresolvedUnknown 诊断面：未处置 unknown 计数随处置归零。
func TestStatusCountsUnresolvedUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedRunning(t, f, "run_seed")
	f.missions.status["mi_a"] = "running"
	_, _ = f.svc.RecoverUnfinished(ctx)
	st, _ := f.svc.Status(ctx)
	if st["unresolved_unknown_runs"] != 1 {
		t.Fatalf("未处置计数: %v", st["unresolved_unknown_runs"])
	}
	_, _ = f.svc.ResolveRun(ctx, "run_seed", ResolutionDiscarded, "")
	st, _ = f.svc.Status(ctx)
	if st["unresolved_unknown_runs"] != 0 {
		t.Fatalf("处置后归零: %v", st["unresolved_unknown_runs"])
	}
}

// TestLateResultAuditedWithinGrace 停止后执行器结果在宽限内到达 → late_result
// 事件（只审计：Run 仍 canceled、result_block 空、published=false）。
func TestLateResultAuditedWithinGrace(t *testing.T) {
	f := newFixture(t)
	exec := newPauseExec()
	exec.mu.Lock()
	exec.late = true // ctx 取消后仍返回"迟到结果正文"
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
	_, _ = f.svc.SignalStopByMission(ctx, "mi_a", "cancel")
	var r Run
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("停止后未返回")
	}
	if r.State != StateCanceled || r.ResultBlock != "" {
		t.Fatalf("Run 须 canceled 且结果不落: %+v", r)
	}
	late := awaitEvent(t, f.svc, r.RunID, EventLateResult, 2*time.Second)
	if late.Payload["outcome"] != "ok" || late.Payload["published"] != false {
		t.Fatalf("late_result 载荷: %+v", late.Payload)
	}
	if p, _ := late.Payload["preview"].(string); !strings.Contains(p, "迟到结果正文") {
		t.Fatalf("预览须含正文片段: %+v", late.Payload)
	}
	got, _, _ := f.svc.GetRun(ctx, r.RunID)
	if got.State != StateCanceled || got.ResultBlock != "" {
		t.Fatalf("审计不得改行: %+v", got)
	}
}

// TestLateResultAuditedAfterGrace 宽限后到达 → 后台等待审计仍落 late_result。
func TestLateResultAuditedAfterGrace(t *testing.T) {
	f := newFixture(t)
	WithStopGrace(10 * time.Millisecond)(f.svc)
	f.svc.executor = &lateAfterExec{delay: 80 * time.Millisecond}
	ctx := context.Background()
	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		done <- r
	}()
	waitForRunState(t, f.svc, "mi_a", StateRunning)
	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "paused"
	f.missions.mu.Unlock()
	_, _ = f.svc.SignalStopByMission(ctx, "mi_a", "pause")
	var r Run
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("停止后未返回")
	}
	late := awaitEvent(t, f.svc, r.RunID, EventLateResult, 2*time.Second)
	if late.Payload["outcome"] != "error" || late.Payload["phase"] != "after_stop" {
		t.Fatalf("宽限后迟到审计: %+v", late.Payload)
	}
}

// lateAfterExec ctx 取消后延迟 delay 再返回错误（宽限后迟到面）。
type lateAfterExec struct{ delay time.Duration }

func (e *lateAfterExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	<-ctx.Done()
	time.Sleep(e.delay)
	return "", errors.New("执行器被中断后退出")
}

// awaitEvent 轮询等待某事件出现。
func awaitEvent(t *testing.T, s *Service, runID, eventType string, timeout time.Duration) Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs, _, _ := s.RunEventsByRun(context.Background(), runID)
		for _, e := range evs {
			if e.EventType == eventType {
				return e
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待事件 %s 超时", eventType)
	return Event{}
}
