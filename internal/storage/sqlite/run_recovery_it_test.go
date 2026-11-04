//go:build it

// IT 层（ADR-0030 / 0.6.7）：migration v20 经真实 SQLite——runs 处置三列 +
// missions.waiting_reason 列往返、ResolveRun 条件更新、AppendRunEvents 事件追加
// 不重写行、ListUnresolvedUnknown、waiting 态的 MarkWaiting/TransitionStatus 清
// 原因/PauseMission 保留/ResumeMission 回 waiting、v0.6.6 真旧库升级。测试先行。

package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

// TestRunResolutionStoreIT v20 登记 + 处置列往返 + 条件处置 + 事件追加 + 未处置枚举。
func TestRunResolutionStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v20 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 20`).Scan(&v20)
	if v20 != 1 {
		t.Fatalf("migration v20 未登记: %d", v20)
	}

	// 播种 unknown 终态 Run（恢复语义——UpdateRun 全行含处置三列往返）
	r, err := store.CreateRun(ctx, seedRun("run_u1", "mi_u", 0, missionrun.StateRunning), 0, 0, "")
	if err != nil {
		t.Fatalf("首建: %v", err)
	}
	r.State = missionrun.StateUnknown
	r.ErrorText = "进程重启时在执行——结果未知"
	r.FinishedAt = "2026-11-01T10:01:00.000000000Z"
	r.UpdatedAt = r.FinishedAt
	if err := store.UpdateRun(ctx, r, []missionrun.Event{{
		RunID: r.RunID, EventType: missionrun.EventUnknown, OccurredAt: r.FinishedAt,
		Payload: map[string]any{"recovery": true},
	}}); err != nil {
		t.Fatalf("unknown 终态: %v", err)
	}
	// unknown 出未终态口径 + 入未处置枚举
	if unfinished, _ := store.ListUnfinished(ctx); len(unfinished) != 0 {
		t.Fatalf("unknown 属终态: %+v", unfinished)
	}
	unresolved, err := store.ListUnresolvedUnknown(ctx)
	if err != nil || len(unresolved) != 1 || unresolved[0].RunID != "run_u1" {
		t.Fatalf("未处置枚举: %+v %v", unresolved, err)
	}

	// 条件处置：非 unknown 行 ok=false；unknown 且未处置 → 落三列
	r2, _ := store.CreateRun(ctx, seedRun("run_u2", "mi_u", 0, missionrun.StateRunning), 0, 0, "")
	r2.State = missionrun.StateCompleted
	r2.FinishedAt = "t"
	_ = store.UpdateRun(ctx, r2, nil)
	if _, ok, err := store.ResolveRun(ctx, "run_u2", missionrun.ResolutionDiscarded, "", "t"); err != nil || ok {
		t.Fatalf("非 unknown 处置应 ok=false: ok=%v err=%v", ok, err)
	}
	got, ok, err := store.ResolveRun(ctx, "run_u1", missionrun.ResolutionVerifiedCompleted, "核对产出", "2026-11-01T10:05:00.000000000Z")
	if err != nil || !ok {
		t.Fatalf("处置: ok=%v err=%v", ok, err)
	}
	if got.State != missionrun.StateUnknown || got.Resolution != missionrun.ResolutionVerifiedCompleted ||
		got.ResolutionNote != "核对产出" || got.ResolvedAt == "" {
		t.Fatalf("处置往返: %+v", got)
	}
	// 已处置再处置 → ok=false（幂等/拒绝由服务层分辨）
	if _, ok, _ := store.ResolveRun(ctx, "run_u1", missionrun.ResolutionDiscarded, "", "t2"); ok {
		t.Fatal("已处置不可再写")
	}
	if unresolved, _ := store.ListUnresolvedUnknown(ctx); len(unresolved) != 0 {
		t.Fatalf("处置后未处置枚举须归零: %+v", unresolved)
	}
	// 未知 Run 处置 → ErrRunNotFound
	if _, _, err := store.ResolveRun(ctx, "run_none", missionrun.ResolutionDiscarded, "", "t"); err == nil {
		t.Fatal("未知 Run 处置应报错")
	}

	// 事件追加不重写行：AppendRunEvents 后行字段原样
	before, _, _ := store.RunByID(ctx, "run_u1")
	if err := store.AppendRunEvents(ctx, "run_u1", []missionrun.Event{{
		RunID: "run_u1", EventType: missionrun.EventLateResult, OccurredAt: "t3",
		Payload: map[string]any{"outcome": "ok", "published": false},
	}}); err != nil {
		t.Fatalf("事件追加: %v", err)
	}
	after, _, _ := store.RunByID(ctx, "run_u1")
	if after != before {
		t.Fatalf("追加事件不得改行: %+v vs %+v", before, after)
	}
	evs, _ := store.RunEvents(ctx, "run_u1")
	types := make([]string, 0, len(evs))
	for _, e := range evs {
		types = append(types, e.EventType)
	}
	if strings.Join(types, ",") != "attempt_requested,unknown,resolved,late_result" {
		t.Fatalf("事件序: %v", types)
	}
}

// TestMissionWaitingStoreIT waiting_reason 列：MarkWaiting 落原因；TransitionStatus
// 出 waiting 清原因；PauseMission 保留；ResumeMission 回 waiting 目标。
func TestMissionWaitingStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_it"), nil)
	if err != nil {
		t.Fatalf("建契约: %v", err)
	}
	ev := func(at string) mission.Event {
		return mission.Event{MissionID: m.MissionID, EventType: mission.EventStatusChanged, Payload: []byte(`{}`), OccurredAt: at}
	}
	if changed, _ := store.TransitionStatus(ctx, m.MissionID, mission.StatusReady, mission.StatusRunning, ev("t1")); !changed {
		t.Fatal("预置 running")
	}
	// MarkWaiting：仅 running；落原因
	changed, err := store.MarkWaiting(ctx, m.MissionID, mission.StatusRunning, "reconciliation", ev("t2"))
	if err != nil || !changed {
		t.Fatalf("进 waiting: %v %v", changed, err)
	}
	got, _, _ := store.MissionByID(ctx, m.MissionID)
	if got.Status != mission.StatusWaiting || got.WaitingReason != "reconciliation" {
		t.Fatalf("waiting 往返: %+v", got)
	}
	if changed, _ := store.MarkWaiting(ctx, m.MissionID, mission.StatusRunning, "x", ev("t2b")); changed {
		t.Fatal("非 running 不可进 waiting")
	}
	// 摘要列表携 waiting
	sums, _ := store.ListMissionsByStatus(ctx, mission.StatusWaiting)
	if len(sums) != 1 {
		t.Fatalf("按状态枚举 waiting: %+v", sums)
	}
	// 暂停保留原因
	paused, ok, err := store.PauseMission(ctx, m.MissionID, 1, "", "t3")
	if err != nil || !ok || paused.Status != mission.StatusPaused || paused.WaitingReason != "reconciliation" {
		t.Fatalf("waiting → paused 保留原因: ok=%v err=%v %+v", ok, err, paused)
	}
	// 恢复回 waiting（服务层按 reason 定 target；存储照目标落）
	resumed, ok, err := store.ResumeMission(ctx, m.MissionID, 1, mission.StatusWaiting, "t4")
	if err != nil || !ok || resumed.Status != mission.StatusWaiting || resumed.WaitingReason != "reconciliation" {
		t.Fatalf("恢复回 waiting: ok=%v err=%v %+v", ok, err, resumed)
	}
	// 出 waiting 清原因
	if changed, _ := store.TransitionStatus(ctx, m.MissionID, mission.StatusWaiting, mission.StatusReady, ev("t5")); !changed {
		t.Fatal("出 waiting")
	}
	got, _, _ = store.MissionByID(ctx, m.MissionID)
	if got.Status != mission.StatusReady || got.WaitingReason != "" {
		t.Fatalf("出 waiting 须清原因: %+v", got)
	}
}

// TestV20LegacyUpgradeIT v0.6.6 真旧库（schemaDDL + v18 + v19，无 v20 列）→ Open
// 自动加列并登记 20；旧行处置列缺省空。
func TestV20LegacyUpgradeIT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v066.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	for _, ddl := range []string{schemaDDL, migrationV18, migrationV19} {
		if _, err := db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			t.Fatalf("legacy ddl: %v", err)
		}
	}
	// 旧行（v19 形状——无处置列）
	if _, err := db.Exec(`INSERT INTO runs (run_id, mission_id, attempt, lease_id, manifest_id, runtime_id, binding_id, agent_id, state, fence_token, generation, requested_at, updated_at)
