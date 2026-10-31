// UT 层：执行上下文装配（0.6.2 / ADR-0025）。确定性（同状态必同 digest）、
// 层账本（必需不截断/可选配额/未建模如实 omitted）、授权过滤先于排序与计数
// （非 public 事件不进入不计数）、blocked 优先级、digest 即身份重建重放。
package execctx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

// ---- 测试件 ----

type fakeMissions struct {
	m  mission.Mission
	ok bool
}

func (f fakeMissions) Get(_ context.Context, id string) (mission.Mission, bool, error) {
	if id != f.m.MissionID {
		return mission.Mission{}, false, nil
	}
	return f.m, f.ok, nil
}

type fakeBindings struct {
	facts map[string]BindingFacts
}

func (f fakeBindings) Facts(_ context.Context, bindingID string) (BindingFacts, bool, error) {
	facts, ok := f.facts[bindingID]
	return facts, ok, nil
}

type fakeReadiness struct {
	report slot.ReadinessReport
	err    error
}

func (f fakeReadiness) Check(_ context.Context, _ slot.Binding) (slot.ReadinessReport, error) {
	return f.report, f.err
}

type fakeGrants struct {
	grant slot.AgentGrant
}

func (f fakeGrants) GrantByAgent(_ context.Context, _ string) (slot.AgentGrant, bool, error) {
	return f.grant, true, nil
}

type fakeRooms struct {
	events map[string][]RoomEvent
}

func (f fakeRooms) RoomEvents(_ context.Context, roomID string) ([]RoomEvent, error) {
	return f.events[roomID], nil
}

func readyReport() slot.ReadinessReport {
	return slot.ReadinessReport{
		Ready:                    true,
		Satisfied:                []string{"runtime.required:net", "tools.required:web"},
		EffectiveWorkspaceAccess: "read",
	}
}

func testSpec() slot.SlotSpec {
	return slot.SlotSpec{
		SlotSpecID: "ss_t", Version: 2, SchemaVersion: 1,
		SpecContent: slot.SpecContent{
			Name:           "调研员",
			Responsibility: "负责调研并产出报告",
			Deliverables:   []string{"调研报告"},
			Skills:         []slot.SkillRef{{Ref: "skill/research", Version: 3}},
			Tools:          &slot.CapabilityList{Required: []string{"web"}, Optional: []string{"notes"}},
			Runtime:        &slot.RuntimeRequirements{Required: []string{"net"}, WorkspaceAccess: "read"},
		},
	}
}

func testBinding(agentID, status string) slot.Binding {
	return slot.Binding{
		BindingID: "bd_1", AgentID: agentID, SlotInstanceID: "si_1",
		SlotSpecVersion: 2, Scope: "workspace", Status: status,
	}
}

func testMission() mission.Mission {
	return mission.Mission{
		MissionID: "mi_1", ContractVersion: 3, Status: mission.StatusReady,
		OwnerAgentID: "ag_own", Requester: mission.RequesterOwner,
		RoomSource: &mission.RoomSource{RoomID: "rm_1", TaskID: "tsk_1"},
		Content: mission.Content{
			Objective: "目标", Scope: "范围",
			AcceptanceCriteria: []string{"验收一"}, DeliverableSpecs: []string{"交付一"},
			SourceRefs: []string{"r:1"}, Budget: mission.DefaultBudget(),
		},
	}
}

type fixture struct {
	svc  *Service
	room *fakeRooms
}

