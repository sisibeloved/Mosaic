// UT 层：建房选 Agent + 拉人（dogfood 反馈 #1——RFC-0001 Membership 最小落地）。
package room

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// rosterEnv：双 echo 座（par_echo + par_other），建房即选 par_echo。
func rosterEnv(t *testing.T) (*MemStore, *Engine, *Service, string) {
	t.Helper()
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	t.Cleanup(sup.Shutdown)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "prof_echo", Adapter: "echo"}},
			{ParticipantID: "par_other", Profile: agent.Profile{ProfileID: "po", Adapter: "echo"}},
		},
		Budget: contextx.Limits{},
		Clock:  testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	svc := NewService(Config{Store: store, Reader: store, Clock: testClock,
		NewID: counterNewID(), Tenant: "ten_local"})
	created, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{CommandKind: "create_room", ExpectedRoomVersion: 0,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5ecf", IssuedAt: "2026-08-31T09:00:00.000Z",
			Payload: []byte(`{"display_name":"选人房","agents":["par_echo"]}`),
		})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	roomID := created.RoomID
	seedNoAutoPolicy(t, store, roomID) // 选人/名额断言按轮计数：关自动续聊（默认束 v1.27 起为 3）
	return store, eng, svc, roomID
}

func rosterStimulus(t *testing.T, store *MemStore, eng *Engine, roomID, eventID, body string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"body": body, "reply_to": nil, "addressed_to": []any{}, "relations": []any{},
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

// TestCreateRoomAgentSelection：agents=["par_echo"] → 只有 echo 参与轮；
// invite_agent 拉人后 par_other 入房参与。
func TestCreateRoomAgentSelection(t *testing.T) {
	store, eng, svc, roomID := rosterEnv(t)

	rosterStimulus(t, store, eng, roomID, "evt_ro_h1", "选人房消息")
	waitRoundClosed(t, store, roomID)

	events := store.RoomEvents(roomID)
	for _, ev := range events {
		if ev.Actor.ParticipantID == "par_other" {
			t.Fatalf("未入选的 par_other 不应参与轮（出现在 %s）：%v", ev.Type, typesOf(events))
		}
	}
	if countAgentMsgsOf(events) != 1 {
		t.Fatalf("恰 echo 一条发言：%v", typesOf(events))
	}
	stored, _, _ := store.EventsAfter(context.Background(), roomID, "", 1000)
	snap := ProjectSnapshot(roomID, stored)
	if len(snap.Roster) != 1 || snap.Roster[0] != "par_echo" {
		t.Fatalf("roster 投影不符：%v", snap.Roster)
	}

	// invite_agent 拉人 → par_other 入 roster 并参与下一轮
	version := int64(0)
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Seq > version {
			version = ev.Seq
		}
	}
	if _, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{RoomID: roomID, CommandKind: "invite_agent", ExpectedRoomVersion: version,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5ed1", IssuedAt: "2026-08-31T09:00:01.000Z",
			Payload: []byte(`{"participant_id":"par_other"}`),
		}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	rosterStimulus(t, store, eng, roomID, "evt_ro_h2", "拉人后的消息")
	waitRoundsClosed(t, store, roomID, 2)
	sawOther := false
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Actor.ParticipantID == "par_other" && ev.Seq > version {
			sawOther = true
		}
	}
	if !sawOther {
		t.Fatal("invite 后 par_other 应参与轮")
	}
}

// TestCreateRoomAgentValidation：非法 participant id / 超 8 拒绝。
func TestCreateRoomAgentValidation(t *testing.T) {
	_, _, svc, _ := rosterEnv(t)
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	cases := []struct{ name, payload string }{
		{"非法 id", `{"display_name":"x","agents":["codex"]}`},
		{"超上限", `{"display_name":"x","agents":["par_1","par_2","par_3","par_4","par_5","par_6","par_7","par_8","par_9"]}`},
	}
	for i, tc := range cases {
		if _, err := svc.ExecuteCommand(context.Background(), actor,
			Command{CommandKind: "create_room", ExpectedRoomVersion: 0,
				IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5ed" + string(rune('2'+i)),
				IssuedAt:       "2026-08-31T09:00:02.000Z",
				Payload:        []byte(tc.payload),
			}); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}
}

