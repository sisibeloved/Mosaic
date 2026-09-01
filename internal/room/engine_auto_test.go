// UT 层：自动续聊（轮次自驱动，RFC-0003 §3.1.7 / 计划 v1.26 裁定 M2 dogfood 片）——
// 链式开轮至模式上限、静默轮终止、模式关（roundtable）、人类在轮边界抢占
// （FIFO 到达序 + 锚点轮新鲜度丢弃）、开轮门控的事件溯源上限重验、纯函数计数。
package room

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// seedNoAutoPolicy 追加 auto_rounds=0 的 open_floor 默认束（v1.27 起默认束
// 自动续聊=3——单轮确定性断言的既有用例以显式关续聊保持原语义）。
func seedNoAutoPolicy(t *testing.T, store *MemStore, roomID string) {
	t.Helper()
	p := policyDefaults("open_floor")
	p.AutoRounds = 0
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{{
		EventID: "evt_pol_noauto_" + roomID, TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventPolicyChanged, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    mustMarshalForTest(p), Metadata: map[string]any{},
	}}); err != nil {
		t.Fatalf("seed no-auto policy: %v", err)
	}
}

func seedPolicyParams(t *testing.T, store *MemStore, roomID string, p protocol.PolicyParams) {
	t.Helper()
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{{
		EventID: "evt_pol_" + roomID + "_" + p.Mode, TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventPolicyChanged, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    mustMarshalForTest(p), Metadata: map[string]any{},
	}}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
}

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

func openedPayloads(events []protocol.Envelope) []protocol.RoundOpenedPayload {
	var out []protocol.RoundOpenedPayload
	for _, ev := range events {
		if ev.Type != protocol.EventRoundOpened {
			continue
		}
		var p protocol.RoundOpenedPayload
		_ = json.Unmarshal(ev.Payload, &p)
		out = append(out, p)
	}
	return out
}

func findEnv(events []protocol.Envelope, eventID string) *protocol.Envelope {
	for i := range events {
		if events[i].EventID == eventID {
			return &events[i]
		}
	}
	return nil
}

func waitForRounds(t *testing.T, store *MemStore, roomID string, closed int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countType(store.RoomEvents(roomID), protocol.EventRoundClosed) >= closed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("轮未达 %d 轮：%v", closed, typesOf(store.RoomEvents(roomID)))
}

// 默认束 open_floor auto_rounds=3：一条人类消息 → 1 人类轮 + 3 自动轮后停；
// 自动轮刺激 = 上一轮最后一条 agent 发言，auto_index 链内 1..3 递增。
func TestAutoRoundChainToCap(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_auto")
	deliverHuman(t, store, eng, "room_auto")

	waitForRounds(t, store, "room_auto", 4)
	time.Sleep(150 * time.Millisecond) // 链终止确认窗口（不得出现第 5 轮）

	events := store.RoomEvents("room_auto")
	if n := countType(events, protocol.EventRoundOpened); n != 4 {
		t.Fatalf("round.opened = %d（期望 4：1 人类 + 3 自动）：%v", n, typesOf(events))
	}
	opened := openedPayloads(events)
	for i, want := range []int{0, 1, 2, 3} {
		if opened[i].AutoIndex != want {
			t.Fatalf("第 %d 轮 auto_index = %d（期望 %d）", i+1, opened[i].AutoIndex, want)
		}
	}
	if opened[1].StimulusEventID == opened[0].StimulusEventID {
		t.Fatalf("自动轮刺激应前移至 agent 发言：%+v", opened[1])
	}
	stim := findEnv(events, opened[1].StimulusEventID)
	if stim == nil || stim.Actor.Kind != "agent" {
		t.Fatalf("自动轮刺激必须是 agent 发言：%+v", stim)
	}
}

