package slot

// 0.7.9 / ADR-0046：Slot 换 Agent 交接——编排步骤、单写者不变量、零变更失败
// 族、重入收敛、回执与承接回执事件；v0.2 一审反例钉住（并发串行化/遗留转移
// 拒改目标/排空边界先于收集/曾任 active 旧任识别/激活-回执原子）。fake Ports
// 承载 mission/memory 域边界，slot 域本体用真 Service + memstore。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ---- fake Ports（mission / memory 域边界替身） ----

type fakeHandoverPorts struct {
	mu        sync.Mutex
	missions  []*HandoverMission // 端口侧任务表（fake 自维护归属/状态翻转）
	reassigns []string           // 转移调用序（missionID）
	failOn    map[string]error   // 注入失败（missionID → error）
	// intercept 转移事务入口注入（模拟收集与转移之间的并发归属变化——
	// 0.6.12 承接抢先/第三方改派；或断言边界时序）。
	intercept func(missionID string, missions []*HandoverMission)
	// onCollect 收集回调（第 n 次收集——预检/权威两读；注入"边界后新进任务"）。
	onCollect func(n int, missions *[]*HandoverMission)
	collects  int
	knowledge map[string]int
	private   int
	// transferFacts 换将转移持久事实替身（mission_events 侧——每次 reassign
	// 记一条 {任务, 当时转出人}；四审 P2：按配对归档/去重的对账输入）。
	transferFacts []HandoverTransferFact
}

func (f *fakeHandoverPorts) list(_ context.Context, slotInstanceID string) ([]HandoverMission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collects++
	if f.onCollect != nil {
		f.onCollect(f.collects, &f.missions)
	}
	out := []HandoverMission{}
	for _, m := range f.missions {
		if m.SlotInstanceID == slotInstanceID &&
			m.Status != "accepted" && m.Status != "canceled" {
			cp := *m
			cp.Transferable = offerableStatus(m.Status)
			out = append(out, cp)
		}
	}
	return out, nil
}

func (f *fakeHandoverPorts) reassign(_ context.Context, missionID, fromOwner, toOwner string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[missionID]; err != nil {
		return "", false, err
	}
	if f.intercept != nil {
		f.intercept(missionID, f.missions)
	}
	for _, m := range f.missions {
		if m.MissionID != missionID {
			continue
		}
		// 事务内核对四项的 fake 等价：归属锚 / 状态族 / 无在途 / 承接人 active
		//（承接人 active 由 fakeAgents 场景保证，此处略）。
		if m.OwnerAgentID != fromOwner || m.PendingOwnerAgentID != "" || !offerableStatus(m.Status) {
			return "", false, nil
		}
		m.OwnerAgentID = toOwner
		switch m.Status {
		case "ready", "running", "waiting":
			m.Status = "paused"
		}
		f.reassigns = append(f.reassigns, missionID)
		f.transferFacts = append(f.transferFacts, HandoverTransferFact{MissionID: missionID, FromOwner: fromOwner})
		return m.Status, true, nil
	}
	return "", false, fmt.Errorf("mission %s 不存在", missionID)
}

func (f *fakeHandoverPorts) statusOf(_ context.Context, missionID string) (HandoverMission, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.missions {
		if m.MissionID == missionID {
			return *m, true, nil
		}
	}
	return HandoverMission{}, false, nil
}

func (f *fakeHandoverPorts) counts(_ context.Context, _ string) (map[string]int, error) {
	return f.knowledge, nil
}

func (f *fakeHandoverPorts) privateCount(_ context.Context, _ string) (int, error) {
	return f.private, nil
}

func (f *fakeHandoverPorts) transferFactsOf(_ context.Context, si, owner string) ([]HandoverTransferFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []HandoverTransferFact{}
	ids := map[string]bool{}
	for _, m := range f.missions {
		if m.SlotInstanceID == si {
			ids[m.MissionID] = true
		}
	}
	for _, ft := range f.transferFacts {
		_ = ids // 实例过滤由场景保证（fake 任务均挂目标实例）
		out = append(out, ft)
	}
	return out, nil
}

func (f *fakeHandoverPorts) ports() HandoverPorts {
	return HandoverPorts{
		MissionsOfSlot:              f.list,
		ReassignMission:             f.reassign,
		MissionStatusOf:             f.statusOf,
		SlotKnowledgeCounts:         f.counts,
		AgentPrivateMemoryCount:     f.privateCount,
		HandoverTransferFactsOfSlot: f.transferFactsOf,
	}
}

// offerableStatus 与 mission.HandoverOfferable 同集（fake 侧镜像——UT 不跨域 import）。
func offerableStatus(s string) bool {
	switch s {
	case "draft", "ready", "running", "waiting", "paused":
		return true
	}
	return false
}

// statefulReadiness 可编程准入门（reply 依次取用，耗尽后恒过——测试可在阶段间
// 重设 reply 模拟"预核过、装机门拒"等竞态）。
type statefulReadiness struct {
	mu    sync.Mutex
	reply []bool
}

func (s *statefulReadiness) Check(_ context.Context, _ Binding) (ReadinessReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reply) > 0 {
		ok := s.reply[0]
		s.reply = s.reply[1:]
		if !ok {
			return ReadinessReport{Ready: false, Missing: []string{"skill:demo@v1"}}, nil
		}
	}
	return ReadinessReport{Ready: true}, nil
}

// newHandoverFixture 装配：真 slot Service（memstore）+ fake Ports + HandoverService。
func newHandoverFixture(t *testing.T) (*Service, *fakeAgents, *HandoverService, *fakeHandoverPorts) {
	t.Helper()
	svc, agents := newBindingService(t)
	ports := &fakeHandoverPorts{knowledge: map[string]int{}, failOn: map[string]error{}}
	h := NewHandoverService(svc, ports.ports(), WithHandoverNow(fixedNow))
	return svc, agents, h, ports
}

// activeBindingOf 取实例当前 active 绑定（无则 Fatal）。
func activeBindingOf(t *testing.T, svc *Service, instanceID string) Binding {
	t.Helper()
	bs, err := svc.ListBindings(context.Background(), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Status == BindingActive {
			return b
		}
	}
	t.Fatalf("实例 %s 无 active 绑定", instanceID)
	return Binding{}
}

// seatedInstance 建谱系 + 实例 + ag_a 在任（active）。
func seatedInstance(t *testing.T, svc *Service) string {
	t.Helper()
	si := mustInstance(t, svc)
	if _, err := svc.PrepareBinding(context.Background(), si, "ag_a", 0); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bs, _ := svc.ListBindings(context.Background(), si)
	if _, err := svc.ActivateBinding(context.Background(), bs[len(bs)-1].BindingID); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return si
}

// missionOn 建一条挂实例的端口任务。
func missionOn(ports *fakeHandoverPorts, si, id, owner, status string) *HandoverMission {
	m := &HandoverMission{
		MissionID: id, Objective: "目标 " + id, OwnerAgentID: owner,
		Status: status, SlotInstanceID: si,
	}
	ports.missions = append(ports.missions, m)
	return m
}

