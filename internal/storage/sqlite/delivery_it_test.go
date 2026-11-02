//go:build it

// IT 层（ADR-0031 / 0.6.8）：delivery_packages 经真实 SQLite——migration v21 登记、
// JSON 列往返（含空切片 []）、Mission 列表倒序、v0.6.7 真旧库升级。测试先行。

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

func seedPackage(id, missionID, runID string, attempt int, at string) delivery.Package {
	return delivery.Package{
		PackageID: id, MissionID: missionID, RunID: runID, Attempt: attempt, Generation: attempt + 10,
		ContractVersion: 1,
		ArtifactRefs:    []string{"workspace://out/a.md", "doc:doc_x"},
		AcceptanceMapping: []delivery.MappingEntry{
			{Criterion: "七端点全绿", Evidence: "CI 绿"},
			{Criterion: "CAS 冲突可见", Evidence: ""},
		},
		VerificationEvidence: "本地复跑",
		OpenItems:            []string{"待核 Windows"},
		Risks:                []string{},
		ActionReceipts:       []string{},
		SuggestedMemory:      []string{"记：路径分隔符"},
		Notes:                "首交",
		SubmittedAt:          at,
	}
}

// mkMission 建 Mission 行（守卫需真实 missions 行：版本 1 / 给定状态）。
func mkMission(t *testing.T, store *Store, id, status string) {
	t.Helper()
	row := missionRow(status, "ag_it")
	row.MissionID = id
	if _, err := store.CreateMission(context.Background(), row, nil); err != nil {
		t.Fatalf("建 Mission %s: %v", id, err)
	}
}

// receiptOf 回执（与包同事务落来源 Run 流）。
func receiptOf(p delivery.Package) delivery.Receipt {
	return delivery.Receipt{RunID: p.RunID, EventType: delivery.EventDeliverySubmitted,
		Payload: map[string]any{"package_id": p.PackageID}}
}

// TestDeliveryStoreIT v21 登记 + 往返 + 倒序 + 未知。
func TestDeliveryStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v21 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 21`).Scan(&v21)
	if v21 != 1 {
		t.Fatalf("migration v21 未登记: %d", v21)
	}

	mkMission(t, store, "mi_d", mission.StatusReady)
	mkMission(t, store, "mi_e", mission.StatusReady)
	p1 := seedPackage("dlv_1", "mi_d", "run_a", 1, "2026-11-01T10:00:00.000Z")
	if err := store.CreatePackage(ctx, p1, receiptOf(p1)); err != nil {
		t.Fatalf("建包: %v", err)
	}
	got, ok, err := store.PackageByID(ctx, "dlv_1")
	if err != nil || !ok {
		t.Fatalf("单包: ok=%v err=%v", ok, err)
	}
	if got.RunID != "run_a" || got.Generation != 11 || got.ContractVersion != 1 ||
		len(got.ArtifactRefs) != 2 || len(got.AcceptanceMapping) != 2 || got.AcceptanceMapping[1].Evidence != "" ||
		got.VerificationEvidence != "本地复跑" || len(got.OpenItems) != 1 || len(got.SuggestedMemory) != 1 || got.Notes != "首交" {
		t.Fatalf("往返失真: %+v", got)
	}
	// 空切片往返为 []（非 nil）
	if got.Risks == nil || got.ActionReceipts == nil || len(got.Risks) != 0 {
		t.Fatalf("空切片须为 []: %+v", got)
	}
	// 主键撞键报错
	if err := store.CreatePackage(ctx, p1, receiptOf(p1)); err == nil {
		t.Fatal("重复 package_id 应报错")
	}
	// 回执与包同事务：来源 Run 流有 delivery_submitted
	evs, _ := store.RunEvents(ctx, "run_a")
	if len(evs) != 1 || evs[0].EventType != delivery.EventDeliverySubmitted || evs[0].Payload["package_id"] != "dlv_1" {
		t.Fatalf("回执须同事务落 Run 流（恰一条——撞键重试不重复）: %+v", evs)
	}
	// 倒序列表
	p2 := seedPackage("dlv_2", "mi_d", "run_a", 1, "2026-11-01T10:05:00.000Z")
	p3 := seedPackage("dlv_3", "mi_d", "run_b", 2, "2026-11-01T10:10:00.000Z")
	_ = store.CreatePackage(ctx, p2, receiptOf(p2))
	_ = store.CreatePackage(ctx, p3, receiptOf(p3))
	po := seedPackage("dlv_other", "mi_e", "run_z", 1, "2026-11-01T10:20:00.000Z")
	_ = store.CreatePackage(ctx, po, receiptOf(po))
	list, err := store.ListPackagesByMission(ctx, "mi_d")
	if err != nil || len(list) != 3 || list[0].PackageID != "dlv_3" || list[2].PackageID != "dlv_1" {
		t.Fatalf("倒序列表: %+v %v", list, err)
	}
	if _, ok, _ := store.PackageByID(ctx, "dlv_none"); ok {
		t.Fatal("未知包应 ok=false")
	}
	if list, _ := store.ListPackagesByMission(ctx, "mi_none"); len(list) != 0 {
		t.Fatalf("无包 Mission 应空列表: %+v", list)
	}
}

// TestV21LegacyUpgradeIT v0.6.7 真旧库（至 v20，无 delivery_packages）→ Open 建表并登记 21。
func TestV21LegacyUpgradeIT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v067.db")
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
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧库升级打开: %v", err)
	}
	defer store.Close()
	var v21 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 21`).Scan(&v21)
	if v21 != 1 {
		t.Fatalf("升级后 v21 应登记: %d", v21)
	}
	mkMission(t, store, "mi_u", mission.StatusReady)
	pu := seedPackage("dlv_u", "mi_u", "run_u", 1, "2026-11-01T10:00:00Z")
	if err := store.CreatePackage(context.Background(), pu, receiptOf(pu)); err != nil {
		t.Fatalf("升级库建包: %v", err)
	}
}

