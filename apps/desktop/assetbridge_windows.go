//go:build windows

package main

import "net/http"

// redirectToLoopback 维持 dogfood 验证过的原路径：WebView2 的
// WebResourceRequested 桥自动跟随 307，首次导航即落到回环源。
func redirectToLoopback(w http.ResponseWriter, r *http.Request, target string) {
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}
