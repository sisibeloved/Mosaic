//go:build st

// ST 层（M4-5）：监控变化检测全链——脚本源登记 → 手动检查 → 变化经 run_task
// 交给 slowrun 分析 → 结果消息回房 → 送达水位推进；无变化不调模型（去重）；
// 源失败独立记录（不动水位）；恢复后不漏变化。
package main_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"
)

func monitorsST(t *testing.T, base string) []map[string]any {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/monitors")
	if err != nil {
		t.Fatalf("monitors: %v", err)
	}
	defer resp.Body.Close()
	var doc struct {
		Monitors []map[string]any `json:"monitors"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return doc.Monitors
}

func monitorStateOf(t *testing.T, base, id string) map[string]any {
	t.Helper()
	for _, m := range monitorsST(t, base) {
		if m["monitor_id"] == id {
			st, _ := m["state"].(map[string]any)
			return st
		}
	}
	t.Fatalf("监控源不存在: %s", id)
	return nil
}

func roomRunCountST(t *testing.T, base, roomID string) int {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer resp.Body.Close()
	var snap map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	runs, _ := snap["runs"].([]any)
	return len(runs)
}

func postMonitorActionST(t *testing.T, base, tok, id, action string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/monitors/"+id+"/"+action, nil)
	req.Header.Set("X-Owner-Token", tok)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s status=%d", action, resp.StatusCode)
	}
}

func TestMonitorScriptLifecycle_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()
	cmd, base, _ := startLoggedServer(t, bin, dataDir, "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	waitSlowrunSeat(t, base)
	tok := ownerTokenST(t, base)

	// 观察目标：脚本 cat 状态文件（按平台生成可执行面——Windows 用 .bat）
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.txt")
	script := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(stateFile, []byte("版本=v1\n"), 0o600); err != nil {
		t.Fatalf("state: %v", err)
	}
	if goruntime.GOOS == "windows" {
		script = filepath.Join(dir, "check.bat")
		if err := os.WriteFile(script, []byte("@type "+stateFile+"\r\n"), 0o755); err != nil {
			t.Fatalf("script: %v", err)
		}
	} else if err := os.WriteFile(script, []byte("#!/bin/sh\ncat "+stateFile+"\n"), 0o755); err != nil {
		t.Fatalf("script: %v", err)
	}

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": stUUIDv7(), "issued_at": "2026-09-07T21:00:01.000Z",
		"payload": map[string]any{"display_name": "监控 ST 房"},
	})
	roomID := created["room_id"].(string)

	// 登记（interval 60s——测试窗内由手动检查驱动）
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/monitors", jsonBody(map[string]any{
		"kind": "script", "target": script, "room_id": roomID,
		"assignee": "par_slowrun", "interval_sec": 60,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Owner-Token", tok)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	var created2 struct {
		MonitorID string `json:"monitor_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created2)
	resp.Body.Close()
	if resp.StatusCode != 201 || created2.MonitorID == "" {
		t.Fatalf("登记失败: %d %+v", resp.StatusCode, created2)
	}
	monID := created2.MonitorID

	// 首次检查 = 首次观察（变化）→ run → 结果回房 → 送达水位
	postMonitorActionST(t, base, tok, monID, "check")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st := monitorStateOf(t, base, monID)
		if st["last_delivered"] != nil && st["last_delivered"].(string) != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if st := monitorStateOf(t, base, monID); st["last_delivered"] == nil || st["last_delivered"].(string) == "" {
		t.Fatalf("首次观察应完成送达: %+v", st)
	}
	if got := roomRunCountST(t, base, roomID); got != 1 {
		t.Fatalf("首次观察应恰 1 次 run, got %d", got)
	}
	// 结果消息回房（分析正文）
	resp2, _ := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/v1/rooms/" + roomID + "/snapshot")
	var snap struct {
		Timeline []struct {
			ActorKind string `json:"actor_kind"`
			Body      string `json:"body"`
		} `json:"timeline"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&snap)
	resp2.Body.Close()
	found := false
	for _, it := range snap.Timeline {
		if it.ActorKind == "agent" && len(it.Body) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("监控分析结果消息未回房")
	}

	// 无变化：不调模型（run 数不变；no-change 计数递增）
	postMonitorActionST(t, base, tok, monID, "check")
	time.Sleep(500 * time.Millisecond)
	if got := roomRunCountST(t, base, roomID); got != 1 {
		t.Fatalf("无变化不得再发起 run, got %d", got)
	}
	if st := monitorStateOf(t, base, monID); st["no_change_count"].(float64) < 1 {
		t.Fatalf("no-change 计数应递增: %+v", st)
	}

	// 变化 → 新 run → 送达
	if err := os.WriteFile(stateFile, []byte("版本=v2\n发布=beta\n"), 0o600); err != nil {
		t.Fatalf("state v2: %v", err)
	}
	postMonitorActionST(t, base, tok, monID, "check")
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if got := roomRunCountST(t, base, roomID); got == 2 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got := roomRunCountST(t, base, roomID); got != 2 {
		t.Fatalf("变化应发起新 run, got %d", got)
	}

	// 源失败：独立记录、不动水位、不调模型（覆写为失败脚本——chmod 在 Windows
	// 不剥夺 .bat 可执行性，内容替换双平台确定）
	goodScript, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	failScript := []byte("#!/bin/sh\nexit 1\n")
	if goruntime.GOOS == "windows" {
		failScript = []byte("@exit /b 1\r\n")
	}
	if err := os.WriteFile(script, failScript, 0o755); err != nil {
		t.Fatalf("break script: %v", err)
	}
	postMonitorActionST(t, base, tok, monID, "check")
	time.Sleep(500 * time.Millisecond)
	st := monitorStateOf(t, base, monID)
	if st["last_error"] == nil || st["last_error"].(string) == "" {
		t.Fatalf("源失败应记录: %+v", st)
	}
	if got := roomRunCountST(t, base, roomID); got != 2 {
		t.Fatalf("源失败不得发起 run, got %d", got)
	}

	// 恢复（无新变化）：错误清空、不补跑
	if err := os.WriteFile(script, goodScript, 0o755); err != nil {
		t.Fatalf("restore script: %v", err)
	}
	postMonitorActionST(t, base, tok, monID, "check")
	time.Sleep(500 * time.Millisecond)
	st = monitorStateOf(t, base, monID)
	if st["last_error"] != nil && st["last_error"].(string) != "" {
		t.Fatalf("恢复后错误应清空: %+v", st)
	}
	if got := roomRunCountST(t, base, roomID); got != 2 {
		t.Fatalf("恢复且无变化不得发起 run, got %d", got)
	}

	// 删除
	req3, _ := http.NewRequest(http.MethodDelete, base+"/v1/monitors/"+monID, nil)
	req3.Header.Set("X-Owner-Token", tok)
	resp3, _ := (&http.Client{Timeout: 10 * time.Second}).Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("删除失败: %d", resp3.StatusCode)
	}
	if got := len(monitorsST(t, base)); got != 0 {
		t.Fatalf("删除后列表应空, got %d", got)
	}
}
