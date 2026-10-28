// UT：extdocs 域——凭据管理（per-provider JSON + txdocs-token 迁移）、腾讯面
// （Bearer 重试/URL 提取/请求形状）、飞书面（OAuth 交换与轮换持久化、导入三步
// multipart 形状与轮询）、Service 编排（already_shared/force、host 分派）。
package extdocs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- Manager ----

func TestManagerPerProviderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadManager(filepath.Join(dir, "extdocs"))
	if err != nil {
		t.Fatal(err)
	}
	var tc tencentCreds
	if err := m.Save(ProviderTencent, tencentCreds{Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(ProviderTencent, &tc); err != nil || tc.Token != "tok" {
		t.Fatalf("round trip = %+v, err = %v", tc, err)
	}
	// feishu 独立文件互不影响
	var fc feishuCreds
	if err := m.Save(ProviderFeishu, feishuCreds{AppID: "cli_x", AppSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(ProviderFeishu, &fc); err != nil || fc.AppID != "cli_x" {
		t.Fatalf("feishu = %+v", fc)
	}
	_ = m.Load(ProviderTencent, &tc)
	if tc.Token != "tok" {
		t.Fatal("tencent 凭据被 feishu 写入破坏")
	}
	// 0600（仅 Unix 面——platform-notes W-6：Windows 无 POSIX 权限语义，写位
	// 断言在 windows 宿主恒为 -rw-rw-rw-，CI windows 腿因此红过，2026-10-28）
	if goruntime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "extdocs", ProviderFeishu+".json")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("权限 = %v", fi.Mode().Perm())
		}
	}
	// 清除
	if err := m.Clear(ProviderFeishu); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "extdocs", ProviderFeishu+".json")); !os.IsNotExist(err) {
		t.Fatal("清除应删除文件")
	}
}

func TestManagerMigratesLegacyTxdocsToken(t *testing.T) {
	base := t.TempDir()
	// 旧布局：DataDir/txdocs-token + .state.json；新布局：DataDir/extdocs/
	_ = os.WriteFile(filepath.Join(base, "txdocs-token"), []byte("legacy-tok\n"), 0o600)
	_ = os.WriteFile(filepath.Join(base, "txdocs-token.state.json"), []byte(`{"last_verified_at":"2026-09-24T10:00:00Z"}`), 0o600)
	m, err := LoadManager(filepath.Join(base, "extdocs"))
	if err != nil {
		t.Fatal(err)
	}
	var tc tencentCreds
	_ = m.Load(ProviderTencent, &tc)
	if tc.Token != "legacy-tok" || tc.LastVerifiedAt != "2026-09-24T10:00:00Z" {
		t.Fatalf("迁移 = %+v", tc)
	}
	if _, err := os.Stat(filepath.Join(base, "txdocs-token")); !os.IsNotExist(err) {
		t.Fatal("旧文件应删除（幂等）")
	}
	// 二次装载：迁移幂等（不覆盖既有）
	m2, _ := LoadManager(filepath.Join(base, "extdocs"))
	var tc2 tencentCreds
	_ = m2.Load(ProviderTencent, &tc2)
	if tc2.Token != "legacy-tok" {
		t.Fatalf("二次装载 = %+v", tc2)
	}
}

// ---- 腾讯面（平移） ----

func newTencent(t *testing.T, handler http.HandlerFunc) (*Manager, *TencentProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m, _ := LoadManager(filepath.Join(t.TempDir(), "extdocs"))
	p := NewTencentProvider(m, srv.URL)
	return m, p, srv
}

