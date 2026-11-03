package room

// 0.6.10 审校 #3/#7（ADR-0033 v0.2）：回房系统卡的投影与锚点——快照与 SSE 两路
// 同形保留 mission_ref（刷新/重开不丢任务链接）；lastMessage 反应锚点跳过 system
// 消息（系统卡不抢占已排定回复的讨论锚点）。

import (
	"encoding/json"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func TestLastMessageSkipsSystem(t *testing.T) {
	human := StoredEvent{Envelope: protocol.Envelope{
		Type: protocol.EventMessagePosted, Seq: 3,
		Actor:   protocol.Actor{ParticipantID: "par_u", Kind: "human"},
		Payload: []byte(`{"body":"线程内点名提问"}`),
	}}
	sys := StoredEvent{Envelope: protocol.Envelope{
		Type: protocol.EventMessagePosted, Seq: 4,
		Actor:   protocol.Actor{ParticipantID: "par_system", Kind: "system"},
		Payload: []byte(`{"body":"✅ 验收通过","mission_ref":{"mission_id":"mi_1","kind":"accepted","contract_version":1,"task_id":"tsk_1","source_seq":9}}`),
	}}
	got := lastMessage([]StoredEvent{human, sys})
	if got == nil || got.Actor.Kind != "human" || got.Seq != 3 {
		t.Fatalf("锚点应跳过 system 指向人类消息: %+v", got)
	}
	if only := lastMessage([]StoredEvent{sys}); only != nil {
		t.Fatalf("仅系统消息时无锚点: %+v", only)
	}
}

func TestProjectSnapshotKeepsMissionRef(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"body": "✅ 验收通过",
		"mission_ref": map[string]any{
			"mission_id": "mi_p", "kind": "accepted", "contract_version": 1,
			"review_round": 2, "task_id": "tsk_p", "source_seq": 42,
		},
	})
	events := []StoredEvent{
		{Envelope: protocol.Envelope{EventID: "evt_pr1", RoomID: "room_p", Type: protocol.EventRoomCreated, Seq: 1,
			Actor: protocol.Actor{ParticipantID: "par_u", Kind: "human"}, Payload: []byte(`{"display_name":"回房投影"}`)}},
		{Envelope: protocol.Envelope{EventID: "evt_pr2", RoomID: "room_p", Type: protocol.EventMessagePosted, Seq: 2,
			Actor: protocol.Actor{ParticipantID: "par_system", Kind: "system"}, Payload: payload}},
	}
	snap := ProjectSnapshot("room_p", events)
	var item *TimelineItem
	for i := range snap.Timeline {
		if snap.Timeline[i].EventID == "evt_pr2" {
			item = &snap.Timeline[i]
		}
	}
	if item == nil {
		t.Fatal("快照缺消息项")
	}
	if item.MissionRef == nil || item.MissionRef.MissionID != "mi_p" || item.MissionRef.Kind != "accepted" ||
		item.MissionRef.ReviewRound != 2 || item.MissionRef.SourceSeq != 42 || item.MissionRef.TaskID != "tsk_p" {
		t.Fatalf("快照应保留 mission_ref（与 SSE 路同形）: %+v", item.MissionRef)
	}
}
