// 共享 HTTP 辅助（provider 通用）：title 尽力拉取、截断。
package extdocs

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	resolveTimeout = 10 * time.Second
	titleMaxRunes  = 200
	pageCap        = 1 << 20 // 页面拉取上限 1MiB（monitor.FetchURL 同量级）
)

// FetchTitle 尽力而为的页面标题拉取：无凭据 GET（公开页面），重定向仅同 host
// 跟随（≤3 跳，出 host 即断）、10s 超时、1MiB 上限。do 为可注入的传输端口
// （nil = 自建 client）；失败返回空串（卡片降级显示 URL）。UA 固定标识来源。
func FetchTitle(do func(*http.Request) (*http.Response, error), rawURL string) string {
	client := &http.Client{
		Timeout: resolveTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return http.ErrUseLastResponse
			}
			// 同 host 判定在 provider 侧（后缀匹配）；此处放宽为同注册域后缀由
			// 调用方确保——统一以初始 URL 与目标 URL 的 host 相同为界。
			if len(via) > 0 && req.URL.Hostname() != via[0].URL.Hostname() {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	if do == nil {
		do = client.Do
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mosaic-extdocs-resolver/1.0")
	resp, err := do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, pageCap))
	if err != nil {
		return ""
	}
	return ExtractTitle(raw)
}

var titleTagPattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// ogTitlePattern SPA 页面 og:title meta（飞书文档页标题常在此而非 <title>）。
var ogTitlePattern = regexp.MustCompile(`(?is)<meta[^>]+property=["']og:title["'][^>]+content=["']([^"']+)["']`)

// ExtractTitle og:title 优先（SPA 页面 <title> 常为通用占位——飞书/腾讯文档
// 页面的文档标题在 og:title meta）+ <title> 兜底；实体解码 + 空白归一 + 截断。
func ExtractTitle(raw []byte) string {
	m := ogTitlePattern.FindSubmatch(raw)
	if m == nil {
		m = titleTagPattern.FindSubmatch(raw)
	}
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	r := []rune(t)
	if len(r) > titleMaxRunes {
		r = r[:titleMaxRunes]
	}
	return string(r)
}

// truncate 字符串截断（错误摘要用；rune 安全）。
func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// parseURL URL 解析辅助（doc_type 推断用；错误返回 err）。
func parseURL(raw string) (*url.URL, error) {
	return url.Parse(strings.TrimSpace(raw))
}