// TestHandoverFullChain 全链：转移/不转移分类、知识清单、排除面、绑定变迁、
// 承接回执事件落流。
func TestHandoverFullChain(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	oldB := activeBindingOf(t, svc, si)

	// 任务面：ready（→paused）/ draft（保持）/ reviewing、canceling（不转移）。
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")
	m2 := missionOn(ports, si, "m2", "ag_a", "draft")
	m3 := missionOn(ports, si, "m3", "ag_a", "reviewing")
	m4 := missionOn(ports, si, "m4", "ag_a", "canceling")
	ports.knowledge = map[string]int{"active": 3, "proposed": 1, "superseded": 2}
	ports.private = 7

	rep, err := h.Handover(context.Background(), si, "ag_b", "周期报告注意口径")
	if err != nil {
		t.Fatalf("交接: %v", err)
	}
	if rep.FromAgentID != "ag_a" || rep.ToAgentID != "ag_b" {
		t.Fatalf("归属: %+v", rep)
	}
	if len(rep.MissionsTransferred) != 2 ||
		rep.MissionsTransferred[0].MissionID != "m1" || rep.MissionsTransferred[0].FromStatus != "ready" ||
		rep.MissionsTransferred[0].Status != "paused" ||
		rep.MissionsTransferred[1].MissionID != "m2" || rep.MissionsTransferred[1].Status != "draft" {
		t.Fatalf("转移清单: %+v", rep.MissionsTransferred)
	}
	if len(rep.MissionsNotTransferred) != 2 {
		t.Fatalf("不转移清单: %+v", rep.MissionsNotTransferred)
	}
	for _, nt := range rep.MissionsNotTransferred {
		if nt.MissionID != "m3" && nt.MissionID != "m4" {
			t.Fatalf("不转移清单混入: %+v", nt)
		}
		if nt.Reason == "" {
			t.Fatalf("不转移须含原因: %+v", nt)
		}
	}
	if rep.KnowledgeInventory["active"] != 3 || rep.KnowledgeInventory["proposed"] != 1 || rep.KnowledgeInventory["superseded"] != 2 {
		t.Fatalf("知识清单: %+v", rep.KnowledgeInventory)
	}
	if rep.ExcludedPrivateCount != 7 {
		t.Fatalf("私有记忆排除计数: %d", rep.ExcludedPrivateCount)
	}
	if len(rep.Excluded) == 0 || !strings.Contains(rep.Excluded[0], "私有记忆") {
		t.Fatalf("排除面注记: %v", rep.Excluded)
	}
	if rep.ReleasedBindingID != oldB.BindingID || rep.ActivatedBindingID == "" || rep.ActivatedBindingID == oldB.BindingID {
		t.Fatalf("绑定变迁: %+v", rep)
	}
	if rep.Notes != "周期报告注意口径" || rep.OccurredAt != fixedNow() {
		t.Fatalf("回执字段: %+v", rep)
	}

	// 端口侧终态：m1 → ag_b/paused；m2 → ag_b/draft；m3/m4 归属不动。
	if m1.OwnerAgentID != "ag_b" || m1.Status != "paused" {
		t.Fatalf("m1: %+v", m1)
	}
	if m2.OwnerAgentID != "ag_b" || m2.Status != "draft" {
		t.Fatalf("m2: %+v", m2)
	}
	if m3.OwnerAgentID != "ag_a" || m4.OwnerAgentID != "ag_a" {
		t.Fatalf("不转移任务归属被动: m3=%+v m4=%+v", m3, m4)
	}

	// 绑定终态：旧任 released（handoff_note 入事件）；新任 active。
	got, _, _ := svc.GetBinding(context.Background(), oldB.BindingID)
	if got.Status != BindingReleased {
		t.Fatalf("旧任未释放: %+v", got)
	}
	evs, _, _ := svc.BindingEvents(context.Background(), oldB.BindingID)
	foundNote := false
	for _, e := range evs {
		if e.EventType == EventStatusChanged && strings.Contains(string(e.Payload), "周期报告注意口径") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatalf("释放事件须携 handoff_note: %+v", evs)
	}
	newB := activeBindingOf(t, svc, si)
	if newB.BindingID != rep.ActivatedBindingID {
		t.Fatalf("新任: %+v", newB)
	}

	// 承接回执事件落新任流（slot_handover：from/to/计数——与激活同事务）。
	newEvs, _, _ := svc.BindingEvents(context.Background(), newB.BindingID)
	var receiptEv *BindingEvent
	for i := range newEvs {
		if newEvs[i].EventType == EventSlotHandover {
			receiptEv = &newEvs[i]
		}
	}
	if receiptEv == nil {
		t.Fatalf("新任流缺 slot_handover 事件: %+v", newEvs)
	}
	var p map[string]any
	if err := json.Unmarshal(receiptEv.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["from_agent"] != "ag_a" || p["to_agent"] != "ag_b" || p["transferred_count"] != float64(2) {
		t.Fatalf("承接回执事件体: %+v", p)
	}
}

// TestHandoverIdempotentReplay 交接完成后再跑：幂等返回当前态（不新建绑定、
// 转移清单为空、from 仍可溯——重放语义）。
func TestHandoverIdempotentReplay(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	missionOn(ports, si, "m1", "ag_a", "ready")
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err != nil {
		t.Fatalf("首次: %v", err)
	}
	before, _ := svc.ListBindings(context.Background(), si)

	rep, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("重放: %v", err)
	}
	if len(rep.MissionsTransferred) != 0 {
		t.Fatalf("重放转移清单应空: %+v", rep.MissionsTransferred)
	}
	if rep.FromAgentID != "ag_a" {
		t.Fatalf("重放 from 溯源: %s", rep.FromAgentID)
	}
	after, _ := svc.ListBindings(context.Background(), si)
	if len(after) != len(before) {
		t.Fatalf("重放不得新建绑定: %d → %d", len(before), len(after))
	}
	activeBindingOf(t, svc, si) // 仍有且仅有 ag_b 在任
}

