// UT 层：房间引擎——人类消息驱动完整轮事件链（echo 适配器）。
package room

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func TestEngineRoundProducesEventChain(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	if err := sup.Register(echo.Adapter{}); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	defer sup.Shutdown()

	var idMu chan struct{} = make(chan struct{}, 1)
	idMu <- struct{}{}
	var n int64
	newID := func(prefix string) string {
		<-idMu
		n++
		idMu <- struct{}{}
		return prefix + "_eng_" + jsonNumber(n)
	}

	eng := NewEngine(EngineConfig{
		Store:  store,
		Reader: store,
		Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "prof_echo", Adapter: "echo"}}},
		Policy: attention.Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.30, Weights: attention.DefaultWeights},
		Clock:  testClock,
		Now:    func() time.Time { return time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC) },
		NewID:  newID,
		Tenant: "ten_local",
		RoomID: "room_eng",
	})

	// 种子：room.created + 人类消息（经真实 append 路径）
	seedHuman := protocol.Envelope{
		EventID:       "evt_eng_seed",
		TenantID:      "ten_local",
		RoomID:        "room_eng",
		Type:          protocol.EventRoomCreated,
		SchemaVersion: 1,
		OccurredAt:    testClock(),
		Actor:         protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       []byte(`{"display_name":"eng"}`),
		Metadata:      map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{seedHuman}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	human := protocol.Envelope{
		EventID:       "evt_eng_human",
		TenantID:      "ten_local",
		RoomID:        "room_eng",
		Type:          protocol.EventMessagePosted,
		SchemaVersion: 1,
		OccurredAt:    testClock(),
		Actor:         protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       []byte(`{"body":"讨论开始"}`),
		Metadata:      map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{human}); err != nil {
		t.Fatalf("seed human msg: %v", err)
	}

	raw, _ := json.Marshal(human)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_eng", Envelope: raw})

	// 轮异步：轮询直到 round.closed（超时 3s）
	deadline := time.Now().Add(3 * time.Second)
	var events []protocol.Envelope
	for time.Now().Before(deadline) {
		events = store.RoomEvents("room_eng")
		if len(events) > 0 && events[len(events)-1].Type == protocol.EventRoundClosed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(events) == 0 || events[len(events)-1].Type != protocol.EventRoundClosed {
		t.Fatalf("轮未完成，事件数=%d", len(events))
	}

	// 事件链与顺序
	wantTypes := []string{
		protocol.EventRoomCreated,
		protocol.EventMessagePosted, // human
		protocol.EventRoundOpened,
		protocol.EventIntentRecorded,
		protocol.EventFloorGranted,
		protocol.EventMessagePosted, // agent
		protocol.EventRoundClosed,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("事件数 = %d（期望 %d）：%v", len(events), len(wantTypes), typesOf(events))
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("第 %d 个事件 = %s（期望 %s）：%v", i, events[i].Type, want, typesOf(events))
		}
		if events[i].Seq != int64(i+1) {
			t.Fatalf("%s seq = %d（期望 %d）", events[i].Type, events[i].Seq, i+1)
		}
	}

	// causation 纪律（RFC-0003）：agent 发言 causation 指向 floor.granted；grant 指向 intent.recorded
	agentMsg := events[5]
	grant := events[4]
	intent := events[3]
	if agentMsg.CausationID == nil || *agentMsg.CausationID != grant.EventID {
		t.Fatalf("agent 消息 causation 应指向 floor.granted：%v", agentMsg.CausationID)
	}
	if grant.CausationID == nil || *grant.CausationID != intent.EventID {
		t.Fatalf("grant causation 应指向 intent.recorded：%v", grant.CausationID)
	}
	if agentMsg.Actor.Kind != "agent" || agentMsg.Actor.ParticipantID != "par_echo" {
		t.Fatalf("agent 消息 actor 不符：%+v", agentMsg.Actor)
	}
	// 同轮 correlation
	roundID := events[2].CorrelationID
	for _, ev := range events[2:] {
		if ev.CorrelationID == nil || *ev.CorrelationID != *roundID {
			t.Fatalf("%s correlation 应为 round id", ev.Type)
		}
	}
	// intent.recorded 投影字段（echo 确定性：relevance 0.5 → medium；selected）
	var ir protocol.IntentRecordedPayload
	if err := json.Unmarshal(intent.Payload, &ir); err != nil {
		t.Fatalf("intent payload: %v", err)
	}
	// echo 确定性：全 0.5 分 + 中性特征 → 记分卡 0.375 → band "low"（band 来自记分卡分而非自报 relevance）
	if ir.ScoreBand != "low" || !ir.Selected || ir.Action != "speak" || ir.Type != "extend" {
		t.Fatalf("intent 投影不符：%+v", ir)
	}
	// round.closed 结果
	var rc protocol.RoundClosedPayload
	if err := json.Unmarshal(events[6].Payload, &rc); err != nil || rc.Outcome != "published" || rc.SelectedCount != 1 {
		t.Fatalf("round.closed 不符：%+v err=%v", rc, err)
	}
}

