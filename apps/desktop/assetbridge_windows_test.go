//go:build windows

package main

import (
	"net/http"
	"testing"
)

// Windows 契约（保持 dogfood 验证过的原路径，字节不变）：资产桥上一切请求
// 307 到回环源——WebView2 的 WebResourceRequested 桥会缓冲/改写响应（SSE
// 过不去），自动跟随 307 后首次导航即落到真实 HTTP 源，SPA/API/SSE 与
// web 形态同源同行为。
func TestAssetServerRedirectsToLoopbackOrigin(t *testing.T) {
	const base = "http://127.0.0.1:39871"
	for _, tc := range assetBridgeCases(base) {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveViaAssetBridge(t, base, tc.uri)
			if recorder.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTemporaryRedirect)
			}
			if got := recorder.Header().Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}