// TestHandoverFailsFast 零变更失败族：所有可预期拒绝在变更前发生。
func TestHandoverFailsFast(t *testing.T) {
	t.Run("实例未知", func(t *testing.T) {
		_, _, h, _ := newHandoverFixture(t)
		_, err := h.Handover(context.Background(), "si_ghost", "ag_b", "")
		if !errors.Is(err, ErrInstanceNotFound) {
			t.Fatalf("须 ErrInstanceNotFound: %v", err)
		}
	})
	t.Run("无在任且无可恢复态", func(t *testing.T) {
		svc, _, h, _ := newHandoverFixture(t)
		si := mustInstance(t, svc) // 有实例、无绑定
		_, err := h.Handover(context.Background(), si, "ag_b", "")
		if !errors.Is(err, ErrHandoverConflict) {
			t.Fatalf("须 ErrHandoverConflict: %v", err)
		}
	})
	t.Run("新任即在任", func(t *testing.T) {
		svc, _, h, _ := newHandoverFixture(t)
		si := seatedInstance(t, svc)
		_, err := h.Handover(context.Background(), si, "ag_a", "")
		if !errors.Is(err, ErrHandoverConflict) {
			t.Fatalf("须 ErrHandoverConflict: %v", err)
		}
	})
	t.Run("新任非 active", func(t *testing.T) {
		svc, agents, h, _ := newHandoverFixture(t)
		si := seatedInstance(t, svc)
		agents.status["ag_suspended"] = "suspended"
		_, err := h.Handover(context.Background(), si, "ag_suspended", "")
		if !errors.Is(err, ErrAgentNotActive) {
			t.Fatalf("须 ErrAgentNotActive: %v", err)
		}
		if bs, _ := svc.ListBindings(context.Background(), si); len(bs) != 1 {
			t.Fatalf("零变更：不得新建绑定: %+v", bs)
		}
	})
	t.Run("在途单任务交接整体拒", func(t *testing.T) {
		svc, _, h, ports := newHandoverFixture(t)
		si := seatedInstance(t, svc)
		m := missionOn(ports, si, "m1", "ag_a", "ready")
		m.PendingOwnerAgentID = "ag_c"
		_, err := h.Handover(context.Background(), si, "ag_b", "")
		if !errors.Is(err, ErrHandoverConflict) {
			t.Fatalf("须 ErrHandoverConflict: %v", err)
		}
		if m.OwnerAgentID != "ag_a" || m.Status != "ready" {
			t.Fatalf("零变更：任务被动: %+v", m)
		}
		if bs, _ := svc.ListBindings(context.Background(), si); len(bs) != 1 {
			t.Fatalf("零变更：不得新建绑定: %+v", bs)
		}
		if activeBindingOf(t, svc, si).AgentID != "ag_a" {
			t.Fatal("零变更：在任不得变")
		}
	})
	t.Run("readiness 预核不满足", func(t *testing.T) {
		agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_b": "active"}}
		gate := &statefulReadiness{}
		svc := New(NewMemStore(), WithNow(fixedNow), WithAgentLookup(agents), WithReadiness(gate))
		si := mustInstance(t, svc)
		if _, err := svc.PrepareBinding(context.Background(), si, "ag_a", 0); err != nil {
			t.Fatal(err)
		}
		bs, _ := svc.ListBindings(context.Background(), si)
		if _, err := svc.ActivateBinding(context.Background(), bs[0].BindingID); err != nil {
			t.Fatal(err)
		}
		gate.reply = []bool{false} // 预核即拒
		ports := &fakeHandoverPorts{knowledge: map[string]int{}, failOn: map[string]error{}}
		h := NewHandoverService(svc, ports.ports(), WithHandoverNow(fixedNow))
		m := missionOn(ports, si, "m1", "ag_a", "ready")
		_, err := h.Handover(context.Background(), si, "ag_b", "")
		if !errors.Is(err, ErrReadinessNotMet) {
			t.Fatalf("须 ErrReadinessNotMet: %v", err)
		}
		if m.OwnerAgentID != "ag_a" {
			t.Fatalf("零变更：任务被动: %+v", m)
		}
		if bs2, _ := svc.ListBindings(context.Background(), si); len(bs2) != 1 {
			t.Fatalf("零变更：不得新建绑定: %+v", bs2)
		}
	})
	t.Run("备注超长", func(t *testing.T) {
		svc, _, h, _ := newHandoverFixture(t)
		si := seatedInstance(t, svc)
		_, err := h.Handover(context.Background(), si, "ag_b", strings.Repeat("字", 4097))
		if !errors.Is(err, ErrInvalidBinding) {
			t.Fatalf("须 ErrInvalidBinding: %v", err)
		}
	})
}

// TestHandoverResumeAfterPartial 中断续跑：旧任已释、新任 ready 未激活、
// 任务未转——重入收敛完成。
func TestHandoverResumeAfterPartial(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	oldB := activeBindingOf(t, svc, si)
	m1 := missionOn(ports, si, "m1", "ag_a", "running")

	// 手工推进到中断态：旧任释放 + 新任 ready（未激活）。
	if _, err := svc.ReleaseBinding(context.Background(), oldB.BindingID, "中断前释放"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareBinding(context.Background(), si, "ag_b", 0); err != nil {
		t.Fatal(err)
	}

	rep, err := h.Handover(context.Background(), si, "ag_b", "续跑")
	if err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if m1.OwnerAgentID != "ag_b" || m1.Status != "paused" {
		t.Fatalf("m1: %+v", m1)
	}
	if len(rep.MissionsTransferred) != 1 || rep.FromAgentID != "ag_a" {
		t.Fatalf("回执: %+v", rep)
	}
	newB := activeBindingOf(t, svc, si)
	if newB.AgentID != "ag_b" {
		t.Fatalf("新任未激活: %+v", newB)
	}
}

// TestHandoverReassignRetryConvergence 部分转移失败：中止（旧任留排空边界
// draining——任务转移先于释放）→ 排障后重跑按归属锚收敛。
func TestHandoverReassignRetryConvergence(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	oldB := activeBindingOf(t, svc, si)
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")
	m2 := missionOn(ports, si, "m2", "ag_a", "ready")
	ports.failOn["m2"] = fmt.Errorf("boom")

	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err == nil {
		t.Fatal("首跑须失败")
	}
	// 中止安全态：m1 已转、m2 未动、旧任处排空边界（非 active——停接单）。
	if m1.OwnerAgentID != "ag_b" || m2.OwnerAgentID != "ag_a" {
		t.Fatalf("中止态: m1=%+v m2=%+v", m1, m2)
	}
	got, _, _ := svc.GetBinding(context.Background(), oldB.BindingID)
	if got.Status != BindingDraining || got.AgentID != "ag_a" {
		t.Fatalf("中止态旧任应 draining: %+v", got)
	}

	ports.failOn = map[string]error{}
	rep, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if m2.OwnerAgentID != "ag_b" || m2.Status != "paused" {
		t.Fatalf("m2: %+v", m2)
	}
	// 审校 #3：重跑回执列**全批**（m1 既成事实 + m2 本轮补转——不漏已转部分）。
	if len(rep.MissionsTransferred) != 2 {
		t.Fatalf("重跑回执须列全批: %+v", rep.MissionsTransferred)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" {
		t.Fatalf("重跑后新任: %+v", b)
	}
}

// TestHandoverBlockedThenRetry 预核过、装机门拒（授权竞态）：blocked 留痕、
// 零任务变更；修授权后重跑走 RetryPrepare 收敛。
func TestHandoverBlockedThenRetry(t *testing.T) {
	agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_b": "active"}}
	gate := &statefulReadiness{} // 装任阶段恒过
	svc := New(NewMemStore(), WithNow(fixedNow), WithAgentLookup(agents), WithReadiness(gate))
	si := mustInstance(t, svc)
	if _, err := svc.PrepareBinding(context.Background(), si, "ag_a", 0); err != nil {
		t.Fatal(err)
	}
	bs, _ := svc.ListBindings(context.Background(), si)
	if _, err := svc.ActivateBinding(context.Background(), bs[0].BindingID); err != nil {
		t.Fatal(err)
	}
	ports := &fakeHandoverPorts{knowledge: map[string]int{}, failOn: map[string]error{}}
	h := NewHandoverService(svc, ports.ports(), WithHandoverNow(fixedNow))
	m := missionOn(ports, si, "m1", "ag_a", "ready")

	gate.reply = []bool{true, false} // 预核过、装机门拒（授权竞态）
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); !errors.Is(err, ErrHandoverConflict) {
		t.Fatalf("装机门拒须 ErrHandoverConflict: %v", err)
	}
	if m.OwnerAgentID != "ag_a" {
		t.Fatalf("零变更：任务被动: %+v", m)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_a" {
		t.Fatal("零变更：在任不得变")
	}
	// ag_b 留 blocked 绑定（修授权后重跑的恢复锚）。
	var blocked *Binding
	for _, b := range mustBindings(t, svc, si) {
		if b.AgentID == "ag_b" && b.Status == BindingBlocked {
			blocked = &b
		}
	}
	if blocked == nil {
		t.Fatal("ag_b 须留 blocked 绑定")
	}

	gate.reply = nil // 授权已修
	rep, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if m.OwnerAgentID != "ag_b" || m.Status != "paused" {
		t.Fatalf("m: %+v", m)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" || b.BindingID != rep.ActivatedBindingID {
		t.Fatalf("新任: %+v", b)
	}
}

