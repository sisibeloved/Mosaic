package mission

// 0.6.9 审校修复（ADR-0032 v0.2）：#3 送验轮次守卫（同包重审也不串轮）、#8 CAS
// 未胜但同包同版本 → 幂等 200、#9 验收人 ≤64 字、#10 退回事件目标为真实状态、
// #13 取消清受审包锚、#2 ready 残留未核验 Run 也能进等待。测试先行。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func reviewable(t *testing.T, svc *Service) Mission {
	t.Helper()
	m, err := svc.Create(context.Background(), "ag_a", fullContent(), nil)
	if err != nil || m.Status != StatusReady {
		t.Fatalf("建 ready: %+v %v", m, err)
	}
	return m
}

// TestRecordReviewRoundGuard 审校 #3：裁定须携带送验轮次——退回 A 重送 B 后，
// 旧轮次的接受/退回请求一律冲突；同包重审轮次也递增（单比包 ID 不足以防串轮）。
func TestRecordReviewRoundGuard(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc)

	r1, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_a")
	if err != nil || r1.ReviewRound != 1 {
		t.Fatalf("首轮送验: %+v %v", r1, err)
	}
	// 缺轮次 → 不合法
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 0, ReviewDecision{Decision: ReviewAccepted}); !errors.Is(err, ErrInvalidReview) {
		t.Fatalf("裁定须携轮次: %v", err)
	}
	// 轮次正确 → 退回
	out, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewReturned, Reason: "A 不足"})
	if err != nil || out.Status != StatusReady || out.ReviewRound != 1 {
		t.Fatalf("首轮退回（轮次单调保留——防轮次值跨轮复用）: %+v %v", out, err)
	}
	// 重送 B → 轮次 2
	r2, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_b")
	if err != nil || r2.ReviewRound != 2 || r2.ReviewPackageID != "dlv_b" {
		t.Fatalf("次轮送验: %+v %v", r2, err)
	}
	// 旧轮次（1）的裁定——无论接受还是重放旧退回——一律冲突
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewAccepted}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("旧轮次接受须冲突: %v", err)
	}
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewReturned, Reason: "A 不足"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("重放旧轮次退回须冲突: %v", err)
	}
	// 同包重审：退回 B → 再送 B → 轮次 3；旧轮次 2 的裁定仍冲突
	svc.RecordReview(ctx, m.MissionID, 1, 2, ReviewDecision{Decision: ReviewReturned, Reason: "B 再看"})
	r3, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_b")
	if err != nil || r3.ReviewRound != 3 {
		t.Fatalf("同包重审轮次递增: %+v %v", r3, err)
	}
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 2, ReviewDecision{Decision: ReviewAccepted}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("同包旧轮次裁定须冲突: %v", err)
	}
	// 当前轮次（3）接受 → accepted，事件与锚同为 dlv_b
	acc, err := svc.RecordReview(ctx, m.MissionID, 1, 3, ReviewDecision{Decision: ReviewAccepted, Reason: "B 达成"})
	if err != nil || acc.Status != StatusAccepted || acc.ReviewPackageID != "dlv_b" {
		t.Fatalf("当轮接受: %+v %v", acc, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	var lastAcc map[string]any
	for _, e := range evs {
		if strings.Contains(string(e.Payload), `"decision":"accepted"`) {
			_ = json.Unmarshal(e.Payload, &lastAcc)
		}
	}
	if lastAcc["package_id"] != "dlv_b" {
		t.Fatalf("接受事件锚须为当轮包: %v", lastAcc)
	}
}

// TestRequestReviewIdempotentCASMiss 审校 #8：并发同版本同包送验，CAS 未胜者重读
// 后发现 reviewing 且同包同版本 → 返回当前（200 语义），而非 409。
func TestRequestReviewIdempotentCASMiss(t *testing.T) {
	svc, mem := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc)
	// 直接落一轮 reviewing（模拟并发胜者）
	if _, ok, err := mem.RequestReview(ctx, m.MissionID, 1, "dlv_a", Event{MissionID: m.MissionID, EventType: EventStatusChanged, Payload: []byte(`{}`), OccurredAt: "t"}); err != nil || !ok {
		t.Fatalf("预置 reviewing: %v %v", ok, err)
	}
	// 服务层 CAS 未胜路径（当前已是 reviewing、同包同版本）→ 幂等返回
	out, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_a")
	if err != nil || out.Status != StatusReviewing || out.ReviewPackageID != "dlv_a" || out.ReviewRound != 1 {
		t.Fatalf("同包幂等: %+v %v", out, err)
	}
	// 换包仍冲突
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_z"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("换包须冲突: %v", err)
	}
}

