// 腾讯文档 provider（RFC-0015 → RFC-0016 平移）：官方 MCP 端点无状态 JSON-RPC
// 薄客户端。凭据 = 个人 token（官方授权页 docs.qq.com/scenario/open-claw.html
// 「获取/重置 token」——老地址 open/auth/mcp.html 现跳转至此）；Authorization
// 裸值先行、401 按 Bearer 前缀重试一次（官方配置示例为裸值、端点文案提示
// Bearer——两种形态兼容不赌注）。
package extdocs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// 腾讯上游常量。
const (
	tencentBaseURL = "https://docs.qq.com/openapi/mcp"
	tencentHost    = "docs.qq.com"
)

// tencentCreds 腾讯凭据（DataDir/extdocs/tencent_docs.json）。
type tencentCreds struct {
	Token          string `json:"token,omitempty"`
	LastVerifiedAt string `json:"last_verified_at,omitempty"`
}

// TencentProvider 腾讯文档实现。HTTPDo 端口供 UT fake。
type TencentProvider struct {
	mgr     *Manager
	BaseURL string
	HTTPDo  func(*http.Request) (*http.Response, error)
	Logger  *slog.Logger
}

// NewTencentProvider 构造（BaseURL 空 = 官方端点）。
func NewTencentProvider(mgr *Manager, baseURL string) *TencentProvider {
	if baseURL == "" {
		baseURL = tencentBaseURL
	}
	return &TencentProvider{mgr: mgr, BaseURL: baseURL, Logger: slog.Default()}
}

func (p *TencentProvider) ID() string                   { return ProviderTencent }
func (p *TencentProvider) DisplayName() string          { return "腾讯文档" }
func (p *TencentProvider) RefKind() string              { return "txdoc" }
func (p *TencentProvider) HostMatches(host string) bool { return host == tencentHost }

// CredentialStatus 凭据状态（设置页）。
type TencentStatus struct {
	Configured     bool   `json:"configured"`
	LastVerifiedAt string `json:"last_verified_at,omitempty"`
}

func (p *TencentProvider) Status() TencentStatus {
	var c tencentCreds
	_ = p.mgr.Load(ProviderTencent, &c)
	return TencentStatus{Configured: c.Token != "", LastVerifiedAt: c.LastVerifiedAt}
}

// UpdateToken 写入/清除 token（空串 = 清除）。
func (p *TencentProvider) UpdateToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return p.mgr.Clear(ProviderTencent)
	}
	var c tencentCreds
	_ = p.mgr.Load(ProviderTencent, &c)
	c.Token = token
	return p.mgr.Save(ProviderTencent, c)
}

// ---- 上游 MCP 客户端 ----

const upstreamTimeout = 30 * time.Second
const responseCap = 1 << 20 // 1MiB

type jsonRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

type toolsCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

type toolsListResult struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// call 上游 JSON-RPC 调用（401 时按 Bearer 前缀形态重试一次）。
func (p *TencentProvider) call(ctx context.Context, method string, params map[string]any) (jsonRPCResponse, error) {
	return p.callCap(ctx, method, params, responseCap)
}

// callCap 同 call，响应体上限由调用方指定（表格区域读单次可达数 MB）。
func (p *TencentProvider) callCap(ctx context.Context, method string, params map[string]any, respCap int64) (jsonRPCResponse, error) {
	var c tencentCreds
	_ = p.mgr.Load(ProviderTencent, &c)
	if c.Token == "" {
		return jsonRPCResponse{}, ErrTokenMissing
	}
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return jsonRPCResponse{}, fmt.Errorf("extdocs: marshal: %w", err)
	}
	var resp jsonRPCResponse
	var lastErr error
	for _, form := range []string{c.Token, "Bearer " + c.Token} {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL, bytes.NewReader(body))
		if rerr != nil {
			return jsonRPCResponse{}, fmt.Errorf("extdocs: request: %w", rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", form)
		raw, rerr := p.doCap(req, respCap)
		if rerr != nil {
			return jsonRPCResponse{}, &UpstreamError{Code: "unknown", Message: truncate(rerr.Error(), 300)}
		}
		perr := decodeJSONRPC(raw, &resp)
		if perr == nil {
			return resp, nil
		}
		lastErr = perr
		// 401 且首形态（裸值）被拒 → Bearer 形态重试；其余错误不重试
		if raw.status != http.StatusUnauthorized || form == "Bearer "+c.Token {
			return jsonRPCResponse{}, perr
		}
	}
	return jsonRPCResponse{}, lastErr
}

type rawResponse struct {
	status int
	body   []byte
}

func (p *TencentProvider) do(req *http.Request) (rawResponse, error) {
	return p.doCap(req, responseCap)
}

