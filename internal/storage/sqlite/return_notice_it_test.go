//go:build it

// IT 层（ADR-0033 / 0.6.10）：回房通知经真实 SQLite——migration v25、投影恰一次
// （游标 + source_seq UNIQUE）、Deliver 原子投递（paused/deleted 不落卡，卡与
// 标记同事务）、DeleteRoom 级联（待投取消 + 来源墓碑）、v24 旧库升级补投影、
// 台账/Due/退避状态机。

package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/missionreturn"
)

func seedRoomLifecycle(t *testing.T, store *Store, roomID, typ string, seq int64) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO room_events
		(event_id, tenant_id, room_id, seq, type, schema_version, occurred_at, envelope)
		VALUES (?, 'ten_it', ?, ?, ?, 1, '2026-11-02T10:00:00Z', '{}')`,
		"evt_seed_"+roomID+"_"+typ+"_"+jsonSeq(seq), roomID, seq, typ); err != nil {
		t.Fatalf("seed room event %s/%s: %v", roomID, typ, err)
	}
}

func jsonSeq(seq int64) string {
	b, _ := json.Marshal(seq)
	return string(b)
}

func seedMissionStatus(t *testing.T, store *Store, missionID, payload string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO mission_events (mission_id, event_type, payload, occurred_at)
		VALUES (?, 'status_changed', ?, '2026-11-02T10:00:00Z')`, missionID, payload); err != nil {
		t.Fatalf("seed mission event: %v", err)
	}
}

func returnMission(t *testing.T, store *Store, id, roomID, taskID, objective string) mission.Mission {
	t.Helper()
	m := missionRow(mission.StatusReady, "ag_rn")
	m.MissionID = id
	m.Content.Objective = objective
	var src *mission.RoomSource
	if roomID != "" {
		src = &mission.RoomSource{RoomID: roomID, TaskID: taskID}
	}
	out, err := store.CreateMission(context.Background(), m, src)
	if err != nil {
		t.Fatalf("建契约 %s: %v", id, err)
	}
	return out
}

