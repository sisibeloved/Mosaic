//go:build it

// IT 层（0.6.13 复审）：建单身份门（#3）、CreateRun 执行者门（#4）、
// workspace_access 完整快照（#5）、模板唯一性锚（#7）、v30 遗留恢复与 reason
// 回落（#11/#12）、摘要墓碑（#13）。
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

func recheckAgent(t *testing.T, store *Store, id string) {
	t.Helper()
	if _, err := store.EnsureAgent(context.Background(), personalagent.Agent{
		AgentID: id, DisplayName: id, Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExplicitOwnerGateIT 复审 #3：显式 owner 的网关交办（RequireOwnerActive）
// ——建单事务内核对身份；预检后退役/暂停不得落入新承诺；域内直建（无门）
// 仍按任意生命周期可指派。
func TestExplicitOwnerGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_gate")
	gate := &mission.CreationGate{RequireOwnerActive: true}
	mk := func(id string) mission.Mission {
		row := missionRow(mission.StatusReady, "ag_gate")
		row.MissionID = id
		return row
	}
	if _, err := store.CreateMissionGated(ctx, mk("mi_gate_ok"), nil, gate); err != nil {
		t.Fatalf("active 建单: %v", err)
	}
	// 暂停 → 门拒绝（ErrOwnerGateViolated）且零残留
	if _, _, err := store.TransitionAgent(ctx, "ag_gate", []string{"active"}, "suspended", "复审", "2026-11-03T10:01:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMissionGated(ctx, mk("mi_gate_susp"), nil, gate); !errors.Is(err, mission.ErrOwnerGateViolated) {
		t.Fatalf("暂停后门建单须拒: %v", err)
	}
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM missions WHERE mission_id = 'mi_gate_susp'`).Scan(&n)
	if n != 0 {
		t.Fatalf("拒绝须回滚零残留: %d", n)
	}
	// 域内直建（无门）：任意生命周期可指派（归属事实）
	if _, err := store.CreateMission(ctx, mk("mi_gate_plain"), nil); err != nil {
		t.Fatalf("无门直建（域内路径）: %v", err)
	}
	if _, _, err := store.TransitionAgent(ctx, "ag_gate", []string{"suspended"}, "active", "恢复", "2026-11-03T10:02:00Z"); err != nil {
		t.Fatal(err)
	}
}

// TestWorkspaceAccessGateIT 复审 #5：授权档案 workspace_access 随事务内快照读取
// ——模板要求 read、授权 read_write（更高层级）不得误拒；撤为 none 后拒。
func TestWorkspaceAccessGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_ws")
	spec := slot.SlotSpec{SlotSpecID: "slot.wsreq", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "执行", Responsibility: "执行", Deliverables: []string{"产出"},
			Tools:   &slot.CapabilityList{Required: []string{"net"}},
			Runtime: &slot.RuntimeRequirements{WorkspaceAccess: "read"},
		},
		CreatedAt: "2026-11-03T10:00:00Z"}
	if _, err := store.CreateSpec(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{SlotInstanceID: "si_ws", SlotSpecID: "slot.wsreq",
		Scope: "workspace", Title: "执行", KnowledgeNamespace: "kn_ws", CreatedFromVersion: 1,
		CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_ws", AgentID: "ag_ws",
		SlotInstanceID: "si_ws", SlotSpecVersion: 1, Scope: "workspace",
		UpdatedAt: "2026-11-03T10:00:00Z", CreatedAt: "2026-11-03T10:00:00Z"}, slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	// 授权 read_write（满足 read 要求）——修复前 workspace_access 漏读为空 → 误拒
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_ws", Tools: []string{"net"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"net"}, WorkspaceAccess: "read_write"}}); err != nil {
		t.Fatal(err)
	}
	row := missionRow(mission.StatusReady, "ag_ws")
	row.MissionID = "mi_ws_ok"
	if _, err := store.CreateMissionGated(ctx, row, nil, &mission.CreationGate{
		Slot: &mission.SlotIntent{InstanceID: "si_ws", AgentID: "ag_ws", SpecID: "slot.wsreq"},
	}); err != nil {
		t.Fatalf("read_write 满足 read 要求——不得误拒（#5）: %v", err)
	}
	// 撤为 none → 拒
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_ws", Tools: []string{"net"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"net"}, WorkspaceAccess: "none"}}); err != nil {
		t.Fatal(err)
	}
	row2 := missionRow(mission.StatusReady, "ag_ws")
	row2.MissionID = "mi_ws_none"
	if _, err := store.CreateMissionGated(ctx, row2, nil, &mission.CreationGate{
		Slot: &mission.SlotIntent{InstanceID: "si_ws", AgentID: "ag_ws", SpecID: "slot.wsreq"},
	}); !errors.Is(err, mission.ErrSlotIntentViolated) {
		t.Fatalf("none 须拒: %v", err)
	}
}

// TestTemplateUniquenessGateIT 复审 #7：模板引用的意图在建单事务内核对该 scope
// 实例集仍恰为解析实例——长出兄弟实例即歧义拒建；显式实例路径不受限。
func TestTemplateUniquenessGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_tu")
	spec := slot.SlotSpec{SlotSpecID: "slot.tu", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "执行", Responsibility: "执行", Deliverables: []string{"产出"}},
		CreatedAt:   "2026-11-03T10:00:00Z"}
	if _, err := store.CreateSpec(ctx, spec); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"si_tu1", "si_tu2"} {
		if err := store.CreateInstance(ctx, slot.SlotInstance{SlotInstanceID: id, SlotSpecID: "slot.tu",
			Scope: "workspace", Title: id, KnowledgeNamespace: "kn_" + id, CreatedFromVersion: 1,
			CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_tu", AgentID: "ag_tu",
		SlotInstanceID: "si_tu1", SlotSpecVersion: 1, Scope: "workspace",
		UpdatedAt: "2026-11-03T10:00:00Z", CreatedAt: "2026-11-03T10:00:00Z"}, slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_tu",
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}); err != nil {
		t.Fatal(err)
	}
	row := missionRow(mission.StatusReady, "ag_tu")
	row.MissionID = "mi_tu_amb"
	// 模板意图 + 兄弟实例在场 → 歧义拒建
	if _, err := store.CreateMissionGated(ctx, row, nil, &mission.CreationGate{
		Slot: &mission.SlotIntent{InstanceID: "si_tu1", AgentID: "ag_tu", SpecID: "slot.tu"},
	}); !errors.Is(err, mission.ErrSlotIntentViolated) {
		t.Fatalf("模板兄弟实例须拒: %v", err)
	}
	// 显式实例路径（SpecID 空）不受兄弟实例数限制
	row2 := missionRow(mission.StatusReady, "ag_tu")
	row2.MissionID = "mi_tu_exp"
	if _, err := store.CreateMissionGated(ctx, row2, nil, &mission.CreationGate{
		Slot: &mission.SlotIntent{InstanceID: "si_tu1", AgentID: "ag_tu"},
	}); err != nil {
		t.Fatalf("显式实例路径不受限: %v", err)
	}
	// 恰一实例时模板路径通过
	if err := store.CreateInstance(ctx, slot.SlotInstance{SlotInstanceID: "si_tu_only", SlotSpecID: "slot.tu_only",
		Scope: "workspace", Title: "唯一", KnowledgeNamespace: "kn_only", CreatedFromVersion: 1,
		CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	// slot.tu_only 模板未建谱系——先建
	spec2 := slot.SlotSpec{SlotSpecID: "slot.tu_only", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "唯一", Responsibility: "执行", Deliverables: []string{"产出"}},
		CreatedAt:   "2026-11-03T10:00:00Z"}
	if _, err := store.CreateSpec(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_tu_only", AgentID: "ag_tu",
		SlotInstanceID: "si_tu_only", SlotSpecVersion: 1, Scope: "workspace",
		UpdatedAt: "2026-11-03T10:00:00Z", CreatedAt: "2026-11-03T10:00:00Z"}, slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	row3 := missionRow(mission.StatusReady, "ag_tu")
	row3.MissionID = "mi_tu_uniq"
	if _, err := store.CreateMissionGated(ctx, row3, nil, &mission.CreationGate{
		Slot: &mission.SlotIntent{InstanceID: "si_tu_only", AgentID: "ag_tu", SpecID: "slot.tu_only"},
	}); err != nil {
		t.Fatalf("恰一实例模板路径须通过: %v", err)
	}
}

// TestCreateRunOwnerGateIT 复审 #4：领取 Run 的建行事务内执行者门——负责人
// 暂停、所钉任职失效/换人均拒（ErrOwnerNotActive）；active 照常。
func TestCreateRunOwnerGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_run")
	recheckAgent(t, store, "ag_run2")
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_run"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mkRun := func(id, bindingID, agentID string) missionrun.Run {
		return missionrun.Run{RunID: id, MissionID: m.MissionID, LeaseID: "lse_r",
			BindingID: bindingID, AgentID: agentID, State: missionrun.StatePending,
			RequestedAt: "t", UpdatedAt: "t"}
	}
	// active + 在职 → 通过（随即终态化——后续用例不撞未终态探针）
	first, err := store.CreateRun(ctx, mkRun("run_ok", "bd_run", "ag_run"), 0, 1)
	if errors.Is(err, missionrun.ErrOwnerNotActive) {
		t.Fatalf("active 负责人不得拒: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	first.State = missionrun.StateCompleted
	if err := store.UpdateRun(ctx, first, nil); err != nil {
		t.Fatal(err)
	}
	// 暂停 → 拒（不自动解除任职的暂停身份不得启动）
	if _, _, err := store.TransitionAgent(ctx, "ag_run", []string{"active"}, "suspended", "复审", "2026-11-03T10:01:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(ctx, mkRun("run_susp", "bd_run", "ag_run"), 0, 1); !errors.Is(err, missionrun.ErrOwnerNotActive) {
		t.Fatalf("暂停身份启动须拒: %v", err)
	}
	if _, _, err := store.TransitionAgent(ctx, "ag_run", []string{"suspended"}, "active", "恢复", "2026-11-03T10:02:00Z"); err != nil {
		t.Fatal(err)
	}
	// 所钉任职换人 → 拒（active binding 属他人）
	spec := slot.SlotSpec{SlotSpecID: "slot.run", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "执行", Responsibility: "执行", Deliverables: []string{"产出"}},
		CreatedAt:   "2026-11-03T10:00:00Z"}
	if _, err := store.CreateSpec(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{SlotInstanceID: "si_run", SlotSpecID: "slot.run",
		Scope: "workspace", Title: "执行", KnowledgeNamespace: "kn_run", CreatedFromVersion: 1,
		CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_runx", AgentID: "ag_run2",
		SlotInstanceID: "si_run", SlotSpecVersion: 1, Scope: "workspace",
		UpdatedAt: "2026-11-03T10:00:00Z", CreatedAt: "2026-11-03T10:00:00Z"}, slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(ctx, mkRun("run_stale", "bd_runx", "ag_run"), 0, 1); !errors.Is(err, missionrun.ErrOwnerNotActive) {
		t.Fatalf("所钉任职换人须拒: %v", err)
	}
}

// TestUpdateLegacyUpgradeIT 复审 #11/#12：v30 候选库两故障点升级——
// (a) 修订已提交回执未完结（内容等值 → 恢复完结，reason 以命令值回落）；
// (b) 认领后修订未执行（v1 仍 v1 → 同键正确版本重试成功）。
func TestUpdateLegacyUpgradeIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_lg")
	mkRow := func(id, objective string, version int) mission.Mission {
		r := missionRow(mission.StatusReady, "ag_lg")
		r.MissionID = id
		r.ContractVersion = version
		r.Objective = objective
		r.Content = fixContent(objective)
		return r
	}

	// (a) 修订已提交 + 认领未完结（b59a395 窗口）：恢复完结，reason 回落
	if _, err := store.CreateMission(ctx, mkRow("mi_lg_a", "原目标", 1), nil); err != nil {
		t.Fatal(err)
	}
	c2 := fixContent("遗留恢复载荷")
	cmdA := dispatch.UpdateCmd{MissionID: "mi_lg_a", ExpectedVersion: 1,
		IdempotencyKey: "idem_lg_a", RequestID: "req_a", Contract: c2, Reason: "任务卡追加"}
	if err := seedLegacyClaim(store, "idem_lg_a", "dpr_lg_a", "dg_lg_a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ReviseMission(ctx, "mi_lg_a", 1, "ag_lg", mkRow("mi_lg_a", "遗留恢复载荷", 2)); err != nil || !ok {
		t.Fatalf("播种 v2: ok=%v %v", ok, err)
	}
	rcA, replayedA, errA := store.UpdateMissionAtomic(ctx, cmdA, "dg_lg_a", "2026-11-03T12:00:05Z", "dpr_new_a")
	if errA != nil || replayedA {
		t.Fatalf("遗留恢复 (a): %+v replayed=%v %v", rcA, replayedA, errA)
	}
	if rcA.DomainCommandID != "mi_lg_a:revise:v2" || rcA.ReceiptID != "dpr_lg_a" || rcA.Reason != "任务卡追加" {
		t.Fatalf("恢复回执 (a)（reason 回落 #12）: %+v", rcA)
	}

	// (b) 认领后修订未执行（v1）→ 同键正确版本重试成功
	if _, err := store.CreateMission(ctx, mkRow("mi_lg_b", "原目标", 1), nil); err != nil {
		t.Fatal(err)
	}
	cmdB := dispatch.UpdateCmd{MissionID: "mi_lg_b", ExpectedVersion: 1,
		IdempotencyKey: "idem_lg_b", Contract: fixContent("认领未执行载荷"), Reason: "追加"}
	if err := seedLegacyClaim(store, "idem_lg_b", "dpr_lg_b", "dg_lg_b"); err != nil {
		t.Fatal(err)
	}
	rcB, replayedB, errB := store.UpdateMissionAtomic(ctx, cmdB, "dg_lg_b", "2026-11-03T12:00:06Z", "dpr_new_b")
	if errB != nil || replayedB {
		t.Fatalf("遗留恢复 (b): %+v replayed=%v %v", rcB, replayedB, errB)
	}
	if rcB.DomainCommandID != "mi_lg_b:revise:v2" || rcB.ReceiptID != "dpr_lg_b" {
		t.Fatalf("恢复回执 (b): %+v", rcB)
	}

	// (c) 版本恰 expected+1 但内容不等值（他人已改——不能证明）→ 冲突透传
	if _, ok, err := store.ReviseMission(ctx, "mi_lg_a", 2, "ag_lg", mkRow("mi_lg_a", "他人载荷", 3)); err != nil || !ok {
		t.Fatalf("播种 v3: ok=%v %v", ok, err)
	}
	cmdC := dispatch.UpdateCmd{MissionID: "mi_lg_a", ExpectedVersion: 2,
		IdempotencyKey: "idem_lg_c", Contract: fixContent("与 v3 不等值载荷")}
	if _, _, err := store.UpdateMissionAtomic(ctx, cmdC, "dg_lg_c", "2026-11-03T12:00:07Z", "dpr_new_c"); err == nil {
		t.Fatal("内容不等值不得宣告成功")
	} else if !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("不等值须版本冲突透传: %v", err)
	}
	var nC int
	store.db.QueryRow(`SELECT COUNT(*) FROM dispatch_receipts WHERE idempotency_key = 'idem_lg_c'`).Scan(&nC)
	if nC != 0 {
		t.Fatalf("未证明提交的拒绝不留认领（原子面回滚）: %d", nC)
	}
}

// seedLegacyClaim 直插 v30 形态 pending 认领行（reason 空列——升级默认值）。
func seedLegacyClaim(store *Store, key, receiptID, dg string) error {
	_, err := store.db.Exec(`
INSERT INTO dispatch_receipts (receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at)
VALUES (?, 'owner', 'workspace', ?, 'req_legacy', ?, 'rd_legacy', 0, '', '[]', '[]', '', '')`,
		receiptID, key, dg)
	return err
}

// 摘要墓碑（#13）：删房后列表与详情对同一来源给出一致墓碑。
func TestListMissionsTombstoneIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_tb")
	m, err := store.CreateMission(ctx, func() mission.Mission {
		r := missionRow(mission.StatusReady, "ag_tb")
		r.MissionID = "mi_tb"
		return r
	}(), &mission.RoomSource{RoomID: "rm_tb", TaskID: "tsk_tb"})
	if err != nil {
		t.Fatal(err)
	}
	_ = m
	// 模拟删房墓碑（0.6.10 建表语义——来源墓碑不删映射）
	if _, err := store.db.Exec(`UPDATE mission_sources SET deleted_at = '2026-11-03T11:00:00Z' WHERE room_id = 'rm_tb'`); err != nil {
		t.Fatal(err)
	}
	sums, err := store.ListMissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sums {
		if s.RoomSource != nil && s.RoomSource.RoomID == "rm_tb" {
			if s.RoomSource.DeletedAt != "2026-11-03T11:00:00Z" {
				t.Fatalf("摘要须携墓碑（与详情一致）: %+v", s.RoomSource)
			}
			return
		}
	}
	t.Fatal("摘要未找到来源行")
}
