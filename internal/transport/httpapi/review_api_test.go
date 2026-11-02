package httpapi

// 0.6.9 验收端点（ADR-0032）：request-review（必需项 409 → 补证 → reviewing）、
// review（退回 → ready / 接受 → accepted 终态）、403 验收人、验收期间冻结族、
// accepted 终态拒绝族、503。真 mission + run + delivery + review 域（runTestServer）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// readyMissionReviewer 带契约验收人的 ready Mission（两条验收标准）。
func readyMissionReviewer(t *testing.T, mSvc *mission.Service, owner, reviewer string) mission.Mission {
	t.Helper()
	m, err := mSvc.Create(context.Background(), owner, mission.Content{
		Objective: "验收端点", Scope: "s",
		AcceptanceCriteria: []string{"七端点全绿", "CAS 冲突可见"}, DeliverableSpecs: []string{"d"},
		SourceRefs: []string{"r:1"}, Budget: mission.DefaultBudget(), Reviewer: reviewer,
	}, nil)
	if err != nil {
		t.Fatalf("建 Mission: %v", err)
	}
	return m
}

func submitPkg(t *testing.T, base, mi, runID string, gen, ver int, evidence2 string) string {
	t.Helper()
	body := `{"run_id":"` + runID + `","generation":` + itoa(int64(gen)) + `,"expected_version":` + itoa(int64(ver)) + `,` +
		`"artifact_refs":["ws://report"],"acceptance_mapping":[{"criterion":"七端点全绿","evidence":"CI 绿"},{"criterion":"CAS 冲突可见","evidence":"` + evidence2 + `"}]}`
	r := postSlot(t, base+"/v1/missions/"+mi+"/delivery-packages", body)
	var p struct {
		PackageID string `json:"package_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("提交包: %d", r.StatusCode)
	}
	return p.PackageID
}

type missionDoc struct {
	Status          string `json:"status"`
	ReviewPackageID string `json:"review_package_id"`
	ReviewRound     int    `json:"review_round"`
	ContractVersion int    `json:"contract_version"`
}

func decodeMission(t *testing.T, r *http.Response) missionDoc {
	t.Helper()
	var d missionDoc
	_ = json.NewDecoder(r.Body).Decode(&d)
	r.Body.Close()
	return d
}

func TestReviewEndpointsFlow(t *testing.T) {
	ts, mSvc, _ := runTestServer(t)
	m := readyMissionReviewer(t, mSvc, "ag_r1", "")
	mi := m.MissionID
	rd := executeOnce(t, ts.URL, mi)

	// 1) 缺证据包 → request-review 409 missing_required_items（detail 携缺项）
	partial := submitPkg(t, ts.URL, mi, rd.RunID, rd.Generation, 1, "")
	r := postSlot(t, ts.URL+"/v1/missions/"+mi+"/request-review", `{"expected_version":1,"package_id":"`+partial+`"}`)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("缺项应 409: %d", r.StatusCode)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(r.Body).Decode(&e)
	r.Body.Close()
	if e.Error.Code != "missing_required_items" || !strings.Contains(e.Error.Message, "CAS 冲突可见") {
		t.Fatalf("缺项回显: %+v", e.Error)
	}
	// 404 包未知；400 形状
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/request-review", `{"expected_version":1,"package_id":"dlv_none"}`)
	if r.StatusCode != http.StatusNotFound || errCodeOf(t, r) != "package_not_found" {
		t.Fatalf("包未知: %d", r.StatusCode)
	}
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/request-review", `{"expected_version":1}`)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 package_id 应 400: %d", r.StatusCode)
	}

	// 2) 补证 → 200 reviewing + 锚（另备一个完整包——reviewing 期间不可再提交）
	full := submitPkg(t, ts.URL, mi, rd.RunID, rd.Generation, 1, "并发日志")
	other := submitPkg(t, ts.URL, mi, rd.RunID, rd.Generation, 1, "证据齐全")
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/request-review", `{"expected_version":1,"package_id":"`+full+`"}`)
	d := decodeMission(t, r)
	if r.StatusCode != http.StatusOK || d.Status != "reviewing" || d.ReviewPackageID != full {
		t.Fatalf("送验: %d %+v", r.StatusCode, d)
	}
	// 幂等同包 200；换包（另一完整包——partial 会先撞缺项检查）409 invalid_transition
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/request-review", `{"expected_version":1,"package_id":"`+full+`"}`)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("同包幂等: %d", r.StatusCode)
	}
	r.Body.Close()
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/request-review", `{"expected_version":1,"package_id":"`+other+`"}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "invalid_transition" {
		t.Fatalf("换包须先退回: %d", r.StatusCode)
	}

	// 3) 验收期间冻结：提交 409 mission_reviewing；修订 409；暂停 409；执行 409
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", `{"run_id":"`+rd.RunID+`","generation":`+itoa(int64(rd.Generation))+`,"expected_version":1,"artifact_refs":["x"],"acceptance_mapping":[]}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "mission_reviewing" {
		t.Fatalf("验收期间提交: %d", r.StatusCode)
	}
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/revise", `{"expected_version":1,"objective":"o2","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"],"owner_agent_id":"ag_r1"}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "invalid_transition" {
		t.Fatalf("验收期间修订: %d", r.StatusCode)
	}
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/pause", `{"expected_version":1}`)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("验收期间暂停: %d", r.StatusCode)
	}
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "mission_not_ready" {
		t.Fatalf("验收期间执行: %d", r.StatusCode)
	}

	// 4) 退回：缺 reason 400；带 reason → ready + 锚清空
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/review", `{"expected_version":1,"review_round":1,"decision":"returned"}`)
	if r.StatusCode != http.StatusBadRequest || errCodeOf(t, r) != "invalid_review" {
		t.Fatalf("退回缺原因: %d", r.StatusCode)
	}
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/review", `{"expected_version":1,"review_round":1,"decision":"returned","reason":"第二条证据不足","item_notes":[{"criterion":"CAS 冲突可见","note":"补并发日志"}]}`)
	d = decodeMission(t, r)
	if r.StatusCode != http.StatusOK || d.Status != "ready" || d.ReviewPackageID != "" {
		t.Fatalf("退回: %d %+v", r.StatusCode, d)
	}
	// 退回后可再提交/再送验/接受
	full2 := submitPkg(t, ts.URL, mi, rd.RunID, rd.Generation, 1, "并发日志 v2")
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/request-review", `{"expected_version":1,"package_id":"`+full2+`"}`)
	r.Body.Close()
	// 审校 #3：旧轮次（1）裁定落到第二轮 → 409 invalid_transition（旧标签页/重放）
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/review", `{"expected_version":1,"review_round":1,"decision":"accepted"}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "invalid_transition" {
		t.Fatalf("旧轮次裁定须 409: %d", r.StatusCode)
	}
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/review", `{"expected_version":1,"review_round":2,"decision":"accepted","reason":"全部达成"}`)
	d = decodeMission(t, r)
	if r.StatusCode != http.StatusOK || d.Status != "accepted" || d.ReviewPackageID != full2 || d.ReviewRound != 2 {
		t.Fatalf("接受: %d %+v", r.StatusCode, d)
	}

	// 5) accepted 终态拒绝族
	for name, tc := range map[string]struct{ path, body, code string }{
		"修订":  {"/revise", `{"expected_version":1,"objective":"o","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"],"owner_agent_id":"ag_r1"}`, "invalid_transition"},
		"取消":  {"/cancel", `{"expected_version":1}`, "invalid_transition"},
		"暂停":  {"/pause", `{"expected_version":1}`, "invalid_transition"},
		"再送验": {"/request-review", `{"expected_version":1,"package_id":"` + full2 + `"}`, "invalid_transition"},
		"再裁定": {"/review", `{"expected_version":1,"review_round":2,"decision":"returned","reason":"x"}`, "invalid_transition"},
		"再提交": {"/delivery-packages", `{"run_id":"` + rd.RunID + `","generation":` + itoa(int64(rd.Generation)) + `,"expected_version":1,"artifact_refs":["x"],"acceptance_mapping":[]}`, "mission_accepted"},
	} {
		rr := postSlot(t, ts.URL+"/v1/missions/"+mi+tc.path, tc.body)
		if rr.StatusCode != http.StatusConflict || errCodeOf(t, rr) != tc.code {
			t.Fatalf("accepted 后%s 应 409 %s: %d", name, tc.code, rr.StatusCode)
		}
	}
	rr := postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	if rr.StatusCode != http.StatusConflict {
		t.Fatalf("accepted 不可执行: %d", rr.StatusCode)
	}
	// 事件流含 reviewing/returned/accepted 留痕
	evs, _ := http.Get(ts.URL + "/v1/missions/" + mi + "/events")
	var evDoc struct {
		Events []struct {
			Payload map[string]any `json:"payload"`
		} `json:"events"`
	}
	_ = json.NewDecoder(evs.Body).Decode(&evDoc)
	evs.Body.Close()
	var decisions []string
	for _, e := range evDoc.Events {
		if dcs, ok := e.Payload["decision"].(string); ok {
			decisions = append(decisions, dcs)
		}
	}
	if len(decisions) != 2 || decisions[0] != "returned" || decisions[1] != "accepted" {
		t.Fatalf("裁定留痕: %v", decisions)
	}
}