// TestReturnNoticeProjectionIT 投影：四类过滤、无关/无来源不产行、快照定格、
// 重放恰一次（游标推进 + source_seq UNIQUE）。
func TestReturnNoticeProjectionIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	var v25 int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 25`).Scan(&v25); err != nil || v25 != 1 {
		t.Fatalf("migration v25 未登记: %d err=%v", v25, err)
	}
	long := strings.Repeat("纪", 100) // > 80 runes → 快照截断
	returnMission(t, store, "mi_rn_1", "room_rn_1", "tsk_rn_1", long)
	returnMission(t, store, "mi_rn_2", "", "", "无来源任务")
	seedRoomLifecycle(t, store, "room_rn_1", "room.started", 1)

	seedMissionStatus(t, store, "mi_rn_1", `{"from":"ready","to":"reviewing","package_id":"dlv_a","review_round":1}`)
	seedMissionStatus(t, store, "mi_rn_1", `{"from":"reviewing","to":"ready"}`) // 退回缺 decision → 无关
	seedMissionStatus(t, store, "mi_rn_2", `{"from":"ready","to":"accepted"}`)  // 无房间来源 → 不产行
	seedMissionStatus(t, store, "mi_rn_1", `{"from":"reviewing","to":"ready","decision":"returned","reason":"证据不足","reviewer":"rv1","review_round":1,"package_id":"dlv_a"}`)

	rs := ReturnStore{store}
	n, err := rs.Project(ctx)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if n != 2 {
		t.Fatalf("投影应新建 2 行，得 %d", n)
	}
	rows, err := rs.ListReturnNotices(ctx, "mi_rn_1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("台账应 2 行，得 %d", len(rows))
	}
	first, second := rows[0], rows[1]
	if first.Kind != missionreturn.KindReviewing || second.Kind != missionreturn.KindReturned {
		t.Fatalf("kind 序 = %s, %s", first.Kind, second.Kind)
	}
	if first.SourceSeq >= second.SourceSeq {
		t.Fatalf("台账应按 source_seq 升序: %d, %d", first.SourceSeq, second.SourceSeq)
	}
	if first.PackageID != "dlv_a" || first.ReviewRound != 1 || first.RoomID != "room_rn_1" || first.TaskID != "tsk_rn_1" {
		t.Fatalf("快照字段: %+v", first)
	}
	if second.Reason != "证据不足" || second.Reviewer != "rv1" {
		t.Fatalf("退回快照: %+v", second)
	}
	if got := []rune(first.Objective); len(got) != 80 {
		t.Fatalf("objective 快照应截 80 runes，得 %d", len(got))
	}
	if first.Status != missionreturn.StatusPending || first.Attempts != 0 {
		t.Fatalf("新行应 pending/0: %+v", first)
	}
	if rows2, _ := rs.ListReturnNotices(ctx, "mi_rn_2"); len(rows2) != 0 {
		t.Fatalf("无来源任务不应有通知: %d", len(rows2))
	}
	// 重放恰一次：游标已推进，重复 Project 不产第二行。
	if n, _ := rs.Project(ctx); n != 0 {
		t.Fatalf("重投影应 0 行，得 %d", n)
	}
	if rows, _ := rs.ListReturnNotices(ctx, "mi_rn_1"); len(rows) != 2 {
		t.Fatalf("重投影后台账仍应 2 行，得 %d", len(rows))
	}
}

// TestReturnNoticeDeliverIT 原子投递：卡 + 标记同落（outbox/FTS 同事务）、
// 幂等重投、暂停不落卡、删除不落卡、退避/Due 状态机。
func TestReturnNoticeDeliverIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rs := ReturnStore{store}

	// ── 正常投递 ──
	returnMission(t, store, "mi_dv_1", "room_dv_1", "tsk_dv_1", "整理会议纪要")
	seedRoomLifecycle(t, store, "room_dv_1", "room.started", 1)
	seedMissionStatus(t, store, "mi_dv_1", `{"from":"ready","to":"reviewing","package_id":"dlv_a","review_round":1}`)
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	due, err := rs.Due(ctx, "2026-11-02T10:00:00Z", 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %v %d", err, len(due))
	}
	n := due[0]
	env := missionreturn.EnvelopeInput{EventID: "evt_card_1", OccurredAt: "2026-11-02T10:00:01Z", Body: "📋 已提请验收"}
	if err := rs.Deliver(ctx, n, env); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	rows, _ := rs.ListReturnNotices(ctx, "mi_dv_1")
	if rows[0].Status != missionreturn.StatusDelivered || rows[0].RoomEventID != "evt_card_1" {
		t.Fatalf("投后行: %+v", rows[0])
	}
	var typ, envelope string
	if err := store.db.QueryRow(`SELECT type, envelope FROM room_events WHERE event_id = 'evt_card_1'`).Scan(&typ, &envelope); err != nil {
		t.Fatalf("卡未落 room_events: %v", err)
	}
	if typ != "message.posted" {
		t.Fatalf("卡类型 = %s", typ)
	}
	var card struct {
		Actor struct {
			ParticipantID string `json:"participant_id"`
			Kind          string `json:"kind"`
		} `json:"actor"`
		Payload struct {
			Body       string `json:"body"`
			MissionRef *struct {
				MissionID string `json:"mission_id"`
				Kind      string `json:"kind"`
				SourceSeq int64  `json:"source_seq"`
				TaskID    string `json:"task_id"`
			} `json:"mission_ref"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(envelope), &card); err != nil {
		t.Fatalf("卡信封解析: %v", err)
	}
	if card.Actor.ParticipantID != "par_system" || card.Actor.Kind != "system" {
		t.Fatalf("卡 actor = %+v", card.Actor)
	}
	if card.Payload.MissionRef == nil || card.Payload.MissionRef.MissionID != "mi_dv_1" ||
		card.Payload.MissionRef.Kind != "reviewing" || card.Payload.MissionRef.SourceSeq != n.SourceSeq ||
		card.Payload.MissionRef.TaskID != "tsk_dv_1" {
		t.Fatalf("卡 mission_ref = %+v", card.Payload.MissionRef)
	}
	if card.Payload.Body != "📋 已提请验收" {
		t.Fatalf("卡 body = %q", card.Payload.Body)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM outbox WHERE event_id = 'evt_card_1'`,
		`SELECT COUNT(*) FROM room_fts WHERE event_id = 'evt_card_1'`,
	} {
		var c int
		if err := store.db.QueryRow(q).Scan(&c); err != nil || c != 1 {
			t.Fatalf("outbox/FTS 应随卡同落: q=%s c=%d err=%v", q, c, err)
		}
	}
	// 幂等重投：行已 delivered → 拒绝，不落第二张卡。
	if err := rs.Deliver(ctx, n, env); !errors.Is(err, missionreturn.ErrNoticeNotPending) {
		t.Fatalf("重投应 ErrNoticeNotPending，得 %v", err)
	}
	var cards int
	store.db.QueryRow(`SELECT COUNT(*) FROM room_events WHERE room_id='room_dv_1' AND type='message.posted'`).Scan(&cards)
	if cards != 1 {
		t.Fatalf("房间应恰 1 张卡，得 %d", cards)
	}

	// ── 暂停：不落卡，行保持待投 ──
	returnMission(t, store, "mi_dv_2", "room_dv_2", "tsk_dv_2", "暂停房任务")
	seedRoomLifecycle(t, store, "room_dv_2", "room.started", 1)
	seedRoomLifecycle(t, store, "room_dv_2", "room.paused", 2)
	seedMissionStatus(t, store, "mi_dv_2", `{"from":"ready","to":"accepted","review_round":1,"reviewer":"rv"}`)
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	due2, _ := rs.Due(ctx, "2026-11-02T10:00:00Z", 10)
	if len(due2) != 1 || due2[0].MissionID != "mi_dv_2" {
		t.Fatalf("due2 = %+v", due2)
	}
	if err := rs.Deliver(ctx, due2[0], missionreturn.EnvelopeInput{EventID: "evt_card_2", OccurredAt: "t", Body: "b"}); !errors.Is(err, missionreturn.ErrRoomPaused) {
		t.Fatalf("暂停房应 ErrRoomPaused，得 %v", err)
	}
	rows2, _ := rs.ListReturnNotices(ctx, "mi_dv_2")
	if rows2[0].Status != missionreturn.StatusPending {
		t.Fatalf("暂停行应保持 pending: %+v", rows2[0])
	}
	var cards2 int
	store.db.QueryRow(`SELECT COUNT(*) FROM room_events WHERE room_id='room_dv_2' AND type='message.posted'`).Scan(&cards2)
	if cards2 != 0 {
		t.Fatalf("暂停房不应落卡，得 %d", cards2)
	}
	// 恢复（room.started 之后）→ 可投递。
	seedRoomLifecycle(t, store, "room_dv_2", "room.started", 3)
	if err := rs.Deliver(ctx, due2[0], missionreturn.EnvelopeInput{EventID: "evt_card_2b", OccurredAt: "t", Body: "b"}); err != nil {
		t.Fatalf("恢复后应可投递: %v", err)
	}

	// ── 删除：DeleteRoom 级联（待投取消 + 来源墓碑）+ 投递侧兜底 ──
	returnMission(t, store, "mi_dv_3", "room_dv_3", "tsk_dv_3", "将删房任务")
	seedRoomLifecycle(t, store, "room_dv_3", "room.started", 1)
	seedMissionStatus(t, store, "mi_dv_3", `{"from":"ready","to":"canceled"}`)
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := store.DeleteRoom(ctx, "room_dv_3"); err != nil {
		t.Fatalf("delete room: %v", err)
	}
	rows3, _ := rs.ListReturnNotices(ctx, "mi_dv_3")
	if len(rows3) != 1 || rows3[0].Status != missionreturn.StatusCanceled || rows3[0].LastError != "room_deleted" {
		t.Fatalf("删房后待投应 canceled(room_deleted): %+v", rows3)
	}
	var deletedAt string
	if err := store.db.QueryRow(`SELECT COALESCE(deleted_at, '') FROM mission_sources WHERE room_id='room_dv_3'`).Scan(&deletedAt); err != nil || deletedAt == "" {
		t.Fatalf("来源墓碑未落: %q err=%v", deletedAt, err)
	}
	if src := store.sourceOf(ctx, "mi_dv_3"); src == nil || src.DeletedAt == "" {
		t.Fatalf("sourceOf 应回填墓碑: %+v", src)
	}
	// 投递侧兜底：即便行仍待投（旧库/竞态），墓碑在场即拒。
	if err := rs.Deliver(ctx, missionreturn.Notice{NoticeID: rows3[0].NoticeID, RoomID: "room_dv_3"}, missionreturn.EnvelopeInput{EventID: "evt_card_3", OccurredAt: "t", Body: "b"}); !errors.Is(err, missionreturn.ErrNoticeNotPending) {
		t.Fatalf("已 canceled 行重投应 ErrNoticeNotPending，得 %v", err)
	}

	// ── 退避/Due 状态机 ──
	returnMission(t, store, "mi_dv_4", "room_dv_4", "tsk_dv_4", "退避任务")
	seedRoomLifecycle(t, store, "room_dv_4", "room.started", 1)
	seedMissionStatus(t, store, "mi_dv_4", `{"from":"ready","to":"reviewing","review_round":1}`)
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	due4, _ := rs.Due(ctx, "2026-11-02T10:00:00Z", 10)
	if len(due4) != 1 {
		t.Fatalf("due4 = %d", len(due4))
	}
	if err := rs.UpdateStatus(ctx, due4[0].NoticeID, missionreturn.StatusPending, 1, "2026-11-02T10:00:30Z", "boom"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if d, _ := rs.Due(ctx, "2026-11-02T10:00:10Z", 10); len(d) != 0 {
		t.Fatalf("退避未到期不应 due: %d", len(d))
	}
	if d, _ := rs.Due(ctx, "2026-11-02T10:00:30Z", 10); len(d) != 1 {
		t.Fatalf("到期应 due: %d", len(d))
	}
	if err := rs.UpdateStatus(ctx, due4[0].NoticeID, missionreturn.StatusFailed, 2, "", "boom2"); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if d, _ := rs.Due(ctx, "2026-11-02T11:00:00Z", 10); len(d) != 0 {
		t.Fatalf("failed 不应 due: %d", len(d))
	}
	if err := rs.UpdateStatus(ctx, due4[0].NoticeID, missionreturn.StatusPending, 3, "", ""); !errors.Is(err, missionreturn.ErrNoticeNotPending) {
		t.Fatalf("终态行更新应拒: %v", err)
	}
}

// TestReturnNoticeUpgradeIT v24 旧库升级：v25 表/列重建后，历史事件照常投影。
func TestReturnNoticeUpgradeIT(t *testing.T) {
	store, path := openTempStore(t)
	ctx := context.Background()
	returnMission(t, store, "mi_up_1", "room_up_1", "tsk_up_1", "升级前事实")
	seedRoomLifecycle(t, store, "room_up_1", "room.started", 1)
	seedMissionStatus(t, store, "mi_up_1", `{"from":"ready","to":"accepted","review_round":2,"reviewer":"rv"}`)
	// 降回 v24 形态：删 v25 表/列与登记。
	for _, stmt := range []string{
		`DROP TABLE mission_return_notices`,
		`DROP TABLE mission_return_state`,
		`ALTER TABLE mission_sources DROP COLUMN deleted_at`,
		`DELETE FROM migrations WHERE version = 25`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("降级 %s: %v", stmt, err)
		}
	}
	store.Close()

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen（v25 迁移）: %v", err)
	}
	t.Cleanup(func() { store2.Close() })
	var v25 int
	store2.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 25`).Scan(&v25)
	if v25 != 1 {
		t.Fatalf("v25 应重新登记: %d", v25)
	}
	rs := ReturnStore{store2}
	if n, err := rs.Project(ctx); err != nil || n != 1 {
		t.Fatalf("升级后投影历史事件应 1 行: n=%d err=%v", n, err)
	}
	rows, _ := rs.ListReturnNotices(ctx, "mi_up_1")
	if len(rows) != 1 || rows[0].Kind != missionreturn.KindAccepted || rows[0].ReviewRound != 2 || rows[0].Reviewer != "rv" {
		t.Fatalf("升级后台账: %+v", rows)
	}
	if err := rs.Deliver(ctx, rows[0], missionreturn.EnvelopeInput{EventID: "evt_up_card", OccurredAt: "t", Body: "b"}); err != nil {
		t.Fatalf("升级后投递: %v", err)
	}
}
