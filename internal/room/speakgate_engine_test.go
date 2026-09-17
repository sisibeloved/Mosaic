// 发言资格闸（附录 K）引擎级行为测试：闸拦截/点名豁免/软冷却/活读关闭/
// your_activity 语境注入。桩恒 speak（自决永不沉默）——沉默与否全由闸门
// 决定，正好隔离被测面。
package room

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// gateStub 可配分数的恒 speak 桩；捕获评估任务的 Inline（your_activity 断言）。
type gateStub struct {
	rel, urg float64
	mu       sync.Mutex
	seen     []map[string]any // 各次评估任务的 Inline
}

func (g *gateStub) Name() string                     { return "gate_stub" }
func (g *gateStub) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (g *gateStub) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return g, nil
}
func (g *gateStub) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	if task.Kind == agent.KindEvaluateIntent {
		g.mu.Lock()
		g.seen = append(g.seen, task.Context.Inline)
		g.mu.Unlock()
	}
	return gateHandle{g: g}, nil
}
func (g *gateStub) Cancel(string) {}
func (g *gateStub) Close()        {}

type gateHandle struct{ g *gateStub }

func (gateHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (gateHandle) Cancel()                           {}

func (h gateHandle) Result() (agent.Result, error) {
	switch {
	case h.g == nil:
		return agent.Result{Block: "unsupported"}, nil
	default:
		return agent.Result{Block: "turn_intent", Data: map[string]any{
			"action": "speak", "type": "extend", "public_rationale": "gate stub",
			"scores": map[string]any{
				"relevance": h.g.rel, "novelty": 0.5, "urgency": h.g.urg, "confidence": 0.5,
			},
		}}, nil
	}
}

func newGateEngine(t *testing.T, stub *gateStub, gate func() SpeakGateSettings) (*MemStore, *Engine) {
	t.Helper()
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(stub)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_gate", Profile: agent.Profile{ProfileID: "pg", Adapter: "gate_stub"}},
		},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now, NewID: counterNewID(), Tenant: "ten_local",
		SpeakGateFunc: gate,
	})
	return store, eng
}

// gatedIntents 取被闸意图（selected=false 且 reason 非空、action=speak）。
func gatedIntents(events []protocol.Envelope) []protocol.IntentRecordedPayload {
	var out []protocol.IntentRecordedPayload
	for _, ev := range events {
		if ev.Type != protocol.EventIntentRecorded {
			continue
		}
		var p protocol.IntentRecordedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		if !p.Selected && p.Action == "speak" && p.UnselectedReason != "" {
			out = append(out, p)
		}
	}
	return out
}

func TestSpeakGateBlocksLowScoreIntent(t *testing.T) {
	stub := &gateStub{rel: 0.1, urg: 0.1}     // 资格分 0.10 < 0.30
	store, eng := newGateEngine(t, stub, nil) // nil = 缺省束（闸开）
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_gate")
	deliverHuman(t, store, eng, "room_gate")

	waitRoundsClosed(t, store, "room_gate", 1)
	time.Sleep(100 * time.Millisecond)
	events := store.RoomEvents("room_gate")
	if n := countAgentMsgsOf(events); n != 0 {
		t.Fatalf("低分意图应被闸：agent 消息 = %d", n)
	}
	gated := gatedIntents(events)
	if len(gated) != 1 || gated[0].UnselectedReason != reasonBelowThreshold {
		t.Fatalf("应记录被闸意图（reason=%s）：%+v", reasonBelowThreshold, gated)
	}
}

