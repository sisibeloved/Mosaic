package dispatch

// 0.6.13 / ADR-0036：网关续接 mission_update（幂等 + 复核 + revise 回执）与
// Slot 职责寻址（§4.5：scope→实例→active 任职→Agent；无任职待任 / 多实例
// 歧义 / 投递复核 route_stale）。测试先行（红→绿）。

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// statusAgents 可配状态的身份件（fakeAgents 扩展——续接复核"owner 须 active"用例）。
type statusAgents struct {
	ids      map[string]bool
	statuses map[string]string
}

func (f *statusAgents) AgentExists(_ context.Context, id string) (bool, error) { return f.ids[id], nil }

func (f *statusAgents) AgentStatus(_ context.Context, id string) (string, bool, error) {
	if st, ok := f.statuses[id]; ok {
		return st, true, nil
	}
	if !f.ids[id] {
		return "", false, nil
	}
	return mission.AgentStatusActive, true, nil
}

func (f *statusAgents) setStatus(id, st string) {
	if f.statuses == nil {
		f.statuses = map[string]string{}
	}
	f.statuses[id] = st
}

// fakeSlots SlotLookups 测试件：实例表 + 每实例 active 任职 Agent（可中途改绑）。
type fakeSlots struct {
	instances    map[string]string   // instance → spec
	active       map[string]string   // instance → agent（无键 = 无 active 任职）
	exists       map[string]bool     // instance 存在性
	specs        map[string][]string // spec → 实例集
	grantMissing map[string][]string // instance → 授权缺失清单（0.6.13 审校 #7）
}

func (f *fakeSlots) SlotInstanceExists(_ context.Context, id string) (bool, error) {
	return f.exists[id], nil
}

func (f *fakeSlots) SlotInstancesOfSpec(_ context.Context, specID string) ([]string, error) {
	return f.specs[specID], nil
}

func (f *fakeSlots) ActiveBindingAgent(_ context.Context, instanceID string) (string, bool, error) {
	a, ok := f.active[instanceID]
	return a, ok, nil
}

// GrantAdmission 授权准入（0.6.13 审校 #7）：缺省放行；perInstance 配置缺失
// 清单模拟撤权（必需工具/技能被撤销 → blocked/拒建）。
func (f *fakeSlots) GrantAdmission(_ context.Context, instanceID, _ string) (bool, []string, error) {
	if f.grantMissing == nil {
		return true, nil, nil
	}
	if missing, ok := f.grantMissing[instanceID]; ok {
		return false, missing, nil
	}
	return true, nil, nil
}

// newUpdateFixture 真 mission 服务 + dispatch + 可配身份状态 + slot 件。
func newUpdateFixture(t *testing.T) (*Service, *mission.Service, *mission.MemStore, *statusAgents) {
	t.Helper()
	mem := mission.NewMemStore()
	agents := &statusAgents{ids: map[string]bool{"ag_a": true, "ag_b": true}}
	sources := fakeSources{tasks: map[[2]string]bool{{"r1", "tsk_a"}: true, {"r1", "tsk_b"}: true}}
	var n atomic.Int64
	mSvc := mission.New(mem,
		mission.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_u" + itoa(n.Add(1)) }),
		mission.WithAgentLookup(agents),
		mission.WithSourceLookup(sources))
	svc := New(NewMemStore(), mSvc, Lookups{
		Agents:  agents,
		Sources: sources,
		Mapped:  mSvc.SourceMapped,
	}, WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		WithNewRouteID(func() string { return "rd_" + itoa(n.Add(1)) }),
		WithNewReceiptID(func() string { return "dpr_" + itoa(n.Add(1)) }))
	return svc, mSvc, mem, agents
}