// 其他房间/agent 事件不得触发开轮（无反馈环）
func TestEngineIgnoresNonHumanAndOtherRooms(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Policy: attention.Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.30, Weights: attention.DefaultWeights},
		Clock:  testClock, Now: time.Now, NewID: func(p string) string { return p + "_x" },
		Tenant: "ten_local", RoomID: "room_eng",
	})

	agentMsg := protocol.Envelope{
		EventID: "evt_x", TenantID: "ten_local", RoomID: "room_eng",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_echo", Kind: "agent"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: []byte(`{}`), Metadata: map[string]any{},
	}
	raw, _ := json.Marshal(agentMsg)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_eng", Envelope: raw})

	humanOther := agentMsg
	humanOther.EventID = "evt_y"
	humanOther.RoomID = "room_other"
	humanOther.Actor = protocol.Actor{ParticipantID: "par_owner", Kind: "human"}
	raw2, _ := json.Marshal(humanOther)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_other", Envelope: raw2})

	time.Sleep(100 * time.Millisecond)
	if got := len(store.RoomEvents("room_eng")) + len(store.RoomEvents("room_other")); got != 0 {
		t.Fatalf("不应产生任何事件，got %d", got)
	}
}

func typesOf(events []protocol.Envelope) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func jsonNumber(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// 多座选择：两 echo 座同分 → 平分决胜 participant_id 字典序 → rank 1/2；
// 同轮 grant 共享 epoch；round.closed selected_count=2。
func TestEngineMultiSeatSelection(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()

	var mu sync.Mutex
	var n int64
	newID := func(prefix string) string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return prefix + "_m_" + jsonNumber(n)
	}
	eng := NewEngine(EngineConfig{
		Store:  store,
		Reader: store,
		Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_echo_b", Profile: agent.Profile{ProfileID: "prof_b", Adapter: "echo"}},
			{ParticipantID: "par_echo_a", Profile: agent.Profile{ProfileID: "prof_a", Adapter: "echo"}},
		},
		Policy: attention.Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.30, Weights: attention.DefaultWeights},
		Clock:  testClock,
		Now:    func() time.Time { return time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC) },
		NewID:  newID,
		Tenant: "ten_local",
		RoomID: "room_multi",
	})

	store.AppendEvents(context.Background(), []protocol.Envelope{{
		EventID: "evt_m_create", TenantID: "ten_local", RoomID: "room_multi",
		Type: protocol.EventRoomCreated, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: []byte(`{}`), Metadata: map[string]any{},
	}})
	store.AppendEvents(context.Background(), []protocol.Envelope{{
		EventID: "evt_m_human", TenantID: "ten_local", RoomID: "room_multi",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: []byte(`{"body":"多座"}`), Metadata: map[string]any{},
	}})
	raw, _ := json.Marshal(protocol.Envelope{
		EventID: "evt_m_human", TenantID: "ten_local", RoomID: "room_multi",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: []byte(`{"body":"多座"}`), Metadata: map[string]any{},
	})
	eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_multi", Envelope: raw})

	deadline := time.Now().Add(3 * time.Second)
	var events []protocol.Envelope
	for time.Now().Before(deadline) {
		events = store.RoomEvents("room_multi")
		if len(events) > 0 && events[len(events)-1].Type == protocol.EventRoundClosed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(events) == 0 || events[len(events)-1].Type != protocol.EventRoundClosed {
		t.Fatalf("轮未完成，事件数=%d", len(events))
	}
	// 期望：created, human, round.opened, intent×2, grant×2, agent msg×2, round.closed = 10
	if len(events) != 10 {
		t.Fatalf("事件数 = %d（期望 10）：%v", len(events), typesOf(events))
	}
	want := []string{
		protocol.EventRoomCreated, protocol.EventMessagePosted, protocol.EventRoundOpened,
		protocol.EventIntentRecorded, protocol.EventIntentRecorded,
		protocol.EventFloorGranted, protocol.EventMessagePosted,
		protocol.EventFloorGranted, protocol.EventMessagePosted,
		protocol.EventRoundClosed,
	}
	for i, w := range want {
		if events[i].Type != w {
			t.Fatalf("第 %d 事件 = %s（期望 %s）：%v", i, events[i].Type, w, typesOf(events))
		}
	}
	// grant rank 与共享 epoch
	var grants []protocol.FloorGrantedPayload
	for _, ev := range events {
		if ev.Type == protocol.EventFloorGranted {
			var g protocol.FloorGrantedPayload
			_ = json.Unmarshal(ev.Payload, &g)
			grants = append(grants, g)
		}
	}
	if grants[0].ParticipantID != "par_echo_a" || grants[0].Rank != 1 {
		t.Fatalf("rank1 应为字典序 par_echo_a：%+v", grants[0])
	}
	if grants[1].ParticipantID != "par_echo_b" || grants[1].Rank != 2 {
		t.Fatalf("rank2 应为 par_echo_b：%+v", grants[1])
	}
	if grants[0].Epoch != grants[1].Epoch || grants[0].Epoch != 1 {
		t.Fatalf("同轮 grant 应共享 epoch=1：%+v %+v", grants[0], grants[1])
	}
	// 两条 intent 均 selected 且 band 一致（同分同特征）
	selected := 0
	for _, ev := range events {
		if ev.Type == protocol.EventIntentRecorded {
			var ir protocol.IntentRecordedPayload
			_ = json.Unmarshal(ev.Payload, &ir)
			if ir.Selected {
				selected++
			}
			if ir.ScoreBand != "low" {
				t.Fatalf("band = %s（期望 low，来自记分卡 0.375）", ir.ScoreBand)
			}
		}
	}
	if selected != 2 {
		t.Fatalf("selected intents = %d（期望 2）", selected)
	}
	// round.closed 汇总
	var rc protocol.RoundClosedPayload
	_ = json.Unmarshal(events[9].Payload, &rc)
	if rc.Outcome != "published" || rc.SelectedCount != 2 {
		t.Fatalf("round.closed 不符：%+v", rc)
	}
}

