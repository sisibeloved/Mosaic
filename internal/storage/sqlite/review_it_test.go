//go:build it

// IT 层（ADR-0032 / 0.6.9）：migration v22 review_package_id 经真实 SQLite——
// RequestReview/RecordReview CAS 与锚往返、验收期间修订冻结、accepted 终态守卫、
// delivery 守卫扩 reviewing/accepted、v0.6.8 真旧库升级。测试先行。

package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/delivery"
	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

func reviewEv(missionID, payload, at string) mission.Event {
	return mission.Event{MissionID: missionID, EventType: mission.EventStatusChanged, Payload: []byte(payload), OccurredAt: at}
}

// TestMissionReviewStoreIT v22 登记 + 送验/裁定 CAS + 锚往返 + 冻结/终态守卫。
func TestMissionReviewStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	var v22 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 22`).Scan(&v22)
	if v22 != 1 {
		t.Fatalf("migration v22 未登记: %d", v22)
	}
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_it"), nil)
	if err != nil {
		t.Fatalf("建契约: %v", err)
	}
	if got, _, _ := store.MissionByID(ctx, m.MissionID); got.ReviewPackageID != "" {
		t.Fatalf("锚缺省空: %+v", got)
	}

	// RequestReview：版本不符 ok=false；ready → reviewing + 锚
	if _, ok, _ := store.RequestReview(ctx, m.MissionID, 9, "dlv_1", reviewEv(m.MissionID, `{}`, "t1")); ok {
		t.Fatal("版本不符送验应 ok=false")
	}
	// 审校 #2：未决 Run（在途/未处置 unknown）事务内拦送验
	if _, err := store.CreateRun(ctx, seedRun("run_inflight", m.MissionID, 0, "running"), 0, 0); err != nil {
		t.Fatalf("播种在途 Run: %v", err)
	}
	if _, ok, err := store.RequestReview(ctx, m.MissionID, 1, "dlv_1", reviewEv(m.MissionID, `{}`, "t0")); err == nil || !errors.Is(err, mission.ErrUnsettledRuns) || ok {
		t.Fatalf("未决 Run 须拦送验: ok=%v err=%v", ok, err)
	}
	// 清在途（终态化）→ 放行
	seedRunRow, _, _ := store.RunByID(ctx, "run_inflight")
	seedRunRow.State = "completed"
	seedRunRow.FinishedAt = "t0b"
	if err := store.UpdateRun(ctx, seedRunRow, nil); err != nil {
		t.Fatalf("终态化: %v", err)
	}
	rv, ok, err := store.RequestReview(ctx, m.MissionID, 1, "dlv_1", reviewEv(m.MissionID, `{"to":"reviewing","package_id":"dlv_1"}`, "t1"))
	if err != nil || !ok || rv.Status != mission.StatusReviewing || rv.ReviewPackageID != "dlv_1" || rv.ReviewRound != 1 {
		t.Fatalf("送验: ok=%v err=%v %+v", ok, err, rv)
	}
	// reviewing 上再送验 ok=false（服务层幂等先读）
	if _, ok, _ := store.RequestReview(ctx, m.MissionID, 1, "dlv_2", reviewEv(m.MissionID, `{}`, "t2")); ok {
		t.Fatal("非 ready 送验应 ok=false")
	}
	// 验收期间修订冻结（存储事务内守卫）
	next := m
	next.ContractVersion = 2
	next.UpdatedAt = "t2b"
	if _, ok, _ := store.ReviseMission(ctx, m.MissionID, 1, "ag_it", next); ok {
		t.Fatal("reviewing 修订应 ok=false（基线冻结）")
	}
	// PauseMission 不含 reviewing
	if _, ok, _ := store.PauseMission(ctx, m.MissionID, 1, "", "t2c"); ok {
		t.Fatal("reviewing 不可暂停")
	}

	// RecordReview returned → ready + 锚清空
	// 轮次不符（旧轮次）ok=false
	if _, ok, _ := store.RecordReview(ctx, m.MissionID, 1, 0, mission.StatusReady, reviewEv(m.MissionID, `{}`, "t2b"), "dlv_1"); ok {
		t.Fatal("旧轮次裁定应 ok=false")
	}
	rt, ok, err := store.RecordReview(ctx, m.MissionID, 1, 1, mission.StatusReady, reviewEv(m.MissionID, `{"decision":"returned"}`, "t3"), "dlv_1")
	if err != nil || !ok || rt.Status != mission.StatusReady || rt.ReviewPackageID != "" || rt.ReviewRound != 1 {
		t.Fatalf("退回: ok=%v err=%v %+v", ok, err, rt)
	}
	// 非 reviewing 裁定 ok=false
	if _, ok, _ := store.RecordReview(ctx, m.MissionID, 1, 2, mission.StatusAccepted, reviewEv(m.MissionID, `{}`, "t3b"), "dlv_x"); ok {
		t.Fatal("非 reviewing 裁定应 ok=false")
	}
	// 再送验 → accepted（锚保留；轮次递增为 2——同包重审也递增）
	rv2, okR, errR := store.RequestReview(ctx, m.MissionID, 1, "dlv_2", reviewEv(m.MissionID, `{"to":"reviewing","package_id":"dlv_2"}`, "t4"))
	if errR != nil || !okR || rv2.ReviewRound != 2 {
		t.Fatalf("次轮送验: ok=%v err=%v round=%d", okR, errR, rv2.ReviewRound)
	}
	acc, ok, err := store.RecordReview(ctx, m.MissionID, 1, 2, mission.StatusAccepted, reviewEv(m.MissionID, `{"decision":"accepted"}`, "t5"), "dlv_2")
	if err != nil || !ok || acc.Status != mission.StatusAccepted || acc.ReviewPackageID != "dlv_2" || acc.ReviewRound != 2 {
		t.Fatalf("接受: ok=%v err=%v %+v", ok, err, acc)
	}
	// accepted 终态守卫：取消/修订/暂停/送验 ok=false；投影不翻转
	if _, ok, _ := store.MarkCanceling(ctx, m.MissionID, 1, "", "t6"); ok {
		t.Fatal("accepted 不可取消")
	}
	if _, ok, _ := store.ReviseMission(ctx, m.MissionID, 1, "ag_it", next); ok {
		t.Fatal("accepted 不可修订")
	}
	if _, ok, _ := store.PauseMission(ctx, m.MissionID, 1, "", "t6"); ok {
		t.Fatal("accepted 不可暂停")
	}
	if _, ok, _ := store.RequestReview(ctx, m.MissionID, 1, "dlv_3", reviewEv(m.MissionID, `{}`, "t6")); ok {
		t.Fatal("accepted 不可再送验")
	}
	if changed, _ := store.TransitionStatus(ctx, m.MissionID, mission.StatusReady, mission.StatusRunning, reviewEv(m.MissionID, `{}`, "t7")); changed {
		t.Fatal("accepted 不可被调度投影")
	}
	sums, _ := store.ListMissionsByStatus(ctx, mission.StatusAccepted)
	if len(sums) != 1 {
		t.Fatalf("按状态枚举 accepted: %+v", sums)
	}
	// 事件链含送验/退回/送验/接受
	evs, _, _ := store.MissionEvents(ctx, m.MissionID)
	var tos []string
	for _, e := range evs {
		if e.EventType == mission.EventStatusChanged {
			tos = append(tos, string(e.Payload))
		}
	}
	if len(tos) != 4 || !strings.Contains(tos[3], "accepted") {
		t.Fatalf("事件链: %v", tos)
	}
}

// TestDeliveryGuardReviewingAcceptedIT delivery 写包守卫扩 reviewing / accepted。
func TestDeliveryGuardReviewingAcceptedIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	mkMission(t, store, "mi_rv", mission.StatusReady)
	store.RequestReview(ctx, "mi_rv", 1, "dlv_x", reviewEv("mi_rv", `{}`, "t1"))
	p := seedPackage("dlv_in_review", "mi_rv", "run_rv", 1, "2026-11-02T10:00:00Z")
	if err := store.CreatePackage(ctx, p, receiptOf(p)); !errors.Is(err, delivery.ErrMissionReviewing) {
		t.Fatalf("reviewing 拒绝提交: %v", err)
	}
	store.RecordReview(ctx, "mi_rv", 1, 1, mission.StatusAccepted, reviewEv("mi_rv", `{}`, "t2"), "dlv_x")
	p2 := seedPackage("dlv_after_accept", "mi_rv", "run_rv", 1, "2026-11-02T10:01:00Z")
	if err := store.CreatePackage(ctx, p2, receiptOf(p2)); !errors.Is(err, delivery.ErrMissionAccepted) {
		t.Fatalf("accepted 拒绝提交: %v", err)
	}
}

// TestV22LegacyUpgradeIT v0.6.8 真旧库（至 v21，无 review_package_id）→ Open 加列并登记 22。
func TestV22LegacyUpgradeIT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v068.db")
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
	for _, stmt := range migrationV20 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			t.Fatalf("legacy v20: %v", err)
		}
	}
	if _, err := db.Exec(migrationV21); err != nil {
		db.Close()
		t.Fatalf("legacy v21: %v", err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧库升级打开: %v", err)
	}
	defer store.Close()
	var v22 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 22`).Scan(&v22)
	if v22 != 1 {
		t.Fatalf("升级后 v22 应登记: %d", v22)
	}
	m, _ := store.CreateMission(context.Background(), missionRow(mission.StatusReady, "ag_it"), nil)
	if got, _, _ := store.MissionByID(context.Background(), m.MissionID); got.ReviewPackageID != "" {
		t.Fatalf("锚缺省空: %+v", got)
	}
	store.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("二次打开: %v", err)
	}
	again.Close()
}

