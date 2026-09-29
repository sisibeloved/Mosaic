// 飞书云文档 provider（RFC-0016）：用户身份 OAuth（授权码 + 手动粘 code，
// user_access_token 自动刷新轮换持久化）+ 云文档 markdown 导入三步
// （上传素材 ccm_import_open → import_tasks → 轮询）。文档落在用户自己的
// "我的云文档"根目录。
//
// 一次性配置（用户侧）：open.feishu.cn 创建自建应用，开通云文档相关权限
// （drive:drive + offline_access），安全设置登记重定向 URL（推荐
// http://localhost:3000/callback——官方 lark-mcp 同款）。
package extdocs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 飞书上游常量。
const (
	feishuBase       = "https://open.feishu.cn"
	feishuHostSuffix = ".feishu.cn"
	feishuHostExact  = "feishu.cn"
	// FeishuRedirectURI 推荐登记值（官方 lark-mcp 同款；须与飞书后台安全设置
	// 完全一致——授权后浏览器跳转到该地址，用户从地址栏复制 code）。
	FeishuRedirectURI = "http://localhost:3000/callback"
	// FeishuScopes 授权 scope（空格分隔；drive:drive 覆盖上传+导入+云空间，
	// offline_access 授予 refresh_token，bitable:app:readonly 覆盖多维表格
	// 读取——sheets API 吃 drive scope 但 bitable 只认专属 scope，drive 无效
	//（真机 99991679 实证）。设置页展示"需在后台开通的权限"。
	FeishuScopes     = "offline_access drive:drive bitable:app:readonly"
	feishuImportPoll = 500 * time.Millisecond
	feishuImportMax  = 60 * time.Second
	// feishuState 授权 state（回调须原样带回，作最小校验）。
	feishuState = "mosaic"
	// feishuCallbackIdle 回环监听空闲上限：超时未落回调即收摊（重新发起
	// "登录飞书"会再挂）。
	feishuCallbackIdle = 10 * time.Minute
)

// feishuCreds 飞书凭据（DataDir/extdocs/feishu_docs.json；secret 持久但不回传）。
type feishuCreds struct {
	AppID          string    `json:"app_id,omitempty"`
	AppSecret      string    `json:"app_secret,omitempty"`
	AccessToken    string    `json:"access_token,omitempty"`
	RefreshToken   string    `json:"refresh_token,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	LastVerifiedAt string    `json:"last_verified_at,omitempty"`
}

// FeishuProvider 飞书实现。HTTPDo 端口供 UT fake。
type FeishuProvider struct {
	mgr     *Manager
	BaseURL string
	// RedirectURI 回调登记值覆盖（缺省 FeishuRedirectURI；UT 用假端口驱动
	// 回环监听，生产恒缺省）。
	RedirectURI string
	HTTPDo      func(*http.Request) (*http.Response, error)
	Logger      *slog.Logger

	refreshMu sync.Mutex // token 刷新串行（轮换语义下并发刷新会互相作废）
	cbMu      sync.Mutex
	cbLn      net.Listener // 回环回调监听（授权窗口期持有）
}

// NewFeishuProvider 构造（BaseURL 空 = 官方端点）。
func NewFeishuProvider(mgr *Manager, baseURL string) *FeishuProvider {
	if baseURL == "" {
		baseURL = feishuBase
	}
	return &FeishuProvider{mgr: mgr, BaseURL: baseURL, Logger: slog.Default()}
}

func (p *FeishuProvider) ID() string          { return ProviderFeishu }
func (p *FeishuProvider) DisplayName() string { return "飞书云文档" }
func (p *FeishuProvider) RefKind() string     { return "fdoc" }
func (p *FeishuProvider) HostMatches(host string) bool {
	return host == feishuHostExact || strings.HasSuffix(host, feishuHostSuffix)
}

// FeishuStatus 凭据状态（设置页）。
type FeishuStatus struct {
	AppConfigured  bool      `json:"app_configured"`
	Authorized     bool      `json:"authorized"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	LastVerifiedAt string    `json:"last_verified_at,omitempty"`
}

func (p *FeishuProvider) Status() FeishuStatus {
	var c feishuCreds
	_ = p.mgr.Load(ProviderFeishu, &c)
	return FeishuStatus{
		AppConfigured:  c.AppID != "" && c.AppSecret != "",
		Authorized:     c.AccessToken != "" || c.RefreshToken != "",
		ExpiresAt:      c.ExpiresAt,
		LastVerifiedAt: c.LastVerifiedAt,
	}
}

