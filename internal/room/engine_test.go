// UT 层：房间引擎——人类消息驱动完整轮事件链（echo 适配器）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
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
		Clock:  testClock, ReactionWindow: 5 * time.Millisecond,
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
		// RFC-0012：波后 echo 处于冷却 → 无第二波（单座静默终止，无事件）
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
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Clock: testClock, Now: time.Now, NewID: func(p string) string { return p + "_x" }, ReactionWindow: 5 * time.Millisecond,
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
		Clock: testClock, ReactionWindow: 5 * time.Millisecond,
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
	// 期望：created, human, round.opened, intent×2, (grant,msg)×2 交错, closed = 10
	// （RFC-0012：意愿放行 sequential——intent 齐后逐座 授→生成→发布；上波发言者下波冷却，
	//  双座同波都回 → 下波全冷却 → 单波终止）
	if len(events) != 10 {
		t.Fatalf("事件数 = %d（期望 10）：%v", len(events), typesOf(events))
	}
	want := []string{
		protocol.EventRoomCreated, protocol.EventMessagePosted, protocol.EventRoundOpened,
		protocol.EventIntentRecorded, protocol.EventIntentRecorded,
		protocol.EventFloorGranted, protocol.EventMessagePosted, // 序贯：授→生成→发布 逐座
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

func (*gatedSession) Run(ctx context.Context, task agent.Task) (agent.Handle, error) {
	return &gatedHandle{task: task, ctx: ctx}, nil
}
func (*gatedSession) Cancel(string) {}
func (*gatedSession) Close()        {}

type gatedHandle struct {
	task agent.Task
	ctx  context.Context
	mu   sync.Mutex
	done bool
}

func (h *gatedHandle) Updates() <-chan agent.DraftUpdate { return nil }

func (h *gatedHandle) Result() (agent.Result, error) {
	if h.task.Kind == agent.KindEvaluateIntent {
		if _, _, hook := gatedSnapshot(); hook != nil { // 评估阶段注入事件（复审 #13）
			hook()
		}
		return agent.Result{Block: "turn_intent", Data: map[string]any{
			"action": "speak", "type": "extend", "public_rationale": "g",
			"scores": map[string]any{"relevance": .5, "novelty": .5, "urgency": .5, "confidence": .5},
		}}, nil
	}
	if h.task.Kind == agent.KindGenerate {
		g, fail, _ := gatedSnapshot()
		if fail {
			return agent.Result{}, fmt.Errorf("gated: generate boom")
		}
		if g != nil {
			select {
			case <-g: // 阻塞直到测试放行（期间可注入 pause/Close）
			case <-h.ctx.Done():
				return agent.Result{}, agent.ErrStale
			}
		}
		gatedMu.Lock()
		u := genUsage
		gatedMu.Unlock()
		return agent.Result{Block: "public_draft", Data: map[string]any{"body": "gated draft"}, Usage: u}, nil
	}
	return agent.Result{Block: "public_draft", Data: map[string]any{"body": "gated draft"}}, nil
}
func (h *gatedHandle) Cancel() {}

// gated 桩的包级状态：room worker 常驻（复审 #16 队列化）后，测试清理 defer 会与
// 在途轮并发读写——一律经 gatedMu 护栏（-race 清零）。
var (
	gatedMu  sync.Mutex
	gate     chan struct{}
	genFail  bool
	evalHook func()
	genUsage *agent.Usage // 四轮复审 #13：gated 生成结果的 usage 注入
)

func gatedSnapshot() (chan struct{}, bool, func()) {
	gatedMu.Lock()
	defer gatedMu.Unlock()
	return gate, genFail, evalHook
}

func setGate(ch chan struct{}) {
	gatedMu.Lock()
	gate = ch
	gatedMu.Unlock()
}

func setGenFail(v bool) {
	gatedMu.Lock()
	genFail = v
	gatedMu.Unlock()
}

func setEvalHook(hook func()) {
	gatedMu.Lock()
	evalHook = hook
	gatedMu.Unlock()
}

func setGenUsage(u *agent.Usage) {
	gatedMu.Lock()
	genUsage = u
	gatedMu.Unlock()
}

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
	// M3-2：预算熔断写暂停胶囊（未收敛快照，非结论——不写 closure.accepted/不关线程）
	events := store.RoomEvents("room_b")
	if n := len(events); n != 5 {
		t.Fatalf("预算硬停后不得开轮（但写暂停胶囊），事件数 = %d：%v", n, typesOf(events))
	}
	if events[len(events)-1].Type != protocol.EventPauseCapsuleCreated {
		t.Fatalf("末事件应为 pause_capsule.created：%v", typesOf(events))
	}
	// 暂停胶囊去重：再投一条人类消息不重复写胶囊（在位即不补）
	deliverHuman2(t, store, eng, "room_b", "evt_human_b2")
	time.Sleep(100 * time.Millisecond)
	if n := len(store.RoomEvents("room_b")); n != 6 {
		t.Fatalf("胶囊在位不重复写（只多一条人类消息），事件数 = %d", n)
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

	setGate(make(chan struct{}))
	defer setGate(nil)
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
	if rc.Outcome != "quiescent" {
		t.Fatalf("outcome = %s（期望 quiescent——全撤销波零发布）", rc.Outcome)
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

// ---- M1 收口补课（审校 2026-08-28）----

// R-01：失格候选（预算预留不足）也必须落 intent.recorded——零痕迹违反 RFC-0003 R-01 全记录。
func TestEngineRecordsRejectedIntents(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	// MaxTokens=1：admission 过（用量 0），但对称预留 1×600>1 → BudgetOK=false → 失格
	eng := newEchoEngine(store, sup, contextx.Limits{MaxTokens: 1}, "echo")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "r1", TenantID: "ten_local", RoomID: "room_r", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_r")
	waitRoundClosed(t, store, "room_r")
	events := store.RoomEvents("room_r")
	var ir protocol.IntentRecordedPayload
	found := false
	for _, ev := range events {
		if ev.Type != protocol.EventIntentRecorded {
			continue
		}
		found = true
		_ = json.Unmarshal(ev.Payload, &ir)
		if ir.Selected || ir.ScoreBand != "unranked" {
			t.Fatalf("失格意图应 selected=false + score_band=unranked：%+v", ir)
		}
		if ev.Metadata["unselected_reason"] != "budget" {
			t.Fatalf("unselected_reason 应为 budget：%v", ev.Metadata["unselected_reason"])
		}
	}
	if !found {
		t.Fatalf("失格候选零痕迹（R-01 违约）：%v", typesOf(events))
	}
	if hasType(events, protocol.EventFloorGranted) {
		t.Fatalf("失格轮不得有 grant：%v", typesOf(events))
	}
	var rc protocol.RoundClosedPayload
	_ = json.Unmarshal(events[len(events)-1].Payload, &rc)
	if rc.Outcome != "quiescent" {
		t.Fatalf("outcome = %s（期望 quiescent）", rc.Outcome)
	}
}

// 预算 token 三维：评估 usage 必须入 intent.recorded metadata 并计入账本重建。
func TestEngineRecordsEvalUsage(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(usageAdapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "usage")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "u1", TenantID: "ten_local", RoomID: "room_u", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_u")
	waitRoundClosed(t, store, "room_u")
	var sawUsage bool
	for _, ev := range store.RoomEvents("room_u") {
		if ev.Type != protocol.EventIntentRecorded {
			continue
		}
		usage, ok := ev.Metadata["usage"].(map[string]any)
		if !ok {
			t.Fatalf("intent.recorded 缺评估 usage：%v", ev.Metadata)
		}
		if num(usage["input_tokens"]) != 11 || num(usage["output_tokens"]) != 7 {
			t.Fatalf("评估 usage 不符：%v", usage)
		}
		sawUsage = true
	}
	if !sawUsage {
		t.Fatal("未找到 intent.recorded")
	}
	// 账本重建把评估 usage 计入 token 维度（三维账本不得缺评估侧）
	envs := store.RoomEvents("room_u")
	if led := contextx.RebuildBudget(envs); led.Tokens != 18 { // eval 18（agent 消息 usage 0 自报缺失记 0）
		t.Fatalf("RebuildBudget tokens = %d（期望 18，含评估 usage）", led.Tokens)
	}
}

// generate 失败撤销的 reason 必须是 generation_failed（张冠李戴 human_preemption 是缺陷）。
func TestEngineGenerateFailureRevokesWithReason(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	setGenFail(true)
	defer setGenFail(false)
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "g1", TenantID: "ten_local", RoomID: "room_g", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_g")
	waitRoundClosed(t, store, "room_g")
	events := store.RoomEvents("room_g")
	for _, ev := range events {
		if ev.Type != protocol.EventFloorRevoked {
			continue
		}
		var fr protocol.FloorRevokedPayload
		_ = json.Unmarshal(ev.Payload, &fr)
		if fr.Reason != "generation_failed" {
			t.Fatalf("revoked reason = %s（期望 generation_failed）", fr.Reason)
		}
	}
	if !hasType(events, protocol.EventFloorRevoked) {
		t.Fatalf("应产生 floor.revoked：%v", typesOf(events))
	}
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			t.Fatalf("失败生成不得发布正文：%v", typesOf(events))
		}
	}
}

// 同房间轮串行：第二条刺激不得与在途轮并发开轮（同 epoch 双轮是竞态缺陷）。
// RFC-0012：波串行 + 去抖合并——两条快速连发的人类消息合并为一个反应波
// （窗口重锚，锚=最新消息 s2）；波内生成阻塞期间无第二波开启（单飞）。
// 唯一座发言后冷却 → 无后续波（自然终止）。
func TestEngineWavesSerializeAndDebounce(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	setGate(make(chan struct{}))
	defer setGate(nil)
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "s0", TenantID: "ten_local", RoomID: "room_s", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	for _, id := range []string{"evt_s1", "evt_s2"} {
		env := protocol.Envelope{
			EventID: id, TenantID: "ten_local", RoomID: "room_s",
			Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
			Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"}, Payload: []byte(`{"body":"s"}`), Metadata: map[string]any{},
		}
		if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
			t.Fatalf("append: %v", err)
		}
		raw, _ := json.Marshal(env)
		eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_s", Envelope: raw})
	}
	// 等波进入生成（generate 阻塞中）——期间不得有第二波
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasType(store.RoomEvents("room_s"), protocol.EventFloorGranted) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if n := countType(store.RoomEvents("room_s"), protocol.EventRoundOpened); n != 1 {
		t.Fatalf("在途波未完成时 round.opened = %d（期望 1，串行）", n)
	}
	close(gate)
	waitRoundClosed(t, store, "room_s")

	events := store.RoomEvents("room_s")
	if n := countType(events, protocol.EventRoundOpened); n != 1 {
		t.Fatalf("连发两条应去抖合并为一个波：round.opened = %d（期望 1）", n)
	}
	// 锚点 = 最新消息（s2）：round.opened.stimulus_event_id
	for _, ev := range events {
		if ev.Type != protocol.EventRoundOpened {
			continue
		}
		var p protocol.RoundOpenedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		if p.StimulusEventID != "evt_s2" {
			t.Fatalf("波锚点应为最新消息 evt_s2：%+v", p)
		}
		break
	}
	// 唯一座发言后冷却 → 无后续波
	time.Sleep(120 * time.Millisecond)
	if n := countType(store.RoomEvents("room_s"), protocol.EventRoundOpened); n != 1 {
		t.Fatalf("发言者冷却应终止流：round.opened = %d（期望 1）", n)
	}
}