// TestReviewRecheckR_IT 复审 R1/R10：接受事务内复查未决 Run；裁定核对包锚。
func TestReviewRecheckR_IT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	mkMission(t, store, "mi_rr", mission.StatusReady)

	// R1 反向顺序：送验先提交、Run 后落——CreateRun 事务内核对过期调度拒绝
	store.RequestReview(ctx, "mi_rr", 1, "dlv_rr", reviewEv("mi_rr", `{}`, "t1"))
	if _, err := store.CreateRun(ctx, seedRun("run_rr", "mi_rr", 0, "running"), 0, 0); err == nil {
		t.Fatal("送验后的过期调度 CreateRun 须拒绝（事务内核核对任务状态）")
	}
	// 退回 → ready；在途 Run 存在 → 送验被事务内复核拒绝（正向顺序）
	store.RecordReview(ctx, "mi_rr", 1, 1, mission.StatusReady, reviewEv("mi_rr", `{}`, "t2"), "dlv_rr")
	store.CreateRun(ctx, seedRun("run_rr2", "mi_rr", 0, "running"), 0, 0) // ready 时合法
	if _, ok, err := store.RequestReview(ctx, "mi_rr", 1, "dlv_rr2", reviewEv("mi_rr", `{}`, "t3")); err == nil && ok {
		t.Fatal("在途 Run 未决时送验须拒绝")
	}
	// 终态化 run_rr2（模拟已核验）→ 送验放行；reviewing 下 CreateRun 拒绝；接受放行
	rr2, _, _ := store.RunByID(ctx, "run_rr2")
	rr2.State = "completed"
	rr2.FinishedAt = "t4b"
	store.UpdateRun(ctx, rr2, nil)
	if _, ok, err := store.RequestReview(ctx, "mi_rr", 1, "dlv_rr2", reviewEv("mi_rr", `{}`, "t5")); err != nil || !ok {
		t.Fatalf("未决清零后送验: ok=%v err=%v", ok, err)
	}
	if _, err := store.CreateRun(ctx, seedRun("run_rr3", "mi_rr", 0, "running"), 0, 0); err == nil {
		t.Fatal("reviewing 下 CreateRun 须拒绝")
	}
	if _, ok, err := store.RecordReview(ctx, "mi_rr", 1, 2, mission.StatusAccepted, reviewEv("mi_rr", `{}`, "t6"), "dlv_rr2"); err != nil || !ok {
		t.Fatalf("未决清零后接受: ok=%v err=%v", ok, err)
	}
	// R1 接受复查正例：reviewing + 未处置 unknown 残留（v22 升级形态——送验复核
	// 之前的既有数据）→ 接受在事务内拒绝
	mkMission(t, store, "mi_rr9", mission.StatusReady)
	store.CreateRun(ctx, seedRun("run_rr9", "mi_rr9", 0, "unknown"), 0, 0)
	store.db.Exec(`UPDATE missions SET status = 'reviewing', review_package_id = 'dlv_rr9', review_round = 1 WHERE mission_id = ?`, "mi_rr9")
	if _, ok, err := store.RecordReview(ctx, "mi_rr9", 1, 1, mission.StatusAccepted, reviewEv("mi_rr9", `{}`, "t8"), "dlv_rr9"); err == nil && ok {
		t.Fatal("未决 unknown 残留时接受须拒绝")
	}
	// R10：锚不符（预填另一包）→ ok=false（防预填未来轮次 + 换包交错记错事件）
	mkMission(t, store, "mi_r10", mission.StatusReady)
	store.RequestReview(ctx, "mi_r10", 1, "dlv_a", reviewEv("mi_r10", `{}`, "t1"))
	if _, ok, _ := store.RecordReview(ctx, "mi_r10", 1, 1, mission.StatusAccepted, reviewEv("mi_r10", `{}`, "t2"), "dlv_other"); ok {
		t.Fatal("锚不符裁定须 ok=false")
	}
}

