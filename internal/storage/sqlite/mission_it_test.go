//go:build it

// IT 层（ADR-0023 / 0.6.0）：missions + mission_contract_versions +
// mission_events 经真实 SQLite——migration v13 新库、v0.5.5 真旧库升级、
// 并发 CAS 修订恰一胜者、版本行不可变。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

func missionRow(status, owner string) mission.Mission {
	return mission.Mission{
		MissionID:       "mi_it_1",
		ContractVersion: 1,
		Status:          status,
		OwnerAgentID:    owner,
		Requester:       mission.RequesterOwner,
		Content: mission.Content{
			Objective:          "目标",
			Scope:              "范围",
			AcceptanceCriteria: []string{"验收"},
			DeliverableSpecs:   []string{"交付"},
			SourceRefs:         []string{"room:abc:seq1"},
		},
		CreatedAt: "2026-10-30T10:00:00.000Z",
		UpdatedAt: "2026-10-30T10:00:00.000Z",
	}
}

// TestMissionStoreIT v13 建表 + 迁移登记 + 全读写往返。
func TestMissionStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v13 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 13`).Scan(&v13)
	if v13 != 1 {
		t.Fatalf("migration v13 未登记: %d", v13)
	}

	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_it"), nil)
	if err != nil || m.Status != mission.StatusReady {
		t.Fatalf("建契约: %+v %v", m, err)
	}
	evs, ok, _ := store.MissionEvents(ctx, m.MissionID)
	if !ok || len(evs) != 1 || evs[0].EventType != mission.EventCreated {
		t.Fatalf("created 事件: ok=%v %+v", ok, evs)
	}

	// 修订：完备 → 削验收（ready → draft）
	next := m
	next.ContractVersion = 2
	next.Content.AcceptanceCriteria = nil
	next.Status = mission.StatusDraft
	next.UpdatedAt = "t2"
	out, ok2, err := store.ReviseMission(ctx, m.MissionID, 1, next)
	if err != nil || !ok2 || out.Status != mission.StatusDraft {
		t.Fatalf("修订: ok=%v err=%v status=%s", ok2, err, out.Status)
	}
	evs, _, _ = store.MissionEvents(ctx, m.MissionID)
	if len(evs) != 3 { // created + contract_revised + status_changed
		t.Fatalf("事件 %d 条, want 3: %+v", len(evs), evs)
	}

	// CAS 败（旧版本号）
	if _, ok3, _ := store.ReviseMission(ctx, m.MissionID, 1, next); ok3 {
		t.Fatal("旧版本号修订应 ok=false")
	}
	// 取消（0.6.6 两段）+ 终态拒修
	cing, ok4, err := store.MarkCanceling(ctx, m.MissionID, 2, "计划变更", "t3")
	if err != nil || !ok4 || cing.Status != mission.StatusCanceling {
		t.Fatalf("停止意图: ok=%v err=%v", ok4, err)
	}
	canceled, ok4b, err := store.FinishCancel(ctx, m.MissionID, "计划变更", "t3b")
	if err != nil || !ok4b || canceled.Status != mission.StatusCanceled {
		t.Fatalf("收尾: ok=%v err=%v", ok4b, err)
	}
	if _, ok5, _ := store.ReviseMission(ctx, m.MissionID, 2, next); ok5 {
		t.Fatal("终态修订应 ok=false")
	}
	if _, ok6, _ := store.MarkCanceling(ctx, m.MissionID, 2, "", "t4"); ok6 {
		t.Fatal("重复取消应 ok=false")
	}
	if _, _, err := store.MarkCanceling(ctx, "mi_none", 1, "", "t"); !errors.Is(err, mission.ErrMissionNotFound) {
		t.Fatalf("未知取消应 ErrMissionNotFound: %v", err)
	}

	// 版本行不可变快照
	vs, okV, _ := store.MissionVersions(ctx, m.MissionID)
	if !okV || len(vs) != 2 || vs[0].Status != mission.StatusReady || vs[1].Status != mission.StatusDraft {
		t.Fatalf("版本快照: ok=%v %+v", okV, vs)
	}
	// 摘要
	sums, _ := store.ListMissions(ctx)
	if len(sums) != 1 || sums[0].Status != mission.StatusCanceled || sums[0].Objective != "目标" {
		t.Fatalf("摘要: %+v", sums)
	}
}

// TestMissionConcurrentReviseIT 并发修订恰一胜者（AT-13）：8 路同
// expected_version，版本恰 +1、恰一胜者，事件序列无交错损坏。
func TestMissionConcurrentReviseIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_it"), nil)
	if err != nil {
		t.Fatalf("建契约: %v", err)
	}

	var wg sync.WaitGroup
	var wins, losses atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next := m
			next.ContractVersion = 2
			next.Objective = strings.Repeat("改", i+1)
			next.Content.Objective = next.Objective
			_, ok, err := store.ReviseMission(ctx, m.MissionID, 1, next)
			if err != nil {
				t.Errorf("并发修订 %d: %v", i, err)
				return
			}
			if ok {
				wins.Add(1)
			} else {
				losses.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != 7 {
		t.Fatalf("恰一胜者: wins=%d losses=%d", wins.Load(), losses.Load())
	}
	got, _, _ := store.MissionByID(ctx, m.MissionID)
	if got.ContractVersion != 2 {
		t.Fatalf("版本应恰 v2: %d", got.ContractVersion)
	}
	vs, _, _ := store.MissionVersions(ctx, m.MissionID)
	if len(vs) != 2 {
		t.Fatalf("版本行应恰 2: %d", len(vs))
	}
}

// TestMissionLegacyUpgradeIT v0.5.5 旧库升级：截 v13 段建库 → 表就位、
// 登记恰一条 v13、重开幂等。
func TestMissionLegacyUpgradeIT(t *testing.T) {
	marker := "-- v13（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v13 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-legacy-v055.db")
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
	if _, err := upgraded.CreateMission(ctx, missionRow(mission.StatusDraft, ""), nil); err != nil {
		t.Fatalf("升级后建契约: %v", err)
	}
	var v13 int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 13`).Scan(&v13)
	if v13 != 1 {
		t.Fatalf("v13 登记数 = %d", v13)
	}
	var tbl int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='mission_contract_versions'`).Scan(&tbl)
	if tbl != 1 {
		t.Fatal("mission_contract_versions 未建")
	}
	upgraded.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if g, ok, _ := reopened.MissionByID(ctx, "mi_it_1"); !ok || g.Status != mission.StatusDraft {
		t.Fatalf("重开后契约丢失: ok=%v", ok)
	}
}
