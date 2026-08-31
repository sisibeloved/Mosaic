//go:build st

// ST 层：owner token 写门（M2，四轮复审 #15 残留收口）——真实二进制：
// 裸写 401 → bootstrap 取凭据（无 Origin 可达）→ 携凭据写放行；
// 跨源读 bootstrap 403（rebinding 页面拿不到凭据）。
package main_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestOwnerTokenGate_ST(t *testing.T) {
	bin := buildServer(t)
	dataDir := t.TempDir()
	cmd, base, _ := startLoggedServer(t, bin, dataDir)
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	client := &http.Client{Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9401",
		"issued_at":       "2026-08-31T09:00:00.000Z",
		"payload":         map[string]any{"display_name": "token st"},
	})
	post := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/rooms", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Owner-Token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 1) 裸写（无凭据）→ 401
	if code := post(""); code != http.StatusUnauthorized {
		t.Fatalf("裸写应 401，got %d", code)
	}

	// 2) 跨源读 bootstrap → 403（rebinding 攻击者域名）
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/owner/bootstrap", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, _ := client.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨源读 bootstrap 应 403，got %d", resp.StatusCode)
	}

	// 3) 本机取凭据 → 携凭据写放行
	tok := ownerTokenST(t, base)
	if code := post(tok); code != http.StatusOK {
		t.Fatalf("携凭据写应放行，got %d", code)
	}

	// 4) token 跨进程稳定：重启同数据目录，凭据不变（会话连续性）
	cmd2, base2, _ := startLoggedServer(t, bin, dataDir)
	defer func() { _ = cmd2.Process.Kill(); _, _ = cmd2.Process.Wait() }()
	if tok2 := ownerTokenST(t, base2); tok2 != tok {
		t.Fatalf("同数据目录重启后 token 漂移：%q vs %q", tok, tok2)
	}
}
