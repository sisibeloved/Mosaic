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
	"github.com/sisibeloved/Mosaic/internal/personalagent"
)

func handoverEvent(kind string, payload map[string]any, at string) mission.Event {
	raw, _ := json.Marshal(payload)
	return mission.Event{EventType: kind, Payload: raw, OccurredAt: at}
}

// TestHandoverStoreIT v29 登记 + 全环（发起 → 冻结窗 → 承接 → 锚）。
func TestHandoverStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	// 审校 #3a：承接事务内核对承接人身份——直查 agents 表，须先有身份行
	for _, a := range []personalagent.Agent{
		{AgentID: "ag_b", DisplayName: "乙", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"},
		{AgentID: "ag_c", DisplayName: "丙", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:02Z"},
	} {
		if _, err := store.EnsureAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

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
	out, ok, err := store.HandoverOffer(ctx, m.MissionID, 1, "ag_b", "hdo_it_1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "ag_a", "to_owner": "ag_b", "offer_id": "hdo_it_1", "contract_version": 1}, "2026-11-03T12:00:00Z"))
	if err != nil || !ok || out.PendingOwnerAgentID != "ag_b" || out.PendingOfferID != "hdo_it_1" || out.OwnerAgentID != "ag_a" || out.ContractVersion != 1 {
		t.Fatalf("发起: ok=%v %+v %v", ok, out, err)
	}
	// 在途唯一：二发 ok=false
	if _, ok, _ := store.HandoverOffer(ctx, m.MissionID, 1, "ag_c", "hdo_it_2",
		handoverEvent(mission.EventHandoverOffered, map[string]any{}, "t")); ok {
		t.Fatal("在途唯一：二发须 ok=false")
	}

	// 事务内冻结窗：Revise / RequestReview / MarkCanceling 拒；Pause 照常
	next := out
	next.ContractVersion = 2
	next.Objective = "改"
	if _, ok, err := store.ReviseMission(ctx, m.MissionID, 1, "ag_a", next); err != nil || ok {
		t.Fatalf("冻结窗修订须 ok=false: ok=%v %v", ok, err)
	}
	// 审校 #2：归属锚——owner 不符的修订 ok=false（承接翻转后旧表单）
	if _, ok, err := store.ReviseMission(ctx, m.MissionID, 1, "ag_z", next); err != nil || ok {
		t.Fatalf("归属锚不符修订须 ok=false: ok=%v %v", ok, err)
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

	// 承接：发起标识锚不符 ok=false；锚相符 → owner 翻转、pending 清空、回执事件、版本不变
	if _, ok, _ := store.HandoverAccept(ctx, m.MissionID, 1, "hdo_bogus",
		handoverEvent(mission.EventHandoverAccepted, map[string]any{}, "t")); ok {
		t.Fatal("offer 锚不符须 ok=false")
	}
	acc, ok, err := store.HandoverAccept(ctx, m.MissionID, 1, "hdo_it_1",
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
	if _, ok, _ := store.HandoverWithdraw(ctx, m.MissionID, 1, "hdo_it_1",
		handoverEvent(mission.EventHandoverRevoked, map[string]any{}, "t")); ok {
		t.Fatal("无在途撤回须 ok=false")
	}
	// 承接后修订恢复可用（冻结窗解除；归属锚 = 新 owner）
	next = acc
	next.ContractVersion = 2
	if _, ok, err := store.ReviseMission(ctx, m.MissionID, 1, "ag_b", next); err != nil || !ok {
		t.Fatalf("承接后修订: ok=%v %v", ok, err)
	}
}

// TestHandoverAcceptIdentityGateIT 审校 #3a：承接事务内核对承接人身份——offer
// 后 SQL 直写置 retired（模拟服务层校验后的竞态窗口），accept 须 ok=false 且零写入。
func TestHandoverAcceptIdentityGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{AgentID: "ag_gate", DisplayName: "门", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_src"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverOffer(ctx, m.MissionID, 1, "ag_gate", "hdo_g1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "ag_src", "to_owner": "ag_gate", "offer_id": "hdo_g1"}, "t")); err != nil || !ok {
		t.Fatalf("发起: ok=%v %v", ok, err)
	}
	// 竞态窗口：服务层身份校验后、事务前，承接人被退役（SQL 直写模拟——门拦截方向）
	if _, err := store.db.Exec(`UPDATE agents SET status = 'retired' WHERE agent_id = 'ag_gate'`); err != nil {
		t.Fatal(err)
	}
	if out, ok, err := store.HandoverAccept(ctx, m.MissionID, 1, "hdo_g1",
		handoverEvent(mission.EventHandoverAccepted, map[string]any{}, "t")); err != nil || ok || out.OwnerAgentID != "" {
		t.Fatalf("retired 承接须 ok=false 且零翻转: ok=%v %+v %v", ok, out, err)
	}
	got, _, _ := store.MissionByID(ctx, m.MissionID)
	if got.OwnerAgentID != "ag_src" || got.PendingOwnerAgentID != "ag_gate" {
		t.Fatalf("零写入: %+v", got)
	}
}

// TestRetireCommitmentsGateIT 审校 #3b：退役事务内核对未处置承诺——owned 非终态
// 任务存在时 RetireAgent 零写入（done=false）；承接翻转后放行。
func TestRetireCommitmentsGateIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{AgentID: "ag_r1", DisplayName: "退役甲", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{AgentID: "ag_r2", DisplayName: "接手乙", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:01Z"}); err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_r1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// owned 非终态任务在——事务门拦（零写入；from 探针先于门读到 active 属实）
	if _, done, err := store.RetireAgent(ctx, "ag_r1", []string{"active"}, "清退", "t"); err != nil || done {
		t.Fatalf("门拦: done=%v %v", done, err)
	}
	var st string
	store.db.QueryRow(`SELECT status FROM agents WHERE agent_id = 'ag_r1'`).Scan(&st)
	if st != "active" {
		t.Fatalf("门拦须零写入（仍 active）: %s", st)
	}
	// 交接落地 → 门放行 → retired
	if _, ok, err := store.HandoverOffer(ctx, m.MissionID, 1, "ag_r2", "hdo_r1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"offer_id": "hdo_r1"}, "t")); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverAccept(ctx, m.MissionID, 1, "hdo_r1",
		handoverEvent(mission.EventHandoverAccepted, map[string]any{}, "t")); err != nil || !ok {
		t.Fatal(err)
	}
	if _, done, err := store.RetireAgent(ctx, "ag_r1", []string{"active"}, "清退", "t"); err != nil || !done {
		t.Fatalf("清单空后退役: done=%v %v", done, err)
	}
}

// TestHandoverAcceptReceiptStatusIT 审校 #4：回执注入事务内实际状态——offer（ready）
// 后并发暂停，accept 事务内 status=paused，回执记 paused（服务层预填 ready 不进事件）。
func TestHandoverAcceptReceiptStatusIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{AgentID: "ag_s2", DisplayName: "状态乙", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_src"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverOffer(ctx, m.MissionID, 1, "ag_s2", "hdo_s1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"offer_id": "hdo_s1"}, "t")); err != nil || !ok {
		t.Fatal(err)
	}
	// 并发暂停（服务层陈旧读 = ready；事务内实为 paused）
	if _, ok, err := store.PauseMission(ctx, m.MissionID, 1, "排空", "t"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverAccept(ctx, m.MissionID, 1, "hdo_s1",
		handoverEvent(mission.EventHandoverAccepted, map[string]any{"status_at_accept": "ready"}, "t")); err != nil || !ok {
		t.Fatal(err)
	}
	got, _, _ := store.MissionByID(ctx, m.MissionID)
	if got.Status != mission.StatusPaused || got.OwnerAgentID != "ag_s2" {
		t.Fatalf("承接保持 paused + 翻转: %+v", got)
	}
	evs, _, _ := store.MissionEvents(ctx, m.MissionID)
	for _, e := range evs {
		if e.EventType == mission.EventHandoverAccepted {
			if !strings.Contains(string(e.Payload), `"status_at_accept":"paused"`) {
				t.Fatalf("回执须注入实际 paused: %s", e.Payload)
			}
		}
	}
}

// TestHandoverAcceptCompletenessIT unassigned draft 承接 → ready + status_changed
// （reason=handover），事务内一次提交。
func TestHandoverAcceptCompletenessIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAgent(ctx, personalagent.Agent{AgentID: "ag_b", DisplayName: "乙", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	row := missionRow(mission.StatusDraft, "")
	row.MissionID = "mi_it_draft"
	if _, err := store.CreateMission(ctx, row, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverOffer(ctx, row.MissionID, 1, "ag_b", "hdo_it_d1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "", "to_owner": "ag_b", "offer_id": "hdo_it_d1"}, "t")); err != nil || !ok {
		t.Fatalf("draft 发起: ok=%v %v", ok, err)
	}
	acc, ok, err := store.HandoverAccept(ctx, row.MissionID, 1, "hdo_it_d1",
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
	if out, ok, err := store2.HandoverOffer(ctx, "mi_it_1", 1, "ag_b", "hdo_up_1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "ag_a", "to_owner": "ag_b", "offer_id": "hdo_up_1"}, "t")); err != nil || !ok || out.PendingOwnerAgentID != "ag_b" || out.PendingOfferID != "hdo_up_1" {
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

// TestHandoverFactsInjectedIT 复审 #2：事件事实由事务内现值生成。
// (a) offer：payload 预填陈旧 from_owner、事务前行内 owner 已被并发翻转（SQL 直写
// 模拟）→ 事件记事务内 owner；(b) accept：offer 后并发把状态带到 draft（SQL 直写，
// 模拟 paused→resume 重判交错）→ 完备契约按**现行 content** 重判 ready，不被
// 事务前快照放回 paused；回执 from_owner / status_at_accept 均为事务内值。
func TestHandoverFactsInjectedIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	for _, a := range []personalagent.Agent{
		{AgentID: "ag_fx", DisplayName: "事实乙", Status: "active", Source: "manual", CreatedAt: "2026-11-03T10:00:00Z"},
	} {
		if _, err := store.EnsureAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_src"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// (a) offer：事务前行内 owner 已翻为 ag_other（SQL 直写模拟并发承接）
	if _, err := store.db.Exec(`UPDATE missions SET owner_agent_id = 'ag_other' WHERE mission_id = ?`, m.MissionID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.HandoverOffer(ctx, m.MissionID, 1, "ag_fx", "hdo_fx1",
		handoverEvent(mission.EventHandoverOffered, map[string]any{"from_owner": "ag_stale", "to_owner": "ag_fx", "offer_id": "hdo_fx1"}, "t")); err != nil || !ok {
		t.Fatalf("offer: ok=%v %v", ok, err)
	}
	evs, _, _ := store.MissionEvents(ctx, m.MissionID)
	for _, e := range evs {
		if e.EventType == mission.EventHandoverOffered {
			if !strings.Contains(string(e.Payload), `"from_owner":"ag_other"`) || strings.Contains(string(e.Payload), "ag_stale") {
				t.Fatalf("offer 事件须注入事务内 owner: %s", e.Payload)
			}
		}
	}
	// (b) accept：并发把状态带到 draft——完备 content 重判 ready（旧实现按服务层
	// 快照 targetStatus 会放回 paused）
	if _, err := store.db.Exec(`UPDATE missions SET status = 'draft' WHERE mission_id = ?`, m.MissionID); err != nil {
		t.Fatal(err)
	}
	acc, ok, err := store.HandoverAccept(ctx, m.MissionID, 1, "hdo_fx1",
		handoverEvent(mission.EventHandoverAccepted, map[string]any{"status_at_accept": "draft"}, "t"))
	if err != nil || !ok {
		t.Fatalf("accept: ok=%v %v", ok, err)
	}
	if acc.Status != mission.StatusReady || acc.OwnerAgentID != "ag_fx" {
		t.Fatalf("draft 重判须按现行 content → ready: %+v", acc)
	}
	evs, _, _ = store.MissionEvents(ctx, m.MissionID)
	for _, e := range evs {
		if e.EventType == mission.EventHandoverAccepted {
			if !strings.Contains(string(e.Payload), `"status_at_accept":"ready"`) ||
				!strings.Contains(string(e.Payload), `"from_owner":"ag_other"`) {
				t.Fatalf("回执须记事务内事实: %s", e.Payload)
			}
		}
	}
}
