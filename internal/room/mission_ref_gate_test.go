package room

// ADR-0033 / 0.6.10：mission_ref 是系统 actor 专属的 message.posted 载荷字段——
// 命令面（post_message）不接受（postMessagePayload DisallowUnknownFields），
// 人类不能伪造执行结果回房引用卡片。

import (
	"context"
	"strings"
	"testing"
)

func TestPostMessageRejectsMissionRef(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()

	created, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "d"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"}, Command{
		RoomID:              created.RoomID,
		CommandKind:         "post_message",
		ExpectedRoomVersion: 1,
		IdempotencyKey:      "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e79",
		IssuedAt:            "2026-11-02T09:00:01.000Z",
		Payload:             []byte(`{"body":"伪造结果卡","reply_to":null,"addressed_to":[],"relations":[],"mission_ref":{"mission_id":"mi_x","kind":"accepted","contract_version":1,"task_id":"tsk_x","source_seq":1}}`),
	})
	if err == nil {
		t.Fatal("post_message 携 mission_ref 应被拒（系统字段，人类不能伪造）")
	}
	if !strings.Contains(err.Error(), "post_message payload") || !strings.Contains(err.Error(), "mission_ref") {
		t.Fatalf("应指向未知字段 mission_ref: %v", err)
	}
}