// TestReviewAuthorization 契约 reviewer 非空：不符 403；相等 200。
func TestReviewAuthorization(t *testing.T) {
	ts, mSvc, _ := runTestServer(t)
	m := readyMissionReviewer(t, mSvc, "ag_r1", "qa@team")
	rd := executeOnce(t, ts.URL, m.MissionID)
	full := submitPkg(t, ts.URL, m.MissionID, rd.RunID, rd.Generation, 1, "ok")
	postSlot(t, ts.URL+"/v1/missions/"+m.MissionID+"/request-review", `{"expected_version":1,"package_id":"`+full+`"}`).Body.Close()
	r := postSlot(t, ts.URL+"/v1/missions/"+m.MissionID+"/review", `{"expected_version":1,"review_round":1,"decision":"accepted","reviewer":"someone"}`)
	if r.StatusCode != http.StatusForbidden || errCodeOf(t, r) != "review_not_authorized" {
		t.Fatalf("验收人不符应 403: %d", r.StatusCode)
	}
	r = postSlot(t, ts.URL+"/v1/missions/"+m.MissionID+"/review", `{"expected_version":1,"review_round":1,"decision":"accepted","reviewer":"qa@team"}`)
	if d := decodeMission(t, r); r.StatusCode != http.StatusOK || d.Status != "accepted" {
		t.Fatalf("验收人匹配: %d %+v", r.StatusCode, d)
	}
}

// TestReviewUnavailable 验收域未装配 → 503。
func TestReviewUnavailable(t *testing.T) {
	bare := httptest.NewServer(New(Deps{}))
	t.Cleanup(bare.Close)
	for _, path := range []string{"/request-review", "/review"} {
		r := postSlot(t, bare.URL+"/v1/missions/mi_x"+path, `{"expected_version":1,"review_round":1,"package_id":"dlv_x","decision":"accepted"}`)
		if r.StatusCode != http.StatusServiceUnavailable || errCodeOf(t, r) != "review_unavailable" {
			t.Fatalf("%s 未装配应 503: %d", path, r.StatusCode)
		}
	}
}