// TestHandoverAnchorMissClassification 锚错配分类：收集与转移之间并发归属变化
// ——0.6.12 承接抢先（owner 已是新任）→ 计已转；改派第三方 → 不转移（归属已变化）。
func TestHandoverAnchorMissClassification(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	missionOn(ports, si, "m_moved", "ag_a", "ready")
	missionOn(ports, si, "m_third", "ag_a", "paused")
	missionOn(ports, si, "m_ours", "ag_a", "ready")
	ports.intercept = func(missionID string, missions []*HandoverMission) {
		switch missionID {
		case "m_moved": // 0.6.12 承接抢先（→ ag_b）
			for _, m := range missions {
				if m.MissionID == "m_moved" {
					m.OwnerAgentID = "ag_b"
				}
			}
		case "m_third": // 改派第三方（→ ag_z）
			for _, m := range missions {
				if m.MissionID == "m_third" {
					m.OwnerAgentID = "ag_z"
				}
			}
		}
	}

	rep, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("交接: %v", err)
	}
	var moved, third, ours bool
	for _, tr := range rep.MissionsTransferred {
		if tr.MissionID == "m_moved" {
			moved = true
		}
		if tr.MissionID == "m_ours" {
			ours = true
		}
	}
	for _, nt := range rep.MissionsNotTransferred {
		if nt.MissionID == "m_third" {
			third = true
		}
	}
	if !moved || !ours {
		t.Fatalf("已转/本批清单: %+v", rep.MissionsTransferred)
	}
	if !third {
		t.Fatalf("第三方归属须列不转移: %+v", rep.MissionsNotTransferred)
	}
}

// TestHandoverNoMissions 无在办：交接仍完成绑定换任（空清单回执）。
func TestHandoverNoMissions(t *testing.T) {
	svc, _, h, _ := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	rep, err := h.Handover(context.Background(), si, "ag_b", "无在办")
	if err != nil {
		t.Fatalf("交接: %v", err)
	}
	if len(rep.MissionsTransferred) != 0 || len(rep.MissionsNotTransferred) != 0 {
		t.Fatalf("空清单: %+v", rep)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" {
		t.Fatalf("新任: %+v", b)
	}
	if rep.Notes != "无在办" {
		t.Fatalf("回执备注: %q", rep.Notes)
	}
}

// ---- v0.2 一审反例钉住（4 P1 + 2 P2） ----

// TestHandoverConcurrentSerialized 审校 #2（P1）：并发换将不得交错分批——
// 串行化后两轮各自完整收敛，无任务遗落第三方。
func TestHandoverConcurrentSerialized(t *testing.T) {
	svc, agents, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	agents.status["ag_c"] = "active"
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")
	m2 := missionOn(ports, si, "m2", "ag_a", "running")

	var wg sync.WaitGroup
	for _, target := range []string{"ag_b", "ag_c"} {
		wg.Add(1)
		go func(tg string) {
			defer wg.Done()
			// 收敛族错误可容忍（某轮可能与已收敛态冲突）；不变量由末态断言。
			if _, err := h.Handover(context.Background(), si, tg, ""); err != nil {
				t.Logf("并发轮（→%s）: %v", tg, err)
			}
		}(target)
	}
	wg.Wait()

	// 不变量：无批次分裂——每条可转移任务都归**最终在任者**（链式收敛），
	// 不存在归给"从未完成交接的第三方"而悬空的任务。
	final := activeBindingOf(t, svc, si)
	for _, m := range []*HandoverMission{m1, m2} {
		if m.OwnerAgentID != final.AgentID {
			t.Fatalf("批次分裂：任务 %s 归 %s，最终在任 %s（应链式收敛到在任者）", m.MissionID, m.OwnerAgentID, final.AgentID)
		}
		if m.Status != "paused" {
			t.Fatalf("转移后应 paused: %+v", m)
		}
	}
}

// TestHandoverDanglingRejectsNewTarget 审校 #3（P1）：前轮部分转移后改选接任
// 者 → 409 拒（遗留任务悬空风险）；循原目标重跑收敛。
func TestHandoverDanglingRejectsNewTarget(t *testing.T) {
	svc, agents, h, ports := newHandoverFixture(t)
	agents.status["ag_c"] = "active"
	si := seatedInstance(t, svc)
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")
	m2 := missionOn(ports, si, "m2", "ag_a", "ready")
	ports.failOn["m2"] = fmt.Errorf("boom")
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err == nil {
		t.Fatal("首跑须失败（前置）")
	} // 中止态：m1→ag_b 已转、m2 归 ag_a、旧任 draining

	// 改选 ag_c：409——遗留 m1（ag_b）会被悬空（ag_c 激活后 m1 仍挂无任职的 ag_b）。
	_, err := h.Handover(context.Background(), si, "ag_c", "")
	if !errors.Is(err, ErrHandoverConflict) || !strings.Contains(err.Error(), "遗留转移") {
		t.Fatalf("改目标须 409 遗留转移: %v", err)
	}
	if m1.OwnerAgentID != "ag_b" || m2.OwnerAgentID != "ag_a" {
		t.Fatalf("拒绝时不得动物: m1=%+v m2=%+v", m1, m2)
	}

	// 循原目标 ag_b 重跑：收敛（m2 补转；m1 既成事实入回执）。
	ports.failOn = map[string]error{}
	rep, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("原目标重跑: %v", err)
	}
	if m1.OwnerAgentID != "ag_b" || m2.OwnerAgentID != "ag_b" {
		t.Fatalf("收敛后归属: m1=%+v m2=%+v", m1, m2)
	}
	var hasM1, hasM2 bool
	for _, tr := range rep.MissionsTransferred {
		if tr.MissionID == "m1" {
			hasM1 = true
		}
		if tr.MissionID == "m2" {
			hasM2 = true
		}
	}
	if !hasM1 || !hasM2 {
		t.Fatalf("回执须列既成事实与本批: %+v", rep.MissionsTransferred)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" {
		t.Fatalf("收敛后在任: %+v", b)
	}
}