// 静默轮（评估全弃权 → quiescent）终止链：只开 1 轮。
func TestAutoRoundSilentStopsChain(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor() // 不注册任何适配器：评估失败 → 弃权 → quiescent
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_silent")
	deliverHuman(t, store, eng, "room_silent")

	waitForRounds(t, store, "room_silent", 1)
	time.Sleep(150 * time.Millisecond)

	events := store.RoomEvents("room_silent")
	if n := countType(events, protocol.EventRoundOpened); n != 1 {
		t.Fatalf("静默轮应终止链：round.opened = %d（期望 1）：%v", n, typesOf(events))
	}
	var rc protocol.RoundClosedPayload
	_ = json.Unmarshal(events[len(events)-1].Payload, &rc)
	if rc.Outcome != "quiescent" {
		t.Fatalf("outcome = %s（期望 quiescent）", rc.Outcome)
	}
}

// Roundtable 默认束 auto_rounds=0：轮内有 cross 接力，轮间不自动续。
func TestAutoRoundOffForRoundtable(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_rt")
	seedPolicyParams(t, store, "room_rt", policyDefaults("roundtable"))
	deliverHuman(t, store, eng, "room_rt")

	waitForRounds(t, store, "room_rt", 1)
	time.Sleep(150 * time.Millisecond)
	if n := countType(store.RoomEvents("room_rt"), protocol.EventRoundOpened); n != 1 {
		t.Fatalf("roundtable 应关自动续聊：round.opened = %d（期望 1）", n)
	}
}

// 人类在轮边界抢占：轮 1 生成期间到达的人类消息排队在续轮请求之前——
// 轮 2 为人类轮（stimulus=人类消息），轮 1 的续轮请求因锚点过期被丢弃；
// 链自轮 2 重新计数（轮 3 auto_index=1，auto_rounds=1 达限停）。
func TestAutoRoundHumanPreemptionAndStaleness(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(gatedAdapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "gated")
	defer eng.Close()

	seedRoomCreatedFor(t, store, "room_pre")
	p := policyDefaults("open_floor")
	p.AutoRounds = 1
	seedPolicyParams(t, store, "room_pre", p)

	g := make(chan struct{})
	setGate(g)
	defer setGate(nil)
	deliverHuman(t, store, eng, "room_pre") // 轮 1 生成阻塞在 gate

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) &&
		countType(store.RoomEvents("room_pre"), protocol.EventFloorGranted) < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if countType(store.RoomEvents("room_pre"), protocol.EventFloorGranted) < 1 {
		t.Fatal("轮 1 未进入生成阶段")
	}
	deliverHuman2(t, store, eng, "room_pre", "evt_pre_second") // 排队：先于续轮请求
	close(g)                                                   // 放行 → 轮 1 收口 → 续轮请求入队（在 second 之后）

	waitForRounds(t, store, "room_pre", 3)
	time.Sleep(150 * time.Millisecond)

	events := store.RoomEvents("room_pre")
	if n := countType(events, protocol.EventRoundOpened); n != 3 {
		t.Fatalf("round.opened = %d（期望 3：人类/抢占人类/自动）：%v", n, typesOf(events))
	}
	opened := openedPayloads(events)
	if opened[1].StimulusEventID != "evt_pre_second" || opened[1].AutoIndex != 0 {
		t.Fatalf("轮 2 应为人类抢占轮（stimulus=evt_pre_second, auto=0）：%+v", opened[1])
	}
	if opened[2].AutoIndex != 1 {
		t.Fatalf("轮 3 应为自轮 2 重计的自动轮（auto_index=1）：%+v", opened[2])
	}
	if stim := findEnv(events, opened[2].StimulusEventID); stim == nil || stim.Actor.Kind != "agent" {
		t.Fatalf("自动轮刺激必须是 agent 发言：%+v", stim)
	}
}