// ---- 可控 fake 适配器：generate 可阻塞、可发 draft 流 ----

type gatedAdapter struct {
	release chan struct{} // nil = 不阻塞
	drafts  []agent.DraftUpdate
}

func (gatedAdapter) Name() string                     { return "gated" }
func (gatedAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{CancelMode: "none"} }
func (gatedAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return &gatedSession{}, nil
}

type gatedSession struct{}

func (*gatedSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return &gatedHandle{task: task}, nil
}
func (*gatedSession) Cancel(string) {}
func (*gatedSession) Close()        {}

type gatedHandle struct {
	task agent.Task
	mu   sync.Mutex
	done bool
}

func (h *gatedHandle) Updates() <-chan agent.DraftUpdate { return nil }

func (h *gatedHandle) Result() (agent.Result, error) {
	if h.task.Kind == agent.KindGenerate && gate != nil {
		<-gate // 阻塞直到测试放行（期间可注入 pause）
	}
	if h.task.Kind == agent.KindEvaluateIntent {
		return agent.Result{Block: "turn_intent", Data: map[string]any{
			"action": "speak", "type": "extend", "public_rationale": "g",
			"scores": map[string]any{"relevance": .5, "novelty": .5, "urgency": .5, "confidence": .5},
		}}, nil
	}
	return agent.Result{Block: "public_draft", Data: map[string]any{"body": "gated draft"}}, nil
}
func (h *gatedHandle) Cancel() {}

var gate chan struct{}

