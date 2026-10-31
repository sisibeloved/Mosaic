package httpapi

// 0.6.2 执行上下文清单端点（ADR-0025）：装配（assembled/blocked 恒 200）、
// 确定性重建重放、清单历史/详情、404/409/400 族、未装配 503。
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/execctx"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

type fakeExecBindings struct {
	facts map[string]execctx.BindingFacts
}

func (f fakeExecBindings) Facts(_ context.Context, bindingID string) (execctx.BindingFacts, bool, error) {
	facts, ok := f.facts[bindingID]
	return facts, ok, nil
}

type fakeExecReadiness struct{ ready bool }

func (f fakeExecReadiness) Check(_ context.Context, _ slot.Binding) (slot.ReadinessReport, error) {
	return slot.ReadinessReport{Ready: f.ready, Satisfied: []string{"runtime.required:net"}}, nil
}

type fakeExecGrants struct{ tools []string }

func (f fakeExecGrants) GrantByAgent(_ context.Context, _ string) (slot.AgentGrant, bool, error) {
	return slot.AgentGrant{Tools: f.tools}, true, nil
}

type fakeExecRooms struct {
	events map[string][]execctx.RoomEvent
}

func (f fakeExecRooms) RoomEvents(_ context.Context, roomID string) ([]execctx.RoomEvent, error) {
	return f.events[roomID], nil
}

// execctxTestServer 身份 + Mission + execctx（任职/授权/房间经端口注入）。
func execctxTestServer(t *testing.T, ready bool) (*httptest.Server, *mission.Service) {
	t.Helper()
	roomTasks := fakeRoomTasks{tasks: map[[2]string]bool{{"r1", "tsk_a"}: true}}
	var seq int64
	ident := personalagent.New(personalagent.NewMemStore(), emptyIdentityRegistry{},
		personalagent.WithNewAgentID(func() string { seq++; return "ag_e" + itoa(seq) }),
		personalagent.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }))
	lookups := missionIdentAgents{ident}
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_exec" }),
		mission.WithAgentLookup(lookups),
		mission.WithSourceLookup(roomTasks))
	spec := slot.SlotSpec{SlotSpecID: "ss_e", Version: 1, SpecContent: slot.SpecContent{
		Name: "执行者", Responsibility: "负责执行", Deliverables: []string{"产出"},
		Tools: &slot.CapabilityList{Required: []string{"web"}},
	}}
	bindings := fakeExecBindings{facts: map[string]execctx.BindingFacts{
		"bd_act": {Binding: slot.Binding{BindingID: "bd_act", AgentID: "ag_e1", SlotInstanceID: "si_e",
			SlotSpecVersion: 1, Status: slot.BindingActive}, Spec: spec},
		"bd_rel": {Binding: slot.Binding{BindingID: "bd_rel", AgentID: "ag_e1", SlotInstanceID: "si_e",
			SlotSpecVersion: 1, Status: slot.BindingReleased}, Spec: spec},
	}}
	var n int64
	eSvc := execctx.New(execctx.NewMemStore(), execctx.Ports{
		Missions:  mSvc,
		Bindings:  bindings,
		Readiness: fakeExecReadiness{ready: ready},
		Grants:    fakeExecGrants{tools: []string{"web"}},
		Rooms: fakeExecRooms{events: map[string][]execctx.RoomEvent{
			"r1": {
				{Seq: 2, EventID: "evt_2", Type: "message.posted", Visibility: "public",
					ParticipantID: "par_a", ActorKind: "agent", Body: "公开消息"},
				{Seq: 3, EventID: "evt_3", Type: "message.posted", Visibility: "participants",
					ParticipantID: "par_b", ActorKind: "agent", Body: "私有消息"},
			},
		}},
	}, execctx.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		execctx.WithNewIDs(func() string { n++; return "cmf_exec" + itoa(n) },
			func() string { n++; return "cxr_exec" + itoa(n) }))
	ts := httptest.NewServer(New(Deps{Missions: mSvc, Execctx: eSvc, Identity: ident}))
	t.Cleanup(ts.Close)
	return ts, mSvc
}