// doCap 同 do，响应体上限由调用方指定（表格区域读单次可达数 MB）。
func (p *TencentProvider) doCap(req *http.Request, respCap int64) (rawResponse, error) {
	do := p.HTTPDo
	if do == nil {
		do = (&http.Client{Timeout: upstreamTimeout}).Do
	}
	resp, err := do(req)
	if err != nil {
		return rawResponse{}, err
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		return rawResponse{status: resp.StatusCode, body: readSSEPayload(resp.Body, respCap)}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, respCap))
	if err != nil {
		return rawResponse{}, err
	}
	return rawResponse{status: resp.StatusCode, body: raw}, nil
}

// decodeJSONRPC HTTP 状态 + JSON-RPC 错误 → 域错误（上游业务错误码透传）。
func decodeJSONRPC(raw rawResponse, out *jsonRPCResponse) error {
	if raw.status < 200 || raw.status >= 300 {
		var restErr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(raw.body, &restErr)
		code := restErr.Error
		if code == "" {
			code = fmt.Sprintf("http_%d", raw.status)
		}
		msg := restErr.ErrorDescription
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", raw.status)
		}
		return &UpstreamError{Code: code, Message: truncate(msg, 300)}
	}
	if err := json.Unmarshal(raw.body, out); err != nil {
		return &UpstreamError{Code: "unknown", Message: "响应不是合法 JSON-RPC: " + truncate(err.Error(), 200)}
	}
	if out.Error != nil {
		return &UpstreamError{Code: fmt.Sprint(out.Error.Code), Message: truncate(out.Error.Message, 300)}
	}
	return nil
}

// readSSEPayload 增量读取 SSE，返回最后一条 data 载荷（有界）。
func readSSEPayload(r io.Reader, capBytes int64) []byte {
	scanner := bufio.NewScanner(io.LimitReader(r, capBytes))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var last []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				last = append([]byte(nil), payload...)
			}
		}
	}
	return last
}

// Verify 上游探活（tools/list）并记录验证水位。
func (p *TencentProvider) Verify(ctx context.Context) error {
	resp, err := p.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var list toolsListResult
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		return &UpstreamError{Code: "unknown", Message: "tools/list 结果不可解析: " + truncate(err.Error(), 200)}
	}
	var c tencentCreds
	_ = p.mgr.Load(ProviderTencent, &c)
	c.LastVerifiedAt = time.Now().Format(time.RFC3339)
	_ = p.mgr.Save(ProviderTencent, c)
	return nil
}

// ShareMarkdown create_smartcanvas_by_markdown 创建智能文档（官方推荐首选
// ——markdown 语义映射最全），从结果文本中提取远端 URL。
func (p *TencentProvider) ShareMarkdown(ctx context.Context, title, markdown string) (string, error) {
	if strings.TrimSpace(title) == "" {
		title = "Mosaic 文档"
	}
	resp, err := p.call(ctx, "tools/call", map[string]any{
		"name": "create_smartcanvas_by_markdown",
		"arguments": map[string]any{
			"title":    title,
			"markdown": markdown,
		},
	})
	if err != nil {
		return "", err
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return "", &UpstreamError{Code: "unknown", Message: "创建结果不可解析: " + truncate(err.Error(), 200)}
	}
	var sb strings.Builder
	for _, c := range res.Content {
		sb.WriteString(c.Text)
		sb.WriteString("\n")
	}
	if res.IsError {
		return "", &UpstreamError{Code: "unknown", Message: "创建失败: " + truncate(sb.String(), 300)}
	}
	u := tencentURLPattern.FindString(sb.String())
	if u == "" {
		return "", &UpstreamError{Code: "unknown", Message: "创建成功但结果未含文档 URL: " + truncate(sb.String(), 200)}
	}
	return u, nil
}

var tencentURLPattern = regexp.MustCompile(`https://docs\.qq\.com/[A-Za-z0-9/_?&=%.\-]+`)

// ResolveLink 链接 → 描述子：doc_type 路径推断 + title 尽力拉取。
func (p *TencentProvider) ResolveLink(ctx context.Context, rawURL string) (LinkInfo, error) {
	info := LinkInfo{URL: rawURL, DocType: tencentDocType(rawURL)}
	info.Title = FetchTitle(p.HTTPDo, rawURL)
	return info, nil
}

// tencentDocType URL 路径前缀 → 文档类型（未知 = 空串）。
func tencentDocType(rawURL string) string {
	u, err := parseURL(rawURL)
	if err != nil {
		return ""
	}
	switch {
	case strings.HasPrefix(u.Path, "/sheet/"):
		return "sheet"
	case strings.HasPrefix(u.Path, "/doc/"):
		return "doc"
	case strings.HasPrefix(u.Path, "/slide/"):
		return "slide"
	case strings.HasPrefix(u.Path, "/aio/"):
		return "aio"
	case strings.HasPrefix(u.Path, "/form/"):
		return "form"
	default:
		return ""
	}
}
