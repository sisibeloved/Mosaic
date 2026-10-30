package app

// 审校 P2#3 回归：roomTaskLookup 跨页累积——任务申报在后页而参与者索引在
// 前页（@指派解析依赖前页 participant.admitted）时，统一重建须命中派生
// TaskID；逐页重建会丢失上下文拒绝真实任务。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
)

// pagedReader 假 EventReader：每页一条、游标推进（page1=admitted 前页，
// page2=带 @指派的申报后页——提及解析必须看见前页参与者）。
type pagedReader struct {
	events []room.StoredEvent
}

func (p pagedReader) EventsAfter(_ context.Context, roomID, cursor string, limit int) ([]room.StoredEvent, string, error) {
	idx := 0
	if cursor != "" {
		for i, e := range p.events {
			if e.Envelope.EventID == cursor {
				idx = i + 1
				break
			}
		}
	}
	if idx >= len(p.events) {
		return nil, "", nil
	}
	one := p.events[idx : idx+1]
	var next string
	if idx+1 < len(p.events) {
		next = one[0].Envelope.EventID
	}
	return one, next, nil
}

func envelope(evID, typ string, actor protocol.Actor, payload any) room.StoredEvent {
	raw, _ := json.Marshal(payload)
	return room.StoredEvent{Envelope: protocol.Envelope{
		EventID: evID, TenantID: "ten_local", RoomID: "rm_cross",
		Seq: 1, Type: typ, SchemaVersion: 1,
		OccurredAt: "2026-10-30T12:00:00.000Z",
		Actor:      actor, Visibility: protocol.Visibility{Kind: "public"},
		Payload: raw,
	}}
}

// TestRoomTaskLookupCrossPage 跨页上下文：page1 admitted（kimi），page2 申报
// "@kimi 调研"——TaskID 以解析后 owner=par_kimi 派生；查找须命中。
func TestRoomTaskLookupCrossPage(t *testing.T) {
	events := []room.StoredEvent{
		envelope("evt_1", protocol.EventParticipantAdmitted, protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			map[string]any{"participant_id": "par_kimi"}),
		envelope("evt_2", protocol.EventMessagePosted, protocol.Actor{ParticipantID: "par_echo", Kind: "agent"},
			map[string]any{"body": "```mosaic-todo\n- [ ] @kimi 跨页调研\n```"}),
	}
	lookup := roomTaskLookup{reader: pagedReader{events: events}}

	want := room.TaskIDOf("evt_2", "par_kimi", "跨页调研")
	ok, err := lookup.RoomTaskExists(context.Background(), "rm_cross", want)
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	if !ok {
		t.Fatalf("跨页任务未被命中（提及解析丢失上下文）：want %s", want)
	}
	// 负例：不存在的任务卡
	if ok, _ := lookup.RoomTaskExists(context.Background(), "rm_cross", "tsk_none"); ok {
		t.Fatal("未知任务卡不应命中")
	}
}
