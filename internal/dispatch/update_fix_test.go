package dispatch

// 0.6.13 审校修复回归（域内面）：#7 预检身份 active + 授权准入、#10 原请求
// 摘要稳定（换绑重解析同键重放）、#11 拒绝回滚认领、#13 pending 采纳原行、
// #17 域校验、模板实例集漂移 stale。SQLite 原子续接面在 it 层覆盖。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// TestResolvePrecheckActiveAndGrants #7：显式 owner 暂停 → blocked
// agent_not_active；slot 任职授权缺失 → blocked slot_grant_missing；恢复后
// resolved——预检与投递复核同源（precheck 单源）。
func TestResolvePrecheckActiveAndGrants(t *testing.T) {
	svc, _, _, agents := newUpdateFixture(t)
	slots := &fakeSlots{
		exists:    map[string]bool{"si_1": true},
		instances: map[string]string{"si_1": "slot.a"},
		active:    map[string]string{"si_1": "ag_b"},
		specs:     map[string][]string{"slot.a": {"si_1"}},
	}
	svc.lookups.Slots = slots
	ctx := context.Background()

	// 显式 owner 暂停 → blocked agent_not_active（暂停身份不接新交办）
	agents.setStatus("ag_a", "suspended")
	d, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, OwnerAgentID: "ag_a",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()})
	if err != nil || d.Status != "blocked" || d.ReasonCode != ReasonAgentNotActive {
		t.Fatalf("暂停身份须 blocked agent_not_active: %+v %v", d, err)
	}
	agents.setStatus("ag_a", "active")

	// slot 任职授权缺失（必需工具被撤销）→ blocked slot_grant_missing（§4.6）
	slots.grantMissing = map[string][]string{"si_1": {"tools:net"}}
	d2, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()})
	if err != nil || d2.Status != "blocked" || d2.ReasonCode != ReasonSlotGrantMissing {
		t.Fatalf("授权缺失须 blocked slot_grant_missing: %+v %v", d2, err)
	}
	slots.grantMissing = nil

	// 恢复后 resolved
	d3, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()})
	if err != nil || d3.Status != "resolved" {
		t.Fatalf("恢复后须 resolved: %+v %v", d3, err)
	}
}

// TestUpdateRejectReleasesKey #11（分步退化面）：领域拒绝回滚认领——同键修正
// 后重试成功（不被 pending 占键 409）；拒绝后的键公开查询不可见。
func TestUpdateRejectReleasesKey(t *testing.T) {
	svc, mSvc, _, _ := newUpdateFixture(t)
	ctx := context.Background()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, "ag_a", contract(), &src)
	if err != nil {
		t.Fatal(err)
	}
	c2 := contract()
	c2.Objective = "修正后的载荷"

	// 版本冲突拒绝 → 键被回滚（不占键）
	if _, _, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 9,
		IdempotencyKey: "idem-rel", Contract: c2,
	}); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("版本冲突须透传: %v", err)
	}
	// 公开查询不可见（pending 是事务内部事实——#11）
	if _, found, _ := svc.ReceiptByKey(ctx, "idem-rel"); found {
		t.Fatal("被拒请求不得留下可见回执")
	}
	// 同键修正后重试 → 成功（键已释放）
	up, replayed, err := svc.Update(ctx, UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-rel", Contract: c2, Reason: "修正后重试",
	})
	if err != nil || replayed {
		t.Fatalf("同键修正重试须成功: %+v replayed=%v %v", up, replayed, err)
	}
	// owner 非 active 拒绝同样释放键
	svc2, mSvc2, _, agents2 := newUpdateFixture(t)
	m2, _ := mSvc2.Create(ctx, "ag_a", contract(), &src)
	agents2.setStatus("ag_a", "suspended")
	if _, _, err := svc2.Update(ctx, UpdateCmd{
		MissionID: m2.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-rel2", Contract: c2,
	}); !errors.Is(err, ErrTargetNotActive) {
		t.Fatalf("非 active 须拒: %v", err)
	}
	agents2.setStatus("ag_a", "active")
	if up2, _, err := svc2.Update(ctx, UpdateCmd{
		MissionID: m2.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-rel2", Contract: c2,
	}); err != nil || up2.DomainCommandID == "" {
		t.Fatalf("恢复后同键须成功: %+v %v", up2, err)
	}
}