// TestDeliveryGuardInTxIT 审校 #1：写包事务内复核 missions 行——契约版本不符 →
// ErrVersionConflict；已取消 → ErrMissionCanceled；未知 → ErrMissionNotFound；
// 均不落包、不记回执。
func TestDeliveryGuardInTxIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	mkMission(t, store, "mi_g", mission.StatusReady)

	stale := seedPackage("dlv_stale", "mi_g", "run_g", 1, "2026-11-01T10:00:00Z")
	stale.ContractVersion = 2 // 读到 v2 但库里仍 v1（或已修订为其他版本）
	if err := store.CreatePackage(ctx, stale, receiptOf(stale)); !errors.Is(err, delivery.ErrVersionConflict) {
		t.Fatalf("版本不符须冲突: %v", err)
	}
	unknown := seedPackage("dlv_unk", "mi_none", "run_g", 1, "2026-11-01T10:00:00Z")
	if err := store.CreatePackage(ctx, unknown, receiptOf(unknown)); !errors.Is(err, delivery.ErrMissionNotFound) {
		t.Fatalf("未知 Mission: %v", err)
	}
	// 取消后拒绝
	if _, ok, _ := store.MarkCanceling(ctx, "mi_g", 1, "", "t1"); !ok {
		t.Fatal("预置 canceling")
	}
	if _, ok, _ := store.FinishCancel(ctx, "mi_g", "", "t2"); !ok {
		t.Fatal("预置 canceled")
	}
	canceled := seedPackage("dlv_c", "mi_g", "run_g", 1, "2026-11-01T10:00:00Z")
	if err := store.CreatePackage(ctx, canceled, receiptOf(canceled)); !errors.Is(err, delivery.ErrMissionCanceled) {
		t.Fatalf("已取消须拒绝: %v", err)
	}
	if list, _ := store.ListPackagesByMission(ctx, "mi_g"); len(list) != 0 {
		t.Fatalf("守卫拒绝不得落包: %+v", list)
	}
	if evs, _ := store.RunEvents(ctx, "run_g"); len(evs) != 0 {
		t.Fatalf("守卫拒绝不得记回执: %+v", evs)
	}
}

// TestDeliveryListRealTimeOrderIT 审校 #3：RFC3339Nano 小数位可变——须按真实
// 时间倒序（字符串序会把 ".1Z" 排在更晚的 ".11Z" 之前）。
func TestDeliveryListRealTimeOrderIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	mkMission(t, store, "mi_t", mission.StatusReady)
	for id, at := range map[string]string{
		"dlv_a": "2026-11-01T12:00:00.1Z",
		"dlv_b": "2026-11-01T12:00:00.011Z",
		"dlv_c": "2026-11-01T12:00:00.11Z",
	} {
		p := seedPackage(id, "mi_t", "run_t", 1, at)
		if err := store.CreatePackage(ctx, p, receiptOf(p)); err != nil {
			t.Fatalf("建包 %s: %v", id, err)
		}
	}
	list, err := store.ListPackagesByMission(ctx, "mi_t")
	if err != nil || len(list) != 3 {
		t.Fatalf("列表: %v %+v", err, list)
	}
	if list[0].PackageID != "dlv_c" || list[1].PackageID != "dlv_a" || list[2].PackageID != "dlv_b" {
		t.Fatalf("须按真实时间倒序（.11Z > .1Z > .011Z）: %s %s %s", list[0].PackageID, list[1].PackageID, list[2].PackageID)
	}
}
