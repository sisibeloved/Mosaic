//go:build it

// IT 层（ADR-0028 / 0.6.5）：runs/run_events 经真实 SQLite——migration v19
// 新库、每 Mission 至多一未终态（部分唯一索引兜底）、attempt 递增、
// 事件回读、v0.6.4 真旧库升级。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

func seedRun(id, missionID string, attempt int, state string) missionrun.Run {
	return missionrun.Run{
		RunID: id, MissionID: missionID, Attempt: attempt,
		LeaseID: "lse_it", ManifestID: "cmf_it", RuntimeID: "rtx_it", BindingID: "bd_it",
		AgentID: "ag_it", State: state, FenceToken: "fnc_it", Generation: 1,
		RequestedAt: "2026-10-31T10:00:00.000000000Z", UpdatedAt: "2026-10-31T10:00:00.000000000Z",
	}
}

// TestRunStoreIT v19 建表 + 未终态互斥 + attempt + 事件。
func TestRunStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v19 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 19`).Scan(&v19)
	if v19 != 1 {
		t.Fatalf("migration v19 未登记: %d", v19)
	}

	// 首建（服务层 attempt=0 传入，存储面 +1）
	r, err := store.CreateRun(ctx, seedRun("run_a", "mi_it", 0, missionrun.StateRunning))
	if err != nil || r.Attempt != 1 {
		t.Fatalf("首建: %+v %v", r, err)
	}
	// 未终态互斥（部分唯一索引 + 先查兜底）
	if _, err := store.CreateRun(ctx, seedRun("run_b", "mi_it", 0, missionrun.StatePending)); !errors.Is(err, missionrun.ErrRunExists) {
		t.Fatalf("未终态互斥: %v", err)
	}
	// 终态后可再建：attempt 2
	r.State = missionrun.StateCompleted
	r.FinishedAt = "2026-10-31T10:01:00.000000000Z"
	if err := store.UpdateRun(ctx, r, []missionrun.Event{{
		RunID: r.RunID, EventType: missionrun.EventCompleted,
		Payload: map[string]any{"late": false}, OccurredAt: "2026-10-31T10:01:00.000000000Z",
	}}); err != nil {
		t.Fatalf("终态更新: %v", err)
	}
	r2, err := store.CreateRun(ctx, seedRun("run_c", "mi_it", 0, missionrun.StateRunning))
	if err != nil || r2.Attempt != 2 {
		t.Fatalf("再建 attempt 2: %+v %v", r2, err)
	}

	// 事件回读（payload 反序列化）
	evs, err := store.RunEvents(ctx, "run_a")
	if err != nil || len(evs) != 2 {
		t.Fatalf("事件流: %v %d", err, len(evs))
	}
	if evs[0].EventType != missionrun.EventAttemptRequested || evs[1].EventType != missionrun.EventCompleted ||
		evs[1].Payload["late"] != false {
		t.Fatalf("事件回读错误: %+v", evs)
	}

	// 列表（attempt 降序）+ 未终态列表
	runs, _ := store.ListRunsByMission(ctx, "mi_it")
	if len(runs) != 2 || runs[0].Attempt != 2 {
		t.Fatalf("Run 历史: %+v", runs)
	}
	unfinished, _ := store.ListUnfinished(ctx)
	if len(unfinished) != 1 || unfinished[0].RunID != "run_c" {
		t.Fatalf("未终态: %+v", unfinished)
	}
}

// TestRunLegacyUpgradeIT v0.6.4 真旧库升级到 v19。
func TestRunLegacyUpgradeIT(t *testing.T) {
	// v18 在独立 const——旧库 = schemaDDL（至 v17）+ migrationV18
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v064.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		t.Fatalf("legacy base ddl: %v", err)
	}
	if _, err := db.Exec(migrationV18); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		t.Fatalf("legacy v18: %v", err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧库升级打开: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var v19 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 19`).Scan(&v19)
	if v19 != 1 {
		t.Fatalf("升级后 v19 应登记: %d", v19)
	}
	if _, err := store.CreateRun(ctx, seedRun("run_l", "mi_l", 0, missionrun.StateRunning)); err != nil {
		t.Fatalf("升级库建 Run: %v", err)
	}
}
