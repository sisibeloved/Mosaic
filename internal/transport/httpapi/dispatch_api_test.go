package httpapi

// 0.6.1 网关首切片端点（ADR-0024）：resolve resolved/blocked（恒 200）、
// dispatch 幂等三态、回执查询、mission 创建 room_source 与映射冲突。
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

type fakeRoomTasks struct{ tasks map[[2]string]bool }

func (f fakeRoomTasks) RoomTaskExists(_ context.Context, roomID, taskID string) (bool, error) {
	return f.tasks[[2]string{roomID, taskID}], nil
}

// dispatchTestServer 身份 + Mission（含来源端口）+ 网关装配。
func dispatchTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	roomTasks := fakeRoomTasks{tasks: map[[2]string]bool{{"r1", "tsk_a"}: true}}
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-10-30T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_id" },
		Tenant: "ten_local",
	})
	var seq int64
	ident := personalagent.New(personalagent.NewMemStore(), emptyIdentityRegistry{},
		personalagent.WithNewAgentID(func() string { seq++; return "ag_d" + itoa(seq) }),
		personalagent.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }))
	lookups := missionIdentAgents{ident}
	var n int64
	nextID := func() int64 { n++; return n }
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_api_" + itoa(nextID()) }),
		mission.WithAgentLookup(lookups),
		mission.WithSourceLookup(roomTasks))
	dSvc := dispatch.New(dispatch.NewMemStore(), mSvc, dispatch.Lookups{
		Agents: lookups, Sources: roomTasks, Mapped: mSvc.SourceMapped,
	}, dispatch.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
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
	return ts
}

// errOf 单次解码取 code 与 message（响应体只可读一次）。
func errOf(t *testing.T, resp *http.Response) (code, message string) {
	t.Helper()
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return doc.Error.Code, doc.Error.Message
}

