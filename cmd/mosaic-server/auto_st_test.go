//go:build st

// ST 层：自动续聊（轮次自驱动，RFC-0003 §3.1.7 / 计划 v1.26）——真实二进制
// 默认束（open_floor auto_rounds=3）：一条人类消息 → 1 人类轮 + 3 自动轮收口，
// round.opened.auto_index 1..3 递增，快照 Timeline 持久化自动轮标记，链不越限。
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

func TestAutoRoundContinuation_ST(t *testing.T) {
	bin := buildServer(t)
	cmd, base, _ := startLoggedServer(t, bin, t.TempDir(), "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9701", "issued_at": "2026-09-01T09:00:00.000Z",
		"payload": map[string]any{"display_name": "auto st"},
	})
	roomID := created["room_id"].(string)
	version := int(created["room_version"].(float64))

	// 默认束自动续聊=3（快照策略区可见——OQ-17 透明）
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snap room.Snapshot
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if snap.Policy.AutoRounds != 3 {
		t.Fatalf("默认束 auto_rounds 应为 3：%+v", snap.Policy)
	}

	postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": version,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9702", "issued_at": "2026-09-01T09:00:01.000Z",
		"payload": map[string]any{"body": "一条消息，agents 自己接力", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	})

	// 等链收口：4 轮 closed（1 人类 + 3 自动），再留窗确认无第 5 轮
	deadline := time.Now().Add(60 * time.Second)
	var opened []protocol.RoundOpenedPayload
	for time.Now().Before(deadline) {
		opened = roundOpenedST(t, client, base, roomID)
		if len(opened) >= 4 && closedCountST(t, client, base, roomID) >= 4 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)

	opened = roundOpenedST(t, client, base, roomID)
	if len(opened) != 4 {
		t.Fatalf("round.opened = %d（期望 4：1 人类 + 3 自动）", len(opened))
	}
	for i, want := range []int{0, 1, 2, 3} {
		if opened[i].AutoIndex != want {
			t.Fatalf("第 %d 轮 auto_index = %d（期望 %d）：%+v", i+1, opened[i].AutoIndex, want, opened[i])
		}
	}

	// 快照 Timeline 持久化自动轮标记（v1.25 双路语义延伸：切房间不丢）
	resp2, err := client.Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot2: %v", err)
	}
	var snap2 room.Snapshot
	_ = json.NewDecoder(resp2.Body).Decode(&snap2)
	resp2.Body.Close()
	autoTimeline := 0
	for _, item := range snap2.Timeline {
		if item.Type == protocol.EventRoundOpened && item.AutoIndex > 0 {
			autoTimeline++
		}
	}
	if autoTimeline != 3 {
		t.Fatalf("快照 Timeline 自动轮标记 = %d（期望 3）", autoTimeline)
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
