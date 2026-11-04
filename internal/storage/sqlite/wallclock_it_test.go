//go:build it

// IT 层（ADR-0036 §3 / 0.6.13）：migration v30（runs += deadline）、deadline
// 落行与过期查询（ListExpiredRunning）、v29 旧库升级幂等。
package sqlite

import (
	"context"
	"testing"

	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

func TestWallClockStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()

	var v30 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 30`).Scan(&v30)
	if v30 != 1 {
		t.Fatalf("migration v30 未登记: %d", v30)
	}

	mkRun := func(id, state, started, deadline string) {
		r := missionrun.Run{
			RunID: id, MissionID: "mi_wc_" + id, Attempt: 1, LeaseID: "lse_t", ManifestID: "cmf_t",
			RuntimeID: "rtx_t", BindingID: "bd_t", AgentID: "ag_t", State: state,
			FenceToken: "fnc_t", Generation: 1, RequestedAt: started, StartedAt: started,
			UpdatedAt: started, Deadline: deadline,
		}
		if _, err := store.CreateRun(ctx, r); err != nil {
			t.Fatalf("建 %s: %v", id, err)
		}
	}
	mkRun("run_wc1", "running", "2026-11-03T12:00:00Z", "2026-11-03T12:01:00Z")
	mkRun("run_wc2", "running", "2026-11-03T12:00:00Z", "2026-11-03T13:00:00Z")
	mkRun("run_wc3", "running", "2026-11-03T12:00:00Z", "") // 无墙钟
	mkRun("run_wc4", "canceled", "2026-11-03T12:00:00Z", "2026-11-03T12:00:30Z")

	expired, err := store.ListExpiredRunning(ctx, "2026-11-03T12:02:00Z")
	if err != nil || len(expired) != 1 || expired[0].RunID != "run_wc1" {
		t.Fatalf("过期查询: %v %+v", err, expired)
	}
	// deadline 读回
	got, ok, _ := store.RunByID(ctx, "run_wc1")
	if !ok || got.Deadline != "2026-11-03T12:01:00Z" {
		t.Fatalf("deadline 读回: ok=%v %+v", ok, got)
	}
	// UpdateRun 全行替换保留 deadline
	got.State = missionrun.StateCanceled
	got.FinishedAt = "2026-11-03T12:01:01Z"
	if err := store.UpdateRun(ctx, got, nil); err != nil {
		t.Fatal(err)
	}
	again, _, _ := store.RunByID(ctx, "run_wc1")
	if again.Deadline != "2026-11-03T12:01:00Z" || again.State != missionrun.StateCanceled {
		t.Fatalf("更新后失真: %+v", again)
	}
}

// TestWallClockUpgradeIT v29 旧库升级：删列与登记后重开 → v30 再登记、列重建。
func TestWallClockUpgradeIT(t *testing.T) {
	store, path := openTempStore(t)
	for _, stmt := range []string{
		`ALTER TABLE runs DROP COLUMN deadline`,
		`DELETE FROM migrations WHERE version = 30`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("降级 %s: %v", stmt, err)
		}
	}
	store.Close()

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { store2.Close() })
	var v30 int
	store2.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 30`).Scan(&v30)
	if v30 != 1 {
		t.Fatalf("v30 应重新登记: %d", v30)
	}
	ctx := context.Background()
	r := missionrun.Run{RunID: "run_up_wc", MissionID: "mi_up", Attempt: 1, LeaseID: "l", ManifestID: "m",
		RuntimeID: "r", BindingID: "b", AgentID: "a", State: missionrun.StateRunning,
		FenceToken: "f", Generation: 1, RequestedAt: "t", Deadline: "2026-11-03T13:00:00Z"}
	if _, err := store2.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	expired, err := store2.ListExpiredRunning(ctx, "2026-11-03T14:00:00Z")
	if err != nil || len(expired) != 1 || expired[0].RunID != "run_up_wc" {
		t.Fatalf("升级后过期查询: %v %+v", err, expired)
	}
}
