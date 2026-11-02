//go:build it

// IT 层（ADR-0031 / 0.6.8）：delivery_packages 经真实 SQLite——migration v21 登记、
// JSON 列往返（含空切片 []）、Mission 列表倒序、v0.6.7 真旧库升级。测试先行。

package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/delivery"
)

func seedPackage(id, missionID, runID string, attempt int, at string) delivery.Package {
	return delivery.Package{
		PackageID: id, MissionID: missionID, RunID: runID, Attempt: attempt, Generation: attempt + 10,
		ContractVersion: 2,
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

	p1 := seedPackage("dlv_1", "mi_d", "run_a", 1, "2026-11-01T10:00:00.000Z")
	if err := store.CreatePackage(ctx, p1); err != nil {
		t.Fatalf("建包: %v", err)
	}
	got, ok, err := store.PackageByID(ctx, "dlv_1")
	if err != nil || !ok {
		t.Fatalf("单包: ok=%v err=%v", ok, err)
	}
	if got.RunID != "run_a" || got.Generation != 11 || got.ContractVersion != 2 ||
		len(got.ArtifactRefs) != 2 || len(got.AcceptanceMapping) != 2 || got.AcceptanceMapping[1].Evidence != "" ||
		got.VerificationEvidence != "本地复跑" || len(got.OpenItems) != 1 || len(got.SuggestedMemory) != 1 || got.Notes != "首交" {
		t.Fatalf("往返失真: %+v", got)
	}
	// 空切片往返为 []（非 nil）
	if got.Risks == nil || got.ActionReceipts == nil || len(got.Risks) != 0 {
		t.Fatalf("空切片须为 []: %+v", got)
	}
	// 主键撞键报错
	if err := store.CreatePackage(ctx, p1); err == nil {
		t.Fatal("重复 package_id 应报错")
	}
	// 倒序列表
	p2 := seedPackage("dlv_2", "mi_d", "run_a", 1, "2026-11-01T10:05:00.000Z")
	p3 := seedPackage("dlv_3", "mi_d", "run_b", 2, "2026-11-01T10:10:00.000Z")
	_ = store.CreatePackage(ctx, p2)
	_ = store.CreatePackage(ctx, p3)
	_ = store.CreatePackage(ctx, seedPackage("dlv_other", "mi_e", "run_z", 1, "2026-11-01T10:20:00.000Z"))
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
	if err := store.CreatePackage(context.Background(), seedPackage("dlv_u", "mi_u", "run_u", 1, "t")); err != nil {
		t.Fatalf("升级库建包: %v", err)
	}
}
