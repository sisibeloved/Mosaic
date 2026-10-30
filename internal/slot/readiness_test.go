package slot

// 0.5.5 职责准入（ADR-0022）：EvaluateReadiness 交集纯函数、AgentGrant
// 授权档案校验与存取、GrantReadinessChecker 经 prepare/retry 的端到端语义、
// 试算三态。测试先行（红→绿）。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fullGrant() AgentGrant {
	return AgentGrant{
		AgentID: "ag_x",
		Skills:  []SkillRef{{Ref: "workspace/skills/evidence-review", Version: 2}},
		Tools:   []string{"documents.read", "documents.write", "web.search"},
		Context: []string{"mission.contract", "decision.capsule", "room.recent"},
		Runtime: AgentGrantRuntime{
			Capabilities:    []string{"execute", "cancel"},
			WorkspaceAccess: "read_write",
		},
	}
}

// TestEvaluateReadinessFullGrant 全授权 → 全满足、ready、有效权限 = 模板要求。
func TestEvaluateReadinessFullGrant(t *testing.T) {
	r := EvaluateReadiness(validContent(), fullGrant())
	if !r.Ready || len(r.Missing) != 0 || len(r.Degraded) != 0 {
		t.Fatalf("全授权应 ready：ready=%v missing=%v degraded=%v", r.Ready, r.Missing, r.Degraded)
	}
	if r.EffectiveWorkspaceAccess != "read_write" {
		t.Fatalf("有效权限应 read_write（min(授权, 要求)）：%s", r.EffectiveWorkspaceAccess)
	}
	for _, want := range []string{
		"skill:workspace/skills/evidence-review@v1", "tool:documents.read", "tool:documents.write",
		"tool:web.search", "context:mission.contract", "runtime:execute", "workspace_access:read_write",
	} {
		if !containsStr(r.Satisfied, want) {
			t.Errorf("满足项缺 %s（%v）", want, r.Satisfied)
		}
	}
}

// TestEvaluateReadinessNoGrant 无档案 = 空授权：一切声明要求缺失 → blocked，
// 选配缺失记降级；有效权限 none。
func TestEvaluateReadinessNoGrant(t *testing.T) {
	r := EvaluateReadiness(validContent(), AgentGrant{AgentID: "ag_x"})
	if r.Ready {
		t.Fatal("空授权对有要求模板不得 ready（安全缺省）")
	}
	for _, want := range []string{
		"skill:workspace/skills/evidence-review@v1", "tool:documents.read",
		"context:mission.contract", "runtime:execute", "workspace_access:read_write（授权 none）",
	} {
		if !containsStr(r.Missing, want) {
			t.Errorf("缺失项缺 %s（%v）", want, r.Missing)
		}
	}
	if !containsStr(r.Degraded, "tool:web.search（选配缺失，降级）") {
		t.Errorf("选配缺失应记降级（%v）", r.Degraded)
	}
	if r.EffectiveWorkspaceAccess != "none" {
		t.Fatalf("有效权限应 none：%s", r.EffectiveWorkspaceAccess)
	}
}

// TestEvaluateReadinessSkillVersionBoundary 授权版本 ≥ 模板引用版即满足
//（工作方法向后兼容）；低版本缺失。
func TestEvaluateReadinessSkillVersionBoundary(t *testing.T) {
	c := validContent()
	g := fullGrant()
	g.Skills = []SkillRef{{Ref: "workspace/skills/evidence-review", Version: 1}}
	if r := EvaluateReadiness(c, g); !containsStr(r.Satisfied, "skill:workspace/skills/evidence-review@v1") {
		t.Fatalf("授权 v1 ≥ 引用 v1 应满足（%v）", r.Satisfied)
	}
	g.Skills = []SkillRef{{Ref: "workspace/skills/evidence-review", Version: 1}}
	c.Skills = []SkillRef{{Ref: "workspace/skills/evidence-review", Version: 3}}
	r := EvaluateReadiness(c, g)
	if r.Ready || !containsStr(r.Missing, "skill:workspace/skills/evidence-review@v3") {
		t.Fatalf("授权 v1 < 引用 v3 应缺失（ready=%v missing=%v）", r.Ready, r.Missing)
	}
}

