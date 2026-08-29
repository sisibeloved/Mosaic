// UT 层：命令处理域（切片 A）——幂等 receipt、乐观并发、校验。
// TDD：本文件先行于 service.go（红→绿驱动）。
package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ---- 测试装置 ----

var testClock = func() string { return "2026-08-28T09:00:00.000Z" }

func newTestService(store AtomicStore) *Service {
	var mu sync.Mutex
	var n int64
	return NewService(Config{
		Store: store,
		Clock: testClock,
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("%s_test_%08d", prefix, n)
		},
		Tenant: "ten_local",
	})
}

func createCmd(kind, idem string, version int64, payload any) Command {
	raw, _ := json.Marshal(payload)
	return Command{
		CommandKind:         kind,
		ExpectedRoomVersion: version,
		IdempotencyKey:      idem,
		IssuedAt:            "2026-08-28T08:59:59.000Z",
		Payload:             raw,
	}
}

const validUUIDv7 = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e6f"

// ---- 用例 ----

func TestCreateRoom(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()

	res, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "测试房"}))
	if err != nil {
		t.Fatalf("create_room: %v", err)
	}
	if res.RoomVersion != 1 {
		t.Fatalf("room version = %d（期望 1）", res.RoomVersion)
	}
	events := store.byRoom[res.RoomID]
	if len(events) != 1 || events[0].Type != "room.created" {
		t.Fatalf("应产生 room.created，got %+v", events)
	}
	if events[0].Actor.ParticipantID != "par_owner" || events[0].Actor.Kind != "human" {
		t.Fatalf("actor 应为创建者人类：%+v", events[0].Actor)
	}
}

func TestPostMessageHappyPath(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()

	created, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "d"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"}, Command{
		RoomID:              created.RoomID,
		CommandKind:         "post_message",
		ExpectedRoomVersion: 1,
		IdempotencyKey:      "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e70",
		IssuedAt:            "2026-08-28T09:00:01.000Z",
		Payload:             []byte(`{"body":"第一条消息","reply_to":null,"addressed_to":[],"relations":[]}`),
	})
	if err != nil {
		t.Fatalf("post_message: %v", err)
	}
	if res.RoomVersion != 2 {
		t.Fatalf("version = %d（期望 2）", res.RoomVersion)
	}
	events := store.byRoom[created.RoomID]
	msg := events[1]
	if msg.Type != "message.posted" || msg.Seq != 2 {
		t.Fatalf("message.posted seq=2 expected，got type=%s seq=%d", msg.Type, msg.Seq)
	}
	var payload struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil || payload.Body != "第一条消息" {
		t.Fatalf("payload body 丢失：%v %q", err, payload.Body)
	}
}

func TestIdempotentReplayReturnsSameEvent(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()

	created, _ := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{}))

	cmd := Command{
		RoomID:              created.RoomID,
		CommandKind:         "post_message",
		ExpectedRoomVersion: 1,
		IdempotencyKey:      "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e71",
		IssuedAt:            "2026-08-28T09:00:02.000Z",
		Payload:             []byte(`{"body":"只出现一次"}`),
	}
	first, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"}, cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// 房间版本已前进；幂等回放不看版本（RFC-0001：回放返回已记录结果）
	cmd.ExpectedRoomVersion = first.RoomVersion
	second, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"}, cmd)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed {
		t.Fatal("回放必须标记 Replayed=true")
	}
	if second.EventID != first.EventID {
		t.Fatalf("回放返回不同事件：%s vs %s", second.EventID, first.EventID)
	}
	if len(store.byRoom[created.RoomID]) != 2 {
		t.Fatalf("回放不得追加事件，事件数 = %d", len(store.byRoom[created.RoomID]))
	}
}

func TestIdempotencyKeyConflict(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	created, _ := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{}))

	cmd := Command{
		RoomID:              created.RoomID,
		CommandKind:         "post_message",
		ExpectedRoomVersion: 1,
		IdempotencyKey:      "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e72",
		IssuedAt:            "2026-08-28T09:00:03.000Z",
		Payload:             []byte(`{"body":"原始内容"}`),
	}
	if _, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"}, cmd); err != nil {
		t.Fatalf("first: %v", err)
	}
	cmd.Payload = []byte(`{"body":"同键不同内容"}`)
	_, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"}, cmd)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("同键不同指纹应报 ErrIdempotencyConflict，got %v", err)
	}
}

