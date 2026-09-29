// UT 层：外部云文档面 HTTP 端点（RFC-0016 多 provider）——写门三件套、
// settings 双 provider 往返（凭据值不回传）、共享事务（双 provider 并存/
// 409/force/404/bad_provider）、verify、resolve host 分派、飞书 OAuth 引导与
// 绑定、post_message external_refs kind→host 校验与快照同形、未装配 404。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// extTestEnv 房间 + 文档 + extdocs 双 provider 装配 + 可脚本上游。
type extTestEnv struct {
	ts        *httptest.Server
	docStore  *doc.MemStore
	roomStore *room.MemStore
	svc       *extdocs.Service
	tencent   *extdocs.TencentProvider
	feishu    *extdocs.FeishuProvider
	// cbBlock 占住回调端口的阻塞监听：authorize 的 auto_captured 恒 false
	// （释放后测自动路径）。
	cbBlock net.Listener
}

// newExtTestEnv upstreamMCP 为腾讯 MCP 假上游应答体；feishuUpstream 为飞书
// API 假上游处理函数（nil = 默认成功脚本）。
func newExtTestEnv(t *testing.T, ownerToken, upstreamMCP string, feishuUpstream http.HandlerFunc) *extTestEnv {
	t.Helper()
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamMCP))
	}))
	t.Cleanup(mcpSrv.Close)
	if feishuUpstream == nil {
		feishuUpstream = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/open-apis/authen/v2/oauth/token":
				_, _ = w.Write([]byte(`{"code":0,"access_token":"u-1","refresh_token":"r-1","expires_in":7200}`))
			case r.URL.Path == "/open-apis/drive/v1/medias/upload_all":
				_, _ = w.Write([]byte(`{"code":0,"data":{"file_token":"ft-1"}}`))
			case r.URL.Path == "/open-apis/drive/v1/import_tasks" && r.Method == "POST":
				_, _ = w.Write([]byte(`{"code":0,"data":{"ticket":"tk-1"}}`))
			default:
				_, _ = w.Write([]byte(`{"code":0,"data":{"result":{"job_status":0,"url":"https://x.feishu.cn/docx/fk-1"}}}`))
			}
		}
	}
	feishuSrv := httptest.NewServer(feishuUpstream)
	t.Cleanup(feishuSrv.Close)

	roomStore := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	newID := func(prefix string) string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return prefix + "_ex_" + fmt.Sprintf("%08d", n)
	}
	roomSvc := room.NewService(room.Config{
		Store:  roomStore,
		Lister: roomStore,
		Clock:  func() string { return "2026-09-26T11:00:00.000Z" },
		NewID:  newID,
		Tenant: "ten_local",
	})
	docStore := doc.NewMemStore()
	docSvc := doc.NewService(doc.Config{
		Store:    docStore,
		Searcher: docStore,
		Lister:   docStore,
		Clock:    func() string { return "2026-09-26T11:00:00.000Z" },
		NewID:    newID,
		NewDocID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return "doc_" + fmt.Sprintf("%012d", n)
		},
		Tenant: "ten_local",
	})
	mgr, err := extdocs.LoadManager(t.TempDir() + "/extdocs")
	if err != nil {
		t.Fatal(err)
	}
	tencent := extdocs.NewTencentProvider(mgr, mcpSrv.URL)
	feishu := extdocs.NewFeishuProvider(mgr, feishuSrv.URL)
	// 回调端口钉死占用：RedirectURI 指向阻塞监听（生产恒默认登记值）
	cbBlock, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cbBlock.Close() })
	feishu.RedirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", cbBlock.Addr().(*net.TCPAddr).Port)
	titleResponder := func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(`<meta property="og:title" content="测试文档"/>`)),
			Request:    req,
		}, nil
	}
	tencent.HTTPDo = func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "docs.qq.com" {
			return titleResponder(req)
		}
		return mcpSrv.Client().Do(req)
	}
	feishu.HTTPDo = func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Hostname(), "feishu.cn") {
			return titleResponder(req)
		}
		return feishuSrv.Client().Do(req)
	}
	svc := extdocs.NewService(&memExtStore{})
	svc.Register(tencent)
	svc.Register(feishu)

	env := &extTestEnv{docStore: docStore, roomStore: roomStore, svc: svc, tencent: tencent, feishu: feishu, cbBlock: cbBlock}
	handler := New(Deps{
		SVC:        roomSvc,
		Reader:     roomStore,
		DocSVC:     docSvc,
		DocReader:  docStore,
		ExtDocs:    svc,
		Hub:        sse.NewHub(),
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: ownerToken,
	})
	env.ts = httptest.NewServer(handler)
	t.Cleanup(env.ts.Close)
	return env
}

