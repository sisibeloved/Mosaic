//go:build st

// ST 层：RFC-0012 群聊交互模型——真实二进制：一条人类消息 → 去抖反应窗口 →
// 反应波（echo 意愿放行 + sequential 发布）→ 意愿静默收束（v1.40：结构冷却拆除，
// echo 对自己消息礼貌自决 silent → quiescent）；
// 快照 Timeline 不含 round.*（内部化），事件流仍可检视波链路。
package main_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func TestChatFlow_ST(t *testing.T) {
	bin := buildServer(t)
	cmd, base, _ := startLoggedServer(t, bin, t.TempDir(), "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9701", "issued_at": "2026-09-01T09:00:00.000Z",
		"payload": map[string]any{"display_name": "chat st"},
	})
	roomID := created["room_id"].(string)

	postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9702", "issued_at": "2026-09-01T09:00:01.000Z",
		"payload": map[string]any{"body": "群聊第一条", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	})

	client := &http.Client{Timeout: 5 * time.Second}
	// 等波收敛：两波收束（波1 echo 发言 → 波2 对自己消息礼貌自决 silent → quiescent）
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if closedCountST(t, client, base, roomID) >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(3 * time.Second) // 静默收束确认窗（若礼貌自决静默失效，第三波在此窗口内可见）

	if closedCountST(t, client, base, roomID) < 2 {
		t.Fatal("反应波未在时限内收敛")
	}
	opened := roundOpenedST(t, client, base, roomID)
	if len(opened) != 2 {
		t.Fatalf("发言后应由意愿静默收束：round.opened = %d（期望 2：发言波 + 静默收束波）", len(opened))
	}

	// 快照：Timeline 只有消息族（round.* 内部化不入列）
	resp, err := client.Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snap room.Snapshot
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	agentMsgs := 0
	for _, item := range snap.Timeline {
		if item.Type == protocol.EventRoundOpened || item.Type == protocol.EventRoundClosed {
			t.Fatalf("round.* 不得入 Timeline（RFC-0012 内部化）：%+v", item)
		}
		if item.ActorKind == "agent" {
			agentMsgs++
		}
	}
	if agentMsgs != 1 {
		t.Fatalf("echo 应恰发言一条：agent 消息 = %d（期望 1）", agentMsgs)
	}
}

func roundOpenedST(t *testing.T, client *http.Client, base, roomID string) []protocol.RoundOpenedPayload {
	t.Helper()
	resp, err := client.Get(base + "/v1/debug/rooms/" + roomID + "/events?type=round.opened")
	if err != nil {
		t.Fatalf("debug events: %v", err)
	}
	defer resp.Body.Close()
	var doc struct {
		Events []struct {
			Envelope protocol.Envelope `json:"envelope"`
		} `json:"events"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	var out []protocol.RoundOpenedPayload
	for _, item := range doc.Events {
		var p protocol.RoundOpenedPayload
		_ = json.Unmarshal(item.Envelope.Payload, &p)
		out = append(out, p)
	}
	return out
}

func closedCountST(t *testing.T, client *http.Client, base, roomID string) int {
	t.Helper()
	resp, err := client.Get(base + "/v1/debug/rooms/" + roomID + "/events?type=round.closed")
	if err != nil {
		t.Fatalf("debug events: %v", err)
	}
	defer resp.Body.Close()
	var doc struct {
		Events []json.RawMessage `json:"events"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	return len(doc.Events)
}
