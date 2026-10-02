//go:build darwin

package main

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"testing"
)

// macOS 契约：WKWebView 不跟随自定义 scheme 资产桥上的 307（会直接渲染重定向
// 响应体），故资产桥落地 200 跳转页——脚本 location.replace 为主，meta refresh
// 与锚点兜底，三路均指向回环源 + 原始路径与查询串。
func TestAssetServerServesJumpPageToLoopbackOrigin(t *testing.T) {
	const base = "http://127.0.0.1:39871"
	for _, tc := range assetBridgeCases(base) {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveViaAssetBridge(t, base, tc.uri)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("Content-Type = %q, want text/html", ct)
			}
			body := recorder.Body.String()
			wantJS, err := json.Marshal(tc.want)
			if err != nil {
				t.Fatalf("marshal target: %v", err)
			}
			if !strings.Contains(body, "location.replace("+string(wantJS)+")") {
				t.Fatalf("jump page missing location.replace(%s)", wantJS)
			}
			wantHTML := html.EscapeString(tc.want)
			if !strings.Contains(body, "url="+wantHTML) {
				t.Fatalf("jump page missing meta refresh fallback for %q", wantHTML)
			}
			if !strings.Contains(body, `href="`+wantHTML+`"`) {
				t.Fatalf("jump page missing anchor fallback for %q", wantHTML)
			}
		})
	}
}