func TestVersionConflict(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	created, _ := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{}))

	cmd := Command{
		RoomID:              created.RoomID,
		CommandKind:         "post_message",
		ExpectedRoomVersion: 99, // 过期版本
		IdempotencyKey:      "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e73",
		IssuedAt:            "2026-08-28T09:00:04.000Z",
		Payload:             []byte(`{"body":"stale"}`),
	}
	_, err := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"}, cmd)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本应报 ErrVersionConflict，got %v", err)
	}
	if len(store.byRoom[created.RoomID]) != 1 {
		t.Fatalf("冲突命令不得追加事件，事件数 = %d", len(store.byRoom[created.RoomID]))
	}
}

func TestPostMessageValidation(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	created, _ := svc.ExecuteCommand(ctx, Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{}))
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	cases := []struct {
		name string
		cmd  Command
		want error
	}{
		{"空 body", Command{created.RoomID, "post_message", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e74", "t", []byte(`{"body":""}`)}, ErrInvalidCommand},
		{"缺 body", Command{created.RoomID, "post_message", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e75", "t", []byte(`{}`)}, ErrInvalidCommand},
		{"body 超长", Command{created.RoomID, "post_message", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e76", "t", []byte(`{"body":"` + strings.Repeat("长", 20001) + `"}`)}, ErrInvalidCommand},
		{"非 UUIDv7 幂等键", Command{created.RoomID, "post_message", 1, "not-a-uuid", "t", []byte(`{"body":"x"}`)}, ErrInvalidCommand},
		{"未知命令", Command{created.RoomID, "teleport", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e77", "t", []byte(`{}`)}, ErrInvalidCommand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Command 字面量构造按字段名重排（RoomID, CommandKind, ExpectedRoomVersion, IdempotencyKey, IssuedAt, Payload）
			cmd := Command{RoomID: tc.cmd.RoomID, CommandKind: tc.cmd.CommandKind, ExpectedRoomVersion: tc.cmd.ExpectedRoomVersion, IdempotencyKey: tc.cmd.IdempotencyKey, IssuedAt: tc.cmd.IssuedAt, Payload: tc.cmd.Payload}
			_, err := svc.ExecuteCommand(ctx, actor, cmd)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestPostMessageToUnknownRoom(t *testing.T) {
	svc := newTestService(NewMemStore())
	_, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"}, Command{
		RoomID:              "room_ghost",
		CommandKind:         "post_message",
		ExpectedRoomVersion: 0,
		IdempotencyKey:      "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e78",
		IssuedAt:            "2026-08-28T09:00:05.000Z",
		Payload:             []byte(`{"body":"hello?"}`),
	})
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("未知房间应报 ErrRoomNotFound，got %v", err)
	}
}

// pause/resume 命令链：事件落库、幂等、版本并发。
func TestPauseResumeCommands(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	created, err := svc.ExecuteCommand(ctx, actor, createCmd("create_room", validUUIDv7, 0, map[string]any{}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pause := func(v int64, idem string) *CommandResult {
		res, err := svc.ExecuteCommand(ctx, actor, Command{
			RoomID: created.RoomID, CommandKind: "pause_room", ExpectedRoomVersion: v,
			IdempotencyKey: idem, IssuedAt: "t", Payload: []byte(`{"reason":"休息一下"}`),
		})
		if err != nil {
			t.Fatalf("pause: %v", err)
		}
		return res
	}
	res := pause(1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6101")
	events := store.RoomEvents(created.RoomID)
	if len(events) != 2 || events[1].Type != "room.paused" {
		t.Fatalf("应产生 room.paused：%s", events[1].Type)
	}
	var pp struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(events[1].Payload, &pp)
	if pp.Reason != "休息一下" {
		t.Fatalf("reason 丢失：%+v", pp)
	}
	// resume → room.started
	rres, err := svc.ExecuteCommand(ctx, actor, Command{
		RoomID: created.RoomID, CommandKind: "resume_room", ExpectedRoomVersion: res.RoomVersion,
		IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6102", IssuedAt: "t", Payload: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	events = store.RoomEvents(created.RoomID)
	if len(events) != 3 || events[2].Type != "room.started" {
		t.Fatalf("应产生 room.started：%s", events[2].Type)
	}
	if rres.RoomVersion != 3 {
		t.Fatalf("version = %d", rres.RoomVersion)
	}
}
