package httpapi

// 0.6.13 审校修复回归（API 面）：#17 续接字段校验 400（先于幂等认领）、
// #11 被拒请求公开查询无回执、#5 手动执行共用预算（达限 409 budget_exhausted
// + 墙钟 deadline 落快照）。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// TestUpdateDispatchValidation400 #17：expected_version 缺失/为 0、mission_id
// 形制、reason 超长 → 400 invalid_request（不占键——修正后可重试）。
func TestUpdateDispatchValidation400(t *testing.T) {
	ts, mSvc := slotDispatchServer(t, nil)
	ctx := context.Background()
	ag := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(ag.Body).Decode(&agDoc)
	ag.Body.Close()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, agDoc.AgentID, mission.Content{
		Objective: "原目标", Scope: "s", AcceptanceCriteria: []string{"a"},
		DeliverableSpecs: []string{"d"}, SourceRefs: []string{"r:1"},
	}, &src)
	if err != nil {
		t.Fatal(err)
	}
	body := func(patch string) string {
		return `{"mission_id":"` + m.MissionID + `","expected_version":1,
		"idempotency_key":"idem-val","objective":"o","scope":"s",
		"acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]` + patch
	}
	cases := []struct {
		name string
		body string
	}{
		{"expected_version 0", `{"mission_id":"` + m.MissionID + `","expected_version":0,
		"idempotency_key":"idem-val","objective":"o","scope":"s",
		"acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`},
		{"mission_id 形制", `{"mission_id":"mi_非法 id!","expected_version":1,
		"idempotency_key":"idem-val","objective":"o","scope":"s",
		"acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`},
		{"reason 超长", body(`,"reason":"` + strings.Repeat("字", 281) + `"}`)},
		{"request_id 超长", body(`,"request_id":"` + strings.Repeat("r", 129) + `"}`)},
	}
	for _, tc := range cases {
		resp := postSlot(t, ts.URL+"/v1/dispatch/update", tc.body)
		code, _ := errOf(t, resp)
		if resp.StatusCode != http.StatusBadRequest || code != "invalid_request" {
			t.Fatalf("%s 须 400 invalid_request: %d %s", tc.name, resp.StatusCode, code)
		}
		resp.Body.Close()
	}
}

