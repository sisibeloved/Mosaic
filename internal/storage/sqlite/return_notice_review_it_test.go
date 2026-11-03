//go:build it

// IT 层（0.6.10 审校修复 / ADR-0033 v0.2）：#1/#5 事实归属——投影按来源事件时点
// 还原当时契约版本/目标/轮次（payload 优先 + 事件流重建）；#2/#6 房首调度——
// 队首未到期同房不越序、暂停房积压不饿死他房；#8 v26 墓碑回填 + 投递侧兜底；
// 审校单列测试缺口——删房后才投影出的待投行 → ErrRoomDeleted 且零新增。

package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/missionreturn"
)

func seedMissionEventTyped(t *testing.T, store *Store, missionID, eventType, payload string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO mission_events (mission_id, event_type, payload, occurred_at)
		VALUES (?, ?, ?, '2026-11-02T10:00:00Z')`, missionID, eventType, payload); err != nil {
		t.Fatalf("seed mission event %s: %v", eventType, err)
	}
}

func insertNoticeRow(t *testing.T, store *Store, noticeID, roomID, missionID string, seq int64, kind, status, next string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO mission_return_notices
		(notice_id, source_seq, mission_id, room_id, task_id, kind, contract_version, review_round,
		 package_id, reviewer, reason, objective, status, attempts, next_attempt_at, last_error,
		 room_event_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'tsk_x', ?, 1, 0, '', '', '', '目标', ?, 0, ?, '', '',
		 '2026-11-02T10:00:00Z', '2026-11-02T10:00:00Z')`,
		noticeID, seq, missionID, roomID, kind, status, next); err != nil {
		t.Fatalf("insert notice %s: %v", noticeID, err)
	}
}

// TestReturnNoticeHeadOrderingIT #2/#6：先取各房队首、再判到期——队首退避/复查中
// 同房后继不越序（accepted 不抢在 reviewing 前）；每房每轮一条，暂停房积压不占
// 满批次饿死他房。
func TestReturnNoticeHeadOrderingIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rs := ReturnStore{store}
	seedRoomLifecycle(t, store, "room_h1", "room.started", 1)
	seedRoomLifecycle(t, store, "room_h2", "room.started", 1)
	// 房 1：队首退避中（next 未来）、后继到期；房 2：唯一通知到期。
	insertNoticeRow(t, store, "rn_h1a", "room_h1", "mi_h1", 11, "reviewing", "pending", "2030-01-01T00:00:00Z")
	insertNoticeRow(t, store, "rn_h1b", "room_h1", "mi_h1", 12, "accepted", "pending", "")
	insertNoticeRow(t, store, "rn_h2", "room_h2", "mi_h2", 13, "canceled", "pending", "")
	due, err := rs.Due(ctx, "2026-11-02T10:00:00Z", 100)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 || due[0].NoticeID != "rn_h2" {
		t.Fatalf("队首未到期的房不取后继，他房照常: %+v", due)
	}
	// 房 1 队首到期 → 队首入列（房 2 仍未投仍在列）；投出房 1 队首后后继成为新队首。
	if _, err := store.db.Exec(`UPDATE mission_return_notices SET next_attempt_at = '' WHERE notice_id = 'rn_h1a'`); err != nil {
		t.Fatalf("reset next: %v", err)
	}
	due2, _ := rs.Due(ctx, "2026-11-02T10:00:00Z", 100)
	if len(due2) != 2 || due2[0].NoticeID != "rn_h1a" || due2[1].NoticeID != "rn_h2" {
		t.Fatalf("到期只取队首（房 2 仍未投）: %+v", due2)
	}
	if err := rs.Deliver(ctx, due2[0], missionreturn.EnvelopeInput{EventID: "evt_h1a", OccurredAt: "t", Body: "b"}); err != nil {
		t.Fatalf("deliver head: %v", err)
	}
	due3, _ := rs.Due(ctx, "2026-11-02T10:00:00Z", 100)
	if len(due3) != 2 || due3[0].NoticeID != "rn_h1b" || due3[1].NoticeID != "rn_h2" {
		t.Fatalf("后继成为新队首: %+v", due3)
	}
	// 暂停房积压 205 条：每房每轮仍只占一个名额，正常房不被饿死。
	seedRoomLifecycle(t, store, "room_h3", "room.started", 1)
	seedRoomLifecycle(t, store, "room_h4", "room.started", 1)
	for i := 0; i < 205; i++ {
		insertNoticeRow(t, store, fmt.Sprintf("rn_h3_%03d", i), "room_h3", "mi_h3", 1000+int64(i), "reviewing", "pending", "")
	}
	insertNoticeRow(t, store, "rn_h4", "room_h4", "mi_h4", 2000, "canceled", "pending", "")
	due4, _ := rs.Due(ctx, "2026-11-02T10:00:00Z", 100)
	rooms := map[string]int{}
	for _, n := range due4 {
		rooms[n.RoomID]++
	}
	if len(due4) != 4 || rooms["room_h3"] != 1 || rooms["room_h4"] != 1 || rooms["room_h1"] != 1 || rooms["room_h2"] != 1 {
		t.Fatalf("各房恰队首（4 房各 1）：n=%d rooms=%v", len(due4), rooms)
	}
}

