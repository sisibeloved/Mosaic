//go:build it

// IT 层（ADR-0020 / 0.5.3）：slot_instances 经真实 SQLite——migration v10 建表、
// v0.5.2 旧库升级、knowledge_namespace 唯一约束与并发实例全唯一。
package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/slot"
)

// TestSlotInstanceStoreIT 新库：v10 建表 + 迁移登记 + 建实例/列表/单查往返 +
// 命名空间唯一约束实测。
func TestSlotInstanceStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v10 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 10`).Scan(&v10)
	if v10 != 1 {
		t.Fatalf("migration v10 未登记: %d", v10)
	}

	store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.it", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "2026-10-29T10:00:00.000Z",
	})
	if err := store.CreateInstance(ctx, slot.SlotInstance{
		SlotInstanceID: "si_it_1", SlotSpecID: "slot.it", Scope: "workspace",
		Title: "研究负责人", KnowledgeNamespace: "slotns_it_1",
		CreatedFromVersion: 1, CreatedAt: "2026-10-29T11:00:00.000Z",
	}); err != nil {
		t.Fatalf("建实例: %v", err)
	}

	// 命名空间唯一：裸插重复 → 约束错
	if err := store.CreateInstance(ctx, slot.SlotInstance{
		SlotInstanceID: "si_it_2", SlotSpecID: "slot.it", Scope: "workspace",
		Title: "重复命名空间", KnowledgeNamespace: "slotns_it_1",
		CreatedFromVersion: 1, CreatedAt: "t",
	}); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("命名空间重复应撞约束, got %v", err)
	}

	list, err := store.ListInstances(ctx)
	if err != nil || len(list) != 1 || list[0].SlotInstanceID != "si_it_1" {
		t.Fatalf("list: %v n=%d", err, len(list))
	}
	got, ok, err := store.InstanceByID(ctx, "si_it_1")
	if err != nil || !ok || got.KnowledgeNamespace != "slotns_it_1" || got.Title != "研究负责人" {
		t.Fatalf("byid: %v %v %+v", err, ok, got)
	}
	if _, ok, _ := store.InstanceByID(ctx, "si_missing"); ok {
		t.Fatal("未知实例应 ok=false")
	}
}

// TestSlotInstanceLegacyUpgradeIT v0.5.2 旧库升级：截 v10 段建库 → 开库即建
// slot_instances，登记恰一条 v10，重开幂等。
func TestSlotInstanceLegacyUpgradeIT(t *testing.T) {
	marker := "-- v10（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v10 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-legacy-v052.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	db.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	ctx := context.Background()
	upgraded.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.up", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "t",
	})
	if err := upgraded.CreateInstance(ctx, slot.SlotInstance{
		SlotInstanceID: "si_up_1", SlotSpecID: "slot.up", Scope: "workspace",
		Title: "升级实例", KnowledgeNamespace: "slotns_up_1",
		CreatedFromVersion: 1, CreatedAt: "t",
	}); err != nil {
		t.Fatalf("升级后建实例: %v", err)
	}
	var v10 int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 10`).Scan(&v10)
	if v10 != 1 {
		t.Fatalf("v10 登记数 = %d", v10)
	}
	upgraded.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reopened.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 10`).Scan(&v10)
	if v10 != 1 {
		t.Fatalf("重开后 v10 登记 %d（要恰 1）", v10)
	}
	if _, ok, _ := reopened.InstanceByID(ctx, "si_up_1"); !ok {
		t.Fatal("重开后实例丢失")
	}
}

// TestSlotInstanceConcurrentCreateIT 并发建 8 实例：ID 与命名空间全唯一。
func TestSlotInstanceConcurrentCreateIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.race", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "t",
	})

	var mu sync.Mutex
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			counter++
			n := counter
			mu.Unlock()
			if err := store.CreateInstance(ctx, slot.SlotInstance{
				SlotInstanceID: "si_race_" + intName(n), SlotSpecID: "slot.race", Scope: "workspace",
				Title: "并发实例", KnowledgeNamespace: "slotns_race_" + intName(n),
				CreatedFromVersion: 1, CreatedAt: "t",
			}); err != nil {
				t.Errorf("并发建实例: %v", err)
			}
		}()
	}
	wg.Wait()

	list, _ := store.ListInstances(ctx)
	if len(list) != 8 {
		t.Fatalf("实例数 = %d, want 8", len(list))
	}
	ids := map[string]bool{}
	nss := map[string]bool{}
	for _, inst := range list {
		ids[inst.SlotInstanceID] = true
		nss[inst.KnowledgeNamespace] = true
	}
	if len(ids) != 8 || len(nss) != 8 {
		t.Fatalf("唯一性破：ids=%d namespaces=%d", len(ids), len(nss))
	}
}

func intName(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestSlotInstanceListInvariantUnderConcurrencyIT（审校 P2#1 靶向）：并发
// 实例化 + 模板追加 + 列表读取交替施压——每次读取都满足 latest ≥
// created_from_version 且名称非空（先读实例后读模板的顺序保证）。
func TestSlotInstanceListInvariantUnderConcurrencyIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	store.CreateSpec(ctx, slot.SlotSpec{
		SlotSpecID: "slot.inv", Version: 1, SchemaVersion: 1,
		SpecContent: validSlotContent(), CreatedAt: "t",
	})

	var n sync.Mutex
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(3)
		go func() { // 持续追加版本
			defer wg.Done()
			for j := 0; j < 5; j++ {
				store.AppendVersion(ctx, "slot.inv", validSlotContent(), "t")
			}
		}()
		go func() { // 持续实例化
			defer wg.Done()
			for j := 0; j < 5; j++ {
				n.Lock()
				counter++
				k := counter
				n.Unlock()
				store.CreateInstance(ctx, slot.SlotInstance{
					SlotInstanceID: "si_inv_" + intName(k), SlotSpecID: "slot.inv", Scope: "workspace",
					Title: "实例", KnowledgeNamespace: "slotns_inv_" + intName(k),
					CreatedFromVersion: 1, CreatedAt: "t",
				})
			}
		}()
		go func() { // 持续读取断言不变量
			defer wg.Done()
			for j := 0; j < 10; j++ {
				instances, _ := store.ListInstances(ctx)
				specs, _ := store.LatestByLineage(ctx)
				for _, inst := range instances {
					now := specs[inst.SlotSpecID]
					if now.Name == "" {
						t.Errorf("空模板名: %+v", inst)
					}
					// 存储直读无钳制（服务层负责）；此处仅验证"先实例后模板"
					// 的组合口径：现势不旧于创建版本
					if now.LatestVersion < inst.CreatedFromVersion {
						t.Errorf("矛盾快照: inst from v%d, spec latest v%d", inst.CreatedFromVersion, now.LatestVersion)
					}
				}
			}
		}()
	}
	wg.Wait()
}