// TestRejectedUpdateHiddenReceiptAPI #11：领域拒绝（版本冲突）后，公开回执查询
// 不得返回"已提交"空回执——404（pending 认领是事务内部事实）。
func TestRejectedUpdateHiddenReceiptAPI(t *testing.T) {
	ts, mSvc := slotDispatchServer(t, nil)
	ctx := context.Background()
	ag := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(ag.Body).Decode(&agDoc)
	ag.Body.Close()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, agDoc.AgentID, mission.Content{
		Objective: "原目标", Scope: "s", AcceptanceCriteria: []string{"a"},
		DeliverableSpecs: []string{"d"}, SourceRefs: []string{"r:1"},
	}, &src)
	if err != nil {
		t.Fatal(err)
	}
	// 版本冲突拒绝（键已认领又回滚）
	vc := postSlot(t, ts.URL+"/v1/dispatch/update", `{"mission_id":"`+m.MissionID+`","expected_version":9,
		"idempotency_key":"idem-hidden","objective":"x","scope":"s",
		"acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if vc.StatusCode != http.StatusConflict {
		t.Fatalf("版本冲突须 409: %d", vc.StatusCode)
	}
	vc.Body.Close()
	// 公开查询 → 404（不得出现 replayed=true 的空回执）
	resp, err := http.Get(ts.URL + "/v1/dispatch/receipts?idempotency_key=idem-hidden")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("被拒请求公开查询须 404: %d", resp.StatusCode)
	}
}

// TestManualRunBudgetAPI #5：手动执行共用启动预算——max_runs=1 两次 POST →
// 201 + 409 budget_exhausted；墙钟随权威契约落 deadline（不因手动入口缺省）。
func TestManualRunBudgetAPI(t *testing.T) {
	ts, mSvc, _ := runTestServer(t)
	ctx := context.Background()
	budget := mission.DefaultBudget()
	budget.MaxRuns = 1
	budget.MaxWallClockSeconds = 60
	m, err := mSvc.Create(ctx, "ag_r1", mission.Content{
		Objective: "预算手动", Scope: "s", AcceptanceCriteria: []string{"a"},
		DeliverableSpecs: []string{"d"}, SourceRefs: []string{"r:1"}, Budget: budget,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 第 1 次：201（deadline 快照经域内 TestExecuteMissionAuthoritativeBudget
	// 钉住——视图无此字段，此处核入口达限面）
	resp := postSlot(t, ts.URL+"/v1/missions/"+m.MissionID+"/runs", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("首次手动执行须 201: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// 第 2 次：达限 → 409 budget_exhausted（可识别错误码）
	resp2 := postSlot(t, ts.URL+"/v1/missions/"+m.MissionID+"/runs", "")
	code, _ := errOf(t, resp2)
	if resp2.StatusCode != http.StatusConflict || code != "budget_exhausted" {
		t.Fatalf("手动达限须 409 budget_exhausted: %d %s", resp2.StatusCode, code)
	}
	resp2.Body.Close()
}

// TestReleaseDispatchClaimAPI 三审 #6：遗留认领处置端点——无行 404 /
// 已完结 409（不可动）；digest 核对与成功释放在 IT 层（ClaimByKey/ReleaseKey）覆盖。
func TestReleaseDispatchClaimAPI(t *testing.T) {
	ts, mSvc := slotDispatchServer(t, nil)
	ctx := context.Background()
	ag := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(ag.Body).Decode(&agDoc)
	ag.Body.Close()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, agDoc.AgentID, mission.Content{
		Objective: "原目标", Scope: "s", AcceptanceCriteria: []string{"a"},
		DeliverableSpecs: []string{"d"}, SourceRefs: []string{"r:1"},
	}, &src)
	if err != nil {
		t.Fatal(err)
	}
	// 成功续接 → 已完结回执不可释放
	ok := postSlot(t, ts.URL+"/v1/dispatch/update", `{"mission_id":"`+m.MissionID+`","expected_version":1,
		"idempotency_key":"idem-rel-api","objective":"追加","scope":"s",
		"acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("续接: %d", ok.StatusCode)
	}
	ok.Body.Close()
	dg := make([]byte, 64)
	for i := range dg {
		dg[i] = 'a'
	}
	// 无认领行 → 404
	r404 := postSlot(t, ts.URL+"/v1/dispatch/claims/idem-none/release", `{"request_digest":"`+string(dg)+`"}`)
	code404, _ := errOf(t, r404)
	if r404.StatusCode != http.StatusNotFound || code404 != "claim_not_found" {
		t.Fatalf("无行须 404: %d %s", r404.StatusCode, code404)
	}
	r404.Body.Close()
	// 已完结 → 409 claim_finalized
	r409 := postSlot(t, ts.URL+"/v1/dispatch/claims/idem-rel-api/release", `{"request_digest":"`+string(dg)+`"}`)
	code409, _ := errOf(t, r409)
	if r409.StatusCode != http.StatusConflict || code409 != "claim_finalized" {
		t.Fatalf("已完结须 409: %d %s", r409.StatusCode, code409)
	}
	r409.Body.Close()
}

// TestDispatchClaimViewAPI 四审 #7：认领只读面——已完结回执可见（state=
// finalized，releasable=false）；未知键 404。pending/create 类语义在 IT 覆盖。
func TestDispatchClaimViewAPI(t *testing.T) {
	ts, mSvc := slotDispatchServer(t, nil)
	ctx := context.Background()
	ag := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(ag.Body).Decode(&agDoc)
	ag.Body.Close()
	src := mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}
	m, err := mSvc.Create(ctx, agDoc.AgentID, mission.Content{
		Objective: "原目标", Scope: "s", AcceptanceCriteria: []string{"a"},
		DeliverableSpecs: []string{"d"}, SourceRefs: []string{"r:1"},
	}, &src)
	if err != nil {
		t.Fatal(err)
	}
	ok := postSlot(t, ts.URL+"/v1/dispatch/update", `{"mission_id":"`+m.MissionID+`","expected_version":1,
		"idempotency_key":"idem-view","objective":"追加","scope":"s",
		"acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("续接: %d", ok.StatusCode)
	}
	ok.Body.Close()
	codeView, resp := getJSONMap(t, ts.URL+"/v1/dispatch/claims/idem-view")
	if codeView != http.StatusOK || resp["state"] != "finalized" || resp["releasable"] != false {
		t.Fatalf("已完结认领视图: %d %v", codeView, resp)
	}
	codeNF, nf := getJSONMap(t, ts.URL+"/v1/dispatch/claims/idem-none")
	if codeNF != http.StatusNotFound || nf == nil {
		t.Fatalf("未知键须 404: %d %v", codeNF, nf)
	}
}
