// UT 层：set_policy 命令链与快照策略区（B1——RFC-0003 §3.1.7 / R-10）。
package room

import (
	"context"
	"errors"
	"testing"
)

const setPolicyValidJSON = `{"mode":"roundtable","max_speakers":8,"lambda":0.3,` +
	`"weights":{"relevance":0.3,"novelty":0.2,"diversity":0.15,"urgency":0.1,"direct_address":0.15,"floor_share":0.05,"repetition":0.05},` +
	`"intent_window":"30s","response_cap":600,"reveal_strategy":"independent_then_cross","rebuttals":1}`

const setPolicyBadRevealJSON = `{"mode":"open_floor","max_speakers":3,"lambda":0.3,` +
	`"weights":{"relevance":0.3,"novelty":0.2,"diversity":0.15,"urgency":0.1,"direct_address":0.15,"floor_share":0.05,"repetition":0.05},` +
	`"intent_window":"20s","response_cap":500,"reveal_strategy":"random"}`

const setPolicyUnknownFieldJSON = `{"mode":"open_floor","max_speakers":3,"lambda":0.3,` +
	`"weights":{"relevance":0.3,"novelty":0.2,"diversity":0.15,"urgency":0.1,"direct_address":0.15,"floor_share":0.05,"repetition":0.05},` +
	`"intent_window":"20s","response_cap":500,"reveal_strategy":"sequential","secret":"x"}`

func TestSetPolicyCommands(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	created, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	cmd := Command{RoomID: created.RoomID, CommandKind: "set_policy", ExpectedRoomVersion: 1,
		IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e90", IssuedAt: "2026-08-31T09:00:00.000Z",
		Payload: []byte(setPolicyValidJSON)}
	res, err := svc.ExecuteCommand(ctx, actor, cmd)
	if err != nil || res.Replayed {
		t.Fatalf("set_policy 应首发受理：%v %+v", err, res)
	}
	events := store.RoomEvents(created.RoomID)
	if len(events) < 2 || events[1].Type != "policy.changed" {
		t.Fatalf("第二条事件应为 policy.changed，got %+v", events)
	}
	if events[1].Actor.Kind != "human" || events[1].Visibility.Kind != "public" {
		t.Fatalf("policy.changed 应为人类 actor + public（记分卡透明）：%+v", events[1])
	}

	// 快照策略区投影回读（版本 = pol_2：默认束之后第一次变更）
	stored, _, _ := store.EventsAfter(ctx, created.RoomID, "", 100)
	snap := ProjectSnapshot(created.RoomID, stored)
	if snap.Policy.Mode != "roundtable" || snap.Policy.PolicyVersion != "pol_2" {
		t.Fatalf("快照策略区不符：%+v", snap.Policy)
	}

	// 幂等回放：同键同载荷返回原事件
	replay, err := svc.ExecuteCommand(ctx, actor, cmd)
	if err != nil || !replay.Replayed || replay.EventID != res.EventID {
		t.Fatalf("同键应回放：%v %+v", err, replay)
	}

	// 越界拒绝：未开放的 reveal 策略
	bad := cmd
	bad.IdempotencyKey = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e91"
	bad.ExpectedRoomVersion = 2
	bad.Payload = []byte(setPolicyBadRevealJSON)
	if _, err := svc.ExecuteCommand(ctx, actor, bad); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("未开放的 reveal 策略应拒绝，got %v", err)
	}

	// 严格字段集：未知字段拒绝
	bad2 := cmd
	bad2.IdempotencyKey = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e92"
	bad2.ExpectedRoomVersion = 2
	bad2.Payload = []byte(setPolicyUnknownFieldJSON)
	if _, err := svc.ExecuteCommand(ctx, actor, bad2); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("未知字段应拒绝，got %v", err)
	}
}
