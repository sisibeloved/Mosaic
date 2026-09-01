// UT 层：读投影与快照四元组——回放一致性（同事件流两次投影逐位一致）、
// 版本三元组、水位游标、控制事件不入 Timeline。
package room

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func snapEvents() []StoredEvent {
	thread := "thr_root"
	return []StoredEvent{
		{Envelope: protocol.Envelope{EventID: "e1", RoomID: "r", Seq: 1, Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, OccurredAt: "t1"}, Cursor: "c1"},
		{Envelope: protocol.Envelope{EventID: "e2", RoomID: "r", Seq: 2, Type: protocol.EventRoundOpened,
			Actor: protocol.Actor{ParticipantID: "s", Kind: "system"}, OccurredAt: "t2"}, Cursor: "c2"},
		{Envelope: protocol.Envelope{EventID: "e3", RoomID: "r", Seq: 3, Type: protocol.EventMessagePosted,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, ThreadID: &thread,
			Payload: []byte(`{"body":"hi"}`), OccurredAt: "t3"}, Cursor: "c3"},
		{Envelope: protocol.Envelope{EventID: "e4", RoomID: "r", Seq: 4, Type: protocol.EventMessagePosted,
			Actor:   protocol.Actor{ParticipantID: "par_echo", Kind: "agent"},
			Payload: []byte(`{"body":"hello"}`), OccurredAt: "t4"}, Cursor: "c4"},
	}
}

func TestProjectSnapshotReplayConsistency(t *testing.T) {
	events := snapEvents()
	first := ProjectSnapshot("r", events)
	second := ProjectSnapshot("r", events)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("同事件流两次投影不一致（回放一致性被破坏）")
	}
}

func TestProjectSnapshotQuadruple(t *testing.T) {
	snap := ProjectSnapshot("r", snapEvents())
	if snap.RoomVersion != 4 {
		t.Fatalf("room_version = %d", snap.RoomVersion)
	}
	if snap.Watermark != "c4" {
		t.Fatalf("watermark = %s（应为末事件 opaque cursor）", snap.Watermark)
	}
	if snap.ProjectionVersion != ProjectionVersion || snap.AlgorithmVersion != AlgorithmVersion {
		t.Fatalf("版本三元组不符：%d/%d", snap.ProjectionVersion, snap.AlgorithmVersion)
	}
	// 系统事件（round.opened）入 Timeline（v1.25 持久化）+ 两条消息；无 seq 字段
	if len(snap.Timeline) != 3 {
		t.Fatalf("timeline = %d 项", len(snap.Timeline))
	}
	if snap.Timeline[0].Type != protocol.EventRoundOpened {
		t.Fatalf("timeline[0] 应为 round.opened：%+v", snap.Timeline[0])
	}
	if snap.Timeline[1].Body != "hi" || snap.Timeline[2].ActorKind != "agent" {
		t.Fatalf("timeline 内容不符：%+v", snap.Timeline)
	}
	if snap.Timeline[1].ThreadID == nil || *snap.Timeline[1].ThreadID != "thr_root" {
		t.Fatalf("thread_id 丢失：%v", snap.Timeline[1].ThreadID)
	}
	raw, _ := json.Marshal(snap.Timeline[1])
	if strings.Contains(string(raw), `"seq"`) {
		t.Fatalf("Timeline 项不得含 seq：%s", raw)
	}
}

func TestSnapshotJSONHasNoSeq(t *testing.T) {
	raw, err := json.Marshal(ProjectSnapshot("r", snapEvents()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if _, ok := doc["seq"]; ok {
		t.Fatal("快照顶层不得含 seq")
	}
}

// 房间名投影（UI 重设计切片 1）：room.created 命名、room.renamed 覆盖；
// participants 为装配层注入位——投影恒为空数组（ADR-0011 注记）。
func TestProjectSnapshotDisplayName(t *testing.T) {
	events := []StoredEvent{
		{Envelope: protocol.Envelope{EventID: "e1", RoomID: "r", Seq: 1, Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, OccurredAt: "t1",
			Payload: []byte(`{"display_name":"旧名","thread_id":"thr_root"}`)}, Cursor: "c1"},
		{Envelope: protocol.Envelope{EventID: "e2", RoomID: "r", Seq: 2, Type: protocol.EventRoomRenamed,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, OccurredAt: "t2",
			Payload: []byte(`{"display_name":"新名"}`)}, Cursor: "c2"},
	}
	snap := ProjectSnapshot("r", events)
	if snap.DisplayName != "新名" {
		t.Fatalf("display_name = %q（room.renamed 应覆盖 room.created）", snap.DisplayName)
	}
	raw, _ := json.Marshal(snap)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if parts, ok := doc["participants"].([]any); !ok || len(parts) != 0 {
		t.Fatalf("投影产物的 participants 应为空数组（装配层注入位）：%s", raw)
	}
}
