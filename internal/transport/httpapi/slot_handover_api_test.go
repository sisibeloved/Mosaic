package httpapi

// 0.7.9 / ADR-0046：Slot 换 Agent 交接端点 IT——真装配（身份 + slot + mission
// + memory 域经端口闭包连 HandoverService）：全链回执/任务转移/绑定变迁/
// 承接回执事件 + 错误族（401/404/409）。测试先行（红→绿）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/memory"
	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/slot"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// handoverTestServer 全域装配（生产形制的端口闭包镜像 app 装配）。
func slotHandoverServer(t *testing.T) (*httptest.Server, *mission.Service, *slot.Service, *mission.MemStore) {
	t.Helper()
	now := func() string { return "2026-11-12T12:00:00Z" }
	var seq int64
	ident := personalagent.New(personalagent.NewMemStore(), emptyIdentityRegistry{},
		personalagent.WithNewAgentID(func() string { seq++; return fmt.Sprintf("ag_h%d", seq) }),
		personalagent.WithNow(now))
	lookups := missionIdentAgents{ident}
	mStore := mission.NewMemStore()
	mSvc := mission.New(mStore, mission.WithNow(now),
		mission.WithNewMissionID(func() string { return "mi_h1" }),
		mission.WithAgentLookup(lookups))
	var bs int64
	sSvc := slot.New(slot.NewMemStore(), slot.WithNow(now),
		slot.WithNewInstanceID(func() string { bs++; return fmt.Sprintf("si_h%d", bs) }),
		slot.WithNewNamespace(func() string { bs++; return fmt.Sprintf("slotns_h%d", bs) }),
		slot.WithNewBindingID(func() string { bs++; return fmt.Sprintf("bd_h%d", bs) }),
		slot.WithAgentLookup(lookups))
	memStore := memory.NewMemStore()
	memSvc := memory.NewService(memStore, memory.Anchors{}, fixedMemNow)

	// 端口闭包（app 装配同款）：mission 读/转 + memory 清单。
	ports := slot.HandoverPorts{
		MissionsOfSlot: func(ctx context.Context, si string) ([]slot.HandoverMission, error) {
			ms, err := mSvc.ListOfSlot(ctx, si)
			if err != nil {
				return nil, err
			}
			out := make([]slot.HandoverMission, 0, len(ms))
			for _, m := range ms {
				out = append(out, slot.HandoverMission{
					MissionID: m.MissionID, Objective: m.Objective, OwnerAgentID: m.OwnerAgentID,
					Status: m.Status, PendingOwnerAgentID: m.PendingOwnerAgentID, SlotInstanceID: m.SlotInstanceID,
					Transferable: mission.HandoverOfferable(m.Status),
				})
			}
			return out, nil
		},
		ReassignMission: func(ctx context.Context, missionID, fromOwner, toOwner string) (string, bool, error) {
			out, ok, err := mSvc.ReassignForSlot(ctx, missionID, fromOwner, toOwner)
			if err != nil || !ok {
				return "", false, err
			}
			return out.Status, true, nil
		},
		MissionStatusOf: func(ctx context.Context, missionID string) (slot.HandoverMission, bool, error) {
			m, ok, err := mSvc.Get(ctx, missionID)
			if err != nil || !ok {
				return slot.HandoverMission{}, false, err
			}
			return slot.HandoverMission{
				MissionID: m.MissionID, Objective: m.Objective, OwnerAgentID: m.OwnerAgentID,
				Status: m.Status, PendingOwnerAgentID: m.PendingOwnerAgentID, SlotInstanceID: m.SlotInstanceID,
			}, true, nil
		},
		SlotKnowledgeCounts: func(ctx context.Context, scopeID string) (map[string]int, error) {
			out := map[string]int{}
			for _, st := range []memory.Status{memory.StatusActive, memory.StatusProposed, memory.StatusSuperseded, memory.StatusDeleted} {
				res, err := memSvc.List(ctx, memory.ListFilter{
					Namespace: memory.NSSlot, ScopeID: scopeID, Status: st, Limit: 100, IncludeExpired: true,
				})
				if err != nil {
					return nil, err
				}
				out[string(st)] = res.TotalHits
			}
			return out, nil
		},
		AgentPrivateMemoryCount: func(ctx context.Context, agentID string) (int, error) {
			res, err := memSvc.List(ctx, memory.ListFilter{
				Namespace: memory.NSAgent, ScopeID: agentID, Limit: 100, IncludeExpired: true,
			})
			if err != nil {
				return 0, err
			}
			return res.TotalHits, nil
		},
		HandoverTransferFactsOfSlot: func(ctx context.Context, slotInstanceID, ownerAgentID string) ([]slot.HandoverTransferFact, error) {
			facts, err := mSvc.SlotHandoverTransfersOf(ctx, slotInstanceID, ownerAgentID)
			if err != nil {
				return nil, err
			}
			out := make([]slot.HandoverTransferFact, 0, len(facts))
			for _, f := range facts {
				out = append(out, slot.HandoverTransferFact{MissionID: f.MissionID, FromOwner: f.FromOwner})
			}
			return out, nil
		},
	}
	hSvc := slot.NewHandoverService(sSvc, ports, slot.WithHandoverNow(now))

	ts := httptest.NewServer(New(Deps{
		Identity: ident, Slots: sSvc, SlotHandover: hSvc, Missions: mSvc,
		MemorySVC: memSvc, Hub: sse.NewHub(),
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: "tok",
	}))
	t.Cleanup(ts.Close)
	return ts, mSvc, sSvc, mStore
}

