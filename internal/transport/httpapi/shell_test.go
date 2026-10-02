// UT 层：httpapi 桌面壳集成面——POST /v1/shell/open-external 仅壳装配注册，
// 写端点三层门全量适用，URL 纪律仅 http/https。
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newShellTestServer(t *testing.T, hook func(string) error) *httptest.Server {
	t.Helper()
	return httptest.NewServer(New(Deps{ShellOpenURL: hook}))
}

// 未注入 ShellOpenURL（服务端形态）：端点不注册。
func TestShellOpenExternalRouteAbsentWithoutHook(t *testing.T) {
	ts := newShellTestServer(t, nil)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/shell/open-external", "application/json",
		strings.NewReader(`{"url":"https://open.feishu.cn/"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 404/405（路由不注册）", resp.StatusCode)
	}
}

func TestShellOpenExternal(t *testing.T) {
	var opened []string
	ts := newShellTestServer(t, func(u string) error {
		opened = append(opened, u)
		return nil
	})
	defer ts.Close()

	post := func(body string) int {
		resp, err := http.Post(ts.URL+"/v1/shell/open-external", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(`{"url":"https://open.feishu.cn/a?b=1"}`); code != http.StatusOK {
		t.Fatalf("合法 URL status = %d, want 200", code)
	}
	if len(opened) != 1 || opened[0] != "https://open.feishu.cn/a?b=1" {
		t.Fatalf("hook opened = %v, want 精确一次且保留查询串", opened)
	}
	for name, body := range map[string]string{
		"非 http scheme": `{"url":"file:///etc/passwd"}`,
		"自定义 scheme":    `{"url":"wails://wails/"}`,
		"无 host":        `{"url":"https://"}`,
		"未知字段":          `{"url":"https://a.example/","x":1}`,
		"非 JSON":        `not-json`,
	} {
		if code := post(body); code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", name, code)
		}
	}
	if len(opened) != 1 {
		t.Fatalf("非法请求后 hook 仍被调用：opened = %v", opened)
	}
	// 路径空白经 url.String() 规范化为 %20——传给壳的是已转义 URL（安全面收口）。
	if code := post(`{"url":"https://a.example/x y"}`); code != http.StatusOK {
		t.Fatalf("可规范化 URL status = %d, want 200", code)
	}
	if opened[len(opened)-1] != "https://a.example/x%20y" {
		t.Fatalf("hook opened = %q, want 规范化转义后的 URL", opened[len(opened)-1])
	}
}

// 写端点三层门：非 JSON Content-Type 415；OwnerToken 装配下缺凭据 401。
func TestShellOpenExternalWriteGate(t *testing.T) {
	ts := newShellTestServer(t, func(string) error { return nil })
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/shell/open-external",
		strings.NewReader(`{"url":"https://a.example/"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain status = %d, want 415", resp.StatusCode)
	}

	ts2 := httptest.NewServer(New(Deps{ShellOpenURL: func(string) error { return nil }, OwnerToken: "tok_shell"}))
	defer ts2.Close()
	resp2, err := http.Post(ts2.URL+"/v1/shell/open-external", "application/json",
		strings.NewReader(`{"url":"https://a.example/"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺 owner token status = %d, want 401", resp2.StatusCode)
	}
}