// Close 生命周期：关停在途轮，agent 正文零发布（进程退出不孤儿化在途任务）。
func TestEngineCloseAbortsInflight(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	setGate(make(chan struct{}))
	defer setGate(nil)
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "c1", TenantID: "ten_local", RoomID: "room_c2", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_c2")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasType(store.RoomEvents("room_c2"), protocol.EventFloorGranted) {
		time.Sleep(5 * time.Millisecond)
	}
	eng.Close() // 关停：在途 generate 取消
	eng.Close() // 幂等
	time.Sleep(200 * time.Millisecond)
	for _, ev := range store.RoomEvents("room_c2") {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			t.Fatal("Close 后不得发布 agent 正文")
		}
	}
	// Close 后新刺激不再开轮
	deliverHuman2(t, store, eng, "room_c2", "evt_c2_after")
	time.Sleep(150 * time.Millisecond)
	if n := countType(store.RoomEvents("room_c2"), protocol.EventRoundOpened); n != 1 {
		t.Fatalf("Close 后不得开新轮：round.opened = %d", n)
	}
}

// Context Receipt 落库必须带 CreatedAt（恒空串是缺陷）。
func TestEngineReceiptCreatedAt(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	var captured []contextx.Receipt
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Receipts: receiptSink(func(ctx context.Context, r contextx.Receipt) error {
			captured = append(captured, r)
			return nil
		}),
		Clock: testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
		NewID: func() func(string) string {
			var mu sync.Mutex
			var n int64
			return func(p string) string {
				mu.Lock()
				defer mu.Unlock()
				n++
				return p + "_ts_" + jsonNumber(n)
			}
		}(),
		Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "t1", TenantID: "ten_local", RoomID: "room_t", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_t")
	waitRoundClosed(t, store, "room_t")
	if len(captured) == 0 {
		t.Fatal("应捕获 Context Receipt")
	}
	if captured[0].CreatedAt != testClock() {
		t.Fatalf("Receipt.CreatedAt = %q（期望引擎时钟赋值）", captured[0].CreatedAt)
	}
}