// memExtStore extdocs.ShareStore 测试件。
type memExtStore struct {
	mu sync.Mutex
	m  map[string]extdocs.Share
}

func (s *memExtStore) ListExternalShares(_ context.Context, docID, provider string) ([]extdocs.Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []extdocs.Share
	for _, sh := range s.m {
		if (docID == "" || sh.DocID == docID) && (provider == "" || sh.Provider == provider) {
			out = append(out, sh)
		}
	}
	return out, nil
}

func (s *memExtStore) GetExternalShare(_ context.Context, docID, provider string) (extdocs.Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh, ok := s.m[docID+"|"+provider]; ok {
		return sh, nil
	}
	return extdocs.Share{}, extdocs.ErrShareNotFound
}

func (s *memExtStore) UpsertExternalShare(_ context.Context, sh extdocs.Share) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]extdocs.Share{}
	}
	s.m[sh.DocID+"|"+sh.Provider] = sh
	return nil
}

func (s *memExtStore) DeleteExternalShare(_ context.Context, docID, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[docID+"|"+provider]; !ok {
		return extdocs.ErrShareNotFound
	}
	delete(s.m, docID+"|"+provider)
	return nil
}

func extRequest(t *testing.T, method, url string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func extJSON(token string) map[string]string {
	return map[string]string{"Content-Type": "application/json", "X-Owner-Token": token}
}

func extCodeOf(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestExtDocsWriteGuards(t *testing.T) {
	env := newExtTestEnv(t, "tok_ex", `{}`, nil)
	h := extJSON("tok_ex")
	// 无 token → 401
	resp, _ := extRequest(t, "PUT", env.ts.URL+"/v1/extdocs/tencent_docs/settings", map[string]any{"token": "x"},
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != 401 {
		t.Fatalf("无 token = %d", resp.StatusCode)
	}
	// 跨源 → 403
	resp, _ = extRequest(t, "PUT", env.ts.URL+"/v1/extdocs/tencent_docs/settings", map[string]any{"token": "x"},
		map[string]string{"Content-Type": "application/json", "X-Owner-Token": "tok_ex", "Origin": "https://evil.com"})
	if resp.StatusCode != 403 {
		t.Fatalf("跨源 = %d", resp.StatusCode)
	}
	// 非 JSON → 415
	resp, _ = extRequest(t, "PUT", env.ts.URL+"/v1/extdocs/tencent_docs/settings", map[string]any{"token": "x"},
		map[string]string{"X-Owner-Token": "tok_ex"})
	if resp.StatusCode != 415 {
		t.Fatalf("非 JSON = %d", resp.StatusCode)
	}
	// 正常
	resp, body := extRequest(t, "PUT", env.ts.URL+"/v1/extdocs/tencent_docs/settings", map[string]any{"token": "abc"}, h)
	if resp.StatusCode != 200 || body["configured"] != true {
		t.Fatalf("put: %d %v", resp.StatusCode, body)
	}
	// 未知 provider → 404 extdocs_bad_provider
	resp, body = extRequest(t, "PUT", env.ts.URL+"/v1/extdocs/pan_docs/settings", map[string]any{"token": "x"}, h)
	if resp.StatusCode != 404 || extCodeOf(body) != "extdocs_bad_provider" {
		t.Fatalf("bad provider: %d %v", resp.StatusCode, body)
	}
}

func TestExtDocsSettingsNoSecretEcho(t *testing.T) {
	env := newExtTestEnv(t, "", `{}`, nil)
	// 腾讯：token 不回传
	_, body := extRequest(t, "PUT", env.ts.URL+"/v1/extdocs/tencent_docs/settings", map[string]any{"token": "secret-tx"}, extJSON(""))
	if body["configured"] != true {
		t.Fatalf("tx put = %v", body)
	}
	_, body = extRequest(t, "GET", env.ts.URL+"/v1/extdocs/tencent_docs/settings", nil, nil)
	if strings.Contains(fmt.Sprint(body), "secret-tx") {
		t.Fatal("腾讯 token 不得回传")
	}
	// 飞书：app_secret 不回传；app_id 属半公开标识也不回值（状态化）
	_, body = extRequest(t, "PUT", env.ts.URL+"/v1/extdocs/feishu_docs/settings",
		map[string]any{"app_id": "cli_x", "app_secret": "sec-fs"}, extJSON(""))
	if body["app_configured"] != true {
		t.Fatalf("fs put = %v", body)
	}
	_, body = extRequest(t, "GET", env.ts.URL+"/v1/extdocs/feishu_docs/settings", nil, nil)
	if strings.Contains(fmt.Sprint(body), "sec-fs") || strings.Contains(fmt.Sprint(body), "cli_x") {
		t.Fatal("飞书凭据不得回传")
	}
}

func TestExtDocsShareFlowBothProviders(t *testing.T) {
	env := newExtTestEnv(t, "tok_ex", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"https://docs.qq.com/aio/p/t1"}]}}`, nil)
	h := extJSON("tok_ex")

	resp, created := postDocJSON(t, env.ts.URL+"/v1/docs",
		docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9301", map[string]any{"title": "双端共享"}),
		map[string]string{"X-Owner-Token": "tok_ex"})
	if resp.StatusCode != 200 {
		t.Fatalf("create doc: %d", resp.StatusCode)
	}
	docID := created.DocId

	// 腾讯未配置 → 409 token_missing
	resp, body := extRequest(t, "POST", env.ts.URL+"/v1/extdocs/shares", map[string]any{"doc_id": docID, "provider": "tencent_docs"}, h)
	if resp.StatusCode != 409 || extCodeOf(body) != "extdocs_token_missing" {
		t.Fatalf("tx no cred: %d %v", resp.StatusCode, body)
	}
	_ = env.tencent.UpdateToken("tok")

	// 飞书未授权 → 409 reauth_required（已存 app 凭据但未 OAuth）
	_ = env.feishu.UpdateApp("cli_x", "sec")
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/shares", map[string]any{"doc_id": docID, "provider": "feishu_docs"}, h)
	if resp.StatusCode != 409 || extCodeOf(body) != "extdocs_reauth_required" {
		t.Fatalf("fs reauth: %d %v", resp.StatusCode, body)
	}
	// 绑定授权（走假 token 端点）
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/feishu/callback", map[string]any{"code": "c1"}, h)
	if resp.StatusCode != 200 || body["authorized"] != true {
		t.Fatalf("callback: %d %v", resp.StatusCode, body)
	}

	// 双 provider 并存共享
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/shares", map[string]any{"doc_id": docID, "provider": "tencent_docs"}, h)
	if resp.StatusCode != 201 || body["external_url"] != "https://docs.qq.com/aio/p/t1" {
		t.Fatalf("tx share: %d %v", resp.StatusCode, body)
	}
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/shares", map[string]any{"doc_id": docID, "provider": "feishu_docs"}, h)
	if resp.StatusCode != 201 || body["external_url"] != "https://x.feishu.cn/docx/fk-1" {
		t.Fatalf("fs share: %d %v", resp.StatusCode, body)
	}

	// 重复（同 provider）→ 409 携带既有
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/shares", map[string]any{"doc_id": docID, "provider": "tencent_docs"}, h)
	if resp.StatusCode != 409 || extCodeOf(body) != "extdocs_already_shared" {
		t.Fatalf("re-share: %d %v", resp.StatusCode, body)
	}
	if share, ok := body["share"].(map[string]any); !ok || share["external_url"] != "https://docs.qq.com/aio/p/t1" {
		t.Fatalf("409 应携带既有映射: %v", body)
	}

	// 文档不存在 → 404；未知 provider → 404
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/shares", map[string]any{"doc_id": "doc_0000000000ff", "provider": "tencent_docs"}, h)
	if resp.StatusCode != 404 || extCodeOf(body) != "extdocs_doc_not_found" {
		t.Fatalf("doc nf: %d %v", resp.StatusCode, body)
	}
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/shares", map[string]any{"doc_id": docID, "provider": "pan_docs"}, h)
	if resp.StatusCode != 404 || extCodeOf(body) != "extdocs_bad_provider" {
		t.Fatalf("bad provider: %d %v", resp.StatusCode, body)
	}

	// 解除（provider 查询参数）
	resp, _ = extRequest(t, "DELETE", env.ts.URL+"/v1/extdocs/shares/"+docID+"?provider=tencent_docs", nil, h)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, body = extRequest(t, "DELETE", env.ts.URL+"/v1/extdocs/shares/"+docID+"?provider=tencent_docs", nil, h)
	if resp.StatusCode != 404 || extCodeOf(body) != "extdocs_share_not_found" {
		t.Fatalf("re-delete: %d %v", resp.StatusCode, body)
	}
}

func TestExtDocsResolveDispatch(t *testing.T) {
	env := newExtTestEnv(t, "", `{}`, nil)
	h := extJSON("")
	// 腾讯链接 → kind txdoc + title
	resp, body := extRequest(t, "POST", env.ts.URL+"/v1/extdocs/resolve", map[string]any{"url": "https://docs.qq.com/sheet/DZ95"}, h)
	if resp.StatusCode != 200 || body["kind"] != "txdoc" || body["title"] != "测试文档" || body["doc_type"] != "sheet" {
		t.Fatalf("tx resolve: %d %v", resp.StatusCode, body)
	}
	// 飞书链接 → kind fdoc
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/resolve", map[string]any{"url": "https://abc.feishu.cn/docx/abc"}, h)
	if resp.StatusCode != 200 || body["kind"] != "fdoc" || body["doc_type"] != "docx" {
		t.Fatalf("fs resolve: %d %v", resp.StatusCode, body)
	}
	// 非白名单 → 400
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/resolve", map[string]any{"url": "https://evil.com/x"}, h)
	if resp.StatusCode != 400 || extCodeOf(body) != "extdocs_bad_link" {
		t.Fatalf("evil: %d %v", resp.StatusCode, body)
	}
}

func TestFeishuAuthorize(t *testing.T) {
	env := newExtTestEnv(t, "", `{}`, nil)
	// 未存应用凭据 → 409
	resp, body := extRequest(t, "GET", env.ts.URL+"/v1/extdocs/feishu/authorize", nil, nil)
	if resp.StatusCode != 409 || extCodeOf(body) != "extdocs_token_missing" {
		t.Fatalf("no app: %d %v", resp.StatusCode, body)
	}
	_ = env.feishu.UpdateApp("cli_x", "sec")
	resp, body = extRequest(t, "GET", env.ts.URL+"/v1/extdocs/feishu/authorize", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}
	if body["redirect_uri"] != env.feishu.RedirectURIValue() {
		t.Fatalf("redirect = %v", body["redirect_uri"])
	}
	scopes, _ := body["scopes"].([]any)
	if len(scopes) == 0 {
		t.Fatalf("scopes = %v", body["scopes"])
	}
	// 测试环境回调端口被钉死占用 → 自动绑定降级 false
	if body["auto_captured"] != false {
		t.Fatalf("auto_captured = %v（端口占用应 false）", body["auto_captured"])
	}
}

func TestFeishuAuthorizeAutoCallback(t *testing.T) {
	env := newExtTestEnv(t, "", `{}`, nil)
	_ = env.feishu.UpdateApp("cli_x", "sec")
	// 释放阻塞端口 → 挂起回环监听
	_ = env.cbBlock.Close()
	resp, body := extRequest(t, "GET", env.ts.URL+"/v1/extdocs/feishu/authorize", nil, nil)
	if resp.StatusCode != 200 || body["auto_captured"] != true {
		t.Fatalf("authorize auto: %d %v", resp.StatusCode, body)
	}
	defer env.feishu.CloseCallback()
	// 授权页跳转落回 → 自动换 token（假上游成功脚本）→ 浏览器收成功页
	cb, err := http.Get(env.feishu.RedirectURIValue() + "?code=abc&state=mosaic")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	if cb.StatusCode != 200 {
		t.Fatalf("callback page: %d", cb.StatusCode)
	}
	// 设置状态面自动变绿
	resp, body = extRequest(t, "GET", env.ts.URL+"/v1/extdocs/feishu_docs/settings", nil, nil)
	if resp.StatusCode != 200 || body["authorized"] != true {
		t.Fatalf("settings: %d %v", resp.StatusCode, body)
	}
}

// TestPostMessageExternalRefs kind→host 校验矩阵 + 快照同形。
func TestPostMessageExternalRefs(t *testing.T) {
	env := newExtTestEnv(t, "", `{}`, nil)
	resp, created := postJSON(t, env.ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9401", "issued_at": "2026-09-26T12:00:00.000Z",
		"payload": map[string]any{"display_name": "外链房"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("create room: %d", resp.StatusCode)
	}
	roomID := created.RoomId
	post := func(version int, refs any) *http.Response {
		resp, _ := postJSON(t, env.ts.URL+"/v1/rooms/"+roomID+"/commands", map[string]any{
			"command_kind": "post_message", "expected_room_version": version,
			"idempotency_key": fmt.Sprintf("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d941%d", version), "issued_at": "2026-09-26T12:00:00.000Z",
			"payload": map[string]any{"body": "看这个", "external_refs": refs},
		})
		return resp
	}

	// fdoc + 飞书 host → 200
	if resp := post(1, []any{map[string]any{"kind": "fdoc", "url": "https://x.feishu.cn/wiki/n1", "title": "知识库页", "doc_type": "wiki"}}); resp.StatusCode != 200 {
		t.Fatalf("fdoc good: %d", resp.StatusCode)
	}
	// txdoc + 飞书 host → 400（kind↔host 不符）
	if resp := post(2, []any{map[string]any{"kind": "txdoc", "url": "https://x.feishu.cn/docx/a"}}); resp.StatusCode != 400 {
		t.Fatalf("txdoc feishu host: %d", resp.StatusCode)
	}
	// fdoc + 腾讯 host → 400
	if resp := post(2, []any{map[string]any{"kind": "fdoc", "url": "https://docs.qq.com/aio/x"}}); resp.StatusCode != 400 {
		t.Fatalf("fdoc qq host: %d", resp.StatusCode)
	}
	// fdoc 子域名 → 200（后缀匹配）
	if resp := post(2, []any{map[string]any{"kind": "fdoc", "url": "https://abc123.feishu.cn/docx/b"}}); resp.StatusCode != 200 {
		t.Fatalf("fdoc subdomain: %d", resp.StatusCode)
	}

	// 快照同形
	snapResp, err := http.Get(env.ts.URL + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer snapResp.Body.Close()
	var snap struct {
		Timeline []struct {
			ExternalRefs []struct {
				Kind    string `json:"kind"`
				URL     string `json:"url"`
				Title   string `json:"title"`
				DocType string `json:"doc_type"`
			} `json:"external_refs"`
		} `json:"timeline"`
	}
	if err := json.NewDecoder(snapResp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	var foundF, foundW bool
	for _, item := range snap.Timeline {
		for _, r := range item.ExternalRefs {
			if r.Kind == "fdoc" && r.URL == "https://x.feishu.cn/wiki/n1" && r.Title == "知识库页" && r.DocType == "wiki" {
				foundW = true
			}
			if r.Kind == "fdoc" && r.URL == "https://abc123.feishu.cn/docx/b" {
				foundF = true
			}
		}
	}
	if !foundF || !foundW {
		t.Fatalf("快照 external_refs 不符: %+v", snap.Timeline)
	}
}

// TestExtDocsUnavailable 未装配面 → 全端点 404。
func TestExtDocsUnavailable(t *testing.T) {
	ts := httptest.NewServer(New(Deps{}))
	t.Cleanup(ts.Close)
	for _, p := range []string{"/v1/extdocs/providers", "/v1/extdocs/tencent_docs/settings", "/v1/extdocs/shares", "/v1/extdocs/resolve", "/v1/extdocs/feishu/authorize"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s = %d（期望 404）", p, resp.StatusCode)
		}
	}
}

var _ = context.Background // 保留 context 导入（脚本扩展用）