// trailingAutoRounds 纯函数：链尾连续自动轮计数，遇人类轮止。
func TestTrailingAutoRounds(t *testing.T) {
	mk := func(auto int) StoredEvent {
		return StoredEvent{Envelope: protocol.Envelope{
			Type:    protocol.EventRoundOpened,
			Payload: mustMarshalForTest(protocol.RoundOpenedPayload{RoundID: "rnd", AutoIndex: auto}),
		}}
	}
	cases := []struct {
		autos []int // round.opened 序列（0=人类轮）
		want  int
	}{
		{[]int{0, 1, 2}, 2},
		{[]int{0}, 0},
		{nil, 0},
		{[]int{0, 1, 0, 1, 2, 3}, 3},
		{[]int{1}, 1},
	}
	for _, c := range cases {
		var evs []StoredEvent
		for _, a := range c.autos {
			evs = append(evs, mk(a))
		}
		if got := trailingAutoRounds(evs); got != c.want {
			t.Fatalf("trailingAutoRounds(%v) = %d（期望 %d）", c.autos, got, c.want)
		}
	}
}

// 开轮门控重验（事件溯源）：已达上限（trailing ≥ auto_rounds）的续轮请求直接丢弃。
func TestAutoRoundOpenGateRechecksCap(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := newEchoEngine(store, sup, contextx.Limits{}, "echo")
	defer eng.Close()

	// 手铺历史：1 人类轮 + 2 自动轮（auto_rounds=2 已达限），锚点=最新 agent 发言
	p := policyDefaults("open_floor")
	p.AutoRounds = 2
	seedRoomCreatedFor(t, store, "room_cap")
	seedPolicyParams(t, store, "room_cap", p)
	envOf := func(eventID, typ string, payload []byte) protocol.Envelope {
		return protocol.Envelope{EventID: eventID, TenantID: "ten_local", RoomID: "room_cap",
			Type: typ, SchemaVersion: 1, OccurredAt: testClock(),
			Actor:      protocol.Actor{ParticipantID: "par_echo", Kind: "agent"},
			Visibility: protocol.Visibility{Kind: "public"}, Payload: payload, Metadata: map[string]any{}}
	}
	agentMsg := envOf("evt_cap_agent2", protocol.EventMessagePosted, []byte(`{"body":"a2"}`))
	human := envOf("evt_cap_human", protocol.EventMessagePosted, []byte(`{"body":"h"}`))
	human.Actor = protocol.Actor{ParticipantID: "par_owner", Kind: "human"}
	mkOpened := func(roundID, stimulus string, auto int) protocol.Envelope {
		return envOf("evt_op_"+roundID, protocol.EventRoundOpened, mustMarshalForTest(protocol.RoundOpenedPayload{
			RoundID: roundID, StimulusEventID: stimulus, Mode: "open_floor",
			RevealStrategy: "simultaneous", IntentWindow: "20s", PolicyVersion: "pol_2", AutoIndex: auto,
		}))
	}
	mkClosed := func(roundID string) protocol.Envelope {
		return envOf("evt_cl_"+roundID, protocol.EventRoundClosed,
			mustMarshalForTest(protocol.RoundClosedPayload{RoundID: roundID, Outcome: "published"}))
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		human, mkOpened("rnd_c1", "evt_cap_human", 0),
		envOf("evt_cap_agent1", protocol.EventMessagePosted, []byte(`{"body":"a1"}`)), mkClosed("rnd_c1"),
		mkOpened("rnd_c2", "evt_cap_agent1", 1), mkClosed("rnd_c2"),
		mkOpened("rnd_c3", "evt_cap_agent2", 2), mkClosed("rnd_c3"),
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	eng.runAutoRound(context.Background(), autoContinue{
		roomID: "room_cap", anchorRoundID: "rnd_c3", anchor: agentMsg,
	})
	time.Sleep(100 * time.Millisecond)
	if n := countType(store.RoomEvents("room_cap"), protocol.EventRoundOpened); n != 3 {
		t.Fatalf("达限续轮请求应被开轮门控丢弃：round.opened = %d（期望 3）", n)
	}
}
