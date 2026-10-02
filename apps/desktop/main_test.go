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

// 壳传输契约（平台无关部分）：资产桥不持有静态资产，一切请求（含首次导航）
// 都落到 Handler 分流到回环源。平台响应契约见 assetbridge_*_test.go：
// Windows 307（WebView2 自动跟随）/ macOS 跳转页（WKWebView 不跟随桥上 307）。
func TestAssetServerForwardsEveryRequestToHandler(t *testing.T) {
	options := newAssetServerOptions("http://127.0.0.1:39871")
	if options.Assets != nil {
		t.Fatal("desktop assets must be nil so Wails forwards every request to the redirect handler")
	}
}

// serveViaAssetBridge 模拟资产桥收到 uri 请求，返回录制响应。
func serveViaAssetBridge(t *testing.T, base, uri string) *httptest.ResponseRecorder {
	t.Helper()
	assetHandler, err := wailsassetserver.NewAssetHandler(*newAssetServerOptions(base), testLogger{})
	if err != nil {
		t.Fatalf("create Wails asset handler: %v", err)
	}
	recorder := httptest.NewRecorder()
	assetHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, uri, nil))
	return recorder
}

// assetBridgeCases 覆盖根路径、SPA 路由与带查询串的 API 路径（平台测试共用），
// 期望目标均为回环源 + 原始路径与查询串。
func assetBridgeCases(base string) []struct {
	name string
	uri  string
	want string
} {
	return []struct {
		name string
		uri  string
		want string
	}{
		{"root", "http://wails.localhost/", base + "/"},
		{"spa route", "http://wails.localhost/rooms/room_1", base + "/rooms/room_1"},
		{"api with query", "http://wails.localhost/v1/rooms/room_1/events?cursor=cur_9", base + "/v1/rooms/room_1/events?cursor=cur_9"},
	}
}
