package mission

// 0.6.0 MissionContract（ADR-0023）：完备性两态、校验族、CAS 修订、终态、
// 事件序列、缺省填充。测试先行。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func fixedNow() string { return "2026-10-30T12:00:00.000Z" }

type fakeAgents struct{ ids map[string]bool }

func (f fakeAgents) AgentExists(_ context.Context, agentID string) (bool, error) {
	return f.ids[agentID], nil
}

func fullContent() Content {
	return Content{
		Objective:          "完成 0.6.0 契约验收",
		Scope:              "Mosaic 仓库内 Mission 契约域",
		Constraints:        []string{"不得引入外部网络依赖"},
		AcceptanceCriteria: []string{"七端点全绿", "CAS 冲突可见"},
		DeliverableSpecs:   []string{"测试报告"},
		SourceRefs:         []string{"room:abc:seq42"},
		ReturnTargets:      []string{"room:abc"},
	}
}

func newSvc(t *testing.T, agents AgentLookup) (*Service, *MemStore) {
	t.Helper()
	mem := NewMemStore()
	var n atomic.Int64
	svc := New(mem, WithNow(fixedNow),
		WithNewMissionID(func() string { return "mi_t" + strconv.FormatInt(n.Add(1), 10) }),
		WithAgentLookup(agents))
	return svc, mem
}

// TestCreateCompletenessTwoStates 完备性两态：齐备+owner → ready；验收/来源/
// 交付物缺失或 unassigned → draft（保存明确请求，不捏造共识）。
func TestCreateCompletenessTwoStates(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()

	m, err := svc.Create(ctx, "ag_a", fullContent(), nil)
	if err != nil || m.Status != StatusReady || m.ContractVersion != 1 {
		t.Fatalf("完备契约应 ready v1: %+v %v", m, err)
	}
	if m.Requester != "owner" || m.OwnerAgentID != "ag_a" {
		t.Fatalf("requester/owner 失真: %+v", m)
	}

	for name, mutate := range map[string]func(*Content){
		"验收缺失":   func(c *Content) { c.AcceptanceCriteria = nil },
		"交付物缺失": func(c *Content) { c.DeliverableSpecs = nil },
		"来源缺失":   func(c *Content) { c.SourceRefs = nil },
	} {
		c := fullContent()
		mutate(&c)
		m2, err := svc.Create(ctx, "ag_a", c, nil)
		if err != nil || m2.Status != StatusDraft {
			t.Fatalf("%s 应 draft: %+v %v", name, m2, err)
		}
	}
	// unassigned（owner 空串）→ draft
	m2, err := svc.Create(ctx, "", fullContent(), nil)
	if err != nil || m2.Status != StatusDraft {
		t.Fatalf("unassigned 应 draft: %+v %v", m2, err)
	}
}

// TestCreateValidation 校验族：必填/上限/模式/预算界限/委派枚举。
func TestCreateValidation(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()

	for name, mutate := range map[string]func(*Content){
		"objective 空":  func(c *Content) { c.Objective = "  " },
		"scope 空":      func(c *Content) { c.Scope = "" },
		"验收项空白":     func(c *Content) { c.AcceptanceCriteria = []string{" "} },
		"来源模式":      func(c *Content) { c.SourceRefs = []string{"带 空格"} },
		"预算部分":      func(c *Content) { c.Budget = Budget{Concurrency: 2} },
		"预算越界":      func(c *Content) { c.Budget = Budget{Concurrency: 1, MaxRuns: 9999, MaxWallClockSeconds: 3600, MaxRetries: 0, MaxDelegationDepth: 0} },
		"委派枚举":      func(c *Content) { c.Delegation = DelegationPolicy{Allowed: []string{"anyone"}} },
		"约束上限":      func(c *Content) { c.Constraints = repeat(17, "c") },
	} {
		c := fullContent()
		mutate(&c)
		if _, err := svc.Create(ctx, "ag_a", c, nil); !errors.Is(err, ErrInvalidMission) {
			t.Errorf("%s 应 ErrInvalidMission，得到 %v", name, err)
		}
	}

	// 完整预算合法 + 委派合法
	c := fullContent()
	c.Budget = Budget{Concurrency: 2, MaxRuns: 64, MaxWallClockSeconds: 7200, MaxRetries: 1, MaxDelegationDepth: 1}
	c.Delegation = DelegationPolicy{Allowed: []string{"existing_agent"}, MaxDepth: 1}
	if _, err := svc.Create(ctx, "ag_a", c, nil); err != nil {
		t.Fatalf("合法预算/委派应通过: %v", err)
	}

	// 未知 owner / 未装配 lookup
	if _, err := svc.Create(ctx, "ag_none", fullContent(), nil); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("未知 owner 应 ErrAgentNotFound: %v", err)
	}
	bare := New(NewMemStore(), WithNow(fixedNow))
	if _, err := bare.Create(ctx, "ag_a", fullContent(), nil); err == nil || !strings.Contains(err.Error(), "AgentLookup") {
		t.Errorf("未装配 lookup 带 owner 应拒绝: %v", err)
	}
	if _, err := bare.Create(ctx, "", fullContent(), nil); err != nil {
		t.Errorf("未装配 lookup 无 owner 应可用（draft）: %v", err)
	}
}