// TestEvaluateReadinessWorkspaceLattice 权限格栅：授权 ≥ 要求即过、有效 =
// min；授权 < 要求缺失。
func TestEvaluateReadinessWorkspaceLattice(t *testing.T) {
	c := validContent()
	c.Runtime.WorkspaceAccess = "read"
	g := fullGrant() // read_write
	r := EvaluateReadiness(c, g)
	if !r.Ready || r.EffectiveWorkspaceAccess != "read" {
		t.Fatalf("read_write 授权 read 要求：应 ready 且有效 read（%v/%s）", r.Ready, r.EffectiveWorkspaceAccess)
	}
	g.Runtime.WorkspaceAccess = "read"
	c.Runtime.WorkspaceAccess = "read_write"
	r = EvaluateReadiness(c, g)
	if r.Ready || !containsStr(r.Missing, "workspace_access:read_write（授权 read）") {
		t.Fatalf("read 授权 read_write 要求：应缺失（%v/%v）", r.Ready, r.Missing)
	}
	if r.EffectiveWorkspaceAccess != "read" {
		t.Fatalf("有效权限应为授权侧 read：%s", r.EffectiveWorkspaceAccess)
	}
}

// TestEvaluateReadinessNoRequirements 模板未声明要求 → 无门：空授权也 ready；
// 有效权限 = 授权级（无模板约束时授权即上限）。
func TestEvaluateReadinessNoRequirements(t *testing.T) {
	c := SpecContent{Name: "轻职责", Responsibility: "r", Deliverables: []string{"d"}}
	r := EvaluateReadiness(c, AgentGrant{})
	if !r.Ready || len(r.Missing) != 0 || len(r.Degraded) != 0 {
		t.Fatalf("无要求模板空授权应 ready（%v %v %v）", r.Ready, r.Missing, r.Degraded)
	}
	if r.EffectiveWorkspaceAccess != "none" {
		t.Fatalf("空授权有效权限 none：%s", r.EffectiveWorkspaceAccess)
	}
	g := fullGrant()
	if r2 := EvaluateReadiness(c, g); r2.EffectiveWorkspaceAccess != "read_write" {
		t.Fatalf("无模板约束时有效权限 = 授权级：%s", r2.EffectiveWorkspaceAccess)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// newGrantService 装配真实准入门（GrantReadinessChecker）的测试服务。
func newGrantService(t *testing.T, agents AgentLookup, mem *MemStore) *Service {
	t.Helper()
	svc := New(mem, WithNow(fixedNow), WithAgentLookup(agents),
		WithReadiness(NewGrantReadinessChecker(mem)))
	return svc
}

// TestPutGrantValidation 授权档案校验族：合法落库往返；模式 / 重复 ref /
// 枚举 / 上限 / 未知 Agent 拒绝。
func TestPutGrantValidation(t *testing.T) {
	mem := NewMemStore()
	agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_s": "suspended"}}
	svc := newGrantService(t, agents, mem)
	ctx := context.Background()

	g, err := svc.PutGrant(ctx, "ag_a", fullGrant())
	if err != nil {
		t.Fatalf("合法授权应通过：%v", err)
	}
	if g.UpdatedAt != fixedNow() {
		t.Fatalf("UpdatedAt 应取服务时钟：%s", g.UpdatedAt)
	}
	got, ok, err := svc.Grant(ctx, "ag_a")
	if err != nil || !ok {
		t.Fatalf("读取授权：%v %v", ok, err)
	}
	if len(got.Tools) != 3 || got.Runtime.WorkspaceAccess != "read_write" {
		t.Fatalf("往返失真：%+v", got)
	}

	// suspended Agent 也可存档（授权是数据，行为许可另校验）
	if _, err := svc.PutGrant(ctx, "ag_s", AgentGrant{Runtime: AgentGrantRuntime{WorkspaceAccess: "none"}}); err != nil {
		t.Fatalf("suspended Agent 授权是数据，应允许：%v", err)
	}

	for name, bad := range map[string]AgentGrant{
		"skill ref": {AgentID: "ag_a", Skills: []SkillRef{{Ref: "带 空格", Version: 1}}, Runtime: AgentGrantRuntime{WorkspaceAccess: "none"}},
		"skill ver": {AgentID: "ag_a", Skills: []SkillRef{{Ref: "a/b", Version: 0}}, Runtime: AgentGrantRuntime{WorkspaceAccess: "none"}},
		"重复 ref":   {AgentID: "ag_a", Skills: []SkillRef{{Ref: "a/b", Version: 1}, {Ref: "a/b", Version: 2}}, Runtime: AgentGrantRuntime{WorkspaceAccess: "none"}},
		"工具名模式":   {AgentID: "ag_a", Tools: []string{"Bad Name"}, Runtime: AgentGrantRuntime{WorkspaceAccess: "none"}},
		"重复工具":    {AgentID: "ag_a", Tools: []string{"x", "x"}, Runtime: AgentGrantRuntime{WorkspaceAccess: "none"}},
		"权限枚举":    {AgentID: "ag_a", Runtime: AgentGrantRuntime{WorkspaceAccess: "admin"}},
		"空枚举":     {AgentID: "ag_a", Runtime: AgentGrantRuntime{WorkspaceAccess: ""}},
		"能力名模式":   {AgentID: "ag_a", Runtime: AgentGrantRuntime{Capabilities: []string{"Execute"}, WorkspaceAccess: "none"}},
	} {
		if _, err := svc.PutGrant(ctx, g.AgentID, bad); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s 应 ErrInvalidGrant，得到 %v", name, err)
		}
	}
	if _, err := svc.PutGrant(ctx, "ag_none", fullGrant()); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("未知 Agent 应 ErrAgentNotFound：%v", err)
	}
	// 未设置档案读取：ok=false
	if _, ok, _ := svc.Grant(ctx, "ag_s2"); ok {
		t.Fatal("未设置档案应 ok=false")
	}
}

