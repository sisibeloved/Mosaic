package httpapi

// 0.5.2 职责模板面（ADR-0019）：SlotSpec 谱系建/追加/查询端点 + 409/404 +
// 校验 400 + 写门。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/slot"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

func slotTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-10-29T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_id" },
		Tenant: "ten_local",
	})
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Slots: slot.New(slot.NewMemStore(), slot.WithNow(func() string { return "2026-10-29T12:00:00.000Z" })),
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

func postSlot(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

// TestSlotSpecsEndpoints 全链：建谱系 201 → 重复 409 → 追加 201 → 最新/版本列
// 表 → 404 → 校验 400 → 读面无门。
func TestSlotSpecsEndpoints(t *testing.T) {
	ts := slotTestServer(t)

	// 空列表
	empty, err := http.Get(ts.URL + "/v1/slots/specs")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var emptyDoc struct {
		Specs []json.RawMessage `json:"specs"`
	}
	_ = json.NewDecoder(empty.Body).Decode(&emptyDoc)
	empty.Body.Close()
	if empty.StatusCode != http.StatusOK || len(emptyDoc.Specs) != 0 {
		t.Fatalf("空表: status=%d n=%d", empty.StatusCode, len(emptyDoc.Specs))
	}

	// 建谱系（全字段）
	body := `{
		"slot_spec_id": "slot.research-lead",
		"name": "研究负责人",
		"responsibility": "汇总证据、组织协作，并对研究交付负责",
		"deliverables": ["结论、来源与具名异议", "可验收产物清单"],
		"skills": [{"ref": "workspace/skills/evidence-review", "version": 1}],
		"tools": {"required": ["documents.read"], "optional": ["web.search"]},
		"context": {"required": ["mission.contract"], "optional": []},
		"delegation": {"allowed": ["existing_agent"], "max_depth": 2},
		"runtime": {"required": ["execute"], "workspace_access": "read_write"},
		"handoff": {"required": ["checkpoint"]}
	}`
	resp := postSlot(t, ts.URL+"/v1/slots/specs", body)
	var created struct {
		SlotSpecID    string   `json:"slot_spec_id"`
		Version       int      `json:"version"`
		SchemaVersion int      `json:"schema_version"`
		Name          string   `json:"name"`
		Deliverables  []string `json:"deliverables"`
		Tools         *struct {
			Required []string `json:"required"`
		} `json:"tools"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Version != 1 || created.SlotSpecID != "slot.research-lead" ||
		created.SchemaVersion != 1 || len(created.Deliverables) != 2 || created.Tools == nil {
		t.Fatalf("create: status=%d %+v", resp.StatusCode, created)
	}

	// 重复建 → 409 spec_exists
	resp = postSlot(t, ts.URL+"/v1/slots/specs", body)
	var errDoc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || errDoc.Error.Code != "spec_exists" {
		t.Fatalf("重复建: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}

	// 追加 v2 → 201 version=2
	resp = postSlot(t, ts.URL+"/v1/slots/specs/slot.research-lead/versions",
		`{"name":"研究负责人（修订）","responsibility":"同上","deliverables":["新增交付项"]}`)
	var appended struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&appended)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || appended.Version != 2 || appended.Name != "研究负责人（修订）" {
		t.Fatalf("append: status=%d %+v", resp.StatusCode, appended)
	}

	// 最新 = v2
	latest, err := http.Get(ts.URL + "/v1/slots/specs/slot.research-lead")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	var latestDoc struct {
		Version int `json:"version"`
	}
	_ = json.NewDecoder(latest.Body).Decode(&latestDoc)
	latest.Body.Close()
	if latest.StatusCode != http.StatusOK || latestDoc.Version != 2 {
		t.Fatalf("latest: status=%d v=%d", latest.StatusCode, latestDoc.Version)
	}

	// 版本列表升序 [1,2]
	versions, err := http.Get(ts.URL + "/v1/slots/specs/slot.research-lead/versions")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	var verDoc struct {
		Versions []struct {
			Version int    `json:"version"`
			Name    string `json:"name"`
		} `json:"versions"`
	}
	_ = json.NewDecoder(versions.Body).Decode(&verDoc)
	versions.Body.Close()
	if versions.StatusCode != http.StatusOK || len(verDoc.Versions) != 2 ||
		verDoc.Versions[0].Version != 1 || verDoc.Versions[1].Version != 2 {
		t.Fatalf("versions: status=%d %+v", versions.StatusCode, verDoc.Versions)
	}
	// v1 不可变（原名称）
	if verDoc.Versions[0].Name != "研究负责人" {
		t.Fatalf("v1 被改写: %+v", verDoc.Versions[0])
	}

	// 列表摘要
	list, err := http.Get(ts.URL + "/v1/slots/specs")
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	var listDoc struct {
		Specs []struct {
			SlotSpecID    string `json:"slot_spec_id"`
			LatestVersion int    `json:"latest_version"`
			VersionCount  int    `json:"version_count"`
		} `json:"specs"`
	}
	_ = json.NewDecoder(list.Body).Decode(&listDoc)
	list.Body.Close()
	if len(listDoc.Specs) != 1 || listDoc.Specs[0].LatestVersion != 2 || listDoc.Specs[0].VersionCount != 2 {
		t.Fatalf("摘要: %+v", listDoc.Specs)
	}

	// 未知谱系 404 spec_not_found（latest 与 versions 同口径）
	miss, _ := http.Get(ts.URL + "/v1/slots/specs/slot.missing")
	_ = json.NewDecoder(miss.Body).Decode(&errDoc)
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound || errDoc.Error.Code != "spec_not_found" {
		t.Fatalf("404: status=%d code=%q", miss.StatusCode, errDoc.Error.Code)
	}
	resp = postSlot(t, ts.URL+"/v1/slots/specs/slot.missing/versions",
		`{"name":"x","responsibility":"y","deliverables":["z"]}`)
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || errDoc.Error.Code != "spec_not_found" {
		t.Fatalf("append 404: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}

	// 校验失败 400 invalid_spec（空名；bad id）
	resp = postSlot(t, ts.URL+"/v1/slots/specs",
		`{"slot_spec_id":"slot.bad","name":"  ","responsibility":"y","deliverables":["z"]}`)
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || errDoc.Error.Code != "invalid_spec" {
		t.Fatalf("校验: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}
	resp = postSlot(t, ts.URL+"/v1/slots/specs",
		`{"slot_spec_id":"bad-id","name":"x","responsibility":"y","deliverables":["z"]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id: status=%d", resp.StatusCode)
	}

	// 空 runtime 段拒绝（审校 P2#3）：workspace_access 缺失/空串经零值到达
	// 服务层，必须 400 不得落库——创建与追加两条路径同口径
	for _, badRuntime := range []string{
		`{"slot_spec_id":"slot.rt1","name":"x","responsibility":"y","deliverables":["z"],"runtime":{"required":["execute"]}}`,
		`{"slot_spec_id":"slot.rt2","name":"x","responsibility":"y","deliverables":["z"],"runtime":{"required":[],"workspace_access":""}}`,
	} {
		resp = postSlot(t, ts.URL+"/v1/slots/specs", badRuntime)
		_ = json.NewDecoder(resp.Body).Decode(&errDoc)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || errDoc.Error.Code != "invalid_spec" {
			t.Fatalf("空 runtime 建谱系: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
		}
	}
	resp = postSlot(t, ts.URL+"/v1/slots/specs/slot.gate/versions",
		`{"name":"x","responsibility":"y","deliverables":["z"],"runtime":{"required":[]}}`)
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || errDoc.Error.Code != "invalid_spec" {
		t.Fatalf("空 runtime 追加: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}
}

// TestSlotWriteGates 写门：跨源 Origin 403、非 JSON 415；读面不受门影响。
func TestSlotWriteGates(t *testing.T) {
	ts := slotTestServer(t)
	postSlot(t, ts.URL+"/v1/slots/specs", `{"slot_spec_id":"slot.gate","name":"值守","responsibility":"例行","deliverables":["日报"]}`).Body.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/slots/specs",
		strings.NewReader(`{"slot_spec_id":"slot.x","name":"x","responsibility":"y","deliverables":["z"]}`))
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("跨源: %v", err)
	}
	var errDoc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || errDoc.Error.Code != "origin_rejected" {
		t.Fatalf("跨源: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}

	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/slots/specs", strings.NewReader("name=x"))
	req2.Header.Set("Content-Type", "text/plain")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("文本: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("文本类型: status=%d", resp2.StatusCode)
	}

	// 追加端点同口径（表单类型 415）
	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/slots/specs/slot.gate/versions", strings.NewReader("a=1"))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("表单: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("追加表单类型: status=%d", resp3.StatusCode)
	}

	get, err := http.Get(ts.URL + "/v1/slots/specs")
	if err != nil || get.StatusCode != http.StatusOK {
		t.Fatalf("读面: %v %d", err, get.StatusCode)
	}
	get.Body.Close()
}
