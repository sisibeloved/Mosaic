//go:build st

// ST 层（M4-4）：显式路由拒绝面 + 稳定身份跨重启（真机 opt-in——CI 无真实
// CLI 时如实 skip；负责人开发机跑全链）。
package main_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// stUUIDv7 幂等键（48bit 毫秒时间戳 + 版本/变体位 + 随机尾——与服务端契约一致）。
func stUUIDv7() string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	r := hex.EncodeToString(b[:])
	t := fmt.Sprintf("%012x", time.Now().UnixMilli())
	return t[:8] + "-" + t[8:12] + "-7" + r[:3] + "-9" + r[3:6] + "-" + r[6:18]
}

func jsonBody(v any) *bytes.Reader {
	raw, _ := json.Marshal(v)
	return bytes.NewReader(raw)
}

// enableST 启用注册表项（id 含路径分隔符，需 URL 编码）。
func enableST(t *testing.T, base, exeID string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/harness/executables/"+url.PathEscape(exeID)+"/enable", nil)
	req.Header.Set("X-Owner-Token", ownerTokenST(t, base))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("enable status=%d", resp.StatusCode)
	}
}

// TestRunRoutingReject_ST：未知连接显式拒（invalid_command，错误面明示不回落）。
// 连接校验先于能力门——echo 房即可钉住契约。
func TestRunRoutingReject_ST(t *testing.T) {
	bin := buildServer(t)
	cmd, base, _ := startLoggedServer(t, bin, t.TempDir())
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e01", "issued_at": "2026-09-07T20:00:01.000Z",
		"payload": map[string]any{"display_name": "路由 ST"},
	})
	roomID := created["room_id"].(string)

	for _, conn := range []string{"remote-foo", "Local"} { // 大小写敏感：未知即拒
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/rooms/"+roomID+"/commands",
			jsonBody(map[string]any{
				"command_kind": "run_task", "expected_room_version": 1,
				"idempotency_key": stUUIDv7(), "issued_at": "2026-09-07T20:00:02.000Z",
				"payload": map[string]any{"assignee": "par_echo", "instruction": "x", "connection": conn},
			}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Owner-Token", ownerTokenST(t, base))
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("run_task: %v", err)
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || body.Error.Code != "invalid_command" {
			t.Fatalf("connection=%q 应 400 invalid_command, got %d %+v", conn, resp.StatusCode, body.Error)
		}
		if !containsStr(body.Error.Message, "连接不可达") {
			t.Fatalf("错误面应明示不可达且不回落: %q", body.Error.Message)
		}
	}
}

// TestIdentityAcrossRestart_ST（真机 opt-in）：已启用的真实 CLI 座位在重启后
// 保持同一 ParticipantID（ADR-131 稳定身份落盘）。CI 无真实 CLI → skip。
func TestIdentityAcrossRestart_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()

	pidsOf := func(base string) []string {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/v1/agents")
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			return nil
		}
		var doc struct {
			Agents []struct {
				ParticipantID string `json:"participant_id"`
			} `json:"agents"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		out := []string{}
		for _, a := range doc.Agents {
			if a.ParticipantID != "par_echo" {
				out = append(out, a.ParticipantID)
			}
		}
		return out
	}

	cmd, base, _ := startLoggedServer(t, bin, dataDir)
	// 等扫描出已登录项并启用（真机：codex/minimax 等；CI：无 → skip）
	exeID := ""
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) && exeID == "" {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/v1/harness/executables")
		if err == nil && resp.StatusCode == 200 {
			var doc struct {
				Executables []struct {
					ID    string `json:"id"`
					Login string `json:"login_state"`
				} `json:"executables"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&doc)
			resp.Body.Close()
			for _, e := range doc.Executables {
				if e.Login == "logged_in" {
					exeID = e.ID
					break
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	if exeID == "" {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Skip("未发现已登录的真实 CLI（CI 环境无座位面）——稳定身份真机 ST 为 opt-in")
	}
	t.Logf("真机座位源：%s", exeID)
	enableST(t, base, exeID)

	var before []string
	for time.Now().Before(deadline) && len(before) == 0 {
		before = pidsOf(base)
		time.Sleep(500 * time.Millisecond)
	}
	if len(before) == 0 {
		t.Fatal("启用后 45s 内未见真实座位")
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	cmd2, base2, _ := startLoggedServer(t, bin, dataDir)
	defer func() { _ = cmd2.Process.Kill(); _, _ = cmd2.Process.Wait() }()
	var after []string
	for time.Now().Before(time.Now().Add(45*time.Second)) && len(after) == 0 {
		after = pidsOf(base2)
		time.Sleep(500 * time.Millisecond)
	}
	if len(after) != len(before) {
		t.Fatalf("重启后座位数变化: %v → %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("稳定身份跨重启漂移: %v → %v", before, after)
		}
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOfStr(s, sub) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