// newFixture 默认全绿：owner 任职 active、授权就绪、房间有 3 条 public 消息
// （seq 1 房间创建型非消息事件、seq 2/4 public 消息、seq 3 participants 私有）。
func newFixture(t *testing.T) fixture {
	t.Helper()
	rooms := &fakeRooms{events: map[string][]RoomEvent{
		"rm_1": {
			{Seq: 1, EventID: "evt_1", Type: "room.created", Visibility: "public", ParticipantID: "par_own", ActorKind: "user"},
			{Seq: 2, EventID: "evt_2", Type: "message.posted", Visibility: "public", ParticipantID: "par_a", ActorKind: "agent", Body: "公开消息甲"},
			{Seq: 3, EventID: "evt_3", Type: "message.posted", Visibility: "participants", ParticipantID: "par_b", ActorKind: "agent", Body: "私有消息乙"},
			{Seq: 4, EventID: "evt_4", Type: "message.posted", Visibility: "public", ParticipantID: "par_c", ActorKind: "agent", Body: "公开消息丙"},
		},
	}}
	svc := New(NewMemStore(), Ports{
		Missions:  fakeMissions{m: testMission(), ok: true},
		Bindings:  fakeBindings{facts: map[string]BindingFacts{"bd_1": {Binding: testBinding("ag_own", slot.BindingActive), Spec: testSpec()}}},
		Readiness: fakeReadiness{report: readyReport()},
		Grants:    fakeGrants{grant: slot.AgentGrant{AgentID: "ag_own", Tools: []string{"web", "notes"}}},
		Rooms:     rooms,
	})
	return fixture{svc: svc, room: rooms}
}

