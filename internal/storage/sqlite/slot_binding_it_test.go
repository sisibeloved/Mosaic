//go:build it

// IT 层（ADR-0021 / 0.5.4）：slot_bindings + binding_events 经真实 SQLite——
// migration v11（含单 active 部分唯一索引）、v0.5.3 旧库升级、并发激活恒 ≤1
// 在任、释放原子性。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/slot"
)

func bindingRow() slot.Binding {
	return slot.Binding{
		BindingID: "bd_it_1", AgentID: "ag_it", SlotInstanceID: "si_it_1",
		SlotSpecVersion: 1, Scope: "workspace", Status: slot.BindingReady,
		CreatedAt: "2026-10-29T10:00:00.000Z", UpdatedAt: "2026-10-29T10:00:00.000Z",
	}
}

// TestSlotBindingStoreIT v11 建表 + 迁移登记 + 转移/释放/事件往返。
func TestSlotBindingStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v11 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 11`).Scan(&v11)
	if v11 != 1 {
		t.Fatalf("migration v11 未登记: %d", v11)
	}

	b, err := store.CreateBinding(ctx, bindingRow(), slot.BindingReady, nil)
	if err != nil || b.Status != slot.BindingReady {
		t.Fatalf("建任职: %+v %v", b, err)
	}
	// 单事务建任职（审校 P2#3）：created + preparing→ready 两事件一次落
	evs, _ := store.BindingEvents(ctx, b.BindingID)
	if len(evs) != 2 || evs[0].EventType != slot.EventCreated {
		t.Fatalf("created 事件: %+v", evs)
	}

	// ready → active（updated_at 推进）
	if _, ok, err := store.TransitionBinding(ctx, b.BindingID,
		[]string{slot.BindingReady}, slot.BindingActive, map[string]string{}, "t1"); err != nil || !ok {
		t.Fatalf("activate: ok=%v err=%v", ok, err)
	}
	got, _, _ := store.BindingByID(ctx, b.BindingID)
	if got.Status != slot.BindingActive || got.UpdatedAt != "t1" {
		t.Fatalf("投影: %+v", got)
	}

	// 释放（两段一事务，含交接备注）
	if _, ok, err := store.ReleaseBinding(ctx, b.BindingID, "交接备注", "t2"); err != nil || !ok {
		t.Fatalf("release: ok=%v err=%v", ok, err)
	}
	got, _, _ = store.BindingByID(ctx, b.BindingID)
	if got.Status != slot.BindingReleased || got.HandoffNote != "交接备注" || got.UpdatedAt != "t2" {
		t.Fatalf("释放投影: %+v", got)
	}
	evs, _ = store.BindingEvents(ctx, b.BindingID)
	if len(evs) != 5 { // created + preparing→ready + ready→active + 两段释放
		t.Fatalf("事件数 %d: %+v", len(evs), evs)
	}

	// 列表过滤
	store.CreateBinding(ctx, slot.Binding{
		BindingID: "bd_it_2", AgentID: "ag_it", SlotInstanceID: "si_it_2",
		SlotSpecVersion: 1, Scope: "workspace", Status: slot.BindingReady,
		CreatedAt: "t", UpdatedAt: "t",
	}, slot.BindingReady, nil)
	only, _ := store.ListBindings(ctx, "si_it_1")
	if len(only) != 1 || only[0].BindingID != "bd_it_1" {
		t.Fatalf("过滤: %+v", only)
	}
	if _, ok, _ := store.BindingByID(ctx, "bd_missing"); ok {
		t.Fatal("未知任职应 ok=false")
	}
}

// TestSlotBindingSingleActiveIndexIT 单 active 部分唯一索引实测：同实例两
// Binding 并发激活（含交替）→ active 恒 ≤1。
func TestSlotBindingSingleActiveIndexIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		store.CreateBinding(ctx, slot.Binding{
			BindingID: "bd_race_" + strconv.Itoa(i), AgentID: "ag_it", SlotInstanceID: "si_race",
			SlotSpecVersion: 1, Scope: "workspace", Status: slot.BindingReady,
			CreatedAt: "t", UpdatedAt: "t",
		}, slot.BindingReady, nil)
	}

	var wg sync.WaitGroup
	activeWins, conflict := 0, 0
	var mu sync.Mutex
	for round := 0; round < 8; round++ {
		wg.Add(2)
		for _, i := range []int{1, 2} {
			go func(i int) {
				defer wg.Done()
				_, _, err := store.TransitionBinding(ctx, "bd_race_"+strconv.Itoa(i),
					[]string{slot.BindingReady}, slot.BindingActive, map[string]string{}, "t")
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					activeWins++
				case errors.Is(err, slot.ErrActiveBindingExists):
					conflict++
				default:
					t.Errorf("并发激活: %v", err)
				}
			}(i)
		}
		// 每轮结束释放胜者，回到双 ready 再赛（交替施压）
		wg.Wait()
		winner := 0
		for _, i := range []int{1, 2} {
			b, ok, _ := store.BindingByID(ctx, "bd_race_"+strconv.Itoa(i))
			if ok && b.Status == slot.BindingActive {
				winner = i
			}
		}
		var activeCount int
		store.db.QueryRow(`SELECT COUNT(*) FROM slot_bindings WHERE slot_instance_id='si_race' AND status='active'`).Scan(&activeCount)
		if activeCount != 1 {
			t.Fatalf("第 %d 轮 active 数 = %d（要恒 1）", round, activeCount)
		}
		if winner > 0 {
			store.ReleaseBinding(ctx, "bd_race_"+strconv.Itoa(winner), "", "t")
			// 重置回 ready 供下轮
			store.db.Exec(`UPDATE slot_bindings SET status='ready' WHERE binding_id=?`, "bd_race_"+strconv.Itoa(winner))
		}
	}
	if activeWins < 8 || conflict < 8 {
		t.Logf("胜者 %d 次、冲突 %d 次（两路竞争交替）", activeWins, conflict)
	}
}

// TestSlotBindingReleaseAtomicIT 释放原子性：预取消上下文整体回滚（不留
// draining、不留半条事件）。
func TestSlotBindingReleaseAtomicIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	b, _ := store.CreateBinding(ctx, bindingRow(), slot.BindingReady, nil)
	store.TransitionBinding(ctx, b.BindingID, []string{slot.BindingReady}, slot.BindingActive, map[string]string{}, "t")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.ReleaseBinding(canceled, b.BindingID, "", "t"); err == nil {
		t.Fatal("取消上下文应报错")
	}
	got, _, _ := store.BindingByID(ctx, b.BindingID)
	if got.Status != slot.BindingActive {
		t.Fatalf("取消回滚后 status=%s, want active", got.Status)
	}
	evs, _ := store.BindingEvents(ctx, b.BindingID)
	if len(evs) != 3 { // created + preparing→ready + ready→active（释放整体回滚）
		t.Fatalf("取消回滚后事件 %d 条, want 3", len(evs))
	}
}

// TestSlotBindingLegacyUpgradeIT v0.5.3 旧库升级：截 v11 段建库 → 表与索引
// 就位、登记恰一条 v11、重开幂等。
func TestSlotBindingLegacyUpgradeIT(t *testing.T) {
	marker := "-- v11（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v11 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-legacy-v053.db")
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
	defer upgraded.Close()
	ctx := context.Background()
	if _, err := upgraded.CreateBinding(ctx, bindingRow(), slot.BindingReady, nil); err != nil {
		t.Fatalf("升级后建任职: %v", err)
	}
	var v11 int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 11`).Scan(&v11)
	if v11 != 1 {
		t.Fatalf("v11 登记数 = %d", v11)
	}
	var idxCount int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='idx_slot_bindings_single_active'`).Scan(&idxCount)
	if idxCount != 1 {
		t.Fatal("部分唯一索引未建")
	}
	upgraded.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reopened.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 11`).Scan(&v11)
	if v11 != 1 {
		t.Fatalf("重开 v11 登记 %d", v11)
	}
	if _, ok, _ := reopened.BindingByID(ctx, "bd_it_1"); !ok {
		t.Fatal("重开后任职丢失")
	}
}