// TestUpdatePendingAdoptsOriginalReceipt #13/#16：恢复重驱动命中 pending 原行
// ——receipt_id/request_id/reason 沿用首认领；目标引用携修订后版本；重放同 ID。
func TestUpdatePendingAdoptsOriginalReceipt(t *testing.T) {
	svc, mSvc, _, _ := newUpdateFixture(t)
	ctx := context.Background()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, "ag_a", contract(), &src)
	if err != nil {
		t.Fatal(err)
	}
	c2 := contract()
	c2.Objective = "重驱动载荷"
	cmd := UpdateCmd{
		MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-pend", RequestID: "req_orig",
		Contract: c2, Reason: "任务卡追加",
	}
	// 模拟首认领后、完结前中断：直接落 pending 原行（dpr_orig）
	claim := Receipt{
		ReceiptID: "dpr_orig", CallerRef: CallerOwner, Scope: ScopeWorkspace,
		IdempotencyKey: cmd.IdempotencyKey, RequestID: "req_orig",
		RequestDigest: updateDigest(cmd), Reason: cmd.Reason,
		TargetRefs: []string{m.MissionID},
	}
	if _, conflict, finalized, err := svc.store.ClaimKey(ctx, claim); err != nil || conflict || finalized {
		t.Fatalf("播种 pending: %v conflict=%v finalized=%v", err, conflict, finalized)
	}
	// 重驱动：成功响应沿用原行标识（不换新 receipt_id）
	up, replayed, err := svc.Update(ctx, cmd)
	if err != nil || replayed {
		t.Fatalf("重驱动: %+v replayed=%v %v", up, replayed, err)
	}
	if up.ReceiptID != "dpr_orig" || up.RequestID != "req_orig" || up.Reason != "任务卡追加" {
		t.Fatalf("须采纳原行: %+v", up)
	}
	if len(up.TargetRefs) != 1 || up.TargetRefs[0] != "mission:"+m.MissionID+":v2" {
		t.Fatalf("目标引用须携修订后版本: %+v", up.TargetRefs)
	}
	// 重放 → 同一回执 ID（#13：重驱动成功与重放不再给出不同 ID）
	again, replayed2, err := svc.Update(ctx, cmd)
	if err != nil || !replayed2 || again.ReceiptID != "dpr_orig" {
		t.Fatalf("重放须同 ID: %+v replayed=%v %v", again, replayed2, err)
	}
}

// TestUpdateValidationDomain #17（域侧兜底）：expected_version <1 / request_id
// 超长 / reason 超长 → ErrInvalidRequest（先于幂等认领）。
func TestUpdateValidationDomain(t *testing.T) {
	svc, _, _, _ := newUpdateFixture(t)
	ctx := context.Background()
	base := UpdateCmd{MissionID: "mi_x", ExpectedVersion: 1, IdempotencyKey: "idem-v", Contract: contract()}
	if _, _, err := svc.Update(ctx, UpdateCmd{MissionID: "mi_x", IdempotencyKey: "idem-v", Contract: contract()}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected_version<1 须拒: %v", err)
	}
	long := strings.Repeat("字", 281)
	bad := base
	bad.Reason = long
	if _, _, err := svc.Update(ctx, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("reason 281 字须拒: %v", err)
	}
	bad2 := base
	bad2.RequestID = strings.Repeat("r", 129)
	if _, _, err := svc.Update(ctx, bad2); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("request_id 129 字须拒: %v", err)
	}
}

// TestDispatchSameRequestReplayAfterRebind #10：原请求摘要稳定——换绑后重解析
// 同一请求（决策事实不同/任务已映射）摘要不变，同键投递重放已提交回执而非
// 幂等冲突。
func TestDispatchSameRequestReplayAfterRebind(t *testing.T) {
	svc, _, _, _ := newUpdateFixture(t)
	slots := &fakeSlots{
		exists:    map[string]bool{"si_1": true},
		instances: map[string]string{"si_1": "slot.a"},
		active:    map[string]string{"si_1": "ag_b"},
		specs:     map[string][]string{"slot.a": {"si_1"}},
	}
	svc.lookups.Slots = slots
	ctx := context.Background()
	req := ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()}

	d1, err := svc.Resolve(ctx, req)
	if err != nil || d1.Status != "resolved" || d1.ResolvedAgentID != "ag_b" {
		t.Fatalf("首解析: %+v %v", d1, err)
	}
	rc, replayed, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d1.RouteID, IdempotencyKey: "idem-d10"})
	if err != nil || replayed {
		t.Fatalf("首投递: %+v %v", rc, err)
	}
	// 换绑 → 同一原始请求重解析：摘要不变（Request 未被解析改写）；任务已
	// 映射 → 决策 blocked，但摘要一致。
	slots.active["si_1"] = "ag_c"
	d2, err := svc.Resolve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if d2.RequestDigest != d1.RequestDigest {
		t.Fatalf("同一请求摘要须稳定: %s vs %s", d1.RequestDigest, d2.RequestDigest)
	}
	// 同键投递 → 重放已提交回执（不因决策变化/路由不同变幂等冲突）
	again, replayed2, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "idem-d10"})
	if err != nil || !replayed2 || again.ReceiptID != rc.ReceiptID {
		t.Fatalf("同键重放: %+v replayed=%v %v", again, replayed2, err)
	}
}

// TestDispatchTemplateInstanceDrift #2（读侧面）：模板引用解析时恰一实例；决策
// 后模板长出第二实例 → 投递复核按实例核对失败（slot_ambiguous）→ route_stale。
func TestDispatchTemplateInstanceDrift(t *testing.T) {
	svc, _, _, _ := newUpdateFixture(t)
	slots := &fakeSlots{
		exists:    map[string]bool{"si_1": true, "si_2": true},
		instances: map[string]string{"si_1": "slot.a", "si_2": "slot.a"},
		active:    map[string]string{"si_1": "ag_b"},
		specs:     map[string][]string{"slot.a": {"si_1"}},
	}
	svc.lookups.Slots = slots
	ctx := context.Background()

	d, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, SlotSpecID: "slot.a",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()})
	if err != nil || d.Status != "resolved" || d.ResolvedInstanceID != "si_1" {
		t.Fatalf("模板恰一实例: %+v %v", d, err)
	}
	// 模板长出第二实例 → 投递复核重解析歧义 → stale（禁止猜选原实例）
	slots.specs["slot.a"] = []string{"si_1", "si_2"}
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d.RouteID, IdempotencyKey: "idem-t1"}); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("模板实例集漂移须 route_stale: %v", err)
	}
}