// TestGrantReadinessGateAT07 AT-07 端到端（域内）：必需工具缺失 → prepare
// blocked（missing+degraded 入事件）；补授权（选配仍缺）→ retry ready（降级
// 有报告）；激活；不自动换人不自动扩权——blocked 期间另建任职同样 blocked。
func TestGrantReadinessGateAT07(t *testing.T) {
	mem := NewMemStore()
	agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_b": "active"}}
	svc := newGrantService(t, agents, mem)
	ctx := context.Background()
	inst := mustInstance(t, svc)

	// 无授权 → blocked；事件含 missing 与 degraded 两键
	b, err := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	if err != nil {
		t.Fatalf("建任职: %v", err)
	}
	if b.Status != BindingBlocked {
		t.Fatalf("无授权应 blocked：%s", b.Status)
	}
	evs, _, err := svc.BindingEvents(ctx, b.BindingID)
	if err != nil {
		t.Fatalf("事件: %v", err)
	}
	last := evs[len(evs)-1]
	if !strings.Contains(string(last.Payload), `"missing"`) || !strings.Contains(string(last.Payload), `"degraded"`) {
		t.Fatalf("blocked 事件应含 missing+degraded（%s）", last.Payload)
	}

	// 部分授权：必需齐、选配缺 → retry → ready（降级仍报告）
	partial := fullGrant()
	partial.Tools = []string{"documents.read", "documents.write"} // web.search 选配缺
	if _, err := svc.PutGrant(ctx, "ag_a", partial); err != nil {
		t.Fatalf("补授权: %v", err)
	}
	b2, err := svc.RetryPrepare(ctx, b.BindingID)
	if err != nil {
		t.Fatalf("重试: %v", err)
	}
	if b2.Status != BindingReady {
		t.Fatalf("补授权后重试应 ready：%s", b2.Status)
	}
	evs2, _, _ := svc.BindingEvents(ctx, b.BindingID)
	last2 := evs2[len(evs2)-1]
	if !strings.Contains(string(last2.Payload), "web.search") {
		t.Fatalf("ready 转移事件应记降级（%s）", last2.Payload)
	}

	// 激活闭环
	if _, err := svc.ActivateBinding(ctx, b.BindingID); err != nil {
		t.Fatalf("激活: %v", err)
	}

	// 无授权第二人同样 blocked（不静默换人）
	bB, err := svc.PrepareBinding(ctx, inst, "ag_b", 0)
	if err != nil {
		t.Fatalf("第二人建任职: %v", err)
	}
	if bB.Status != BindingBlocked {
		t.Fatalf("第二人无授权同样 blocked：%s", bB.Status)
	}
}

