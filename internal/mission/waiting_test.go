package mission

// 0.6.7 Mission waiting 执行控制态（ADR-0030）：running → waiting(reason) →
// ready|draft；暂停保留等待原因、恢复回 waiting；取消两段照常；修订保持
// 投影；waiting 不可被调度。测试先行。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func readyRunning(t *testing.T, svc *Service) Mission {
	t.Helper()
	ctx := context.Background()
	m, err := svc.Create(ctx, "ag_a", fullContent(), nil)
	if err != nil || m.Status != StatusReady {
		t.Fatalf("建 ready: %+v %v", m, err)
	}
	if changed, err := svc.MarkRunning(ctx, m.MissionID); err != nil || !changed {
		t.Fatalf("预置 running: %v %v", changed, err)
	}
	return m
}

// TestMarkWaitingAndClear running → waiting(reconciliation)（事件携 reason，
// 等待原因入投影）→ waiting 不可调度 → ClearWaiting → ready + 清原因。
func TestMarkWaitingAndClear(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyRunning(t, svc)

	changed, err := svc.MarkWaiting(ctx, m.MissionID, "reconciliation")
	if err != nil || !changed {
		t.Fatalf("进 waiting: %v %v", changed, err)
	}
	got, _, _ := svc.Get(ctx, m.MissionID)
	if got.Status != StatusWaiting || got.WaitingReason != "reconciliation" {
		t.Fatalf("waiting 投影: %+v", got)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	last := evs[len(evs)-1]
	if last.EventType != EventStatusChanged || !strings.Contains(string(last.Payload), `"to":"waiting"`) ||
		!strings.Contains(string(last.Payload), `"reason":"reconciliation"`) {
		t.Fatalf("waiting 事件失真: %s", last.Payload)
	}
	// 摘要也带 waiting
	sums, _ := svc.ListMissions(ctx)
	if sums[0].Status != StatusWaiting {
		t.Fatalf("摘要状态: %+v", sums[0])
	}
	// waiting 不可调度（SetRunning 条件不符）
	if changed, err := svc.MarkRunning(ctx, m.MissionID); err != nil || changed {
		t.Fatalf("waiting 不可被调度: %v %v", changed, err)
	}
	// 幂等：已 waiting 再 MarkWaiting → changed=false 无操作
	if changed, err := svc.MarkWaiting(ctx, m.MissionID, "reconciliation"); err != nil || changed {
		t.Fatalf("重复进 waiting 应无操作: %v %v", changed, err)
	}

	// 处置后回调度列（完备 → ready）+ 清等待原因
	changed, err = svc.ClearWaiting(ctx, m.MissionID)
	if err != nil || !changed {
		t.Fatalf("出 waiting: %v %v", changed, err)
	}
	got, _, _ = svc.Get(ctx, m.MissionID)
	if got.Status != StatusReady || got.WaitingReason != "" {
		t.Fatalf("出 waiting 须 ready 且清原因: %+v", got)
	}
	// 非 waiting 清除 → 幂等无操作
	if changed, err := svc.ClearWaiting(ctx, m.MissionID); err != nil || changed {
		t.Fatalf("非 waiting 清除应无操作: %v %v", changed, err)
	}
}

// TestMarkWaitingFromRunningOrReadyOnly 仅 running | ready 可进 waiting（0.6.9
// 审校 #2：ready 残留未核验 Run 亦须进等待）；paused/draft/canceled 等无操作。
func TestMarkWaitingFromRunningOrReadyOnly(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	draft, _ := svc.Create(ctx, "", fullContent(), nil) // draft
	if changed, err := svc.MarkWaiting(ctx, draft.MissionID, "reconciliation"); err != nil || changed {
		t.Fatalf("draft 不可进 waiting: %v %v", changed, err)
	}
	paused, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
	svc.Pause(ctx, paused.MissionID, 1, "")
	if changed, err := svc.MarkWaiting(ctx, paused.MissionID, "reconciliation"); err != nil || changed {
		t.Fatalf("paused 不可进 waiting: %v %v", changed, err)
	}
	got, _, _ := svc.Get(ctx, paused.MissionID)
	if got.Status != StatusPaused || got.WaitingReason != "" {
		t.Fatalf("状态不得改: %+v", got)
	}
}

// TestClearWaitingToDraftWhenIncomplete 等待期间修订削验收 → 处置后回 draft
// （完备性重判，非盲回 ready）。
func TestClearWaitingToDraftWhenIncomplete(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyRunning(t, svc)
	svc.MarkWaiting(ctx, m.MissionID, "reconciliation")

	// waiting 中修订（投影保持 waiting；版本 +1）
	c := fullContent()
	c.AcceptanceCriteria = nil
	rev, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", "ag_a", c)
	if err != nil || rev.Status != StatusWaiting || rev.ContractVersion != 2 || rev.WaitingReason != "reconciliation" {
		t.Fatalf("waiting 中修订须保持投影与等待原因: %+v %v", rev, err)
	}
	changed, err := svc.ClearWaiting(ctx, m.MissionID)
	if err != nil || !changed {
		t.Fatalf("出 waiting: %v %v", changed, err)
	}
	got, _, _ := svc.Get(ctx, m.MissionID)
	if got.Status != StatusDraft || got.WaitingReason != "" {
		t.Fatalf("不完备须回 draft: %+v", got)
	}
}

// TestWaitingPauseResumeRoundTrip waiting → paused（等待原因保留）→ resume →
// waiting（非 ready——暂停/恢复不绕过禁止盲目重跑）；处置后 resume 才回 ready。
func TestWaitingPauseResumeRoundTrip(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyRunning(t, svc)
	svc.MarkWaiting(ctx, m.MissionID, "reconciliation")

	paused, err := svc.Pause(ctx, m.MissionID, 1, "先停")
	if err != nil || paused.Status != StatusPaused || paused.WaitingReason != "reconciliation" {
		t.Fatalf("waiting → paused 须保留等待原因: %+v %v", paused, err)
	}
	resumed, err := svc.Resume(ctx, m.MissionID, 1)
	if err != nil || resumed.Status != StatusWaiting || resumed.WaitingReason != "reconciliation" {
		t.Fatalf("恢复须回 waiting（非 ready）: %+v %v", resumed, err)
	}
	// 处置（ClearWaiting）后再暂停/恢复 → ready
	svc.ClearWaiting(ctx, m.MissionID)
	svc.Pause(ctx, m.MissionID, 1, "")
	back, err := svc.Resume(ctx, m.MissionID, 1)
	if err != nil || back.Status != StatusReady {
		t.Fatalf("处置后恢复回 ready: %+v %v", back, err)
	}
}

// TestWaitingCancelTwoStage waiting → canceling → canceled（两段照常）。
func TestWaitingCancelTwoStage(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyRunning(t, svc)
	svc.MarkWaiting(ctx, m.MissionID, "reconciliation")
	cing, err := svc.MarkCanceling(ctx, m.MissionID, 1, "不要了")
	if err != nil || cing.Status != StatusCanceling {
		t.Fatalf("waiting 取消: %+v %v", cing, err)
	}
	done, err := svc.FinishCancel(ctx, m.MissionID, "不要了")
	if err != nil || done.Status != StatusCanceled {
		t.Fatalf("收尾: %+v %v", done, err)
	}
	// 终态后 ClearWaiting 无操作（处置事实落 Run 侧——Mission 不翻转）
	if changed, err := svc.ClearWaiting(ctx, m.MissionID); err != nil || changed {
		t.Fatalf("canceled 上清等待应无操作: %v %v", changed, err)
	}
	if _, err := svc.Pause(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态不可暂停: %v", err)
	}
}
