package mission

// 0.7.9 / ADR-0046：Slot 换 Agent 交接的单任务转移面——ReassignForSlot
// （归属锚 + 状态族单源 + 无在途 + 承接人 active + owner 翻转不递增版本 +
// ready/running/waiting→paused + handover_accepted 事件 cause=slot_handover）
// 与 MissionsOfOwnerSlot 过滤口径。测试先行（红→绿）。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// slotMissionRow 直插行（挂实例归属——绕过建单服务面，专注转移语义）。
func slotMissionRow(id, owner, slotID string) Mission {
	return Mission{
		MissionID: id, ContractVersion: 1, Status: StatusReady, OwnerAgentID: owner,
		SlotInstanceID: slotID, Requester: RequesterOwner,
		Content:   fullContent(),
		CreatedAt: fixedNow(),
		UpdatedAt: fixedNow(),
	}
}

// TestReassignStatusMatrix 状态族矩阵：可转集（draft/ready/running/waiting/
// paused）逐态断言去向与事件；reviewing/canceling 拒。
func TestReassignStatusMatrix(t *testing.T) {
	cases := []struct {
		from     string
		ok       bool
		post     string
		statusEv bool // 是否伴随 status_changed（reason=slot_handover）
	}{
		{StatusReady, true, StatusPaused, true},
		{StatusRunning, true, StatusPaused, true},
		{StatusWaiting, true, StatusPaused, true},
		{StatusPaused, true, StatusPaused, false},
		{StatusDraft, true, StatusDraft, false},
		{StatusReviewing, false, StatusReviewing, false},
		{StatusCanceling, false, StatusCanceling, false},
	}
	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			agents := fakeAgents{ids: map[string]bool{"ag_a": true, "ag_b": true}}
			svc, mem := newSvc(t, agents)
			ctx := context.Background()
			row := slotMissionRow("mi_re_"+tc.from, "ag_a", "si_x")
			row.Status = tc.from
			// draft 须经不完备内容（fullContent 完备会 ready——直接以行落库，
			// 状态由调用方口径给定；完备性与转移面无关）。
			if _, err := mem.CreateMission(ctx, row, nil); err != nil {
				t.Fatal(err)
			}
			out, ok, err := svc.ReassignForSlot(ctx, row.MissionID, "ag_a", "ag_b")
			if err != nil {
				t.Fatalf("转移: %v", err)
			}
			if ok != tc.ok {
				t.Fatalf("ok 期望 %v: %+v", tc.ok, out)
			}
			if !tc.ok {
				got, _, _ := mem.MissionByID(ctx, row.MissionID)
				if got.OwnerAgentID != "ag_a" || got.Status != tc.from {
					t.Fatalf("拒转不得动物: %+v", got)
				}
				return
			}
			if out.OwnerAgentID != "ag_b" || out.Status != tc.post {
				t.Fatalf("转移后: %+v", out)
			}
			if out.ContractVersion != 1 {
				t.Fatalf("归属翻转不递增版本: %+v", out)
			}
			evs := mustEvents(t, mem, row.MissionID)
			var accepted map[string]any
			hasStatusEv := false
			for _, e := range evs {
				switch e.EventType {
				case EventHandoverAccepted:
					_ = json.Unmarshal(e.Payload, &accepted)
				case EventStatusChanged:
					var p map[string]any
					_ = json.Unmarshal(e.Payload, &p)
					if p["reason"] == "slot_handover" {
						hasStatusEv = true
					}
				}
			}
			if accepted == nil {
				t.Fatalf("缺 handover_accepted 事件: %+v", evs)
			}
			if accepted["cause"] != "slot_handover" || accepted["from_owner"] != "ag_a" ||
				accepted["to_owner"] != "ag_b" || accepted["slot_instance_id"] != "si_x" {
				t.Fatalf("承接回执事件体: %+v", accepted)
			}
			if accepted["status_at_accept"] != tc.from {
				t.Fatalf("status_at_accept 应记转移前态: %+v", accepted)
			}
			if hasStatusEv != tc.statusEv {
				t.Fatalf("status_changed 期望 %v", tc.statusEv)
			}
		})
	}
}

