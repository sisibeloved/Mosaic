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

// TestSlotInstancesEndpoints 实例三端点：建（默认/显式标题 + 现势连接）、
// 未知谱系 404、空标题 400、列表/详情/404、模板演进不回写实例标题。
func TestSlotInstancesEndpoints(t *testing.T) {
	ts := slotTestServer(t)

	// 建谱系（供实例化）
	resp := postSlot(t, ts.URL+"/v1/slots/specs", `{"slot_spec_id":"slot.duty","name":"值守","responsibility":"例行检查","deliverables":["日报"],"tools":{"required":["documents.read"],"optional":[]}}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建谱系: %d", resp.StatusCode)
	}

	// 建实例（缺省标题）
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.duty"}`)
	var inst struct {
		SlotInstanceID     string `json:"slot_instance_id"`
		SlotSpecID         string `json:"slot_spec_id"`
		Scope              string `json:"scope"`
		Title              string `json:"title"`
		KnowledgeNamespace string `json:"knowledge_namespace"`
		CreatedFromVersion int    `json:"created_from_version"`
		Spec               struct {
			Name          string `json:"name"`
			LatestVersion int    `json:"latest_version"`
		} `json:"spec"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&inst)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || !strings.HasPrefix(inst.SlotInstanceID, "si_") ||
		!strings.HasPrefix(inst.KnowledgeNamespace, "slotns_") {
		t.Fatalf("建实例: status=%d %+v", resp.StatusCode, inst)
	}
	if inst.Title != "值守" || inst.CreatedFromVersion != 1 || inst.Scope != "workspace" ||
		inst.Spec.Name != "值守" || inst.Spec.LatestVersion != 1 {
		t.Fatalf("缺省形状: %+v", inst)
	}

	// 显式标题 + 未知谱系 404 + 空标题 400
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.duty","title":"夜间值守"}`)
	var inst2 struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&inst2)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || inst2.Title != "夜间值守" {
		t.Fatalf("显式标题: status=%d %+v", resp.StatusCode, inst2)
	}
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.missing"}`)
	var errDoc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || errDoc.Error.Code != "spec_not_found" {
		t.Fatalf("未知谱系: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.duty","title":"  "}`)
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || errDoc.Error.Code != "invalid_instance" {
		t.Fatalf("空标题: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}

	// 模板追加 v2（改名）：实例标题存档不变、现势连接演进
	resp = postSlot(t, ts.URL+"/v1/slots/specs/slot.duty/versions", `{"name":"值守（修订）","responsibility":"例行","deliverables":["日报"]}`)
	resp.Body.Close()
	list, err := http.Get(ts.URL + "/v1/slots/instances")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var doc struct {
		Instances []struct {
			SlotInstanceID string `json:"slot_instance_id"`
			Title          string `json:"title"`
			Spec           struct {
				Name          string `json:"name"`
				LatestVersion int    `json:"latest_version"`
			} `json:"spec"`
		} `json:"instances"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	if list.StatusCode != http.StatusOK || len(doc.Instances) != 2 {
		t.Fatalf("列表: status=%d n=%d", list.StatusCode, len(doc.Instances))
	}
	first := doc.Instances[0]
	if first.Title != "值守" || first.Spec.Name != "值守（修订）" || first.Spec.LatestVersion != 2 {
		t.Fatalf("存档标题漂移或现势未演进: %+v", first)
	}

	// 详情 / 404
	one, _ := http.Get(ts.URL + "/v1/slots/instances/" + inst.SlotInstanceID)
	one.Body.Close()
	if one.StatusCode != http.StatusOK {
		t.Fatalf("详情: %d", one.StatusCode)
	}
	miss, _ := http.Get(ts.URL + "/v1/slots/instances/si_missing")
	_ = json.NewDecoder(miss.Body).Decode(&errDoc)
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound || errDoc.Error.Code != "instance_not_found" {
		t.Fatalf("详情404: status=%d code=%q", miss.StatusCode, errDoc.Error.Code)
	}

	// 写门：跨源 403 / 非 JSON 415（读面无门）
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/slots/instances", strings.NewReader(`{"slot_spec_id":"slot.duty"}`))
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "application/json")
	gate, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("跨源: %v", err)
	}
	_ = json.NewDecoder(gate.Body).Decode(&errDoc)
	gate.Body.Close()
	if gate.StatusCode != http.StatusForbidden || errDoc.Error.Code != "origin_rejected" {
		t.Fatalf("跨源: status=%d code=%q", gate.StatusCode, errDoc.Error.Code)
	}
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/slots/instances", strings.NewReader("x"))
	req2.Header.Set("Content-Type", "text/plain")
	gate2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("文本: %v", err)
	}
	gate2.Body.Close()
	if gate2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("文本类型: status=%d", gate2.StatusCode)
	}
}

// TestSlotInstanceExplicitEmptyTitle 显式空标题 400（审校 P2#3）：省略 → 缺省；
// 空串/纯空白 → 400，不与省略合流。
func TestSlotInstanceExplicitEmptyTitle(t *testing.T) {
	ts := slotTestServer(t)
	resp := postSlot(t, ts.URL+"/v1/slots/specs", `{"slot_spec_id":"slot.t3","name":"值守","responsibility":"r","deliverables":["d"]}`)
	resp.Body.Close()

	// 空串 → 400
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.t3","title":""}`)
	var errDoc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || errDoc.Error.Code != "invalid_instance" {
		t.Fatalf("空串标题: status=%d code=%q", resp.StatusCode, errDoc.Error.Code)
	}
	// 纯空白 → 400
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.t3","title":"  "}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("纯空白标题: status=%d", resp.StatusCode)
	}
	// 省略 → 201 缺省
	resp = postSlot(t, ts.URL+"/v1/slots/instances", `{"slot_spec_id":"slot.t3"}`)
	var inst struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&inst)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || inst.Title != "值守" {
		t.Fatalf("省略标题: status=%d title=%q", resp.StatusCode, inst.Title)
	}
}