// TestRecordReviewReviewerLimit 审校 #9：验收人 >64 字拒绝（不入事件）。
func TestRecordReviewReviewerLimit(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc)
	svc.RequestReview(ctx, m.MissionID, 1, "dlv_a")
	long := strings.Repeat("字", 65)
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewAccepted, Reviewer: long}); !errors.Is(err, ErrInvalidReview) {
		t.Fatalf("验收人超限须拒绝: %v", err)
	}
	edge := strings.Repeat("字", 64)
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewAccepted, Reviewer: edge}); err != nil {
		t.Fatalf("恰 64 字应通过: %v", err)
	}
}

// TestRecordReviewReturnedEventTarget 审校 #10：退回事件的 to 为真实目标状态
// ready（decision 仍为 returned）。
func TestRecordReviewReturnedEventTarget(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc)
	svc.RequestReview(ctx, m.MissionID, 1, "dlv_a")
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewReturned, Reason: "补证"}); err != nil {
		t.Fatalf("退回: %v", err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	var payload map[string]any
	_ = json.Unmarshal(evs[len(evs)-1].Payload, &payload)
	if payload["to"] != StatusReady || payload["decision"] != ReviewReturned {
		t.Fatalf("退回事件目标须为 ready: %v", payload)
	}
}

// TestCancelClearsReviewAnchor 审校 #13：reviewing 两段取消后受审包锚清空
// （送验事件中的包 ID 留痕不改写）。
func TestCancelClearsReviewAnchor(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc)
	svc.RequestReview(ctx, m.MissionID, 1, "dlv_a")
	if _, err := svc.MarkCanceling(ctx, m.MissionID, 1, "不要了"); err != nil {
		t.Fatalf("取消: %v", err)
	}
	if got, _, _ := svc.Get(ctx, m.MissionID); got.Status != StatusCanceling || got.ReviewPackageID != "" {
		t.Fatalf("canceling 须清锚: %+v", got)
	}
	done, _ := svc.FinishCancel(ctx, m.MissionID, "不要了")
	if done.Status != StatusCanceled || done.ReviewPackageID != "" {
		t.Fatalf("canceled 须清锚: %+v", done)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	var sawPkg bool
	for _, e := range evs {
		if strings.Contains(string(e.Payload), `"package_id":"dlv_a"`) {
			sawPkg = true
		}
	}
	if !sawPkg {
		t.Fatal("送验事件中的包 ID 须留痕")
	}
}

// TestMarkWaitingFromReady 审校 #2：ready 残留未核验 Run（Run 落库后、投影
// running 前崩溃）恢复时也进 waiting——不再是"ready + 未处置 unknown"死角。
func TestMarkWaitingFromReady(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc) // ready（未投影 running）
	changed, err := svc.MarkWaiting(ctx, m.MissionID, "reconciliation")
	if err != nil || !changed {
		t.Fatalf("ready 也可进等待: %v %v", changed, err)
	}
	got, _, _ := svc.Get(ctx, m.MissionID)
	if got.Status != StatusWaiting || got.WaitingReason != "reconciliation" {
		t.Fatalf("等待投影: %+v", got)
	}
	// 幂等：已 waiting 再进 → 无操作
	if changed, _ := svc.MarkWaiting(ctx, m.MissionID, "reconciliation"); changed {
		t.Fatal("重复进等待应无操作")
	}
}