// TestSlotBindingPrepareAtomicIT（审校 P2#3）：建任职单事务——取消上下文
// 整体回滚，不留行、不留任何事件（含 preparing 中间态）。
func TestSlotBindingPrepareAtomicIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.CreateBinding(canceled, bindingRow(), slot.BindingReady, nil); err == nil {
		t.Fatal("取消上下文应报错")
	}
	var rows, events int
	store.db.QueryRow(`SELECT COUNT(*) FROM slot_bindings`).Scan(&rows)
	store.db.QueryRow(`SELECT COUNT(*) FROM binding_events`).Scan(&events)
	if rows != 0 || events != 0 {
		t.Fatalf("取消回滚残留：rows=%d events=%d（要 0/0）", rows, events)
	}

	// blocked 路径同样单事务（created + preparing→blocked 一次落）
	if _, err := store.CreateBinding(ctx, slot.Binding{
		BindingID: "bd_blk_1", AgentID: "ag_it", SlotInstanceID: "si_blk",
		SlotSpecVersion: 1, Scope: "workspace", Status: slot.BindingBlocked,
		CreatedAt: "t", UpdatedAt: "t",
	}, slot.BindingBlocked, map[string]string{"missing": "工具 x 缺失"}); err != nil {
		t.Fatalf("blocked 建任职: %v", err)
	}
	b, ok, _ := store.BindingByID(ctx, "bd_blk_1")
	if !ok || b.Status != slot.BindingBlocked {
		t.Fatalf("blocked 终态: %+v", b)
	}
	evs, _ := store.BindingEvents(ctx, "bd_blk_1")
	if len(evs) != 2 {
		t.Fatalf("blocked 事件 %d 条, want 2", len(evs))
	}
	// 单事务重试：blocked → preparing → ready 两事件一次落
	got, done, err := store.RetryBindingPreparation(ctx, "bd_blk_1", slot.BindingReady, nil, "t2")
	if err != nil || !done || got.Status != slot.BindingReady {
		t.Fatalf("重试: %+v %v %v", got, done, err)
	}
	evs, _ = store.BindingEvents(ctx, "bd_blk_1")
	if len(evs) != 4 {
		t.Fatalf("重试后事件 %d 条, want 4", len(evs))
	}
	// 非 blocked 重试拒绝
	if _, done, err := store.RetryBindingPreparation(ctx, "bd_blk_1", slot.BindingReady, nil, "t3"); err != nil || done {
		t.Fatalf("非 blocked 重试: %v %v", done, err)
	}
}
