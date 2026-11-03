package mission

// 0.6.12 Mission 责任交接（ADR-0035）：两段式（offer → accept | decline |
// revoke）+ 承接回执（handover_accepted 事件）+ 在途交接冻结窗（Revise /
// RequestReview / MarkCanceling 拒，Pause 照常）。归属翻转不递增
// contract_version（控制面事实）；unassigned draft 承接后完备性重判入 ready。
// 测试先行（红→绿）。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func handoverAgents() fakeAgents {
	return fakeAgents{ids: map[string]bool{"ag_a": true, "ag_b": true, "ag_c": true}}
}

// TestHandoverOfferAcceptCycle 基本环：发起（pending 可见、owner 不变、版本
// 不递增、事件留痕）→ 承接（owner 翻转、回执事件、pending 清空）→ 历史读面。
func TestHandoverOfferAcceptCycle(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "") // owner ag_a, ready v1

	out, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "在途 Run 无；验收标准见契约 v1")
	if err != nil || out.PendingOwnerAgentID != "ag_b" || out.OwnerAgentID != "ag_a" {
		t.Fatalf("发起: %+v %v", out, err)
	}
	if out.ContractVersion != 1 {
		t.Fatalf("发起不得递增契约版本: v%d", out.ContractVersion)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	last := evs[len(evs)-1]
	if last.EventType != EventHandoverOffered ||
		!strings.Contains(string(last.Payload), `"from_owner":"ag_a"`) ||
		!strings.Contains(string(last.Payload), `"to_owner":"ag_b"`) ||
		!strings.Contains(string(last.Payload), `"contract_version":1`) {
		t.Fatalf("发起事件失真: %s %s", last.EventType, last.Payload)
	}

	acc, err := svc.HandoverAccept(ctx, m.MissionID, 1, "已接手")
	if err != nil || acc.OwnerAgentID != "ag_b" || acc.PendingOwnerAgentID != "" {
		t.Fatalf("承接: %+v %v", acc, err)
	}
	if acc.ContractVersion != 1 {
		t.Fatalf("承接不得递增契约版本: v%d", acc.ContractVersion)
	}
	if acc.Status != StatusReady {
		t.Fatalf("ready 承接保持 ready: %s", acc.Status)
	}
	evs, _, _ = svc.MissionEvents(ctx, m.MissionID)
	last = evs[len(evs)-1]
	if last.EventType != EventHandoverAccepted ||
		!strings.Contains(string(last.Payload), `"from_owner":"ag_a"`) ||
		!strings.Contains(string(last.Payload), `"to_owner":"ag_b"`) ||
		!strings.Contains(string(last.Payload), `"status_at_accept":"ready"`) ||
		!strings.Contains(string(last.Payload), `"note":"已接手"`) {
		t.Fatalf("承接回执失真: %s %s", last.EventType, last.Payload)
	}

	// 版本行不被交接增改（归属翻转不是契约修订）
	vs, _, _ := svc.MissionVersions(ctx, m.MissionID)
	if len(vs) != 1 {
		t.Fatalf("版本行须仍 1 行: %d", len(vs))
	}
	// 历史读面：offered + accepted 两事实按 seq 升序
	hf, ok, err := svc.HandoverHistory(ctx, m.MissionID)
	if err != nil || !ok || len(hf) != 2 {
		t.Fatalf("历史: %v %v %d", hf, err, len(hf))
	}
	if hf[0].Kind != "offered" || hf[1].Kind != "accepted" ||
		hf[1].FromOwnerAgentID != "ag_a" || hf[1].ToOwnerAgentID != "ag_b" {
		t.Fatalf("历史失真: %+v", hf)
	}
}

// TestHandoverOfferStateGuards 发起状态族：draft/ready/running/waiting/paused
// 允许；reviewing/canceling/accepted/canceled 拒绝。
func TestHandoverOfferStateGuards(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()

	draft, _ := svc.Create(ctx, "", fullContent(), nil) // unassigned draft
	if _, err := svc.HandoverOffer(ctx, draft.MissionID, 1, "ag_b", ""); err != nil {
		t.Fatalf("draft 应可交接: %v", err)
	}

	for name, reach := range map[string]func(m Mission) Mission{
		"running": func(m Mission) Mission { svc.MarkRunning(ctx, m.MissionID); return m },
		"waiting": func(m Mission) Mission {
			svc.MarkRunning(ctx, m.MissionID)
			svc.MarkWaiting(ctx, m.MissionID, WaitingReconciliation)
			return m
		},
		"paused": func(m Mission) Mission {
			svc.MarkRunning(ctx, m.MissionID)
			svc.Pause(ctx, m.MissionID, m.ContractVersion, "")
			return m
		},
		"reviewing": func(m Mission) Mission {
			svc.RequestReview(ctx, m.MissionID, 1, "dlv_1")
			return m
		},
		"canceling": func(m Mission) Mission {
			svc.MarkCanceling(ctx, m.MissionID, 1, "不要了")
			return m
		},
		"accepted": func(m Mission) Mission {
			svc.RequestReview(ctx, m.MissionID, 1, "dlv_1")
			svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewAccepted})
			return m
		},
		"canceled": func(m Mission) Mission {
			svc.MarkCanceling(ctx, m.MissionID, 1, "不要了")
			svc.FinishCancel(ctx, m.MissionID, "不要了")
			return m
		},
	} {
		m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
		m = reach(m)
		_, err := svc.HandoverOffer(ctx, m.MissionID, m.ContractVersion, "ag_b", "")
		wantErr := name == "reviewing" || name == "canceling" || name == "accepted" || name == "canceled"
		if wantErr && !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s 须拒绝: %v", name, err)
		}
		if !wantErr && err != nil {
			t.Fatalf("%s 应允许: %v", name, err)
		}
	}
}