// dispatchMissionEndpoints 全链：resolve（resolved/blocked）→ dispatch 建契
// 约（含映射）→ 幂等重放/同键异路由冲突 → 回执查询 → mission 直建来源冲突。
func TestDispatchEndpoints(t *testing.T) {
	ts := dispatchTestServer(t)
	ctx := context.Background()

	agentResp := postSlot(t, ts.URL+"/v1/identity/agents", `{"display_name":"负责人"}`)
	var agDoc struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(agentResp.Body).Decode(&agDoc)
	agentResp.Body.Close()

	resolveBody := `{"kind":"create_mission","room_source":{"room_id":"r1","task_id":"tsk_a"},` +
		`"owner_agent_id":"` + agDoc.AgentID + `","objective":"房间交办","scope":"s",` +
		`"acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["room:r1:tsk_a"]}`

	// 1) resolve → resolved（恒 200）
	resp := postSlot(t, ts.URL+"/v1/dispatch/resolve", resolveBody)
	var d struct {
		RouteID       string `json:"route_id"`
		Status        string `json:"status"`
		RequestDigest string `json:"request_digest"`
		ExpiresAt     string `json:"expires_at"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || d.Status != "resolved" || d.RouteID == "" || d.ExpiresAt == "" {
		t.Fatalf("resolve: %d %+v", resp.StatusCode, d)
	}

	// 2) blocked 决策（任务卡不存在）也是 200
	bad := postSlot(t, ts.URL+"/v1/dispatch/resolve", strings.Replace(resolveBody, "tsk_a", "tsk_z", -1))
	var bd struct {
		Status     string `json:"status"`
		ReasonCode string `json:"reason_code"`
	}
	_ = json.NewDecoder(bad.Body).Decode(&bd)
	bad.Body.Close()
	if bad.StatusCode != http.StatusOK || bd.Status != "blocked" || bd.ReasonCode != "task_not_found" {
		t.Fatalf("blocked 决策: %d %+v", bad.StatusCode, bd)
	}
	// 非法 kind → 400
	inv := postSlot(t, ts.URL+"/v1/dispatch/resolve", strings.Replace(resolveBody, "create_mission", "semantic", -1))
	if inv.StatusCode != http.StatusBadRequest || errCodeOf(t, inv) != "invalid_request" {
		t.Fatalf("非法 kind: %d %s", inv.StatusCode, errCodeOf(t, inv))
	}

	// 3) dispatch → 回执（mission 建立 + 映射）
	r1 := postSlot(t, ts.URL+"/v1/dispatch/dispatch", `{"route_id":"`+d.RouteID+`","idempotency_key":"k1","request_id":"req-1"}`)
	var receipt struct {
		ReceiptID       string `json:"receipt_id"`
		DomainCommandID string `json:"domain_command_id"`
		Replayed        bool   `json:"replayed"`
	}
	_ = json.NewDecoder(r1.Body).Decode(&receipt)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK || receipt.DomainCommandID == "" || receipt.Replayed {
		t.Fatalf("dispatch: %d %+v", r1.StatusCode, receipt)
	}
	det, _ := http.Get(ts.URL + "/v1/missions/" + receipt.DomainCommandID)
	var mDoc struct {
		Status     string `json:"status"`
		RoomSource *struct {
			RoomID string `json:"room_id"`
			TaskID string `json:"task_id"`
		} `json:"room_source"`
	}
	_ = json.NewDecoder(det.Body).Decode(&mDoc)
	det.Body.Close()
	if mDoc.Status != "ready" || mDoc.RoomSource == nil || mDoc.RoomSource.TaskID != "tsk_a" {
		t.Fatalf("Mission 来源失真: %+v", mDoc)
	}

	// 4) 幂等：同键同路由重放（replayed=true）；同键异路由 409
	r2 := postSlot(t, ts.URL+"/v1/dispatch/dispatch", `{"route_id":"`+d.RouteID+`","idempotency_key":"k1"}`)
	var receipt2 struct {
		ReceiptID string `json:"receipt_id"`
		Replayed  bool   `json:"replayed"`
	}
	_ = json.NewDecoder(r2.Body).Decode(&receipt2)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK || !receipt2.Replayed || receipt2.ReceiptID != receipt.ReceiptID {
		t.Fatalf("重放: %d %+v", r2.StatusCode, receipt2)
	}
	// 同键异路由 → 409 idempotency_conflict（幂等检查先于决策载入——
	// 已绑定键不得改投他路由）
	conf := postSlot(t, ts.URL+"/v1/dispatch/dispatch", `{"route_id":"rd_none_x","idempotency_key":"k1"}`)
	if conf.StatusCode != http.StatusConflict || errCodeOf(t, conf) != "idempotency_conflict" {
		t.Fatalf("同键异路由: %d %s", conf.StatusCode, errCodeOf(t, conf))
	}
	// 未绑定键 + 未知路由 → 404 route_not_found
	nfRoute := postSlot(t, ts.URL+"/v1/dispatch/dispatch", `{"route_id":"rd_none_x","idempotency_key":"k-fresh"}`)
	if nfRoute.StatusCode != http.StatusNotFound || errCodeOf(t, nfRoute) != "route_not_found" {
		t.Fatalf("未知路由: %d %s", nfRoute.StatusCode, errCodeOf(t, nfRoute))
	}

	// 5) 回执查询 + 404
	q, _ := http.Get(ts.URL + "/v1/dispatch/receipts?idempotency_key=k1")
	var qDoc struct {
		ReceiptID string `json:"receipt_id"`
	}
	_ = json.NewDecoder(q.Body).Decode(&qDoc)
	q.Body.Close()
	if q.StatusCode != http.StatusOK || qDoc.ReceiptID != receipt.ReceiptID {
		t.Fatalf("回执查询: %d %+v", q.StatusCode, qDoc)
	}
	nf, _ := http.Get(ts.URL + "/v1/dispatch/receipts?idempotency_key=nope")
	if nf.StatusCode != http.StatusNotFound || errCodeOf(t, nf) != "receipt_not_found" {
		t.Fatalf("回执 404: %d %s", nf.StatusCode, errCodeOf(t, nf))
	}

	// 6) mission 直建带来源：同任务卡 → 409 source_already_mapped；未知任务卡 → 400
	dup := postSlot(t, ts.URL+"/v1/missions", `{"room_source":{"room_id":"r1","task_id":"tsk_a"},"objective":"o","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	dupCode, dupMsg := errOf(t, dup)
	if dup.StatusCode != http.StatusConflict || dupCode != "source_already_mapped" {
		t.Fatalf("映射冲突: %d %s", dup.StatusCode, dupCode)
	}
	if !strings.Contains(dupMsg, receipt.DomainCommandID) {
		t.Fatalf("冲突应回显已映射 mission_id（%s）", dupMsg)
	}
	unk := postSlot(t, ts.URL+"/v1/missions", `{"room_source":{"room_id":"r1","task_id":"tsk_none"},"objective":"o","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"]}`)
	unkCode, _ := errOf(t, unk)
	if unk.StatusCode != http.StatusBadRequest || unkCode != "invalid_mission" {
		t.Fatalf("未知任务卡: %d %s", unk.StatusCode, unkCode)
	}
	_ = ctx
}

// TestDispatchResolveContractChecks（审校 P2#5）：网关入口与直接创建同源
// 校验——缺必填键 / 显式全零预算 → 400，不得以空列表或默认预算入库。
func TestDispatchResolveContractChecks(t *testing.T) {
	ts := dispatchTestServer(t)

	// 缺验收键 → 400 bad_request
	missing := postSlot(t, ts.URL+"/v1/dispatch/resolve",
		`{"kind":"create_mission","room_source":{"room_id":"r1","task_id":"tsk_a"},"objective":"o","scope":"s","deliverable_specs":["d"],"source_refs":["r:1"]}`)
	if code, _ := errOf(t, missing); missing.StatusCode != http.StatusBadRequest || code != "bad_request" {
		t.Fatalf("缺验收键应 400 bad_request: %d %s", missing.StatusCode, code)
	}
	// 显式全零预算 → 400 invalid_mission
	zero := postSlot(t, ts.URL+"/v1/dispatch/resolve",
		`{"kind":"create_mission","room_source":{"room_id":"r1","task_id":"tsk_a"},"objective":"o","scope":"s","acceptance_criteria":["a"],"deliverable_specs":["d"],"source_refs":["r:1"],"budget":{"concurrency":0,"max_runs":0,"max_wall_clock_seconds":0,"max_retries":0,"max_delegation_depth":0}}`)
	if code, _ := errOf(t, zero); zero.StatusCode != http.StatusBadRequest || code != "invalid_mission" {
		t.Fatalf("全零预算应 400 invalid_mission: %d %s", zero.StatusCode, code)
	}
}
