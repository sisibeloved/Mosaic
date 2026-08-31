// UT 层：rename_room 命令链（UI 重设计切片 1）——受理/版本冲突/幂等回放/校验/事件落日志。
package room

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func TestRenameRoomCommand(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	created, err := svc.ExecuteCommand(ctx, actor,
		createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "旧名"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rename := createCmd("rename_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7001", 1,
		map[string]any{"display_name": "新名"})
	rename.RoomID = created.RoomID
	res, err := svc.ExecuteCommand(ctx, actor, rename)
	if err != nil || res.Replayed || res.RoomVersion != 2 {
		t.Fatalf("rename 应首发受理：%v %+v", err, res)
	}

	// 事件落日志：第二条为 room.renamed，载荷带新名
	events := store.RoomEvents(created.RoomID)
	if len(events) != 2 || events[1].Type != protocol.EventRoomRenamed {
		t.Fatalf("第二条事件应为 room.renamed，got %+v", events)
	}
	var p struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(events[1].Payload, &p); err != nil || p.DisplayName != "新名" {
		t.Fatalf("room.renamed 载荷不符：%s", events[1].Payload)
	}

	// 事件可重放：投影重建后快照体现新名
	stored, _, err := store.EventsAfter(ctx, created.RoomID, "", 100)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	if snap := ProjectSnapshot(created.RoomID, stored); snap.DisplayName != "新名" {
		t.Fatalf("快照 display_name = %q（应投影 room.renamed 新名）", snap.DisplayName)
	}

	// 幂等回放：同键同载荷 → replayed=true 且同事件
	replay, err := svc.ExecuteCommand(ctx, actor, rename)
	if err != nil || !replay.Replayed || replay.EventID != res.EventID || replay.RoomVersion != 2 {
		t.Fatalf("同键应回放：%v %+v", err, replay)
	}
	if n := len(store.RoomEvents(created.RoomID)); n != 2 {
		t.Fatalf("回放不得新增事件，事件数 = %d", n)
	}

	// 版本冲突：过期 expected_room_version 拒绝
	stale := createCmd("rename_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7002", 99,
		map[string]any{"display_name": "迟到改名"})
	stale.RoomID = created.RoomID
	if _, err := svc.ExecuteCommand(ctx, actor, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本应报 version conflict，got %v", err)
	}

	// 空名/纯空白/超长拒绝（改名不接受置空）
	keys := []string{
		"018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7100",
		"018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7101",
		"018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7102",
	}
	for i, name := range []string{"", "   ", strings.Repeat("长", 121)} {
		bad := createCmd("rename_room", keys[i], 2, map[string]any{"display_name": name})
		bad.RoomID = created.RoomID
		if _, err := svc.ExecuteCommand(ctx, actor, bad); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("非法 display_name（case %d）应拒收，got %v", i, err)
		}
	}

	// 严格字段集：未知字段拒绝
	unknown := createCmd("rename_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7005", 2,
		map[string]any{"display_name": "x", "secret": "y"})
	unknown.RoomID = created.RoomID
	if _, err := svc.ExecuteCommand(ctx, actor, unknown); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("未知字段应拒收，got %v", err)
	}

	// 未知房间 404 语义
	ghost := createCmd("rename_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7006", 0,
		map[string]any{"display_name": "x"})
	ghost.RoomID = "room_ghost"
	if _, err := svc.ExecuteCommand(ctx, actor, ghost); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("未知房间应报 room not found，got %v", err)
	}
}
