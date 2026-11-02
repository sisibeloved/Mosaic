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
	rv, ok, err := store.RequestReview(ctx, m.MissionID, 1, "dlv_1", reviewEv(m.MissionID, `{"to":"reviewing","package_id":"dlv_1"}`, "t1"))
	if err != nil || !ok || rv.Status != mission.StatusReviewing || rv.ReviewPackageID != "dlv_1" {
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
	if _, ok, _ := store.ReviseMission(ctx, m.MissionID, 1, next); ok {
		t.Fatal("reviewing 修订应 ok=false（基线冻结）")
	}
	// PauseMission 不含 reviewing
	if _, ok, _ := store.PauseMission(ctx, m.MissionID, 1, "", "t2c"); ok {
		t.Fatal("reviewing 不可暂停")
	}

	// RecordReview returned → ready + 锚清空
	rt, ok, err := store.RecordReview(ctx, m.MissionID, 1, mission.StatusReady, reviewEv(m.MissionID, `{"decision":"returned"}`, "t3"))
	if err != nil || !ok || rt.Status != mission.StatusReady || rt.ReviewPackageID != "" {
		t.Fatalf("退回: ok=%v err=%v %+v", ok, err, rt)
	}
	// 非 reviewing 裁定 ok=false
	if _, ok, _ := store.RecordReview(ctx, m.MissionID, 1, mission.StatusAccepted, reviewEv(m.MissionID, `{}`, "t3b")); ok {
		t.Fatal("非 reviewing 裁定应 ok=false")
	}
	// 再送验 → accepted（锚保留）
	store.RequestReview(ctx, m.MissionID, 1, "dlv_2", reviewEv(m.MissionID, `{"to":"reviewing","package_id":"dlv_2"}`, "t4"))
	acc, ok, err := store.RecordReview(ctx, m.MissionID, 1, mission.StatusAccepted, reviewEv(m.MissionID, `{"decision":"accepted"}`, "t5"))
	if err != nil || !ok || acc.Status != mission.StatusAccepted || acc.ReviewPackageID != "dlv_2" {
		t.Fatalf("接受: ok=%v err=%v %+v", ok, err, acc)
	}
	// accepted 终态守卫：取消/修订/暂停/送验 ok=false；投影不翻转
	if _, ok, _ := store.MarkCanceling(ctx, m.MissionID, 1, "", "t6"); ok {
		t.Fatal("accepted 不可取消")
	}
	if _, ok, _ := store.ReviseMission(ctx, m.MissionID, 1, next); ok {
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
	store.RecordReview(ctx, "mi_rv", 1, mission.StatusAccepted, reviewEv("mi_rv", `{}`, "t2"))
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
