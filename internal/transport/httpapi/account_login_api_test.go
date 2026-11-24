// zcode 账号登录端点 UT（独立登录档的设置页入口）：状态查询 / 异步发起 / 过程 SSE。
// 后端以桩替身（真实登录任务在 zcode 适配器包 UT + 真机验证）——此处验端点接线：
// 写门与请求体纪律、受理状态语义、SSE 事件转发与关流时机、未接入登录面的降级。
package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// fakeAccountLogin 登录任务面桩：固定状态；Start 首次受理并建任务通道（在途幂等）；
// 测试经 push 向在订连接注入事件。
type fakeAccountLogin struct {
	mu         sync.Mutex
	status     AccountLoginStatus
	supported  bool
	starts     int
	lastFamily string
	startErr   error
	job        chan AccountLoginEvent
}

func (f *fakeAccountLogin) AccountLoginStatus(string) (AccountLoginStatus, bool) {
	return f.status, f.supported
}

func (f *fakeAccountLogin) StartAccountLogin(_ context.Context, _ string, family string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.lastFamily = family
	if f.startErr != nil {
		return false, f.startErr
	}
	if f.job == nil {
		f.job = make(chan AccountLoginEvent, 16)
		return true, nil
	}
	return false, nil
}

func (f *fakeAccountLogin) SubscribeAccountLogin(string) (events <-chan AccountLoginEvent, cancel func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.job == nil {
		return nil, nil, false
	}
	return f.job, func() {}, true
}

func (f *fakeAccountLogin) push(evs ...AccountLoginEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range evs {
		f.job <- ev
	}
}

// newAccountLoginTestServer 登记一个 zcode 与一个 codex 实例，返回服务与两者 id。
func newAccountLoginTestServer(t *testing.T, backend AccountLoginBackend) (ts *httptest.Server, zcodeID, codexID string) {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-11-24T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_al" },
		Tenant: "ten_local",
	})
	reg, err := harness.LoadOrCreate(t.TempDir() + "/harness.json")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ts = httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:   room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Harness: reg, ProbeRunner: miniRunner{},
		AccountLogin: backend,
	}))
	t.Cleanup(ts.Close)
	for _, add := range []struct{ adapter, path string }{{"zcode", "/opt/tools/zcode"}, {"codex", "/opt/tools/codex"}} {
		resp, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
			strings.NewReader(`{"adapter":"`+add.adapter+`","runtime":"native","path":"`+add.path+`"}`))
		if err != nil {
			t.Fatalf("add %s: %v", add.adapter, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("add %s status = %d", add.adapter, resp.StatusCode)
		}
	}
	list, _ := http.Get(ts.URL + "/v1/harness/executables")
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	for _, exe := range doc.Executables {
		switch exe.Adapter {
		case "zcode":
			zcodeID = exe.ID
		case "codex":
			codexID = exe.ID
		}
	}
	if zcodeID == "" || codexID == "" {
		t.Fatal("登记后应同时存在 zcode 与 codex 实例")
	}
	return ts, zcodeID, codexID
}

func accountLoginBase(ts *httptest.Server, id, suffix string) string {
	return ts.URL + "/v1/harness/executables/" + url.PathEscape(id) + "/account-login" + suffix
}

