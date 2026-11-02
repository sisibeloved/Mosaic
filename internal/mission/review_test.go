package mission

// 0.6.9 Mission 验收（ADR-0032）：ready → reviewing（request_review CAS +
// review_package_id）→ accepted（终态）| ready（退回清锚）；验收期间契约冻结
// （revise/pause 拒）；取消两段照常；验收权限为契约一致性校验。测试先行。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func readyWithReviewer(t *testing.T, svc *Service, reviewer string) Mission {
	t.Helper()
	c := fullContent()
	c.Reviewer = reviewer
	m, err := svc.Create(context.Background(), "ag_a", c, nil)
	if err != nil || m.Status != StatusReady {
		t.Fatalf("建 ready: %+v %v", m, err)
	}
	return m
}

// TestRequestReviewCycle ready → reviewing（锚 + 事件）；幂等同包；换包拒绝；
// reviewing 不可调度/修订/暂停；取消两段照常。
func TestRequestReviewCycle(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")

	out, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_1")
	if err != nil || out.Status != StatusReviewing || out.ReviewPackageID != "dlv_1" || out.ContractVersion != 1 {
		t.Fatalf("送验: %+v %v", out, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	last := evs[len(evs)-1]
	if !strings.Contains(string(last.Payload), `"to":"reviewing"`) || !strings.Contains(string(last.Payload), `"package_id":"dlv_1"`) {
		t.Fatalf("送验事件须携包锚: %s", last.Payload)
	}
	// 幂等同包；换包拒绝
	if again, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_1"); err != nil || again.Status != StatusReviewing {
		t.Fatalf("同包幂等: %+v %v", again, err)
	}
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_2"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reviewing 中换包须拒绝（先退回）: %v", err)
	}
	// 版本冲突
	if _, err := svc.RequestReview(ctx, m.MissionID, 9, "dlv_1"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本冲突: %v", err)
	}
	// 验收期间：不可调度、契约冻结、不可暂停
	if changed, _ := svc.MarkRunning(ctx, m.MissionID); changed {
		t.Fatal("reviewing 不可被调度")
	}
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", fullContent()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("验收期间修订须拒绝（基线冻结）: %v", err)
	}
	if _, err := svc.Pause(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("验收期间暂停须拒绝: %v", err)
	}
	// 取消两段照常
	if _, err := svc.MarkCanceling(ctx, m.MissionID, 1, "不要了"); err != nil {
		t.Fatalf("reviewing 可取消: %v", err)
	}
	if done, err := svc.FinishCancel(ctx, m.MissionID, "不要了"); err != nil || done.Status != StatusCanceled {
		t.Fatalf("收尾: %+v %v", done, err)
	}
}

// TestRequestReviewRejects 非 ready 入口拒绝：draft / running / waiting / paused。
func TestRequestReviewRejects(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	draft, _ := svc.Create(ctx, "", fullContent(), nil)
	if _, err := svc.RequestReview(ctx, draft.MissionID, 1, "dlv_x"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("draft: %v", err)
	}
	for name, pre := range map[string]func(id string){
		"running": func(id string) { svc.MarkRunning(ctx, id) },
		"waiting": func(id string) { svc.MarkRunning(ctx, id); svc.MarkWaiting(ctx, id, "reconciliation") },
		"paused":  func(id string) { svc.Pause(ctx, id, 1, "") },
	} {
		m := readyWithReviewer(t, svc, "")
		pre(m.MissionID)
		if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_x"); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s 不可送验: %v", name, err)
		}
	}
	if _, err := svc.RequestReview(ctx, "mi_none", 1, "dlv_x"); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("未知: %v", err)
	}
}

// TestRecordReviewAcceptedTerminal accepted 终态：锚保留、事件携裁定；此后修订/
// 取消/暂停/送验/调度全拒绝；不被投影翻转。
func TestRecordReviewAcceptedTerminal(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")
	svc.RequestReview(ctx, m.MissionID, 1, "dlv_1")

	out, err := svc.RecordReview(ctx, m.MissionID, 1, ReviewDecision{
		Decision: ReviewAccepted, Reviewer: "owner", Reason: "全部达成",
		ItemNotes: []ReviewItemNote{{Criterion: "七端点全绿", Note: "CI 绿"}},
	})
	if err != nil || out.Status != StatusAccepted || out.ReviewPackageID != "dlv_1" {
		t.Fatalf("接受: %+v %v", out, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	last := string(evs[len(evs)-1].Payload)
	if !strings.Contains(last, `"decision":"accepted"`) || !strings.Contains(last, `"package_id":"dlv_1"`) || !strings.Contains(last, `"七端点全绿"`) {
		t.Fatalf("裁定事件须携 decision/包锚/逐项依据: %s", last)
	}
	// 终态拒绝族
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", fullContent()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("accepted 修订: %v", err)
	}
	if _, err := svc.MarkCanceling(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("accepted 取消: %v", err)
	}
	if _, err := svc.Pause(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("accepted 暂停: %v", err)
	}
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_2"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("accepted 再送验: %v", err)
	}
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, ReviewDecision{Decision: ReviewReturned, Reason: "x"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("accepted 再裁定: %v", err)
	}
	if changed, _ := svc.MarkRunning(ctx, m.MissionID); changed {
		t.Fatal("accepted 不可调度")
	}
	if err := svc.MarkReadyFromRunning(ctx, m.MissionID, "late"); err != nil {
		t.Fatalf("投影不应报错: %v", err)
	}
	if got, _, _ := svc.Get(ctx, m.MissionID); got.Status != StatusAccepted {
		t.Fatalf("终态不被迟到投影翻转: %+v", got)
	}
}