// UpdateApp 写入/清除应用凭据（app_id 与 app_secret 须同时给；空 = 清除全部）。
func (p *FeishuProvider) UpdateApp(appID, appSecret string) error {
	appID, appSecret = strings.TrimSpace(appID), strings.TrimSpace(appSecret)
	if appID == "" && appSecret == "" {
		return p.mgr.Clear(ProviderFeishu)
	}
	if appID == "" || appSecret == "" {
		return errors.New("extdocs: app_id 与 app_secret 须同时提供")
	}
	var c feishuCreds
	_ = p.mgr.Load(ProviderFeishu, &c)
	c.AppID, c.AppSecret = appID, appSecret
	return p.mgr.Save(ProviderFeishu, c)
}

// redirectURI 当前生效的回调登记值（授权 URL / token 交换 / 回环监听三处共用）。
func (p *FeishuProvider) redirectURI() string {
	if p.RedirectURI != "" {
		return p.RedirectURI
	}
	return FeishuRedirectURI
}

// RedirectURIValue 当前生效的回调登记值（设置页展示/authorize 响应）。
func (p *FeishuProvider) RedirectURIValue() string { return p.redirectURI() }

// AuthorizeURL 授权页 URL（设置页"登录飞书"打开；redirect_uri 须与后台登记一致）。
func (p *FeishuProvider) AuthorizeURL() string {
	var c feishuCreds
	_ = p.mgr.Load(ProviderFeishu, &c)
	q := url.Values{}
	q.Set("app_id", c.AppID)
	q.Set("redirect_uri", p.redirectURI())
	q.Set("scope", FeishuScopes)
	q.Set("state", feishuState)
	return p.BaseURL + "/open-apis/authen/v1/authorize?" + q.Encode()
}

// ArmCallback 挂起回环回调监听（自动绑定）：按登记的 redirect_uri 绑定回环
// 端口，授权页跳转落回时直接换 token 落盘，浏览器收到结果页——全程免手动
// 复制 code。端口被占（典型：开发态 Vite 占 3000）返回 false，设置页降级
// 手动粘 code。重复调用先撤旧监听；首次回调或空闲超时自动收摊。
func (p *FeishuProvider) ArmCallback() bool {
	u, err := url.Parse(p.redirectURI())
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Path == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	p.CloseCallback() // 同端口重挂：先撤旧监听再绑
	ln, err := net.Listen("tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		p.logf("feishu callback port busy, manual fallback", "err", err.Error())
		return false
	}
	p.cbMu.Lock()
	p.cbLn = ln
	p.cbMu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
		defer p.dropCallback(ln)
		q := r.URL.Query()
		code := strings.TrimSpace(q.Get("code"))
		if code == "" || q.Get("state") != feishuState {
			p.logf("feishu callback rejected", "has_code", code != "", "state", q.Get("state"))
			writeCallbackPage(w, false, "回调缺少 code 或 state 不符——请回到 Mosaic 重新点「登录飞书」")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.ExchangeCode(ctx, code); err != nil {
			p.logf("feishu callback exchange failed", "err", err.Error())
			writeCallbackPage(w, false, "授权码换取失败："+err.Error())
			return
		}
		p.logf("feishu authorized via loopback callback")
		writeCallbackPage(w, true, "飞书授权成功——回到 Mosaic 设置页，绑定已自动完成")
	})
	go func() { _ = http.Serve(ln, mux) }()
	go func() {
		t := time.NewTimer(feishuCallbackIdle)
		defer t.Stop()
		<-t.C
		p.dropCallback(ln)
	}()
	return true
}

// CloseCallback 撤下当前回调监听（重复授权/收摊）。
func (p *FeishuProvider) CloseCallback() {
	p.cbMu.Lock()
	defer p.cbMu.Unlock()
	if p.cbLn != nil {
		_ = p.cbLn.Close()
		p.cbLn = nil
	}
}

// dropCallback 仅当 ln 仍是当前挂起的监听时撤下（防旧回调误关新监听）。
func (p *FeishuProvider) dropCallback(ln net.Listener) {
	p.cbMu.Lock()
	defer p.cbMu.Unlock()
	if p.cbLn == ln {
		_ = ln.Close()
		p.cbLn = nil
	}
}

