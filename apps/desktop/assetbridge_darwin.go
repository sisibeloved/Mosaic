//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
)

// redirectToLoopback：WKWebView 对自定义 scheme 资产桥返回的 307 不自动跟随
// （新版 WebKit 直接把重定向响应体当页面渲染，首屏即 "Temporary Redirect"
// 白页，需手动点链接才落到回环源）。改为落地一枚立即 location.replace 的
// 跳转页——页面自身发起的导航 WKWebView 必然跟随；meta refresh 与锚点仅为
// 无脚本环境的兜底。目标端点与 Windows 路径完全一致，落源后行为不变。
func redirectToLoopback(w http.ResponseWriter, r *http.Request, target string) {
	targetJS, err := json.Marshal(target)
	if err != nil {
		http.Error(w, "redirect target invalid", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0; url=%[1]s">
<title>Mosaic</title>
</head>
<body>
<script>location.replace(%[2]s)</script>
<p>Mosaic 正在启动…若未自动跳转，请<a href="%[1]s">点击这里</a>。</p>
</body>
</html>
`, html.EscapeString(target), targetJS)
}