type receiptSink func(ctx context.Context, r contextx.Receipt) error

func (f receiptSink) InsertReceipt(ctx context.Context, r contextx.Receipt) error { return f(ctx, r) }

// usageAdapter：评估返回固定 usage（11/7），generate 返回零值 usage（自报缺失记 0）。
type usageAdapter struct{}

func (usageAdapter) Name() string                     { return "usage" }
func (usageAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (usageAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return usageSession{}, nil
}

type usageSession struct{}

func (usageSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return usageHandle{task: task}, nil
}
func (usageSession) Cancel(string) {}
func (usageSession) Close()        {}

type usageHandle struct {
	task agent.Task
}

func (usageHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (usageHandle) Cancel()                           {}
func (h usageHandle) Result() (agent.Result, error) {
	if h.task.Kind == agent.KindEvaluateIntent {
		return agent.Result{
			Block: "turn_intent",
			Data: map[string]any{
				"action": "speak", "type": "extend", "public_rationale": "u",
				"scores": map[string]any{"relevance": .5, "novelty": .5, "urgency": .5, "confidence": .5},
			},
			Usage: &agent.Usage{InputTokens: 11, OutputTokens: 7, Model: "usage"},
		}, nil
	}
	return agent.Result{Block: "public_draft", Data: map[string]any{"body": "u"}, Usage: nil}, nil
}

// 二轮审校 #8：畸形 intent（未知枚举/非数值分数/缺分字段）必须弃权——不得转成合法零分入选择。
func TestEngineRejectsMalformedIntent(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
	}{
		{"未知 action", map[string]any{"action": "explode", "type": "extend",
			"scores": map[string]any{"relevance": .5, "novelty": .5, "urgency": .5, "confidence": .5}}},
		{"未知 type", map[string]any{"action": "speak", "type": "explode",
			"scores": map[string]any{"relevance": .5, "novelty": .5, "urgency": .5, "confidence": .5}}},
		{"字符串分数", map[string]any{"action": "speak", "type": "extend",
			"scores": map[string]any{"relevance": "0.5", "novelty": .5, "urgency": .5, "confidence": .5}}},
		{"缺分字段", map[string]any{"action": "speak", "type": "extend",
			"scores": map[string]any{"relevance": .5, "novelty": .5, "confidence": .5}}},
		{"缺 scores", map[string]any{"action": "speak", "type": "extend"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			sup := agent.NewSupervisor()
			stubIntentData = tc.data
			_ = sup.Register(intentStubAdapter{})
			defer sup.Shutdown()
			defer func() { stubIntentData = nil }()
			eng := newEchoEngine(store, sup, contextx.Limits{}, "stub_intent")
			store.AppendEvents(context.Background(), []protocol.Envelope{
				{EventID: "mi0", TenantID: "ten_local", RoomID: "room_mi", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
			})
			deliverHuman(t, store, eng, "room_mi")
			waitRoundClosed(t, store, "room_mi")
			events := store.RoomEvents("room_mi")
			// 复审 #12：弃权仍全记录（R-01）——band=unranked、理由入 metadata、不得 grant
			if !hasType(events, protocol.EventIntentRecorded) {
				t.Fatalf("畸形 intent 也必须落 intent.recorded（R-01 全记录）：%v", typesOf(events))
			}
			if hasType(events, protocol.EventFloorGranted) {
				t.Fatalf("畸形 intent 不得获选：%v", typesOf(events))
			}
			var ir protocol.IntentRecordedPayload
			for _, ev := range events {
				if ev.Type != protocol.EventIntentRecorded {
					continue
				}
				_ = json.Unmarshal(ev.Payload, &ir)
				if ir.Action != "silent" || ir.ScoreBand != "unranked" || ir.Selected {
					t.Fatalf("畸形 intent 应记录为 silent/unranked/未选：%+v", ir)
				}
				if ev.Metadata["unselected_reason"] != "invalid_intent_structure" {
					t.Fatalf("弃权理由应入 metadata：%v", ev.Metadata)
				}
			}
			var rc protocol.RoundClosedPayload
			_ = json.Unmarshal(events[len(events)-1].Payload, &rc)
			if rc.Outcome != "quiescent" {
				t.Fatalf("outcome = %s（期望 quiescent）", rc.Outcome)
			}
		})
	}
}

// intentStubAdapter：按包级 stubIntentData 返回评估结果（畸形注入用）。
type intentStubAdapter struct{}

func (intentStubAdapter) Name() string                     { return "stub_intent" }
func (intentStubAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (intentStubAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return intentStubSession{}, nil
}

type intentStubSession struct{}

func (intentStubSession) Run(context.Context, agent.Task) (agent.Handle, error) {
	return intentStubHandle{}, nil
}
func (intentStubSession) Cancel(string) {}
func (intentStubSession) Close()        {}

type intentStubHandle struct{}

func (intentStubHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (intentStubHandle) Cancel()                           {}
func (intentStubHandle) Result() (agent.Result, error) {
	return agent.Result{Block: "turn_intent", Data: stubIntentData, Usage: stubIntentUsage}, nil
}

var stubIntentData map[string]any

var stubIntentUsage *agent.Usage

// 二轮审校 #7：生成在途 pause→快速 resume，旧生成仍必须拒发（fence：grant 后出现过
// room.paused 即失效，"最终未暂停"检查挡不住这个窗口）。
func TestEnginePauseResumeFence(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	setGate(make(chan struct{}))
	defer setGate(nil)
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "pf1", TenantID: "ten_local", RoomID: "room_pf", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_pf")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasType(store.RoomEvents("room_pf"), protocol.EventFloorGranted) {
		time.Sleep(5 * time.Millisecond)
	}
	// 生成在途：pause → 立即 resume
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "pf2", TenantID: "ten_local", RoomID: "room_pf", Type: protocol.EventRoomPaused, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		{EventID: "pf3", TenantID: "ten_local", RoomID: "room_pf", Type: protocol.EventRoomStarted, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	close(gate)
	waitRoundClosed(t, store, "room_pf")
	events := store.RoomEvents("room_pf")
	if !hasType(events, protocol.EventFloorRevoked) {
		t.Fatalf("pause→resume 后旧生成必须撤销：%v", typesOf(events))
	}
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			t.Fatalf("失效生成不得发布正文：%v", typesOf(events))
		}
	}
	var rc protocol.RoundClosedPayload
	_ = json.Unmarshal(events[len(events)-1].Payload, &rc)
	if rc.Outcome != "quiescent" {
		t.Fatalf("outcome = %s（期望 quiescent——全撤销波零发布）", rc.Outcome)
	}
}

// 二轮审校 #9：durable handoff——Deliver 返回前声明已落盘；outbox 重放不双开轮；
// 崩溃窗口（已声明未开轮）由 RecoverClaims 重驱动。
func TestEngineDurableHandoffClaim(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	setGate(make(chan struct{}))
	defer setGate(nil)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup, Claims: store,
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "gated"}}},
		Clock: testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "dh0", TenantID: "ten_local", RoomID: "room_dh", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	stimulus := protocol.Envelope{
		EventID: "dh1", TenantID: "ten_local", RoomID: "room_dh",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:   protocol.Actor{ParticipantID: "o", Kind: "human"},
		Payload: []byte(`{"body":"dh"}`), Metadata: map[string]any{},
	}
	store.AppendEvents(context.Background(), []protocol.Envelope{stimulus})
	raw, _ := json.Marshal(stimulus)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_dh", Envelope: raw})
	// Deliver 已返回：声明必须可查（此时轮仍在 generate 阻塞中——这正是 outbox 确认前的交接窗口）
	claims, err := store.PendingClaims(context.Background())
	if err != nil || len(claims) != 1 || claims[0].StimulusEventID != "dh1" {
		t.Fatalf("Deliver 返回后声明必须已落盘：%v %v", claims, err)
	}
	// outbox 重放同一刺激（at-least-once）：不得双开轮
	eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_dh", Envelope: raw})
	close(gate)
	waitRoundClosed(t, store, "room_dh")
	if n := countType(store.RoomEvents("room_dh"), protocol.EventRoundOpened); n != 1 {
		t.Fatalf("重放不得双开轮：round.opened = %d", n)
	}
	// 开轮后声明应被清除
	claims, _ = store.PendingClaims(context.Background())
	if len(claims) != 0 {
		t.Fatalf("开轮后声明应清除，剩 %d", len(claims))
	}
}

