// UT 层：定向交锋快速通道（B3——RFC-0003 §3.1.9）。
package room

import (
	"context"
	"fmt"

	"encoding/json"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// deliverHumanAddressed 带点名的人类消息（直达引擎，绕过命令面以聚焦机制）。
func deliverHumanAddressed(t *testing.T, store *MemStore, eng *Engine, roomID, eventID string, addressed ...string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"body": "点名", "reply_to": nil, "addressed_to": addressed, "relations": []any{},
	})
	env := protocol.Envelope{
		EventID: eventID, TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: payload, Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, _ := json.Marshal(env)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: roomID, Envelope: raw})
}

// waitRoundsClosed 等到累计第 n 个 round.closed（多轮推进的等待语义）。
func waitRoundsClosed(t *testing.T, store *MemStore, roomID string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countType(store.RoomEvents(roomID), "round.closed") >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("第 %d 轮未完成：%v", n, typesOf(store.RoomEvents(roomID)))
}

// directedTestEngine：三座（echo ×3），max=2 的 open_floor（slotCap = ceil(2/2)=1）。
func directedTestEngine(t *testing.T) (*MemStore, *Engine, string) {
	t.Helper()
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	t.Cleanup(sup.Shutdown)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_a", Profile: agent.Profile{ProfileID: "pa", Adapter: "echo"}},
			{ParticipantID: "par_b", Profile: agent.Profile{ProfileID: "pb", Adapter: "echo"}},
			{ParticipantID: "par_c", Profile: agent.Profile{ProfileID: "pc", Adapter: "echo"}},
		},
		Budget: contextx.Limits{},
		Clock:  testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	// open_floor max=2（默认束即 3——显式收窄到 2 以钉 slotCap 语义）
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "dv0", TenantID: "ten_local", RoomID: "room_dir", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		{EventID: "dv_pol", TenantID: "ten_local", RoomID: "room_dir", Type: protocol.EventPolicyChanged,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"},
			Payload: mustMarshalForTest(func() protocol.PolicyParams {
				p := policyDefaults("open_floor")
				p.MaxSpeakers = 2
				return p
			}()), Metadata: map[string]any{}},
	})
	return store, eng, "room_dir"
}

func firstDirectedGrant(t *testing.T, events []protocol.Envelope) (protocol.FloorGrantedPayload, bool) {
	t.Helper()
	for _, ev := range events {
		if ev.Type != protocol.EventFloorGranted {
			continue
		}
		var g protocol.FloorGrantedPayload
		_ = json.Unmarshal(ev.Payload, &g)
		if g.Directed {
			return g, true
		}
	}
	return protocol.FloorGrantedPayload{}, false
}

// TestDirectedSlotPriorityAndCap：点名 par_c → par_c 获 rank-1 定向授予
// （优先资格 + 顺序前置）；slotCap=1 → 仅一个定向授予。
func TestDirectedSlotPriorityAndCap(t *testing.T) {
	store, eng, roomID := directedTestEngine(t)
	deliverHumanAddressed(t, store, eng, roomID, "evt_dir_h1", "par_c")
	waitRoundClosed(t, store, roomID)

	events := store.RoomEvents(roomID)
	var grants []protocol.FloorGrantedPayload
	directed := 0
	for _, ev := range events {
		if ev.Type == protocol.EventFloorGranted {
			var g protocol.FloorGrantedPayload
			_ = json.Unmarshal(ev.Payload, &g)
			grants = append(grants, g)
			if g.Directed {
				directed++
			}
		}
	}
	if len(grants) != 2 {
		t.Fatalf("max=2 应两授：%v", typesOf(events))
	}
	if directed != 1 {
		t.Fatalf("slotCap=1 应恰一个定向授予，got %d", directed)
	}
	if grants[0].ParticipantID != "par_c" || !grants[0].Directed || grants[0].Rank != 1 {
		t.Fatalf("被点名者应 rank-1 定向前置：%+v", grants[0])
	}
	if grants[1].Directed {
		t.Fatalf("第二个授予不应是定向：%+v", grants[1])
	}
}

// TestDirectedChainWindowShortening：连续定向（交锋链）→ round.opened 窗口缩短
// （20s × 2/3 → 14s）；深度超限回正常队列。
func TestDirectedChainWindowShortening(t *testing.T) {
	store, eng, roomID := directedTestEngine(t)

	// 第 1 轮：点名 par_a（链=1，首次点名不缩短）
	deliverHumanAddressed(t, store, eng, roomID, "evt_chain_h1", "par_a")
	waitRoundsClosed(t, store, roomID, 1)
	round1 := roundOpenedWindowOf(t, store.RoomEvents(roomID))
	if round1 != "20s" {
		t.Fatalf("链=1 不缩短，got %q", round1)
	}

	// 第 2 轮：再次点名（链=2 → 20s×2/3 → 14s）
	deliverHumanAddressed(t, store, eng, roomID, "evt_chain_h2", "par_a")
	waitRoundsClosed(t, store, roomID, 2)
	round2 := roundOpenedWindowOf(t, store.RoomEvents(roomID))
	if round2 != "14s" {
		t.Fatalf("链=2 应缩短到 14s，got %q", round2)
	}

	// 交锋链深度超限（连续 5 轮定向）→ 回正常窗口
	for i := 3; i <= 5; i++ {
		deliverHumanAddressed(t, store, eng, roomID, fmt.Sprintf("evt_chain_h%d", i), "par_a")
		waitRoundsClosed(t, store, roomID, i)
	}
	last := roundOpenedWindowOf(t, store.RoomEvents(roomID))
	if last != "20s" {
		t.Fatalf("链超限应回正常窗口，got %q", last)
	}
}

// TestDirectedSlotsForCap：上限推导 min(ceil(max/2), 2)。
func TestDirectedSlotsForCap(t *testing.T) {
	stim := protocol.Envelope{Type: protocol.EventMessagePosted,
		Payload: []byte(`{"addressed_to":["par_a"]}`), Metadata: map[string]any{}}
	seats := []AgentSeat{{ParticipantID: "par_a"}, {ParticipantID: "par_b"}}
	for _, tc := range []struct{ max, want int }{{1, 1}, {2, 1}, {3, 2}, {4, 2}, {8, 2}} {
		_, cap, _ := directedSlotsFor(nil, stim, seats, tc.max)
		if cap != tc.want {
			t.Fatalf("max=%d slotCap=%d（want %d）", tc.max, cap, tc.want)
		}
	}
}

func roundOpenedWindowOf(t *testing.T, events []protocol.Envelope) string {
	t.Helper()
	var last protocol.RoundOpenedPayload
	for _, ev := range events {
		if ev.Type == protocol.EventRoundOpened {
			_ = json.Unmarshal(ev.Payload, &last)
		}
	}
	return last.IntentWindow
}
