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
	if err != nil || out.PendingOwnerAgentID != "ag_b" || out.OwnerAgentID != "ag_a" || out.PendingOfferID == "" {
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
		!strings.Contains(string(last.Payload), `"contract_version":1`) ||
		!strings.Contains(string(last.Payload), `"offer_id":"`) {
		t.Fatalf("发起事件失真: %s %s", last.EventType, last.Payload)
	}

	acc, err := svc.HandoverAccept(ctx, m.MissionID, 1, out.PendingOfferID, "已接手")
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
		hf[1].FromOwnerAgentID != "ag_a" || hf[1].ToOwnerAgentID != "ag_b" ||
		hf[0].OfferID == "" || hf[1].OfferID != hf[0].OfferID {
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
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", "ag_a", fullContent()); !errors.Is(err, ErrInvalidTransition) {
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
	of1, err := svc.HandoverOffer(ctx, draft.MissionID, 1, "ag_b", "")
	if err != nil {
		t.Fatalf("发起: %v", err)
	}
	if got, _, _ := svc.Get(ctx, draft.MissionID); got.Status != StatusDraft || got.OwnerAgentID != "" {
		t.Fatalf("发起不改归属与状态: %+v", got)
	}
	acc, err := svc.HandoverAccept(ctx, draft.MissionID, 1, of1.PendingOfferID, "")
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
	of2, _ := svc.HandoverOffer(ctx, d2.MissionID, 1, "ag_b", "")
	acc2, err := svc.HandoverAccept(ctx, d2.MissionID, 1, of2.PendingOfferID, "")
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
		of, _ := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
		acc, err := svc.HandoverAccept(ctx, m.MissionID, 1, of.PendingOfferID, "")
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

	if _, err := svc.HandoverDecline(ctx, m.MissionID, 1, "hdo_x", "没理由"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("无在途婉拒须拒绝: %v", err)
	}
	if _, err := svc.HandoverRevoke(ctx, m.MissionID, 1, "hdo_x", "没理由"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("无在途撤回须拒绝: %v", err)
	}

	ofa, _ := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
	d, err := svc.HandoverDecline(ctx, m.MissionID, 1, ofa.PendingOfferID, "暂不接手")
	if err != nil || d.PendingOwnerAgentID != "" || d.OwnerAgentID != "ag_a" {
		t.Fatalf("婉拒: %+v %v", d, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	if evs[len(evs)-1].EventType != EventHandoverDeclined ||
		!strings.Contains(string(evs[len(evs)-1].Payload), `"reason":"暂不接手"`) {
		t.Fatalf("婉拒事件失真: %s", evs[len(evs)-1].Payload)
	}

	ofb, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_c", strings.Repeat("字", 1))
	if err != nil {
		t.Fatalf("婉拒后可再发起: %v", err)
	}
	r, err := svc.HandoverRevoke(ctx, m.MissionID, 1, ofb.PendingOfferID, "改派")
	if err != nil || r.PendingOwnerAgentID != "" || r.OwnerAgentID != "ag_a" {
		t.Fatalf("撤回: %+v %v", r, err)
	}
	evs, _, _ = svc.MissionEvents(ctx, m.MissionID)
	if evs[len(evs)-1].EventType != EventHandoverRevoked {
		t.Fatalf("撤回事件失真: %s", evs[len(evs)-1].EventType)
	}
	if _, err := svc.HandoverDecline(ctx, m.MissionID, 1, "hdo_x", strings.Repeat("字", 281)); !errors.Is(err, ErrInvalidMission) {
		t.Fatalf("reason 超限: %v", err)
	}
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, "hdo_x", strings.Repeat("字", 281)); !errors.Is(err, ErrInvalidMission) {
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
	ofg, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
	if err != nil {
		t.Fatalf("发起: %v", err)
	}

	if _, err := svc.HandoverAccept(ctx, m.MissionID, 9, ofg.PendingOfferID, ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("CAS 冲突: %v", err)
	}
	// offer 后 B 转 suspended：accept 复核拒绝（真实锚——确因身份而非锚拒）
	statuses["ag_b"] = "suspended"
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, ofg.PendingOfferID, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("承接人非 active 须拒绝: %v", err)
	}
	// 撤回后旧 accept 拒（无在途）
	statuses["ag_b"] = "active"
	if _, err := svc.HandoverRevoke(ctx, m.MissionID, 1, ofg.PendingOfferID, "改派"); err != nil {
		t.Fatalf("撤回: %v", err)
	}
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, ofg.PendingOfferID, ""); !errors.Is(err, ErrInvalidTransition) {
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

// TestHandoverOfferAnchor 审校 #1：承接/婉拒/撤回绑定所见发起——撤回改派后
// （contract_version 不递增）旧请求一律拒绝；空 offer_id 拒绝。
func TestHandoverOfferAnchor(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")
	out, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
	if err != nil || out.PendingOfferID == "" {
		t.Fatalf("发起: %+v %v", out, err)
	}
	first := out.PendingOfferID

	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, "", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("空 offer_id 须拒: %v", err)
	}
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, "hdo_bogus", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("陌生 offer_id 须拒: %v", err)
	}
	// 撤回（正确锚）→ 改派 ag_c → 旧 offer 的承接/婉拒均拒
	if _, err := svc.HandoverRevoke(ctx, m.MissionID, 1, first, "改派"); err != nil {
		t.Fatalf("撤回: %v", err)
	}
	out2, err := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_c", "")
	if err != nil {
		t.Fatalf("改派: %v", err)
	}
	if out2.PendingOfferID == first {
		t.Fatal("新发起不得复用旧 offer_id")
	}
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, first, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("旧承接须拒（不得处理新交接）: %v", err)
	}
	if _, err := svc.HandoverDecline(ctx, m.MissionID, 1, first, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("旧婉拒须拒: %v", err)
	}
	if _, err := svc.HandoverRevoke(ctx, m.MissionID, 1, first, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("旧撤回须拒: %v", err)
	}
	// 正确锚照常
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, out2.PendingOfferID, ""); err != nil {
		t.Fatalf("新锚承接: %v", err)
	}
	if got, _, _ := svc.Get(ctx, m.MissionID); got.OwnerAgentID != "ag_c" {
		t.Fatalf("承接翻转: %+v", got)
	}
}