// TestV24LegacyUpgradeIT 复审 R2/R9：v23 形状残留（reviewing 轮次 0 / canceled 带锚）→ v24 补正。
func TestV24LegacyUpgradeIT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v069.db")
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
	for _, stmt := range append(append([]string{}, migrationV20...), migrationV22...) {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			t.Fatalf("legacy v20/22: %v", err)
		}
	}
	if _, err := db.Exec(migrationV21); err != nil {
		db.Close()
		t.Fatalf("legacy v21: %v", err)
	}
	for _, stmt := range migrationV23 {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			t.Fatalf("legacy v23: %v", err)
		}
	}
	// v22 残留形态：reviewing 轮次 0 带锚；canceled 带锚
	if _, err := db.Exec(`INSERT INTO missions (mission_id, contract_version, status, owner_agent_id, requester, objective, content, created_at, updated_at, review_package_id)
VALUES ('mi_rv0', 1, 'reviewing', 'a', 'owner', 'o', '{}', 't', 't', 'dlv_old')`); err != nil {
		db.Close()
		t.Fatalf("seed reviewing: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO missions (mission_id, contract_version, status, owner_agent_id, requester, objective, content, created_at, updated_at, review_package_id)
VALUES ('mi_cx0', 1, 'canceled', 'a', 'owner', 'o', '{}', 't', 't', 'dlv_gone')`); err != nil {
		db.Close()
		t.Fatalf("seed canceled: %v", err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("升级打开: %v", err)
	}
	defer store.Close()
	var v24 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 24`).Scan(&v24)
	if v24 != 1 {
		t.Fatalf("v24 未登记: %d", v24)
	}
	// R2：reviewing 轮次补 1（可裁定：接受成功）
	m, _, _ := store.MissionByID(context.Background(), "mi_rv0")
	if m.ReviewRound != 1 || m.ReviewPackageID != "dlv_old" {
		t.Fatalf("R2 补正: %+v", m)
	}
	if out, ok, err := store.RecordReview(context.Background(), "mi_rv0", 1, 1, mission.StatusAccepted, reviewEv("mi_rv0", `{}`, "t9"), "dlv_old"); err != nil || !ok || out.Status != mission.StatusAccepted {
		t.Fatalf("R2 补正后可接受: ok=%v err=%v", ok, err)
	}
	// R9：canceled 清锚
	c, _, _ := store.MissionByID(context.Background(), "mi_cx0")
	if c.ReviewPackageID != "" {
		t.Fatalf("R9 清锚: %+v", c)
	}
	// 再次打开幂等
	store.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("二次打开: %v", err)
	}
	again.Close()
}