// 预算硬停：轮次耗尽后人类消息不再触发自动轮。
func TestEngineBudgetHardStop(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	_ = sup.Register(echo.Adapter{})
	eng := newEchoEngine(store, sup, contextx.Limits{MaxRounds: 2}, "echo")

	// 种子：created + human + 两轮已开的 round.opened（账本轮次已满）
	seed := []protocol.Envelope{
		{EventID: "e1", TenantID: "ten_local", RoomID: "room_b", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		{EventID: "e2", TenantID: "ten_local", RoomID: "room_b", Type: protocol.EventRoundOpened, Actor: protocol.Actor{ParticipantID: "s", Kind: "system"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		{EventID: "e3", TenantID: "ten_local", RoomID: "room_b", Type: protocol.EventRoundOpened, Actor: protocol.Actor{ParticipantID: "s", Kind: "system"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}
	if _, err := store.AppendEvents(context.Background(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deliverHuman(t, store, eng, "room_b")
	time.Sleep(150 * time.Millisecond)
	if n := len(store.RoomEvents("room_b")); n != 4 {
		t.Fatalf("预算硬停后不得开轮，事件数 = %d：%v", n, typesOf(store.RoomEvents("room_b")))
	}
}

// 暂停期间人类消息不触发自动轮（人类消息本身已落库，不受限）。
func TestEnginePauseBlocksRounds(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	_ = sup.Register(echo.Adapter{})
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	seed := []protocol.Envelope{
		{EventID: "p1", TenantID: "ten_local", RoomID: "room_p", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		{EventID: "p2", TenantID: "ten_local", RoomID: "room_p", Type: protocol.EventRoomPaused, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}
	store.AppendEvents(context.Background(), seed)
	deliverHuman(t, store, eng, "room_p")
	time.Sleep(150 * time.Millisecond)
	events := store.RoomEvents("room_p")
	if len(events) != 3 { // created + paused + human（无轮事件）
		t.Fatalf("暂停期间不得开轮：%v", typesOf(events))
	}
}

// 迟到拒绝：生成在途时暂停 → 结果不发布，floor.revoked 落库（正文零迟到污染）。
func TestEngineLateRejectionOnPause(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()

	gate = make(chan struct{})
	defer func() { gate = nil }()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "l1", TenantID: "ten_local", RoomID: "room_l", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_l")

	// 等 grant 出现（generate 阻塞中）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hasType(store.RoomEvents("room_l"), protocol.EventFloorGranted) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 生成在途注入暂停
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "l9", TenantID: "ten_local", RoomID: "room_l", Type: protocol.EventRoomPaused, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	close(gate) // 放行 generate

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events := store.RoomEvents("room_l")
		if len(events) > 0 && events[len(events)-1].Type == protocol.EventRoundClosed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	events := store.RoomEvents("room_l")
	types := typesOf(events)
	if !hasType(events, protocol.EventFloorRevoked) {
		t.Fatalf("应产生 floor.revoked：%v", types)
	}
	agentMsgs := 0
	for _, e := range events {
		if e.Type == protocol.EventMessagePosted && e.Actor.Kind == "agent" {
			agentMsgs++
		}
	}
	if agentMsgs != 0 {
		t.Fatalf("迟到正文不得发布：%v", types)
	}
	var rc protocol.RoundClosedPayload
	_ = json.Unmarshal(events[len(events)-1].Payload, &rc)
	if rc.Outcome != "revoked_all" {
		t.Fatalf("outcome = %s（期望 revoked_all）", rc.Outcome)
	}
}

func hasType(events []protocol.Envelope, typ string) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func deliverHuman(t *testing.T, store *MemStore, eng *Engine, roomID string) {
	t.Helper()
	env := protocol.Envelope{
		EventID: "evt_human_" + roomID, TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"body":"stimulus"}`), Metadata: map[string]any{},
	}
	if store != nil {
		if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
			t.Fatalf("append human: %v", err)
		}
	}
	raw, _ := json.Marshal(env)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: roomID, Envelope: raw})
}

func newEchoEngine(store *MemStore, sup *agent.Supervisor, limits contextx.Limits, adapterName string) *Engine {
	var mu sync.Mutex
	var n int64
	return NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: adapterName}}},
		Policy: attention.Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.30, Weights: attention.DefaultWeights},
		Budget: limits,
		Clock:  testClock, Now: time.Now,
		NewID: func(p string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return p + "_g_" + jsonNumber(n)
		},
		Tenant: "ten_local",
	})
}
