//go:build st

// ST 层：策略事件溯源（B1）——真实二进制 set_policy → 快照策略区版本化 →
// 后续轮次按新模式开（round.opened 携带投影策略与 pol_2 版本）。
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

func TestSetPolicyFlow_ST(t *testing.T) {
	bin := buildServer(t)
	cmd, base, _ := startLoggedServer(t, bin, t.TempDir(), "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9601", "issued_at": "2026-08-31T09:00:00.000Z",
		"payload": map[string]any{"display_name": "policy st"},
	})
	roomID := created["room_id"].(string)
	version := int(created["room_version"].(float64))

	// set_policy → roundtable 默认束
	posted := postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "set_policy", "expected_room_version": version,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9602", "issued_at": "2026-08-31T09:00:01.000Z",
		"payload": map[string]any{
			"mode": "roundtable", "max_speakers": 8, "lambda": 0.3,
			"weights": map[string]any{"relevance": 0.3, "novelty": 0.2, "diversity": 0.15,
				"urgency": 0.1, "direct_address": 0.15, "floor_share": 0.05, "repetition": 0.05},
			"intent_window": "30s", "response_cap": 600, "reveal_strategy": "sequential",
			"rebuttals": 1, "auto_rounds": 0,
		},
	})
	nextVersion := int(posted["room_version"].(float64))

	// 快照策略区（记分卡透明）：模式与版本可查
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snap room.Snapshot
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if snap.Policy.Mode != "roundtable" || snap.Policy.PolicyVersion != "pol_2" {
		t.Fatalf("快照策略区不符：%+v", snap.Policy)
	}

	// 新刺激 → 轮按投影策略开
	postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": nextVersion,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9603", "issued_at": "2026-08-31T09:00:02.000Z",
		"payload": map[string]any{"body": "模式切换后的第一条", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	})

	deadline := time.Now().Add(30 * time.Second)
	var opened protocol.RoundOpenedPayload
	for time.Now().Before(deadline) {
		resp2, err := client.Get(base + "/v1/debug/rooms/" + roomID + "/events?type=round.opened")
		if err == nil {
			var doc struct {
				Events []struct {
					Envelope protocol.Envelope `json:"envelope"`
				} `json:"events"`
			}
			if raw, rerr := io.ReadAll(resp2.Body); rerr == nil && json.Unmarshal(raw, &doc) == nil {
				for _, item := range doc.Events {
					_ = json.Unmarshal(item.Envelope.Payload, &opened)
				}
			}
			resp2.Body.Close()
		}
		if opened.Mode != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if opened.Mode != "roundtable" || opened.PolicyVersion != "pol_2" ||
		opened.RevealStrategy != "sequential" || opened.IntentWindow != "30s" {
		t.Fatalf("round.opened 未按投影策略开：%+v", opened)
	}
}