func createExecAgent(t *testing.T, ts *httptest.Server) {
	t.Helper()
	resp := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"执行者"}`)
	resp.Body.Close()
}

func execctxContent() mission.Content {
	return mission.Content{
		Objective: "目标", Scope: "范围",
		AcceptanceCriteria: []string{"验收"}, DeliverableSpecs: []string{"交付"},
		SourceRefs: []string{"r:1"}, Budget: mission.DefaultBudget(),
	}
}

// TestExecctxEndpoints 全链：装配（含 blocked 恒 200）→ 重建重放 → 历史/
// 详情 → 404/409/400 族。
func TestExecctxEndpoints(t *testing.T) {
	ts, mSvc := execctxTestServer(t, true)
	ctx := context.Background()
	createExecAgent(t, ts)

	ag, err := mSvc.Create(ctx, "ag_e1", execctxContent(), &mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"})
	if err != nil {
		t.Fatalf("建 Mission: %v", err)
	}

	// 装配成功：assembled、9 层、水位、回执同源
	resp := postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{"binding_id":"bd_act"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("装配期望 200，得到 %d", resp.StatusCode)
	}
	var doc struct {
		Manifest struct {
			ManifestID    string `json:"manifest_id"`
			State         string `json:"state"`
			ReasonCode    string `json:"reason_code"`
			Digest        string `json:"digest"`
			PolicyVersion string `json:"policy_version"`
			Layers        []struct {
				Name  string `json:"name"`
				State string `json:"state"`
				Bytes int    `json:"bytes"`
				Role  string `json:"role"`
			} `json:"layers"`
			Watermarks []struct {
				Stream   string `json:"stream"`
				Position int64  `json:"position"`
			} `json:"watermarks"`
			Receipt struct {
				ReceiptID string `json:"receipt_id"`
				Digest    string `json:"digest"`
			} `json:"receipt"`
		} `json:"manifest"`
		Replayed bool `json:"replayed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if doc.Manifest.State != "assembled" || doc.Manifest.ReasonCode != "" || doc.Replayed {
		t.Fatalf("装配结果错误: %+v replayed=%v", doc.Manifest, doc.Replayed)
	}
	if len(doc.Manifest.Layers) != 9 {
		t.Fatalf("期望 9 层，得到 %d", len(doc.Manifest.Layers))
	}
	if doc.Manifest.PolicyVersion != "context-assembly/1" {
		t.Fatalf("政策版本错误: %s", doc.Manifest.PolicyVersion)
	}
	// 私有事件不进入不计数：水位 = 实际交付的 evt_2（seq 2），非读取前沿 3
	if len(doc.Manifest.Watermarks) != 1 || doc.Manifest.Watermarks[0].Stream != "room:r1" ||
		doc.Manifest.Watermarks[0].Position != 2 {
		t.Fatalf("水位错误: %+v", doc.Manifest.Watermarks)
	}
	if doc.Manifest.Receipt.ReceiptID == "" || doc.Manifest.Receipt.Digest != doc.Manifest.Digest {
		t.Fatalf("回执关联错误: %+v", doc.Manifest.Receipt)
	}

	// 确定性重建 → replayed=true（digest 即身份）
	resp = postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{"binding_id":"bd_act"}`)
	var replay struct {
		Replayed bool `json:"replayed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&replay)
	resp.Body.Close()
	if !replay.Replayed {
		t.Fatal("同状态重建应 replayed=true")
	}

	// 任职非活跃 → blocked 恒 200 携原因
	resp = postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{"binding_id":"bd_rel"}`)
	var blocked struct {
		Manifest struct {
			State      string `json:"state"`
			ReasonCode string `json:"reason_code"`
		} `json:"manifest"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&blocked)
	if resp.StatusCode != http.StatusOK || blocked.Manifest.State != "blocked" ||
		blocked.Manifest.ReasonCode != "binding_not_active" {
		t.Fatalf("blocked 期望 200/binding_not_active，得到 %d %+v", resp.StatusCode, blocked.Manifest)
	}

	// 历史（创建序倒序）与详情
	resp = postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{"binding_id":"bd_act"}`)
	resp.Body.Close() // 无变化 → 重放（不新增行）
	resp, _ = http.Get(ts.URL + "/v1/missions/" + ag.MissionID + "/context-manifests")
	var list struct {
		Manifests []struct {
			ManifestID string `json:"manifest_id"`
			State      string `json:"state"`
		} `json:"manifests"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(list.Manifests) != 2 {
		t.Fatalf("装配历史期望 2 条（assembled + blocked），得到 %d", len(list.Manifests))
	}
	resp, _ = http.Get(ts.URL + "/v1/context-manifests/" + doc.Manifest.ManifestID)
	var detail struct {
		Layers []struct {
			Name    string         `json:"name"`
			Content map[string]any `json:"content"`
		} `json:"layers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&detail)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(detail.Layers) != 9 || detail.Layers[0].Content == nil {
		t.Fatalf("详情期望含层内容块: %d 层", len(detail.Layers))
	}

	// 错误族
	resp = postSlot(t, ts.URL+"/v1/missions/mi_none/context-manifests", `{"binding_id":"bd_act"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "mission_not_found" {
		t.Fatalf("未知 Mission 期望 404/mission_not_found，得到 %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{"binding_id":"bd_none"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "binding_not_found" {
		t.Fatalf("未知任职期望 404/binding_not_found，得到 %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusBadRequest || code != "bad_request" {
		t.Fatalf("缺 binding_id 期望 400/bad_request，得到 %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{"binding_id":"bd_act","x":1}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusBadRequest || code != "bad_request" {
		t.Fatalf("未知字段期望 400/bad_request，得到 %d/%s", resp.StatusCode, code)
	}

	// canceled → 409
	ag2, err := mSvc.Create(ctx, "ag_e1", execctxContent(), nil)
	if err != nil {
		t.Fatalf("建 Mission2: %v", err)
	}
	if _, err := mSvc.Cancel(ctx, ag2.MissionID, 1, "不再需要"); err != nil {
		t.Fatalf("取消: %v", err)
	}
	resp = postSlot(t, ts.URL+"/v1/missions/"+ag2.MissionID+"/context-manifests", `{"binding_id":"bd_act"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusConflict || code != "mission_canceled" {
		t.Fatalf("canceled 期望 409/mission_canceled，得到 %d/%s", resp.StatusCode, code)
	}

	// 查面 404 族
	resp, _ = http.Get(ts.URL + "/v1/missions/mi_none/context-manifests")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "mission_not_found" {
		t.Fatalf("未知 Mission 列表期望 404，得到 %d/%s", resp.StatusCode, code)
	}
	resp, _ = http.Get(ts.URL + "/v1/context-manifests/cmf_none")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "manifest_not_found" {
		t.Fatalf("未知清单期望 404/manifest_not_found，得到 %d/%s", resp.StatusCode, code)
	}
}

// TestExecctxEndpointsReadinessBlocked 授权缺失 → 200 blocked
// readiness_not_met（probe 纪律——blocked 是数据不是错误）。
func TestExecctxEndpointsReadinessBlocked(t *testing.T) {
	ts, mSvc := execctxTestServer(t, false)
	ctx := context.Background()
	createExecAgent(t, ts)
	ag, err := mSvc.Create(ctx, "ag_e1", execctxContent(), nil)
	if err != nil {
		t.Fatalf("建 Mission: %v", err)
	}
	resp := postSlot(t, ts.URL+"/v1/missions/"+ag.MissionID+"/context-manifests", `{"binding_id":"bd_act"}`)
	var blocked struct {
		Manifest struct {
			State      string `json:"state"`
			ReasonCode string `json:"reason_code"`
		} `json:"manifest"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&blocked)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || blocked.Manifest.State != "blocked" ||
		blocked.Manifest.ReasonCode != "readiness_not_met" {
		t.Fatalf("期望 200 blocked readiness_not_met，得到 %d %+v", resp.StatusCode, blocked.Manifest)
	}
}

// TestExecctxUnavailable 未装配 → 503 execctx_unavailable。
func TestExecctxUnavailable(t *testing.T) {
	ts := httptest.NewServer(New(Deps{}))
	t.Cleanup(ts.Close)
	resp := postSlot(t, ts.URL+"/v1/missions/mi_x/context-manifests", `{"binding_id":"bd_1"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "execctx_unavailable" {
		t.Fatalf("装配期望 503/execctx_unavailable，得到 %d/%s", resp.StatusCode, code)
	}
	resp, _ = http.Get(ts.URL + "/v1/missions/mi_x/context-manifests")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "execctx_unavailable" {
		t.Fatalf("列表期望 503，得到 %d/%s", resp.StatusCode, code)
	}
	resp, _ = http.Get(ts.URL + "/v1/context-manifests/cmf_x")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "execctx_unavailable" {
		t.Fatalf("详情期望 503，得到 %d/%s", resp.StatusCode, code)
	}
}
