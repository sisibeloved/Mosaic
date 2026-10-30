//go:build it

// IT 层（ADR-0024 / 0.6.1）：mission_sources 唯一映射、dispatch_decisions/
// dispatch_receipts 经真实 SQLite——migration v14 新库、v0.6.0 真旧库升级、
// 并发同键孪生恰一胜者。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/dispatch"
	"github.com/sisibeloved/Mosaic/internal/mission"
)

func sourceMission(id string) mission.Mission {
	return mission.Mission{
		MissionID:       id,
		ContractVersion: 1,
		Status:          mission.StatusReady,
		OwnerAgentID:    "ag_it",
		Requester:       mission.RequesterOwner,
		Content: mission.Content{
			Objective:          "房间交办",
			Scope:              "s",
			AcceptanceCriteria: []string{"a"},
			DeliverableSpecs:   []string{"d"},
			SourceRefs:         []string{"room:r1:tsk_a"},
		},
		CreatedAt: "2026-10-30T10:00:00.000Z",
		UpdatedAt: "2026-10-30T10:00:00.000Z",
	}
}

// TestMissionSourceStoreIT v14 建表 + 映射唯一 + 读面回填。
func TestMissionSourceStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v14 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 14`).Scan(&v14)
	if v14 != 1 {
		t.Fatalf("migration v14 未登记: %d", v14)
	}

	m, err := store.CreateMission(ctx, sourceMission("mi_s1"),
		&mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"})
	if err != nil || m.RoomSource == nil || m.RoomSource.TaskID != "tsk_a" {
		t.Fatalf("建契约（含来源）: %+v %v", m, err)
	}
	// 撞键 → ErrSourceAlreadyMapped 携已映射 id
	_, err = store.CreateMission(ctx, sourceMission("mi_s2"),
		&mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"})
	if !errors.Is(err, mission.ErrSourceAlreadyMapped) || !strings.Contains(err.Error(), "mi_s1") {
		t.Fatalf("撞键应携已映射 id: %v", err)
	}
	// 不同任务卡可建
	if _, err := store.CreateMission(ctx, sourceMission("mi_s3"),
		&mission.RoomSource{RoomID: "r1", TaskID: "tsk_b"}); err != nil {
		t.Fatalf("不同任务卡应可建: %v", err)
	}
	// 映射查询 + 详情/版本回填
	id, ok, _ := store.MissionIDBySource(ctx, "r1", "tsk_a")
	if !ok || id != "mi_s1" {
		t.Fatalf("映射查询: %s %v", id, ok)
	}
	got, _, _ := store.MissionByID(ctx, "mi_s1")
	if got.RoomSource == nil || got.RoomSource.RoomID != "r1" {
		t.Fatalf("详情回填: %+v", got.RoomSource)
	}
	vs, _, _ := store.MissionVersions(ctx, "mi_s1")
	if len(vs) != 1 || vs[0].RoomSource == nil {
		t.Fatalf("版本回填: %+v", vs)
	}
}

// TestDispatchStoreIT 决策/回执往返 + 唯一约束 + 并发孪生恰一胜者。
func TestDispatchStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	d := dispatch.Decision{
		RouteID: "rd_it_1", DecisionVersion: 1, RequestDigest: "dig1",
		Status: "resolved", SelectionSource: "room_default",
		TargetRefs: []string{"room:r1:tsk_c"},
		Request: dispatch.ResolveRequest{
			Kind:       dispatch.KindCreateMission,
			RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_c"},
			Contract: mission.Content{
				Objective: "o", Scope: "s",
				AcceptanceCriteria: []string{"a"}, DeliverableSpecs: []string{"d"},
				SourceRefs: []string{"room:r1:tsk_c"},
			},
		},
		ExpiresAt: "2026-10-30T12:10:00.000Z", CreatedAt: "2026-10-30T12:00:00.000Z",
	}
	if err := store.SaveDecision(ctx, d); err != nil {
		t.Fatalf("存决策: %v", err)
	}
	got, ok, _ := store.DecisionByID(ctx, "rd_it_1")
	if !ok || got.Request.RoomSource.TaskID != "tsk_c" || len(got.TargetRefs) != 1 {
		t.Fatalf("决策往返: %+v", got)
	}

	r := dispatch.Receipt{
		ReceiptID: "dpr_it_1", CallerRef: dispatch.CallerOwner, Scope: dispatch.ScopeWorkspace,
		IdempotencyKey: "k1", RequestDigest: "dig1", RouteID: "rd_it_1",
		DecisionVersion: 1, DomainCommandID: "mi_x",
		TargetRefs: []string{"room:r1:tsk_c"}, ReturnRefs: []string{"room:r1"},
		CommittedAt: "t",
	}
	// 认领 → 完结两段（审校 P1）：pending（domain_command_id 空）→ finalize
	ex0, conflict0, finalized0, err := store.ClaimKey(ctx, r)
	if err != nil || conflict0 || finalized0 || ex0.ReceiptID != "" {
		t.Fatalf("首认领: ex=%+v conflict=%v finalized=%v err=%v", ex0, conflict0, finalized0, err)
	}
	if err := store.FinalizeReceipt(ctx, r); err != nil {
		t.Fatalf("完结: %v", err)
	}
	// 同键异摘要 → conflict（写入前退出，零副作用）
	ex, conflict, finalized, err := store.ClaimKey(ctx, dispatch.Receipt{
		ReceiptID: "dpr_it_2", CallerRef: dispatch.CallerOwner, Scope: dispatch.ScopeWorkspace,
		IdempotencyKey: "k1", RequestDigest: "other-digest", RouteID: "rd_other", CommittedAt: "t2",
	})
	if err != nil || !conflict || finalized || ex.ReceiptID != "dpr_it_1" {
		t.Fatalf("异摘要应 conflict+已存回执: ex=%+v conflict=%v finalized=%v err=%v", ex, conflict, finalized, err)
	}
	// 同键同摘要已完结 → finalized 重放（P2#4）
	same := r
	same.ReceiptID = "dpr_it_3"
	exR, conflictR, finalizedR, err := store.ClaimKey(ctx, same)
	if err != nil || conflictR || !finalizedR || exR.DomainCommandID != "mi_x" {
		t.Fatalf("同摘要应 finalized 重放: ex=%+v conflict=%v finalized=%v err=%v", exR, conflictR, finalizedR, err)
	}
	byKey, ok, _ := store.ReceiptByKey(ctx, dispatch.CallerOwner, dispatch.ScopeWorkspace, "k1")
	if !ok || byKey.ReceiptID != "dpr_it_1" || byKey.ReturnRefs[0] != "room:r1" {
		t.Fatalf("按键查回执: %+v", byKey)
	}

	// 并发同键同摘要认领：恰一胜者（其余 pending 同摘要——重驱动恢复路径）
	var wg sync.WaitGroup
	var wins, sameDigest atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ex, conflict, finalized, err := store.ClaimKey(ctx, dispatch.Receipt{
				ReceiptID: "dpr_race_" + strings.Repeat("a", i+1),
				CallerRef: dispatch.CallerOwner, Scope: dispatch.ScopeWorkspace,
				IdempotencyKey: "race", RequestDigest: "race-digest", CommittedAt: "t",
			})
			switch {
			case err != nil:
				t.Errorf("并发认领: %v", err)
			case conflict || finalized:
				t.Errorf("并发首认领阶段不得 conflict/finalized: %v %v", conflict, finalized)
			case ex.ReceiptID == "":
				wins.Add(1) // 认领胜者（INSERT 成功——返回行空）
			case ex.RequestDigest == "race-digest":
				sameDigest.Add(1) // pending 同摘要（重驱动恢复路径）
			default:
				t.Errorf("并发认领意外返回行: %+v", ex)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || sameDigest.Load() != 7 {
		t.Fatalf("恰一认领胜者: wins=%d pending=%d", wins.Load(), sameDigest.Load())
	}
}

// TestDispatchLegacyUpgradeIT v0.6.0 旧库升级：截 v14 段建库 → 三表就位、
// 登记恰一条 v14、重开幂等。
func TestDispatchLegacyUpgradeIT(t *testing.T) {
	marker := "-- v14（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v14 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-legacy-v060.db")
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
	if err := upgraded.SaveDecision(ctx, dispatch.Decision{
		RouteID: "rd_up", DecisionVersion: 1, Status: "resolved", SelectionSource: "room_default",
		ExpiresAt: "t", CreatedAt: "t",
		Request: dispatch.ResolveRequest{Kind: dispatch.KindCreateMission,
			RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_u"}},
	}); err != nil {
		t.Fatalf("升级后存决策: %v", err)
	}
	var v14, tbl int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 14`).Scan(&v14)
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='dispatch_receipts'`).Scan(&tbl)
	if v14 != 1 || tbl != 1 {
		t.Fatalf("v14=%d dispatch_receipts=%d", v14, tbl)
	}
	upgraded.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, ok, _ := reopened.DecisionByID(ctx, "rd_up"); !ok {
		t.Fatal("重开后决策丢失")
	}
}
