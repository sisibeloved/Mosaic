package httpapi

// 0.5.5 职责准入面（ADR-0022）：授权档案 GET/PUT、准入试算、blocked 重试
// 四端点 + AT-07 全链（必需缺失 blocked → 补授权 → retry ready → 激活；
// 选配降级有报告；显式越界版本拒绝）。测试先行（红→绿）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/slot"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// grantTestServer 身份 + 职责 + 真实准入门（GrantReadinessChecker）装配。
func grantTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-10-30T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_id" },
		Tenant: "ten_local",
	})
	var seq atomic.Int64
	ident := personalagent.New(personalagent.NewMemStore(), emptyIdentityRegistry{},
		personalagent.WithNewAgentID(func() string { return "ag_seq_" + strconv.FormatInt(seq.Add(1), 10)}),
		personalagent.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }))
	slotMem := slot.NewMemStore()
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Slots: slot.New(slotMem, slot.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
			slot.WithAgentLookup(identAgents{ident}),
			slot.WithReadiness(slot.NewGrantReadinessChecker(slotMem))),
		Identity: ident,
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

// demandingSpec 带全量要求域的模板（能力核对 × 权限交集全覆盖）。
const demandingSpec = `{
  "slot_spec_id": "slot.duty", "name": "值守", "responsibility": "r", "deliverables": ["d"],
  "skills": [{"ref": "workspace/skills/evidence-review", "version": 1}],
  "tools": {"required": ["documents.read"], "optional": ["web.search"]},
  "context": {"required": ["mission.contract"], "optional": []},
  "runtime": {"required": ["execute"], "workspace_access": "read_write"}
}`

const fullGrantBody = `{
  "skills": [{"ref": "workspace/skills/evidence-review", "version": 1}],
  "tools": ["documents.read"], "context": ["mission.contract"],
  "runtime": {"capabilities": ["execute"], "workspace_access": "read_write"}
}`

func grantFixture(t *testing.T, ts *httptest.Server) (instID, agentID string) {
	t.Helper()
	resp := postSlot(t, ts.URL+"/v1/slots/specs", demandingSpec)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建谱系: %d", resp.StatusCode)
	}
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.duty"}`)
	var inst struct {
		SlotInstanceID string `json:"slot_instance_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&inst)
	resp.Body.Close()
	resp = postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"值守 Agent"}`)
	var ag struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ag)
	resp.Body.Close()
	return inst.SlotInstanceID, ag.AgentID
}

func errCodeOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return doc.Error.Code
}

