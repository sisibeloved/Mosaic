package httpapi

// 0.6.13 / ADR-0036 API：网关续接 POST /v1/dispatch/update（幂等 + 复核 + revise
// 回执）与 resolve 的 slot 职责寻址字段。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/dispatch"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// fakeSlotLookups dispatch.SlotLookups 测试件（实例 → active 任职 Agent；指针
// 可变——先建服务注册真实身份再注入其 ID，precheck 的存在性检查才过得去）。
type fakeSlotLookups struct {
	active map[string]string
	specs  map[string][]string
}

func (f *fakeSlotLookups) SlotInstanceExists(_ context.Context, id string) (bool, error) {
	_, ok := f.active[id]
	if ok {
		return true, nil
	}
	return strings.HasPrefix(id, "si_"), nil
}

func (f *fakeSlotLookups) SlotInstancesOfSpec(_ context.Context, spec string) ([]string, error) {
	return f.specs[spec], nil
}

func (f *fakeSlotLookups) ActiveBindingAgent(_ context.Context, id string) (string, bool, error) {
	a, ok := f.active[id]
	return a, ok, nil
}

// GrantAdmission 授权准入（0.6.13 审校 #7）：缺省放行（API 件不测撤权分支——
// 域内 update_test 覆盖）。
func (f *fakeSlotLookups) GrantAdmission(_ context.Context, _, _ string) (bool, []string, error) {
	return true, nil, nil
}

// slotDispatchServer 装配 Slots 的 dispatch 测试件（寻址/续接共用）。
func slotDispatchServer(t *testing.T, slots dispatch.SlotLookups) (*httptest.Server, *mission.Service) {
	t.Helper()
	roomTasks := fakeRoomTasks{tasks: map[[2]string]bool{{"r1", "tsk_a"}: true, {"r1", "tsk_b"}: true}}
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-11-03T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_id" },
		Tenant: "ten_local",
	})
	var seq int64
	ident := personalagent.New(personalagent.NewMemStore(), emptyIdentityRegistry{},
		personalagent.WithNewAgentID(func() string { seq++; return "ag_d" + itoa(seq) }),
		personalagent.WithNow(func() string { return "2026-11-03T12:00:00.000Z" }))
	lookups := missionIdentAgents{ident}
	var n int64
	nextID := func() int64 { n++; return n }
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-11-03T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_api_" + itoa(nextID()) }),
		mission.WithAgentLookup(lookups),
		mission.WithSourceLookup(roomTasks))
	dSvc := dispatch.New(dispatch.NewMemStore(), mSvc, dispatch.Lookups{
		Agents: lookups, Sources: roomTasks, Mapped: mSvc.SourceMapped, Slots: slots,
	}, dispatch.WithNow(func() string { return "2026-11-03T12:00:00.000Z" }),
		dispatch.WithNewRouteID(func() string { return "rd_api_" + itoa(nextID()) }),
		dispatch.WithNewReceiptID(func() string { return "dpr_api_" + itoa(nextID()) }))
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:    room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Missions: mSvc, Dispatch: dSvc, Identity: ident,
		Seats: func() []room.AgentSeat {
			return []room.AgentSeat{{
				ParticipantID: "par_echo",
				Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
			}}
		},
	}))
	t.Cleanup(ts.Close)
	return ts, mSvc
}