// TestUpdateIdempotentRevision 续接全链：修订成功（owner 不变 + 新版本 + 回执
// domain_command_id）→ 同键同载荷重放（不二次修订）→ 同键异载荷冲突 → 版本
// 冲突透传 → owner 非 active 拒。
func TestUpdateIdempotentRevision(t *testing.T) {
	svc, mSvc, _, agents := newUpdateFixture(t)
	ctx := context.Background()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, "ag_a", contract(), &src)
	if err != nil {
		t.Fatal(err)
	}
	c2 := contract()
	c2.Objective = "再加 Windows 与绑核结果"

	up, replayed, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-u1", Contract: c2, Reason: "任务卡追加",
	})
	if err != nil || replayed {
		t.Fatalf("续接: %+v replayed=%v %v", up, replayed, err)
	}
	if !strings.HasPrefix(up.DomainCommandID, m.MissionID+":revise:") || up.CommittedAt == "" {
		t.Fatalf("回执失真: %+v", up)
	}
	got, _, _ := mSvc.Get(ctx, m.MissionID)
	if got.ContractVersion != 2 || got.Objective != "再加 Windows 与绑核结果" || got.OwnerAgentID != "ag_a" {
		t.Fatalf("修订事实: %+v", got)
	}
	// 同键同载荷 → 重放原回执，不二次修订（版本仍 2）
	again, replayed, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-u1", Contract: c2, Reason: "任务卡追加", // 同载荷（含 reason）才是重放
	})
	if err != nil || !replayed || again.ReceiptID != up.ReceiptID {
		t.Fatalf("重放: replayed=%v %+v %v", replayed, again, err)
	}
	if got2, _, _ := mSvc.Get(ctx, m.MissionID); got2.ContractVersion != 2 {
		t.Fatalf("重放不得二次修订: v%d", got2.ContractVersion)
	}
	// 同键异载荷 → 幂等冲突
	c3 := contract()
	c3.Objective = "换载荷"
	if _, _, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 2,
		IdempotencyKey: "idem-u1", Contract: c3,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("同键异载荷须冲突: %v", err)
	}
	// 版本冲突透传（mission 域语义不变）
	if _, _, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 9,
		IdempotencyKey: "idem-u2", Contract: c2,
	}); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("版本冲突须透传: %v", err)
	}
	// owner 非 active → 拒（续接不发往已停身份）
	agents.setStatus("ag_a", "suspended")
	if _, _, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 2,
		IdempotencyKey: "idem-u3", Contract: c2,
	}); !errors.Is(err, ErrTargetNotActive) {
		t.Fatalf("owner 非 active 须拒: %v", err)
	}
	agents.setStatus("ag_a", "active")
	// 未知任务 / 非法幂等键
	if _, _, err := svc.Update(ctx, UpdateCmd{
		MissionID: "mi_none", ExpectedVersion: 1,
		IdempotencyKey: "idem-u4", Contract: c2,
	}); !errors.Is(err, mission.ErrMissionNotFound) {
		t.Fatalf("未知任务: %v", err)
	}
	if _, _, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 2,
		IdempotencyKey: "bad key!", Contract: c2,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法幂等键: %v", err)
	}
}

