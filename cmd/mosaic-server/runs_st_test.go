//go:build st

// ST 层（M4-1 切片 B）：任务执行通道全链——受控执行（-dev slowrun 桩）→
// 结果以 assignee 名义回房（metadata.run_id 可追溯）→ SSE run.* 帧下发；
// 崩溃重启 → run.unknown（结果未知，不自动重跑）；设置族跨重启持久化。
package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// waitSlowrunSeat 轮询 /v1/agents 直至引擎装配完成（宿主扫描后 -dev 桩入席）。
func waitSlowrunSeat(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/v1/agents")
		if err == nil && resp.StatusCode == 200 {
			var doc struct {
				Agents []struct {
					ParticipantID string `json:"participant_id"`
				} `json:"agents"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&doc)
			resp.Body.Close()
			for _, a := range doc.Agents {
				if a.ParticipantID == "par_slowrun" {
					return
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("45s 内引擎未装配（par_slowrun 未入席）")
}

// snapshotRunsST 快照 runs 投影（轮询面）。
func snapshotRunsST(t *testing.T, base, roomID string) []map[string]any {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var snap map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	raw, _ := snap["runs"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// waitRunStatusST 轮询直至首个 run 达到目标态（或超时返回最后观测）。
func waitRunStatusST(t *testing.T, base, roomID, want string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		runs := snapshotRunsST(t, base, roomID)
		if len(runs) > 0 {
			last = runs[0]
			if last["status"] == want {
				return last
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("run 未在 %s 内达到 %s（最后观测 %+v）", timeout, want, last)
	return nil
}

// sseCollector 收集 SSE 帧类型（断线重连外的直播面验证——run.* 帧经 Hub 下发）。
func sseCollector(ctx context.Context, t *testing.T, base, roomID string) chan string {
	t.Helper()
	out := make(chan string, 256)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/rooms/"+roomID+"/events", nil)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				out <- strings.TrimPrefix(line, "event: ")
			}
		}
	}()
	return out
}

// TestRunLifecycle_ST：受控执行全链（M4-1 验收——"长于当前单轮时限"的可控
// 执行由 -dev slowrun 桩承担：SLEEP 指令控时、TaskRuns 能力真实声明）。
// run_task → run.started/完成 → 结果消息以 assignee 名义入房（metadata.run_id）
// → SSE 直播帧 run.started/message.posted/run.completed → 快照投影一致。
func TestRunLifecycle_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()
	cmd, base, _ := startLoggedServer(t, bin, dataDir, "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	waitSlowrunSeat(t, base)

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f01", "issued_at": "2026-09-07T00:00:01.000Z",
		"payload": map[string]any{"display_name": "run ST 房"},
	})
	roomID := created["room_id"].(string)

	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	frames := sseCollector(sseCtx, t, base, roomID)

	posted := postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "run_task", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f02", "issued_at": "2026-09-07T00:00:02.000Z",
		"payload": map[string]any{"assignee": "par_slowrun", "instruction": "SLEEP 2 然后报告：独立任务执行完成（ST 验证）"},
	})
	_ = posted

	run := waitRunStatusST(t, base, roomID, "completed", 30*time.Second)
	resultEventID, _ := run["result_event_id"].(string)
	if resultEventID == "" {
		t.Fatalf("完成 run 应携带结果消息: %+v", run)
	}

	// 结果消息：agent 名义 + 正文回房 + metadata.run_id（调试事件面核验可追溯）
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/debug/rooms/" + roomID + "/events")
	if err != nil {
		t.Fatalf("debug events: %v", err)
	}
	var eventsDoc struct {
		Events []struct {
			Envelope struct {
				EventID string `json:"event_id"`
				Type    string `json:"type"`
				Actor   struct {
					Kind          string `json:"kind"`
					ParticipantID string `json:"participant_id"`
				} `json:"actor"`
				Payload  json.RawMessage `json:"payload"`
				Metadata map[string]any  `json:"metadata"`
			} `json:"envelope"`
		} `json:"events"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eventsDoc)
	resp.Body.Close()
	foundResult := false
	for _, item := range eventsDoc.Events {
		env := item.Envelope
		if env.EventID != resultEventID || env.Type != "message.posted" {
			continue
		}
		foundResult = true
		if env.Actor.Kind != "agent" || env.Actor.ParticipantID != "par_slowrun" {
			t.Fatalf("结果消息应以 assignee 名义: %+v", env.Actor)
		}
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(env.Payload, &p)
		if !strings.Contains(p.Body, "独立任务执行完成") {
			t.Fatalf("结果正文应含桩回显: %q", p.Body)
		}
		if fmt.Sprint(env.Metadata["run_id"]) == "" {
			t.Fatalf("结果消息 metadata 应携 run_id: %+v", env.Metadata)
		}
	}
	if !foundResult {
		t.Fatal("结果消息事件缺失（debug 事件面）")
	}

	// SSE 直播帧：run.started / message.posted / run.completed
	wantFrames := map[string]bool{"run.started": false, "run.completed": false}
	sawAgentPost := false
	deadline := time.Now().Add(10 * time.Second)
	for (!wantFrames["run.started"] || !wantFrames["run.completed"] || !sawAgentPost) && time.Now().Before(deadline) {
		select {
		case f := <-frames:
			if _, ok := wantFrames[f]; ok {
				wantFrames[f] = true
			}
			if f == "message.posted" {
				sawAgentPost = true
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	for k, ok := range wantFrames {
		if !ok {
			t.Errorf("SSE 缺帧 %s", k)
		}
	}
	if !sawAgentPost {
		t.Error("SSE 缺 message.posted 帧（结果回房）")
	}
}

// TestRunRestartUnknown_ST：执行中崩溃重启 → run.unknown（不自动重跑——可能
// 已产生副作用）；设置族跨重启持久化（OQ-B 首员落盘面）。
func TestRunRestartUnknown_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()
	cmd, base, _ := startLoggedServer(t, bin, dataDir, "-dev")
	waitSlowrunSeat(t, base)

	// 设置面：PUT 生效 + 重启后仍在（同一 dataDir）
	putSettingsST(t, base, 120)

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f11", "issued_at": "2026-09-07T00:00:11.000Z",
		"payload": map[string]any{"display_name": "run 重启房"},
	})
	roomID := created["room_id"].(string)
	postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "run_task", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f12", "issued_at": "2026-09-07T00:00:12.000Z",
		"payload": map[string]any{"assignee": "par_slowrun", "instruction": "SLEEP 30 长活（重启验证）"},
	})
	waitRunStatusST(t, base, roomID, "running", 30*time.Second)

	// 崩溃（非优雅关停——优雅路径会取消在途 run 但同样不落终态）
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	cmd2, base2, _ := startLoggedServer(t, bin, dataDir, "-dev")
	defer func() { _ = cmd2.Process.Kill(); _, _ = cmd2.Process.Wait() }()
	waitSlowrunSeat(t, base2)

	run := waitRunStatusST(t, base2, roomID, "unknown", 45*time.Second)
	note, _ := run["error"].(string)
	if !strings.Contains(note, "结果未知") {
		t.Fatalf("unknown 注记应说明结果未知: %+v", run)
	}
	// 设置持久化
	getSettingsST(t, base2, 120)
}

// putSettingsST / getSettingsST 设置族端点面（写门携 token）。
func putSettingsST(t *testing.T, base string, seconds int) {
	t.Helper()
	raw := fmt.Sprintf(`{"run_timeout_seconds":%d}`, seconds)
	req, _ := http.NewRequest(http.MethodPut, base+"/v1/system/settings", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Owner-Token", ownerTokenST(t, base))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("put settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put settings status=%d", resp.StatusCode)
	}
}

func getSettingsST(t *testing.T, base string, want int) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/system/settings")
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	defer resp.Body.Close()
	var doc struct {
		RunTimeoutSeconds int `json:"run_timeout_seconds"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	if resp.StatusCode != 200 || doc.RunTimeoutSeconds != want {
		t.Fatalf("settings = %d (status %d), want %d", doc.RunTimeoutSeconds, resp.StatusCode, want)
	}
}