VALUES ('run_old', 'mi_old', 1, 'l', 'm', 'r', 'b', 'a', 'failed', 'f', 1, 't', 't')`); err != nil {
		db.Close()
		t.Fatalf("legacy row: %v", err)
	}
	db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧库升级打开: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	var v20 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 20`).Scan(&v20)
	if v20 != 1 {
		t.Fatalf("升级后 v20 应登记: %d", v20)
	}
	old, ok, err := store.RunByID(ctx, "run_old")
	if err != nil || !ok || old.Resolution != "" || old.ResolvedAt != "" {
		t.Fatalf("旧行处置列缺省空: ok=%v err=%v %+v", ok, err, old)
	}
	m, _ := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_it"), nil)
	if got, _, _ := store.MissionByID(ctx, m.MissionID); got.WaitingReason != "" {
		t.Fatalf("waiting_reason 缺省空: %+v", got)
	}
	// 再次打开幂等（重复加列容忍）
	store.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("二次打开: %v", err)
	}
	again.Close()
}

// TestLegacyResidueConvergedIT 0.6.7 发布前补齐：旧库（0.6.5/0.6.6）崩溃残留
// 终态行（failed+结果未知）经真实 SQLite 枚举收敛——ListLegacyResidues 前缀
// 精确命中（停止路径 canceled"结果未知"文案不混淆）。
func TestLegacyResidueConvergedIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateMission(ctx, missionRow(mission.StatusRunning, "ag_it"), nil); err != nil {
		t.Fatalf("建契约: %v", err)
	}
	const mi = "mi_it_1"
	// 旧库残留行 + 停止路径 canceled 行（不同文案，不得命中）
	legacy, err := store.CreateRun(ctx, seedRun("run_lg", mi, 0, missionrun.StateRunning), 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	legacy.State = missionrun.StateFailed
	legacy.ErrorText = "进程重启时在执行——结果未知；请确认后手动再执行（不会自动重跑）"
	legacy.FinishedAt = "t1"
	if err := store.UpdateRun(ctx, legacy, nil); err != nil {
		t.Fatal(err)
	}
	stopped, err := store.CreateRun(ctx, seedRun("run_st", "mi_it_2", 0, missionrun.StateRunning), 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	stopped.State = missionrun.StateCanceled
	stopped.ErrorText = "停止请求在途时进程重启——结果未知"
	stopped.FinishedAt = "t1"
	if err := store.UpdateRun(ctx, stopped, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListLegacyResidues(ctx, "进程重启时在执行——结果未知")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != "run_lg" {
		t.Fatalf("前缀须恰命中旧残留行: %+v", got)
	}
}
