package mission

// 0.6.11 / ADR-0034：reopen_requested 事件——CAS 版本（任意状态可发起，不改
// 状态不翻转终态）；payload 事实随事件留痕（request_id / decision_ref / 证据 /
// 受影响项 / reason / 实际契约版本）。

import (
	"context"
	"encoding/json"
	"errors"

	"testing"
)

func TestReopenRequestEvent(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m := reviewable(t, svc)

	out, err := svc.ReopenRequest(ctx, m.MissionID, 1,
		"rop_t1", "closure:room_a:seq42", []string{"doc:d1:r3", "room:a:seq9"}, []string{"mi_other"}, "统计口径有误")
	if err != nil {
		t.Fatalf("提交重开请求: %v", err)
	}
	// 不改状态、不翻版本：事实留痕、Mission 照旧
	if out.Status != StatusReady || out.ContractVersion != 1 {
		t.Fatalf("请求不改 Mission: %+v", out)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	var hit bool
	for _, e := range evs {
		if e.EventType != EventReopenRequested {
			continue
		}
		hit = true
		var p map[string]any
		if json.Unmarshal(e.Payload, &p) != nil {
			t.Fatalf("载荷解析: %s", e.Payload)
		}
		if p["request_id"] != "rop_t1" || p["decision_ref"] != "closure:room_a:seq42" ||
			p["contract_version"] != float64(1) || p["reason"] != "统计口径有误" {
			t.Fatalf("载荷事实: %v", p)
		}
		if ev, ok := p["evidence_refs"].([]any); !ok || len(ev) != 2 {
			t.Fatalf("证据引用: %v", p["evidence_refs"])
		}
		if af, ok := p["affected_mission_ids"].([]any); !ok || len(af) != 1 || af[0] != "mi_other" {
			t.Fatalf("受影响项: %v", p["affected_mission_ids"])
		}
	}
	if !hit {
		t.Fatal("缺 reopen_requested 事件")
	}

	// 版本不符 → 冲突（任何状态可发起，CAS 只核版本）
	if _, err := svc.ReopenRequest(ctx, m.MissionID, 9, "rop_t2", "d", []string{"e"}, nil, "r"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本不符应冲突: %v", err)
	}
	// 终态（accepted）同样可发起——不翻转
	r1, _ := svc.RequestReview(ctx, m.MissionID, 1, "dlv_r")
	_ = r1
	if _, err := svc.RecordReview(ctx, m.MissionID, 1, 1, ReviewDecision{Decision: ReviewAccepted}); err != nil {
		t.Fatalf("接受: %v", err)
	}
	out2, err := svc.ReopenRequest(ctx, m.MissionID, 1, "rop_t3", "d", []string{"e"}, nil, "新证据")
	if err != nil || out2.Status != StatusAccepted {
		t.Fatalf("终态可发起且不翻转: %+v %v", out2, err)
	}
	// 形状校验
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "e"
	}
	for name, fn := range map[string]func() error{
		"空决定引用": func() error {
			_, e := svc.ReopenRequest(ctx, m.MissionID, 1, "rop_x", "  ", []string{"e"}, nil, "")
			return e
		},
		"空证据":  func() error { _, e := svc.ReopenRequest(ctx, m.MissionID, 1, "rop_x", "d", nil, nil, ""); return e },
		"证据超限": func() error { _, e := svc.ReopenRequest(ctx, m.MissionID, 1, "rop_x", "d", nine, nil, ""); return e },
	} {
		if err := fn(); !errors.Is(err, ErrInvalidMission) {
			t.Fatalf("%s 应拒: %v", name, err)
		}
	}
}
