package httpapi

// 0.6.12 / ADR-0035 责任交接 API：发起 → 冻结窗 → 在途唯一 → 承接（owner 翻转 +
// 回执）→ 婉拒/撤回留痕 → 历史读面；退役承诺门端到端（owned 非终态任务 → retiring
// 停留 + 清单；承接后清空 → retired）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/missioncontrol"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/room"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// handoverCommitments 承诺门测试件（与 app.missionCommitments 同语义：非终态
// owned Mission = 未处置承诺）。
type handoverCommitments struct{ m *mission.MemStore }

func (c handoverCommitments) Outstanding(ctx context.Context, agentID string) ([]string, error) {
	list, err := c.m.ListMissions(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, s := range list {
		if s.OwnerAgentID == agentID && s.Status != mission.StatusAccepted && s.Status != mission.StatusCanceled {
			out = append(out, fmt.Sprintf("%s（%s）%s", s.MissionID, s.Status, s.Objective))
		}
	}
	return out, nil
}

// handoverTestServer 身份（含承诺门）+ Mission 契约装配。
func handoverTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-11-03T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_id" },
		Tenant: "ten_local",
	})
	var seq atomic.Int64
	mStore := mission.NewMemStore()
	ident := personalagent.New(personalagent.NewMemStore(), emptyIdentityRegistry{},
		personalagent.WithNewAgentID(func() string { return "ag_seq_" + itoa(seq.Add(1)) }),
		personalagent.WithNow(func() string { return "2026-11-03T12:00:00.000Z" }),
		personalagent.WithCommitments(handoverCommitments{m: mStore}))
	mSvc := mission.New(mStore,
		mission.WithNow(func() string { return "2026-11-03T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_api_" + itoa(seq.Add(1)) }),
		mission.WithAgentLookup(missionIdentAgents{ident}))
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:    room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Missions: mSvc, Identity: ident,
		MissionControl: missioncontrol.New(mSvc,
			missionrun.New(missionrun.NewMemStore(), epRunMissions{mSvc}, epRunBindings{}, epRunRuntimes{}, &epRunLeases{}, epRunManifests{}, epRunExecutor{},
				missionrun.WithSchedulerDisabled()),
			missioncontrol.WithCancelWait(50*time.Millisecond)),
	}))
	t.Cleanup(ts.Close)
	return ts
}

func mkAgent(t *testing.T, ts *httptest.Server, name string) string {
	t.Helper()
	resp := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"`+name+`"}`)
	defer resp.Body.Close()
	var doc struct {
		AgentID string `json:"agent_id"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&doc) != nil || doc.AgentID == "" {
		t.Fatalf("建 Agent %s: %d", name, resp.StatusCode)
	}
	return doc.AgentID
}