// TestHandoverBoundaryBeforeCollection 审校 #4（P1）：排空边界（active→
// draining）先于权威收集——预检后新进任务仍入批；且转移时旧任已非 active
// （停接单边界成立）。
func TestHandoverBoundaryBeforeCollection(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	oldB := activeBindingOf(t, svc, si)
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")

	// 第二次收集（边界后的权威读）时"新进"一条合法在办任务（预检后、边界
	// 提交前进来的——旧实现收集一次即漏）。
	ports.onCollect = func(n int, ms *[]*HandoverMission) {
		if n == 2 {
			*ms = append(*ms, &HandoverMission{
				MissionID: "m_late", Objective: "边界前进来的", OwnerAgentID: "ag_a",
				Status: "ready", SlotInstanceID: si, Transferable: true,
			})
		}
	}
	// 转移入口核旧任状态：必须已 draining（停接单边界先于转移）。
	boundarySeen := false
	ports.intercept = func(string, []*HandoverMission) {
		got, _, _ := svc.GetBinding(context.Background(), oldB.BindingID)
		if got.Status != BindingDraining {
			t.Errorf("转移时旧任须 draining（停接单边界）: %s", got.Status)
		}
		boundarySeen = true
	}

	rep, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("交接: %v", err)
	}
	if !boundarySeen {
		t.Fatal("未走到转移面")
	}
	var late bool
	for _, tr := range rep.MissionsTransferred {
		if tr.MissionID == "m_late" {
			late = true
		}
	}
	if !late || m1.OwnerAgentID != "ag_b" {
		t.Fatalf("边界前进来的任务须入批转移: %+v", rep.MissionsTransferred)
	}
	// 完成后旧任已 released（边界 → 转移 → 释放的时序终态）。
	got, _, _ := svc.GetBinding(context.Background(), oldB.BindingID)
	if got.Status != BindingReleased {
		t.Fatalf("旧任终态: %s", got.Status)
	}
}

