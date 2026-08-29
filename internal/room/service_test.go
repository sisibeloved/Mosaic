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
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
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

// 二轮审校 #21：issued_at 非 RFC3339 拒收（运行时校验，不依赖 Schema 门禁）。
func TestIssuedAtMustBeRFC3339(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	bad := createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "x"})
	bad.IssuedAt = "yesterday"
	_, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"}, bad)
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("非法 issued_at 应拒收，got %v", err)
	}
}

// 复审 #22：并发同键重试不得误报 version_conflict——入口回放检查与版本预检之间
// 存在竞态窗口（首次执行恰在窗口内落回执）；预检冲突前必须重查回放。
// 全部结果应为：恰一次首发 + 其余回放，零 ErrVersionConflict。
func TestConcurrentSameKeyRetryNeverVersionConflict(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	first, err := svc.ExecuteCommand(ctx, actor, createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "race"}))
	if err != nil {
		t.Fatalf("create_room: %v", err)
	}
	cmd := createCmd("post_message", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7f01", 1,
		map[string]any{"body": "concurrent retry"})
	cmd.RoomID = first.RoomID

	const n = 16
	var wg sync.WaitGroup
	var conflicts atomic.Int32
	var replays atomic.Int32
	var others atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.ExecuteCommand(ctx, actor, cmd)
			switch {
			case err == nil && res.Replayed:
				replays.Add(1)
			case err == nil:
				// 首发胜出（恰一次）
			case errors.Is(err, ErrVersionConflict):
				conflicts.Add(1)
			default:
				others.Add(1)
			}
		}()
	}
	wg.Wait()
	if conflicts.Load() != 0 {
		t.Fatalf("并发同键重试出现 %d 次 version_conflict（应回放）", conflicts.Load())
	}
	if others.Load() != 0 {
		t.Fatalf("并发同键出现意外错误：%d", others.Load())
	}
	if replays.Load() != int32(n-1) {
		t.Fatalf("应恰一次首发 + %d 次回放，replayed=%d", n-1, replays.Load())
	}
}

// ---- M1 收口补课（审校 2026-08-28）：幂等/并发/回执三件套 ----

// D1：create_room 同键异载荷 → 幂等冲突（不得静默回放原房间）。
func TestCreateRoomIdempotencyConflict(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	first, err := svc.ExecuteCommand(ctx, actor, createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "A"}))
	if err != nil {
		t.Fatalf("首次 create_room: %v", err)
	}
	_, err = svc.ExecuteCommand(ctx, actor, createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "B"}))
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("同键异载荷应报 ErrIdempotencyConflict，got %v", err)
	}
	// 同载荷重放：返回原房间
	res, err := svc.ExecuteCommand(ctx, actor, createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "A"}))
	if err != nil || !res.Replayed || res.RoomID != first.RoomID {
		t.Fatalf("同载荷重放不符：%+v %v", res, err)
	}
}

// D2：并发同版本命令只有一个成功——check-then-append 竞态由存储事务内校验封死。
func TestConcurrentSameVersionOnlyOneWins(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	created, err := svc.ExecuteCommand(ctx, actor, createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "race"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const n = 8
	var wg sync.WaitGroup
	var okCount, conflictCount int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := Command{
				RoomID:              created.RoomID,
				CommandKind:         "post_message",
				ExpectedRoomVersion: 1, // 全部基于同一版本
				IdempotencyKey:      fmt.Sprintf("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e%02x", i),
				IssuedAt:            "2026-08-28T12:00:00.000Z",
				Payload:             []byte(fmt.Sprintf(`{"body":"msg %d"}`, i)),
			}
			_, err := svc.ExecuteCommand(ctx, actor, cmd)
			switch {
			case err == nil:
				atomic.AddInt64(&okCount, 1)
			case errors.Is(err, ErrVersionConflict):
				atomic.AddInt64(&conflictCount, 1)
			default:
				t.Errorf("并发命令出现意外错误：%v", err)
			}
		}(i)
	}
	wg.Wait()
	if okCount != 1 || conflictCount != n-1 {
		t.Fatalf("ok=%d conflict=%d（期望 1/%d）", okCount, conflictCount, n-1)
	}
}

// D2/D3：存储端口契约——回执式追加在事务内强制乐观并发，并权威回填 RoomVersion。
func TestStoreReceiptAppendContract(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{
		{EventID: "evt_c1", TenantID: "ten_local", RoomID: "room_c", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 过期 expected（0 ≠ 当前 1）→ 事务内拒绝
	_, err := store.AppendWithReceipt(ctx, []protocol.Envelope{
		{EventID: "evt_c2", TenantID: "ten_local", RoomID: "room_c", Type: protocol.EventMessagePosted,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}, CommandReceipt{
		TenantID: "ten_local", RoomID: "room_c",
		IdempotencyKey: validUUIDv7, CommandKind: "post_message",
		RequestFingerprint: "fp", EventID: "evt_c2",
		ExpectedRoomVersion: 0, // 过期
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期 expected 版本应报 ErrVersionConflict，got %v", err)
	}
	// 正确 expected（=1）→ 落库且回执 RoomVersion 由存储权威回填（调用方传 999 也不信）
	rc := CommandReceipt{
		TenantID: "ten_local", RoomID: "room_c",
		IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e71", CommandKind: "post_message",
		RequestFingerprint: "fp", EventID: "evt_c3",
		ExpectedRoomVersion: 1, RoomVersion: 999, // 999 应被忽略
	}
	if _, err := store.AppendWithReceipt(ctx, []protocol.Envelope{
		{EventID: "evt_c3", TenantID: "ten_local", RoomID: "room_c", Type: protocol.EventMessagePosted,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}, rc); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := store.LookupReceipt(ctx, "ten_local", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e71", "post_message")
	if err != nil || got == nil {
		t.Fatalf("lookup: %v %v", got, err)
	}
	if got.RoomVersion != 2 {
		t.Fatalf("回执 RoomVersion 应权威回填为 2，got %d", got.RoomVersion)
	}
}

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
			IdempotencyKey: idem, IssuedAt: "2026-08-28T12:00:00.000Z", Payload: []byte(`{"reason":"休息一下"}`),
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
		IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6102", IssuedAt: "2026-08-28T12:00:00.000Z", Payload: []byte(`{}`),
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
