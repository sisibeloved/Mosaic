// UT 层：httpapi——UI 重设计切片 1 的对外端点：
// GET /v1/rooms 房间列表、rename_room 命令端点、快照 participants 装配注入。
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// getJSONInto 读端点解码助手（本文件三个用例共用）。
func getJSONInto(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp
}

func TestListRoomsEndpoint(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// 空库：rooms 为 [] 而非 null
	var empty apigen.RoomList
	resp := getJSONInto(t, ts.URL+"/v1/rooms", &empty)
	if resp.StatusCode != 200 || empty.Rooms == nil || len(empty.Rooms) != 0 {
		t.Fatalf("空库应 200 + 空数组：status=%d rooms=%v", resp.StatusCode, empty.Rooms)
	}

	_, roomA := postJSON(t, ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8001", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "甲房"},
	})
	_, roomB := postJSON(t, ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8002", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "乙房"},
	})
	postJSON(t, ts.URL+"/v1/rooms/"+roomA.RoomId+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8003", "issued_at": "2026-08-28T12:00:01.000Z",
		"payload": map[string]any{"body": "hello"},
	})
	postJSON(t, ts.URL+"/v1/rooms/"+roomB.RoomId+"/commands", map[string]any{
		"command_kind": "pause_room", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8004", "issued_at": "2026-08-28T12:00:02.000Z",
		"payload": map[string]any{"reason": "停"},
	})

	var list apigen.RoomList
	resp = getJSONInto(t, ts.URL+"/v1/rooms", &list)
	if resp.StatusCode != 200 || len(list.Rooms) != 2 {
		t.Fatalf("列表应 200 + 两项：status=%d body=%+v", resp.StatusCode, list)
	}
	byID := map[string]apigen.RoomSummary{}
	for _, r := range list.Rooms {
		byID[r.RoomId] = r
	}
	a, okA := byID[roomA.RoomId]
	b, okB := byID[roomB.RoomId]
	if !okA || !okB {
		t.Fatalf("列表缺房间：%+v", list)
	}
	if a.DisplayName != "甲房" || a.MessageCount != 1 || a.Paused || a.CreatedAt.IsZero() || a.LastEventAt.Before(a.CreatedAt) {
		t.Fatalf("A 摘要不符：%+v", a)
	}
	if b.DisplayName != "乙房" || b.MessageCount != 0 || !b.Paused {
		t.Fatalf("B 摘要不符（paused 位）：%+v", b)
	}
}

func TestRenameRoomEndpoint(t *testing.T) {
	ts, _, _ := newTestServer(t)

	_, created := postJSON(t, ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8101", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "旧名"},
	})
	url := ts.URL + "/v1/rooms/" + created.RoomId + "/commands"
	renameCmd := map[string]any{
		"command_kind": "rename_room", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8102", "issued_at": "2026-08-28T12:01:00.000Z",
		"payload": map[string]any{"display_name": "新名"},
	}

	// 正常改名：200 + room_version=2
	resp, renamed := postJSON(t, url, renameCmd)
	if resp.StatusCode != 200 || renamed.RoomVersion != 2 || renamed.Replayed {
		t.Fatalf("rename: status=%d body=%+v", resp.StatusCode, renamed)
	}

	// 幂等重放：replayed=true 且同事件
	resp, replay := postJSON(t, url, renameCmd)
	if resp.StatusCode != 200 || !replay.Replayed || replay.EventId != renamed.EventId {
		t.Fatalf("replay: status=%d body=%+v", resp.StatusCode, replay)
	}

	// 版本冲突 409
	resp, _ = postJSON(t, url, map[string]any{
		"command_kind": "rename_room", "expected_room_version": 99,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8103", "issued_at": "2026-08-28T12:02:00.000Z",
		"payload": map[string]any{"display_name": "迟到改名"},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("version conflict status = %d", resp.StatusCode)
	}

	// 空名 400
	resp, _ = postJSON(t, url, map[string]any{
		"command_kind": "rename_room", "expected_room_version": 2,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8104", "issued_at": "2026-08-28T12:03:00.000Z",
		"payload": map[string]any{"display_name": "  "},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty name status = %d", resp.StatusCode)
	}

	// 列表与快照均体现新名
	var list apigen.RoomList
	getJSONInto(t, ts.URL+"/v1/rooms", &list)
	if len(list.Rooms) != 1 || list.Rooms[0].DisplayName != "新名" {
		t.Fatalf("列表应体现新名：%+v", list)
	}
	var snap map[string]any
	resp = getJSONInto(t, ts.URL+"/v1/rooms/"+created.RoomId+"/snapshot", &snap)
	if resp.StatusCode != 200 || snap["display_name"] != "新名" {
		t.Fatalf("快照应体现新名：status=%d display_name=%v", resp.StatusCode, snap["display_name"])
	}
}

func TestSnapshotParticipants(t *testing.T) {
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := room.NewService(room.Config{
		Store:  store,
		Lister: store,
		Clock:  func() string { return "2026-08-28T11:00:00.000Z" },
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return prefix + "_api_" + fmt.Sprintf("%08d", n)
		},
		Tenant: "ten_local",
	})
	handler := New(Deps{
		SVC:    svc,
		Reader: store,
		Hub:    sse.NewHub(),
		Actor:  room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Seats: func() []room.AgentSeat {
			return []room.AgentSeat{
				{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"}},
				{ParticipantID: "par_codex_x", Profile: agent.Profile{ProfileID: "prof_codex_x", Adapter: "codex", DisplayName: "Codex", Channel: "app:codex-desktop"}},
			}
		},
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	_, created := postJSON(t, ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8201", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "参与者房"},
	})

	var snap map[string]any
	resp := getJSONInto(t, ts.URL+"/v1/rooms/"+created.RoomId+"/snapshot", &snap)
	if resp.StatusCode != 200 {
		t.Fatalf("snapshot status = %d", resp.StatusCode)
	}
	parts, ok := snap["participants"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("participants 应为 3 项（owner + 2 座位）：%v", snap["participants"])
	}
	owner, _ := parts[0].(map[string]any)
	if owner["participant_id"] != "par_owner" || owner["kind"] != "human" ||
		owner["display_name"] != "Owner" || owner["seat_status"] != "seated" {
		t.Fatalf("owner 项不符：%v", owner)
	}
	if _, has := owner["adapter"]; has {
		t.Fatalf("human 不得带 adapter 键：%v", owner)
	}
	echo, _ := parts[1].(map[string]any)
	if echo["participant_id"] != "par_echo" || echo["kind"] != "agent" ||
		echo["display_name"] != "Echo" || echo["adapter"] != "echo" || echo["seat_status"] != "seated" {
		t.Fatalf("echo 座位项不符：%v", echo)
	}
	if _, has := echo["channel"]; has {
		t.Fatalf("无渠道的座位应省略 channel 键：%v", echo)
	}
	codex, _ := parts[2].(map[string]any)
	if codex["participant_id"] != "par_codex_x" || codex["display_name"] != "Codex" ||
		codex["adapter"] != "codex" || codex["channel"] != "app:codex-desktop" {
		t.Fatalf("codex 座位项（含渠道）不符：%v", codex)
	}
}