// 二轮审校 #9：崩溃窗口（已声明、未开轮）——恢复扫描重驱动。
func TestEngineRecoverClaimsDrivesLostRound(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup, Claims: store,
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Clock: testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "rc0", TenantID: "ten_local", RoomID: "room_rc", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	lost := protocol.Envelope{
		EventID: "rc1", TenantID: "ten_local", RoomID: "room_rc",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:   protocol.Actor{ParticipantID: "o", Kind: "human"},
		Payload: []byte(`{"body":"lost"}`), Metadata: map[string]any{},
	}
	// 模拟崩溃窗口：声明落盘、事件落库，但轮从未开（未走 Deliver）
	store.AppendEvents(context.Background(), []protocol.Envelope{lost})
	if _, err := store.ClaimStimulus(context.Background(), "room_rc", "rc1", mustRawJSON(lost), 1); err != nil {
		t.Fatalf("claim: %v", err)
	}
	eng.RecoverClaims()
	waitRoundClosed(t, store, "room_rc")
	events := store.RoomEvents("room_rc")
	if countType(events, protocol.EventRoundOpened) != 1 {
		t.Fatalf("恢复扫描应重驱动丢失的轮：%v", typesOf(events))
	}
	if claims, _ := store.PendingClaims(context.Background()); len(claims) != 0 {
		t.Fatalf("恢复后声明应清除，剩 %d", len(claims))
	}
}

