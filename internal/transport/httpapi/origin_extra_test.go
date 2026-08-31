// UT 层：壳集成信任源（ExtraOriginHosts，M2 桌面壳）——wails.localhost 精确放行，
// 未配置即拒（仍是跨源写门纪律）；回环路径不受影响。
package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

func originTestServer(t *testing.T, extra []string) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-08-31T12:00:00.000Z" },
		NewID:  func(prefix string) string { mu.Lock(); defer mu.Unlock(); n++; return prefix + "_o" },
		Tenant: "ten_local",
	})
	srv := httptest.NewServer(New(Deps{
		SVC:              svc,
		Reader:           store,
		Hub:              sse.NewHub(),
		Actor:            room.Actor{ParticipantID: "par_owner", Kind: "human"},
		ExtraOriginHosts: extra,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postWithOrigin(ts *httptest.Server, origin string) int {
	body := []byte(`{"command_kind":"create_room","expected_room_version":0,` +
		`"idempotency_key":"018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9201",` +
		`"issued_at":"2026-08-31T09:00:00.000Z","payload":{"display_name":"origin"}}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/rooms", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestExtraOriginHostsGate(t *testing.T) {
	// 未配置：wails.localhost 是非回环 host → 拒（403）
	ts := originTestServer(t, nil)
	if code := postWithOrigin(ts, "http://wails.localhost"); code != http.StatusForbidden {
		t.Fatalf("未配置信任源时 wails.localhost 写应 403，got %d", code)
	}

	// 配置后：精确放行（wails:// scheme 形态同样命中——按 hostname 比对）
	ts2 := originTestServer(t, []string{"wails.localhost"})
	if code := postWithOrigin(ts2, "http://wails.localhost"); code != http.StatusOK {
		t.Fatalf("配置信任源后 wails.localhost 写应放行，got %d", code)
	}

	// 配置不能放宽其他源（子域/别名都不匹配）
	if code := postWithOrigin(ts2, "http://evil-wails.localhost"); code != http.StatusForbidden {
		t.Fatalf("近似主机名不得放行，got %d", code)
	}
	if code := postWithOrigin(ts2, "http://wails.localhost.evil.example"); code != http.StatusForbidden {
		t.Fatalf("后缀拼接主机名不得放行，got %d", code)
	}

	// 回环同源路径不受影响
	if code := postWithOrigin(ts2, ""); code != http.StatusOK {
		t.Fatalf("无 Origin 的本机客户端不应受信任源配置影响，got %d", code)
	}
}
