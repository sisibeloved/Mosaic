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
	req := ResolveRequest{Kind: KindCreateMission, SlotInstanceID: "si_1",
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()}

	d1, err := svc.Resolve(ctx, req)
	if err != nil || d1.Status != "resolved" {
		t.Fatalf("解析: %+v %v", d1, err)
	}
	// 换绑后旧决策投递 → route_stale（三审 #1：认领**不释放**——并发孪生共享）
	slots.active["si_1"] = "ag_c"
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d1.RouteID, IdempotencyKey: "idem-rc"}); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("换绑拒绝: %v", err)
	}
	// 同键异载荷（不同请求）→ 幂等冲突（键被同摘要 pending 占用——修复前被
	// 释放后可冒用建第二单，三审 #1 串单根因）
	other := req
	other.RoomSource.TaskID = "tsk_b"
	dOther, _ := svc.Resolve(ctx, other)
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: dOther.RouteID, IdempotencyKey: "idem-rc"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("异载荷不得冒用被拒键: %v", err)
	}
	// 同一原始请求重解析（新决策、同摘要）→ 换绑已修复 → 同键重投成功；
	// 首次响应与重放/按键查询同一份（采纳原行 + route 真值）
	d2, err := svc.Resolve(ctx, req)
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
	again, replayed2, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "idem-rc"})
	if err != nil || !replayed2 || again.ReceiptID != rc.ReceiptID || again.RouteID != rc.RouteID {
		t.Fatalf("重放一致性: %+v vs %+v (%v)", again, rc, err)
	}
	persist, found, _ := svc.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, "idem-rc")
	if !found || persist.ReceiptID != rc.ReceiptID || persist.RouteID != d2.RouteID {
		t.Fatalf("按键查询与首次响应同一份: %+v vs %+v", persist, rc)
	}
	if ms, _ := svc.missions.(interface {
		ListMissions(context.Context) ([]mission.Summary, error)
	}); ms != nil {
		if list, _ := ms.ListMissions(ctx); len(list) != 1 {
			t.Fatalf("恰一单: %d", len(list))
		}
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
	// 模拟 b59a395 形态：认领行未完结（reason 空列）
	cmd := UpdateCmd{MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-legacy", RequestID: "req_lg", Contract: c2, Reason: "任务卡追加"}
	claim := Receipt{
		ReceiptID: "dpr_legacy", CallerRef: CallerOwner, Scope: ScopeWorkspace,
		IdempotencyKey: cmd.IdempotencyKey, RequestID: "req_lg",
		RequestDigest: updateDigest(cmd), Reason: "",
		TargetRefs: []string{m.MissionID},
	}
	if _, conflict, finalized, err := svc.store.ClaimKey(ctx, claim); err != nil || conflict || finalized {
		t.Fatalf("播种遗留 pending: %v", err)
	}
	// (a) 任务已前进（v2 由他方提交）→ 同键原载荷重驱动 CAS 败 → 待核验
	if _, err := mSvc.Revise(ctx, m.MissionID, 1, "ag_a", "ag_a", c2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Update(ctx, cmd); !errors.Is(err, ErrLegacyPendingUnresolved) {
		t.Fatalf("遗留 CAS 败须待核验（不冒领）: %v", err)
	}
	// (b) 处置：确认未执行 → release 释放键（digest 核对）→ 同键携当前版本重提成功
	if err := svc.ReleaseClaim(ctx, "idem-legacy", updateDigest(cmd)); err != nil {
		t.Fatalf("release: %v", err)
	}
	cmdOK := cmd
	cmdOK.ExpectedVersion = 2
	rc, replayed, err := svc.Update(ctx, cmdOK)
	if err != nil || replayed {
		t.Fatalf("release 后重提: %+v replayed=%v %v", rc, replayed, err)
	}
	if rc.DomainCommandID != m.MissionID+":revise:v3" {
		t.Fatalf("重提回执: %+v", rc)
	}
	// (c) 新键 CAS 败（等值内容也不冒领——三审 #2 新库面）
	cmdHijack := UpdateCmd{MissionID: m.MissionID, ExpectedVersion: 1,
		IdempotencyKey: "idem-hijack", Contract: c2} // 内容恰为 v2，但 v2 非其所为
	if _, _, err := svc.Update(ctx, cmdHijack); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("新键等值内容须冲突（不冒领）: %v", err)
	}
	if _, found, _ := svc.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, "idem-hijack"); found {
		t.Fatal("新键冲突不得留回执")
	}
}