func TestSpeakGateAddressedBypass(t *testing.T) {
	stub := &gateStub{rel: 0.1, urg: 0.1}
	store, eng := newGateEngine(t, stub, nil)
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_gate_at")
	env := protocol.Envelope{
		EventID: "evt_gate_at", TenantID: "ten_local", RoomID: "room_gate_at",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"body":"@par_gate 你说","addressed_to":["par_gate"]}`), Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
		t.Fatalf("append: %v", err)
	}
	eng.Deliver(context.Background(), outboxEntryOf(env))

	waitRoundsClosed(t, store, "room_gate_at", 1)
	time.Sleep(100 * time.Millisecond)
	events := store.RoomEvents("room_gate_at")
	if n := countAgentMsgsOf(events); n != 1 {
		t.Fatalf("点名豁免直通：agent 消息 = %d（期望 1）：%v", n, typesOf(events))
	}
	// 点名锚（causation=点名消息）的意图应豁免放行；链式波2（锚=agent 自己
	// 消息，无点名）低分被闸是闸门的正确行为，不在本用例断言范围。
	for _, ev := range events {
		if ev.Type != protocol.EventIntentRecorded || ev.CausationID == nil || *ev.CausationID != "evt_gate_at" {
			continue
		}
		var p protocol.IntentRecordedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		if !p.Selected {
			t.Fatalf("点名意图应豁免放行：%+v", p)
		}
	}
}

func TestSpeakGateCooldownGatesRecentSpeaker(t *testing.T) {
	// 贴线分 0.31 ≥ 0.30：波1（无冷却）过闸发布；波2（对自己消息的反应波，
	// SpokeLastWave=true）0.31-0.20=0.11 被闸，归因 recent_speaker。
	stub := &gateStub{rel: 0.35, urg: 0.25}
	store, eng := newGateEngine(t, stub, nil)
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_gate_cd")
	deliverHuman(t, store, eng, "room_gate_cd")

	waitRoundsClosed(t, store, "room_gate_cd", 2) // 波1 published → 波2 quiescent
	time.Sleep(100 * time.Millisecond)
	events := store.RoomEvents("room_gate_cd")
	if n := countAgentMsgsOf(events); n != 1 {
		t.Fatalf("波1 应发布、波2 应被冷却闸：agent 消息 = %d（期望 1）：%v", n, typesOf(events))
	}
	gated := gatedIntents(events)
	if len(gated) != 1 || gated[0].UnselectedReason != reasonRecentSpeaker {
		t.Fatalf("波2 被闸应归因 recent_speaker：%+v", gated)
	}
}

func TestSpeakGateDisabledByInjection(t *testing.T) {
	stub := &gateStub{rel: 0.1, urg: 0.1}
	off := func() SpeakGateSettings { return SpeakGateSettings{Enabled: false} }
	store, eng := newGateEngine(t, stub, off)
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_gate_off")
	deliverHuman(t, store, eng, "room_gate_off")

	waitRoundsClosed(t, store, "room_gate_off", 2) // 波1 published → 波2（自答循环？见下）
	time.Sleep(100 * time.Millisecond)
	events := store.RoomEvents("room_gate_off")
	if n := countAgentMsgsOf(events); n < 1 {
		t.Fatalf("闸关时低分意图应照发：%v", typesOf(events))
	}
}

func TestYourActivityInjectedIntoEvalContext(t *testing.T) {
	stub := &gateStub{rel: 0.9, urg: 0.9}
	store, eng := newGateEngine(t, stub, nil)
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_gate_ctx")
	deliverHuman(t, store, eng, "room_gate_ctx")
	waitRoundsClosed(t, store, "room_gate_ctx", 2) // 波1 published → 波2 quiescent
	time.Sleep(100 * time.Millisecond)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.seen) < 2 {
		t.Fatalf("至少应有两次评估（波1/波2），got %d", len(stub.seen))
	}
	first, ok := stub.seen[0]["your_activity"].(SeatActivity)
	if !ok {
		t.Fatalf("波1 评估语境应含 your_activity：%T", stub.seen[0]["your_activity"])
	}
	if first.SpokeLastWave || first.MessagesSince != -1 {
		t.Fatalf("波1 从未发言：应 SpokeLastWave=false/MessagesSince=-1，got %+v", first)
	}
	second, ok := stub.seen[1]["your_activity"].(SeatActivity)
	if !ok {
		t.Fatalf("波2 评估语境应含 your_activity：%T", stub.seen[1]["your_activity"])
	}
	if !second.SpokeLastWave || second.MessagesSince != 0 {
		t.Fatalf("波2 该座上波已发言且最新消息是自己：应 true/0，got %+v", second)
	}
	// 共享层不受 per-seat 注入污染（your_activity 之外的共享键仍在）。
	if _, ok := stub.seen[0]["stimulus_body"]; !ok {
		t.Fatal("共享语境键应保留（stimulus_body 丢失）")
	}
}