func TestTencentBearerRetryAndShare(t *testing.T) {
	var bearerOK atomic.Bool
	m, p, _ := newTencent(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if auth == "Bearer tok" {
			bearerOK.Store(true)
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["method"] == "tools/list" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"},{"name":"b"}]}}`))
				return
			}
			params := req["params"].(map[string]any)
			if params["name"] != "create_smartcanvas_by_markdown" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"创建成功 https://docs.qq.com/aio/p/abc123"}]}}`))
			return
		}
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid_token","error_description":"Bearer token required"}`))
	})
	// 未配置 → ErrTokenMissing
	if _, err := p.ShareMarkdown(context.Background(), "t", "m"); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("err = %v", err)
	}
	if err := m.Save(ProviderTencent, tencentCreds{Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bearerOK.Load() {
		t.Fatal("应经 Bearer 形态重试")
	}
	u, err := p.ShareMarkdown(context.Background(), "标题", "# m")
	if err != nil || u != "https://docs.qq.com/aio/p/abc123" {
		t.Fatalf("share = %q, err = %v", u, err)
	}
}

func TestTencentHostAndDocType(t *testing.T) {
	_, p, _ := newTencent(t, func(w http.ResponseWriter, r *http.Request) {})
	if !p.HostMatches("docs.qq.com") || p.HostMatches("evil.cn") {
		t.Fatal("host 判定")
	}
	for _, c := range []struct{ u, dt string }{
		{"https://docs.qq.com/sheet/ABC", "sheet"},
		{"https://docs.qq.com/aio/p/x", "aio"},
		{"https://docs.qq.com/unknown/x", ""},
	} {
		if got := tencentDocType(c.u); got != c.dt {
			t.Fatalf("%s → %q（期望 %q）", c.u, got, c.dt)
		}
	}
}

// ---- 飞书面 ----

func newFeishu(t *testing.T, handler http.HandlerFunc) (*Manager, *FeishuProvider) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m, _ := LoadManager(filepath.Join(t.TempDir(), "extdocs"))
	p := NewFeishuProvider(m, srv.URL)
	return m, p
}

func TestFeishuHostAndDocType(t *testing.T) {
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, c := range []struct {
		host string
		ok   bool
	}{
		{"feishu.cn", true},
		{"abc123xyz.feishu.cn", true},
		{"evil.cn", false},
		{"feishu.cn.evil.com", false},
	} {
		if p.HostMatches(c.host) != c.ok {
			t.Fatalf("host %s = %v（期望 %v）", c.host, p.HostMatches(c.host), c.ok)
		}
	}
	for _, c := range []struct{ u, dt string }{
		{"https://x.feishu.cn/docx/abc", "docx"},
		{"https://x.feishu.cn/wiki/n1", "wiki"},
		{"https://x.feishu.cn/sheets/s1", "sheet"},
		{"https://x.feishu.cn/base/b1", "base"},
		{"https://x.feishu.cn/minutes/m1", "minutes"},
		{"https://x.feishu.cn/unknown/u", ""},
	} {
		if got := feishuDocType(c.u); got != c.dt {
			t.Fatalf("%s → %q", c.u, got)
		}
	}
}