// TestHandoverOfferValidation 发起形状/目标校验：同 owner、在途唯一、目标
// 不存在、目标非 active、owner 空、notes 超限、CAS 冲突。
func TestHandoverOfferValidation(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{
		ids:      map[string]bool{"ag_a": true, "ag_b": true, "ag_s": true, "ag_r": true},
		statuses: map[string]string{"ag_s": "suspended", "ag_r": "retiring"},
	})
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")

	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_a", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("同 owner 须拒绝: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "", ""); !errors.Is(err, ErrInvalidMission) {
		t.Fatalf("空承接人须拒绝: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_x", ""); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("目标不存在: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_s", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("suspended 承接人须拒绝: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_r", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retiring 承接人须拒绝: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", strings.Repeat("字", 2001)); !errors.Is(err, ErrInvalidMission) {
		t.Fatalf("notes 超限: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 9, "ag_b", ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("CAS 冲突: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, "mi_none", 1, "ag_b", ""); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("任务不存在: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "一次"); err != nil {
		t.Fatalf("首次发起: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_c", "二次"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("在途唯一（二 offer 须先了结）: %v", err)
	}
}

// TestHandoverFreezeWindow 在途交接冻结窗：Revise / RequestReview /
// MarkCanceling 拒绝；Pause / ReopenRequest 照常。
func TestHandoverFreezeWindow(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", ""); err != nil {
		t.Fatalf("发起: %v", err)
	}
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", fullContent()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("冻结窗内修订须拒绝: %v", err)
	}
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("冻结窗内送验须拒绝: %v", err)
	}
	if _, err := svc.MarkCanceling(ctx, m.MissionID, 1, "不要了"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("冻结窗内取消须拒绝: %v", err)
	}
	if _, err := svc.Pause(ctx, m.MissionID, 1, "排空"); err != nil {
		t.Fatalf("冻结窗内暂停照常: %v", err)
	}
	if _, err := svc.Resume(ctx, m.MissionID, 1); err != nil {
		t.Fatalf("冻结窗内恢复照常: %v", err)
	}
	if _, err := svc.ReopenRequest(ctx, m.MissionID, 1, "rop_1", "决定 X", []string{"证据"}, nil, ""); err != nil {
		t.Fatalf("冻结窗内重开请求照常（append-only 事实）: %v", err)
	}
}