// 二轮审校 #1：运行时 SetSeats 后新座位参与下一轮（动态启用不重建引擎）。
func TestEngineDynamicSeats(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo") // 初始单座 par_echo
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "ds0", TenantID: "ten_local", RoomID: "room_ds", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	eng.SetSeats([]AgentSeat{
		{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p1", Adapter: "echo"}},
		{ParticipantID: "par_late", Profile: agent.Profile{ProfileID: "p2", Adapter: "echo"}},
	})
	deliverHuman(t, store, eng, "room_ds")
	waitRoundClosed(t, store, "room_ds")
	intents := countType(store.RoomEvents("room_ds"), protocol.EventIntentRecorded)
	if intents != 2 {
		t.Fatalf("动态座位应双双参与（intent.recorded=%d，期望 2）", intents)
	}
}

// agent-native：刺激带 thread_id → agent 回复落同一线程（线程归属不丢）。
func TestEngineThreadPassthrough(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "tp0", TenantID: "ten_local", RoomID: "room_tp", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	threadID := "thr_test_branch"
	stimulus := protocol.Envelope{
		EventID: "tp1", TenantID: "ten_local", RoomID: "room_tp", ThreadID: &threadID,
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"body":"threaded"}`), Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{stimulus}); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, _ := json.Marshal(stimulus)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_tp", Envelope: raw})
	waitRoundClosed(t, store, "room_tp")
	for _, ev := range store.RoomEvents("room_tp") {
		if ev.Type != protocol.EventMessagePosted || ev.Actor.Kind != "agent" {
			continue
		}
		if ev.ThreadID == nil || *ev.ThreadID != threadID {
			t.Fatalf("agent 回复应落在刺激线程 %s：%v", threadID, ev.ThreadID)
		}
		return
	}
	t.Fatal("未找到 agent 消息")
}

func counterNewID() func(string) string {
	var mu sync.Mutex
	var n int64
	return func(p string) string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return p + "_c" + jsonNumber(n)
	}
}

func mustRawJSON(v protocol.Envelope) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func waitRoundClosed(t *testing.T, store *MemStore, roomID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events := store.RoomEvents(roomID)
		if len(events) > 0 && events[len(events)-1].Type == protocol.EventRoundClosed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("轮未完成：%v", typesOf(store.RoomEvents(roomID)))
}

// waitRoundsClosed 等待第 n 个 round.closed（原 directed_test 辅助，随文件退役迁此）。
// countAgentMsgsOf agent 发言计数（原 parallel_test 辅助，随文件退役迁此）。
func countAgentMsgsOf(events []protocol.Envelope) int {
	n := 0
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			n++
		}
	}
	return n
}

func waitRoundsClosed(t *testing.T, store *MemStore, roomID string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countType(store.RoomEvents(roomID), protocol.EventRoundClosed) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("轮未达 %d 轮：%v", n, typesOf(store.RoomEvents(roomID)))
}

func countType(events []protocol.Envelope, typ string) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// num 容忍 int64/float64（metadata 是否经 JSON 往返取决于存储实现）。
func num(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	case int:
		return n
	default:
		return -1
	}
}

func deliverHuman2(t *testing.T, store *MemStore, eng *Engine, roomID, eventID string) {
	t.Helper()
	env := protocol.Envelope{
		EventID: eventID, TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: []byte(`{"body":"after"}`), Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, _ := json.Marshal(env)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: roomID, Envelope: raw})
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
		Budget: limits, ReactionWindow: 5 * time.Millisecond, // RFC-0012 去抖窗口测试化
		Clock: testClock, Now: time.Now,
		NewID: func(p string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return p + "_g_" + jsonNumber(n)
		},
		Tenant: "ten_local",
	})
}

// ---- 三轮复审（2026-08-29）----

// 复审 #12：畸形 intent 的真实 usage 必须入 metadata——预算账本不被畸形输出绕过。
func TestEngineRecordsInvalidIntentUsage(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	stubIntentData = map[string]any{"action": "speak", "type": "extend",
		"scores": map[string]any{"relevance": "0.5", "novelty": .5, "urgency": .5, "confidence": .5}} // 字符串分数 → 畸形
	stubIntentUsage = &agent.Usage{InputTokens: 7, OutputTokens: 3}
	_ = sup.Register(intentStubAdapter{})
	defer sup.Shutdown()
	defer func() { stubIntentData = nil; stubIntentUsage = nil }()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "stub_intent")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "iu0", TenantID: "ten_local", RoomID: "room_iu", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_iu")
	waitRoundClosed(t, store, "room_iu")
	for _, ev := range store.RoomEvents("room_iu") {
		if ev.Type != protocol.EventIntentRecorded {
			continue
		}
		usage, _ := ev.Metadata["usage"].(map[string]any)
		if usage == nil || num(usage["input_tokens"]) != 7 || num(usage["output_tokens"]) != 3 {
			t.Fatalf("畸形 intent 的真实 usage 必须入账：%v", ev.Metadata)
		}
		return
	}
	t.Fatal("未找到畸形 intent 的 intent.recorded")
}

// 复审 #10：暂停期间到达的刺激形成声明但不开轮——resume（room.started）即时重驱动，
// 不再只能等进程重启（RecoverClaims）才重放。
func TestEngineResumeRedrivesPausedStimulus(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup, Claims: store,
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Clock: testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "rd0", TenantID: "ten_local", RoomID: "room_rd", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		{EventID: "rd1", TenantID: "ten_local", RoomID: "room_rd", Type: protocol.EventRoomPaused, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_rd")
	time.Sleep(150 * time.Millisecond) // 暂停门：不开轮、声明留存
	if hasType(store.RoomEvents("room_rd"), protocol.EventRoundOpened) {
		t.Fatal("暂停期间不得开轮")
	}
	if claims, _ := store.PendingClaims(context.Background()); len(claims) != 1 {
		t.Fatalf("暂停期刺激应留存声明：%d", len(claims))
	}
	// resume：事件落库 + 经 Deliver 送达（outbox 对全部事件投递）
	resume := protocol.Envelope{
		EventID: "rd2", TenantID: "ten_local", RoomID: "room_rd",
		Type: protocol.EventRoomStarted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{resume}); err != nil {
		t.Fatalf("append resume: %v", err)
	}
	if err := eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_rd", Envelope: mustRawJSON(resume)}); err != nil {
		t.Fatalf("deliver resume: %v", err)
	}
	waitRoundClosed(t, store, "room_rd")
	if n := countType(store.RoomEvents("room_rd"), protocol.EventRoundOpened); n != 1 {
		t.Fatalf("resume 后应重驱动开轮：round.opened = %d", n)
	}
	if claims, _ := store.PendingClaims(context.Background()); len(claims) != 0 {
		t.Fatalf("重驱动后声明应清除：%d", len(claims))
	}
	agentPublished := false
	for _, ev := range store.RoomEvents("room_rd") {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			agentPublished = true
		}
	}
	if !agentPublished {
		t.Fatal("重驱动轮应完成发布（echo）")
	}
}

// 复审 #13：评估阶段 pause→grant 前 resume——旧轮仍不得发布（fence 锚点=本轮 round.opened，
// 不再只看 grant 之后；开轮前的暂停历史不毒化重驱动的新轮）。
func TestEnginePauseDuringEvalRevokesBeforeGrant(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "pe0", TenantID: "ten_local", RoomID: "room_pe", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	setEvalHook(func() { // 评估阶段注入 pause→resume（grant 之前）
		store.AppendEvents(context.Background(), []protocol.Envelope{
			{EventID: "pe1", TenantID: "ten_local", RoomID: "room_pe", Type: protocol.EventRoomPaused, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
			{EventID: "pe2", TenantID: "ten_local", RoomID: "room_pe", Type: protocol.EventRoomStarted, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		})
	})
	defer setEvalHook(nil)
	deliverHuman(t, store, eng, "room_pe")
	waitRoundClosed(t, store, "room_pe")
	events := store.RoomEvents("room_pe")
	if !hasType(events, protocol.EventFloorRevoked) {
		t.Fatalf("评估阶段 pause→resume 的旧轮必须撤销：%v", typesOf(events))
	}
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			t.Fatalf("被 fence 的旧轮不得发布正文：%v", typesOf(events))
		}
	}
	var rc protocol.RoundClosedPayload
	_ = json.Unmarshal(events[len(events)-1].Payload, &rc)
	if rc.Outcome != "quiescent" {
		t.Fatalf("outcome = %s（期望 quiescent——全撤销波零发布）", rc.Outcome)
	}
}

// failClaimStore 复审 #15：声明落库恒失败（模拟存储故障）。
type failClaimStore struct{ *MemStore }

func (f failClaimStore) ClaimStimulus(ctx context.Context, roomID, stimulusEventID string, envelope []byte, position int64) (bool, error) {
	return false, fmt.Errorf("claim store down")
}

// 复审 #15：声明失败 fail closed——Deliver 返回错误（分发器不确认、按序重投），
// 不再退化成易丢失的内存直驱。
func TestEngineDeliverClaimErrorFailsClosed(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup, Claims: failClaimStore{store},
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Clock: testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "fc0", TenantID: "ten_local", RoomID: "room_fc", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	stimulus := protocol.Envelope{
		EventID: "fc1", TenantID: "ten_local", RoomID: "room_fc",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{"body":"fc"}`), Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{stimulus}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_fc", Envelope: mustRawJSON(stimulus)}); err == nil {
		t.Fatal("声明落库失败必须返回错误（退回分发器重投），不得静默内存直驱")
	}
	time.Sleep(150 * time.Millisecond)
	if hasType(store.RoomEvents("room_fc"), protocol.EventRoundOpened) {
		t.Fatal("声明失败不得开内存轮（fail closed）")
	}
}

