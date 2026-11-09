// bundle_api_test.go —— 0.7.5 / ADR-0042：Portable Agent Bundle 导出三端点。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/bundle"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func newBundleTestServer(t *testing.T, ownerToken string, withSvc bool) (*httptest.Server, *fakeBundlePorts) {
	t.Helper()
	fp := &fakeBundlePorts{
		agents: map[string]bundle.AgentFacts{"ag_ok": {
			AgentID: "ag_ok", DisplayName: "小助", Adapter: "codex", Status: "active",
		}},
		missions: map[string][]bundle.MissionFact{"ag_ok": {{
			MissionID: "mi_a", ContractVersion: 1, Status: "running", OwnerAgentID: "ag_ok",
		}}},
		memories: map[string][]bundle.MemoryFact{
			"agent|ag_ok": {
				{MemoryID: "mem_n", Namespace: "agent", ScopeID: "ag_ok", Status: "active", Sensitivity: "normal"},
				{MemoryID: "mem_s", Namespace: "agent", ScopeID: "ag_ok", Status: "active", Sensitivity: "sensitive"},
			},
			"workspace|default": {
				{MemoryID: "mem_w", Namespace: "workspace", ScopeID: "default", Status: "active", Sensitivity: "normal"},
			},
		},
	}
	deps := Deps{
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: ownerToken,
	}
	if withSvc {
		deps.BundleSVC = bundle.New(fp.ports(), t.TempDir(), "0.7.5-dev",
			bundle.WithNow(func() string { return "2026-11-08T12:00:00Z" }),
			bundle.WithNewID(func() string { return "bdl_api01" }))
	}
	handler := New(deps)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, fp
}

type fakeBundlePorts struct {
	agents   map[string]bundle.AgentFacts
	missions map[string][]bundle.MissionFact
	memories map[string][]bundle.MemoryFact
}

func (f *fakeBundlePorts) ports() bundle.Ports {
	return bundle.Ports{
		Agent: func(_ context.Context, id string) (bundle.AgentFacts, bool, error) {
			a, ok := f.agents[id]
			return a, ok, nil
		},
		Slots: func(_ context.Context, _ string) ([]bundle.SlotFact, error) { return nil, nil },
		Missions: func(_ context.Context, id string) ([]bundle.MissionFact, error) {
			return f.missions[id], nil
		},
		Checkpoint: func(_ context.Context, _ string) (bundle.CheckpointFact, bool, error) {
			return bundle.CheckpointFact{}, false, nil
		},
		Memories: func(_ context.Context, ns, scope string) ([]bundle.MemoryFact, error) {
			return f.memories[ns+"|"+scope], nil
		},
	}
}

func bundlePost(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func bundleGet(url, token string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Owner-Token", token)
	return http.DefaultClient.Do(req)
}

func TestBundleExportEndpoints(t *testing.T) {
	ts, _ := newBundleTestServer(t, "tok", true)

	// 创建：默认范围 → 201 + 脱敏报告内联（敏感省略计数）。
	resp := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var view map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view["bundle_id"] != "bdl_api01" {
		t.Fatalf("bundle_id: %v", view["bundle_id"])
	}
	red, _ := view["redaction"].(map[string]any)
	om, _ := red["omitted_sensitive"].(map[string]any)
	if om["count"].(float64) != 1 {
		t.Fatalf("敏感省略计数: %v", om)
	}
	scope, _ := view["scope"].(map[string]any)
	if scope["mission_selection"] != "auto" {
		t.Fatalf("mission_selection: %v", scope)
	}
	ids, _ := scope["mission_ids"].([]any)
	if len(ids) != 1 || ids[0] != "mi_a" {
		t.Fatalf("自动圈定展开: %v", ids)
	}

	// 列表：新→旧摘要（GET 亦在 token 门内——与 checkpoint 读端点同口径）。
	lresp, err := bundleGet(ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok")
	if err != nil || lresp.StatusCode != http.StatusOK {
		t.Fatalf("list: %v %d", err, lresp.StatusCode)
	}
	t.Cleanup(func() { lresp.Body.Close() })
	var lv map[string]any
	_ = json.NewDecoder(lresp.Body).Decode(&lv)
	exports, _ := lv["exports"].([]any)
	if len(exports) != 1 {
		t.Fatalf("exports: %v", exports)
	}

	// 详情。
	dresp, err := bundleGet(ts.URL+"/v1/bundle-exports/bdl_api01", "tok")
	if err != nil || dresp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %v %d", err, dresp.StatusCode)
	}
	t.Cleanup(func() { dresp.Body.Close() })
	var dv map[string]any
	_ = json.NewDecoder(dresp.Body).Decode(&dv)
	if dv["bundle_id"] != "bdl_api01" {
		t.Fatalf("detail bundle_id: %v", dv["bundle_id"])
	}
}

func TestBundleExportValidationAndGates(t *testing.T) {
	ts, _ := newBundleTestServer(t, "tok", true)

	cases := []struct {
		name, body string
		want       int
	}{
		{"未知命名空间", `{"namespaces":["agent","global"]}`, 400},
		{"room 不开放", `{"namespaces":["room"]}`, 400},
		{"显式任务非所属", `{"mission_ids":["mi_ghost"]}`, 400},
	}
	for _, tc := range cases {
		resp := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", json.RawMessage(tc.body))
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: got %d want %d", tc.name, resp.StatusCode, tc.want)
		}
	}

	// Agent 不存在。
	resp := bundlePost(t, ts.URL+"/v1/agents/ag_ghost/bundle-exports", "tok", map[string]any{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("agent 404: %d", resp.StatusCode)
	}

	// 写门：无 token 401。
	resp = bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "", map[string]any{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token 门: %d", resp.StatusCode)
	}

	// 详情不存在。
	dresp, err := bundleGet(ts.URL+"/v1/bundle-exports/bdl_nope", "tok")
	if err != nil || dresp.StatusCode != http.StatusNotFound {
		t.Fatalf("detail 404: %v %d", err, dresp.StatusCode)
	}
	t.Cleanup(func() { dresp.Body.Close() })
}

func TestBundleExportUnavailable(t *testing.T) {
	ts, _ := newBundleTestServer(t, "tok", false)
	resp := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", map[string]any{})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未装配 503: %d", resp.StatusCode)
	}
}
