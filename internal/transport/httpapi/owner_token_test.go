// UT 层：owner token 写门（M2，四轮复审 #15 残留收口）——写端点 401/200、
// bootstrap 端点的跨源读保护、未启用装配的 404 与免认证。
package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

func tokenTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-08-31T13:00:00.000Z" },
		NewID:  func(prefix string) string { mu.Lock(); defer mu.Unlock(); n++; return prefix + "_t" },
		Tenant: "ten_local",
	})
	srv := httptest.NewServer(New(Deps{
		SVC:        svc,
		Reader:     store,
		Hub:        sse.NewHub(),
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: token,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postCreateRoom(ts *httptest.Server, token string) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9301",
		"issued_at":       "2026-08-31T09:00:00.000Z",
		"payload":         map[string]any{"display_name": "token"},
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/rooms", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, _ := http.DefaultClient.Do(req)
	return resp
}

func TestOwnerTokenWriteGate(t *testing.T) {
	const tok = "st_owner_token_0123456789abcdef"
	ts := tokenTestServer(t, tok)

	// 缺 token / 错 token → 401 owner_token_required
	for _, bad := range []string{"", "wrong-token"} {
		resp := postCreateRoom(ts, bad)
		var out struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || out.Error.Code != "owner_token_required" {
			t.Fatalf("token=%q: %d %s", bad, resp.StatusCode, out.Error.Code)
		}
	}

	// 正确 token → 200
	resp := postCreateRoom(ts, tok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确 token 应放行，got %d", resp.StatusCode)
	}
}

func TestOwnerBootstrapEndpoint(t *testing.T) {
	const tok = "st_owner_token fedcba9876543210"
	ts := tokenTestServer(t, tok)

	// 无 Origin（同源 GET / 本机客户端）→ 返回 token
	resp, _ := http.Get(ts.URL + "/v1/owner/bootstrap")
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out.Token != tok {
		t.Fatalf("bootstrap 无 Origin 应返回 token：%d %q", resp.StatusCode, out.Token)
	}

	// 跨源 Origin（rebinding 后同源化的攻击者域名）→ 403 拒读
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/owner/bootstrap", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("跨源读 bootstrap 应 403，got %d", resp2.StatusCode)
	}
}

func TestOwnerTokenDisabledAssembly(t *testing.T) {
	ts := tokenTestServer(t, "")
	// 未启用：写免认证、bootstrap 404
	resp := postCreateRoom(ts, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未启用 token 的装配写应免认证，got %d", resp.StatusCode)
	}
	resp2, _ := http.Get(ts.URL + "/v1/owner/bootstrap")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("未启用 token 的装配 bootstrap 应 404，got %d", resp2.StatusCode)
	}
}