// 复审 #16：同房间两条刺激按到达序开轮——per-room FIFO，不因 goroutine 调度乱序。
// RFC-0012 §2.1 去抖重锚：窗口内先后两条消息只开一个波，锚=最新那条；
// 首条不单独开波（消息是语境——合并评估，成本闸）。
func TestReactionDebounceMergesToLatestAnchor(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "so0", TenantID: "ten_local", RoomID: "room_so", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	stimulus := func(id, body string) protocol.Envelope {
		return protocol.Envelope{
			EventID: id, TenantID: "ten_local", RoomID: "room_so",
			Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{"body":"` + body + `"}`), Metadata: map[string]any{},
		}
	}
	s1, s2 := stimulus("so1", "first"), stimulus("so2", "second")
	store.AppendEvents(context.Background(), []protocol.Envelope{s1, s2})
	_ = eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_so", Envelope: mustRawJSON(s1)})
	_ = eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_so", Envelope: mustRawJSON(s2)})

	waitRoundClosed(t, store, "room_so")
	time.Sleep(120 * time.Millisecond)
	events := store.RoomEvents("room_so")
	if n := countType(events, protocol.EventRoundOpened); n != 1 {
		t.Fatalf("去抖应合并为单波：round.opened = %d（期望 1）：%v", n, typesOf(events))
	}
	for _, ev := range events {
		if ev.Type != protocol.EventRoundOpened {
			continue
		}
		var p protocol.RoundOpenedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		if p.StimulusEventID != "so2" {
			t.Fatalf("波锚点应为最新消息 so2：%+v", p)
		}
		break
	}
}

// ---- 四轮复审（2026-08-30）----

// 四轮复审 #10：本轮评估消耗计入同轮 admission——BudgetOK 用"现在"的账本。
// MaxTokens=1000、MaxSpeakers=1、cap=600：评估花费 500 后 500+600>1000 → 失格。
func TestEngineEvalUsageCountsTowardAdmission(t *testing.T) {
	run := func(evalUsage *agent.Usage) bool {
		store := NewMemStore()
		sup := agent.NewSupervisor()
		stubIntentData = map[string]any{"action": "speak", "type": "extend",
			"scores": map[string]any{"relevance": .8, "novelty": .5, "urgency": .5, "confidence": .5}}
		stubIntentUsage = evalUsage
		_ = sup.Register(intentStubAdapter{})
		defer sup.Shutdown()
		defer func() { stubIntentData = nil; stubIntentUsage = nil }()
		eng := NewEngine(EngineConfig{
			Store: store, Reader: store, Agents: sup,
			Seats:  []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "stub_intent"}}},
			Budget: contextx.Limits{MaxTokens: 800}, // RFC-0012：默认 cap 500——零耗时预留过、评估 500 后 500+500>800 失格
			Clock:  testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
			NewID: counterNewID(), Tenant: "ten_local",
		})
		store.AppendEvents(context.Background(), []protocol.Envelope{
			{EventID: "eu0", TenantID: "ten_local", RoomID: "room_eu", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		})
		deliverHuman(t, store, eng, "room_eu")
		waitRoundClosed(t, store, "room_eu")
		return hasType(store.RoomEvents("room_eu"), protocol.EventFloorGranted)
	}
	if !run(nil) {
		t.Fatal("零评估消耗时对称预留应通过（500<=800），对照失败")
	}
	if run(&agent.Usage{InputTokens: 500, OutputTokens: 0}) {
		t.Fatal("评估消耗 500 后同波 admission 应失格（500+500>800）——同波 eval 用量不得绕过预算")
	}
}

func mustMarshalForTest(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// casHookStore 四轮复审 #12：在正文 CAS 落库前注入并发事件（复现检查与落库间的窗口）。
type casHookStore struct {
	*MemStore
	inject func(roomID string)
	hooked bool
}

func (c *casHookStore) AppendEventsIf(ctx context.Context, envs []protocol.Envelope, expected int64) ([]protocol.Envelope, error) {
	if !c.hooked && len(envs) > 0 && envs[0].Type == protocol.EventMessagePosted && envs[0].Actor.Kind == "agent" {
		c.hooked = true
		c.inject(envs[0].RoomID) // 检查后、落库前：并发事件先落库
		return nil, ErrVersionConflict
	}
	return c.MemStore.AppendEventsIf(ctx, envs, expected)
}

func newCasEngine(t *testing.T, store *casHookStore) *Engine {
	t.Helper()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	t.Cleanup(sup.Shutdown)
	return NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Clock: testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
		NewID: counterNewID(), Tenant: "ten_local",
	})
}

// 四轮复审 #12：窗口内到达 pause → CAS 失败 → 回读判真迟到 → 撤销，正文零发布。
func TestEngineLatePauseInAppendWindowRevoked(t *testing.T) {
	store := &casHookStore{MemStore: NewMemStore()}
	store.inject = func(roomID string) {
		store.MemStore.AppendEvents(context.Background(), []protocol.Envelope{
			{EventID: "cw_pause", TenantID: "ten_local", RoomID: roomID, Type: protocol.EventRoomPaused,
				Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		})
	}
	eng := newCasEngine(t, store)
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "cw0", TenantID: "ten_local", RoomID: "room_cw", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store.MemStore, eng, "room_cw")
	waitRoundClosed(t, store.MemStore, "room_cw")
	events := store.RoomEvents("room_cw")
	if !hasType(events, protocol.EventFloorRevoked) {
		t.Fatalf("窗口内 pause 必须撤销：%v", typesOf(events))
	}
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			t.Fatalf("竞态窗口的正文不得发布：%v", typesOf(events))
		}
	}
}

// 四轮复审 #12 对照：窗口内到达的是良性交错（人类消息）→ 换期位重试后正常发布。
func TestEngineBenignInterleaveRetriesAppend(t *testing.T) {
	store := &casHookStore{MemStore: NewMemStore()}
	store.inject = func(roomID string) {
		store.MemStore.AppendEvents(context.Background(), []protocol.Envelope{
			{EventID: "cw_benign", TenantID: "ten_local", RoomID: roomID, Type: protocol.EventMessagePosted,
				Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{"body":"mid-round"}`), Metadata: map[string]any{}},
		})
	}
	eng := newCasEngine(t, store)
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "cb0", TenantID: "ten_local", RoomID: "room_cb", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store.MemStore, eng, "room_cb")
	waitRoundClosed(t, store.MemStore, "room_cb")
	events := store.RoomEvents("room_cb")
	published := false
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			published = true
		}
	}
	if !published {
		t.Fatalf("良性交错应换期位重试并发布：%v", typesOf(events))
	}
}

