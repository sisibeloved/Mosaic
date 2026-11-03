//go:build it

// IT 层（ADR-0035 / 0.6.12）：责任交接经真实 SQLite——v29 新库 + v28 旧库升级
// （幂等加列 + 登记）、offer/accept/withdraw 单事务与 pending 锚、事务内冻结窗
// （Revise / RequestReview / MarkCanceling 拒，Pause 照常）、unassigned draft
// 承接完备性重判、承诺门直查读面（ListMissions 归属随承接翻转）。
package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

func handoverEvent(kind string, payload map[string]any, at string) mission.Event {
	raw, _ := json.Marshal(payload)
	return mission.Event{EventType: kind, Payload: raw, OccurredAt: at}
}

// TestHandoverStoreIT v29 登记 + 全环（发起 → 冻结窗 → 承接 → 锚）。
func TestHandoverStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()

	var v29 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 29`).Scan(&v29)
	if v29 != 1 {
		t.Fatalf("migration v29 未登记: %d", v29)
	}

	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_a"), nil)
	if err != nil {
		t.Fatalf("建契约: %v", err)
	}
	if m.PendingOwnerAgentID != "" {
		t.Fatalf("新建 pending 应空: %+v", m)
	}

	// 发起：pending 置位、owner/版本不变、事件
	out, ok, err := store.HandoverOffer(ctx, m.MissionID, 1, "ag_b",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "ag_a", "to_owner": "ag_b", "contract_version": 1}, "2026-11-03T12:00:00Z"))
	if err != nil || !ok || out.PendingOwnerAgentID != "ag_b" || out.OwnerAgentID != "ag_a" || out.ContractVersion != 1 {
		t.Fatalf("发起: ok=%v %+v %v", ok, out, err)
	}
	// 在途唯一：二发 ok=false
	if _, ok, _ := store.HandoverOffer(ctx, m.MissionID, 1, "ag_c",
		handoverEvent(mission.EventHandoverOffered, map[string]any{}, "t")); ok {
		t.Fatal("在途唯一：二发须 ok=false")
	}

	// 事务内冻结窗：Revise / RequestReview / MarkCanceling 拒；Pause 照常
	next := out
	next.ContractVersion = 2
	next.Objective = "改"
	if _, ok, err := store.ReviseMission(ctx, m.MissionID, 1, next); err != nil || ok {
		t.Fatalf("冻结窗修订须 ok=false: ok=%v %v", ok, err)
	}
	if _, ok, err := store.RequestReview(ctx, m.MissionID, 1, "dlv_1",
		handoverEvent(mission.EventStatusChanged, map[string]any{}, "t")); err != nil || ok {
		t.Fatalf("冻结窗送验须 ok=false: ok=%v %v", ok, err)
	}
	if _, ok, err := store.MarkCanceling(ctx, m.MissionID, 1, "r", "t"); err != nil || ok {
		t.Fatalf("冻结窗取消须 ok=false: ok=%v %v", ok, err)
	}
	if p, ok, err := store.PauseMission(ctx, m.MissionID, 1, "排空", "t"); err != nil || !ok || p.Status != mission.StatusPaused || p.PendingOwnerAgentID != "ag_b" {
		t.Fatalf("冻结窗暂停照常且 pending 保留: ok=%v %+v %v", ok, p, err)
	}
	if _, ok, err := store.ResumeMission(ctx, m.MissionID, 1, mission.StatusReady, "t"); err != nil || !ok {
		t.Fatalf("恢复: ok=%v %v", ok, err)
	}

	// 承接：锚不符 ok=false；锚相符 → owner 翻转、pending 清空、回执事件、版本不变
	if _, ok, _ := store.HandoverAccept(ctx, m.MissionID, 1, "ag_c", mission.StatusReady,
		handoverEvent(mission.EventHandoverAccepted, map[string]any{}, "t")); ok {
		t.Fatal("pending 锚不符须 ok=false")
	}
	acc, ok, err := store.HandoverAccept(ctx, m.MissionID, 1, "ag_b", mission.StatusReady,
		handoverEvent(mission.EventHandoverAccepted, map[string]any{"from_owner": "ag_a", "to_owner": "ag_b", "status_at_accept": "ready", "contract_version": 1}, "2026-11-03T12:01:00Z"))
	if err != nil || !ok || acc.OwnerAgentID != "ag_b" || acc.PendingOwnerAgentID != "" || acc.ContractVersion != 1 || acc.Status != mission.StatusReady {
		t.Fatalf("承接: ok=%v %+v %v", ok, acc, err)
	}
	// 版本行不受交接影响（归属翻转非契约修订）
	vs, _, _ := store.MissionVersions(ctx, m.MissionID)
	if len(vs) != 1 || vs[0].OwnerAgentID != "ag_a" {
		t.Fatalf("版本行须仍 1 行且记历史 owner: %+v", vs)
	}
	// 事件序：created / offered / paused / resumed / accepted
	evs, _, _ := store.MissionEvents(ctx, m.MissionID)
	kinds := []string{}
	for _, e := range evs {
		kinds = append(kinds, e.EventType)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "handover_offered") || !strings.HasSuffix(joined, "handover_accepted") {
		t.Fatalf("事件序: %s", joined)
	}
	// 承诺门直查读面：摘要 owner 已翻转
	list, _ := store.ListMissions(ctx)
	if len(list) != 1 || list[0].OwnerAgentID != "ag_b" {
		t.Fatalf("摘要归属: %+v", list)
	}
	// 了结：无在途 ok=false
	if _, ok, _ := store.HandoverWithdraw(ctx, m.MissionID, 1, "ag_b",
		handoverEvent(mission.EventHandoverRevoked, map[string]any{}, "t")); ok {
		t.Fatal("无在途撤回须 ok=false")
	}
	// 承接后修订恢复可用（冻结窗解除）
	next = acc
	next.ContractVersion = 2
	if _, ok, err := store.ReviseMission(ctx, m.MissionID, 1, next); err != nil || !ok {
		t.Fatalf("承接后修订: ok=%v %v", ok, err)
	}
}

// TestHandoverAcceptCompletenessIT unassigned draft 承接 → ready + status_changed
// （reason=handover），事务内一次提交。
func TestHandoverAcceptCompletenessIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	row := missionRow(mission.StatusDraft, "")
	row.MissionID = "mi_it_draft"
	if _, err := store.CreateMission(ctx, row, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverOffer(ctx, row.MissionID, 1, "ag_b",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "", "to_owner": "ag_b"}, "t")); err != nil || !ok {
		t.Fatalf("draft 发起: ok=%v %v", ok, err)
	}
	acc, ok, err := store.HandoverAccept(ctx, row.MissionID, 1, "ag_b", mission.StatusReady,
		handoverEvent(mission.EventHandoverAccepted, map[string]any{"from_owner": "", "to_owner": "ag_b", "status_at_accept": "ready"}, "t"))
	if err != nil || !ok || acc.Status != mission.StatusReady || acc.OwnerAgentID != "ag_b" {
		t.Fatalf("承接完备性重判: ok=%v %+v %v", ok, acc, err)
	}
	evs, _, _ := store.MissionEvents(ctx, row.MissionID)
	last := evs[len(evs)-1]
	if last.EventType != mission.EventStatusChanged || !strings.Contains(string(last.Payload), `"reason":"handover"`) ||
		!strings.Contains(string(last.Payload), `"to":"ready"`) {
		t.Fatalf("缺 status_changed(reason=handover): %s %s", last.EventType, last.Payload)
	}
	// 入调度列
	ready, _ := store.ListMissionsByStatus(ctx, mission.StatusReady)
	if len(ready) != 1 || ready[0].MissionID != row.MissionID {
		t.Fatalf("应入 ready 列: %+v", ready)
	}
}

// TestHandoverUpgradeIT v28 旧库升级：删 pending 列与登记后重开 → v29 再登记、
// 列重建（默认空）、既有任务可发起交接。
func TestHandoverUpgradeIT(t *testing.T) {
	store, path := openTempStore(t)
	ctx := context.Background()
	if _, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_a"), nil); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE missions DROP COLUMN pending_owner_agent_id`,
		`DELETE FROM migrations WHERE version = 29`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("降级 %s: %v", stmt, err)
		}
	}
	store.Close()

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen（v29 迁移）: %v", err)
	}
	t.Cleanup(func() { store2.Close() })
	var v29 int
	store2.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 29`).Scan(&v29)
	if v29 != 1 {
		t.Fatalf("v29 应重新登记: %d", v29)
	}
	m, ok, err := store2.MissionByID(ctx, "mi_it_1")
	if err != nil || !ok || m.PendingOwnerAgentID != "" || m.OwnerAgentID != "ag_a" {
		t.Fatalf("升级后读取: ok=%v %+v %v", ok, m, err)
	}
	if out, ok, err := store2.HandoverOffer(ctx, "mi_it_1", 1, "ag_b",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "ag_a", "to_owner": "ag_b"}, "t")); err != nil || !ok || out.PendingOwnerAgentID != "ag_b" {
		t.Fatalf("升级后发起: ok=%v %+v %v", ok, out, err)
	}
	// 再开一次：幂等（重复加列容忍、登记不重复）
	store2.Close()
	store3, err := Open(path)
	if err != nil {
		t.Fatalf("三开: %v", err)
	}
	t.Cleanup(func() { store3.Close() })
	store3.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 29`).Scan(&v29)
	if v29 != 1 {
		t.Fatalf("v29 登记须恰 1: %d", v29)
	}
}