// TestTerminalWriteGuards_IT 复审 R5/R7/R8：终态任务的迟到装配 / 处置 / 租约在写事务内被拒。
func TestTerminalWriteGuards_IT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	// 三审 #5：先在 ready 下成功建 Run（否则 CreateRun 被终态守卫拒——Run 不存在
	// 的"通过"不算数），再转 accepted，断言处置被 ErrMissionTerminal 拒。
	mkMission(t, store, "mi_term", mission.StatusReady)
	if _, err := store.CreateRun(ctx, seedRun("run_term", "mi_term", 0, "unknown"), 0, 0); err != nil {
		t.Fatalf("预置 Run（ready 期）: %v", err)
	}
	store.db.Exec(`UPDATE missions SET status = 'accepted' WHERE mission_id = ?`, "mi_term")

	// R7：处置拒绝（事务内终态——ErrMissionTerminal 而非 ErrRunNotFound）
	if _, ok, err := store.ResolveRun(ctx, "run_term", "discarded", "", "t1"); !errors.Is(err, missionrun.ErrMissionTerminal) || ok {
		t.Fatalf("终态任务处置须 ErrMissionTerminal: ok=%v err=%v", ok, err)
	}
	got, _, _ := store.RunByID(ctx, "run_term")
	if got.Resolution != "" {
		t.Fatalf("处置不得落: %+v", got)
	}

	// R5：装配拒绝（事务内终态）
	man, rec := sampleManifest("cmf_term", "mi_term", "dterm")
	if err := store.CreateManifest(ctx, man, rec); err == nil {
		t.Fatal("终态任务装配须拒绝")
	}

	// R8：租约拒绝（事务内终态）
	lt := activeLease("lse_term", "rtx_t", "env_t", 0, "2026-11-02T12:00:00.000000000Z")
	lt.MissionID = "mi_term"
	if _, err := store.AcquireLease(ctx, lt); err == nil {
		t.Fatal("终态任务租约须拒绝")
	}
	// 对照：ready 任务领取成功（终态不占排他资源——不同运行时避免互斥干扰）
	mkMission(t, store, "mi_ready8", mission.StatusReady)
	lr := activeLease("lse_ok8", "rtx_t2", "env_t2", 0, "2026-11-02T12:00:00.000000000Z")
	lr.MissionID = "mi_ready8"
	if _, err := store.AcquireLease(ctx, lr); err != nil {
		t.Fatalf("ready 任务领取: %v", err)
	}
}
