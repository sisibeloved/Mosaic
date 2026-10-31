//go:build it

// IT 层（ADR-0027 / 0.6.4）：runtime_leases 经真实 SQLite——migration v17
// 新库、双部分唯一索引互斥（并发恰一胜者）、代次 MAX 单源、惰性到期、
// v0.6.3 真旧库升级。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mosaicruntime "github.com/sisibeloved/Mosaic/internal/runtime"
)

func activeLease(id, runtimeID, envID string, gen int, expires string) mosaicruntime.Lease {
	return mosaicruntime.Lease{
		LeaseID: id, RuntimeID: runtimeID, EnvironmentID: envID, WorkspaceRef: "/d/" + envID,
		MissionID: "mi_it", Generation: gen, FenceToken: "fnc_" + id, State: mosaicruntime.LeaseStateActive,
		ResourceLimits: map[string]int{"concurrency": 1},
		AcquiredAt:     "2026-10-30T10:00:00.000000000Z", ExpiresAt: expires, UpdatedAt: "2026-10-30T10:00:00.000000000Z",
	}
}

// TestLeaseStoreIT v17 建表 + 互斥 + 代次 + 惰性到期 + 读面。
func TestLeaseStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v17 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 17`).Scan(&v17)
	if v17 != 1 {
		t.Fatalf("migration v17 未登记: %d", v17)
	}

	// 首领：代次 1
	l1, err := store.AcquireLease(ctx, activeLease("lse_a", "rtx_a", "env_x", 0, "2026-10-30T11:00:00.000000000Z"))
	if err != nil || l1.Generation != 1 {
		t.Fatalf("首领: %+v %v", l1, err)
	}
	// 容量互斥（同运行时）
	if _, err := store.AcquireLease(ctx, activeLease("lse_b", "rtx_a", "env_y", 0, "2026-10-30T11:00:00.000000000Z")); !errors.Is(err, mosaicruntime.ErrLeaseConflictRuntime) {
		t.Fatalf("容量互斥: %v", err)
	}
	// 工作区独占（跨运行时同环境）
	if _, err := store.AcquireLease(ctx, activeLease("lse_c", "rtx_b", "env_x", 0, "2026-10-30T11:00:00.000000000Z")); !errors.Is(err, mosaicruntime.ErrLeaseConflictWorkspace) {
		t.Fatalf("工作区独占: %v", err)
	}

	// 库级部分唯一索引兜底：绕过探查直插同运行时活跃行 → 拒绝
	_, err = store.db.Exec(`INSERT INTO runtime_leases (lease_id, runtime_id, environment_id, workspace_ref, mission_id, generation, fence_token, state, resource_limits, acquired_at, expires_at, released_at, updated_at)
VALUES ('lse_raw', 'rtx_a', 'env_z', '/d/env_z', 'mi_it', 9, 'fnc_raw', 'active', '{}', 't', 't', '', 't')`)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("部分唯一索引应兜底拒绝: %v", err)
	}

	// 释放 → 再领：代次 2（MAX 单源）
	rel := l1
	rel.State, rel.ReleasedAt, rel.UpdatedAt = mosaicruntime.LeaseStateReleased, "2026-10-30T10:30:00.000000000Z", "2026-10-30T10:30:00.000000000Z"
	if err := store.UpdateLease(ctx, rel); err != nil {
		t.Fatalf("释放: %v", err)
	}
	l2, err := store.AcquireLease(ctx, activeLease("lse_d", "rtx_a", "env_x", 0, "2026-10-30T10:40:00.000000000Z"))
	if err != nil || l2.Generation != 2 {
		t.Fatalf("再领代次: %+v %v", l2, err)
	}
	if gen, _ := store.MaxLeaseGeneration(ctx, "rtx_a"); gen != 2 {
		t.Fatalf("当代代次应 2: %d", gen)
	}

	// 惰性到期：expires ≤ now 转 expired
	if err := store.ExpireOverdue(ctx, "2026-10-30T10:41:00.000000000Z"); err != nil {
		t.Fatalf("惰性到期: %v", err)
	}
	got, ok, _ := store.LeaseByID(ctx, "lse_d")
	if !ok || got.State != mosaicruntime.LeaseStateExpired {
		t.Fatalf("应见 expired: %+v", got)
	}
	list, _ := store.ListLeases(ctx, mosaicruntime.LeaseStateActive, "")
	if len(list) != 0 {
		t.Fatalf("活跃应清零: %+v", list)
	}
	all, _ := store.ListLeases(ctx, mosaicruntime.LeaseStateAll, "rtx_a")
	if len(all) != 2 {
		t.Fatalf("历史应 2: %+v", all)
	}
}

// TestLeaseConcurrentAcquireIT 并发领取恰一胜者（临界区串行 + 库级兜底）。
func TestLeaseConcurrentAcquireIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.AcquireLease(ctx, activeLease(
				"lse_race"+strings.Repeat("0", 2)+string(rune('0'+i)), "rtx_race", "env_race", 0,
				"2026-10-30T11:00:00.000000000Z"))
		}(i)
	}
	wg.Wait()
	okN := 0
	for _, err := range errs {
		switch {
		case err == nil:
			okN++
		case errors.Is(err, mosaicruntime.ErrLeaseConflictRuntime):
		default:
			t.Fatalf("并发意外错误: %v", err)
		}
	}
	if okN != 1 {
		t.Fatalf("并发恰一胜者: %d", okN)
	}
	if gen, _ := store.MaxLeaseGeneration(ctx, "rtx_race"); gen != 1 {
		t.Fatalf("胜者代次应 1: %d", gen)
	}
}

// TestLeaseLegacyUpgradeIT v0.6.3 真旧库升级到 v17。
func TestLeaseLegacyUpgradeIT(t *testing.T) {
	marker := "-- v17（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v17 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v063.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧库升级打开: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var v17 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 17`).Scan(&v17)
	if v17 != 1 {
		t.Fatalf("升级后 migration v17 应登记: %d", v17)
	}
	if _, err := store.AcquireLease(ctx, activeLease("lse_l", "rtx_l", "env_l", 0, "2026-10-30T11:00:00.000000000Z")); err != nil {
		t.Fatalf("升级库领取: %v", err)
	}
}
