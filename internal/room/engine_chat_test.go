// UT 层：RFC-0012 群聊交互模型专项——agent 消息触发后续波、发言冷却与
// @点名豁免、对话环检测强制收口、意愿静默终止。
package room

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/outbox"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// seedRoomCreatedFor 建房种子（原 engine_auto_test 辅助，随文件退役迁此）。
func seedRoomCreatedFor(t *testing.T, store *MemStore, roomID string) {
	t.Helper()
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{{
		EventID: "evt_create_" + roomID, TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventRoomCreated, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: []byte(`{}`), Metadata: map[string]any{},
	}}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
}

// 双座（echo + 恒 silent 桩）：人类消息 → 波1 echo 发言（silent 自决不回）→
// echo 发言触发新窗口 → 波2 echo 冷却跳过、silent 意图 → quiescent 终止。
// 验证：agent 消息是观察事件（触发后续波）+ 冷却 + 意愿静默三语义。
func TestChatAgentMessageTriggersNextWave(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	_ = sup.Register(silentAdapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "pe", Adapter: "echo"}},
			{ParticipantID: "par_quiet", Profile: agent.Profile{ProfileID: "pq", Adapter: "silent_stub"}},
		},
		Budget:         contextx.Limits{},
		ReactionWindow: 5 * time.Millisecond,
		Clock:          testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_chat")
	deliverHuman(t, store, eng, "room_chat")

	// 波1（published：echo 发言）→ 波2（quiescent：echo 冷却、quiet silent）
	waitRoundsClosed(t, store, "room_chat", 2)
	time.Sleep(150 * time.Millisecond)
	events := store.RoomEvents("room_chat")
	if n := countType(events, protocol.EventRoundOpened); n != 2 {
		t.Fatalf("round.opened = %d（期望 2：发言波 + 静默波后终止）：%v", n, typesOf(events))
	}
	var outcomes []string
	for _, ev := range events {
		if ev.Type != protocol.EventRoundClosed {
			continue
		}
		var rc protocol.RoundClosedPayload
		_ = json.Unmarshal(ev.Payload, &rc)
		outcomes = append(outcomes, rc.Outcome)
	}
	if len(outcomes) != 2 || outcomes[0] != "published" || outcomes[1] != "quiescent" {
		t.Fatalf("波结局应为 published → quiescent（意愿静默终止）：%v", outcomes)
	}
}

// @点名豁免冷却：echo 发言进入冷却后，人类点名消息使 echo 重新评估并发言。
func TestChatAddressedExemptsCooldown(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_at")
	deliverHuman(t, store, eng, "room_at")
	waitRoundClosed(t, store, "room_at") // 波1：echo 发言 → 冷却
	time.Sleep(100 * time.Millisecond)
	if n := countType(store.RoomEvents("room_at"), protocol.EventRoundOpened); n != 1 {
		t.Fatalf("单座发言后应冷却终止：round.opened = %d（期望 1）", n)
	}

	// 点名消息：addressed_to 含 par_echo → 冷却豁免
	addressed := protocol.Envelope{
		EventID: "evt_at_ping", TenantID: "ten_local", RoomID: "room_at",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"body":"@par_echo 说两句","addressed_to":["par_echo"]}`), Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{addressed}); err != nil {
		t.Fatalf("append: %v", err)
	}
	eng.Deliver(context.Background(), outboxEntryOf(addressed))

	waitRoundsClosed(t, store, "room_at", 2)
	time.Sleep(120 * time.Millisecond)
	events := store.RoomEvents("room_at")
	if n := countType(events, protocol.EventRoundOpened); n != 2 {
		t.Fatalf("@点名应豁免冷却再开波：round.opened = %d（期望 2）：%v", n, typesOf(events))
	}
	if n := countAgentMsgsOf(events); n != 2 {
		t.Fatalf("echo 应被点名再次点名发言：agent 消息 = %d（期望 2）", n)
	}
}

// 对话环检测：尾部连续 6 条 agent 消息（无人类介入）→ 不开新波（强制收口）。
func TestChatRingDetectionClosesStream(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_ring")
	var ring []protocol.Envelope
	for i := 0; i < maxAgentMessageTail; i++ {
		ring = append(ring, protocol.Envelope{
			EventID: "evt_ring_" + jsonNumber(int64(i)), TenantID: "ten_local", RoomID: "room_ring",
			Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
			Actor:      protocol.Actor{ParticipantID: "par_echo", Kind: "agent"},
			Visibility: protocol.Visibility{Kind: "public"},
			Payload:    []byte(`{"body":"r"}`), Metadata: map[string]any{},
		})
	}
	if _, err := store.AppendEvents(context.Background(), ring); err != nil {
		t.Fatalf("append ring: %v", err)
	}

	// 白盒直驱反应波（尾部全是 agent 消息 → 环检测命中，不开波零事件）
	eng.runReaction(context.Background(), "room_ring", false)
	time.Sleep(50 * time.Millisecond)
	if n := countType(store.RoomEvents("room_ring"), protocol.EventRoundOpened); n != 0 {
		t.Fatalf("对话环应强制收口：round.opened = %d（期望 0）", n)
	}
}

// outboxEntryOf 测试信封 → outbox 条目。
func outboxEntryOf(env protocol.Envelope) outbox.Entry {
	raw, _ := json.Marshal(env)
	return outbox.Entry{RoomID: env.RoomID, Envelope: raw}
}

// TestWaveTimingRecorded 性能定位套件 v1：round.closed.metadata.timing 落库——
// 逐座评估耗时与汇总（串行求和）齐备；重启后经波链路投影可查。
func TestWaveTimingRecorded(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	_ = sup.Register(silentAdapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "pe", Adapter: "echo"}},
			{ParticipantID: "par_quiet", Profile: agent.Profile{ProfileID: "pq", Adapter: "silent_stub"}},
		},
		Budget:         contextx.Limits{},
		ReactionWindow: 5 * time.Millisecond,
		Clock:          testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_timing")
	deliverHuman(t, store, eng, "room_timing")
	waitRoundsClosed(t, store, "room_timing", 1)

	events, _, err := store.EventsAfter(context.Background(), "room_timing", "", 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		if ev.Envelope.Type != protocol.EventRoundClosed {
			continue
		}
		raw, ok := ev.Envelope.Metadata["timing"]
		if !ok {
			t.Fatalf("round.closed 缺 metadata.timing：%+v", ev.Envelope.Metadata)
		}
		b, _ := json.Marshal(raw)
		var tv struct {
			TotalMs     int64            `json:"total_ms"`
			EvalMs      map[string]int64 `json:"eval_ms"`
			EvalTotalMs int64            `json:"eval_total_ms"`
			GenerateMs  map[string]int64 `json:"generate_ms"`
		}
		if err := json.Unmarshal(b, &tv); err != nil {
			t.Fatalf("timing 解析: %v（%s）", err, b)
		}
		if tv.TotalMs < 0 || tv.EvalTotalMs < 0 {
			t.Fatalf("timing 汇总为负：%+v", tv)
		}
		if _, ok := tv.EvalMs["par_echo"]; !ok {
			t.Fatalf("逐座评估耗时缺 par_echo：%v", tv.EvalMs)
		}
		if _, ok := tv.EvalMs["par_quiet"]; !ok {
			t.Fatalf("逐座评估耗时缺 par_quiet：%v", tv.EvalMs)
		}
		return
	}
	t.Fatal("未找到 round.closed")
}
