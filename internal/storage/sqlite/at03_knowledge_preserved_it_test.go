//go:build it

// IT 层（AT-03 收尾断言 / 0.6.12 阶段验收盘点）：退役不抹知识——Mission 版本行、
// 事件流、交付包与职责实例知识均独立于 Agent 生命周期；Agent retired 后全部
// 仍可读（RFC §3.3"共享知识、职责知识与 Mission 产物不随 Agent 退役消失"）。
package sqlite

import (
	"context"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/delivery"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

// TestAT03KnowledgePreservedIT 退役后知识保留：handover 承接 + retire 完成 →
// 版本行 / 事件流（含承接回执）/ 交付包 / 职责实例与授权档案全部仍可读。
func TestAT03KnowledgePreservedIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	for _, a := range []personalagent.Agent{
		{AgentID: "ag_kp1", DisplayName: "退役者", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"},
		{AgentID: "ag_kp2", DisplayName: "接手者", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:01Z"},
	} {
		if _, err := store.EnsureAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// 职责实例 + 授权档案（职责知识面）
	spec, err := store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.kp", Version: 1, SchemaVersion: 1,
		SpecContent: slot.SpecContent{Name: "知识保留", Responsibility: "负责验收", Deliverables: []string{"报告"}},
		CreatedAt:   "2026-11-03T10:00:01Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, slot.SlotInstance{
		SlotInstanceID: "si_kp", SlotSpecID: spec.SlotSpecID, Scope: "workspace",
		Title: "知识保留实例", KnowledgeNamespace: "slotns_kp", CreatedFromVersion: 1, CreatedAt: "2026-11-03T10:00:02Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_kp1", UpdatedAt: "2026-11-03T10:00:03Z",
		Skills: []slot.SkillRef{}, Tools: []string{"documents.read"}, Context: []string{"mission.contract"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"net"}, WorkspaceAccess: "read"}}); err != nil {
		t.Fatal(err)
	}

	// Mission（带修订版本行）+ 交付包 + 交接承接 → retire
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_kp1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	next := m
	next.ContractVersion = 2
	next.Objective = "知识保留（修订）"
	if _, ok, err := store.ReviseMission(ctx, m.MissionID, 1, "ag_kp1", next); err != nil || !ok {
		t.Fatalf("修订: ok=%v %v", ok, err)
	}
	if err := store.CreatePackage(ctx, delivery.Package{
		PackageID: "dlv_kp", MissionID: m.MissionID, RunID: "run_kp", Attempt: 1, Generation: 1,
		ContractVersion: 2, ArtifactRefs: []string{"ws://r.md"},
		AcceptanceMapping: []delivery.MappingEntry{{Criterion: "产出就绪", Evidence: "CI 绿"}},
		SubmittedAt:       "2026-11-03T10:00:04Z",
	}, delivery.Receipt{RunID: "run_kp", EventType: delivery.EventDeliverySubmitted,
		Payload: map[string]any{"package_id": "dlv_kp"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverOffer(ctx, m.MissionID, 2, "ag_kp2", "hdo_kp",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"offer_id": "hdo_kp"}, "t")); err != nil || !ok {
		t.Fatalf("发起: %v %v", ok, err)
	}
	if _, ok, err := store.HandoverAccept(ctx, m.MissionID, 2, "hdo_kp",
		handoverEvent(mission.EventHandoverAccepted, map[string]any{}, "t")); err != nil || !ok {
		t.Fatalf("承接: %v %v", ok, err)
	}
	if _, done, err := store.RetireAgent(ctx, "ag_kp1", []string{"active"}, "清退", "t"); err != nil || !done {
		t.Fatalf("退役: done=%v %v", done, err)
	}
	var st string
	store.db.QueryRow(`SELECT status FROM agents WHERE agent_id = 'ag_kp1'`).Scan(&st)
	if st != "retired" {
		t.Fatalf("应 retired: %s", st)
	}

	// 断言：退役后知识面全部仍可读
	vs, ok, _ := store.MissionVersions(ctx, m.MissionID)
	if !ok || len(vs) != 2 || vs[1].Objective != "知识保留（修订）" {
		t.Fatalf("版本行须保留: ok=%v %+v", ok, vs)
	}
	evs, _, _ := store.MissionEvents(ctx, m.MissionID)
	kinds := map[string]bool{}
	for _, e := range evs {
		kinds[e.EventType] = true
	}
	if !kinds[mission.EventCreated] || !kinds[mission.EventContractRevised] ||
		!kinds[mission.EventHandoverOffered] || !kinds[mission.EventHandoverAccepted] {
		t.Fatalf("事件史（含承接回执）须保留: %v", kinds)
	}
	if _, ok, _ := store.PackageByID(ctx, "dlv_kp"); !ok {
		t.Fatal("交付包须保留可读")
	}
	if got, ok, _ := store.InstanceByID(ctx, "si_kp"); !ok || got.KnowledgeNamespace != "slotns_kp" {
		t.Fatalf("职责实例（知识命名空间）须保留: %+v", got)
	}
	if _, ok, _ := store.GrantByAgent(ctx, "ag_kp1"); !ok {
		t.Fatal("授权档案须保留可读")
	}
	// 承接者接手后照常可用（知识不因换人消失）
	if cur, ok, _ := store.MissionByID(ctx, m.MissionID); !ok || cur.OwnerAgentID != "ag_kp2" || cur.ContractVersion != 2 {
		t.Fatalf("承接着视角: %+v", cur)
	}
	print("AT03-KNOWLEDGE-PRESERVED-OK")
}
