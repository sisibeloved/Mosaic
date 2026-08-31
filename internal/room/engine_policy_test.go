// UT 层：引擎遵守投影策略（B1）——round.opened / floor.granted 的策略字段
// 全部来自事件链投影（R-10 round 边界生效；窗口/cap/reveal/版本不再硬编码）。
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

func TestEngineRoundHonorsProjectedPolicy(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Budget: contextx.Limits{},
		Clock:  testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})

	// set_policy（deep_dive：2 人/15s/900/sequential）经命令面落库——引擎经投影读取
	svc := NewService(Config{Store: store, Clock: testClock, NewID: counterNewID(), Tenant: "ten_local"})
	created, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{CommandKind: "create_room", ExpectedRoomVersion: 0,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5ea0", IssuedAt: "2026-08-31T09:00:00.000Z",
			Payload: []byte(`{"display_name":"p"}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{RoomID: created.RoomID, CommandKind: "set_policy", ExpectedRoomVersion: 1,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5ea1", IssuedAt: "2026-08-31T09:00:01.000Z",
			Payload: []byte(`{"mode":"deep_dive","max_speakers":2,"lambda":0.3,
				"weights":{"relevance":0.3,"novelty":0.2,"diversity":0.15,"urgency":0.1,"direct_address":0.15,"floor_share":0.05,"repetition":0.05},
				"intent_window":"15s","response_cap":900,"reveal_strategy":"sequential"}`)}); err != nil {
		t.Fatalf("set_policy: %v", err)
	}

	deliverHuman(t, store, eng, created.RoomID)
	waitRoundClosed(t, store, created.RoomID)

	events := store.RoomEvents(created.RoomID)
	var opened protocol.RoundOpenedPayload
	var grant protocol.FloorGrantedPayload
	for _, ev := range events {
		switch ev.Type {
		case protocol.EventRoundOpened:
			if err := json.Unmarshal(ev.Payload, &opened); err != nil {
				t.Fatalf("round.opened payload: %v", err)
			}
		case protocol.EventFloorGranted:
			if err := json.Unmarshal(ev.Payload, &grant); err != nil {
				t.Fatalf("floor.granted payload: %v", err)
			}
		}
	}
	if opened.Mode != "deep_dive" {
		t.Fatalf("round.opened.mode = %q（投影策略未生效）", opened.Mode)
	}
	if opened.RevealStrategy != "sequential" || opened.IntentWindow != "15s" {
		t.Fatalf("round.opened 策略字段不符：%+v（M1 硬编码 simultaneous/30s 应已消除）", opened)
	}
	if opened.PolicyVersion != "pol_2" {
		t.Fatalf("policy_version = %q（默认束后第一次变更应为 pol_2）", opened.PolicyVersion)
	}
	if grant.ResponseCap != 900 {
		t.Fatalf("floor.granted.response_cap = %d（应为投影 cap 900）", grant.ResponseCap)
	}
	if grant.RevealStrategy != "sequential" {
		t.Fatalf("floor.granted.reveal_strategy = %q", grant.RevealStrategy)
	}
}
