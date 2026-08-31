// UT 层：room.InspectState——版本/epoch/暂停态从事件重建（与引擎门控同语义）。
package room

import (
	"encoding/json"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func inspectEvent(t *testing.T, roomID, typ string, seq int64, payload any) StoredEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return StoredEvent{Envelope: protocol.Envelope{
		EventID: "evt_x", RoomID: roomID, Type: typ, Seq: seq, Payload: raw,
	}}
}

func TestInspectState(t *testing.T) {
	events := []StoredEvent{
		inspectEvent(t, "room_a", protocol.EventRoomCreated, 1, map[string]any{"display_name": "x"}),
		inspectEvent(t, "room_a", protocol.EventMessagePosted, 2, map[string]any{"body": "hi"}),
		inspectEvent(t, "room_a", protocol.EventRoundOpened, 3, protocol.RoundOpenedPayload{RoundID: "rnd_1"}),
		inspectEvent(t, "room_a", protocol.EventRoundClosed, 6, protocol.RoundClosedPayload{RoundID: "rnd_1", Outcome: "published"}),
	}
	insp := InspectState(events)
	if insp.Version != 6 || insp.Epoch != 1 || insp.Paused {
		t.Fatalf("inspect = %+v, want version=6 epoch=1 paused=false", insp)
	}

	// 暂停语义与引擎一致：paused 后出现 started 即恢复
	events = append(events,
		inspectEvent(t, "room_a", protocol.EventRoomPaused, 7, map[string]any{"reason": "web"}),
	)
	if insp := InspectState(events); !insp.Paused {
		t.Fatal("paused 后 InspectState.Paused 应为 true")
	}
	events = append(events,
		inspectEvent(t, "room_a", protocol.EventRoomStarted, 8, map[string]any{"reason": ""}),
	)
	if insp := InspectState(events); insp.Paused || insp.Version != 8 {
		t.Fatalf("resume 后 inspect = %+v, want paused=false version=8", insp)
	}

	// 空房：全零值
	if insp := InspectState(nil); insp != (StateInspection{}) {
		t.Fatalf("空房 inspect = %+v, want 零值", insp)
	}
}
