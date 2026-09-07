package room

import (
	"context"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func TestM41Debug2(t *testing.T) {
	store := NewMemStore()
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41c", TenantID: "ten_local", RoomID: "room_m41", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	eng := m41Engine(t, store)
	req := protocol.RunRequestedPayload{RunID: "run_h1", Assignee: "par_echo", Instruction: "做点事", Requester: "par_owner"}
	eng.LaunchRun("room_m41", req)
	for i := 0; i < 6; i++ {
		time.Sleep(200 * time.Millisecond)
		v, ok := runViewMaybe(store, "room_m41", "run_h1")
		t.Logf("poll %d ok=%v status=%q err=%q", i, ok, v.Status, v.Error)
		if ok && v.Status == "completed" {
			return
		}
	}
	events, _, _ := store.EventsAfter(context.Background(), "room_m41", "", 100)
	for _, ev := range events {
		t.Logf("EV %s %s", ev.Envelope.Type, string(ev.Envelope.Payload)[:min(60, len(string(ev.Envelope.Payload)))])
	}
	t.Fatal("debug2 未完成")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
