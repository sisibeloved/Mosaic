package httpapi

// 0.6.0 Mission 契约面（ADR-0023）：七端点全链——两态创建、CAS 修订冲突、
// 完备性推进、终态取消、版本/事件只读面、写门。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// missionIdentAgents mission.AgentLookup 测试适配（经身份服务）。
type missionIdentAgents struct{ svc *personalagent.Service }

func (l missionIdentAgents) AgentExists(ctx context.Context, agentID string) (bool, error) {
	_, ok, err := l.svc.AgentByID(ctx, agentID)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// missionTestServer 身份 + Mission 契约装配。
func missionTestServer(t *testing.T) *httptest.Server {
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
		personalagent.WithNewAgentID(func() string { return "ag_seq_" + itoa(seq.Add(1)) }),
		personalagent.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }))
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Missions: mission.New(mission.NewMemStore(),
			mission.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
			mission.WithNewMissionID(func() string { return "mi_api_" + itoa(seq.Add(1)) }),
			mission.WithAgentLookup(missionIdentAgents{ident})),
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

func itoa(n int64) string {
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

const missionFullBody = `{
  "objective": "完成契约验收", "scope": "Mission 契约域",
  "constraints": ["无外部依赖"],
  "acceptance_criteria": ["七端点全绿"], "deliverable_specs": ["测试报告"],
  "source_refs": ["room:abc:seq1"], "return_targets": ["room:abc"],
  "budget": {"concurrency": 1, "max_runs": 8, "max_wall_clock_seconds": 3600, "max_retries": 0, "max_delegation_depth": 0},
  "reviewer": "ag_seq_1"
}`

// TestMissionEndpoints 全链：draft 创建 → 补 owner 修订 ready → CAS 冲突 →
// 取消终态 → 版本/事件只读面 → 校验/存在性反例。
func TestMissionEndpoints(t *testing.T) {
	ts := missionTestServer(t)

	// Agent（owner 候选）
	agentResp := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(agentResp.Body).Decode(&agDoc)
	agentResp.Body.Close()

	// 1) unassigned + 验收缺失 → draft
	resp := postSlot(t, ts.URL+"/v1/missions", `{"objective":"o","scope":"s","acceptance_criteria":[],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	var draft struct {
		MissionID       string `json:"mission_id"`
		Status          string `json:"status"`
		ContractVersion int    `json:"contract_version"`
		Budget          struct {
			MaxRuns int `json:"max_runs"`
		} `json:"budget"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&draft)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || draft.Status != "draft" || draft.ContractVersion != 1 {
		t.Fatalf("draft 创建: %d %+v", resp.StatusCode, draft)
	}
	if draft.Budget.MaxRuns != 32 {
		t.Fatalf("预算缺省应填 32: %+v", draft.Budget)
	}

	// 2) 修订补齐（owner + 验收）→ ready v2
	full := strings.Replace(missionFullBody, `"reviewer": "ag_seq_1"`, `"reviewer": ""`, 1)
	rev := postSlot(t, ts.URL+"/v1/missions/"+draft.MissionID+"/revise",
		`{"expected_version":1,"owner_agent_id":"`+agDoc.AgentID+`","objective":"完成契约验收","scope":"Mission 契约域","acceptance_criteria":["七端点全绿"],"deliverable_specs":["测试报告"],"source_refs":["room:abc:seq1"]}`)
	var up struct {
		Status          string `json:"status"`
		ContractVersion int    `json:"contract_version"`
		OwnerAgentID    string `json:"owner_agent_id"`
	}
	_ = json.NewDecoder(rev.Body).Decode(&up)
	rev.Body.Close()
	if rev.StatusCode != http.StatusOK || up.Status != "ready" || up.ContractVersion != 2 || up.OwnerAgentID != agDoc.AgentID {
		t.Fatalf("修订推进: %d %+v", rev.StatusCode, up)
	}

	// 3) CAS 冲突：旧版本号 → 409 version_conflict
	bad := postSlot(t, ts.URL+"/v1/missions/"+draft.MissionID+"/revise",
		`{"expected_version":1,"objective":"x","scope":"y","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if bad.StatusCode != http.StatusConflict || errCodeOf(t, bad) != "version_conflict" {
		t.Fatalf("CAS 冲突: %d %s", bad.StatusCode, errCodeOf(t, bad))
	}
	_ = full

	// 4) 校验反例：objective 空 → 400 invalid_mission；未知 owner → 404
	inv := postSlot(t, ts.URL+"/v1/missions", `{"objective":"","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if inv.StatusCode != http.StatusBadRequest || errCodeOf(t, inv) != "invalid_mission" {
		t.Fatalf("校验反例: %d %s", inv.StatusCode, errCodeOf(t, inv))
	}
	unk := postSlot(t, ts.URL+"/v1/missions", `{"objective":"o","scope":"s","owner_agent_id":"ag_none","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if unk.StatusCode != http.StatusNotFound || errCodeOf(t, unk) != "agent_not_found" {
		t.Fatalf("未知 owner: %d %s", unk.StatusCode, errCodeOf(t, unk))
	}

	// 5) 取消（错误版本 → 409 version_conflict；正确 → 终态）
	badCancel := postSlot(t, ts.URL+"/v1/missions/"+draft.MissionID+"/cancel", `{"expected_version":1}`)
	if badCancel.StatusCode != http.StatusConflict || errCodeOf(t, badCancel) != "version_conflict" {
		t.Fatalf("取消 CAS: %d %s", badCancel.StatusCode, errCodeOf(t, badCancel))
	}
	cancel := postSlot(t, ts.URL+"/v1/missions/"+draft.MissionID+"/cancel", `{"expected_version":2,"reason":"计划变更"}`)
	var canceled struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(cancel.Body).Decode(&canceled)
	cancel.Body.Close()
	if cancel.StatusCode != http.StatusOK || canceled.Status != "canceled" {
		t.Fatalf("取消: %d %+v", cancel.StatusCode, canceled)
	}
	again := postSlot(t, ts.URL+"/v1/missions/"+draft.MissionID+"/cancel", `{"expected_version":2}`)
	if again.StatusCode != http.StatusConflict || errCodeOf(t, again) != "invalid_transition" {
		t.Fatalf("重复取消: %d %s", again.StatusCode, errCodeOf(t, again))
	}

	// 6) 只读面：列表 / 详情 / 版本 / 事件；404 反例
	list, _ := http.Get(ts.URL + "/v1/missions")
	var listDoc struct {
		Missions []struct {
			MissionID string `json:"mission_id"`
			Status    string `json:"status"`
		} `json:"missions"`
	}
	_ = json.NewDecoder(list.Body).Decode(&listDoc)
	list.Body.Close()
	if len(listDoc.Missions) != 1 || listDoc.Missions[0].Status != "canceled" {
		t.Fatalf("列表: %+v", listDoc)
	}
	det, _ := http.Get(ts.URL + "/v1/missions/" + draft.MissionID)
	if det.StatusCode != http.StatusOK {
		t.Fatalf("详情: %d", det.StatusCode)
	}
	det.Body.Close()
	vers, _ := http.Get(ts.URL + "/v1/missions/" + draft.MissionID + "/versions")
	var versDoc struct {
		Versions []struct {
			ContractVersion int    `json:"contract_version"`
			Status          string `json:"status"`
		} `json:"versions"`
	}
	_ = json.NewDecoder(vers.Body).Decode(&versDoc)
	vers.Body.Close()
	if len(versDoc.Versions) != 2 || versDoc.Versions[0].Status != "draft" || versDoc.Versions[1].Status != "ready" {
		t.Fatalf("版本列表: %+v", versDoc)
	}
	evs, _ := http.Get(ts.URL + "/v1/missions/" + draft.MissionID + "/events")
	var evDoc struct {
		Events []struct {
			EventType string         `json:"event_type"`
			Payload   map[string]any `json:"payload"`
		} `json:"events"`
	}
	_ = json.NewDecoder(evs.Body).Decode(&evDoc)
	evs.Body.Close()
	if len(evDoc.Events) != 4 { // created + contract_revised + status_changed(draft→ready) + status_changed(→canceled)
		t.Fatalf("事件 %d 条, want 4: %+v", len(evDoc.Events), evDoc.Events)
	}
	last := evDoc.Events[len(evDoc.Events)-1]
	if last.EventType != "status_changed" || last.Payload["reason"] != "计划变更" {
		t.Fatalf("取消事件失真: %+v", last)
	}
	nf, _ := http.Get(ts.URL + "/v1/missions/mi_none")
	if nf.StatusCode != http.StatusNotFound || errCodeOf(t, nf) != "mission_not_found" {
		t.Fatalf("未知详情: %d %s", nf.StatusCode, errCodeOf(t, nf))
	}
}

// TestMissionRequiredFieldsAndBudget 半体修订与非法预算（审校 P2#3）：
// 必填键缺失/null → 400 bad_request 且原契约不动；显式全零预算 → 400
// invalid_mission（不得当省略套默认）。
func TestMissionRequiredFieldsAndBudget(t *testing.T) {
	ts := missionTestServer(t)

	agentResp := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(agentResp.Body).Decode(&agDoc)
	agentResp.Body.Close()

	// 基线：完整契约 v1（显式非默认预算）
	resp := postSlot(t, ts.URL+"/v1/missions", `{
		"objective":"o","scope":"s","owner_agent_id":"`+agDoc.AgentID+`",
		"constraints":["c"],"acceptance_criteria":["a"],"deliverable_specs":["d"],
		"source_refs":["r:1"],"return_targets":["room:x"],"reviewer":"rv",
		"budget":{"concurrency":2,"max_runs":2,"max_wall_clock_seconds":600,"max_retries":0,"max_delegation_depth":0}}`)
	var base struct {
		MissionID string `json:"mission_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&base)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("基线创建: %d", resp.StatusCode)
	}

	// 1) 修订缺必填键 → 400 bad_request
	for name, body := range map[string]string{
		"缺验收键":  `{"expected_version":1,"objective":"o2","scope":"s","deliverable_specs":["d"],"source_refs":["r:1"]}`,
		"缺来源键":  `{"expected_version":1,"objective":"o2","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"]}`,
		"验收 null": `{"expected_version":1,"objective":"o2","scope":"s","acceptance_criteria":null,"deliverable_specs":["d"],"source_refs":["r:1"]}`,
	} {
		r := postSlot(t, ts.URL+"/v1/missions/"+base.MissionID+"/revise", body)
		if r.StatusCode != http.StatusBadRequest || errCodeOf(t, r) != "bad_request" {
			t.Fatalf("%s 应 400 bad_request: %d %s", name, r.StatusCode, errCodeOf(t, r))
		}
	}

	// 2) 显式全零预算 → 400 invalid_mission（不得静默套默认）
	r2 := postSlot(t, ts.URL+"/v1/missions/"+base.MissionID+"/revise",
		`{"expected_version":1,"objective":"o2","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"],"budget":{"concurrency":0,"max_runs":0,"max_wall_clock_seconds":0,"max_retries":0,"max_delegation_depth":0}}`)
	if r2.StatusCode != http.StatusBadRequest || errCodeOf(t, r2) != "invalid_mission" {
		t.Fatalf("全零预算应 400 invalid_mission: %d %s", r2.StatusCode, errCodeOf(t, r2))
	}
	// 创建面同样
	r3 := postSlot(t, ts.URL+"/v1/missions",
		`{"objective":"o","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"],"budget":{"concurrency":0,"max_runs":0,"max_wall_clock_seconds":0,"max_retries":0,"max_delegation_depth":0}}`)
	if r3.StatusCode != http.StatusBadRequest || errCodeOf(t, r3) != "invalid_mission" {
		t.Fatalf("创建全零预算应 400: %d %s", r3.StatusCode, errCodeOf(t, r3))
	}

	// 3) 原契约未被半体/非法请求触碰：预算/约束/返回位/验收人保持
	get, _ := http.Get(ts.URL + "/v1/missions/" + base.MissionID)
	var doc struct {
		ContractVersion int      `json:"contract_version"`
		Constraints     []string `json:"constraints"`
		ReturnTargets   []string `json:"return_targets"`
		Reviewer        string   `json:"reviewer"`
		Budget          struct {
			Concurrency         int `json:"concurrency"`
			MaxRuns             int `json:"max_runs"`
			MaxWallClockSeconds int `json:"max_wall_clock_seconds"`
		} `json:"budget"`
	}
	_ = json.NewDecoder(get.Body).Decode(&doc)
	get.Body.Close()
	if doc.ContractVersion != 1 || len(doc.Constraints) != 1 || len(doc.ReturnTargets) != 1 ||
		doc.Reviewer != "rv" || doc.Budget.Concurrency != 2 || doc.Budget.MaxRuns != 2 || doc.Budget.MaxWallClockSeconds != 600 {
		t.Fatalf("无效请求不得改动原契约: %+v", doc)
	}

	// 4) 合法显式预算修订 → 200 且预算按请求落（不被缺省覆盖）
	r4 := postSlot(t, ts.URL+"/v1/missions/"+base.MissionID+"/revise",
		`{"expected_version":1,"objective":"o2","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"],"constraints":["c"],"return_targets":["room:x"],"reviewer":"rv","budget":{"concurrency":1,"max_runs":3,"max_wall_clock_seconds":1200,"max_retries":0,"max_delegation_depth":0}}`)
	if r4.StatusCode != http.StatusOK {
		t.Fatalf("合法修订: %d", r4.StatusCode)
	}
	r4.Body.Close()
	get2, _ := http.Get(ts.URL + "/v1/missions/" + base.MissionID)
	var doc2 struct {
		Budget struct {
			MaxRuns int `json:"max_runs"`
		} `json:"budget"`
	}
	_ = json.NewDecoder(get2.Body).Decode(&doc2)
	get2.Body.Close()
	if doc2.Budget.MaxRuns != 3 {
		t.Fatalf("显式预算应按请求落: %+v", doc2.Budget)
	}
}

// TestMissionConcurrentReviseAPI 并发修订恰一胜者（端点层，AT-13）。
func TestMissionConcurrentReviseAPI(t *testing.T) {
	ts := missionTestServer(t)
	resp := postSlot(t, ts.URL+"/v1/missions", missionFullBody)
	var m struct {
		MissionID string `json:"mission_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()

	var wg sync.WaitGroup
	var wins, conflicts atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := postSlot(t, ts.URL+"/v1/missions/"+m.MissionID+"/revise",
				`{"expected_version":1,"objective":"并发修订 `+itoa(int64(i))+`","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
			if r.StatusCode == http.StatusOK {
				wins.Add(1)
			} else if r.StatusCode == http.StatusConflict {
				conflicts.Add(1)
			} else {
				t.Errorf("并发修订意外状态: %d", r.StatusCode)
			}
			r.Body.Close()
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 7 {
		t.Fatalf("恰一胜者: wins=%d conflicts=%d", wins.Load(), conflicts.Load())
	}
}