// TestAccountLoginStatusEndpoint：状态查询——支持且已登录的实例透出账号/模型/
// 账号 provider；不支持的实例同样 200（supported=false，providers 非 null——界面据此
// 不呈现登录块）；不存在 404。
func TestAccountLoginStatusEndpoint(t *testing.T) {
	backend := &fakeAccountLogin{supported: true, status: AccountLoginStatus{
		Supported: true, LoggedIn: true, Account: "alice", Model: "account:bigmodel-individual-coding-plan/GLM-5.4",
		Providers: []string{"account:bigmodel-individual-coding-plan"},
	}}
	ts, zcodeID, codexID := newAccountLoginTestServer(t, backend)

	var st AccountLoginStatus
	if err := fetchLoginStatus(accountLoginBase(ts, zcodeID, ""), &st); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Supported || !st.LoggedIn || st.Account != "alice" || len(st.Providers) != 1 {
		t.Fatalf("已登录状态应透出：%+v", st)
	}
	backend.supported = false
	if err := fetchLoginStatus(accountLoginBase(ts, codexID, ""), &st); err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Supported || st.LoggedIn || st.Providers == nil || len(st.Providers) != 0 {
		t.Fatalf("不支持的实例应 supported=false 且 providers 为空数组（非 null）：%+v", st)
	}
	resp, err := http.Get(accountLoginBase(ts, "nope", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的实例 = %d，期望 404", resp.StatusCode)
	}
}

// TestAccountLoginStartEndpoint：发起登录——202 started；在途再发 202 running（幂等）；
// 请求体家族透传（缺省空 = bigmodel）；家族不合法/未知字段 400；非 JSON 带体 415；
// 后端不支持 409 account_login_unsupported。
func TestAccountLoginStartEndpoint(t *testing.T) {
	backend := &fakeAccountLogin{supported: true}
	ts, zcodeID, _ := newAccountLoginTestServer(t, backend)
	target := accountLoginBase(ts, zcodeID, "")

	post := func(ct, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(raw)
	}
	if code, body := post("application/json", `{"family":"zai"}`); code != http.StatusAccepted || !strings.Contains(body, `"started"`) {
		t.Fatalf("发起 = %d %s，期望 202 started", code, body)
	}
	if backend.lastFamily != "zai" {
		t.Fatalf("家族应透传：%q", backend.lastFamily)
	}
	if code, body := post("application/json", `{}`); code != http.StatusAccepted || !strings.Contains(body, `"running"`) {
		t.Fatalf("在途再发 = %d %s，期望 202 running", code, body)
	}
	if code, body := post("", ""); code != http.StatusAccepted || !strings.Contains(body, `"running"`) {
		t.Fatalf("空体（无 Content-Type）应受理：%d %s", code, body)
	}
	if backend.lastFamily != "" {
		t.Fatalf("空体应缺省家族（空串）：%q", backend.lastFamily)
	}
	for _, tc := range []struct {
		name, ct, body string
		want           int
	}{
		{"家族不合法", "application/json", `{"family":"openai"}`, http.StatusBadRequest},
		{"未知字段", "application/json", `{"family":"zai","extra":1}`, http.StatusBadRequest},
		{"畸形 JSON", "application/json", `{"family":`, http.StatusBadRequest},
		{"非 JSON 带体", "text/plain", `family=zai`, http.StatusUnsupportedMediaType},
	} {
		if code, _ := post(tc.ct, tc.body); code != tc.want {
			t.Fatalf("%s = %d，期望 %d", tc.name, code, tc.want)
		}
	}
	backend.startErr = ErrAccountLoginUnsupported
	if code, body := post("application/json", ``); code != http.StatusConflict || !strings.Contains(body, "account_login_unsupported") {
		t.Fatalf("不支持 = %d %s，期望 409 account_login_unsupported", code, body)
	}
}

// TestAccountLoginStreamEndpoint：过程 SSE——授权链接、过程行、终态按序转发，终态后
// 关流；无任务发 idle 即关；不存在的实例 404（JSON，不进流）。
func TestAccountLoginStreamEndpoint(t *testing.T) {
	backend := &fakeAccountLogin{supported: true}
	ts, zcodeID, _ := newAccountLoginTestServer(t, backend)
	target := accountLoginBase(ts, zcodeID, "/stream")

	if evs := readLoginStream(t, target); len(evs) != 1 || evs[0] != `{"kind":"idle"}` {
		t.Fatalf("无任务应发 idle 即关：%v", evs)
	}
	resp, err := http.Post(accountLoginBase(ts, zcodeID, ""), "application/json", strings.NewReader(`{"family":"bigmodel"}`))
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: %v %v", err, resp)
	}
	resp.Body.Close()

	// 订阅建链与注入并发：任务通道带缓冲——事件先入通道再订阅同样经通道送达，
	// 无须对时序（真实任务面另有重放语义，见 zcode 适配器包 UT）。
	got := make(chan []string, 1)
	go func() {
		got <- readLoginStream(t, target)
	}()
	backend.push(
		AccountLoginEvent{Kind: "url", URL: "https://bigmodel.cn/login?appId=zcode&state=s"},
		AccountLoginEvent{Kind: "line", Line: "Login successful as bob."},
		AccountLoginEvent{Kind: "done", Account: "bob", Model: "account:x/GLM-5.4"},
	)
	select {
	case evs := <-got:
		want := []AccountLoginEvent{
			{Kind: "url", URL: "https://bigmodel.cn/login?appId=zcode&state=s"},
			{Kind: "line", Line: "Login successful as bob."},
			{Kind: "done", Account: "bob", Model: "account:x/GLM-5.4"},
		}
		if len(evs) != len(want) {
			t.Fatalf("事件数 = %d，期望 %d：%v", len(evs), len(want), evs)
		}
		for i := range want {
			var got AccountLoginEvent
			if err := json.Unmarshal([]byte(evs[i]), &got); err != nil {
				t.Fatalf("事件 %d 不是合法 JSON：%s（%v）", i, evs[i], err)
			}
			if got != want[i] {
				t.Fatalf("事件 %d = %+v，期望 %+v", i, got, want[i])
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SSE 未在终态后关流")
	}
	resp2, err := http.Get(accountLoginBase(ts, "nope", "/stream"))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在的实例 = %d，期望 404", resp2.StatusCode)
	}
}

// TestAccountLoginWithoutBackend：未接入登录面的装配——状态 supported=false（200）、
// 发起 503、流 idle 即关（界面不呈现登录块，不报错）。
func TestAccountLoginWithoutBackend(t *testing.T) {
	ts, zcodeID, _ := newAccountLoginTestServer(t, nil)
	var st AccountLoginStatus
	if err := fetchLoginStatus(accountLoginBase(ts, zcodeID, ""), &st); err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Supported || st.Providers == nil {
		t.Fatalf("未接入应 supported=false 且 providers 非 nil：%+v", st)
	}
	resp, err := http.Post(accountLoginBase(ts, zcodeID, ""), "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未接入发起 = %d，期望 503", resp.StatusCode)
	}
	if evs := readLoginStream(t, accountLoginBase(ts, zcodeID, "/stream")); len(evs) != 1 || evs[0] != `{"kind":"idle"}` {
		t.Fatalf("未接入应发 idle 即关：%v", evs)
	}
}

// fetchLoginStatus 读状态端点并解码（非 200 时返回带状态码的错误）。
func fetchLoginStatus(target string, out *AccountLoginStatus) error {
	resp, err := http.Get(target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return errors.New(resp.Status + " " + string(raw))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readLoginStream 读尽一条登录事件流，返回各事件的 data 载荷（服务端在终态/idle
// 后关流）。
func readLoginStream(t *testing.T, target string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}
	var out []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}
