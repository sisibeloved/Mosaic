// 桌面壳集成面（非对外契约，手工注册——与 /v1/debug 同类别，见 server.go New 注释）：
// WebView（WKWebView/WebView2）不处理 window.open——SPA 的外链兜底经此请壳调
// 系统浏览器（飞书 OAuth 授权页等）。仅 http/https；写端点三层门全量适用。
package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// handleShellOpenExternal POST /v1/shell/open-external {"url": "..."}。
// 路由仅当 Deps.ShellOpenURL 非空时注册（服务端形态无此面）。
func (s *server) handleShellOpenExternal(w http.ResponseWriter, r *http.Request) {
	if !s.guardWrite(w, r, true) {
		return
	}
	if s.deps.ShellOpenURL == nil {
		writeError(w, http.StatusNotFound, "shell_open_disabled", "本装配无桌面壳")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体 JSON 不合法："+err.Error())
		return
	}
	// URL 纪律：仅 http/https、非空 host、无空白/控制字符（壳侧按单参数传给
	// 系统打开器，不含 shell——此处校验是纵深而非唯一防线）。
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		strings.ContainsAny(u.String(), " \t\r\n") {
		writeError(w, http.StatusBadRequest, "invalid_url", "仅支持 http/https URL")
		return
	}
	if err := s.deps.ShellOpenURL(u.String()); err != nil {
		writeError(w, http.StatusInternalServerError, "shell_open_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": true})
}
