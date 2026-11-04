package dispatch

// 0.6.13 复审修复回归（dispatch 面）：#8 拒绝释放认领 + 换路由同键重投回执
// 一致；#11/#12 分步退化面的遗留恢复与 reason 回落。

import (
	"context"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// TestDispatchRejectReleasesAndRerouteConsistency 复审 #8：旧决策占键后因换绑
// 拒绝（认领释放——不占键）→ 新决策同键投递成功 → 首次响应与重放/按键查询
// 读到**同一份**持久化回执（receipt_id/route_id 为实际成功决策）。
func TestDispatchRejectReleasesAndRerouteConsistency(t *testing.T) {
	svc, _, _, agents := newUpdateFixture(t)
	agents.ids["ag_c"] = true
	slots := &fakeSlots{
		exists:    map[string]bool{"si_1": true},
		instances: map[string]string{"si_1": "slot.a"},
		active:    map[string]string{"si_1": "ag_b"},
		specs:     map[string][]string{"slot.a": {"si_1"}},
	}
	svc.lookups.Slots = slots
	ctx := context.Background()

	d1, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()})
	if err != nil || d1.Status != "resolved" {
		t.Fatalf("解析: %+v %v", d1, err)
	}
	// 换绑后旧决策投递 → route_stale（认领须已释放）
	slots.active["si_1"] = "ag_c"
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d1.RouteID, IdempotencyKey: "idem-rc"}); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("换绑拒绝: %v", err)
	}
	if _, found, _ := svc.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, "idem-rc"); found {
		t.Fatal("被拒认领须已释放（公开查询不可见且不占键）")
	}
	// 新决策（寻址到新任职 ag_c、未映射任务卡）同键投递成功
	d2, err := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_b"}, Contract: contract()})
	if err != nil || d2.Status != "resolved" || d2.ResolvedAgentID != "ag_c" {
		t.Fatalf("重解析: %+v %v", d2, err)
	}
	rc, replayed, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "idem-rc"})
	if err != nil || replayed {
		t.Fatalf("同键重投: %+v replayed=%v %v", rc, replayed, err)
	}
	if rc.RouteID != d2.RouteID {
		t.Fatalf("回执 route 须为实际成功决策: %+v", rc)
	}
	// 重放与按键查询与首次响应同一份（持久行即响应行）
	again, replayed2, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "idem-rc"})
	if err != nil || !replayed2 || again.ReceiptID != rc.ReceiptID || again.RouteID != rc.RouteID {
		t.Fatalf("重放一致性: %+v vs %+v (%v)", again, rc, err)
	}
	persist, found, _ := svc.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, "idem-rc")
	if !found || persist.ReceiptID != rc.ReceiptID || persist.RouteID != d2.RouteID {
		t.Fatalf("按键查询与首次响应同一份: %+v vs %+v", persist, rc)
	}
}

// TestUpdateLegacyPendingRecovery 复审 #11/#12（分步退化面）：v30 形态遗留
// pending 认领（reason 空列）+ 修订确已提交（任务恰 v(expected+1) 且内容与
// 载荷等值）→ 重驱动完结回执而非版本冲突；reason 以命令值回落；不等值情形
// 保持冲突且认领保留（不删除可能已提交的键）。
func TestUpdateLegacyPendingRecovery(t *testing.T) {
	svc, mSvc, _, _ := newUpdateFixture(t)
	ctx := context.Background()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, "ag_a", contract(), &src)
	if err != nil {
		t.Fatal(err)
	}
	c2 := contract()
	c2.Objective = "遗留恢复载荷"
	// 模拟 b59a395 形态：修订已提交（v2）+ 认领行未完结（reason 空列）
	up2, err := mSvc.Revise(ctx, m.MissionID, 1, "ag_a", "ag_a", c2)
	if err != nil {
		t.Fatal(err)
	}
	cmd := UpdateCmd{MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-legacy", RequestID: "req_lg", Contract: c2, Reason: "任务卡追加"}
	claim := Receipt{
		ReceiptID: "dpr_legacy", CallerRef: CallerOwner, Scope: ScopeWorkspace,
		IdempotencyKey: cmd.IdempotencyKey, RequestID: "req_lg",
		RequestDigest: updateDigest(cmd), Reason: "", // v30 无 reason 列——升级后空
		TargetRefs: []string{m.MissionID},
	}
	if _, conflict, finalized, err := svc.store.ClaimKey(ctx, claim); err != nil || conflict || finalized {
		t.Fatalf("播种遗留 pending: %v", err)
	}
	// 同键重驱动（expected=1 对 v2 必 CAS 败）→ 内容等值恢复：完结 v2 回执
	rc, replayed, err := svc.Update(ctx, cmd)
	if err != nil || replayed {
		t.Fatalf("遗留恢复: %+v replayed=%v %v", rc, replayed, err)
	}
	if rc.DomainCommandID != up2.MissionID+":revise:v2" || rc.ReceiptID != "dpr_legacy" {
		t.Fatalf("恢复回执: %+v", rc)
	}
	if rc.Reason != "任务卡追加" {
		t.Fatalf("reason 须以命令值回落（#12——空列不得抹掉输入）: %+v", rc)
	}
	// 不等值情形：v3 已是他人内容 → 冲突透传（认领保留——不宣告未知结果成功）
	c3 := contract()
	c3.Objective = "他人载荷"
	if _, err := mSvc.Revise(ctx, m.MissionID, 2, "ag_a", "ag_a", c3); err != nil {
		t.Fatal(err)
	}
	cmd2 := UpdateCmd{MissionID: m.MissionID, ExpectedVersion: 2,
		IdempotencyKey: "idem-legacy2", Contract: c2} // 内容与 v3 不等值
	if _, _, err := svc.Update(ctx, cmd2); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("不等值须冲突: %v", err)
	}
}