// TestUpdateDispatchEndpoint 续接全链：修订成功（回执 domain_command_id 含
// :revise:v2）→ 同键重放（replayed）→ 同键异载荷 409 → 版本冲突 409 透传。
func TestUpdateDispatchEndpoint(t *testing.T) {
	ts, mSvc := slotDispatchServer(t, nil)
	ctx := context.Background()
	ag := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(ag.Body).Decode(&agDoc)
	ag.Body.Close()
	m, err := mSvc.Create(ctx, agDoc.AgentID, mission.Content{
		Objective: "原目标", Scope: "s", AcceptanceCriteria: []string{"a"},
		DeliverableSpecs: []string{"d"}, SourceRefs: []string{"r:1"},
	}, &mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"})
	if err != nil {
		t.Fatal(err)
	}

	resp := postSlot(t, ts.URL+"/v1/dispatch/update", `{"mission_id":"`+m.MissionID+`","expected_version":1,
		"idempotency_key":"idem-api-1","reason":"任务卡追加",
		"objective":"再加 Windows 与绑核结果","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	var rc struct {
		DomainCommandId string `json:"domain_command_id"`
		Replayed        bool   `json:"replayed"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&rc) != nil ||
		rc.DomainCommandId != m.MissionID+":revise:v2" || rc.Replayed {
		t.Fatalf("续接: %d %+v", resp.StatusCode, rc)
	}
	resp.Body.Close()
	// 同键同载荷重放
	resp = postSlot(t, ts.URL+"/v1/dispatch/update", `{"mission_id":"`+m.MissionID+`","expected_version":1,
		"idempotency_key":"idem-api-1","reason":"任务卡追加",
		"objective":"再加 Windows 与绑核结果","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if json.NewDecoder(resp.Body).Decode(&rc) != nil || !rc.Replayed {
		t.Fatalf("重放: %+v", rc)
	}
	resp.Body.Close()
	// 同键异载荷 409 idempotency_conflict
	bad := postSlot(t, ts.URL+"/v1/dispatch/update", `{"mission_id":"`+m.MissionID+`","expected_version":2,
		"idempotency_key":"idem-api-1",
		"objective":"换载荷","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	badCode, _ := errOf(t, bad)
	if bad.StatusCode != http.StatusConflict || badCode != "idempotency_conflict" {
		t.Fatalf("异载荷: %d %s", bad.StatusCode, badCode)
	}
	// 版本冲突透传（新键 + 旧版本）
	vc := postSlot(t, ts.URL+"/v1/dispatch/update", `{"mission_id":"`+m.MissionID+`","expected_version":9,
		"idempotency_key":"idem-api-2",
		"objective":"x","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	vcCode, _ := errOf(t, vc)
	if vc.StatusCode != http.StatusConflict || vcCode != "version_conflict" {
		t.Fatalf("版本冲突: %d %s", vc.StatusCode, vcCode)
	}
}

// TestResolveSlotEndpoint slot 寻址经 API：实例引用 resolved（selection_source=
// slot_binding）→ 投递建契约 owner=任职 Agent；owner+slot 并存 400。
func TestResolveSlotEndpoint(t *testing.T) {
	slots := &fakeSlotLookups{specs: map[string][]string{"slot.api": {"si_api_1"}}}
	ts, mSvc := slotDispatchServer(t, slots)
	// 注册真实身份（任职 Agent 须存在——precheck 同源复核）
	ag := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"任职者"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(ag.Body).Decode(&agDoc)
	ag.Body.Close()
	slots.active = map[string]string{"si_api_1": agDoc.AgentID}

	resp := postSlot(t, ts.URL+"/v1/dispatch/resolve", `{"kind":"create_mission","slot_instance_id":"si_api_1",
		"room_source":{"room_id":"r1","task_id":"tsk_a"},
		"objective":"按职责交办","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	var d struct {
		Status          string `json:"status"`
		SelectionSource string `json:"selection_source"`
		RouteId         string `json:"route_id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&d) != nil ||
		d.Status != "resolved" || d.SelectionSource != "slot_binding" {
		t.Fatalf("slot 解析: %d %+v", resp.StatusCode, d)
	}
	resp.Body.Close()
	// owner 并存 400
	both := postSlot(t, ts.URL+"/v1/dispatch/resolve", `{"kind":"create_mission","slot_instance_id":"si_api_1","owner_agent_id":"ag_x",
		"room_source":{"room_id":"r1","task_id":"tsk_b"},
		"objective":"o","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if both.StatusCode != http.StatusBadRequest {
		t.Fatalf("并存须 400: %d", both.StatusCode)
	}
	both.Body.Close()
	// 投递：owner = 任职 Agent
	rc := postSlot(t, ts.URL+"/v1/dispatch/dispatch", `{"route_id":"`+d.RouteId+`","idempotency_key":"idem-slot-1"}`)
	var rdoc struct {
		DomainCommandId string `json:"domain_command_id"`
	}
	if rc.StatusCode != http.StatusOK || json.NewDecoder(rc.Body).Decode(&rdoc) != nil || rdoc.DomainCommandId == "" {
		t.Fatalf("投递: %d %+v", rc.StatusCode, rdoc)
	}
	rc.Body.Close()
	m, ok, _ := mSvc.Get(context.Background(), rdoc.DomainCommandId)
	if !ok || m.OwnerAgentID != agDoc.AgentID {
		t.Fatalf("投递后 owner 须为任职 Agent: %+v", m)
	}
}