// writeCallbackPage 授权落点最小 HTML（提示后脚本自关窗口，失败时停留展示原因）。
func writeCallbackPage(w http.ResponseWriter, ok bool, msg string) {
	status, icon := http.StatusOK, "✅"
	if !ok {
		status, icon = http.StatusBadRequest, "⚠️"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Mosaic 飞书授权</title></head><body style="font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0"><div style="text-align:center"><div style="font-size:48px">`+icon+`</div><p style="color:#333">`+html.EscapeString(msg)+`</p><script>setTimeout(function(){window.close()},1500)</script></div></body></html>`)
}

func (p *FeishuProvider) logf(msg string, args ...any) {
	if p.Logger != nil {
		p.Logger.Info("extdocs: "+msg, args...)
	}
}

// ExchangeCode 授权码换取 token（设置页粘 code 路径）；持久化并按轮换语义保存。
func (p *FeishuProvider) ExchangeCode(ctx context.Context, code string) error {
	var c feishuCreds
	_ = p.mgr.Load(ProviderFeishu, &c)
	if c.AppID == "" || c.AppSecret == "" {
		return ErrTokenMissing
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.AppID},
		"client_secret": {c.AppSecret},
		"code":          {strings.TrimSpace(code)},
		"redirect_uri":  {p.redirectURI()},
	}
	var tok oauthTokenResponse
	if err := p.postForm(ctx, "/open-apis/authen/v2/oauth/token", form, &tok); err != nil {
		return err
	}
	if tok.AccessToken == "" {
		return &UpstreamError{Code: fmt.Sprint(tok.Code), Message: truncate(tok.Msg, 300)}
	}
	c.AccessToken = tok.AccessToken
	c.RefreshToken = tok.RefreshToken
	c.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return p.mgr.Save(ProviderFeishu, c)
}

type oauthTokenResponse struct {
	Code             int    `json:"code"`
	Msg              string `json:"msg"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
}

// ensureFresh 有效 token 获取（串行刷新；过期前 5 分钟提前刷新；刷新失败 =
// 需重新授权）。未配置 → ErrTokenMissing；未授权/刷新失效 → ErrReauthRequired。
func (p *FeishuProvider) ensureFresh(ctx context.Context) (string, error) {
	var c feishuCreds
	_ = p.mgr.Load(ProviderFeishu, &c)
	if c.AppID == "" || c.AppSecret == "" {
		return "", ErrTokenMissing
	}
	if c.AccessToken != "" && time.Now().Before(c.ExpiresAt.Add(-5*time.Minute)) {
		return c.AccessToken, nil
	}
	if c.RefreshToken == "" {
		return "", ErrReauthRequired
	}
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	// 双检：拿锁期间他者可能已刷新（磁盘重读）
	var c2 feishuCreds
	_ = p.mgr.Load(ProviderFeishu, &c2)
	if c2.AccessToken != "" && time.Now().Before(c2.ExpiresAt.Add(-5*time.Minute)) {
		return c2.AccessToken, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c2.AppID},
		"client_secret": {c2.AppSecret},
		"refresh_token": {c2.RefreshToken},
	}
	var tok oauthTokenResponse
	if err := p.postForm(ctx, "/open-apis/authen/v2/oauth/token", form, &tok); err != nil {
		return "", fmt.Errorf("%w: 刷新 token 失败: %v", ErrReauthRequired, err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("%w: %s", ErrReauthRequired, truncate(tok.Msg, 200))
	}
	c2.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" { // 轮换语义：新 refresh_token 落盘
		c2.RefreshToken = tok.RefreshToken
	}
	c2.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if err := p.mgr.Save(ProviderFeishu, c2); err != nil {
		return "", err
	}
	return c2.AccessToken, nil
}

// postForm 表单 POST → JSON 解析（飞书统一 {code,msg,...} 包络；code!=0 且无
// 业务字段时视为上游错误）。
func (p *FeishuProvider) postForm(ctx context.Context, path string, form url.Values, out any) error {
	return p.doJSON(ctx, http.MethodPost, path, "application/x-www-form-urlencoded", []byte(form.Encode()), "", out)
}

// getJSON / postJSON 带权 JSON 调用（Authorization: Bearer <user_access_token>）。
func (p *FeishuProvider) getJSON(ctx context.Context, token, path string, out any) error {
	return p.doJSON(ctx, http.MethodGet, path, "", nil, token, out)
}

func (p *FeishuProvider) postJSON(ctx context.Context, token, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("extdocs: marshal: %w", err)
	}
	return p.doJSON(ctx, http.MethodPost, path, "application/json; charset=utf-8", raw, token, out)
}

