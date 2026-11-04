//go:build it

// IT 层（0.6.13 审校修复）：v31 迁移（派发意图/解析事实/回执说明）、
// UpdateMissionAtomic 原子续接（#3/#11/#12/#13/#16——拒绝回滚不占键、重放同
// ID、reason 持久化）、CreateMissionSlot 建单事务四核（#2/#7）、期限时刻比较
// （#15 SQLite 面）。
package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/dispatch"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

// TestDispatchFixV31ColumnsIT v31 迁移登记 + 三表新列（旧库升级面由 v30 IT 同
// 法覆盖——此处核结构）。
func TestDispatchFixV31ColumnsIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	var v31 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 31`).Scan(&v31)
	if v31 != 1 {
		t.Fatalf("migration v31 未登记: %d", v31)
	}
	has := func(table, col string) bool {
		var n int
		store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n)
		return n == 1
	}
	for _, tc := range [][2]string{
		{"missions", "slot_instance_id"},
		{"dispatch_decisions", "resolved_agent_id"},
		{"dispatch_decisions", "resolved_instance_id"},
		{"dispatch_receipts", "reason"},
	} {
		if !has(tc[0], tc[1]) {
			t.Fatalf("v31 列缺失: %s.%s", tc[0], tc[1])
		}
	}
}

func fixContent(obj string) mission.Content {
	return mission.Content{
		Objective: obj, Scope: "s", AcceptanceCriteria: []string{"a"},
		DeliverableSpecs: []string{"d"}, SourceRefs: []string{"r:1"},
	}
}

// TestUpdateMissionAtomicIT #3/#11/#12/#13/#16：同一写事务——修订与回执同提交
// （回执失败即整体回滚的结构保证）；领域拒绝/身份门失败回滚认领不占键；重放
// 与重驱动同 receipt_id；reason 持久化；target_refs 携修订后版本。
func TestUpdateMissionAtomicIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{AgentID: "ag_atm", DisplayName: "续接甲",
		Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_atm"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c2 := fixContent("原子续接载荷")
	cmd := dispatch.UpdateCmd{MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-atm", RequestID: "req_atm", Contract: c2, Reason: "任务卡追加"}

	// 首次：修订 + 回执同事务提交；reason/版本引用持久化
	rc, replayed, err := store.UpdateMissionAtomic(ctx, cmd, "dg_atm", "2026-11-03T12:00:05Z", "dpr_atm1")
	if err != nil || replayed {
		t.Fatalf("首次: %+v replayed=%v %v", rc, replayed, err)
	}
	if rc.DomainCommandID != m.MissionID+":revise:v2" || rc.Reason != "任务卡追加" ||
		len(rc.TargetRefs) != 1 || rc.TargetRefs[0] != "mission:"+m.MissionID+":v2" {
		t.Fatalf("回执失真: %+v", rc)
	}
	got, _, _ := store.MissionByID(ctx, m.MissionID)
	if got.ContractVersion != 2 || got.Objective != "原子续接载荷" {
		t.Fatalf("修订事实: %+v", got)
	}

	// 同键同摘要 → 重放原回执（同 receipt_id）
	rc2, replayed2, err := store.UpdateMissionAtomic(ctx, cmd, "dg_atm", "2026-11-03T12:00:06Z", "dpr_atm2")
	if err != nil || !replayed2 || rc2.ReceiptID != rc.ReceiptID {
		t.Fatalf("重放: %+v replayed=%v %v", rc2, replayed2, err)
	}
	// 同键异摘要 → 幂等冲突
	if _, _, err := store.UpdateMissionAtomic(ctx, dispatch.UpdateCmd{MissionID: m.MissionID,
		ExpectedVersion: 2, IdempotencyKey: "idem-atm", Contract: fixContent("异载荷")},
		"dg_other", "2026-11-03T12:00:07Z", "dpr_atm3"); !errors.Is(err, dispatch.ErrIdempotencyConflict) {
		t.Fatalf("异摘要须冲突: %v", err)
	}
	// 回执持久化可读回（reason 原文）
	persist, found, err := store.ReceiptByKey(ctx, dispatch.CallerOwner, dispatch.ScopeWorkspace, "idem-atm")
	if err != nil || !found || persist.Reason != "任务卡追加" || persist.ReceiptID != "dpr_atm1" {
		t.Fatalf("持久回执: found=%v %+v %v", found, persist, err)
	}

	// 领域拒绝（版本冲突）→ 整体回滚：不占键、不修订
	if _, _, err := store.UpdateMissionAtomic(ctx, dispatch.UpdateCmd{MissionID: m.MissionID,
		ExpectedVersion: 9, IdempotencyKey: "idem-atm-vc", Contract: fixContent("冲突载荷")},
		"dg_vc", "2026-11-03T12:00:08Z", "dpr_atm4"); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("版本冲突须透传: %v", err)
	}
	var nVC int
	store.db.QueryRow(`SELECT COUNT(*) FROM dispatch_receipts WHERE idempotency_key = 'idem-atm-vc'`).Scan(&nVC)
	if nVC != 0 {
		t.Fatalf("拒绝不得占键（回滚认领）: %d", nVC)
	}
	// 同键修正后重试成功
	if _, replayed, err := store.UpdateMissionAtomic(ctx, dispatch.UpdateCmd{MissionID: m.MissionID,
		ExpectedVersion: 2, IdempotencyKey: "idem-atm-vc", Contract: fixContent("修正载荷")},
		"dg_vc", "2026-11-03T12:00:09Z", "dpr_atm5"); err != nil || replayed {
		t.Fatalf("修正后同键重试: replayed=%v %v", replayed, err)
	}

	// 身份门（#12 事务内）：暂停 → 拒绝且不占键；恢复后同键成功
	if _, _, err := store.TransitionAgent(ctx, "ag_atm", []string{"active"}, "suspended", "审校复现", "2026-11-03T12:00:10Z"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateMissionAtomic(ctx, dispatch.UpdateCmd{MissionID: m.MissionID,
		ExpectedVersion: 3, IdempotencyKey: "idem-atm-susp", Contract: fixContent("暂停期载荷")},
		"dg_susp", "2026-11-03T12:00:11Z", "dpr_atm6"); !errors.Is(err, dispatch.ErrTargetNotActive) {
		t.Fatalf("暂停身份须拒: %v", err)
	}
	var nS int
	store.db.QueryRow(`SELECT COUNT(*) FROM dispatch_receipts WHERE idempotency_key = 'idem-atm-susp'`).Scan(&nS)
	if nS != 0 {
		t.Fatalf("身份门拒绝不得占键: %d", nS)
	}
	if _, _, err := store.TransitionAgent(ctx, "ag_atm", []string{"suspended"}, "active", "恢复", "2026-11-03T12:00:12Z"); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := store.UpdateMissionAtomic(ctx, dispatch.UpdateCmd{MissionID: m.MissionID,
		ExpectedVersion: 3, IdempotencyKey: "idem-atm-susp", Contract: fixContent("恢复后载荷")},
		"dg_susp", "2026-11-03T12:00:13Z", "dpr_atm7"); err != nil || replayed {
		t.Fatalf("恢复后同键重试: replayed=%v %v", replayed, err)
	}

	// 未指派负责人 → 续接资格拒绝（#12）
	row2 := missionRow(mission.StatusReady, "")
	row2.MissionID = "mi_atm_un"
	m2, err := store.CreateMission(ctx, row2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateMissionAtomic(ctx, dispatch.UpdateCmd{MissionID: m2.MissionID,
		ExpectedVersion: 1, IdempotencyKey: "idem-atm-un", Contract: fixContent("无主载荷")},
		"dg_un", "2026-11-03T12:00:14Z", "dpr_atm8"); !errors.Is(err, dispatch.ErrTargetNotActive) {
		t.Fatalf("未指派须拒: %v", err)
	}
}

// slotFixSeed 建职责链：模板（要求 net 工具）→ 实例 → active 任职（钉 v1）→
// 授权档案（满足）。返回实例 ID。
func slotFixSeed(t *testing.T, store *Store, agentID string) string {
	t.Helper()
	ctx := context.Background()
	spec := slot.SlotSpec{SlotSpecID: "slot.fix1", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "执行", Responsibility: "执行职责",
			Deliverables: []string{"产出"}, Tools: &slot.CapabilityList{Required: []string{"net"}}},
		CreatedAt: "2026-11-03T10:00:00Z"}
	if _, err := store.CreateSpec(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{SlotInstanceID: "si_fix",
		SlotSpecID: "slot.fix1", Scope: "workspace", Title: "执行",
		KnowledgeNamespace: "kn_fix", CreatedFromVersion: 1, CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_fix1", AgentID: agentID,
		SlotInstanceID: "si_fix", SlotSpecVersion: 1, Scope: "workspace",
		CreatedAt: "2026-11-03T10:00:00Z", UpdatedAt: "2026-11-03T10:00:00Z"},
		slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: agentID, Tools: []string{"net"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}); err != nil {
		t.Fatal(err)
	}
	return "si_fix"
}

// TestCreateMissionSlotIntentIT #1/#2/#7：意图随建单事务四核——成功落
// slot_instance_id；换绑/暂停/撤权 → ErrSlotIntentViolated 整体回滚（无行）。
func TestCreateMissionSlotIntentIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	for _, a := range []personalagent.Agent{
		{AgentID: "ag_slot", DisplayName: "任职者", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"},
		{AgentID: "ag_other", DisplayName: "接班人", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"},
	} {
		if _, err := store.EnsureAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	si := slotFixSeed(t, store, "ag_slot")

	mk := func(id string) mission.Mission {
		row := missionRow(mission.StatusReady, "ag_slot")
		row.MissionID = id
		return row
	}
	intent := &mission.SlotIntent{InstanceID: si, AgentID: "ag_slot"}

	// 成功：意图随创建事实落列（#1——执行装配按实例固定任职）
	m, err := store.CreateMissionSlot(ctx, mk("mi_slot_ok"), nil, intent)
	if err != nil {
		t.Fatalf("建单: %v", err)
	}
	if m.SlotInstanceID != si {
		t.Fatalf("slot_instance_id 须落列: %+v", m)
	}

	// 换绑（A→B 插在复核与建单之间——事务内核对，#2）
	if _, _, err := store.ReleaseBinding(ctx, "bd_fix1", "换绑", "2026-11-03T10:01:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_fix2", AgentID: "ag_other",
		SlotInstanceID: si, SlotSpecVersion: 1, Scope: "workspace",
		CreatedAt: "2026-11-03T10:01:01Z", UpdatedAt: "2026-11-03T10:01:01Z"},
		slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMissionSlot(ctx, mk("mi_slot_rebind"), nil, intent); !errors.Is(err, mission.ErrSlotIntentViolated) {
		t.Fatalf("换绑后建单须拒: %v", err)
	}
	// 换回
	if _, _, err := store.ReleaseBinding(ctx, "bd_fix2", "换回", "2026-11-03T10:02:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_fix1b", AgentID: "ag_slot",
		SlotInstanceID: si, SlotSpecVersion: 1, Scope: "workspace",
		CreatedAt: "2026-11-03T10:02:01Z", UpdatedAt: "2026-11-03T10:02:01Z"},
		slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}

	// 暂停身份（#7：暂停不得建单）
	if _, _, err := store.TransitionAgent(ctx, "ag_slot", []string{"active"}, "suspended", "审校", "2026-11-03T10:03:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMissionSlot(ctx, mk("mi_slot_susp"), nil, intent); !errors.Is(err, mission.ErrSlotIntentViolated) {
		t.Fatalf("暂停身份建单须拒: %v", err)
	}
	if _, _, err := store.TransitionAgent(ctx, "ag_slot", []string{"suspended"}, "active", "恢复", "2026-11-03T10:04:00Z"); err != nil {
		t.Fatal(err)
	}

	// 撤权（#7：必需工具撤销 → 授权准入不满足）
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_slot",
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMissionSlot(ctx, mk("mi_slot_grant"), nil, intent); !errors.Is(err, mission.ErrSlotIntentViolated) {
		t.Fatalf("撤权后建单须拒: %v", err)
	}
	// 拒绝路径零残留（事务回滚）
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM missions WHERE mission_id = 'mi_slot_grant'`).Scan(&n)
	if n != 0 {
		t.Fatalf("拒绝须整体回滚: %d", n)
	}
}

// TestListExpiredRunningFractionalIT #15（SQLite 面）：now=.11Z 时恰命中已过期
// 的 .1Z（字符串比较会漏掉它并选中未来的 .111Z）。
func TestListExpiredRunningFractionalIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	mk := func(id, deadline string) {
		r := missionrun.Run{RunID: id, MissionID: "mi_fx_" + id, Attempt: 1, LeaseID: "lse_f",
			ManifestID: "cmf_f", RuntimeID: "rtx_f", BindingID: "bd_f", AgentID: "ag_f",
			State: missionrun.StateRunning, RequestedAt: "t", StartedAt: "t", UpdatedAt: "t",
			Deadline: deadline}
		if _, err := store.CreateRun(ctx, r, 0); err != nil {
			t.Fatal(err)
		}
	}
	mk("run_past", "2026-11-03T12:00:00.1Z")
	mk("run_future", "2026-11-03T12:00:00.111Z")
	got, err := store.ListExpiredRunning(ctx, "2026-11-03T12:00:00.11Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != "run_past" {
		t.Fatalf("时刻比较须恰命中已过期行: %+v", got)
	}
}