// TestDefaultNormalize 缺省填充：预算零值 → 安全缺省；空切片 → []。
func TestDefaultNormalize(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	m, err := svc.Create(context.Background(), "ag_a", fullContent(), nil)
	if err != nil {
		t.Fatalf("建契约: %v", err)
	}
	if m.Budget != DefaultBudget() {
		t.Fatalf("预算缺省失真: %+v", m.Budget)
	}
	if m.Constraints == nil || m.SourceRefs == nil || m.Delegation.Allowed == nil {
		t.Fatalf("空切片未补齐: %+v", m.Content)
	}
	if len(m.Delegation.Allowed) != 0 || m.Delegation.MaxDepth != 0 {
		t.Fatalf("委派缺省应为不允许: %+v", m.Delegation)
	}
}

// TestReviseCAS 并发修订恰一胜者（AT-13）：8 路同 expected_version → 1 胜
// 7 败 version_conflict；胜者版本 +1。
func TestReviseCAS(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)

	var wg sync.WaitGroup
	var wins, conflicts atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := fullContent()
			c.Objective = fmt.Sprintf("修订 %d", i)
			out, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", c)
			switch {
			case err == nil:
				wins.Add(1)
				if out.ContractVersion != 2 {
					t.Errorf("胜者应 v2: %d", out.ContractVersion)
				}
			case errors.Is(err, ErrVersionConflict):
				conflicts.Add(1)
			default:
				t.Errorf("并发修订意外错误: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 7 {
		t.Fatalf("恰一胜者: wins=%d conflicts=%d", wins.Load(), conflicts.Load())
	}
}

// TestReviseCompletenessTransition 修订重判完备性：draft 补齐（含 owner）→
// ready（status_changed 事件）；ready 削减验收 → 回 draft（双向留痕）。
func TestReviseCompletenessTransition(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, err := svc.Create(ctx, "", fullContent(), nil) // unassigned → draft
	if err != nil || m.Status != StatusDraft {
		t.Fatalf("unassigned 应 draft: %+v %v", m, err)
	}

	up, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", fullContent())
	if err != nil || up.Status != StatusReady || up.ContractVersion != 2 {
		t.Fatalf("补齐 owner 应 ready v2: %+v %v", up, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	if len(evs) != 3 || evs[1].EventType != EventContractRevised || evs[2].EventType != EventStatusChanged {
		t.Fatalf("事件序列失真: %+v", evs)
	}
	if !strings.Contains(string(evs[2].Payload), `"from":"draft"`) {
		t.Fatalf("status_changed payload 失真: %s", evs[2].Payload)
	}

	down := fullContent()
	down.AcceptanceCriteria = nil
	d2, err := svc.Revise(ctx, m.MissionID, 2, "ag_a", down)
	if err != nil || d2.Status != StatusDraft {
		t.Fatalf("削减验收应回 draft: %+v %v", d2, err)
	}
}

// TestCancelTerminal 取消终态：CAS 正确 → canceled（原因入事件）；重复取消/
// 终态修订/错误版本 → 对应可见拒绝。
func TestCancelTerminal(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)

	if _, err := svc.Cancel(ctx, m.MissionID, 9, ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("错误版本应冲突: %v", err)
	}
	out, err := svc.Cancel(ctx, m.MissionID, 1, "计划变更")
	if err != nil || out.Status != StatusCanceled {
		t.Fatalf("取消: %+v %v", out, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	last := evs[len(evs)-1]
	if !strings.Contains(string(last.Payload), `"reason":"计划变更"`) {
		t.Fatalf("原因应入事件: %s", last.Payload)
	}

	if _, err := svc.Cancel(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("重复取消应转移拒绝: %v", err)
	}
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", fullContent()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态修订应拒绝: %v", err)
	}
	if _, err := svc.Revise(ctx, m.MissionID, 5, "ag_a", fullContent()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态下版本不匹配仍应转移拒绝（终态先判）: %v", err)
	}
}

// TestVersionsAndEvents 版本行升序不可变、事件流只增；未知 mission ok=false。
func TestVersionsAndEvents(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "", fullContent(), nil)
	c2 := fullContent()
	c2.Objective = "v2"
	svc.Revise(ctx, m.MissionID, 1, "ag_a", c2) // draft → ready

	vs, ok, _ := svc.MissionVersions(ctx, m.MissionID)
	if !ok || len(vs) != 2 || vs[0].ContractVersion != 1 || vs[0].Status != StatusDraft || vs[1].Status != StatusReady {
		t.Fatalf("版本行失真: %+v", vs)
	}
	if vs[0].Objective != "完成 0.6.0 契约验收" || vs[1].Objective != "v2" {
		t.Fatalf("版本内容快照失真: %+v", vs)
	}
	if _, ok, _ := svc.MissionVersions(ctx, "mi_none"); ok {
		t.Fatal("未知 mission 版本应 ok=false")
	}
	if _, ok, _ := svc.MissionEvents(ctx, "mi_none"); ok {
		t.Fatal("未知 mission 事件应 ok=false")
	}
	if _, ok, _ := svc.Get(ctx, "mi_none"); ok {
		t.Fatal("未知 mission 详情应 ok=false")
	}
}

func repeat(n int, s string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}