// TestResolveSlotAddressing §4.5 寻址分支：实例不存在 / 模板无实例 / 模板多实例
// 歧义 / 无 active 任职（ready 不自动激活）/ 正常解析；owner 与 slot 并存 400。
func TestResolveSlotAddressing(t *testing.T) {
	svc, _, _, _ := newUpdateFixture(t)
	svc.lookups.Slots = &fakeSlots{
		exists:    map[string]bool{"si_1": true, "si_2": true, "si_idle": true},
		instances: map[string]string{"si_1": "slot.a", "si_2": "slot.a", "si_idle": "slot.b"},
		active:    map[string]string{"si_1": "ag_b"},
		specs:     map[string][]string{"slot.a": {"si_1", "si_2"}, "slot.b": {"si_idle"}},
	}
	ctx := context.Background()

	slotReq := func() ResolveRequest {
		return ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
			RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()}
	}
	// 正常：实例 → active 任职 → Agent；owner 由寻址得出
	d, err := svc.Resolve(ctx, slotReq())
	if err != nil || d.Status != "resolved" || d.SelectionSource != "slot_binding" ||
		d.ResolvedAgentID != "ag_b" || d.ResolvedInstanceID != "si_1" || d.Request.OwnerAgentID != "" || len(d.TargetRefs) == 0 {
		t.Fatalf("slot 寻址: %+v %v", d, err)
	}
	// 无 active 任职（si_idle 有 ready Binding 也不自动激活——§4.5 明文）
	idle := slotReq()
	idle.SlotInstanceID = "si_idle"
	if d, err := svc.Resolve(ctx, idle); err != nil || d.Status != "blocked" || d.ReasonCode != "slot_unassigned" {
		t.Fatalf("无任职须 blocked slot_unassigned: %+v %v", d, err)
	}
	// 实例不存在
	none := slotReq()
	none.SlotInstanceID = "si_none"
	if d, err := svc.Resolve(ctx, none); err != nil || d.Status != "blocked" || d.ReasonCode != "slot_instance_not_found" {
		t.Fatalf("实例不存在: %+v %v", d, err)
	}
	// 模板多实例 → 歧义禁止猜选（须显式指实例）
	amb := slotReq()
	amb.SlotInstanceID = ""
	amb.SlotSpecID = "slot.a"
	if d, err := svc.Resolve(ctx, amb); err != nil || d.Status != "blocked" || d.ReasonCode != "slot_ambiguous" {
		t.Fatalf("多实例歧义: %+v %v", d, err)
	}
	// 模板恰一实例 → 解析到该实例
	uniq := slotReq()
	uniq.SlotInstanceID = ""
	uniq.SlotSpecID = "slot.b" // si_idle 无任职 → slot_unassigned（经模板路径）
	if d, err := svc.Resolve(ctx, uniq); err != nil || d.Status != "blocked" || d.ReasonCode != "slot_unassigned" {
		t.Fatalf("模板恰一实例（无任职）: %+v %v", d, err)
	}
	// 模板无实例
	noInst := slotReq()
	noInst.SlotInstanceID = ""
	noInst.SlotSpecID = "slot.z"
	if d, err := svc.Resolve(ctx, noInst); err != nil || d.Status != "blocked" || d.ReasonCode != "slot_no_instance" {
		t.Fatalf("模板无实例: %+v %v", d, err)
	}
	// owner 与 slot 并存 → 结构拒绝（显式 Agent ≠ 责任已移交——语义须先明确）
	both := slotReq()
	both.OwnerAgentID = "ag_a"
	if _, err := svc.Resolve(ctx, both); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("owner+slot 并存须 ErrInvalidRequest: %v", err)
	}
	// SlotLookups 未装配 → 宁拒
	svc2, _, _, _ := newUpdateFixture(t)
	if _, err := svc2.Resolve(ctx, slotReq()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("未装配 Slots 须拒绝: %v", err)
	}
}

// TestDispatchSlotStaleRereresolve slot 决策投递复核（§4.6；0.6.13 审校重写：
// 未映射任务卡起步——先断言决策 resolved，再换绑验证旧决策 route_stale；删除
// 复核分支时本测试必须失败——此前版本第二张任务卡预先被占用，初次解析即
// blocked，测试借 blocked 决策的拒投递侥幸通过）。
func TestDispatchSlotStaleRereresolve(t *testing.T) {
	svc, _, _, _ := newUpdateFixture(t)
	slots := &fakeSlots{
		exists:    map[string]bool{"si_1": true},
		instances: map[string]string{"si_1": "slot.a"},
		active:    map[string]string{"si_1": "ag_b"},
		specs:     map[string][]string{"slot.a": {"si_1"}},
	}
	svc.lookups.Slots = slots
	ctx := context.Background()

	// 未映射任务卡（tsk_b）→ resolved（断言决策面——非借 blocked 拒投递）
	d, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_b"}, Contract: contract()})
	if err != nil || d.Status != "resolved" || d.ResolvedAgentID != "ag_b" {
		t.Fatalf("解析须 resolved 且任职 ag_b: %+v %v", d, err)
	}
	// 未换绑 → 投递成功（建单 owner = 任职 Agent；slot 意图随建单）
	rc, replayed, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d.RouteID, IdempotencyKey: "idem-s1"})
	if err != nil || replayed {
		t.Fatalf("投递: %+v %v", rc, err)
	}
	// 新决策（tsk_a）→ 断言 resolved → 换绑（任职换 ag_c）→ 旧决策 409 stale
	d2, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()})
	if err != nil || d2.Status != "resolved" {
		t.Fatalf("第二决策须 resolved: %+v %v", d2, err)
	}
	slots.active["si_1"] = "ag_c"
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "idem-s2"}); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("换绑后投递须 route_stale: %v", err)
	}
}