// TestCreateRoomDefaultMaterializesRoster：缺省选人 = 当时在席名单快照（v1.24，
// dogfood #2——"建房后装新 Agent 怎么办"：不自动入房，走邀请）。三断言：
// 未选人建房物化当时全席；建房后新启用的座位不自动入房（引擎轮排除）；
// 此后新建的房间快照才含新座。
func TestCreateRoomDefaultMaterializesRoster(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	t.Cleanup(sup.Shutdown)
	newID := counterNewID()
	seats := []AgentSeat{
		{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "prof_echo", Adapter: "echo"}},
		{ParticipantID: "par_other", Profile: agent.Profile{ProfileID: "po", Adapter: "echo"}},
	}
	eng := NewEngine(EngineConfig{Store: store, Reader: store, Agents: sup, Seats: seats,
		Budget: contextx.Limits{}, Clock: testClock, Now: time.Now, NewID: newID, Tenant: "ten_local"})
	svc := NewService(Config{Store: store, Reader: store, Clock: testClock, NewID: newID,
		Tenant: "ten_local", Seats: eng.Seats})
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	created, err := svc.ExecuteCommand(context.Background(), actor,
		Command{CommandKind: "create_room", ExpectedRoomVersion: 0,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5fa1", IssuedAt: "2026-08-31T09:00:00.000Z",
			Payload: []byte(`{"display_name":"快照房"}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, _, _ := store.EventsAfter(context.Background(), created.RoomID, "", 1000)
	if got := ProjectSnapshot(created.RoomID, stored).Roster; len(got) != 2 {
		t.Fatalf("缺省选人应物化当时全席（2 席）：%v", got)
	}
	seedNoAutoPolicy(t, store, created.RoomID) // 名额断言按轮计数：关自动续聊（默认束 v1.27 起为 3）

	// 建房后新启用一座（SetSeats 模拟 resync）：旧房间不自动收编
	eng.SetSeats(append(seats, AgentSeat{
		ParticipantID: "par_late", Profile: agent.Profile{ProfileID: "pl", Adapter: "echo"},
	}))
	rosterStimulus(t, store, eng, created.RoomID, "evt_rm_h1", "新座启用后")
	waitRoundClosed(t, store, created.RoomID)
	events := store.RoomEvents(created.RoomID)
	for _, ev := range events {
		if ev.Actor.ParticipantID == "par_late" {
			t.Fatalf("建房后启用的 par_late 不应自动入房：%v", typesOf(events))
		}
	}
	if n := countAgentMsgsOf(events); n != 2 {
		t.Fatalf("恰原两名额发言，实得 %d：%v", n, typesOf(events))
	}

	// 此后新建的房间：快照含新座
	created2, err := svc.ExecuteCommand(context.Background(), actor,
		Command{CommandKind: "create_room", ExpectedRoomVersion: 0,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5fa2", IssuedAt: "2026-08-31T09:00:03.000Z",
			Payload: []byte(`{"display_name":"快照房2"}`)})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	stored2, _, _ := store.EventsAfter(context.Background(), created2.RoomID, "", 1000)
	if got := ProjectSnapshot(created2.RoomID, stored2).Roster; len(got) != 3 {
		t.Fatalf("新房间应快照 3 席：%v", got)
	}
}

// TestRosterOfLegacyDerivation：旧房间（无 agents 载荷）按参与历史推导名单
// （v1.25 dogfood #1：动态全席对存量活房间同样不可接受——历史出现过的 agent
// 固化为名单，此后新启用座位不自动入房）；无 agent 历史回退 nil（首轮兼容）。
func TestRosterOfLegacyDerivation(t *testing.T) {
	mk := func(typ string, actorID, actorKind, payload string) protocol.Envelope {
		return protocol.Envelope{EventID: "evt_d_" + actorID + string(typ), TenantID: "ten_local",
			RoomID: "room_legacy", Type: typ, Actor: protocol.Actor{ParticipantID: actorID, Kind: actorKind},
			Payload: []byte(payload), Metadata: map[string]any{}}
	}
	envs := []protocol.Envelope{
		mk(protocol.EventRoomCreated, "par_owner", "human", `{}`),
		mk(protocol.EventRoundOpened, "par_system", "system", `{}`),
		mk(protocol.EventIntentRecorded, "par_system", "system", `{"participant_id":"par_a","action":"speak"}`),
		mk(protocol.EventFloorGranted, "par_system", "system", `{"participant_id":"par_b","grant_id":"g1"}`),
		mk(protocol.EventMessagePosted, "par_a", "agent", `{"body":"hi"}`),
	}
	roster := RosterOf(envs)
	if roster == nil || !roster["par_a"] || !roster["par_b"] || len(roster) != 2 {
		t.Fatalf("历史推导名单应为 {par_a, par_b}：%v", roster)
	}

	// 无 agent 历史：nil（全部在席——空转旧房/测试夹具的首轮兼容）
	empty := []protocol.Envelope{
		mk(protocol.EventRoomCreated, "par_owner", "human", `{}`),
		mk(protocol.EventMessagePosted, "par_owner", "human", `{"body":"stimulus"}`),
	}
	if got := RosterOf(empty); got != nil {
		t.Fatalf("无 agent 历史应回退 nil（全部在席）：%v", got)
	}
}

// TestTimelineIncludesSystemEvents：系统事件（轮次/暂停）随快照 Timeline 持久化
// （v1.25 dogfood #4：切房间/刷新后轮次提醒消失——SSE 不再是唯一来源）。
func TestTimelineIncludesSystemEvents(t *testing.T) {
	mk := func(id string, typ string, actor protocol.Actor, payload string) StoredEvent {
		return StoredEvent{Cursor: "cur_" + id, Envelope: protocol.Envelope{EventID: id,
			TenantID: "ten_local", RoomID: "room_tl", Type: typ, Actor: actor,
			Payload: []byte(payload), Metadata: map[string]any{}}}
	}
	events := []StoredEvent{
		mk("e1", protocol.EventRoomCreated, protocol.Actor{ParticipantID: "o", Kind: "human"}, `{"display_name":"x"}`),
		mk("e2", protocol.EventMessagePosted, protocol.Actor{ParticipantID: "o", Kind: "human"}, `{"body":"q"}`),
		mk("e3", protocol.EventRoundOpened, protocol.Actor{ParticipantID: "s", Kind: "system"}, `{}`),
		mk("e4", protocol.EventIntentRecorded, protocol.Actor{ParticipantID: "s", Kind: "system"}, `{"participant_id":"par_a"}`),
		mk("e5", protocol.EventRoundClosed, protocol.Actor{ParticipantID: "s", Kind: "system"}, `{"outcome":"published"}`),
		mk("e6", protocol.EventRoomPaused, protocol.Actor{ParticipantID: "o", Kind: "human"}, `{"reason":"r"}`),
	}
	snap := ProjectSnapshot("room_tl", events)
	var types []string
	for _, item := range snap.Timeline {
		types = append(types, item.Type)
	}
	want := []string{"message.posted", "round.opened", "round.closed", "room.paused"}
	if len(types) != len(want) {
		t.Fatalf("Timeline 类型序列 = %v，期望 %v（intent.recorded 不入列）", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("Timeline 类型序列 = %v，期望 %v", types, want)
		}
	}
	for _, item := range snap.Timeline {
		if item.Type == "round.closed" && item.Outcome != "published" {
			t.Fatalf("round.closed 应携带 outcome=published：%+v", item)
		}
	}
}
