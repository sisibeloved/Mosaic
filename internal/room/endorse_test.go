// UT 层：人类保送（B4——RFC-0003 §3.1.11 / OQ-17）。
package room

import (
	"context"

	"encoding/json"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// endorseEnv：双座 echo + max=1（必然产生一个未获选 intent 供保送）。
func endorseEnv(t *testing.T) (*MemStore, *Engine, *Service, string) {
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
		},
		Budget: contextx.Limits{},
		Clock:  testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	svc := NewService(Config{Store: store, Reader: store, Clock: testClock,
		NewID: counterNewID(), Tenant: "ten_local"})
	created, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{CommandKind: "create_room", ExpectedRoomVersion: 0,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5eb0", IssuedAt: "2026-08-31T09:00:00.000Z",
			Payload: []byte(`{"display_name":"e"}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// max=1（一轮恰一个获选、一个未获选）
	p := policyDefaults("open_floor")
	p.MaxSpeakers = 1
	if _, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{RoomID: created.RoomID, CommandKind: "set_policy", ExpectedRoomVersion: 1,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5eb1", IssuedAt: "2026-08-31T09:00:01.000Z",
			Payload: mustMarshalForTest(p)}); err != nil {
		t.Fatalf("set_policy: %v", err)
	}
	return store, eng, svc, created.RoomID
}

func unselectedIntent(t *testing.T, store *MemStore, roomID string) string {
	t.Helper()
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Type != protocol.EventIntentRecorded {
			continue
		}
		var p protocol.IntentRecordedPayload
		if json.Unmarshal(ev.Payload, &p) == nil && !p.Selected {
			return p.IntentID
		}
	}
	return ""
}

// TestEndorseCommandAndExecution：命令落 intent.endorsed（human actor、public）→
// 引擎执行：floor.granted（causation=endorsed 事件）→ 生成 → 发布；
// 人类可追溯链：message.posted → grant → intent.endorsed(human)。
func TestEndorseCommandAndExecution(t *testing.T) {
	store, eng, svc, roomID := endorseEnv(t)
	deliverHuman(t, store, eng, roomID)
	waitRoundClosed(t, store, roomID)

	intentID := unselectedIntent(t, store, roomID)
	if intentID == "" {
		t.Fatal("max=1 双座应产生未获选 intent")
	}
	version := int64(0)
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Seq > version {
			version = ev.Seq
		}
	}
	res, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{RoomID: roomID, CommandKind: "endorse_intent", ExpectedRoomVersion: version,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5eb2", IssuedAt: "2026-08-31T09:00:02.000Z",
			Payload: []byte(`{"intent_id":"` + intentID + `","effect":"grant"}`)})
	if err != nil {
		t.Fatalf("endorse: %v", err)
	}
	// 引擎执行（经 Deliver 投递 endorsed 事件）
	raw, _ := json.Marshal(protocol.Envelope{
		EventID: res.EventID, TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventIntentEndorsed, Payload: []byte(`{"intent_id":"` + intentID + `","endorsed_by":"par_owner","effect":"grant"}`),
		Metadata: map[string]any{},
	})
	eng.Deliver(context.Background(), outbox.Entry{RoomID: roomID, Envelope: raw})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hasEndorsePublish(store.RoomEvents(roomID)) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	events := store.RoomEvents(roomID)
	if !hasEndorsePublish(events) {
		t.Fatalf("保送应产生发布：%v", typesOf(events))
	}
	// 因果链：msg.causation=grant → grant.causation=endorsed（human actor）
	var endorsedID string
	for _, ev := range events {
		if ev.Type == protocol.EventIntentEndorsed {
			endorsedID = ev.EventID
			if ev.Actor.Kind != "human" || ev.Visibility.Kind != "public" {
				t.Fatalf("endorsed 事件应为 human actor + public：%+v", ev.Actor)
			}
		}
	}
	grantID, msgCausation := "", ""
	for _, ev := range events {
		if ev.Type != protocol.EventFloorGranted {
			continue
		}
		var g protocol.FloorGrantedPayload
		_ = json.Unmarshal(ev.Payload, &g)
		if g.GrantID != "" && len(g.GrantID) > len("grant_") && ev.CausationID != nil && *ev.CausationID == endorsedID {
			grantID = ev.EventID
		}
	}
	if grantID == "" {
		t.Fatal("缺少 causation=endorsed 的授予（人类可追溯链断）")
	}
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" &&
			ev.CausationID != nil && *ev.CausationID == grantID {
			msgCausation = *ev.CausationID
		}
	}
	if msgCausation != grantID {
		t.Fatal("保送发布 causation 应指向授予")
	}
	// 快照记分卡：该 intent 的 endorsed 合并为 true
	stored, _, _ := store.EventsAfter(context.Background(), roomID, "", 1000)
	snap := ProjectSnapshot(roomID, stored)
	found := false
	for _, it := range snap.Scorecard {
		if it.IntentID == intentID {
			found = true
			if !it.Endorsed {
				t.Fatal("记分卡应合并 endorsed=true")
			}
		}
	}
	if !found {
		t.Fatal("记分卡缺该 intent")
	}
}

// TestEndorseValidation：boost 未开放 / 未知 intent / 非法 id 拒绝。
func TestEndorseValidation(t *testing.T) {
	store, eng, svc, roomID := endorseEnv(t)
	deliverHuman(t, store, eng, roomID)
	waitRoundClosed(t, store, roomID)
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	version := int64(0)
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Seq > version {
			version = ev.Seq
		}
	}
	mk := func(key, payload string) Command {
		return Command{RoomID: roomID, CommandKind: "endorse_intent", ExpectedRoomVersion: version,
			IdempotencyKey: key, IssuedAt: "2026-08-31T09:00:03.000Z", Payload: []byte(payload)}
	}
	if _, err := svc.ExecuteCommand(context.Background(), actor,
		mk("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5eb3", `{"intent_id":"int_x","effect":"boost"}`)); err == nil {
		t.Fatal("boost 未开放应拒绝")
	}
	if _, err := svc.ExecuteCommand(context.Background(), actor,
		mk("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5eb4", `{"intent_id":"int_ghost","effect":"grant"}`)); err == nil {
		t.Fatal("未知 intent 应拒绝")
	}
	if _, err := svc.ExecuteCommand(context.Background(), actor,
		mk("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5eb5", `{"intent_id":"bad","effect":"grant"}`)); err == nil {
		t.Fatal("非法 intent_id 应拒绝")
	}
}

func hasEndorsePublish(events []protocol.Envelope) bool {
	grantByEvent := map[string]bool{}
	for _, ev := range events {
		if ev.Type == protocol.EventFloorGranted && ev.Metadata != nil {
			if _, ok := ev.Metadata["endorsed"]; ok {
				grantByEvent[ev.EventID] = true
			}
		}
	}
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" &&
			ev.CausationID != nil && grantByEvent[*ev.CausationID] {
			return true
		}
	}
	return false
}
