// UT 层：房间列表只读路径（GET /v1/rooms；UI 重设计切片 1）——
// 空库/多房间/排序/message_count/paused 位/rename 后名字投影。
package room

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// newListService 可控时钟的测试服务（排序用例依赖 occurred_at 推进）。
func newListService(store *MemStore, now *string) *Service {
	var mu sync.Mutex
	var n int64
	return NewService(Config{
		Store:  store,
		Lister: store,
		Clock:  func() string { return *now },
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("%s_list_%08d", prefix, n)
		},
		Tenant: "ten_local",
	})
}

func TestListRooms(t *testing.T) {
	store := NewMemStore()
	now := "2026-08-31T09:00:00.000Z"
	svc := newListService(store, &now)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	// 空库：空列表而非 null
	rooms, err := svc.ListRooms(ctx)
	if err != nil || len(rooms) != 0 {
		t.Fatalf("空库应为空列表：%v %+v", err, rooms)
	}

	// 两个房间（时刻推进：b 更新）
	roomA, err := svc.ExecuteCommand(ctx, actor,
		createCmd("create_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7200", 0, map[string]any{"display_name": "甲房"}))
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	now = "2026-08-31T09:01:00.000Z"
	roomB, err := svc.ExecuteCommand(ctx, actor,
		createCmd("create_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7201", 0, map[string]any{"display_name": "乙房"}))
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	// 倒序：最新活动的房间在前
	rooms, err = svc.ListRooms(ctx)
	if err != nil || len(rooms) != 2 {
		t.Fatalf("两房间：%v %+v", err, rooms)
	}
	if rooms[0].RoomID != roomB.RoomID || rooms[1].RoomID != roomA.RoomID {
		t.Fatalf("应按 last_event_at 倒序：%+v", rooms)
	}
	for _, r := range rooms {
		if r.CreatedAt == "" || r.LastEventAt != r.CreatedAt || r.Paused || r.MessageCount != 0 {
			t.Fatalf("新建房间摘要基线不符：%+v", r)
		}
	}

	// A 发两条消息 → message_count=2，last_event_at 推进后重回列表首
	now = "2026-08-31T09:02:00.000Z"
	for i, key := range []string{"018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7202", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7203"} {
		msg := createCmd("post_message", key, int64(1+i), map[string]any{"body": "hi"})
		msg.RoomID = roomA.RoomID
		if _, err := svc.ExecuteCommand(ctx, actor, msg); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	// A 暂停 → paused 位
	pause := createCmd("pause_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7204", 3, map[string]any{"reason": "停"})
	pause.RoomID = roomA.RoomID
	if _, err := svc.ExecuteCommand(ctx, actor, pause); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// B 改名 → display_name 投影新名
	rename := createCmd("rename_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7205", 1, map[string]any{"display_name": "乙房·改"})
	rename.RoomID = roomB.RoomID
	if _, err := svc.ExecuteCommand(ctx, actor, rename); err != nil {
		t.Fatalf("rename: %v", err)
	}

	rooms, err = svc.ListRooms(ctx)
	if err != nil || len(rooms) != 2 {
		t.Fatalf("两房间：%v %+v", err, rooms)
	}
	a, b := rooms[0], rooms[1]
	if a.RoomID != roomA.RoomID {
		t.Fatalf("A 最新活动应居首：%+v", rooms)
	}
	if a.DisplayName != "甲房" || a.MessageCount != 2 || !a.Paused || a.LastEventAt != "2026-08-31T09:02:00.000Z" {
		t.Fatalf("A 摘要不符：%+v", a)
	}
	if b.RoomID != roomB.RoomID || b.DisplayName != "乙房·改" || b.Paused || b.MessageCount != 0 {
		t.Fatalf("B 摘要（rename 投影）不符：%+v", b)
	}

	// 恢复后 paused 位回落
	resume := createCmd("resume_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7206", 4, map[string]any{})
	resume.RoomID = roomA.RoomID
	if _, err := svc.ExecuteCommand(ctx, actor, resume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	rooms, err = svc.ListRooms(ctx)
	if err != nil || rooms[0].RoomID != roomA.RoomID || rooms[0].Paused {
		t.Fatalf("resume 后 A 应非暂停：%v %+v", err, rooms)
	}
}
