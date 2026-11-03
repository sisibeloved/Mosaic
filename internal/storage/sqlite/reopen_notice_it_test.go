//go:build it

// IT 层（ADR-0034 / 0.6.11）：reopen_requested 事件投影为 kind=reopen 通知行
// （decision_ref/affected_count 快照）；v28 列存在；通知行投递复用既有路径。

package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/missionreturn"
)

// TestReturnNoticeReopenIT 重开通知：事件投影 → decision_ref/affected_count 快照
// → 卡片 mission_ref.kind=reopen + decision_ref + 轮次 0。
func TestReturnNoticeReopenIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	rs := ReturnStore{store}
	returnMission(t, store, "mi_ro", "room_ro", "tsk_ro", "重开冒烟")
	seedRoomLifecycle(t, store, "room_ro", "room.started", 1)

	// 直接写 reopen_requested 事件（绕过 Service——投影行为是存储面契约）
	payload, _ := json.Marshal(map[string]any{
		"request_id": "rop_it", "decision_ref": "closure:room_ro:seq7",
		"evidence_refs": []string{"doc:d1:r2"}, "affected_mission_ids": []string{"mi_a", "mi_b"},
		"reason": "统计口径有误", "contract_version": 1,
	})
	if _, err := store.db.Exec(`INSERT INTO mission_events (mission_id, event_type, payload, occurred_at)
		VALUES ('mi_ro', 'reopen_requested', ?, '2026-11-03T12:00:00Z')`, string(payload)); err != nil {
		t.Fatalf("seed reopen event: %v", err)
	}
	if _, err := rs.Project(ctx); err != nil {
		t.Fatalf("project: %v", err)
	}
	rows, _ := rs.ListReturnNotices(ctx, "mi_ro")
	if len(rows) != 1 {
		t.Fatalf("应 1 行: %+v", rows)
	}
	n := rows[0]
	if n.Kind != missionreturn.KindReopen || n.DecisionRef != "closure:room_ro:seq7" ||
		n.AffectedCount != 2 || n.ReviewRound != 0 || n.ContractVersion != 1 ||
		n.Reason != "统计口径有误" {
		t.Fatalf("快照: %+v", n)
	}
	if err := rs.Deliver(ctx, n, missionreturn.EnvelopeInput{EventID: "evt_ro", OccurredAt: "t", Body: "b"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	var envelope string
	if err := store.db.QueryRow(`SELECT envelope FROM room_events WHERE event_id = 'evt_ro'`).Scan(&envelope); err != nil {
		t.Fatalf("卡未落: %v", err)
	}
	var card struct {
		Payload struct {
			Body       string `json:"body"`
			MissionRef *struct {
				Kind        string `json:"kind"`
				DecisionRef string `json:"decision_ref"`
				ReviewRound int    `json:"review_round"`
			} `json:"mission_ref"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(envelope), &card); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if card.Payload.MissionRef == nil || card.Payload.MissionRef.Kind != "reopen" ||
		card.Payload.MissionRef.DecisionRef != "closure:room_ro:seq7" || card.Payload.MissionRef.ReviewRound != 0 {
		t.Fatalf("卡 mission_ref: %+v", card.Payload.MissionRef)
	}
	if card.Payload.Body == "" {
		t.Fatal("卡正文非空")
	}
}