func layerOf(t *testing.T, m Manifest, name string) Layer {
	t.Helper()
	for _, l := range m.Layers {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("层 %q 不在账本中（共 %d 层）", name, len(m.Layers))
	return Layer{}
}

func TestBuildAssemblesAllLayers(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	m := res.Manifest
	if m.State != StateAssembled || m.ReasonCode != "" {
		t.Fatalf("期望 assembled 无原因，得到 %s/%q", m.State, m.ReasonCode)
	}
	if len(m.Layers) != 9 {
		t.Fatalf("期望 9 层（6 建模 + 3 如实省略），得到 %d", len(m.Layers))
	}
	for _, name := range []string{LayerConstraints, LayerResponsibility, LayerContract, LayerTools, LayerSources, LayerRecentMessages} {
		if l := layerOf(t, m, name); l.State != LayerIncluded {
			t.Fatalf("层 %s 期望 included，得到 %s（%s）", name, l.State, l.ReasonCode)
		}
	}
	for name, reason := range map[string]string{
		LayerCheckpoint:      ReasonCheckpointUnavailable,
		LayerMemory:          ReasonMemoryUnavailable,
		LayerDecisionCapsule: ReasonDecisionCapsuleUnavailable,
	} {
		l := layerOf(t, m, name)
		if l.State != LayerOmitted || l.ReasonCode != reason || l.Bytes != 0 {
			t.Fatalf("层 %s 期望 omitted/%s/0，得到 %s/%s/%d", name, reason, l.State, l.ReasonCode, l.Bytes)
		}
	}
	// 层内容：约束（readiness + 有效格栅 + 工具交集 + 委派缺省不允许）
	cc, ok := layerOf(t, m, LayerConstraints).Content.(ConstraintsContent)
	if !ok {
		t.Fatalf("约束层内容类型错误：%T", layerOf(t, m, LayerConstraints).Content)
	}
	if !cc.Readiness.Ready || cc.EffectiveWorkspaceAccess != "read" {
		t.Fatalf("约束层 readiness/格栅错误：%+v", cc.Readiness)
	}
	if strings.Join(cc.EffectiveTools, ",") != "web,notes" {
		t.Fatalf("有效工具交集错误：%v", cc.EffectiveTools)
	}
	if len(cc.Delegation.Allowed) != 0 || cc.Delegation.MaxDepth != 0 {
		t.Fatalf("委派缺省应不允许：%+v", cc.Delegation)
	}
	// 职责层（模板钉版本 + 任职）
	rc, ok := layerOf(t, m, LayerResponsibility).Content.(ResponsibilityContent)
	if !ok || rc.SpecName != "调研员" || rc.Binding.BindingID != "bd_1" || rc.SpecDeliverables[0] != "调研报告" {
		t.Fatalf("职责层内容错误：%+v", rc)
	}
	// 契约层全文（含钉住版本与来源）
	kc, ok := layerOf(t, m, LayerContract).Content.(ContractContent)
	if !ok || kc.MissionID != "mi_1" || kc.ContractVersion != 3 || kc.RoomSource == nil || kc.RoomSource.RoomID != "rm_1" {
		t.Fatalf("契约层内容错误：%+v", kc)
	}
	// 工具层
	tc, ok := layerOf(t, m, LayerTools).Content.(ToolsContent)
	if !ok || strings.Join(tc.Required, ",") != "web" || strings.Join(tc.Optional, ",") != "notes" {
		t.Fatalf("工具层内容错误：%+v", tc)
	}
	// 引用层
	sc, ok := layerOf(t, m, LayerSources).Content.(SourcesContent)
	if !ok || sc.Refs[0] != "r:1" || sc.RoomSource.TaskID != "tsk_1" {
		t.Fatalf("引用层内容错误：%+v", sc)
	}
	// 总字节 = 各 included 层之和
	var sum int
	for _, l := range m.Layers {
		if l.State == LayerIncluded {
			sum += l.Bytes
		}
	}
	if m.TotalBytes != sum {
		t.Fatalf("total_bytes %d ≠ 各层和 %d", m.TotalBytes, sum)
	}
	// 回执：账目 9 条、不复制内容、digest 同源、政策版本
	if res.Receipt.ManifestID != m.ManifestID || res.Receipt.Digest != m.Digest {
		t.Fatalf("回执关联错误：%+v", res.Receipt)
	}
	if res.Receipt.PolicyVersion != PolicyVersion || len(res.Receipt.Accounting) != 9 {
		t.Fatalf("回执账目/政策版本错误：%d 层 %s", len(res.Receipt.Accounting), res.Receipt.PolicyVersion)
	}
	for _, a := range res.Receipt.Accounting {
		l := layerOf(t, m, a.Name)
		if a.State != l.State || a.Bytes != l.Bytes {
			t.Fatalf("回执账目与层账本不一致（%s）：%+v vs %s/%d", a.Name, a, l.State, l.Bytes)
		}
	}
}

func TestBuildRecentMessagesFilterBeforeCount(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	l := layerOf(t, res.Manifest, LayerRecentMessages)
	mc, ok := l.Content.(RecentMessagesContent)
	if !ok {
		t.Fatalf("消息层内容类型错误：%T", l.Content)
	}
	// 只含两条 public 消息、时序旧→新；私有/非消息事件不进入不计数
	if len(mc.Messages) != 2 || mc.Messages[0].EventID != "evt_2" || mc.Messages[1].EventID != "evt_4" {
		t.Fatalf("消息窗错误：%+v", mc.Messages)
	}
	if l.Dropped != 0 || l.TruncatedMessages != 0 {
		t.Fatalf("全量容纳时不应裁剪：dropped=%d truncated=%d", l.Dropped, l.TruncatedMessages)
	}
	// 水位 = 实际交付的最新 seq（交付前沿——非 public 的 seq 3 不可见）
	if len(res.Manifest.Watermarks) != 1 || res.Manifest.Watermarks[0].Stream != "room:rm_1" || res.Manifest.Watermarks[0].Position != 4 {
		t.Fatalf("水位向量错误：%+v", res.Manifest.Watermarks)
	}
	// 非 public 内容不得出现在账本任何位置（存在性不泄漏）
	blob, _ := json.Marshal(res.Manifest)
	if strings.Contains(string(blob), "私有消息乙") || strings.Contains(string(blob), "evt_3") {
		t.Fatalf("私有事件泄漏进清单：%s", string(blob)[:400])
	}
}

func TestBuildRecentMessagesNoSource(t *testing.T) {
	f := newFixture(t)
	m := testMission()
	m.RoomSource = nil
	f.svc.ports.Missions = fakeMissions{m: m, ok: true}
	res, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	l := layerOf(t, res.Manifest, LayerRecentMessages)
	if l.State != LayerOmitted || l.ReasonCode != ReasonNoSourceRoom {
		t.Fatalf("无来源房应省略 no_source_room，得到 %s/%s", l.State, l.ReasonCode)
	}
	if len(res.Manifest.Watermarks) != 0 {
		t.Fatalf("无来源房不应有水位：%+v", res.Manifest.Watermarks)
	}
}

func TestBuildRecentMessagesQuotaTruncation(t *testing.T) {
	f := newFixture(t)
	// 200 条 ~1KB public 消息 → 64KiB 配额只装得下约 60 条，其余 dropped
	var events []RoomEvent
	for i := 1; i <= 200; i++ {
		events = append(events, RoomEvent{
			Seq: int64(i), EventID: "evt_q", Type: "message.posted", Visibility: "public",
			ParticipantID: "par_a", ActorKind: "agent", Body: strings.Repeat("x", 1000),
		})
	}
	f.room.events["rm_1"] = events
	res, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	l := layerOf(t, res.Manifest, LayerRecentMessages)
	mc := l.Content.(RecentMessagesContent)
	if l.Dropped == 0 || len(mc.Messages)+l.Dropped != 200 {
		t.Fatalf("配额裁剪账目错误：included=%d dropped=%d", len(mc.Messages), l.Dropped)
	}
	// 装的是最新的：水位 = 200；窗口末条 seq 亦为 200
	if res.Manifest.Watermarks[0].Position != 200 || mc.Messages[len(mc.Messages)-1].Seq != 200 {
		t.Fatalf("应从新往旧装满：水位=%d 末条=%d", res.Manifest.Watermarks[0].Position, mc.Messages[len(mc.Messages)-1].Seq)
	}
	if l.Bytes > RecentMessagesQuotaBytes {
		t.Fatalf("消息层 %d 超配额 %d", l.Bytes, RecentMessagesQuotaBytes)
	}
}

func TestBuildPerMessageTruncation(t *testing.T) {
	f := newFixture(t)
	f.room.events["rm_1"] = []RoomEvent{{
		Seq: 7, EventID: "evt_big", Type: "message.posted", Visibility: "public",
		ParticipantID: "par_a", ActorKind: "agent", Body: strings.Repeat("长", 20000),
	}}
	res, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	l := layerOf(t, res.Manifest, LayerRecentMessages)
	if l.TruncatedMessages != 1 {
		t.Fatalf("单条超限应记截断：truncated=%d", l.TruncatedMessages)
	}
	mc := l.Content.(RecentMessagesContent)
	if n := len(mc.Messages[0].Body); n > MessageCapBytes {
		t.Fatalf("截断后单条 %d 仍超上限 %d", n, MessageCapBytes)
	}
}

func TestBuildBlockedPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		binding slot.Binding
		ready   bool
		want    string
	}{
		{"执行者非负责人优先", testBinding("ag_other", slot.BindingReleased), true, ReasonOwnerMismatch},
		{"任职非活跃次之", testBinding("ag_own", slot.BindingReleased), true, ReasonBindingNotActive},
		{"授权缺失再次", testBinding("ag_own", slot.BindingActive), false, ReasonReadinessNotMet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.svc.ports.Bindings = fakeBindings{facts: map[string]BindingFacts{"bd_1": {Binding: tc.binding, Spec: testSpec()}}}
			rep := readyReport()
			rep.Ready = tc.ready
			if !tc.ready {
				rep.Satisfied = nil
				rep.Missing = []string{"tools.required:web"}
			}
			f.svc.ports.Readiness = fakeReadiness{report: rep}
			res, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
			if err != nil {
				t.Fatalf("装配失败：%v", err)
			}
			if res.Manifest.State != StateBlocked || res.Manifest.ReasonCode != tc.want {
				t.Fatalf("期望 blocked/%s，得到 %s/%s", tc.want, res.Manifest.State, res.Manifest.ReasonCode)
			}
			// blocked 也存完整账本（钉住一切引用与版本）
			if len(res.Manifest.Layers) != 9 || layerOf(t, res.Manifest, LayerContract).State != LayerIncluded {
				t.Fatalf("blocked 清单账本不完整：%d 层", len(res.Manifest.Layers))
			}
		})
	}
}