// TestSlotGrantEndpoints GET 未设置 404 grant_not_found → PUT 往返 → 非法
// 400 invalid_grant → 未知 Agent 404。
func TestSlotGrantEndpoints(t *testing.T) {
	ts := grantTestServer(t)
	_, agentID := grantFixture(t, ts)

	get, err := http.Get(ts.URL + "/v1/slots/grants/" + agentID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if get.StatusCode != http.StatusNotFound || errCodeOf(t, get) != "grant_not_found" {
		t.Fatalf("未设置档案应 404 grant_not_found: %d %s", get.StatusCode, errCodeOf(t, get))
	}

	put, err := http.NewRequest(http.MethodPut, ts.URL+"/v1/slots/grants/"+agentID, strings.NewReader(fullGrantBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	put.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	var view struct {
		AgentID  string   `json:"agent_id"`
		Tools    []string `json:"tools"`
		Runtime  struct {
			Capabilities    []string `json:"capabilities"`
			WorkspaceAccess string   `json:"workspace_access"`
		} `json:"runtime"`
		Skills []struct {
			Ref     string `json:"ref"`
			Version int    `json:"version"`
		} `json:"skills"`
		UpdatedAt string `json:"updated_at"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || view.AgentID != agentID || len(view.Tools) != 1 ||
		view.Runtime.WorkspaceAccess != "read_write" || view.Skills[0].Ref != "workspace/skills/evidence-review" ||
		view.UpdatedAt == "" {
		t.Fatalf("PUT 往返失真: %d %+v", resp.StatusCode, view)
	}

	// 读取已设置档案
	get2, _ := http.Get(ts.URL + "/v1/slots/grants/" + agentID)
	if get2.StatusCode != http.StatusOK {
		t.Fatalf("已设置档案 GET: %d", get2.StatusCode)
	}
	get2.Body.Close()

	// 非法档案：工具名模式 + 权限枚举
	for _, bad := range []string{
		`{"skills":[],"tools":["Bad Name"],"context":[],"runtime":{"capabilities":[],"workspace_access":"none"}}`,
		`{"skills":[],"tools":[],"context":[],"runtime":{"capabilities":[],"workspace_access":"admin"}}`,
		`{"skills":[{"ref":"x","version":0}],"tools":[],"context":[],"runtime":{"capabilities":[],"workspace_access":"none"}}`,
	} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/slots/grants/"+agentID, strings.NewReader(bad))
		req.Header.Set("Content-Type", "application/json")
		r, _ := http.DefaultClient.Do(req)
		if r.StatusCode != http.StatusBadRequest || errCodeOf(t, r) != "invalid_grant" {
			t.Fatalf("非法档案应 400 invalid_grant: %d %s（%s）", r.StatusCode, errCodeOf(t, r), bad)
		}
	}

	// 未知 Agent
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/slots/grants/ag_none", strings.NewReader(fullGrantBody))
	req.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusNotFound || errCodeOf(t, r) != "agent_not_found" {
		t.Fatalf("未知 Agent 应 404 agent_not_found: %d %s", r.StatusCode, errCodeOf(t, r))
	}
}

// TestSlotReadinessEndpoint 试算：未就绪 200 报告（缺失/降级/有效权限）→
// 补授权 200 ready；版本缺省最新 / 显式钉住 / 0 与 -1 → 400 / 越界 409。
func TestSlotReadinessEndpoint(t *testing.T) {
	ts := grantTestServer(t)
	instID, agentID := grantFixture(t, ts)

	probe := func(query string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/slots/instances/" + instID + "/readiness?" + query)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		defer resp.Body.Close()
		var doc map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		return resp.StatusCode, doc
	}
	q := url.Values{}
	q.Set("agent_id", agentID)

	// 未授权：200（报告不是错误）+ 缺失清单
	code, doc := probe(q.Encode())
	if code != http.StatusOK {
		t.Fatalf("未就绪试算应 200: %d", code)
	}
	if doc["ready"] != false {
		t.Fatalf("应 not ready: %v", doc["ready"])
	}
	missing, _ := doc["missing"].([]any)
	joined := ""
	for _, m := range missing {
		joined += m.(string)
	}
	for _, want := range []string{"tool:documents.read", "skill:workspace/skills/evidence-review", "runtime:execute", "workspace_access:read_write"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺失清单缺 %s（%v）", want, missing)
		}
	}
	if doc["effective_workspace_access"] != "none" {
		t.Fatalf("有效权限应 none: %v", doc["effective_workspace_access"])
	}
	degraded, _ := doc["degraded"].([]any)
	if len(degraded) == 0 || !strings.Contains(degraded[0].(string), "web.search") {
		t.Fatalf("选配缺失应记降级: %v", degraded)
	}

	// 补授权 → ready（选配仍缺 → 降级仍在报告）
	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/slots/grants/"+agentID, strings.NewReader(fullGrantBody))
	put.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(put)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("补授权: %d", r.StatusCode)
	}
	code, doc = probe(q.Encode())
	if code != http.StatusOK || doc["ready"] != true {
		t.Fatalf("补授权后应 ready: %d %v", code, doc["ready"])
	}
	if doc["slot_spec_version"].(float64) != 1 || doc["effective_workspace_access"] != "read_write" {
		t.Fatalf("报告上下文失真: %v", doc)
	}

	// 追加 v2（+必需工具）→ 缺省试算钉 v2 缺失；显式 v1 钉旧版 ready
	resp := postSlot(t, ts.URL+"/v1/slots/specs/slot.duty/versions", `{
	  "name": "值守 v2", "responsibility": "r", "deliverables": ["d"],
	  "tools": {"required": ["documents.read", "reports.export"], "optional": []},
	  "runtime": {"required": ["execute"], "workspace_access": "read_write"}}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("追加 v2: %d", resp.StatusCode)
	}
	code, doc = probe(q.Encode())
	if code != http.StatusOK || doc["slot_spec_version"].(float64) != 2 || doc["ready"] != false {
		t.Fatalf("缺省试算应钉 v2 not ready: %d %v", code, doc)
	}
	q2 := url.Values{}
	q2.Set("agent_id", agentID)
	q2.Set("slot_spec_version", "1")
	code, doc = probe(q2.Encode())
	if code != http.StatusOK || doc["slot_spec_version"].(float64) != 1 || doc["ready"] != true {
		t.Fatalf("显式 v1 应钉旧版 ready: %d %v", code, doc)
	}

	// 显式非法版本：0 / -1 → 400；越界 → 409
	for _, v := range []string{"0", "-1"} {
		q3 := url.Values{}
		q3.Set("agent_id", agentID)
		q3.Set("slot_spec_version", v)
		resp, err := http.Get(ts.URL + "/v1/slots/instances/" + instID + "/readiness?" + q3.Encode())
		if err != nil {
			t.Fatalf("probe %s: %v", v, err)
		}
		if resp.StatusCode != http.StatusBadRequest || errCodeOf(t, resp) != "invalid_binding" {
			t.Fatalf("显式 %s 应 400 invalid_binding: %d %s", v, resp.StatusCode, errCodeOf(t, resp))
		}
	}
	q4 := url.Values{}
	q4.Set("agent_id", agentID)
	q4.Set("slot_spec_version", "99")
	resp5, _ := http.Get(ts.URL + "/v1/slots/instances/" + instID + "/readiness?" + q4.Encode())
	if resp5.StatusCode != http.StatusConflict || errCodeOf(t, resp5) != "invalid_spec_version" {
		t.Fatalf("越界版本应 409 invalid_spec_version: %d %s", resp5.StatusCode, errCodeOf(t, resp5))
	}

	// 未知实例 / 未知 Agent → 404
	resp6, _ := http.Get(ts.URL + "/v1/slots/instances/si_none/readiness?agent_id=" + agentID)
	if resp6.StatusCode != http.StatusNotFound || errCodeOf(t, resp6) != "instance_not_found" {
		t.Fatalf("未知实例应 404: %d %s", resp6.StatusCode, errCodeOf(t, resp6))
	}
	resp7, _ := http.Get(ts.URL + "/v1/slots/instances/" + instID + "/readiness?agent_id=ag_none")
	if resp7.StatusCode != http.StatusNotFound || errCodeOf(t, resp7) != "agent_not_found" {
		t.Fatalf("未知 Agent 应 404: %d %s", resp7.StatusCode, errCodeOf(t, resp7))
	}
}

// TestSlotBindingRetryEndpoint AT-07 全链：无授权 prepare → blocked（事件含
// missing/degraded）→ 仍未授权 retry → blocked → 补授权 → retry → ready →
// 激活；非 blocked 重试 409；未知 404。
func TestSlotBindingRetryEndpoint(t *testing.T) {
	ts := grantTestServer(t)
	instID, agentID := grantFixture(t, ts)

	// 无授权 → blocked
	resp := postSlot(t, ts.URL+"/v1/slots/bindings", `{"slot_instance_id":"`+instID+`","agent_id":"`+agentID+`"}`)
	var b struct {
		BindingID string `json:"binding_id"`
		Status    string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&b)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || b.Status != "blocked" {
		t.Fatalf("无授权建任职应 blocked: %d %s", resp.StatusCode, b.Status)
	}

	// blocked 事件含 missing + degraded
	evResp, _ := http.Get(ts.URL + "/v1/slots/bindings/" + b.BindingID + "/events")
	var evDoc struct {
		Events []struct {
			Payload map[string]any `json:"payload"`
		} `json:"events"`
	}
	_ = json.NewDecoder(evResp.Body).Decode(&evDoc)
	evResp.Body.Close()
	last := evDoc.Events[len(evDoc.Events)-1]
	if last.Payload["to"] != "blocked" || last.Payload["missing"] == nil || last.Payload["degraded"] == nil {
		t.Fatalf("blocked 事件应含 missing+degraded: %+v", last.Payload)
	}

	// 仍未授权重试 → 仍 blocked
	retry := postSlot(t, ts.URL+"/v1/slots/bindings/"+b.BindingID+"/retry", `{}`)
	var rb struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(retry.Body).Decode(&rb)
	retry.Body.Close()
	if retry.StatusCode != http.StatusOK || rb.Status != "blocked" {
		t.Fatalf("未补授权重试应仍 blocked: %d %s", retry.StatusCode, rb.Status)
	}

	// 补授权 → retry → ready
	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/slots/grants/"+agentID, strings.NewReader(fullGrantBody))
	put.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(put)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("补授权: %d", r.StatusCode)
	}
	retry2 := postSlot(t, ts.URL+"/v1/slots/bindings/"+b.BindingID+"/retry", `{}`)
	_ = json.NewDecoder(retry2.Body).Decode(&rb)
	retry2.Body.Close()
	if retry2.StatusCode != http.StatusOK || rb.Status != "ready" {
		t.Fatalf("补授权后重试应 ready: %d %s", retry2.StatusCode, rb.Status)
	}

	// 激活闭环
	act := postSlot(t, ts.URL+"/v1/slots/bindings/"+b.BindingID+"/activate", `{}`)
	var ab struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(act.Body).Decode(&ab)
	act.Body.Close()
	if act.StatusCode != http.StatusOK || ab.Status != "active" {
		t.Fatalf("激活: %d %s", act.StatusCode, ab.Status)
	}

	// 非 blocked 重试 → 409 invalid_transition
	retry3 := postSlot(t, ts.URL+"/v1/slots/bindings/"+b.BindingID+"/retry", `{}`)
	if retry3.StatusCode != http.StatusConflict || errCodeOf(t, retry3) != "invalid_transition" {
		t.Fatalf("active 重试应 409: %d %s", retry3.StatusCode, errCodeOf(t, retry3))
	}
	// 未知任职 → 404
	retry4 := postSlot(t, ts.URL+"/v1/slots/bindings/bd_none/retry", `{}`)
	if retry4.StatusCode != http.StatusNotFound || errCodeOf(t, retry4) != "binding_not_found" {
		t.Fatalf("未知任职重试应 404: %d %s", retry4.StatusCode, errCodeOf(t, retry4))
	}
}