// TestReturnNoticeLegacyFactsIT #1/#5：v1 送验退回后修订 v2、再送验——旧式载荷
// （无版本/轮次）经投影重放：前两件归属 v1（v1 目标快照）、轮次 1；v2 送验归属
// v2、轮次 2；新式载荷（带 contract_version）优先于事件流重建。
func TestReturnNoticeLegacyFactsIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rs := ReturnStore{store}
	returnMission(t, store, "mi_lg", "room_lg", "tsk_lg", "v1 目标字符串")
	seedRoomLifecycle(t, store, "room_lg", "room.started", 1)
	// v1 期事实（旧式载荷：无 contract_version / review_round）
	seedMissionStatus(t, store, "mi_lg", `{"from":"ready","to":"reviewing","package_id":"dlv_old"}`)
	seedMissionStatus(t, store, "mi_lg", `{"from":"reviewing","to":"ready","decision":"returned","reason":"补证","review_round":1,"package_id":"dlv_old"}`)
	// 修订 v2：版本行 + contract_revised + 投影行推进
	raw, _ := json.Marshal(mission.Content{Objective: "v2 目标完全不同", Scope: "s", AcceptanceCriteria: []string{"a"}, DeliverableSpecs: []string{"d"}})
	if _, err := store.db.Exec(`INSERT INTO mission_contract_versions
		(mission_id, version, status, owner_agent_id, requester, content, created_at)
		VALUES ('mi_lg', 2, 'ready', 'ag_rn', 'owner', ?, '2026-11-02T10:00:05Z')`, string(raw)); err != nil {
		t.Fatalf("版本行 v2: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE missions SET contract_version = 2, objective = 'v2 目标完全不同' WHERE mission_id = 'mi_lg'`); err != nil {
		t.Fatalf("推进投影行: %v", err)
	}
	seedMissionEventTyped(t, store, "mi_lg", "contract_revised", `{"from_version":1,"to_version":2}`)
	seedMissionStatus(t, store, "mi_lg", `{"from":"ready","to":"reviewing","package_id":"dlv_new"}`)
	// 新式载荷（审校后事件随事务写入 contract_version）优先于事件流重建。
	seedMissionStatus(t, store, "mi_lg", `{"from":"reviewing","to":"canceled","contract_version":9}`)

	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	rows, _ := rs.ListReturnNotices(ctx, "mi_lg")
	if len(rows) != 4 {
		t.Fatalf("应 4 行: %+v", rows)
	}
	n1, n2, n3, n4 := rows[0], rows[1], rows[2], rows[3]
	if n1.Kind != missionreturn.KindReviewing || n1.ContractVersion != 1 || n1.ReviewRound != 1 ||
		n1.PackageID != "dlv_old" || n1.Objective != "v1 目标字符串" {
		t.Fatalf("v1 送验应归属 v1/轮 1/v1 目标: %+v", n1)
	}
	if n2.Kind != missionreturn.KindReturned || n2.ContractVersion != 1 || n2.ReviewRound != 1 {
		t.Fatalf("v1 退回应归属 v1: %+v", n2)
	}
	if n3.Kind != missionreturn.KindReviewing || n3.ContractVersion != 2 || n3.ReviewRound != 2 ||
		n3.PackageID != "dlv_new" || n3.Objective != "v2 目标完全不同" {
		t.Fatalf("v2 送验应归属 v2/轮 2/v2 目标: %+v", n3)
	}
	if n4.Kind != missionreturn.KindCanceled || n4.ContractVersion != 9 {
		t.Fatalf("载荷版本优先: %+v", n4)
	}
	// 卡片信封同口径：v1 事实的 mission_ref 契约版本/轮次 + 正文含 v1 目标。
	if err := rs.Deliver(ctx, n1, missionreturn.EnvelopeInput{EventID: "evt_lg1", OccurredAt: "t", Body: "x"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	var envelope string
	if err := store.db.QueryRow(`SELECT envelope FROM room_events WHERE event_id = 'evt_lg1'`).Scan(&envelope); err != nil {
		t.Fatalf("卡未落: %v", err)
	}
	var card struct {
		Payload struct {
			Body       string `json:"body"`
			MissionRef *struct {
				ContractVersion int   `json:"contract_version"`
				ReviewRound     int   `json:"review_round"`
				SourceSeq       int64 `json:"source_seq"`
			} `json:"mission_ref"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(envelope), &card); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if card.Payload.MissionRef == nil || card.Payload.MissionRef.ContractVersion != 1 || card.Payload.MissionRef.ReviewRound != 1 {
		t.Fatalf("卡 mission_ref 应 v1/轮 1: %+v", card.Payload.MissionRef)
	}
}

// TestReturnNoticeDeletedPendingIT 审校单列测试缺口 + #8 投递侧兜底：删房后才
// 投影出的待投行（墓碑在场、行仍 pending）→ ErrRoomDeleted，卡/outbox/FTS 零
// 新增；且投递侧回填来源墓碑（级联与 v26 之外的补口）。
func TestReturnNoticeDeletedPendingIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rs := ReturnStore{store}
	returnMission(t, store, "mi_dp", "room_dp", "tsk_dp", "删房后投影")
	seedRoomLifecycle(t, store, "room_dp", "room.started", 1)
	if err := store.DeleteRoom(ctx, "room_dp"); err != nil {
		t.Fatalf("delete room: %v", err)
	}
	// 删房后才出现的待投行 + 抹掉级联回填（模拟旧库/滞后投影形态）
	var tombAt string
	if err := store.db.QueryRow(`SELECT deleted_at FROM room_tombstones WHERE room_id='room_dp'`).Scan(&tombAt); err != nil || tombAt == "" {
		t.Fatalf("墓碑时间: %q err=%v", tombAt, err)
	}
	insertNoticeRow(t, store, "rn_dp", "room_dp", "mi_dp", 77, "reviewing", "pending", "")
	if _, err := store.db.Exec(`UPDATE mission_sources SET deleted_at = NULL WHERE room_id = 'room_dp'`); err != nil {
		t.Fatalf("抹墓碑: %v", err)
	}
	due, _ := rs.Due(ctx, "2026-11-02T10:00:00Z", 10)
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	err := rs.Deliver(ctx, due[0], missionreturn.EnvelopeInput{EventID: "evt_dp", OccurredAt: "t", Body: "b"})
	if !errors.Is(err, missionreturn.ErrRoomDeleted) {
		t.Fatalf("应 ErrRoomDeleted: %v", err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM room_events WHERE room_id='room_dp'`,
		`SELECT COUNT(*) FROM outbox WHERE room_id='room_dp'`,
		`SELECT COUNT(*) FROM room_fts WHERE room_id='room_dp'`,
	} {
		var c int
		if err := store.db.QueryRow(q).Scan(&c); err != nil || c != 0 {
			t.Fatalf("删房后应零新增: q=%s c=%d err=%v", q, c, err)
		}
	}
	if rows, _ := rs.ListReturnNotices(ctx, "mi_dp"); len(rows) != 1 || rows[0].Status != missionreturn.StatusPending {
		t.Fatalf("行应保持 pending（终态化归 worker）: %+v", rows)
	}
	var deletedAt string
	if err := store.db.QueryRow(`SELECT COALESCE(deleted_at,'') FROM mission_sources WHERE room_id='room_dp'`).Scan(&deletedAt); err != nil || deletedAt == "" {
		t.Fatalf("投递侧应兜底回填来源墓碑: %q err=%v", deletedAt, err)
	}
	// 复审 #7：回填取房间**实际**删除时间（墓碑），不是投递时才发现的时间。
	if deletedAt != tombAt {
		t.Fatalf("回填应为墓碑原删除时间: %q != %q", deletedAt, tombAt)
	}
}

// TestReturnNoticeTombstoneUpgradeIT #8：v24 形态（删房先于 v25 加列）升级——
// v26 按 room_tombstones 原删除时间回填 mission_sources.deleted_at。
func TestReturnNoticeTombstoneUpgradeIT(t *testing.T) {
	store, path := openTempStore(t)
	ctx := context.Background()
	returnMission(t, store, "mi_tb", "room_tb", "tsk_tb", "升级回填")
	seedRoomLifecycle(t, store, "room_tb", "room.started", 1)
	if err := store.DeleteRoom(ctx, "room_tb"); err != nil {
		t.Fatalf("delete room: %v", err)
	}
	var tombAt string
	if err := store.db.QueryRow(`SELECT deleted_at FROM room_tombstones WHERE room_id='room_tb'`).Scan(&tombAt); err != nil || tombAt == "" {
		t.Fatalf("墓碑时间: %q err=%v", tombAt, err)
	}
	// 降回 v24 形态：墓碑列未回填、v25 表/列/登记（25+26）撤销，墓碑保留。
	if _, err := store.db.Exec(`UPDATE mission_sources SET deleted_at = NULL WHERE room_id = 'room_tb'`); err != nil {
		t.Fatalf("抹回填: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE mission_return_notices`,
		`DROP TABLE mission_return_state`,
		`ALTER TABLE mission_sources DROP COLUMN deleted_at`,
		`DELETE FROM migrations WHERE version IN (25, 26)`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("降级 %s: %v", stmt, err)
		}
	}
	store.Close()

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen（v25+v26）: %v", err)
	}
	t.Cleanup(func() { store2.Close() })
	for _, v := range []int{25, 26} {
		var c int
		store2.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = ?`, v).Scan(&c)
		if c != 1 {
			t.Fatalf("migration v%d 未登记", v)
		}
	}
	var backfilled string
	if err := store2.db.QueryRow(`SELECT COALESCE(deleted_at,'') FROM mission_sources WHERE room_id='room_tb'`).Scan(&backfilled); err != nil || backfilled != tombAt {
		t.Fatalf("v26 应回填原删除时间: %q != %q err=%v", backfilled, tombAt, err)
	}
	if src := store2.sourceOf(ctx, "mi_tb"); src == nil || src.DeletedAt != tombAt {
		t.Fatalf("sourceOf 应回填墓碑: %+v", src)
	}
}

// TestReturnNoticeRoundPairingIT 复审 #5：裁定轮次按同包送验还原——v22 形态
// （送验/退回事件均无轮次）三轮同流程后，每对送验/退回轮次一致（1/1、2/2、3/3），
// 不出现"送验第 3 轮、退回第 1 轮"的错位。
func TestReturnNoticeRoundPairingIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rs := ReturnStore{store}
	returnMission(t, store, "mi_rp", "room_rp", "tsk_rp", "轮次配对")
	seedRoomLifecycle(t, store, "room_rp", "room.started", 1)
	// 三轮送验→退回，全部旧式载荷（无 contract_version / review_round）。
	for i := 0; i < 3; i++ {
		seedMissionStatus(t, store, "mi_rp", `{"from":"ready","to":"reviewing","package_id":"dlv_p"}`)
		seedMissionStatus(t, store, "mi_rp", `{"from":"reviewing","to":"ready","decision":"returned","reason":"再补证","package_id":"dlv_p"}`)
	}
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	rows, _ := rs.ListReturnNotices(ctx, "mi_rp")
	if len(rows) != 6 {
		t.Fatalf("应 6 行: %+v", rows)
	}
	for i := 0; i < 3; i++ {
		req, ret := rows[2*i], rows[2*i+1]
		want := i + 1
		if req.Kind != missionreturn.KindReviewing || ret.Kind != missionreturn.KindReturned {
			t.Fatalf("序 %d 类型: %+v %+v", i, req, ret)
		}
		if req.ReviewRound != want || ret.ReviewRound != want {
			t.Fatalf("同包配对应同轮（第 %d 轮）: 送验=%d 退回=%d", want, req.ReviewRound, ret.ReviewRound)
		}
	}
	// 载荷轮次优先（新式裁定事件带轮次不重建）。
	seedMissionStatus(t, store, "mi_rp", `{"from":"ready","to":"reviewing","package_id":"dlv_q"}`)
	seedMissionStatus(t, store, "mi_rp", `{"from":"reviewing","to":"ready","decision":"returned","reason":"r","package_id":"dlv_q","review_round":9}`)
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project 2: %v", err)
	}
	rows2, _ := rs.ListReturnNotices(ctx, "mi_rp")
	if n := len(rows2); n != 8 {
		t.Fatalf("应 8 行: %d", n)
	}
	if got := rows2[7].ReviewRound; got != 9 {
		t.Fatalf("载荷轮次优先: %d", got)
	}
}

// TestReturnNoticeBOMObjectiveIT 复审 #6：v1 目标仅一个 U+FEFF（创建口径合法），
// 修订 v2 后投影 v1 历史事实——快照保留**空**摘要，不回退 v2 目标。
func TestReturnNoticeBOMObjectiveIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rs := ReturnStore{store}
	returnMission(t, store, "mi_bm", "room_bm", "tsk_bm", "\ufeff")
	seedRoomLifecycle(t, store, "room_bm", "room.started", 1)
	seedMissionStatus(t, store, "mi_bm", `{"from":"ready","to":"reviewing","package_id":"dlv_b1"}`)
	// 修订 v2（目标不同）+ 事件与版本行。
	raw, _ := json.Marshal(mission.Content{Objective: "v2 目标", Scope: "s", AcceptanceCriteria: []string{"a"}, DeliverableSpecs: []string{"d"}})
	if _, err := store.db.Exec(`INSERT INTO mission_contract_versions
		(mission_id, version, status, owner_agent_id, requester, content, created_at)
		VALUES ('mi_bm', 2, 'ready', 'ag_rn', 'owner', ?, '2026-11-02T10:00:05Z')`, string(raw)); err != nil {
		t.Fatalf("版本行 v2: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE missions SET contract_version = 2, objective = 'v2 目标' WHERE mission_id = 'mi_bm'`); err != nil {
		t.Fatalf("推进投影行: %v", err)
	}
	seedMissionEventTyped(t, store, "mi_bm", "contract_revised", `{"from_version":1,"to_version":2}`)
	seedMissionStatus(t, store, "mi_bm", `{"from":"ready","to":"reviewing","package_id":"dlv_b2"}`)
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	rows, _ := rs.ListReturnNotices(ctx, "mi_bm")
	if len(rows) != 2 {
		t.Fatalf("应 2 行: %+v", rows)
	}
	if rows[0].Objective != "" || rows[0].ContractVersion != 1 {
		t.Fatalf("v1 历史快照应保留空摘要（BOM 目标）不回退 v2: %+v", rows[0])
	}
	if rows[1].Objective != "v2 目标" || rows[1].ContractVersion != 2 {
		t.Fatalf("v2 事实归属 v2 目标: %+v", rows[1])
	}
}