func TestBuildRequiredOverBudget(t *testing.T) {
	f := newFixture(t)
	// 契约层（必需）渲染全量 Content——巨型引用列表必超 256KiB 总预算
	// （HTTP 入口经 mission 校验不可达；域级守卫仍须成立并 UT 钉住）。
	m := testMission()
	refs := make([]string, 0, 6000)
	for i := 0; i < 6000; i++ {
		refs = append(refs, strings.Repeat("r", 128))
	}
	m.SourceRefs = refs
	f.svc.ports.Missions = fakeMissions{m: m, ok: true}
	res, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if res.Manifest.State != StateBlocked || res.Manifest.ReasonCode != ReasonRequiredOverBudget {
		t.Fatalf("期望 blocked/required_over_budget，得到 %s/%s", res.Manifest.State, res.Manifest.ReasonCode)
	}
}

func TestBuildValidation(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Build(context.Background(), "mi_1", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空 binding_id 期望 ErrInvalidRequest，得到 %v", err)
	}
	if _, err := f.svc.Build(context.Background(), "mi_x", "bd_1"); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("未知 Mission 期望 ErrMissionNotFound，得到 %v", err)
	}
	m := testMission()
	m.Status = mission.StatusCanceled
	f.svc.ports.Missions = fakeMissions{m: m, ok: true}
	if _, err := f.svc.Build(context.Background(), "mi_1", "bd_1"); !errors.Is(err, ErrMissionCanceled) {
		t.Fatalf("canceled 期望 ErrMissionCanceled，得到 %v", err)
	}
	f.svc.ports.Missions = fakeMissions{m: testMission(), ok: true}
	if _, err := f.svc.Build(context.Background(), "mi_1", "bd_x"); !errors.Is(err, ErrBindingNotFound) {
		t.Fatalf("未知任职期望 ErrBindingNotFound，得到 %v", err)
	}
}

