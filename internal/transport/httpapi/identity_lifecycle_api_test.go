package httpapi

// 0.5.1 身份写面（ADR-0018）：手动建身份、生命周期转移端点（suspend/resume/
// retire + 409/404 语义）、事件流只读端点、写门（跨源/类型拒）。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// lifecycleTestServer 身份写面测试件：序号 AgentID 生成器（手动建与迁移建
// 不碰撞）+ 真实 harness 注册表（miniRunner 探测面）。
func lifecycleTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-10-29T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_id" },
		Tenant: "ten_local",
	})
	reg, err := harness.LoadOrCreate(t.TempDir() + "/harness.json")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	var seq atomic.Int64
	ident := personalagent.New(personalagent.NewMemStore(), personalagent.NewRegistryAdapter(reg),
		personalagent.WithNewAgentID(func() string { return "ag_seq_" + jsonItoa(seq.Add(1)) }),
		personalagent.WithNow(func() string { return "2026-10-29T12:00:00.000Z" }))
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:       room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Harness:     reg,
		ProbeRunner: miniRunner{},
		Identity:    ident,
		Seats: func() []room.AgentSeat {
			return []room.AgentSeat{{
				ParticipantID: "par_echo",
				Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
			}}
		},
	}))
	t.Cleanup(ts.Close)
	return ts
}

func jsonItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func postIdentity(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestIdentityLifecycleEndpoints 全转移链 + 409/404 + 事件流形状。
func TestIdentityLifecycleEndpoints(t *testing.T) {
	ts := lifecycleTestServer(t)

	// 迁移来源身份（手动登记 codex → 身份列表读取触发惰性同步——真实用户
	// 流程：UI 先列身份再操作）
	add := postIdentity(t, ts.URL+"/v1/harness/executables", `{"adapter":"codex","runtime":"native","path":"/opt/tools/codex"}`)
	add.Body.Close()
	if add.StatusCode != http.StatusCreated {
		t.Fatalf("登记 status=%d", add.StatusCode)
	}
	sync, err := http.Get(ts.URL + "/v1/identity/agents")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sync.Body.Close()
	if sync.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", sync.StatusCode)
	}

	// suspend → suspended
	var res struct {
		Agent struct {
			AgentID string `json:"agent_id"`
			Status  string `json:"status"`
		} `json:"agent"`
		Outstanding []string `json:"outstanding_commitments"`
	}
	resp := postIdentity(t, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle", `{"action":"suspend","reason":"休假"}`)
	decodeInto(t, resp, &res)
	if resp.StatusCode != http.StatusOK || res.Agent.Status != "suspended" || len(res.Outstanding) != 0 {
		t.Fatalf("suspend: status=%d %+v", resp.StatusCode, res)
	}
	// 重放 suspend → 409 invalid_transition（不静默幂等）
	resp = postIdentity(t, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle", `{"action":"suspend"}`)
	var errDoc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeInto(t, resp, &errDoc)
	if resp.StatusCode != http.StatusConflict || errDoc.Error.Code != "invalid_transition" {
		t.Fatalf("重放 suspend: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}
	// resume → active
	resp = postIdentity(t, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle", `{"action":"resume"}`)
	decodeInto(t, resp, &res)
	if resp.StatusCode != http.StatusOK || res.Agent.Status != "active" {
		t.Fatalf("resume: status=%d %+v", resp.StatusCode, res)
	}
	// retire → retired（0.5.1 承诺门空，同调用完成）
	resp = postIdentity(t, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle", `{"action":"retire"}`)
	decodeInto(t, resp, &res)
	if resp.StatusCode != http.StatusOK || res.Agent.Status != "retired" {
		t.Fatalf("retire: status=%d %+v", resp.StatusCode, res)
	}
	// 终态再动 → 409
	resp = postIdentity(t, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle", `{"action":"suspend"}`)
	decodeInto(t, resp, &errDoc)
	if resp.StatusCode != http.StatusConflict || errDoc.Error.Code != "invalid_transition" {
		t.Fatalf("retired 后 suspend: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}
	// 未知身份 → 404
	resp = postIdentity(t, ts.URL+"/v1/identity/agents/ag_missing/lifecycle", `{"action":"suspend"}`)
	decodeInto(t, resp, &errDoc)
	if resp.StatusCode != http.StatusNotFound || errDoc.Error.Code != "identity_not_found" {
		t.Fatalf("未知身份: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}
	// 非法动作 → 400
	resp = postIdentity(t, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle", `{"action":"bogus"}`)
	decodeInto(t, resp, &errDoc)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法动作: status=%d", resp.StatusCode)
	}

	// 事件流：created + suspend/resume/retiring/retired（retire 两段事件），
	// seq 升序，payload 语义正确
	evResp, err := http.Get(ts.URL + "/v1/identity/agents/ag_seq_1/events")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var evDoc struct {
		Events []struct {
			Seq       int64          `json:"seq"`
			EventType string         `json:"event_type"`
			Payload   map[string]any `json:"payload"`
		} `json:"events"`
	}
	decodeInto(t, evResp, &evDoc)
	if evResp.StatusCode != http.StatusOK || len(evDoc.Events) != 5 {
		t.Fatalf("events: status=%d n=%d", evResp.StatusCode, len(evDoc.Events))
	}
	wantTypes := []string{"created", "status_changed", "status_changed", "status_changed", "status_changed"}
	for i, ev := range evDoc.Events {
		if ev.EventType != wantTypes[i] {
			t.Fatalf("事件#%d = %s, want %s", i, ev.EventType, wantTypes[i])
		}
		if i > 0 && ev.Seq <= evDoc.Events[i-1].Seq {
			t.Fatalf("seq 非升序: %v", evDoc.Events)
		}
	}
	if evDoc.Events[0].Payload["source"] != "migration" {
		t.Fatalf("created payload: %v", evDoc.Events[0].Payload)
	}
	sus := evDoc.Events[1].Payload
	if sus["from"] != "active" || sus["to"] != "suspended" || sus["reason"] != "休假" {
		t.Fatalf("suspend payload: %v", sus)
	}
	retiring := evDoc.Events[3].Payload
	if retiring["from"] != "active" || retiring["to"] != "retiring" {
		t.Fatalf("retiring payload: %v", retiring)
	}
	ret := evDoc.Events[4].Payload
	if ret["from"] != "retiring" || ret["to"] != "retired" {
		t.Fatalf("retired payload: %v", ret)
	}
	// 未知身份事件流 → 404
	miss, err := http.Get(ts.URL + "/v1/identity/agents/ag_missing/events")
	if err != nil {
		t.Fatalf("events404: %v", err)
	}
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("events404 status=%d", miss.StatusCode)
	}
}

// TestIdentityCreateAgentEndpoint 手动建：201 + manual/active 形态；空名 400；
// 与迁移身份互不合并。
func TestIdentityCreateAgentEndpoint(t *testing.T) {
	ts := lifecycleTestServer(t)

	resp := postIdentity(t, ts.URL+"/v1/identity/agents", `{"display_name":"长期助手"}`)
	var created struct {
		AgentID     string `json:"agent_id"`
		DisplayName string `json:"display_name"`
		Adapter     string `json:"adapter"`
		Status      string `json:"status"`
		Source      string `json:"source"`
	}
	decodeInto(t, resp, &created)
	if resp.StatusCode != http.StatusCreated || created.Source != "manual" || created.Status != "active" ||
		created.DisplayName != "长期助手" || !strings.HasPrefix(created.AgentID, "ag_") {
		t.Fatalf("manual 建: status=%d %+v", resp.StatusCode, created)
	}
	if created.Adapter != "" {
		t.Fatalf("无 adapter 挂靠应为空: %q", created.Adapter)
	}

	// 空名 → 400
	bad := postIdentity(t, ts.URL+"/v1/identity/agents", `{"display_name":"  "}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("空名 status=%d, want 400", bad.StatusCode)
	}

	// 手动身份随后可见且不被同步改写
	add := postIdentity(t, ts.URL+"/v1/harness/executables", `{"adapter":"kimi","runtime":"native","path":"/opt/tools/kimi"}`)
	add.Body.Close()
	list, err := http.Get(ts.URL + "/v1/identity/agents")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var doc struct {
		Agents []struct {
			AgentID string `json:"agent_id"`
			Source  string `json:"source"`
			Status  string `json:"status"`
		} `json:"agents"`
	}
	decodeInto(t, list, &doc)
	if len(doc.Agents) != 2 {
		t.Fatalf("agents=%d, want 2", len(doc.Agents))
	}
	sources := map[string]string{}
	for _, a := range doc.Agents {
		sources[a.Source] = a.AgentID
	}
	if sources["manual"] != created.AgentID || sources["migration"] == "" || sources["migration"] == created.AgentID {
		t.Fatalf("manual 与迁移身份应互不合并: %v", sources)
	}
}

// TestIdentityWriteGates 身份写面过生产写门：跨源 Origin 拒（403）、
// 非 JSON Content-Type 拒（415）；读面不受影响。
func TestIdentityWriteGates(t *testing.T) {
	ts := lifecycleTestServer(t)
	add := postIdentity(t, ts.URL+"/v1/harness/executables", `{"adapter":"codex","runtime":"native","path":"/opt/tools/codex"}`)
	add.Body.Close()

	// 跨源 Origin → 403
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle",
		strings.NewReader(`{"action":"suspend"}`))
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("跨源 post: %v", err)
	}
	var errDoc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeInto(t, resp, &errDoc)
	if resp.StatusCode != http.StatusForbidden || errDoc.Error.Code != "origin_rejected" {
		t.Fatalf("跨源: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}

	// 非 JSON 类型 → 415（对 lifecycle 与 manual 建同口径）
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/agents/ag_seq_1/lifecycle",
		strings.NewReader(`action=suspend`))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("表单 post: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("表单类型 status=%d, want 415", resp2.StatusCode)
	}
	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/identity/agents",
		strings.NewReader(`display_name=x`))
	req3.Header.Set("Content-Type", "text/plain")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("文本 post: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("manual 建文本类型 status=%d, want 415", resp3.StatusCode)
	}

	// 读面不受门影响
	get, err := http.Get(ts.URL + "/v1/identity/agents")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	raw, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if get.StatusCode != http.StatusOK || !strings.Contains(string(raw), "ag_seq_1") {
		t.Fatalf("读面被误伤: status=%d", get.StatusCode)
	}
}