// putJSON 带权 PUT → JSON（表格 values 写入与 style 设置均为 PUT，M0 实证）。
func (p *FeishuProvider) putJSON(ctx context.Context, token, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("extdocs: marshal: %w", err)
	}
	return p.doJSON(ctx, http.MethodPut, path, "application/json; charset=utf-8", raw, token, out)
}

func (p *FeishuProvider) doJSON(ctx context.Context, method, path, ct string, body []byte, token string, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+path, rd)
	if err != nil {
		return fmt.Errorf("extdocs: request: %w", err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	do := p.HTTPDo
	if do == nil {
		do = (&http.Client{Timeout: upstreamTimeout}).Do
	}
	resp, err := do(req)
	if err != nil {
		return &UpstreamError{Code: "unknown", Message: truncate(err.Error(), 300)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseCap))
	if err != nil {
		return &UpstreamError{Code: "unknown", Message: truncate(err.Error(), 200)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(raw, &e)
		code := fmt.Sprint(e.Code)
		if e.Code == 0 {
			code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return &UpstreamError{Code: code, Message: truncate(firstNonEmpty(e.Msg, fmt.Sprintf("HTTP %d", resp.StatusCode)), 300)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &UpstreamError{Code: "unknown", Message: "响应不可解析: " + truncate(err.Error(), 200)}
	}
	// 飞书业务包络：HTTP 200 但 code != 0（鉴权失效 99991663/99991661 等）
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Code != 0 {
		if envelope.Code == 99991663 || envelope.Code == 99991661 || envelope.Code == 99991679 {
			return fmt.Errorf("%w: 飞书 code=%d %s", ErrReauthRequired, envelope.Code, truncate(envelope.Msg, 200))
		}
		return &UpstreamError{Code: fmt.Sprint(envelope.Code), Message: truncate(envelope.Msg, 300)}
	}
	return nil
}

// Verify 探活 = token 有效（必要时刷新成功即证明凭据链健康）+ 记录水位。
func (p *FeishuProvider) Verify(ctx context.Context) error {
	if _, err := p.ensureFresh(ctx); err != nil {
		return err
	}
	var c feishuCreds
	_ = p.mgr.Load(ProviderFeishu, &c)
	c.LastVerifiedAt = time.Now().Format(time.RFC3339)
	return p.mgr.Save(ProviderFeishu, c)
}

// ---- 导入三步 ----

type feishuUploadResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		FileToken string `json:"file_token"`
	} `json:"data"`
}

type feishuImportCreateResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Ticket string `json:"ticket"`
	} `json:"data"`
}

type feishuImportGetResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Result struct {
			JobStatus   int    `json:"job_status"`
			JobErrorMsg string `json:"job_error_msg"`
			Token       string `json:"token"`
			Type        string `json:"type"`
			URL         string `json:"url"`
		} `json:"result"`
	} `json:"data"`
}

// ShareMarkdown markdown 快照 → 飞书 docx（"我的云文档"根目录）。
func (p *FeishuProvider) ShareMarkdown(ctx context.Context, title, markdown string) (string, error) {
	if strings.TrimSpace(title) == "" {
		title = "Mosaic 文档"
	}
	token, err := p.ensureFresh(ctx)
	if err != nil {
		return "", err
	}
	fileToken, err := p.uploadImportFile(ctx, token, title, markdown)
	if err != nil {
		return "", err
	}
	ticket, err := p.createImportTask(ctx, token, title, fileToken)
	if err != nil {
		return "", err
	}
	return p.pollImport(ctx, token, ticket)
}