// TestServiceReadinessProbe 试算：不建记录、无副作用；版本三态
//（缺省最新 / 显式越界 ErrInvalidSpecVersion / 合法钉住）。
func TestServiceReadinessProbe(t *testing.T) {
	mem := NewMemStore()
	agents := &fakeAgents{status: map[string]string{"ag_a": "active"}}
	svc := newGrantService(t, agents, mem)
	ctx := context.Background()
	inst := mustInstance(t, svc)

	// 追加 v2：requirements 收紧（+新必需工具）
	c2 := validContent()
	c2.Name = "研究负责人 v2"
	c2.Tools.Required = append([]string{"reports.export"}, c2.Tools.Required...)
	if _, err := svc.AppendVersion(ctx, "slot.duty", c2); err != nil {
		t.Fatalf("追加 v2: %v", err)
	}

	rep, err := svc.Readiness(ctx, inst, "ag_a", 0)
	if err != nil {
		t.Fatalf("试算: %v", err)
	}
	if rep.SlotSpecVersion != 2 || rep.SlotSpecID != "slot.duty" || rep.SlotInstanceID != inst {
		t.Fatalf("试算上下文失真：%+v", rep)
	}
	if rep.Report.Ready {
		t.Fatal("空授权对 v2 不得 ready")
	}
	if !containsStr(rep.Report.Missing, "tool:reports.export") {
		t.Fatalf("v2 新必需工具应缺失（%v）", rep.Report.Missing)
	}

	// 显式 v1：钉旧版内容（无 reports.export）
	rep1, err := svc.Readiness(ctx, inst, "ag_a", 1)
	if err != nil {
		t.Fatalf("试算 v1: %v", err)
	}
	if rep1.SlotSpecVersion != 1 || containsStr(rep1.Report.Missing, "tool:reports.export") {
		t.Fatalf("显式 v1 应钉旧版（%d %v）", rep1.SlotSpecVersion, rep1.Report.Missing)
	}

	// 越界
	for _, v := range []int{-1, 3, 99} {
		if _, err := svc.Readiness(ctx, inst, "ag_a", v); !errors.Is(err, ErrInvalidSpecVersion) {
			t.Errorf("越界 %d 应 ErrInvalidSpecVersion：%v", v, err)
		}
	}
	// 试算不留痕：无任职记录
	bs, _ := svc.ListBindings(ctx, inst)
	if len(bs) != 0 {
		t.Fatalf("试算不应建任职记录（%d 条）", len(bs))
	}
	// 未知实例 / 未知 Agent
	if _, err := svc.Readiness(ctx, "si_none", "ag_a", 0); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("未知实例应 ErrInstanceNotFound：%v", err)
	}
	if _, err := svc.Readiness(ctx, inst, "ag_none", 0); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("未知 Agent 应 ErrAgentNotFound：%v", err)
	}
}