// handoverSeed 建域数据：谱系/实例/ag_h1 在任 + 在办任务（经存储直挂实例）+
// 职责知识/私有记忆。返回实例 ID。
func handoverSeed(t *testing.T, ts *httptest.Server, mStore *mission.MemStore) string {
	t.Helper()
	// 两位 Agent。
	for _, name := range []string{"甲", "乙"} {
		r := postAuth(ts.URL+"/v1/identity/agents", "tok", fmt.Sprintf(`{"display_name":%q}`, name))
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("建 Agent: %d", r.StatusCode)
		}
	}
	// 谱系 + 实例。
	body := `{"slot_spec_id":"slot.ho","name":"值班","responsibility":"负责日常值班","deliverables":["日报"]}`
	r := postAuth(ts.URL+"/v1/slots/specs", "tok", body)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("建谱系: %d", r.StatusCode)
	}
	r = postAuth(ts.URL+"/v1/slots/instances", "tok", `{"slot_spec_id":"slot.ho"}`)
	var inst struct {
		SlotInstanceID string `json:"slot_instance_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&inst)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated || inst.SlotInstanceID == "" {
		t.Fatalf("建实例: %d %+v", r.StatusCode, inst)
	}
	si := inst.SlotInstanceID
	// ag_h1 任职（prepare → activate）。
	r = postAuth(ts.URL+"/v1/slots/bindings", "tok", fmt.Sprintf(`{"slot_instance_id":%q,"agent_id":"ag_h1"}`, si))
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("建任职: %d", r.StatusCode)
	}
	var binds struct {
		Bindings []struct {
			BindingID string `json:"binding_id"`
		} `json:"bindings"`
	}
	r2, _ := http.Get(ts.URL + "/v1/slots/bindings?slot_instance_id=" + si)
	_ = json.NewDecoder(r2.Body).Decode(&binds)
	r2.Body.Close()
	if len(binds.Bindings) == 0 {
		t.Fatal("任职列表空")
	}
	r = postAuth(ts.URL+"/v1/slots/bindings/"+binds.Bindings[0].BindingID+"/activate", "tok", `{}`)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("激活: %d", r.StatusCode)
	}

	// 在办任务两条（ready 挂实例 + reviewing 挂实例——经存储直挂，聚焦交接面）。
	ctx := context.Background()
	for _, m := range []mission.Mission{
		{MissionID: "mi_hm1", ContractVersion: 1, Status: mission.StatusReady,
			OwnerAgentID: "ag_h1", SlotInstanceID: si, Requester: mission.RequesterOwner,
			Content:   handoverContent(),
			CreatedAt: "2026-11-12T11:00:00Z", UpdatedAt: "2026-11-12T11:00:00Z"},
		{MissionID: "mi_hm2", ContractVersion: 1, Status: mission.StatusReviewing,
			OwnerAgentID: "ag_h1", SlotInstanceID: si, ReviewPackageID: "dlv_h1", ReviewRound: 1,
			Requester: mission.RequesterOwner,
			Content:   handoverContent(),
			CreatedAt: "2026-11-12T11:01:00Z", UpdatedAt: "2026-11-12T11:01:00Z"},
	} {
		if _, err := mStore.CreateMission(ctx, m, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 职责知识两条 + 上任私有记忆一条。
	for _, mk := range []struct{ ns, scope, title string }{
		{"slot", si, "值班交接要点"}, {"slot", si, "日报口径"},
		{"agent", "ag_h1", "私人偏好"},
	} {
		r := postAuth(ts.URL+"/v1/memory/items", "tok", fmt.Sprintf(
			`{"namespace":%q,"scope_id":%q,"kind":"semantic","title":%q,"content":"…","observed_at":"2026-11-01T00:00:00Z"}`,
			mk.ns, mk.scope, mk.title))
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("建记忆 %s: %d", mk.title, r.StatusCode)
		}
	}
	return si
}

func handoverContent() mission.Content {
	return mission.Content{
		Objective: "值班任务", Scope: "范围",
		AcceptanceCriteria: []string{"验收"}, DeliverableSpecs: []string{"日报"},
		SourceRefs: []string{"r:1"}, Budget: mission.DefaultBudget(),
	}
}

func postAuth(url, token, body string) *http.Response {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

// TestSlotHandoverEndpoint 全链：回执（转移/不转移/知识清单/排除面）+ 任务
// owner 翻转 paused + 绑定变迁 + 承接回执事件 + 重放幂等。
func TestSlotHandoverEndpoint(t *testing.T) {
	ts, mSvc, sSvc, mStore := slotHandoverServer(t)
	ctx := context.Background()
	si := handoverSeed(t, ts, mStore)

	// 交接。
	r := postAuth(ts.URL+"/v1/slots/instances/"+si+"/handover", "tok",
		`{"new_agent_id":"ag_h2","notes":"注意日报口径"}`)
	if r.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("交接: %d %s", r.StatusCode, b)
	}
	var rep struct {
		FromAgentID         string `json:"from_agent_id"`
		ToAgentID           string `json:"to_agent_id"`
		MissionsTransferred []struct {
			MissionID  string `json:"mission_id"`
			FromStatus string `json:"from_status"`
			Status     string `json:"status"`
		} `json:"missions_transferred"`
		MissionsNotTransferred []struct {
			MissionID string `json:"mission_id"`
			Status    string `json:"status"`
			Reason    string `json:"reason"`
		} `json:"missions_not_transferred"`
		KnowledgeInventory   map[string]int `json:"knowledge_inventory"`
		ExcludedPrivateCount int            `json:"excluded_private_count"`
		Excluded             []string       `json:"excluded"`
		ReleasedBindingID    string         `json:"released_binding_id"`
		ActivatedBindingID   string         `json:"activated_binding_id"`
		KnowledgeNamespace   string         `json:"knowledge_namespace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if rep.FromAgentID != "ag_h1" || rep.ToAgentID != "ag_h2" {
		t.Fatalf("归属: %+v", rep)
	}
	if len(rep.MissionsTransferred) != 1 || rep.MissionsTransferred[0].MissionID != "mi_hm1" ||
		rep.MissionsTransferred[0].FromStatus != "ready" || rep.MissionsTransferred[0].Status != "paused" {
		t.Fatalf("转移清单: %+v", rep.MissionsTransferred)
	}
	if len(rep.MissionsNotTransferred) != 1 || rep.MissionsNotTransferred[0].MissionID != "mi_hm2" ||
		rep.MissionsNotTransferred[0].Status != "reviewing" || rep.MissionsNotTransferred[0].Reason == "" {
		t.Fatalf("不转移清单: %+v", rep.MissionsNotTransferred)
	}
	if rep.KnowledgeInventory["active"] != 2 {
		t.Fatalf("知识清单: %+v", rep.KnowledgeInventory)
	}
	if rep.ExcludedPrivateCount != 1 || len(rep.Excluded) == 0 {
		t.Fatalf("排除面: %d %v", rep.ExcludedPrivateCount, rep.Excluded)
	}
	if rep.ReleasedBindingID == "" || rep.ActivatedBindingID == "" || rep.ReleasedBindingID == rep.ActivatedBindingID {
		t.Fatalf("绑定变迁: %+v", rep)
	}
	if !strings.HasPrefix(rep.KnowledgeNamespace, "slotns_") {
		t.Fatalf("知识命名空间: %q", rep.KnowledgeNamespace)
	}

	// 任务终态：owner 翻转 + paused（reviewing 保持归属与状态）。
	m1, _, _ := mSvc.Get(ctx, "mi_hm1")
	if m1.OwnerAgentID != "ag_h2" || m1.Status != mission.StatusPaused {
		t.Fatalf("mi_hm1: %+v", m1)
	}
	m2, _, _ := mSvc.Get(ctx, "mi_hm2")
	if m2.OwnerAgentID != "ag_h1" || m2.Status != mission.StatusReviewing {
		t.Fatalf("mi_hm2 不得动物: %+v", m2)
	}

	// 绑定终态：旧任 released / 新任 active；承接回执事件在新任流。
	newB, ok := func() (slot.Binding, bool) {
		bs, _ := sSvc.ListBindings(ctx, si)
		for _, b := range bs {
			if b.BindingID == rep.ActivatedBindingID {
				return b, true
			}
		}
		return slot.Binding{}, false
	}()
	if !ok || newB.Status != slot.BindingActive || newB.AgentID != "ag_h2" {
		t.Fatalf("新任: %+v", newB)
	}
	evs, _, _ := sSvc.BindingEvents(ctx, rep.ActivatedBindingID)
	found := false
	for _, e := range evs {
		if e.EventType == "slot_handover" && strings.Contains(string(e.Payload), "ag_h2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("承接回执事件: %+v", evs)
	}

	// 重放幂等：再次交接返回当前态（转移清单空、不新建绑定）。
	r = postAuth(ts.URL+"/v1/slots/instances/"+si+"/handover", "tok", `{"new_agent_id":"ag_h2"}`)
	if r.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("重放: %d %s", r.StatusCode, b)
	}
	var rep2 struct {
		MissionsTransferred []json.RawMessage `json:"missions_transferred"`
		FromAgentID         string            `json:"from_agent_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&rep2)
	r.Body.Close()
	if len(rep2.MissionsTransferred) != 0 || rep2.FromAgentID != "ag_h1" {
		t.Fatalf("重放回执: %+v", rep2)
	}
}

// TestSlotHandoverErrorFamily 错误族：401 / 404 / 409（同 Agent、在途单任务交接）。
func TestSlotHandoverErrorFamily(t *testing.T) {
	ts, mSvc, _, mStore := slotHandoverServer(t)
	ctx := context.Background()
	si := handoverSeed(t, ts, mStore)

	// 401：缺令牌。
	r := postAuth(ts.URL+"/v1/slots/instances/"+si+"/handover", "", `{"new_agent_id":"ag_h2"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("401: %d", r.StatusCode)
	}
	// 404：实例未知。
	r = postAuth(ts.URL+"/v1/slots/instances/si_ghost/handover", "tok", `{"new_agent_id":"ag_h2"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("404: %d", r.StatusCode)
	}
	// 409：新任即在任。
	r = postAuth(ts.URL+"/v1/slots/instances/"+si+"/handover", "tok", `{"new_agent_id":"ag_h1"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("409 同任: %d", r.StatusCode)
	}
	// 409：在途单任务交接（0.6.12 offer）——换将不静默吞承诺，先了结。
	if _, err := mSvc.HandoverOffer(ctx, "mi_hm1", 1, "ag_h2", ""); err != nil {
		t.Fatalf("offer（前置）: %v", err)
	}
	r = postAuth(ts.URL+"/v1/slots/instances/"+si+"/handover", "tok", `{"new_agent_id":"ag_h2"}`)
	if r.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("409 在途: %d %s", r.StatusCode, b)
	}
	r.Body.Close()
}