// TestHandoverReplayDriftRepair 重放漂移修复：完成后经 0.6.12 承接回流入上任
// 名下的任务，重放补转并独立记该轮回执事件。
func TestHandoverReplayDriftRepair(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	missionOn(ports, si, "m1", "ag_a", "ready")
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err != nil {
		t.Fatal(err)
	}
	// 漂移：m1 被改回 ag_a 名下（模拟 0.6.12 承接回流）。
	for _, m := range ports.missions {
		if m.MissionID == "m1" {
			m.OwnerAgentID = "ag_a"
			m.Status = "paused"
		}
	}
	rep, err := h.Handover(context.Background(), si, "ag_b", "漂移修复")
	if err != nil {
		t.Fatalf("重放补转: %v", err)
	}
	if len(rep.MissionsTransferred) != 1 || rep.MissionsTransferred[0].MissionID != "m1" {
		t.Fatalf("漂移补转: %+v", rep.MissionsTransferred)
	}
	// 补转轮独立记 slot_handover 事件（首轮 + 补转轮 = 2）。
	b := activeBindingOf(t, svc, si)
	evs, _, _ := svc.BindingEvents(context.Background(), b.BindingID)
	count := 0
	for _, e := range evs {
		if e.EventType == EventSlotHandover {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("两轮（首轮 + 补转轮）回执事件: %d", count)
	}
}

// TestHandoverFormerActiveIncumbent 审校 #5（P2）：旧任识别按曾任 active 证据
// ——ready→released 直释候选（从未在任、释放时间更晚）不得误作旧任。
func TestHandoverFormerActiveIncumbent(t *testing.T) {
	agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_b": "active"}}
	// 可变时钟：令各绑定 updated_at 可区分（fixedNow 恒值无法排序）。
	now := "2026-11-12T10:00:00.000Z"
	clock := func() string { return now }
	svc := New(NewMemStore(), WithNow(clock), WithAgentLookup(agents))
	var seq int64
	svc.newInstanceID = func() string { seq++; return fmt.Sprintf("si_fa_%d", seq) }
	svc.newNamespace = func() string { seq++; return fmt.Sprintf("slotns_fa_%d", seq) }
	svc.newBindingID = func() string { seq++; return fmt.Sprintf("bd_fa_%d", seq) }
	ports := &fakeHandoverPorts{knowledge: map[string]int{}, failOn: map[string]error{}}
	h := NewHandoverService(svc, ports.ports(), WithHandoverNow(clock))
	ctx := context.Background()
	if _, err := svc.Create(ctx, "slot.fa", validContent()); err != nil {
		t.Fatal(err)
	}
	inst, err := svc.CreateInstance(ctx, "slot.fa", "")
	if err != nil {
		t.Fatal(err)
	}
	si := inst.SlotInstanceID

	// bd1=ag_a 装任（曾任 active）。
	now = "T1"
	if _, err := svc.PrepareBinding(ctx, si, "ag_a", 0); err != nil {
		t.Fatal(err)
	}
	now = "T2"
	bs, _ := svc.ListBindings(ctx, si)
	if _, err := svc.ActivateBinding(ctx, bs[0].BindingID); err != nil {
		t.Fatal(err)
	}
	// bd2=ag_b ready 候选 → 直释（从未在任），释放时间晚于 bd1 后续释放（T4）。
	now = "T3"
	if _, err := svc.PrepareBinding(ctx, si, "ag_b", 0); err != nil {
		t.Fatal(err)
	}
	now = "T5"
	bs, _ = svc.ListBindings(ctx, si)
	for _, b := range bs {
		if b.AgentID == "ag_b" && b.Status == BindingReady {
			if _, err := svc.ReleaseBinding(ctx, b.BindingID, "候选放弃"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 交接 ag_a→ag_b：两时点复核拒（ag_b 中途 suspended）→ 旧任已释（T4）。
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")
	agents.status["ag_b"] = "suspended"
	_, err = h.Handover(ctx, si, "ag_b", "")
	if !errors.Is(err, ErrAgentNotActive) {
		t.Fatalf("激活复核失败: %v", err)
	}
	// 修复后重跑：from 必须仍是 ag_a（曾任 active 证据）——旧实现按"最新
	// released"会误选 bd2（ag_b），误报"新任即在任"而无法恢复。
	agents.status["ag_b"] = "active"
	rep, err := h.Handover(ctx, si, "ag_b", "")
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if rep.FromAgentID != "ag_a" {
		t.Fatalf("旧任识别错误（曾任 active 证据）: from=%s", rep.FromAgentID)
	}
	if m1.OwnerAgentID != "ag_b" || m1.Status != "paused" {
		t.Fatalf("m1: %+v", m1)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" {
		t.Fatalf("新任: %+v", b)
	}
}

// failingTransitionStore 包装 MemStore——ActivateBindingTx 前 failN 次失败
// （审校 #6 反例注入：激活-回执原子性验证）。
type failingTransitionStore struct {
	*MemStore
	mu    sync.Mutex
	failN int
	calls int
}

func (f *failingTransitionStore) ActivateBindingTx(ctx context.Context, bindingID, expectFrom, evType string, evPayload json.RawMessage, occurredAt string) (string, bool, error) {
	f.mu.Lock()
	fail := f.failN > 0
	if fail {
		f.failN-- // "接下来 N 次"递减语义——装任后武装不受既有调用计数影响
	}
	f.mu.Unlock()
	if fail {
		return "", false, fmt.Errorf("injected activate-tx failure")
	}
	return f.MemStore.ActivateBindingTx(ctx, bindingID, expectFrom, evType, evPayload, occurredAt)
}

// TestHandoverActivationReceiptAtomic 审校 #6（P2）：激活与承接回执**同一事务**
// ——注入 TransitionBindingWithEvent 失败：激活未遂且无事件（原子未遂）；重跑
// 成功后激活与回执恰一次同现。
func TestHandoverActivationReceiptAtomic(t *testing.T) {
	agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_b": "active"}}
	store := &failingTransitionStore{MemStore: NewMemStore()}
	svc := New(store, WithNow(fixedNow), WithAgentLookup(agents))
	var seq int64
	svc.newInstanceID = func() string { seq++; return fmt.Sprintf("si_at_%d", seq) }
	svc.newNamespace = func() string { seq++; return fmt.Sprintf("slotns_at_%d", seq) }
	svc.newBindingID = func() string { seq++; return fmt.Sprintf("bd_at_%d", seq) }
	ports := &fakeHandoverPorts{knowledge: map[string]int{}, failOn: map[string]error{}}
	h := NewHandoverService(svc, ports.ports(), WithHandoverNow(fixedNow))
	ctx := context.Background()
	if _, err := svc.Create(ctx, "slot.at", validContent()); err != nil {
		t.Fatal(err)
	}
	inst, err := svc.CreateInstance(ctx, "slot.at", "")
	if err != nil {
		t.Fatal(err)
	}
	si := inst.SlotInstanceID
	missionOn(ports, si, "m1", "ag_a", "ready")
	if _, err := svc.PrepareBinding(ctx, si, "ag_a", 0); err != nil {
		t.Fatal(err)
	}
	bs, _ := svc.ListBindings(ctx, si)
	if _, err := svc.ActivateBinding(ctx, bs[0].BindingID); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.failN = 1 // 装任完成后再武装注入（装任激活也走同一事务面）
	store.mu.Unlock()

	// 首跑：激活+回执事务失败——激活未遂、零 slot_handover 事件（原子未遂）。
	if _, err := h.Handover(ctx, si, "ag_b", ""); err == nil {
		t.Fatal("首跑须失败（注入）")
	}
	all, _ := svc.ListBindings(ctx, si)
	hasActive, hasReceipt := false, false
	for _, b := range all {
		if b.Status == BindingActive {
			hasActive = true
		}
		evs, _, _ := svc.BindingEvents(ctx, b.BindingID)
		for _, e := range evs {
			if e.EventType == EventSlotHandover {
				hasReceipt = true
			}
		}
	}
	if hasActive || hasReceipt {
		t.Fatalf("原子性破坏：激活与回执须同未遂（active=%v receipt=%v）", hasActive, hasReceipt)
	}

	// 重跑（注入耗尽）：激活 + 回执恰一次同现。
	rep, err := h.Handover(ctx, si, "ag_b", "")
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" || b.BindingID != rep.ActivatedBindingID {
		t.Fatalf("新任: %+v", b)
	}
	evs, _, _ := svc.BindingEvents(ctx, rep.ActivatedBindingID)
	count := 0
	for _, e := range evs {
		if e.EventType == EventSlotHandover {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("回执恰一次: %d", count)
	}
}

// mustBindings 断言取绑定列表。
func mustBindings(t *testing.T, svc *Service, instanceID string) []Binding {
	t.Helper()
	bs, err := svc.ListBindings(context.Background(), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

// ---- v0.3 复审反例钉住（1 P1 + 1 P2） ----

// TestHandoverActivationGuardBlocksBypass 复审 P1：交接未完成（旧任 draining、
// 新任 ready、m2 未转）时普通激活被服务端拒——不得绕过未完成批次；续跑交接
// 收敛（m2 补转、旧任释放、回执完整）。
func TestHandoverActivationGuardBlocksBypass(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	oldB := activeBindingOf(t, svc, si)
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")
	m2 := missionOn(ports, si, "m2", "ag_a", "ready")
	ports.failOn["m2"] = fmt.Errorf("boom")
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err == nil {
		t.Fatal("首跑须失败（前置）")
	}
	_ = m1
	// 中断态：m1→ag_b、旧任 draining、新任 ready。
	var newReady Binding
	for _, b := range mustBindings(t, svc, si) {
		if b.AgentID == "ag_b" && b.Status == BindingReady {
			newReady = b
		}
	}
	if newReady.BindingID == "" {
		t.Fatal("ag_b 须留 ready 绑定（前置）")
	}
	// 绕行：普通激活 → 服务端拒（draining 守卫）。
	if _, err := svc.ActivateBinding(context.Background(), newReady.BindingID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("普通激活须被拒（未完成交接批次）: %v", err)
	}
	got, _, _ := svc.GetBinding(context.Background(), newReady.BindingID)
	if got.Status != BindingReady {
		t.Fatalf("拒后不得激活: %+v", got)
	}
	// 收敛入口：重跑交接（draining/ready 续跑）。
	ports.failOn = map[string]error{}
	rep, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if m2.OwnerAgentID != "ag_b" || m2.Status != "paused" {
		t.Fatalf("m2 补转: %+v", m2)
	}
	if old, _, _ := svc.GetBinding(context.Background(), oldB.BindingID); old.Status != BindingReleased {
		t.Fatalf("旧任须释放: %+v", old)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" || b.BindingID != rep.ActivatedBindingID {
		t.Fatalf("新任: %+v", b)
	}
}

// TestHandoverReplayFallbackDraining 复审 P1 兜底：守卫上线前的绕行数据态
// （新任已在任 + 旧任仍 draining、m2 遗留旧任）——重放分支回退 draining 作
// from：补转 m2、释放遗留旧任、自愈。
func TestHandoverReplayFallbackDraining(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	oldB := activeBindingOf(t, svc, si)
	m2 := missionOn(ports, si, "m2", "ag_a", "ready")

	// 手工构造绕行态：旧任仅入边界（draining），新任存储直转激活（模拟守卫
	// 上线前已发生的历史数据形态）。
	if _, err := svc.BeginHandoverDraining(context.Background(), oldB.BindingID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareBinding(context.Background(), si, "ag_b", 0); err != nil {
		t.Fatal(err)
	}
	for _, b := range mustBindings(t, svc, si) {
		if b.AgentID == "ag_b" && b.Status == BindingReady {
			if _, done, err := svc.store.TransitionBinding(context.Background(), b.BindingID,
				[]string{BindingReady}, BindingActive, nil, "test-bypass"); err != nil || !done {
				t.Fatalf("构造绕行态: done=%v %v", done, err)
			}
		}
	}

	// 自愈：交接（新任已在任）→ from 回退 draining 旧任；补转 m2 并释放旧任。
	rep, err := h.Handover(context.Background(), si, "ag_b", "自愈")
	if err != nil {
		t.Fatalf("自愈: %v", err)
	}
	if rep.FromAgentID != "ag_a" {
		t.Fatalf("from 回退 draining: %s", rep.FromAgentID)
	}
	if m2.OwnerAgentID != "ag_b" || m2.Status != "paused" {
		t.Fatalf("m2 补转: %+v", m2)
	}
	if old, _, _ := svc.GetBinding(context.Background(), oldB.BindingID); old.Status != BindingReleased {
		t.Fatalf("遗留旧任须释放: %+v", old)
	}
	if b := activeBindingOf(t, svc, si); b.AgentID != "ag_b" {
		t.Fatalf("新任: %+v", b)
	}
}

// TestHandoverReceiptCompensation 复审 P2：补转轮回执 append 失败不永久丢——
// 重试按 mission_events 换将转移事实补记（补偿）；已覆盖者不重复记（幂等）。
func TestHandoverReceiptCompensation(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	m1 := missionOn(ports, si, "m1", "ag_a", "ready")
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err != nil {
		t.Fatal(err)
	} // 首轮：激活+回执原子（missions=[m1]）
	// 漂移：m1 回流 ag_a → 补转轮。
	for _, m := range ports.missions {
		if m.MissionID == "m1" {
			m.OwnerAgentID = "ag_a"
			m.Status = "paused"
		}
	}
	// 注入：补转轮的独立 append 失败。
	inner := svc.store
	svc.store = &failingAppendStore{Store: inner, failN: 1}
	if _, err := h.Handover(context.Background(), si, "ag_b", "补转"); err == nil {
		t.Fatal("补转轮回执 append 须失败（注入）")
	}
	if m1.OwnerAgentID != "ag_b" {
		t.Fatalf("补转已遂（事实在 mission 侧）: %+v", m1)
	}
	// 重试：任务已属新任（零转移）——不得静默；按 wasTransferred 事实补记回执。
	rep2, err := h.Handover(context.Background(), si, "ag_b", "")
	if err != nil {
		t.Fatalf("补偿重试: %v", err)
	}
	if len(rep2.MissionsTransferred) != 0 {
		t.Fatalf("重试零转移（纯补偿）: %+v", rep2.MissionsTransferred)
	}
	b := activeBindingOf(t, svc, si)
	evs, _, _ := svc.BindingEvents(context.Background(), b.BindingID)
	count, compensated := 0, false
	for _, e := range evs {
		if e.EventType != EventSlotHandover {
			continue
		}
		count++
		if strings.Contains(string(e.Payload), "补记") {
			compensated = true
		}
	}
	if count != 2 || !compensated {
		t.Fatalf("补偿回执（首轮 + 补记）: count=%d compensated=%v", count, compensated)
	}
	// 再试：已覆盖——不再补记（幂等稳定）。
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err != nil {
		t.Fatal(err)
	}
	evs2, _, _ := svc.BindingEvents(context.Background(), b.BindingID)
	count2 := 0
	for _, e := range evs2 {
		if e.EventType == EventSlotHandover {
			count2++
		}
	}
	if count2 != 2 {
		t.Fatalf("补偿幂等: %d", count2)
	}
}

// failingAppendStore 包装 store——AppendBindingEvent 前 failN 次失败。
type failingAppendStore struct {
	Store
	failN int
}

func (f *failingAppendStore) AppendBindingEvent(ctx context.Context, bindingID, eventType string, payload json.RawMessage, occurredAt string) error {
	if f.failN > 0 {
		f.failN--
		return fmt.Errorf("injected append failure")
	}
	return f.Store.AppendBindingEvent(ctx, bindingID, eventType, payload, occurredAt)
}

// ---- v0.4 三审反例钉住（1 P1 + 1 P2） ----

// interleaveReadiness 准入检查期注入器（三审 P1）：Check 回调在 ActivateBinding
// 的服务层阶段（预查后、写事务前）把旧任置入 draining——模拟并发排空边界
// 插入检查与写入之间。
type interleaveReadiness struct {
	svc     *Service
	drainID string
	fired   bool
}

func (r *interleaveReadiness) Check(ctx context.Context, _ Binding) (ReadinessReport, error) {
	if !r.fired {
		r.fired = true
		if _, err := r.svc.BeginHandoverDraining(ctx, r.drainID, "并发插入"); err != nil {
			return ReadinessReport{}, err
		}
	}
	return ReadinessReport{Ready: true}, nil
}

// TestHandoverActivationGuardAtomicInterleave 三审 P1：draining 检查与激活写入
// 不原子——预查通过后并发置 draining 仍可激活。修复后守卫在**写事务内**：
// 准入检查期注入 draining，激活仍被拒（无绕行窗口）。
func TestHandoverActivationGuardAtomicInterleave(t *testing.T) {
	agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_b": "active"}}
	svc := New(NewMemStore(), WithNow(fixedNow), WithAgentLookup(agents))
	var seq int64
	svc.newInstanceID = func() string { seq++; return fmt.Sprintf("si_iv_%d", seq) }
	svc.newNamespace = func() string { seq++; return fmt.Sprintf("slotns_iv_%d", seq) }
	svc.newBindingID = func() string { seq++; return fmt.Sprintf("bd_iv_%d", seq) }
	ctx := context.Background()
	if _, err := svc.Create(ctx, "slot.iv", validContent()); err != nil {
		t.Fatal(err)
	}
	inst, err := svc.CreateInstance(ctx, "slot.iv", "")
	if err != nil {
		t.Fatal(err)
	}
	si := inst.SlotInstanceID
	// 旧任 ag_a 在任 + 新任 ag_b ready。
	if _, err := svc.PrepareBinding(ctx, si, "ag_a", 0); err != nil {
		t.Fatal(err)
	}
	bs, _ := svc.ListBindings(ctx, si)
	if _, err := svc.ActivateBinding(ctx, bs[0].BindingID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareBinding(ctx, si, "ag_b", 0); err != nil {
		t.Fatal(err)
	}
	var newReady Binding
	for _, b := range mustBindings(t, svc, si) {
		if b.AgentID == "ag_b" && b.Status == BindingReady {
			newReady = b
		}
	}
	// 注入器：激活 ag_b 的准入检查期把旧任（ag_a）置 draining。
	svc.readiness = &interleaveReadiness{svc: svc, drainID: bs[0].BindingID}
	if _, err := svc.ActivateBinding(ctx, newReady.BindingID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("并发绕行须被写事务守卫拦（预查后置 draining 仍拒）: %v", err)
	}
	got, _, _ := svc.GetBinding(ctx, newReady.BindingID)
	if got.Status != BindingReady {
		t.Fatalf("不得激活: %+v", got)
	}
	// 旧任确已 draining（注入器生效自证）。
	old, _, _ := svc.GetBinding(ctx, bs[0].BindingID)
	if old.Status != BindingDraining {
		t.Fatalf("注入器须已置 draining（反例前提）: %+v", old)
	}
}

// TestHandoverReceiptCompensationTerminalMission 三审 P2：补转成功、回执写入
// 失败后任务**取消**（终态）——在办收集不再见它；补偿按持久事件源仍补记。
func TestHandoverReceiptCompensationTerminalMission(t *testing.T) {
	svc, _, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	missionOn(ports, si, "m1", "ag_a", "ready")
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err != nil {
		t.Fatal(err)
	} // 首轮：激活+回执原子（missions=[m1]）
	// 漂移：m1 回流 ag_a → 补转轮（回执 append 注入失败）。
	for _, m := range ports.missions {
		if m.MissionID == "m1" {
			m.OwnerAgentID = "ag_a"
			m.Status = "paused"
		}
	}
	inner := svc.store
	svc.store = &failingAppendStore{Store: inner, failN: 1}
	if _, err := h.Handover(context.Background(), si, "ag_b", "补转"); err == nil {
		t.Fatal("补转轮回执 append 须失败（注入）")
	}
	// 三审反例：补转已遂后任务被**取消**（终态）——在办收集（MissionsOfSlot）
	// 不再返回它；再改派归属亦然。补偿不得依赖在办/归属。
	for _, m := range ports.missions {
		if m.MissionID == "m1" {
			m.Status = "canceled"
			m.OwnerAgentID = "ag_z"
		}
	}
	if _, err := h.Handover(context.Background(), si, "ag_b", ""); err != nil {
		t.Fatalf("终态后补偿重试: %v", err)
	}
	b := activeBindingOf(t, svc, si)
	evs, _, _ := svc.BindingEvents(context.Background(), b.BindingID)
	count, compensated := 0, false
	for _, e := range evs {
		if e.EventType != EventSlotHandover {
			continue
		}
		count++
		if strings.Contains(string(e.Payload), "补记") && strings.Contains(string(e.Payload), "m1") {
			compensated = true
		}
	}
	if count != 2 || !compensated {
		t.Fatalf("终态任务的转移回执仍须补记（持久事件源）: count=%d compensated=%v", count, compensated)
	}
}

// TestHandoverReceiptCompensationCrossTenure 四审 P2：跨任期补写归档——
// 同一任务两次 A→B、第二次回执失败、任务取消后实例经 B→C→B：补偿记在 B 的
// 新绑定但 **from=当时的转出人 A**（非当前轮 C）；恰好补齐一条；幂等。
func TestHandoverReceiptCompensationCrossTenure(t *testing.T) {
	svc, agents, h, ports := newHandoverFixture(t)
	si := seatedInstance(t, svc)
	agents.status["ag_c"] = "active"
	missionOn(ports, si, "m1", "ag_a", "ready")
	ctx := context.Background()
	if _, err := h.Handover(ctx, si, "ag_b", ""); err != nil {
		t.Fatal(err)
	} // 第 1 次 A→B：激活+回执原子（from=A, missions=[m1]）
	// 漂移 → 第 2 次 A→B（补转轮），回执 append 注入失败。
	for _, m := range ports.missions {
		if m.MissionID == "m1" {
			m.OwnerAgentID = "ag_a"
			m.Status = "paused"
		}
	}
	inner := svc.store
	svc.store = &failingAppendStore{Store: inner, failN: 1}
	if _, err := h.Handover(ctx, si, "ag_b", "补转"); err == nil {
		t.Fatal("补转轮回执 append 须失败（注入）")
	}
	svc.store = inner
	// 任务取消 + 改派第三方（终态——补偿不得依赖在办/归属）。
	for _, m := range ports.missions {
		if m.MissionID == "m1" {
			m.Status = "canceled"
			m.OwnerAgentID = "ag_z"
		}
	}
	// 跨任期：B→C、C→B（B 的新绑定）。
	if _, err := h.Handover(ctx, si, "ag_c", ""); err != nil {
		t.Fatalf("B→C: %v", err)
	}
	rep, err := h.Handover(ctx, si, "ag_b", "回归")
	if err != nil {
		t.Fatalf("C→B: %v", err)
	}
	bNew := activeBindingOf(t, svc, si)
	if bNew.BindingID != rep.ActivatedBindingID {
		t.Fatalf("B 新绑定: %+v", bNew)
	}
	// 断言：补记回执记 from=ag_a（当时转出人）——非当前轮 ag_c；恰一条。
	evs, _, _ := svc.BindingEvents(ctx, bNew.BindingID)
	compensated, wrongFrom := 0, false
	for _, e := range evs {
		if e.EventType != EventSlotHandover {
			continue
		}
		var pl struct {
			FromAgent string   `json:"from_agent"`
			Missions  []string `json:"missions"`
			Notes     string   `json:"notes"`
		}
		if json.Unmarshal(e.Payload, &pl) != nil {
			continue
		}
		if strings.Contains(pl.Notes, "补记") {
			compensated++
			if pl.FromAgent != "ag_a" {
				wrongFrom = true
				t.Logf("补记 from=%s（应为当时转出人 ag_a）", pl.FromAgent)
			}
			if len(pl.Missions) != 1 || pl.Missions[0] != "m1" {
				t.Fatalf("补记任务面: %+v", pl.Missions)
			}
		}
	}
	if compensated != 1 || wrongFrom {
		t.Fatalf("跨任期补写归档：compensated=%d wrongFrom=%v", compensated, wrongFrom)
	}
	// 幂等：再跑零补记。
	if _, err := h.Handover(ctx, si, "ag_b", ""); err != nil {
		t.Fatal(err)
	}
	evs2, _, _ := svc.BindingEvents(ctx, bNew.BindingID)
	total := 0
	for _, e := range evs2 {
		if e.EventType == EventSlotHandover && strings.Contains(string(e.Payload), "补记") {
			total++
		}
	}
	if total != 1 {
		t.Fatalf("补记幂等: %d", total)
	}
}