// TestHandoverAcceptCompleteness unassigned draft 承接 → 完备性重判 ready
// （status_changed reason=handover）+ 入调度列。
func TestHandoverAcceptCompleteness(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()
	draft, _ := svc.Create(ctx, "", fullContent(), nil) // 齐备但 unassigned → draft
	if draft.Status != StatusDraft {
		t.Fatalf("前置: %s", draft.Status)
	}
	if _, err := svc.HandoverOffer(ctx, draft.MissionID, 1, "ag_b", ""); err != nil {
		t.Fatalf("发起: %v", err)
	}
	if got, _, _ := svc.Get(ctx, draft.MissionID); got.Status != StatusDraft || got.OwnerAgentID != "" {
		t.Fatalf("发起不改归属与状态: %+v", got)
	}
	acc, err := svc.HandoverAccept(ctx, draft.MissionID, 1, "")
	if err != nil || acc.Status != StatusReady || acc.OwnerAgentID != "ag_b" {
		t.Fatalf("承接完备性重判: %+v %v", acc, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, draft.MissionID)
	found := false
	for _, e := range evs {
		if e.EventType == EventStatusChanged &&
			strings.Contains(string(e.Payload), `"to":"ready"`) &&
			strings.Contains(string(e.Payload), `"reason":"handover"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("缺 draft→ready 的 status_changed（reason=handover）事件")
	}
	ready, _ := svc.ListByStatus(ctx, StatusReady)
	hit := false
	for _, s := range ready {
		if s.MissionID == draft.MissionID {
			hit = true
		}
	}
	if !hit {
		t.Fatal("承接后须入 ready 调度列")
	}
	// 不完备 draft 承接保持 draft（缺验收）
	c := fullContent()
	c.AcceptanceCriteria = nil
	d2, _ := svc.Create(ctx, "", c, nil)
	svc.HandoverOffer(ctx, d2.MissionID, 1, "ag_b", "")
	acc2, err := svc.HandoverAccept(ctx, d2.MissionID, 1, "")
	if err != nil || acc2.Status != StatusDraft || acc2.OwnerAgentID != "ag_b" {
		t.Fatalf("不完备保持 draft + 归属仍翻转: %+v %v", acc2, err)
	}
}

// TestHandoverAcceptExecutionProjection 执行控制投影保持：running / waiting /
// paused 承接不翻状态（回执记 status_at_accept）。
func TestHandoverAcceptExecutionProjection(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()
	for name, pre := range map[string]func(id string) string{
		"running": func(id string) string {
			svc.MarkRunning(ctx, id)
			return StatusRunning
		},
		"waiting": func(id string) string {
			svc.MarkRunning(ctx, id)
			svc.MarkWaiting(ctx, id, WaitingReconciliation)
			return StatusWaiting
		},
		"paused": func(id string) string {
			svc.MarkRunning(ctx, id)
			svc.Pause(ctx, id, 1, "")
			return StatusPaused
		},
	} {
		m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
		want := pre(m.MissionID)
		svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
		acc, err := svc.HandoverAccept(ctx, m.MissionID, 1, "")
		if err != nil || acc.Status != want || acc.OwnerAgentID != "ag_b" {
			t.Fatalf("%s 投影保持: %+v %v（want %s）", name, acc, err, want)
		}
		evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
		last := evs[len(evs)-1]
		if !strings.Contains(string(last.Payload), `"status_at_accept":"`+want+`"`) {
			t.Fatalf("%s 回执须记承接时状态: %s", name, last.Payload)
		}
	}
}

// TestHandoverDeclineRevoke 婉拒/撤回了结在途交接（事件留痕、可再发起）；
// 无在途时拒绝；reason 超限拒绝。
func TestHandoverDeclineRevoke(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")

	if _, err := svc.HandoverDecline(ctx, m.MissionID, 1, "没理由"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("无在途婉拒须拒绝: %v", err)
	}
	if _, err := svc.HandoverRevoke(ctx, m.MissionID, 1, "没理由"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("无在途撤回须拒绝: %v", err)
	}

	svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
	d, err := svc.HandoverDecline(ctx, m.MissionID, 1, "暂不接手")
	if err != nil || d.PendingOwnerAgentID != "" || d.OwnerAgentID != "ag_a" {
		t.Fatalf("婉拒: %+v %v", d, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	if evs[len(evs)-1].EventType != EventHandoverDeclined ||
		!strings.Contains(string(evs[len(evs)-1].Payload), `"reason":"暂不接手"`) {
		t.Fatalf("婉拒事件失真: %s", evs[len(evs)-1].Payload)
	}

	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_c", strings.Repeat("字", 1)); err != nil {
		t.Fatalf("婉拒后可再发起: %v", err)
	}
	r, err := svc.HandoverRevoke(ctx, m.MissionID, 1, "改派")
	if err != nil || r.PendingOwnerAgentID != "" || r.OwnerAgentID != "ag_a" {
		t.Fatalf("撤回: %+v %v", r, err)
	}
	evs, _, _ = svc.MissionEvents(ctx, m.MissionID)
	if evs[len(evs)-1].EventType != EventHandoverRevoked {
		t.Fatalf("撤回事件失真: %s", evs[len(evs)-1].EventType)
	}
	if _, err := svc.HandoverDecline(ctx, m.MissionID, 1, strings.Repeat("字", 281)); !errors.Is(err, ErrInvalidMission) {
		t.Fatalf("reason 超限: %v", err)
	}
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, strings.Repeat("字", 281)); !errors.Is(err, ErrInvalidMission) {
		t.Fatalf("note 超限: %v", err)
	}
}

// TestHandoverAcceptGuards 承接前置：CAS 冲突、承接人 offer 后转 suspended
// 拒绝、撤回后旧 accept 拒绝（无在途）。
func TestHandoverAcceptGuards(t *testing.T) {
	statuses := map[string]string{}
	svc, _ := newSvc(t, fakeAgents{
		ids:      map[string]bool{"ag_a": true, "ag_b": true},
		statuses: statuses,
	})
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")
	svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")

	if _, err := svc.HandoverAccept(ctx, m.MissionID, 9, ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("CAS 冲突: %v", err)
	}
	// offer 后 B 转 suspended：accept 复核拒绝
	statuses["ag_b"] = "suspended"
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("承接人非 active 须拒绝: %v", err)
	}
	// 撤回后旧 accept 拒（无在途）
	statuses["ag_b"] = "active"
	svc.HandoverRevoke(ctx, m.MissionID, 1, "改派")
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("撤回后旧 accept 须拒绝: %v", err)
	}
}

// TestHandoverAgentStatusPortMissing 身份端口未装配：发起/承接宁拒不放行。
func TestHandoverAgentStatusPortMissing(t *testing.T) {
	svc, _ := newSvc(t, nil)
	ctx := context.Background()
	m, err := svc.Create(ctx, "", fullContent(), nil) // 无 owner 创建可用
	if err != nil {
		t.Fatalf("建: %v", err)
	}
	if _, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", ""); !errors.Is(err, ErrAgentStatusUnavailable) {
		t.Fatalf("未装配状态端口须拒绝发起: %v", err)
	}
}
