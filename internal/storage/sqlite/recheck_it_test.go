//go:build it

// IT 层（0.6.13 复审）：建单身份门（#3）、CreateRun 执行者门（#4）、
// workspace_access 完整快照（#5）、模板唯一性锚（#7）、v30 遗留恢复与 reason
// 回落（#11/#12）、摘要墓碑（#13）。
package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/dispatch"
	"github.com/sisibeloved/Mosaic/internal/execctx"
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
	first, err := store.CreateRun(ctx, mkRun("run_ok", "bd_run", "ag_run"), 0, 1, "")
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
	if _, err := store.CreateRun(ctx, mkRun("run_susp", "bd_run", "ag_run"), 0, 1, ""); !errors.Is(err, missionrun.ErrOwnerNotActive) {
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
	if _, err := store.CreateRun(ctx, mkRun("run_stale", "bd_runx", "ag_run"), 0, 1, ""); !errors.Is(err, missionrun.ErrOwnerNotActive) {
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

	// (a) 遗留 pending（v30 形态）+ 任务已前进 v2 → 原载荷重驱动 CAS 败 →
	// 待核验（不冒领——等值不可证明归属，三审 #2/#6）
	if _, err := store.CreateMission(ctx, mkRow("mi_lg_a", "原目标", 1), nil); err != nil {
		t.Fatal(err)
	}
	c2 := fixContent("遗留载荷")
	cmdA := dispatch.UpdateCmd{MissionID: "mi_lg_a", ExpectedVersion: 1,
		IdempotencyKey: "idem_lg_a", RequestID: "req_a", Contract: c2, Reason: "任务卡追加"}
	if err := seedLegacyClaim(store, "idem_lg_a", "dpr_lg_a", "dg_lg_a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ReviseMission(ctx, "mi_lg_a", 1, "ag_lg", mkRow("mi_lg_a", "遗留载荷", 2)); err != nil || !ok {
		t.Fatalf("播种 v2: ok=%v %v", ok, err)
	}
	if _, _, err := store.UpdateMissionAtomic(ctx, cmdA, "dg_lg_a", "2026-11-03T12:00:05Z", "dpr_new_a"); !errors.Is(err, dispatch.ErrLegacyPendingUnresolved) {
		t.Fatalf("遗留 CAS 败须待核验: %v", err)
	}
	// 处置：认领行可查（含 pending——ClaimByKey）→ 确认未执行 release → 同键携当前版本重提成功
	claim, found, err := store.ClaimByKey(ctx, dispatch.CallerOwner, dispatch.ScopeWorkspace, "idem_lg_a")
	if err != nil || !found || claim.DomainCommandID != "" || claim.RequestDigest != "dg_lg_a" {
		t.Fatalf("认领查询: found=%v %+v %v", found, claim, err)
	}
	if err := store.ReleaseKey(ctx, dispatch.CallerOwner, dispatch.ScopeWorkspace, "idem_lg_a", "dg_lg_a"); err != nil {
		t.Fatal(err)
	}
	cmdA2 := cmdA
	cmdA2.ExpectedVersion = 2
	rcA, replayedA, errA := store.UpdateMissionAtomic(ctx, cmdA2, "dg_lg_a2", "2026-11-03T12:00:06Z", "dpr_new_a2")
	if errA != nil || replayedA {
		t.Fatalf("release 后重提: %+v replayed=%v %v", rcA, replayedA, errA)
	}
	if rcA.DomainCommandID != "mi_lg_a:revise:v3" {
		t.Fatalf("重提回执: %+v", rcA)
	}

	// (b) 认领后修订未执行（v1）→ 同键正确版本重试直接成功（既有重驱动）
	if _, err := store.CreateMission(ctx, mkRow("mi_lg_b", "原目标", 1), nil); err != nil {
		t.Fatal(err)
	}
	cmdB := dispatch.UpdateCmd{MissionID: "mi_lg_b", ExpectedVersion: 1,
		IdempotencyKey: "idem_lg_b", Contract: fixContent("认领未执行载荷"), Reason: "追加"}
	if err := seedLegacyClaim(store, "idem_lg_b", "dpr_lg_b", "dg_lg_b"); err != nil {
		t.Fatal(err)
	}
	rcB, replayedB, errB := store.UpdateMissionAtomic(ctx, cmdB, "dg_lg_b", "2026-11-03T12:00:07Z", "dpr_new_b")
	if errB != nil || replayedB {
		t.Fatalf("重驱动 (b): %+v replayed=%v %v", rcB, replayedB, errB)
	}
	if rcB.DomainCommandID != "mi_lg_b:revise:v2" || rcB.ReceiptID != "dpr_lg_b" {
		t.Fatalf("重驱动回执 (b): %+v", rcB)
	}

	// (c) 新键等值内容（v2 由他方提交）→ version_conflict 且不留认领（不冒领）
	cmdC := dispatch.UpdateCmd{MissionID: "mi_lg_a", ExpectedVersion: 1,
		IdempotencyKey: "idem_lg_c", Contract: fixContent("遗留载荷")} // 内容恰为 v2
	if _, _, err := store.UpdateMissionAtomic(ctx, cmdC, "dg_lg_c", "2026-11-03T12:00:08Z", "dpr_new_c"); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("新键等值内容须冲突（不冒领）: %v", err)
	}
	var nC int
	store.db.QueryRow(`SELECT COUNT(*) FROM dispatch_receipts WHERE idempotency_key = 'idem_lg_c'`).Scan(&nC)
	if nC != 0 {
		t.Fatalf("新键冲突不留认领: %d", nC)
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

// TestCreateRunGrantGateIT 三审 #3：清单装配后撤回必需工具授权——建 Run 事务
// 内任职授权核对拒启（旧清单不送达执行器）；补授权后放行。
func TestCreateRunGrantGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_grant")
	spec := slot.SlotSpec{SlotSpecID: "slot.grant", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "执行", Responsibility: "执行", Deliverables: []string{"产出"},
			Tools: &slot.CapabilityList{Required: []string{"documents.read"}}},
		CreatedAt: "2026-11-03T10:00:00Z"}
	if _, err := store.CreateSpec(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{SlotInstanceID: "si_grant", SlotSpecID: "slot.grant",
		Scope: "workspace", Title: "执行", KnowledgeNamespace: "kn_grant", CreatedFromVersion: 1,
		CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_grant", AgentID: "ag_grant",
		SlotInstanceID: "si_grant", SlotSpecVersion: 1, Scope: "workspace",
		UpdatedAt: "2026-11-03T10:00:00Z", CreatedAt: "2026-11-03T10:00:00Z"}, slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMission(ctx, func() mission.Mission {
		r := missionRow(mission.StatusReady, "ag_grant")
		r.MissionID = "mi_grant"
		return r
	}(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string) missionrun.Run {
		return missionrun.Run{RunID: id, MissionID: m.MissionID, LeaseID: "lse_g", BindingID: "bd_grant",
			AgentID: "ag_grant", State: missionrun.StatePending, RequestedAt: "t", UpdatedAt: "t"}
	}
	// 授权满足 → 放行（随即终态化腾出未终态位）
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_grant", Tools: []string{"documents.read"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}); err != nil {
		t.Fatal(err)
	}
	r0, err := store.CreateRun(ctx, mk("run_g0"), 0, 0, "")
	if err != nil {
		t.Fatalf("授权满足须放行: %v", err)
	}
	r0.State = missionrun.StateCompleted
	if err := store.UpdateRun(ctx, r0, nil); err != nil {
		t.Fatal(err)
	}
	// 撤权（身份/任职/版本均不变）→ 拒领旧清单
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_grant",
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(ctx, mk("run_g1"), 0, 0, ""); !errors.Is(err, missionrun.ErrOwnerNotActive) {
		t.Fatalf("撤权后须拒领: %v", err)
	}
}

// TestCreateRunOwnerHandoverIT 三审 #4：清单装配与建行之间交接承接（owner 翻转
// 不递增版本）——旧负责人清单不得领取新 Run。
func TestCreateRunOwnerHandoverIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_old")
	recheckAgent(t, store, "ag_new")
	m, err := store.CreateMission(ctx, func() mission.Mission {
		r := missionRow(mission.StatusReady, "ag_old")
		r.MissionID = "mi_ho"
		return r
	}(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟交接承接：owner 翻转、版本不递增（0.6.12 语义）
	if _, err := store.db.Exec(`UPDATE missions SET owner_agent_id = 'ag_new' WHERE mission_id = ?`, m.MissionID); err != nil {
		t.Fatal(err)
	}
	r := missionrun.Run{RunID: "run_ho", MissionID: m.MissionID, LeaseID: "lse_h",
		BindingID: "bd_h", AgentID: "ag_old", State: missionrun.StatePending, RequestedAt: "t", UpdatedAt: "t"}
	if _, err := store.CreateRun(ctx, r, 0, 0, ""); !errors.Is(err, missionrun.ErrOwnerNotActive) {
		t.Fatalf("交接后旧负责人须拒领: %v", err)
	}
	// 新负责人放行
	r2 := r
	r2.RunID, r2.AgentID = "run_ho2", "ag_new"
	if _, err := store.CreateRun(ctx, r2, 0, 0, ""); err != nil {
		t.Fatalf("新负责人须放行: %v", err)
	}
}

// TestUpdateRunTerminalLockWindowIT 四审 #6：终态写入等待 SQLite 写锁期间
// 期限越过——取得写锁后以现行时刻裁决（nowFn 锁后取值）：completed 改判
// canceled，迟到正文只审计。
func TestUpdateRunTerminalLockWindowIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	seed := missionrun.Run{RunID: "run_lw", MissionID: "mi_lw", Attempt: 1, LeaseID: "lse_lw",
		State: missionrun.StateRunning, RequestedAt: "t", UpdatedAt: "t",
		Deadline: "2026-11-03T12:01:00.000000000Z"}
	if _, err := store.CreateRun(ctx, seed, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 另一连接持写锁（模拟并发写者）
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	clockNow := "2026-11-03T12:00:30.000000000Z" // 锁前未过期
	done := make(chan missionrun.Run, 1)
	go func() {
		r := seed
		r.State, r.ResultBlock = missionrun.StateCompleted, "锁窗口内迟到正文"
		out, err := store.UpdateRunTerminal(ctx, r, nil, func() string { return clockNow })
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	time.Sleep(150 * time.Millisecond)          // 等终写阻塞在锁上
	clockNow = "2026-11-03T12:01:30.000000000Z" // 等锁期间期限越过（锁后取值生效）
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	select {
	case out := <-done:
		if out.State != missionrun.StateCanceled || out.ResultBlock != "" {
			t.Fatalf("锁后裁决须 canceled: %+v", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("终写未返回")
	}
	got, _, _ := store.RunByID(ctx, "run_lw")
	if got.State != missionrun.StateCanceled || got.ResultBlock != "" {
		t.Fatalf("落库终态: %+v", got)
	}
}

// TestCreateRunGrantFingerprintIT 四审 #3：清单授权快照指纹——同授权放行、
// 撤销可选上下文（必需项仍满足）即漂移拒领（重新装配降级）。
func TestCreateRunGrantFingerprintIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_fp")
	spec := slot.SlotSpec{SlotSpecID: "slot.fp", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "执行", Responsibility: "执行", Deliverables: []string{"产出"},
			Tools:   &slot.CapabilityList{Required: []string{"net"}},
			Context: &slot.CapabilityList{Optional: []string{"room.recent"}}},
		CreatedAt: "2026-11-03T10:00:00Z"}
	if _, err := store.CreateSpec(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{SlotInstanceID: "si_fp", SlotSpecID: "slot.fp",
		Scope: "workspace", Title: "执行", KnowledgeNamespace: "kn_fp", CreatedFromVersion: 1,
		CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBinding(ctx, slot.Binding{BindingID: "bd_fp", AgentID: "ag_fp",
		SlotInstanceID: "si_fp", SlotSpecVersion: 1, Scope: "workspace",
		UpdatedAt: "2026-11-03T10:00:00Z", CreatedAt: "2026-11-03T10:00:00Z"}, slot.BindingActive, nil); err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMission(ctx, func() mission.Mission {
		r := missionRow(mission.StatusReady, "ag_fp")
		r.MissionID = "mi_fp"
		return r
	}(), nil)
	if err != nil {
		t.Fatal(err)
	}
	grantA := slot.AgentGrant{AgentID: "ag_fp", Tools: []string{"net"}, Context: []string{"room.recent"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}
	if err := store.PutGrant(ctx, grantA); err != nil {
		t.Fatal(err)
	}
	fpA := execctx.GrantFingerprint(grantA)
	r0 := missionrun.Run{RunID: "run_fp0", MissionID: m.MissionID, LeaseID: "lse_f0", BindingID: "bd_fp",
		AgentID: "ag_fp", State: missionrun.StatePending, RequestedAt: "t", UpdatedAt: "t"}
	if _, err := store.CreateRun(ctx, r0, 0, 0, fpA); err != nil {
		t.Fatalf("同授权须放行: %v", err)
	}
	r0.State = missionrun.StateCompleted
	if err := store.UpdateRun(ctx, r0, nil); err != nil {
		t.Fatal(err)
	}
	// 撤销可选上下文（必需仍满足——readiness 不变，指纹漂移）
	grantB := slot.AgentGrant{AgentID: "ag_fp", Tools: []string{"net"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}
	if err := store.PutGrant(ctx, grantB); err != nil {
		t.Fatal(err)
	}
	r1 := r0
	r1.RunID, r1.LeaseID, r1.State = "run_fp1", "lse_f1", missionrun.StatePending
	if _, err := store.CreateRun(ctx, r1, 0, 0, fpA); !errors.Is(err, missionrun.ErrGrantDrift) {
		t.Fatalf("可选撤销须授权漂移拒领: %v", err)
	}
	// 按新授权重新装配（新指纹）放行
	fpB := execctx.GrantFingerprint(grantB)
	r2 := r0
	r2.RunID, r2.LeaseID, r2.State = "run_fp2", "lse_f2", missionrun.StatePending
	if _, err := store.CreateRun(ctx, r2, 0, 0, fpB); err != nil {
		t.Fatalf("新装配须放行: %v", err)
	}
}

// TestReleaseClaimOutcomesIT 四审 #4/#8：处置语义——create 认领（route 非空）
// 不可释放；update pending 同摘要释放成功；摘要不符/已完结精确分辨。
func TestReleaseClaimOutcomesIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	// create 类 pending（route 非空——在途建单）
	if _, err := store.db.Exec(`
INSERT INTO dispatch_receipts (receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at)
VALUES ('dpr_cr1', 'owner', 'workspace', 'key_create', 'r', 'dg_cr', 'rd_1', 1, '', '[]', '[]', '', '')`); err != nil {
		t.Fatal(err)
	}
	if out, err := store.ReleaseClaim(ctx, "owner", "workspace", "key_create", "dg_cr"); err != nil || out != "not_releasable" {
		t.Fatalf("create 认领不可释放: out=%s %v", out, err)
	}
	// update 类 pending（route 空）——同摘要释放
	if _, err := store.db.Exec(`
INSERT INTO dispatch_receipts (receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at)
VALUES ('dpr_up1', 'owner', 'workspace', 'key_upd', 'r', 'dg_up', '', 0, '', '[]', '[]', '', '')`); err != nil {
		t.Fatal(err)
	}
	if out, err := store.ReleaseClaim(ctx, "owner", "workspace", "key_upd", "dg_wrong"); err != nil || out != "digest_mismatch" {
		t.Fatalf("摘要不符: out=%s %v", out, err)
	}
	if out, err := store.ReleaseClaim(ctx, "owner", "workspace", "key_upd", "dg_up"); err != nil || out != "released" {
		t.Fatalf("同摘要释放: out=%s %v", out, err)
	}
	// 已完结
	if _, err := store.db.Exec(`
INSERT INTO dispatch_receipts (receipt_id, caller_ref, scope, idempotency_key, request_id, request_digest, route_id, decision_version, domain_command_id, target_refs, return_refs, reason, committed_at)
VALUES ('dpr_fn1', 'owner', 'workspace', 'key_fin', 'r', 'dg_fn', '', 0, 'mi_x:revise:v2', '[]', '[]', '', '2026-11-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if out, err := store.ReleaseClaim(ctx, "owner", "workspace", "key_fin", "dg_fn"); err != nil || out != "finalized" {
		t.Fatalf("已完结不可动: out=%s %v", out, err)
	}
	if out, err := store.ReleaseClaim(ctx, "owner", "workspace", "key_none", "dg"); err != nil || out != "not_found" {
		t.Fatalf("无行: out=%s %v", out, err)
	}
}

// TestUpdateRunTerminalControlIntentIT 五审既有 P1：人工暂停/取消已持久化
// （mission 行 paused/canceling）——终态事务内核对停止意图，未到期限的完成也
// 改按 canceled、正文只审计；事件组唯一（无 completed/failed 残留）。
func TestUpdateRunTerminalControlIntentIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_ci")
	m, err := store.CreateMission(ctx, func() mission.Mission {
		r := missionRow(mission.StatusRunning, "ag_ci")
		r.MissionID = "mi_ci"
		return r
	}(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := missionrun.Run{RunID: "run_ci", MissionID: m.MissionID, Attempt: 1, LeaseID: "lse_ci",
		BindingID: "bd_ci", AgentID: "ag_ci", State: missionrun.StateRunning,
		Generation: 7, RequestedAt: "t", StartedAt: "t", UpdatedAt: "t",
		Deadline: "2030-01-01T00:00:00.000000000Z"} // 远未到期
	if _, err := store.CreateRun(ctx, seed, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 人工暂停已生效（mission → paused；停止信号已送达的持久等价物）
	if _, ok, err := store.PauseMission(ctx, m.MissionID, m.ContractVersion, "人工暂停", "2026-11-04T10:00:00Z"); err != nil || !ok {
		t.Fatalf("暂停: ok=%v %v", ok, err)
	}
	r := seed
	r.State, r.ResultBlock = missionrun.StateCompleted, "暂停后才返回的正文"
	out, err := store.UpdateRunTerminal(ctx, r, []missionrun.Event{{
		RunID: r.RunID, EventType: missionrun.EventCompleted, OccurredAt: "2026-11-04T10:00:01Z",
		Payload: map[string]any{"late": false},
	}}, func() string { return "2026-11-04T10:00:01Z" })
	if err != nil {
		t.Fatal(err)
	}
	if out.State != missionrun.StateCanceled || out.ResultBlock != "" {
		t.Fatalf("人工停止后完成须改判 canceled: %+v", out)
	}
	evs, _ := store.RunEvents(ctx, "run_ci")
	causeOK, stale := false, false
	for _, e := range evs {
		if e.EventType == missionrun.EventCanceled && e.Payload["cause"] == "pause" {
			causeOK = true // 六审 #3：公开动作码 pause（非状态码 paused）
		}
		if e.EventType == missionrun.EventCompleted || e.EventType == missionrun.EventFailed {
			stale = true
		}
		if e.EventType == missionrun.EventLateResult {
			if e.Payload["generation"] != float64(7) {
				t.Fatalf("迟到审计须携 generation: %+v", e.Payload)
			}
		}
	}
	if !causeOK || stale {
		t.Fatalf("事件组（cause=paused=%v，残留原终态=%v）: %+v", causeOK, stale, evs)
	}
}

// TestPauseResumeTerminalIT 六审 #2：暂停→立即恢复——Run 自身的持久停止事实
//（信号时写入 stop_requested，resume 不清除）使终态事务改判 canceled；cause
// 为公开动作码 pause（六审 #3——非 Mission 状态码 paused）。
func TestPauseResumeTerminalIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_pr")
	m, err := store.CreateMission(ctx, func() mission.Mission {
		r := missionRow(mission.StatusRunning, "ag_pr")
		r.MissionID = "mi_pr"
		return r
	}(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := missionrun.Run{RunID: "run_pr", MissionID: m.MissionID, Attempt: 1, LeaseID: "lse_pr",
		BindingID: "bd_pr", AgentID: "ag_pr", State: missionrun.StateRunning,
		Generation: 3, RequestedAt: "t", StartedAt: "t", UpdatedAt: "t",
		Deadline: "2030-01-01T00:00:00.000000000Z"}
	if _, err := store.CreateRun(ctx, seed, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 暂停（持久停止意图 + 信号送达时写 Run 停止事实）→ 立即恢复（mission 回 ready）
	if _, ok, err := store.PauseMission(ctx, m.MissionID, m.ContractVersion, "人工暂停", "2026-11-04T12:00:00Z"); err != nil || !ok {
		t.Fatalf("暂停: ok=%v %v", ok, err)
	}
	if err := store.AppendRunEvents(ctx, "run_pr", []missionrun.Event{{
		RunID: "run_pr", EventType: missionrun.EventStopRequested, OccurredAt: "2026-11-04T12:00:00Z",
		Payload: map[string]any{"cause": "pause", "signalled": true},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ResumeMission(ctx, m.MissionID, m.ContractVersion, "ready", "2026-11-04T12:00:01Z"); err != nil || !ok {
		t.Fatalf("恢复: ok=%v %v", ok, err)
	}
	// 旧 Run 此后才到达终态写入——即便 mission 已回 ready，Run 自身停止事实生效
	r := seed
	r.State, r.ResultBlock = missionrun.StateCompleted, "暂停窗口内返回的 720 字正文……"
	out, err := store.UpdateRunTerminal(ctx, r, []missionrun.Event{{
		RunID: r.RunID, EventType: missionrun.EventCompleted, OccurredAt: "2026-11-04T12:00:02Z",
		Payload: map[string]any{"late": false},
	}}, func() string { return "2026-11-04T12:00:02Z" })
	if err != nil {
		t.Fatal(err)
	}
	if out.State != missionrun.StateCanceled || out.ResultBlock != "" {
		t.Fatalf("恢复后旧 Run 完成须改判 canceled: %+v", out)
	}
	evs, _ := store.RunEvents(ctx, "run_pr")
	causeOK := false
	for _, e := range evs {
		if e.EventType == missionrun.EventCanceled && e.Payload["cause"] == "pause" {
			causeOK = true // 动作码 pause（非状态码 paused——六审 #3）
		}
		if (e.EventType == missionrun.EventCompleted || e.EventType == missionrun.EventFailed) && e.Payload["late"] == false {
			t.Fatalf("不得残留原终态事件: %+v", e)
		}
	}
	if !causeOK {
		t.Fatalf("canceled 事件 cause 须为 pause: %+v", evs)
	}
}

// TestVerifyGrantFingerprintIT 六审基线：Run 建行后、交付前撤权——交付前复核
// 拒绝（ErrGrantDrift）；同授权恒过；指纹空恒过（测试容差）。
func TestVerifyGrantFingerprintIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	recheckAgent(t, store, "ag_vf")
	gA := slot.AgentGrant{AgentID: "ag_vf", Tools: []string{"net"}, Context: []string{"room.recent"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}
	if err := store.PutGrant(ctx, gA); err != nil {
		t.Fatal(err)
	}
	fpA := execctx.GrantFingerprint(gA)
	if err := store.VerifyGrantFingerprint(ctx, "ag_vf", "bd_vf", fpA); err != nil {
		t.Fatalf("同授权须过: %v", err)
	}
	if err := store.VerifyGrantFingerprint(ctx, "ag_vf", "bd_vf", ""); err != nil {
		t.Fatalf("空指纹（测试容差）须过: %v", err)
	}
	// 交付前撤权（撤可选上下文）→ 漂移拒付
	gB := slot.AgentGrant{AgentID: "ag_vf", Tools: []string{"net"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read"}}
	if err := store.PutGrant(ctx, gB); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyGrantFingerprint(ctx, "ag_vf", "bd_vf", fpA); !errors.Is(err, missionrun.ErrGrantDrift) {
		t.Fatalf("撤权后须拒付: %v", err)
	}
}