func mkOwnedMission(t *testing.T, ts *httptest.Server, owner string) (string, int) {
	t.Helper()
	body := `{"objective":"交接验收","scope":"责任交接域","owner_agent_id":"` + owner +
		`","acceptance_criteria":["回执留痕"],"deliverable_specs":["报告"],"source_refs":["room:h:seq1"]}`
	resp := postSlot(t, ts.URL+"/v1/missions", body)
	defer resp.Body.Close()
	var m struct {
		MissionID       string `json:"mission_id"`
		Status          string `json:"status"`
		ContractVersion int    `json:"contract_version"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&m) != nil ||
		m.Status != "ready" || m.ContractVersion != 1 {
		t.Fatalf("建任务: %d", resp.StatusCode)
	}
	return m.MissionID, m.ContractVersion
}

func handoverJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解码: %v", err)
	}
	return out
}

// TestHandoverEndpoints 全链：发起 → 冻结窗/在途唯一 → 承接翻转 + 回执 → 历史；
// 校验/存在性反例；婉拒/撤回了结与留痕。
func TestHandoverEndpoints(t *testing.T) {
	ts := handoverTestServer(t)
	a := mkAgent(t, ts, "甲")
	b := mkAgent(t, ts, "乙")
	mid, _ := mkOwnedMission(t, ts, a)

	// 历史空列表
	_, got := getJSONMap(t, ts.URL+"/v1/missions/"+mid+"/handovers")
	if evs, _ := got["events"].([]any); len(evs) != 0 {
		t.Fatalf("初始历史应空: %v", got)
	}

	// 发起：同 owner 409；未知承接人 404；空承接人 400；正确 → 201 pending 可见
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover",
		`{"expected_version":1,"new_owner_agent_id":"`+a+`"}`); resp.StatusCode != http.StatusConflict || errCodeOf(t, resp) != "invalid_transition" {
		t.Fatalf("同 owner: %d %s", resp.StatusCode, errCodeOf(t, resp))
	}
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover",
		`{"expected_version":1,"new_owner_agent_id":"ag_none"}`); resp.StatusCode != http.StatusNotFound || errCodeOf(t, resp) != "agent_not_found" {
		t.Fatalf("未知承接人: %d %s", resp.StatusCode, errCodeOf(t, resp))
	}
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover",
		`{"expected_version":1,"new_owner_agent_id":""}`); resp.StatusCode != http.StatusBadRequest || errCodeOf(t, resp) != "invalid_handover" {
		t.Fatalf("空承接人: %d %s", resp.StatusCode, errCodeOf(t, resp))
	}
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover",
		`{"expected_version":1,"new_owner_agent_id":"`+b+`","oops":true}`); resp.StatusCode != http.StatusBadRequest || errCodeOf(t, resp) != "invalid_handover" {
		t.Fatalf("未知字段: %d %s", resp.StatusCode, errCodeOf(t, resp))
	}
	resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover",
		`{"expected_version":1,"new_owner_agent_id":"`+b+`","notes":"在途见契约 v1"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("发起: %d", resp.StatusCode)
	}
	doc := handoverJSON(t, resp)
	if doc["pending_owner_agent_id"] != b || doc["owner_agent_id"] != a || doc["contract_version"] != float64(1) {
		t.Fatalf("发起视图: %v", doc)
	}
	offerID, _ := doc["pending_offer_id"].(string)
	if offerID == "" {
		t.Fatalf("发起须携 pending_offer_id: %v", doc)
	}
	// 审校 #1：缺 offer_id / 陌生 offer_id 的承接 409（不得处理当前在途）
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover/accept", `{"expected_version":1}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("缺 offer_id 承接须 409: %d", resp.StatusCode)
	}
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover/accept", `{"expected_version":1,"offer_id":"hdo_bogus"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("陌生 offer_id 承接须 409: %d", resp.StatusCode)
	}

	// 冻结窗：修订 / 取消 409；在途唯一：二发 409
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/revise",
		`{"expected_version":1,"objective":"x","scope":"y","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("冻结窗修订应 409: %d", resp.StatusCode)
	}
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/cancel", `{"expected_version":1}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("冻结窗取消应 409: %d", resp.StatusCode)
	}
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover",
		`{"expected_version":1,"new_owner_agent_id":"`+mkAgent(t, ts, "丙")+`"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("在途唯一: %d", resp.StatusCode)
	}

	// 承接：CAS 冲突 409 → 正确 200 owner 翻转 + pending 清空 + 版本不递增
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover/accept",
		`{"expected_version":9}`); resp.StatusCode != http.StatusConflict || errCodeOf(t, resp) != "version_conflict" {
		t.Fatalf("承接 CAS: %d %s", resp.StatusCode, errCodeOf(t, resp))
	}
	resp = postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover/accept", `{"expected_version":1,"offer_id":"`+offerID+`","note":"已接手"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("承接: %d", resp.StatusCode)
	}
	doc = handoverJSON(t, resp)
	if doc["owner_agent_id"] != b || doc["pending_owner_agent_id"] != "" || doc["contract_version"] != float64(1) {
		t.Fatalf("承接视图: %v", doc)
	}
	// 再承接（无在途，旧锚）409
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover/accept", `{"expected_version":1,"offer_id":"`+offerID+`"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("无在途承接: %d", resp.StatusCode)
	}

	// 历史：offered + accepted（回执：status_at_accept=ready + note）
	_, got = getJSONMap(t, ts.URL+"/v1/missions/"+mid+"/handovers")
	evs, _ := got["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("历史应 2 事实: %v", got)
	}
	acc, _ := evs[1].(map[string]any)
	if acc["kind"] != "accepted" || acc["from_owner_agent_id"] != a || acc["to_owner_agent_id"] != b ||
		acc["status_at_accept"] != "ready" || acc["note"] != "已接手" || acc["offer_id"] != offerID {
		t.Fatalf("承接回执失真: %v", acc)
	}

	// 婉拒/撤回：另建任务 → 发起给丙 → 婉拒（留痕）→ 再发起 → 撤回（留痕）
	mid2, _ := mkOwnedMission(t, ts, b)
	c := mkAgent(t, ts, "丙2")
	resp = postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover", `{"expected_version":1,"new_owner_agent_id":"`+c+`"}`)
	doc2 := handoverJSON(t, resp)
	offer2, _ := doc2["pending_offer_id"].(string)
	// 审校 #1：撤回改派后，针对旧发起的婉拒 409
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover/revoke", `{"expected_version":1,"offer_id":"`+offer2+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("撤回2: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover/decline", `{"expected_version":1,"offer_id":"`+offer2+`"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("改派后旧婉拒须 409: %d", resp.StatusCode)
	}
	resp = postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover", `{"expected_version":1,"new_owner_agent_id":"`+c+`"}`)
	doc3 := handoverJSON(t, resp)
	offer3, _ := doc3["pending_offer_id"].(string)
	resp = postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover/decline", `{"expected_version":1,"offer_id":"`+offer3+`","reason":"暂不接手"}`)
	doc = handoverJSON(t, resp)
	if resp.StatusCode != http.StatusOK || doc["pending_owner_agent_id"] != "" || doc["owner_agent_id"] != b {
		t.Fatalf("婉拒: %d %v", resp.StatusCode, doc)
	}
	resp = postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover", `{"expected_version":1,"new_owner_agent_id":"`+c+`"}`)
	doc4 := handoverJSON(t, resp)
	offer4, _ := doc4["pending_offer_id"].(string)
	resp = postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover/revoke", `{"expected_version":1,"offer_id":"`+offer4+`","reason":"改派"}`)
	doc = handoverJSON(t, resp)
	if resp.StatusCode != http.StatusOK || doc["pending_owner_agent_id"] != "" {
		t.Fatalf("撤回: %d %v", resp.StatusCode, doc)
	}
	_, got = getJSONMap(t, ts.URL+"/v1/missions/"+mid2+"/handovers")
	evs, _ = got["events"].([]any)
	if len(evs) != 6 {
		t.Fatalf("留痕（offered/revoked/offered/declined/offered/revoked）: %d", len(evs))
	}
	if evs[1].(map[string]any)["kind"] != "revoked" || evs[3].(map[string]any)["kind"] != "declined" || evs[5].(map[string]any)["kind"] != "revoked" {
		t.Fatalf("婉拒/撤回留痕序: %v", got)
	}

	// 未知任务 404（写/读面）
	if resp := postSlot(t, ts.URL+"/v1/missions/mi_none/handover", `{"expected_version":1,"new_owner_agent_id":"`+b+`"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知任务发起: %d", resp.StatusCode)
	}
	if code, _ := getJSON(t, ts.URL+"/v1/missions/mi_none/handovers"); code != http.StatusNotFound {
		t.Fatalf("未知任务历史: %d", code)
	}
}

// TestHandoverRetirementGate 退役承诺门端到端（AT-03）：owned ready 任务 → retire
// 停留 retiring + 清单；发起而未承接仍计入；承接后清单空 → 重试 retired。
// suspended 承接人 offer 拒绝。
func TestHandoverRetirementGate(t *testing.T) {
	ts := handoverTestServer(t)
	a := mkAgent(t, ts, "甲")
	b := mkAgent(t, ts, "乙")
	mid, _ := mkOwnedMission(t, ts, a)

	// retire → retiring 停留 + 未处置清单含任务
	resp := postSlot(t, ts.URL+"/v1/identity/agents/"+a+"/lifecycle", `{"action":"retire"}`)
	var res struct {
		Agent       struct{ Status string } `json:"agent"`
		Outstanding []string                `json:"outstanding_commitments"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&res) != nil ||
		res.Agent.Status != "retiring" || len(res.Outstanding) != 1 || !strings.Contains(res.Outstanding[0], mid) {
		t.Fatalf("retire 门: %d %+v", resp.StatusCode, res)
	}
	resp.Body.Close()

	// retiring 的 A 仍是 owner——可发起交接（处置承诺正是 retiring 的语义）
	resp = postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover", `{"expected_version":1,"new_owner_agent_id":"`+b+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retiring 发起: %d", resp.StatusCode)
	}
	docR := handoverJSON(t, resp)
	oidR, _ := docR["pending_offer_id"].(string)
	if oidR == "" {
		t.Fatalf("retiring 发起须携锚: %v", docR)
	}
	// 未承接仍计入
	resp = postSlot(t, ts.URL+"/v1/identity/agents/"+a+"/lifecycle", `{"action":"retire"}`)
	if json.NewDecoder(resp.Body).Decode(&res) != nil || res.Agent.Status != "retiring" || len(res.Outstanding) != 1 {
		t.Fatalf("在途交接仍计入: %+v", res)
	}
	resp.Body.Close()

	// 承接（绑定 retiring 发起的锚）→ 清单空 → 重试 → retired
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid+"/handover/accept", `{"expected_version":1,"offer_id":"`+oidR+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("承接: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp = postSlot(t, ts.URL+"/v1/identity/agents/"+a+"/lifecycle", `{"action":"retire"}`)
	if json.NewDecoder(resp.Body).Decode(&res) != nil || res.Agent.Status != "retired" || len(res.Outstanding) != 0 {
		t.Fatalf("承接后退役: %+v", res)
	}
	resp.Body.Close()

	// retired 承接人不可接新承诺（offer 409 invalid_transition）
	mid2, _ := mkOwnedMission(t, ts, b)
	if resp := postSlot(t, ts.URL+"/v1/missions/"+mid2+"/handover",
		`{"expected_version":1,"new_owner_agent_id":"`+a+`"}`); resp.StatusCode != http.StatusConflict || errCodeOf(t, resp) != "invalid_transition" {
		t.Fatalf("retired 承接人: %d %s", resp.StatusCode, errCodeOf(t, resp))
	}
}
