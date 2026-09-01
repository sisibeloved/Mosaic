//go:build windows || darwin

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	wailsassetserver "github.com/wailsapp/wails/v2/pkg/assetserver"
)

type testLogger struct{}

func (testLogger) Debug(string, ...interface{}) {}
func (testLogger) Error(string, ...interface{}) {}

// 壳传输契约：资产桥上一切请求必须 307 到回环源（保留原始路径与查询串）——
// WebView2 的 WebResourceRequested 桥不支持流式（SSE 过不去），首次导航即
// 落到真实 HTTP 源，SPA/API/SSE 与 web 形态同源同行为。
func TestAssetServerRedirectsToLoopbackOrigin(t *testing.T) {
	const base = "http://127.0.0.1:39871"
	options := newAssetServerOptions(base)
	if options.Assets != nil {
		t.Fatal("desktop assets must be nil so Wails forwards every request to the redirect handler")
	}

	assetHandler, err := wailsassetserver.NewAssetHandler(*options, testLogger{})
	if err != nil {
		t.Fatalf("create Wails asset handler: %v", err)
	}

	cases := []struct {
		name string
		uri  string
		want string
	}{
		{"root", "http://wails.localhost/", base + "/"},
		{"spa route", "http://wails.localhost/rooms/room_1", base + "/rooms/room_1"},
		{"api with query", "http://wails.localhost/v1/rooms/room_1/events?cursor=cur_9", base + "/v1/rooms/room_1/events?cursor=cur_9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			assetHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.uri, nil))
			if recorder.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTemporaryRedirect)
			}
			if got := recorder.Header().Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}
