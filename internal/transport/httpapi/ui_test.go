// UT 层：UI 服务面——SPA 静态产物 + 前端路由回退 + /v1 命名空间不被吞 +
// 开发者模式注入。UI 未装配时回退 M1 最小 webui（测试装配形态）。
package httpapi

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

func uiTestServer(t *testing.T, ui fs.FS, dev bool) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := room.NewService(room.Config{
		Store: store,
		Clock: func() string { return "2026-08-31T11:00:00.000Z" },
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return prefix + "_ui_" + strings.Repeat("0", 4)
		},
		Tenant: "ten_local",
	})
	handler := New(Deps{
		SVC:    svc,
		Reader: store,
		Hub:    sse.NewHub(),
		Actor:  room.Actor{ParticipantID: "par_owner", Kind: "human"},
		UI:     ui,
		Dev:    dev,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestUISpaServedAndFallback(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":    {Data: []byte(`<html><body>const MOSAIC_DEV = false;</body></html>`)},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	ts := uiTestServer(t, ui, false)

	// 根路径：SPA index
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "MOSAIC_DEV") {
		t.Fatalf("GET / 应返回 SPA index：%d %q", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index 必须 no-cache（WebView2 持久缓存实证会跨进程留旧 UI），got %q", cc)
	}

	// 静态资产直出
	resp2, _ := http.Get(ts.URL + "/assets/app.js")
	if resp2.StatusCode != 200 {
		t.Fatalf("GET /assets/app.js = %d", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("资产 Content-Type = %q", ct)
	}
	if cc := resp2.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("内容哈希资产应 immutable 长缓存，got %q", cc)
	}
	resp2.Body.Close()

	// 前端路由回退：未知非 /v1 路径回 index.html
	resp3, _ := http.Get(ts.URL + "/some/client/route")
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 200 || !strings.Contains(string(body3), "MOSAIC_DEV") {
		t.Fatalf("前端路由应回退 index.html：%d", resp3.StatusCode)
	}
	if cc := resp3.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("回退 index 必须 no-cache，got %q", cc)
	}

	// /v1 命名空间不被回退吞掉
	resp4, _ := http.Get(ts.URL + "/v1/nonexistent")
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/nonexistent = %d（API 命名空间不得被 SPA 回退吞掉）", resp4.StatusCode)
	}
}

func TestUIDevInjection(t *testing.T) {
	ui := fstest.MapFS{
		"index.html": {Data: []byte(`<html><script>const MOSAIC_DEV = false;</script></html>`)},
	}
	ts := uiTestServer(t, ui, true)
	resp, _ := http.Get(ts.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "const MOSAIC_DEV = true;") {
		t.Fatal("-dev 时 SPA index 应注入 MOSAIC_DEV=true")
	}
}

func TestUILegacyFallbackWhenNotAssembled(t *testing.T) {
	ts := uiTestServer(t, nil, false)
	resp, _ := http.Get(ts.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "const MOSAIC_DEV") {
		t.Fatalf("UI 未装配时应回退 M1 内嵌 webui，got: %.200s status=%d", body, resp.StatusCode)
	}
}