func TestBuildDeterministicDigestAndReplay(t *testing.T) {
	f := newFixture(t)
	first, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("首装失败：%v", err)
	}
	// 独立服务实例同状态重装 → digest 相同（确定性）
	other := newFixture(t)
	second, err := other.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("重装失败：%v", err)
	}
	if first.Manifest.Digest != second.Manifest.Digest {
		t.Fatalf("同状态 digest 不一致：%s vs %s", first.Manifest.Digest, second.Manifest.Digest)
	}
	// 同库重建 → replayed=true 返回原清单
	replay, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("重建失败：%v", err)
	}
	if !replay.Replayed || replay.Manifest.ManifestID != first.Manifest.ManifestID {
		t.Fatalf("确定性重建应重放原清单：replayed=%v id=%s", replay.Replayed, replay.Manifest.ManifestID)
	}
	// 状态变化（房间新事件）→ digest 变 → 追加新行
	f.room.events["rm_1"] = append(f.room.events["rm_1"], RoomEvent{
		Seq: 5, EventID: "evt_5", Type: "message.posted", Visibility: "public",
		ParticipantID: "par_a", ActorKind: "agent", Body: "新消息",
	})
	fresh, err := f.svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatalf("变化后装配失败：%v", err)
	}
	if fresh.Replayed || fresh.Manifest.Digest == first.Manifest.Digest {
		t.Fatalf("状态变化应追加新清单：replayed=%v", fresh.Replayed)
	}
	list, err := f.svc.ListByMission(context.Background(), "mi_1")
	if err != nil || len(list) != 2 {
		t.Fatalf("清单历史应 2 条：%v %d", err, len(list))
	}
	if list[0].ManifestID != fresh.Manifest.ManifestID {
		t.Fatalf("列表应创建序倒序（新在前）：%+v", list)
	}
}

func TestGetUnknownManifest(t *testing.T) {
	f := newFixture(t)
	if _, _, ok, err := f.svc.Get(context.Background(), "cmf_x"); err != nil || ok {
		t.Fatalf("未知清单期望 (nil,nil,false,nil)，得到 ok=%v err=%v", ok, err)
	}
}