func TestFeishuExchangeAndShare(t *testing.T) {
	var uploadBody struct {
		parentType string
		parentNode string
		extra      string
		fileName   string
		fileBody   string
	}
	var importBody map[string]any
	var pollCount atomic.Int32
	m, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/open-apis/authen/v2/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "authorization_code" {
				_, _ = w.Write([]byte(`{"code":0,"access_token":"u-1","refresh_token":"r-1","expires_in":7200,"refresh_token_expires_in":2592000}`))
				return
			}
			// refresh：轮换语义
			_, _ = w.Write([]byte(`{"code":0,"access_token":"u-2","refresh_token":"r-2","expires_in":7200}`))
		case r.URL.Path == "/open-apis/drive/v1/medias/upload_all":
			// multipart 解析关键字段
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				w.WriteHeader(400)
				return
			}
			uploadBody.parentType = r.FormValue("parent_type")
			uploadBody.parentNode = r.FormValue("parent_node")
			uploadBody.extra = r.FormValue("extra")
			uploadBody.fileName = r.FormValue("file_name")
			file, _, _ := r.FormFile("file")
			raw, _ := io.ReadAll(file)
			uploadBody.fileBody = string(raw)
			_, _ = w.Write([]byte(`{"code":0,"data":{"file_token":"ft-1"}}`))
		case r.URL.Path == "/open-apis/drive/v1/import_tasks" && r.Method == "POST":
			_ = json.NewDecoder(r.Body).Decode(&importBody)
			_, _ = w.Write([]byte(`{"code":0,"data":{"ticket":"tk-1"}}`))
		case strings.HasPrefix(r.URL.Path, "/open-apis/drive/v1/import_tasks/"):
			n := pollCount.Add(1)
			if n == 1 {
				_, _ = w.Write([]byte(`{"code":0,"data":{"result":{"job_status":2}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"result":{"job_status":0,"token":"doccnX","type":"docx","url":"https://x.feishu.cn/docx/doccnX"}}}`))
		default:
			w.WriteHeader(404)
		}
	})

	// 未配置 → ErrTokenMissing
	if _, err := p.ShareMarkdown(context.Background(), "t", "m"); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("err = %v", err)
	}
	// 配置应用凭据但未授权 → ErrReauthRequired
	if err := p.UpdateApp("cli_x", "sec"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ShareMarkdown(context.Background(), "t", "m"); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("未授权 err = %v", err)
	}
	// 授权码交换
	if err := p.ExchangeCode(context.Background(), "code-1"); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if !st.AppConfigured || !st.Authorized {
		t.Fatalf("status = %+v", st)
	}
	// token 提前过期 → 刷新（轮换持久化）
	var c feishuCreds
	_ = m.Load(ProviderFeishu, &c)
	c.ExpiresAt = time.Now().Add(-time.Minute) // 已过期
	_ = m.Save(ProviderFeishu, c)
	// 共享三步
	u, err := p.ShareMarkdown(context.Background(), "标题", "# 正文")
	if err != nil || u != "https://x.feishu.cn/docx/doccnX" {
		t.Fatalf("share = %q, err = %v", u, err)
	}
	// 刷新轮换落盘
	_ = m.Load(ProviderFeishu, &c)
	if c.AccessToken != "u-2" || c.RefreshToken != "r-2" {
		t.Fatalf("轮换未持久化 = %+v", c)
	}
	// 上传 multipart 形状
	if uploadBody.parentType != "ccm_import_open" || uploadBody.parentNode != "0" {
		t.Fatalf("upload 参数 = %+v", uploadBody)
	}
	var extra map[string]any
	_ = json.Unmarshal([]byte(uploadBody.extra), &extra)
	if extra["obj_type"] != "docx" || extra["file_extension"] != "md" {
		t.Fatalf("extra = %v", extra)
	}
	if uploadBody.fileBody != "# 正文" || !strings.HasSuffix(uploadBody.fileName, ".md") {
		t.Fatalf("file = %q / name = %q", uploadBody.fileBody, uploadBody.fileName)
	}
	// 导入任务形状
	if importBody["file_extension"] != "md" || importBody["file_token"] != "ft-1" || importBody["file_name"] != "标题" {
		t.Fatalf("import = %v", importBody)
	}
	point := importBody["point"].(map[string]any)
	if point["mount_type"] != float64(1) || point["mount_key"] != "" {
		t.Fatalf("point = %v", point)
	}
	// 轮询至少两次（1→0 路径）
	if pollCount.Load() < 2 {
		t.Fatalf("poll = %d", pollCount.Load())
	}
	// verify（token 有效路径）
	if err := p.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFeishuImportFailure(t *testing.T) {
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/open-apis/authen/v2/oauth/token":
			_, _ = w.Write([]byte(`{"code":0,"access_token":"u","refresh_token":"r","expires_in":7200}`))
		case r.URL.Path == "/open-apis/drive/v1/medias/upload_all":
			_, _ = w.Write([]byte(`{"code":0,"data":{"file_token":"ft"}}`))
		case r.URL.Path == "/open-apis/drive/v1/import_tasks" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"code":0,"data":{"ticket":"tk"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{"result":{"job_status":110,"job_error_msg":"无权限"}}}`))
		}
	})
	_ = p.UpdateApp("cli_x", "sec")
	_ = p.ExchangeCode(context.Background(), "c")
	_, err := p.ShareMarkdown(context.Background(), "t", "m")
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Code != "job_110" || !strings.Contains(ue.Message, "无权限") {
		t.Fatalf("err = %v", err)
	}
}

func TestFeishuAuthorizeURL(t *testing.T) {
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {})
	_ = p.UpdateApp("cli_abc", "sec")
	raw := p.AuthorizeURL()
	u, err := url.Parse(raw)
	if err != nil || u.Path != "/open-apis/authen/v1/authorize" {
		t.Fatalf("url = %s err = %v", raw, err)
	}
	q := u.Query()
	if q.Get("app_id") != "cli_abc" || q.Get("redirect_uri") != FeishuRedirectURI {
		t.Fatalf("query = %v", q)
	}
	if !strings.Contains(q.Get("scope"), "offline_access") || !strings.Contains(q.Get("scope"), "drive:drive") {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
}

// ---- Service 编排 ----

type memStore struct {
	mu sync.Mutex
	m  map[string]Share
}

func (s *memStore) ListExternalShares(_ context.Context, docID, provider string) ([]Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Share
	for _, sh := range s.m {
		if (docID == "" || sh.DocID == docID) && (provider == "" || sh.Provider == provider) {
			out = append(out, sh)
		}
	}
	return out, nil
}

func (s *memStore) GetExternalShare(_ context.Context, docID, provider string) (Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh, ok := s.m[docID+"|"+provider]; ok {
		return sh, nil
	}
	return Share{}, ErrShareNotFound
}

func (s *memStore) UpsertExternalShare(_ context.Context, sh Share) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Share{}
	}
	s.m[sh.DocID+"|"+sh.Provider] = sh
	return nil
}

func (s *memStore) DeleteExternalShare(_ context.Context, docID, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[docID+"|"+provider]; !ok {
		return ErrShareNotFound
	}
	delete(s.m, docID+"|"+provider)
	return nil
}

// stubProvider 测试件 provider。
type stubProvider struct {
	id      string
	kind    string
	shareTo string
}

func (p *stubProvider) ID() string          { return p.id }
func (p *stubProvider) DisplayName() string { return p.id }
func (p *stubProvider) RefKind() string     { return p.kind }
func (p *stubProvider) HostMatches(h string) bool {
	return h == p.id+".example.com"
}
func (p *stubProvider) Verify(_ context.Context) error { return nil }
func (p *stubProvider) ShareMarkdown(_ context.Context, _, _ string) (string, error) {
	return p.shareTo, nil
}
func (p *stubProvider) ResolveLink(_ context.Context, rawURL string) (LinkInfo, error) {
	return LinkInfo{URL: rawURL}, nil
}

func TestServiceShareAndDispatch(t *testing.T) {
	store := &memStore{}
	svc := NewService(store)
	svc.Clock = func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) }
	a := &stubProvider{id: "a", kind: "txdoc", shareTo: "https://a.example.com/1"}
	b := &stubProvider{id: "b", kind: "fdoc", shareTo: "https://b.example.com/2"}
	svc.Register(a)
	svc.Register(b)

	ctx := context.Background()
	// 双 provider 各自一条映射（同文档并存）
	if sh, err := svc.ShareDoc(ctx, "a", "doc_000000000001", "t", "m", "me", false); err != nil || sh.ExternalURL != "https://a.example.com/1" {
		t.Fatalf("a = %+v err = %v", sh, err)
	}
	if sh, err := svc.ShareDoc(ctx, "b", "doc_000000000001", "t", "m", "me", false); err != nil || sh.ExternalURL != "https://b.example.com/2" {
		t.Fatalf("b = %+v err = %v", sh, err)
	}
	// already_shared（携带既有；不出网——stub 无副作用，只验错误类型）
	_, err := svc.ShareDoc(ctx, "a", "doc_000000000001", "t", "m", "me", false)
	var as *AlreadySharedError
	if !errors.As(err, &as) || as.Existing.ExternalURL != "https://a.example.com/1" {
		t.Fatalf("err = %v", err)
	}
	// 未知 provider
	if _, err := svc.ShareDoc(ctx, "nope", "doc_000000000002", "t", "m", "me", false); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("err = %v", err)
	}
	// host 分派
	_, got, err := svc.ResolveLink(ctx, "https://b.example.com/x")
	if err != nil || got.ID() != "b" {
		t.Fatalf("dispatch = %v err = %v", got, err)
	}
	if _, _, err := svc.ResolveLink(ctx, "https://evil.com/x"); !errors.Is(err, ErrBadLink) {
		t.Fatalf("evil err = %v", err)
	}
	if _, _, err := svc.ResolveLink(ctx, "http://a.example.com/x"); !errors.Is(err, ErrBadLink) {
		t.Fatalf("http err = %v", err)
	}
	// 双维过滤
	list, _ := svc.ListShares(ctx, "doc_000000000001", "a")
	if len(list) != 1 || list[0].Provider != "a" {
		t.Fatalf("filter = %v", list)
	}
}

// ---- 标题提取 ----

func TestExtractTitle(t *testing.T) {
	if got := ExtractTitle([]byte("<title>季度表 &amp; 说明</title>")); got != "季度表 & 说明" {
		t.Fatalf("title = %q", got)
	}
	// SPA 页面 og:title 兜底
	if got := ExtractTitle([]byte(`<title>飞书云文档</title><meta property="og:title" content="2026 Q3 复盘"/>`)); got != "2026 Q3 复盘" {
		t.Fatalf("og = %q", got)
	}
	if got := ExtractTitle([]byte("nothing")); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

// ---- 飞书回环回调监听（自动绑定） ----

// freePort 取一个当前空闲的回环端口（bind:0 取号后放回，测试内即刻重绑）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestFeishuArmCallbackAutoBind(t *testing.T) {
	var exchangeHits atomic.Int32
	var redirect string // 交换请求须与授权/监听同值（handler 执行时已赋）
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/authen/v2/oauth/token" {
			exchangeHits.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("redirect_uri") == redirect {
				_, _ = w.Write([]byte(`{"code":0,"access_token":"u-1","refresh_token":"r-1","expires_in":7200}`))
				return
			}
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(404)
	})
	if err := p.UpdateApp("cli_x", "sec"); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	redirect = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	p.RedirectURI = redirect

	if !p.ArmCallback() {
		t.Fatal("空闲端口 arm 应成功")
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc&state=mosaic", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "成功") {
		t.Fatalf("回调页：%d %s", resp.StatusCode, body)
	}
	if exchangeHits.Load() != 1 {
		t.Fatalf("exchange 次数 = %d", exchangeHits.Load())
	}
	if st := p.Status(); !st.Authorized {
		t.Fatal("回调落回后应已授权")
	}
	p.CloseCallback()
	// 收摊后同端口可重挂（重复发起授权）
	if !p.ArmCallback() {
		t.Fatal("收摊后重挂应成功")
	}
	p.CloseCallback()
}

func TestFeishuArmCallbackBusyPort(t *testing.T) {
	blk, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {})
	p.RedirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", blk.Addr().(*net.TCPAddr).Port)
	if p.ArmCallback() {
		t.Fatal("占用端口不应 arm 成功")
	}
}

func TestFeishuArmCallbackRejectsBadState(t *testing.T) {
	var upstreamHits atomic.Int32
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(200)
	})
	if err := p.UpdateApp("cli_x", "sec"); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	p.RedirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	if !p.ArmCallback() {
		t.Fatal("arm 应成功")
	}
	defer p.CloseCallback()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc&state=evil", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("state 不符应 400：%d", resp.StatusCode)
	}
	if upstreamHits.Load() != 0 {
		t.Fatal("state 不符不应触达上游")
	}
	if st := p.Status(); st.Authorized {
		t.Fatal("不应授权")
	}
}