// TestRecordReviewReturned 退回：→ ready、锚清空、reason 留痕；可再调度/再送验。
func TestRecordReviewReturned(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")
	svc.RequestReview(ctx, m.MissionID, 1, "dlv_1")

	// returned 缺 reason → 不合法
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, ReviewDecision{Decision: ReviewReturned}); !errors.Is(err, ErrInvalidReview) {
		t.Fatalf("退回须携原因: %v", err)
	}
	out, err := svc.RecordReview(ctx, m.MissionID, 1, ReviewDecision{Decision: ReviewReturned, Reason: "第二条缺证据"})
	if err != nil || out.Status != StatusReady || out.ReviewPackageID != "" {
		t.Fatalf("退回: %+v %v", out, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	if !strings.Contains(string(evs[len(evs)-1].Payload), `"reason":"第二条缺证据"`) {
		t.Fatalf("退回原因留痕: %s", evs[len(evs)-1].Payload)
	}
	if changed, _ := svc.MarkRunning(ctx, m.MissionID); !changed {
		t.Fatal("退回后可再调度")
	}
	svc.MarkReadyFromRunning(ctx, m.MissionID, "run_completed")
	if again, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_2"); err != nil || again.ReviewPackageID != "dlv_2" {
		t.Fatalf("退回后可再送验（另指新包）: %+v %v", again, err)
	}
}

// TestRecordReviewAuthorizationAndValidation 契约 reviewer 非空须相等（403 族）；
// 空 = 自验；形状上限；非 reviewing 拒绝；版本冲突。
func TestRecordReviewAuthorizationAndValidation(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "qa@team")
	svc.RequestReview(ctx, m.MissionID, 1, "dlv_1")

	if _, err := svc.RecordReview(ctx, m.MissionID, 1, ReviewDecision{Decision: ReviewAccepted, Reviewer: "someone"}); !errors.Is(err, ErrReviewNotAuthorized) {
		t.Fatalf("验收人不符须拒绝: %v", err)
	}
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, ReviewDecision{Decision: ReviewAccepted}); !errors.Is(err, ErrReviewNotAuthorized) {
		t.Fatalf("契约指定验收人时缺 reviewer 须拒绝: %v", err)
	}
	// 形状：decision 枚举 / reason 超长 / item_notes 超项
	for name, d := range map[string]ReviewDecision{
		"枚举":   {Decision: "maybe", Reviewer: "qa@team"},
		"原因超长": {Decision: ReviewAccepted, Reviewer: "qa@team", Reason: strings.Repeat("字", 501)},
		"逐项超项": {Decision: ReviewAccepted, Reviewer: "qa@team", ItemNotes: make([]ReviewItemNote, 17)},
		"条文空":  {Decision: ReviewAccepted, Reviewer: "qa@team", ItemNotes: []ReviewItemNote{{Criterion: "", Note: "x"}}},
	} {
		if _, err := svc.RecordReview(ctx, m.MissionID, 1, d); !errors.Is(err, ErrInvalidReview) {
			t.Errorf("%s 应 ErrInvalidReview: %v", name, err)
		}
	}
	// 版本冲突
	if _, err := svc.RecordReview(ctx, m.MissionID, 9, ReviewDecision{Decision: ReviewAccepted, Reviewer: "qa@team"}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本冲突: %v", err)
	}
	// 正确验收人通过
	if out, err := svc.RecordReview(ctx, m.MissionID, 1, ReviewDecision{Decision: ReviewAccepted, Reviewer: "qa@team"}); err != nil || out.Status != StatusAccepted {
		t.Fatalf("验收人匹配: %+v %v", out, err)
	}
	// 非 reviewing 裁定拒绝
	other := readyWithReviewer(t, svc, "")
	if _, err := svc.RecordReview(ctx, other.MissionID, 1, ReviewDecision{Decision: ReviewAccepted}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("非 reviewing 裁定: %v", err)
	}
}