// uploadImportFile 上传素材（parent_type=ccm_import_open；file token 5 分钟有效）。
func (p *FeishuProvider) uploadImportFile(ctx context.Context, token, name, markdown string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("file_name", name+".md")
	_ = w.WriteField("parent_type", "ccm_import_open")
	_ = w.WriteField("parent_node", "0")
	_ = w.WriteField("extra", `{"obj_type":"docx","file_extension":"md"}`)
	fw, err := w.CreateFormFile("file", name+".md")
	if err != nil {
		return "", fmt.Errorf("extdocs: multipart: %w", err)
	}
	if _, err := fw.Write([]byte(markdown)); err != nil {
		return "", fmt.Errorf("extdocs: multipart write: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("extdocs: multipart close: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/open-apis/drive/v1/medias/upload_all", &buf)
	if err != nil {
		return "", fmt.Errorf("extdocs: request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	do := p.HTTPDo
	if do == nil {
		do = (&http.Client{Timeout: upstreamTimeout}).Do
	}
	resp, err := do(req)
	if err != nil {
		return "", &UpstreamError{Code: "unknown", Message: truncate(err.Error(), 300)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, responseCap))
	var out feishuUploadResp
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = json.Unmarshal(raw, &out)
		code := fmt.Sprint(out.Code)
		if out.Code == 0 {
			code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return "", &UpstreamError{Code: code, Message: truncate(firstNonEmpty(out.Msg, fmt.Sprintf("HTTP %d", resp.StatusCode)), 300)}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &UpstreamError{Code: "unknown", Message: "上传响应不可解析: " + truncate(err.Error(), 200)}
	}
	if out.Code != 0 || out.Data.FileToken == "" {
		return "", &UpstreamError{Code: fmt.Sprint(out.Code), Message: truncate(firstNonEmpty(out.Msg, "上传未返回 file_token"), 300)}
	}
	return out.Data.FileToken, nil
}

// createImportTask 创建导入任务（md → docx，挂载"我的云文档"根目录）。
func (p *FeishuProvider) createImportTask(ctx context.Context, token, title, fileToken string) (string, error) {
	var out feishuImportCreateResp
	body := map[string]any{
		"file_extension": "md",
		"file_token":     fileToken,
		"file_name":      title,
		"type":           "docx",
		"point":          map[string]any{"mount_type": 1, "mount_key": ""},
	}
	if err := p.postJSON(ctx, token, "/open-apis/drive/v1/import_tasks", body, &out); err != nil {
		return "", err
	}
	if out.Data.Ticket == "" {
		return "", &UpstreamError{Code: fmt.Sprint(out.Code), Message: truncate(firstNonEmpty(out.Msg, "导入任务未返回 ticket"), 300)}
	}
	return out.Data.Ticket, nil
}

// pollImport 轮询导入结果（job_status 1/2 继续；0 成功取 url；其他携错误码）。
func (p *FeishuProvider) pollImport(ctx context.Context, token, ticket string) (string, error) {
	deadline := time.Now().Add(feishuImportMax)
	for {
		var out feishuImportGetResp
		if err := p.getJSON(ctx, token, "/open-apis/drive/v1/import_tasks/"+url.PathEscape(ticket), &out); err != nil {
			return "", err
		}
		switch out.Data.Result.JobStatus {
		case 0:
			if out.Data.Result.URL != "" {
				return out.Data.Result.URL, nil
			}
			if out.Data.Result.Token != "" {
				return fmt.Sprintf("https://www.feishu.cn/docx/%s", out.Data.Result.Token), nil
			}
			return "", &UpstreamError{Code: "unknown", Message: "导入成功但结果未含 URL"}
		case 1, 2: // 初始化/处理中 → 继续等
		default:
			return "", &UpstreamError{
				Code:    fmt.Sprintf("job_%d", out.Data.Result.JobStatus),
				Message: truncate(firstNonEmpty(out.Data.Result.JobErrorMsg, fmt.Sprintf("导入任务失败 job_status=%d", out.Data.Result.JobStatus)), 300),
			}
		}
		if time.Now().After(deadline) {
			return "", &UpstreamError{Code: "job_timeout", Message: "导入任务超时（60s）——请稍后在飞书云文档查看是否完成"}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(feishuImportPoll):
		}
	}
}

// ResolveLink 链接 → 描述子：doc_type 路径推断 + title 尽力拉取。
func (p *FeishuProvider) ResolveLink(ctx context.Context, rawURL string) (LinkInfo, error) {
	info := LinkInfo{URL: rawURL, DocType: feishuDocType(rawURL)}
	info.Title = FetchTitle(p.HTTPDo, rawURL)
	return info, nil
}

// feishuDocType 路径前缀 → 文档类型（未知 = 空串）。
func feishuDocType(rawURL string) string {
	u, err := parseURL(rawURL)
	if err != nil {
		return ""
	}
	switch {
	case strings.HasPrefix(u.Path, "/docx/"):
		return "docx"
	case strings.HasPrefix(u.Path, "/doc/"):
		return "doc"
	case strings.HasPrefix(u.Path, "/wiki/"):
		return "wiki"
	case strings.HasPrefix(u.Path, "/sheets/"):
		return "sheet"
	case strings.HasPrefix(u.Path, "/base/"):
		return "base"
	case strings.HasPrefix(u.Path, "/minutes/"):
		return "minutes"
	case strings.HasPrefix(u.Path, "/mindnotes/"):
		return "mind"
	case strings.HasPrefix(u.Path, "/slides/"):
		return "slide"
	case strings.HasPrefix(u.Path, "/file/"):
		return "file"
	default:
		return ""
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
