//go:build it

// IT 层（ADR-0046 / 0.7.9）：Slot 换 Agent 交接的存储事务面经真实 SQLite——
// ReassignMission 单事务（归属锚/状态族/无在途/承接人 active 事务内核对 +
// owner 翻转 + paused 确保与事件）、MissionsOfOwnerSlot 过滤查询、
// slot.Store.AppendBindingEvent 落流。零 migration（v35 链不变）。
package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

// TestReassignMissionIT 单任务转移事务：全过才写；核对面逐项拒。
func TestReassignMissionIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	for _, a := range []personalagent.Agent{
		{AgentID: "ag_a", DisplayName: "甲", Status: "active", Source: "manual", CreatedAt: "2026-11-12T10:00:00Z"},
		{AgentID: "ag_b", DisplayName: "乙", Status: "active", Source: "manual", CreatedAt: "2026-11-12T10:00:01Z"},
		{AgentID: "ag_ret", DisplayName: "退", Status: "retired", Source: "manual", CreatedAt: "2026-11-12T10:00:02Z"},
	} {
		if _, err := store.EnsureAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	row := missionRow(mission.StatusReady, "ag_a")
	row.SlotInstanceID = "si_it_1"
	m, err := store.CreateMission(ctx, row, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := func(payload map[string]any) mission.Event {
		raw, _ := json.Marshal(payload)
		return mission.Event{EventType: mission.EventHandoverAccepted, Payload: raw, OccurredAt: "2026-11-12T12:00:00Z"}
	}

	// 归属锚不符：拒。
	if _, ok, err := store.ReassignMission(ctx, m.MissionID, "ag_z", "ag_b",
		ev(map[string]any{})); err != nil || ok {
		t.Fatalf("锚不符须 ok=false: ok=%v %v", ok, err)
	}
	// 承接人 retired（事务内核对直查 agents）：拒。
	if _, ok, err := store.ReassignMission(ctx, m.MissionID, "ag_a", "ag_ret",
		ev(map[string]any{})); err != nil || ok {
		t.Fatalf("承接人非 active 须 ok=false: ok=%v %v", ok, err)
	}

	// 全过：owner 翻转 + ready→paused + 两事件（承接回执 + status_changed）。
	out, ok, err := store.ReassignMission(ctx, m.MissionID, "ag_a", "ag_b",
		ev(map[string]any{"cause": "slot_handover"}))
	if err != nil || !ok {
		t.Fatalf("转移: ok=%v %v", ok, err)
	}
	if out.OwnerAgentID != "ag_b" || out.Status != mission.StatusPaused || out.ContractVersion != m.ContractVersion {
		t.Fatalf("转移后: %+v", out)
	}
	evs, _, _ := store.MissionEvents(ctx, m.MissionID)
	hasAccept, hasPause := false, false
	for _, e := range evs {
		if e.EventType == mission.EventHandoverAccepted && strings.Contains(string(e.Payload), "slot_handover") {
			hasAccept = true
		}
		if e.EventType == mission.EventStatusChanged && strings.Contains(string(e.Payload), "slot_handover") {
			hasPause = true
		}
	}
	if !hasAccept || !hasPause {
		t.Fatalf("事件: accept=%v pause=%v (%+v)", hasAccept, hasPause, evs)
	}

	// 已转（owner=ag_b）再以旧锚转移：锚不符拒（重跑收敛的存储面依据）。
	if _, ok, err := store.ReassignMission(ctx, m.MissionID, "ag_a", "ag_b",
		ev(map[string]any{})); err != nil || ok {
		t.Fatalf("重放锚不符须 ok=false: ok=%v %v", ok, err)
	}

	// 在途交接拒：offer 后转移不可静默吞承诺。
	row2 := missionRow(mission.StatusReady, "ag_a")
	row2.MissionID = "mi_it_2"
	row2.SlotInstanceID = "si_it_1"
	m2, err := store.CreateMission(ctx, row2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.HandoverOffer(ctx, m2.MissionID, 1, "ag_c", "hdo_re_1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{}, "t")); !ok {
		t.Fatal("offer 须成功（前置）")
	}
	if _, ok, err := store.ReassignMission(ctx, m2.MissionID, "ag_a", "ag_b",
		ev(map[string]any{})); err != nil || ok {
		t.Fatalf("在途交接须 ok=false: ok=%v %v", ok, err)
	}
}

// TestMissionsOfSlotIT 全属收集查询（审校 #3）：slot 过滤、非终态全量（不论
// 归属——他人归属遗留也在列），终态排除。
func TestMissionsOfSlotIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rows := []mission.Mission{
		missionRow(mission.StatusReady, "ag_a"),
		missionRow(mission.StatusCanceling, "ag_a"),
		missionRow(mission.StatusAccepted, "ag_a"),
		missionRow(mission.StatusReady, "ag_b"),
	}
	rows[0].MissionID, rows[0].SlotInstanceID = "mi_o1", "si_o"
	rows[1].MissionID, rows[1].SlotInstanceID = "mi_o2", "si_o"
	rows[2].MissionID, rows[2].SlotInstanceID = "mi_o3", "si_o"
	rows[3].MissionID, rows[3].SlotInstanceID = "mi_o4", "si_o"
	for _, r := range rows {
		if _, err := store.CreateMission(ctx, r, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.MissionsOfSlot(ctx, "si_o")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].MissionID != "mi_o1" || got[1].MissionID != "mi_o2" || got[2].MissionID != "mi_o4" {
		t.Fatalf("全属收集: %+v", got)
	}
	if got, _ := store.MissionsOfSlot(ctx, "si_none"); len(got) != 0 {
		t.Fatalf("他实例须空: %+v", got)
	}
}

// TestAppendBindingEventIT 承接回执事件落流：slot_handover 事件按 seq 追加，
// payload 原样；未知绑定拒。
func TestAppendBindingEventIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	b, err := store.CreateBinding(ctx, slot.Binding{
		BindingID: "bd_it_h1", AgentID: "ag_a", SlotInstanceID: "si_it_b", SlotSpecVersion: 1,
		Scope: "workspace", Status: slot.BindingReady, CreatedAt: "t", UpdatedAt: "t",
	}, slot.BindingReady, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"from_agent": "ag_a", "to_agent": "ag_b"})
	if err := store.AppendBindingEvent(ctx, b.BindingID, "slot_handover", payload, "2026-11-12T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	evs, err := store.BindingEvents(ctx, b.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) < 2 || evs[len(evs)-1].EventType != "slot_handover" ||
		!strings.Contains(string(evs[len(evs)-1].Payload), "ag_b") {
		t.Fatalf("事件流: %+v", evs)
	}
	if err := store.AppendBindingEvent(ctx, "bd_ghost", "slot_handover", payload, "t"); err == nil {
		t.Fatal("未知绑定须拒")
	}
}

// TestHandoverDrainingIntakeGateIT 审校 #4（P1）存储面：排空边界（active →
// draining）后，slot 寻址建单门（0.6.13 四核——事务内要求 active 任职）拒新单
// ——"停接新任务"结构性成立。
func TestHandoverDrainingIntakeGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{
		AgentID: "ag_gate", DisplayName: "任职者", Status: "active", Source: "manual", CreatedAt: "2026-11-12T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.gate", Version: 1, SchemaVersion: slot.SchemaVersion,
		SpecContent: slot.SpecContent{Name: "边界", Responsibility: "验证停接单", Deliverables: []string{"报告"}},
		CreatedAt:   "t",
	}); err != nil && !errors.Is(err, slot.ErrSpecExists) {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{
		SlotInstanceID: "si_gate", SlotSpecID: "slot.gate", Scope: "workspace", Title: "边界",
		KnowledgeNamespace: "slotns_gate", CreatedFromVersion: 1, CreatedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{
		BindingID: "bd_gate", AgentID: "ag_gate", SlotInstanceID: "si_gate", SlotSpecVersion: 1,
		Scope: "workspace", CreatedAt: "t", UpdatedAt: "t",
	}, slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	row := missionRow(mission.StatusReady, "ag_gate")
	row.MissionID = "mi_gate"
	intent := &mission.SlotIntent{InstanceID: "si_gate", AgentID: "ag_gate", SpecID: "slot.gate"}

	// 控制组：active 任职建单放行。
	if _, err := store.CreateMissionGated(ctx, row, nil, &mission.CreationGate{Slot: intent}); err != nil {
		t.Fatalf("active 建单应放行: %v", err)
	}
	// 排空边界：active → draining。
	if _, ok, err := store.TransitionBinding(ctx, "bd_gate", []string{slot.BindingActive},
		slot.BindingDraining, nil, "t2"); err != nil || !ok {
		t.Fatalf("排空边界: ok=%v %v", ok, err)
	}
	// 边界后建单门拒——停接新任务。
	row2 := missionRow(mission.StatusReady, "ag_gate")
	row2.MissionID = "mi_gate2"
	if _, err := store.CreateMissionGated(ctx, row2, nil, &mission.CreationGate{Slot: intent}); !errors.Is(err, mission.ErrSlotIntentViolated) {
		t.Fatalf("draining 须拒新单（停接边界）: %v", err)
	}
}

// TestActivateBindingTxIT 三审 P1：激活写事务——状态转移与附加事件同一事务
// （成功同现；状态不符两者皆无）；**同实例 draining 守卫在事务内**（绕行拒）。
func TestActivateBindingTxIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.CreateBinding(ctx, slot.Binding{
		BindingID: "bd_tx", AgentID: "ag_tx", SlotInstanceID: "si_tx", SlotSpecVersion: 1,
		Scope: "workspace", CreatedAt: "t", UpdatedAt: "t",
	}, slot.BindingReady, nil); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"from_agent":"ag_x","to_agent":"ag_tx"}`)
	from, ok, err := store.ActivateBindingTx(ctx, "bd_tx", slot.BindingReady, "slot_handover", payload, "t2")
	if err != nil || !ok || from != slot.BindingReady {
		t.Fatalf("激活事务: from=%s ok=%v %v", from, ok, err)
	}
	evs, err := store.BindingEvents(ctx, "bd_tx")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 || evs[3].EventType != "slot_handover" || !strings.Contains(string(evs[3].Payload), "ag_tx") {
		t.Fatalf("事件（created + preparing→ready + ready→active + slot_handover）: %+v", evs)
	}
	// 状态不符：转移与事件皆无（原子未遂）。
	if _, ok, err := store.ActivateBindingTx(ctx, "bd_tx", slot.BindingReady, "slot_handover", payload, "t3"); err != nil || ok {
		t.Fatalf("非 ready 须 ok=false: ok=%v %v", ok, err)
	}
	// draining 守卫（事务内）：同实例存在 draining → ErrInvalidTransition。
	if _, err := store.CreateBinding(ctx, slot.Binding{
		BindingID: "bd_tx2", AgentID: "ag_tx2", SlotInstanceID: "si_tx", SlotSpecVersion: 1,
		Scope: "workspace", CreatedAt: "t3", UpdatedAt: "t3",
	}, slot.BindingReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{
		SlotInstanceID: "si_tx", SlotSpecID: "slot.tx", Scope: "workspace", Title: "tx",
		KnowledgeNamespace: "slotns_tx", CreatedFromVersion: 1, CreatedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	// 先释放 bd_tx（腾出 active），再置 draining，再激活 bd_tx2 → 拒。
	if _, _, err := store.ReleaseBinding(ctx, "bd_tx", "腾位", "t4"); err != nil {
		t.Fatal(err)
	}
	if _, ok2, err := store.TransitionBinding(ctx, "bd_tx", []string{slot.BindingReleased}, slot.BindingDraining, nil, "t5"); err != nil || !ok2 {
		t.Fatalf("置 draining: ok=%v %v", ok2, err)
	}
	if _, ok3, err := store.ActivateBindingTx(ctx, "bd_tx2", slot.BindingReady, "", nil, "t6"); err == nil || ok3 || !errors.Is(err, slot.ErrInvalidTransition) {
		t.Fatalf("draining 守卫须拒: ok=%v %v", ok3, err)
	}
}

// TestSlotHandoverTransfersIT 三审 P2：换将转移事实枚举（持久事件源）——转移
// 后任务置**终态**仍被枚举（不依赖在办/归属）。
func TestSlotHandoverTransfersIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{
		AgentID: "ag_tr", DisplayName: "受", Status: "active", Source: "manual", CreatedAt: "2026-11-12T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	row := missionRow(mission.StatusReady, "ag_a")
	row.SlotInstanceID = "si_tr"
	m, err := store.CreateMission(ctx, row, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ReassignMission(ctx, m.MissionID, "ag_a", "ag_tr",
		handoverEvent(mission.EventHandoverAccepted, map[string]any{
			"cause": mission.SlotHandoverCause, "from_owner": "ag_a", "to_owner": "ag_tr",
		}, "t")); err != nil || !ok {
		t.Fatalf("转移: ok=%v %v", ok, err)
	}
	got, err := store.SlotHandoverTransfers(ctx, "si_tr", "ag_tr")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MissionID != m.MissionID || got[0].FromOwner != "ag_a" {
		t.Fatalf("枚举（含当时转出人）: %+v", got)
	}
	// 终态后仍在列（持久事件源——补偿不依赖在办）。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE missions SET status = 'canceled' WHERE mission_id = ?`, m.MissionID); err != nil {
		t.Fatal(err)
	}
	got2, _ := store.SlotHandoverTransfers(ctx, "si_tr", "ag_tr")
	if len(got2) != 1 || got2[0].MissionID != m.MissionID {
		t.Fatalf("终态后仍在列: %+v", got2)
	}
	// 他 owner / 他实例不误列。
	if g, _ := store.SlotHandoverTransfers(ctx, "si_tr", "ag_other"); len(g) != 0 {
		t.Fatalf("他 owner 须空: %+v", g)
	}
	if g, _ := store.SlotHandoverTransfers(ctx, "si_none", "ag_tr"); len(g) != 0 {
		t.Fatalf("他实例须空: %+v", g)
	}
}
