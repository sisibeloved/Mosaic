//go:build it

// IT 层（ADR-0019 / 0.5.2）：slot_specs 不可变版本行经真实 SQLite——migration
// v9 建表、v0.5.1 旧库升级、PK 唯一与并发追加连续版本。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/slot"

	_ "modernc.org/sqlite"
)

// sqlOpenRaw 裸开旧库（无 schemaDDL——建 v0.5.1 形态用）。
func sqlOpenRaw(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+strings.ReplaceAll(filepath.ToSlash(path), "?", "%3F")+"?_pragma=busy_timeout(5000)")
}

func validSlotContent() slot.SpecContent {
	return slot.SpecContent{
		Name:           "研究负责人",
		Responsibility: "汇总证据、组织协作，并对研究交付负责",
		Deliverables:   []string{"结论、来源与具名异议"},
	}
}

// TestSlotSpecStoreIT 新库：v9 建表 + 迁移登记 + 建谱系/追加/查询往返。
func TestSlotSpecStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v9 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 9`).Scan(&v9)
	if v9 != 1 {
		t.Fatalf("migration v9 未登记: %d", v9)
	}

	spec, err := store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.research-lead", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "2026-10-29T10:00:00.000Z",
	})
	if err != nil || spec.Version != 1 {
		t.Fatalf("create: %+v %v", spec, err)
	}
	// 谱系碰撞（PK）
	if _, err := store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.research-lead", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "2026-10-29T10:00:01.000Z",
	}); !errors.Is(err, slot.ErrSpecExists) {
		t.Fatalf("碰撞 err = %v, want ErrSpecExists", err)
	}

	c2 := validSlotContent()
	c2.Name = "研究负责人（修订）"
	v2, err := store.AppendVersion(ctx, "slot.research-lead", c2, "2026-10-29T11:00:00.000Z")
	if err != nil || v2.Version != 2 || v2.Name != "研究负责人（修订）" {
		t.Fatalf("append: %+v %v", v2, err)
	}

	latest, ok, err := store.Latest(ctx, "slot.research-lead")
	if err != nil || !ok || latest.Version != 2 {
		t.Fatalf("latest: %v %v %+v", err, ok, latest)
	}
	versions, ok, err := store.Versions(ctx, "slot.research-lead")
	if err != nil || !ok || len(versions) != 2 || versions[0].Version != 1 {
		t.Fatalf("versions: %v %v n=%d", err, ok, len(versions))
	}
	if versions[0].Name != "研究负责人" {
		t.Fatalf("v1 不可变被破: %+v", versions[0])
	}
	sums, err := store.ListSummaries(ctx)
	if err != nil || len(sums) != 1 || sums[0].LatestVersion != 2 || sums[0].VersionCount != 2 {
		t.Fatalf("summaries: %v %+v", err, sums)
	}

	if _, err := store.AppendVersion(ctx, "slot.missing", validSlotContent(), "t"); !errors.Is(err, slot.ErrNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
}

// TestSlotSpecLegacyUpgradeIT v0.5.1 旧库升级：截 v9 段建旧库 → 开库即建
// slot_specs，迁移登记恰一条 v9，重开幂等、写入数据保留。
func TestSlotSpecLegacyUpgradeIT(t *testing.T) {
	marker := "-- v9（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v9 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-legacy-v051.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	db.Close()

	var hasSlots int
	probe, _ := sqlOpenRaw(path)
	probe.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='slot_specs'`).Scan(&hasSlots)
	probe.Close()
	if hasSlots != 0 {
		t.Fatal("升级前不应有 slot_specs 表")
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	ctx := context.Background()
	if _, err := upgraded.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.upgrade", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "2026-10-29T10:00:00.000Z",
	}); err != nil {
		t.Fatalf("升级后写入: %v", err)
	}
	var v9 int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 9`).Scan(&v9)
	if v9 != 1 {
		t.Fatalf("v9 登记数 = %d", v9)
	}
	upgraded.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reopened.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 9`).Scan(&v9)
	if v9 != 1 {
		t.Fatalf("重开后 v9 登记 %d（要恰 1）", v9)
	}
	if _, ok, _ := reopened.Latest(ctx, "slot.upgrade"); !ok {
		t.Fatal("重开后模板丢失")
	}
}

// TestSlotSpecConcurrentAppendIT 并发追加 8 路：版本 2–9 恰各一条（无空洞无
// 重复——BEGIN IMMEDIATE 串行化，ADR-0018 v0.2 #1 同口径）。
func TestSlotSpecConcurrentAppendIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.race", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "2026-10-29T10:00:00.000Z",
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.AppendVersion(ctx, "slot.race", validSlotContent(), "t"); err != nil {
				t.Errorf("并发追加: %v", err)
			}
		}()
	}
	wg.Wait()

	versions, ok, _ := store.Versions(ctx, "slot.race")
	if !ok || len(versions) != 9 {
		t.Fatalf("版本数 = %d, want 9", len(versions))
	}
	seen := map[int]int{}
	for _, v := range versions {
		seen[v.Version]++
	}
	for ver := 1; ver <= 9; ver++ {
		if seen[ver] != 1 {
			t.Fatalf("version %d 出现 %d 次（要恰 1）", ver, seen[ver])
		}
	}
}