// 四轮复审 #13：被 pause 撤销的生成 usage 入 floor.revoked metadata 且进账本。
func TestEngineRevokedUsageEntersLedger(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	setGate(make(chan struct{}))
	defer setGate(nil)
	setGenUsage(&agent.Usage{InputTokens: 40, OutputTokens: 2})
	defer setGenUsage(nil)
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "ru0", TenantID: "ten_local", RoomID: "room_ru", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_ru")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasType(store.RoomEvents("room_ru"), protocol.EventFloorGranted) {
		time.Sleep(5 * time.Millisecond)
	}
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "ru1", TenantID: "ten_local", RoomID: "room_ru", Type: protocol.EventRoomPaused, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	if ch, _, _ := gatedSnapshot(); ch != nil {
		close(ch) // 放行 generate（channel 关闭后所有等待者直接通过）
	}
	waitRoundClosed(t, store, "room_ru")
	events := store.RoomEvents("room_ru")
	found := false
	for _, ev := range events {
		if ev.Type != protocol.EventFloorRevoked {
			continue
		}
		usage, _ := ev.Metadata["usage"].(map[string]any)
		if usage == nil || num(usage["input_tokens"]) != 40 {
			t.Fatalf("撤销事件应携带生成 usage：%v", ev.Metadata)
		}
		found = true
	}
	if !found {
		t.Fatalf("应存在 floor.revoked：%v", typesOf(events))
	}
	envs := make([]protocol.Envelope, len(events))
	for i := range events {
		envs[i] = events[i]
	}
	if tokens := contextx.RebuildBudget(envs).Tokens; tokens < 42 {
		t.Fatalf("撤销的生成开销必须入账（>=42，got %d）", tokens)
	}
}