// TestReviseOwnerAnchor 审校 #2：承接翻转 owner 后（版本不递增），携旧 owner 的
// 修订被 ErrOwnerMismatch 拒——不得无感知撤销责任转移；携新 owner 照常。
func TestReviseOwnerAnchor(t *testing.T) {
	svc, _ := newSvc(t, handoverAgents())
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "") // owner ag_a
	out, _ := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, out.PendingOfferID, ""); err != nil {
		t.Fatalf("承接: %v", err)
	}
	got, _, _ := svc.Get(ctx, m.MissionID)
	if got.OwnerAgentID != "ag_b" || got.ContractVersion != 1 {
		t.Fatalf("承接后: %+v", got)
	}
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", "ag_a", fullContent()); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("旧 owner 修订须拒: %v", err)
	}
	if _, err := svc.Revise(ctx, m.MissionID, 1, "", "ag_b", fullContent()); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("预期 unassigned 修订须拒: %v", err)
	}
	up, err := svc.Revise(ctx, m.MissionID, 1, "ag_b", "ag_b", fullContent())
	if err != nil || up.OwnerAgentID != "ag_b" || up.ContractVersion != 2 {
		t.Fatalf("新 owner 修订照常: %+v %v", up, err)
	}
}

// TestHandoverAcceptReceiptStatus 审校 #4：回执记**承接时实际状态**（MemStore
// 临界区内注入）——offer 后并发暂停（模拟服务层陈旧读），承接回执须记 paused。
func TestHandoverAcceptReceiptStatus(t *testing.T) {
	svc, mem := newSvc(t, handoverAgents())
	ctx := context.Background()
	m := readyWithReviewer(t, svc, "")
	out, _ := svc.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "")
	// 服务层读后并发暂停（模拟：直接改 mem 状态——服务层下次读见 paused）
	if _, ok, _ := mem.PauseMission(ctx, m.MissionID, 1, "排空", "t"); !ok {
		t.Fatal("并发暂停前置失败")
	}
	if _, err := svc.HandoverAccept(ctx, m.MissionID, 1, out.PendingOfferID, ""); err != nil {
		t.Fatalf("paused 承接: %v", err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	var receipt string
	for _, e := range evs {
		if e.EventType == EventHandoverAccepted {
			receipt = string(e.Payload)
		}
	}
	if !strings.Contains(receipt, `"status_at_accept":"paused"`) {
		t.Fatalf("回执须记实际 paused: %s", receipt)
	}
}

// TestHandoverFactsInjectedStoreLevel 复审 #2：事件事实由临界区内现值生成——
// store 级直调（payload 预填陈旧 from_owner），注入后与行内 owner 一致；draft
// 重判按临界区 content（并发改 status 模拟：paused 行直接置 draft，承接按现行
// content 重判 ready，不被事务前快照放回 paused）。
func TestHandoverFactsInjectedStoreLevel(t *testing.T) {
	mem := NewMemStore()
	ctx := context.Background()
	svc := New(mem, WithNow(fixedNow), WithNewMissionID(func() string { return "mi_fi" }),
		WithAgentLookup(handoverAgents()))
	m, err := svc.Create(ctx, "ag_a", fullContent(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 陈旧 from_owner 预填（模拟服务层读后并发翻转）——MemStore 临界区内注入
	ev := Event{EventType: EventHandoverOffered, OccurredAt: fixedNow()}
	ev.Payload = []byte(`{"from_owner":"ag_stale","to_owner":"ag_b","offer_id":"hdo_fi1","contract_version":1}`)
	if out, ok, err := mem.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "hdo_fi1", ev); err != nil || !ok || out.PendingOfferID != "hdo_fi1" {
		t.Fatalf("offer: ok=%v %v", ok, err)
	}
	evs, _, _ := mem.MissionEvents(ctx, m.MissionID)
	var got string
	for _, e := range evs {
		if e.EventType == EventHandoverOffered {
			got = string(e.Payload)
		}
	}
	if !strings.Contains(got, `"from_owner":"ag_a"`) || strings.Contains(got, "ag_stale") {
		t.Fatalf("offer 事件须注入临界区内 owner: %s", got)
	}

	// 并发把 paused 带到 draft（模拟 paused→resume 重判不完备外的路径：此处直接
	// 构造行状态交错）——承接按临界区 content + 新 owner 重判，完备 → ready
	wp := `{"offer_id":"hdo_fi1"}`
	if _, ok, err := mem.HandoverWithdraw(ctx, m.MissionID, 1, "hdo_fi1", Event{EventType: EventHandoverRevoked, Payload: []byte(wp), OccurredAt: fixedNow()}); err != nil || !ok {
		t.Fatalf("withdraw: %v %v", ok, err)
	}
	mem.missions[0].Status = StatusPaused
	if _, ok, err := mem.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "hdo_fi2", Event{EventType: EventHandoverOffered, Payload: []byte(`{"offer_id":"hdo_fi2"}`), OccurredAt: fixedNow()}); err != nil || !ok {
		t.Fatalf("offer2: %v %v", ok, err)
	}
	mem.missions[0].Status = StatusDraft // 并发交错：临界区前一刻状态翻 draft
	acc, ok, err := mem.HandoverAccept(ctx, m.MissionID, 1, "hdo_fi2", Event{EventType: EventHandoverAccepted, Payload: []byte(`{"status_at_accept":"draft"}`), OccurredAt: fixedNow()})
	if err != nil || !ok {
		t.Fatalf("accept: %v %v", ok, err)
	}
	if acc.Status != StatusReady || acc.OwnerAgentID != "ag_b" {
		t.Fatalf("draft 重判须按临界区 content → ready（不得放回 paused/陈旧 draft）: %+v", acc)
	}
}
