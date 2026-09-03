// UT 层：RFC-0012 群聊交互模型专项——agent 消息触发后续波、连续发言（v1.40
// 结构冷却拆除）、对话环检测强制收口、意愿静默终止、逐发言人生成时语境刷新。
package room

import (
	"context"
	"encoding/json"
	"fmt"
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

// 双座（echo + 恒 silent 桩）：人类消息 → 波1 echo 发言（quiet 自决不回）→
// echo 发言触发新窗口 → 波2 全员评估：echo 对自己消息礼貌自决 silent、quiet
// silent → quiescent 终止。验证：agent 消息是观察事件（触发后续波）+ 意愿静默。
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

	// 波1（published：echo 发言）→ 波2（quiescent：echo 礼貌静默、quiet silent）
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

// v1.40（结构冷却拆除）：agent 发言后不被一刀切跳过——下一条人类消息照常开波，
// 该 agent 连续参与两波发言（波1、波3 各一条；波2/波4 为其对自己消息的礼貌
// 静默收束）。@点名保留意愿排序前置语义（不再承担冷却豁免）。
func TestChatAgentSpeaksAgainOnNewMessage(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_at")
	deliverHuman(t, store, eng, "room_at")
	waitRoundsClosed(t, store, "room_at", 2) // 波1 published → 波2 quiescent（对自己消息礼貌静默）

	// 新人类消息（点名与否不影响开波，仅排序前置）→ 波3：echo 再次发言
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

	waitRoundsClosed(t, store, "room_at", 4) // 波3 published → 波4 quiescent（链收敛终态）
	time.Sleep(120 * time.Millisecond)
	events := store.RoomEvents("room_at")
	if n := countType(events, protocol.EventRoundOpened); n != 4 {
		t.Fatalf("新刺激应再开波：round.opened = %d（期望 4）：%v", n, typesOf(events))
	}
	if n := countAgentMsgsOf(events); n != 2 {
		t.Fatalf("echo 应连续两波各发一条：agent 消息 = %d（期望 2）", n)
	}
}

// TestChatGenerationContextRefresh v1.40 核心断言：同一波内后发者的生成语境必须
// 含先发者刚发布的消息（逐发言人生成时刷新——波内盲生成"互相对答上一条"的治本
// 位）。双 recap 座（同分，字典序 par_recap_a 先发）：a 生成时近窗仅人类刺激；
// b 生成时近窗含 a 的正文——若沿用开波快照，b 只能看到近窗=1。
func TestChatGenerationContextRefresh(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(recapAdapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_recap_a", Profile: agent.Profile{ProfileID: "pa", Adapter: "recap_stub"}},
			{ParticipantID: "par_recap_b", Profile: agent.Profile{ProfileID: "pb", Adapter: "recap_stub"}},
		},
		Budget:         contextx.Limits{},
		ReactionWindow: 5 * time.Millisecond,
		Clock:          testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_gen")
	deliverHuman(t, store, eng, "room_gen")
	waitRoundsClosed(t, store, "room_gen", 2) // 波1 双发 → 波2 对 agent 锚自决静默 quiescent
	time.Sleep(100 * time.Millisecond)

	bodies := map[string]string{}
	for _, ev := range store.RoomEvents("room_gen") {
		if ev.Type != protocol.EventMessagePosted || ev.Actor.Kind != "agent" {
			continue
		}
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		bodies[ev.Actor.ParticipantID] = p.Body
	}
	if len(bodies) != 2 {
		t.Fatalf("双座应各发一条：%v", bodies)
	}
	if bodies["par_recap_a"] != "recent=1;last=stimulus" {
		t.Fatalf("先发者应只见人类刺激（recent=1）：%q", bodies["par_recap_a"])
	}
	if want := "recent=2;last=" + bodies["par_recap_a"]; bodies["par_recap_b"] != want {
		t.Fatalf("后发者生成语境应含先发者消息（%q）：%q", want, bodies["par_recap_b"])
	}
}

// ---- recap 复述桩：意图仅对人类锚点表态；生成正文复述语境近窗（条数+尾条正文）----

type recapAdapter struct{}

func (recapAdapter) Name() string                     { return "recap_stub" }
func (recapAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (recapAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return recapSession{}, nil
}

type recapSession struct{}

func (recapSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return recapHandle{task: task}, nil
}
func (recapSession) Cancel(string) {}
func (recapSession) Close()        {}

type recapHandle struct{ task agent.Task }

func (recapHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (recapHandle) Cancel()                           {}

func (h recapHandle) Result() (agent.Result, error) {
	recent, _ := h.task.Context.Inline["recent"].([]map[string]any)
	lastKind, lastBody := "", ""
	if len(recent) > 0 {
		lastKind, _ = recent[len(recent)-1]["kind"].(string)
		lastBody, _ = recent[len(recent)-1]["body"].(string)
	}
	switch h.task.Kind {
	case agent.KindEvaluateIntent:
		action := "speak"
		if lastKind != "human" {
			action = "silent" // 锚=agent 消息（波2+）自决静默，链收敛
		}
		return agent.Result{Block: "turn_intent", Data: map[string]any{
			"action": action, "type": "extend", "public_rationale": "recap stub",
			"scores": map[string]any{"relevance": 0.5, "novelty": 0.5, "urgency": 0.5, "confidence": 0.5},
		}}, nil
	case agent.KindGenerate:
		return agent.Result{Block: "public_draft", Data: map[string]any{
			"body": fmt.Sprintf("recent=%d;last=%s", len(recent), lastBody),
		}}, nil
	}
	return agent.Result{Block: "unsupported"}, nil
}

// 对话环检测（形制导 v1.42）：闭环保龄——尾部连续 6 条 agent 消息出自 ≤2 说话人
// （无人类介入）→ 不开新波（强制收口）。单声音种子即病理形状。
func TestChatRingDetectionClosesStream(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_ring")
	var ring []protocol.Envelope
	for i := 0; i < defaultRingDyadTail; i++ {
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

	// 白盒直驱反应波（尾部全是同一 agent 的消息 → 闭环保龄命中，不开波零事件）
	eng.runReaction(context.Background(), "room_ring", false)
	time.Sleep(50 * time.Millisecond)
	if n := countType(store.RoomEvents("room_ring"), protocol.EventRoundOpened); n != 0 {
		t.Fatalf("对话环应强制收口：round.opened = %d（期望 0）", n)
	}
}

// TestRingShapeGate（v1.42 形制导环检测，纯函数）：病理=闭环保龄（尾部 ≤2 说话人
// 互答 ≥6）；多方轮转（≥3 说话人）是健康讨论，豁免短闸，仅受绝对兜底（≥30）——
// dogfood 实证：三方来源审计讨论（互相纠错、逐条核验）被旧计数闸在第 6 条误杀。
func TestRingShapeGate(t *testing.T) {
	seed := func(n int, speakers ...string) []StoredEvent {
		var out []StoredEvent
		k := 0
		for i := 0; i < n; i++ {
			pid := speakers[i%len(speakers)]
			out = append(out, StoredEvent{Envelope: protocol.Envelope{
				EventID: "evt_rs_" + jsonNumber(int64(k)), Actor: protocol.Actor{ParticipantID: pid, Kind: "agent"},
				Type: protocol.EventMessagePosted, Payload: []byte(`{"body":"r"}`),
			}})
			k++
		}
		return out
	}
	withHuman := func(events []StoredEvent) []StoredEvent { // 尾部人类消息重置
		return append(events, StoredEvent{Envelope: protocol.Envelope{
			EventID: "evt_rs_h", Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Type: protocol.EventMessagePosted, Payload: []byte(`{"body":"h"}`),
		}})
	}
	eng := NewEngine(EngineConfig{}) // 阈值默认：短闸 6 / 绝对兜底 30
	cases := []struct {
		name string
		want bool
		tail []StoredEvent
	}{
		{"单声音6条=病理", true, seed(6, "par_a")},
		{"双声音6条=病理", true, seed(6, "par_a", "par_b")},
		{"双声音5条未达", false, seed(5, "par_a", "par_b")},
		{"三方6条=健康讨论", false, seed(6, "par_a", "par_b", "par_c")},
		{"三方29条仍在讨论", false, seed(29, "par_a", "par_b", "par_c")},
		{"三方30条触绝对兜底", true, seed(30, "par_a", "par_b", "par_c")},
		{"人类插话重置尾部", false, withHuman(seed(40, "par_a"))},
	}
	for _, tc := range cases {
		if got := eng.ringTripped(tc.tail); got != tc.want {
			t.Fatalf("%s：ringTripped = %v（期望 %v）", tc.name, got, tc.want)
		}
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