// failPendingClaims 四轮复审 #19：声明扫描恒失败（模拟存储故障）。
type failPendingClaims struct{ *MemStore }

func (f failPendingClaims) PendingClaims(ctx context.Context) ([]StimulusClaim, error) {
	return nil, fmt.Errorf("claims scan down")
}

// 四轮复审 #19：resume 重驱动扫描失败必须退回分发器（返回错误），不得只告警后确认。
func TestEngineResumeScanErrorPropagates(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup, Claims: failPendingClaims{store},
		Seats: []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Clock: testClock, Now: time.Now, ReactionWindow: 5 * time.Millisecond,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	resume := protocol.Envelope{
		EventID: "rs1", TenantID: "ten_local", RoomID: "room_rs",
		Type: protocol.EventRoomStarted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{},
	}
	if err := eng.Deliver(context.Background(), outbox.Entry{RoomID: "room_rs", Envelope: mustRawJSON(resume)}); err == nil {
		t.Fatal("resume 扫描失败必须返回错误（分发器不确认、按序重投）")
	}
}

// 四轮复审 #14：声明按持久位升序返回（恢复顺序 = 到达顺序）。
func TestClaimOrderingByPosition(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	if _, err := store.ClaimStimulus(ctx, "room_o", "late", mustRawJSON(protocol.Envelope{EventID: "late"}), 5); err != nil {
		t.Fatalf("claim late: %v", err)
	}
	if _, err := store.ClaimStimulus(ctx, "room_o", "early", mustRawJSON(protocol.Envelope{EventID: "early"}), 3); err != nil {
		t.Fatalf("claim early: %v", err)
	}
	claims, err := store.PendingClaims(ctx)
	if err != nil || len(claims) != 2 {
		t.Fatalf("pending: %v %v", claims, err)
	}
	if claims[0].StimulusEventID != "early" || claims[1].StimulusEventID != "late" {
		t.Fatalf("应按 position 升序：[%s %s]", claims[0].StimulusEventID, claims[1].StimulusEventID)
	}
}

// 评估相并行回归（dogfood "非常慢" 治理：串行求和 3 座 ≈ 51s）：两座评估必须并发
// 在途——自释放栅栏在第 2 座进入时放行；串行实现下栅栏永不闭合，轮超时判负。
func TestEngineEvaluatesSeatsInParallel(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(&parEvalAdapter{gate: make(chan struct{})})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_a", Profile: agent.Profile{ProfileID: "pa", Adapter: "par_eval"}},
			{ParticipantID: "par_b", Profile: agent.Profile{ProfileID: "pb", Adapter: "par_eval"}},
		},
		ReactionWindow: 5 * time.Millisecond,
		Clock:          testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "pe0", TenantID: "ten_local", RoomID: "room_pe", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_pe")
	waitRoundClosed(t, store, "room_pe")
	intents := 0
	for _, ev := range store.RoomEvents("room_pe") {
		if ev.Type == protocol.EventIntentRecorded {
			intents++
		}
	}
	if intents != 2 {
		t.Fatalf("两座均应记录 intent：%d", intents)
	}
}

type parEvalAdapter struct {
	mu       sync.Mutex
	inFlight int
	gate     chan struct{}
}

func (*parEvalAdapter) Name() string                     { return "par_eval" }
func (*parEvalAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (a *parEvalAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return &parEvalSession{a: a}, nil
}

type parEvalSession struct{ a *parEvalAdapter }

func (s *parEvalSession) Run(context.Context, agent.Task) (agent.Handle, error) {
	return &parEvalHandle{a: s.a}, nil
}
func (*parEvalSession) Cancel(string) {}
func (*parEvalSession) Close()        {}

type parEvalHandle struct{ a *parEvalAdapter }

func (*parEvalHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (*parEvalHandle) Cancel()                           {}
func (h *parEvalHandle) Result() (agent.Result, error) {
	h.a.mu.Lock()
	h.a.inFlight++
	if h.a.inFlight == 2 {
		close(h.a.gate)
	}
	h.a.mu.Unlock()
	<-h.a.gate
	return agent.Result{Block: "turn_intent", Data: map[string]any{"action": "silent"}}, nil
}

// 冷却死锁回归（dogfood "输入无回复"）：全员发言一波后，人类新消息必须照常开波——
// 冷却仅约束 agent 锚点（被跳过的波不落 round.opened，人类锚点若吃冷却，冷却集
// 永不更新，房间对人类后续输入永久静默）。
func TestEngineHumanStimulusBypassesCooldown(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "hb0", TenantID: "ten_local", RoomID: "room_hb", Type: protocol.EventRoomCreated, Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	deliverHuman(t, store, eng, "room_hb")
	waitRoundClosed(t, store, "room_hb") // 波 1：echo 发言并进冷却（唯一座 = 全员冷却态）
	deliverHuman2(t, store, eng, "room_hb", "evt_human_hb2")
	waitRoundsClosed(t, store, "room_hb", 2) // 波 2：人类锚点豁免冷却，必须再开
	if n := countAgentMsgsOf(store.RoomEvents("room_hb")); n != 2 {
		t.Fatalf("两波人类刺激应各得一条回复：agent 消息 = %d", n)
	}
}