// TestReassignGuards 锚与在途守卫：归属锚不符 / 在途交接 / 承接人非 active /
// 终态（accepted）拒。
func TestReassignGuards(t *testing.T) {
	agents := fakeAgents{ids: map[string]bool{"ag_a": true, "ag_b": true, "ag_c": true}}
	svc, mem := newSvc(t, agents)
	ctx := context.Background()

	if _, err := mem.CreateMission(ctx, slotMissionRow("mi_g1", "ag_a", "si_x"), nil); err != nil {
		t.Fatal(err)
	}
	// 归属锚不符：不动物、不落事件。
	if _, ok, err := svc.ReassignForSlot(ctx, "mi_g1", "ag_z", "ag_b"); err != nil || ok {
		t.Fatalf("锚不符须 ok=false: ok=%v %v", ok, err)
	}
	got, _, _ := mem.MissionByID(ctx, "mi_g1")
	if got.OwnerAgentID != "ag_a" {
		t.Fatalf("锚不符动物: %+v", got)
	}

	// 在途交接（0.6.12 offer）：换将不得静默吞承诺。
	if _, err := svc.HandoverOffer(ctx, "mi_g1", 1, "ag_c", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.ReassignForSlot(ctx, "mi_g1", "ag_a", "ag_b"); err != nil || ok {
		t.Fatalf("在途交接须 ok=false: ok=%v %v", ok, err)
	}
	if _, err := svc.HandoverRevoke(ctx, "mi_g1", 1, mustOfferID(t, mem, "mi_g1"), ""); err != nil {
		t.Fatal(err)
	}

	// 承接人非 active：可见拒绝（ErrInvalidTransition）。
	suspended := fakeAgents{ids: map[string]bool{"ag_a": true, "ag_b": true}, statuses: map[string]string{"ag_b": "suspended"}}
	mem2 := NewMemStore()
	svc2 := New(mem2, WithNow(fixedNow), WithAgentLookup(suspended))
	if _, err := mem2.CreateMission(ctx, slotMissionRow("mi_g2", "ag_a", "si_x"), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc2.ReassignForSlot(ctx, "mi_g2", "ag_a", "ag_b"); !errors.Is(err, ErrInvalidTransition) || ok {
		t.Fatalf("承接人非 active 须 ErrInvalidTransition: ok=%v %v", ok, err)
	}
	got2, _, _ := mem2.MissionByID(ctx, "mi_g2")
	if got2.OwnerAgentID != "ag_a" {
		t.Fatalf("拒转动物: %+v", got2)
	}

	// 终态拒：直接以 accepted 行落库。
	acc := slotMissionRow("mi_g3", "ag_a", "si_x")
	acc.Status = StatusAccepted
	if _, err := mem.CreateMission(ctx, acc, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.ReassignForSlot(ctx, "mi_g3", "ag_a", "ag_b"); err != nil || ok {
		t.Fatalf("终态须 ok=false: ok=%v %v", ok, err)
	}
}

// mustOfferID 从事件流取在途发起标识。
func mustOfferID(t *testing.T, mem *MemStore, missionID string) string {
	t.Helper()
	evs := mustEvents(t, mem, missionID)
	var p map[string]any
	for _, e := range evs {
		if e.EventType == EventHandoverOffered {
			_ = json.Unmarshal(e.Payload, &p)
			return p["offer_id"].(string)
		}
	}
	t.Fatal("无 offer 事件")
	return ""
}

// TestMissionsOfSlot 全属收集口径（审校 #3——不论归属）：挂本实例非终态全在列
// （含他人归属——遗留转移检测依据）；他实例/终态排除。
func TestMissionsOfSlot(t *testing.T) {
	agents := fakeAgents{ids: map[string]bool{"ag_a": true}}
	svc, mem := newSvc(t, agents)
	ctx := context.Background()
	rows := []Mission{
		slotMissionRow("mi_s1", "ag_a", "si_x"),
		slotMissionRow("mi_s2", "ag_a", "si_x"), // reviewing——非终态、在列（编排分类）
		slotMissionRow("mi_s3", "ag_b", "si_x"), // 他人归属——也在列（全属收集）
		slotMissionRow("mi_s4", "ag_a", "si_y"), // 他实例
		slotMissionRow("mi_s5", "ag_a", "si_x"), // 终态
	}
	rows[1].Status = StatusReviewing
	rows[4].Status = StatusAccepted
	for _, r := range rows {
		if _, err := mem.CreateMission(ctx, r, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.ListOfSlot(ctx, "si_x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].MissionID != "mi_s1" || got[1].MissionID != "mi_s2" || got[2].MissionID != "mi_s3" {
		t.Fatalf("全属收集口径: %+v", got)
	}
}
