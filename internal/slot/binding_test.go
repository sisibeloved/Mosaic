package slot

// Binding 任职状态机（RFC-0018 §3.2/§5.2 / ADR-0021，0.5.4）。

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// ---- 测试件 ----

type fakeAgents struct {
	mu     sync.Mutex
	status map[string]string // agentID → status
}

func (f *fakeAgents) AgentStatus(_ context.Context, agentID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.status[agentID]
	return s, ok, nil
}

func newBindingService(t *testing.T) (*Service, *fakeAgents) {
	t.Helper()
	var n atomic.Int64
	agents := &fakeAgents{status: map[string]string{"ag_a": "active", "ag_b": "active", "ag_suspended": "suspended", "ag_retired": "retired"}}
	svc := New(NewMemStore(),
		WithNow(fixedNow),
		WithNewInstanceID(func() string { return "si_test_" + itoa(n.Add(1)) }),
		WithNewNamespace(func() string { return "slotns_test_" + itoa(n.Add(1)) }),
		WithNewBindingID(func() string { return "bd_test_" + itoa(n.Add(1)) }),
		WithAgentLookup(agents),
	)
	return svc, agents
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

func mustInstance(t *testing.T, svc *Service) string {
	t.Helper()
	if _, err := svc.Create(context.Background(), "slot.duty", validContent()); err != nil {
		t.Fatalf("建谱系: %v", err)
	}
	inst, err := svc.CreateInstance(context.Background(), "slot.duty", "")
	if err != nil {
		t.Fatalf("建实例: %v", err)
	}
	return inst.SlotInstanceID
}

func bindingTypes(evs []BindingEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.EventType
	}
	return out
}

func statusPayload(t *testing.T, ev BindingEvent) map[string]string {
	t.Helper()
	var p map[string]string
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return p
}

// TestPrepareBindingHappy prepare happy：默认钉最新版；created + preparing→ready 两事件；
// scope 随实例；Agent 须 active。
func TestPrepareBindingHappy(t *testing.T) {
	svc, _ := newBindingService(t)
	ctx := context.Background()
	inst := mustInstance(t, svc)

	b, err := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !hasPrefix(b.BindingID, "bd_") || b.Status != BindingReady || b.AgentID != "ag_a" ||
		b.SlotSpecVersion != 1 || b.Scope != "workspace" || b.HandoffNote != "" {
		t.Fatalf("prepare 形状: %+v", b)
	}
	evs, _, _ := svc.BindingEvents(ctx, b.BindingID)
	if !equalStrs(bindingTypes(evs), []string{EventCreated, EventStatusChanged}) {
		t.Fatalf("事件序: %v", bindingTypes(evs))
	}
	if p := statusPayload(t, evs[1]); p["from"] != BindingPreparing || p["to"] != BindingReady {
		t.Fatalf("ready 事件: %v", p)
	}

	// 显式钉旧版：模板追加 v2 后，prepare 钉 v1
	c2 := validContent()
	c2.Name = "第二版"
	if _, err := svc.AppendVersion(ctx, "slot.duty", c2); err != nil {
		t.Fatalf("追加: %v", err)
	}
	b2, err := svc.PrepareBinding(ctx, inst, "ag_b", 1)
	if err != nil || b2.SlotSpecVersion != 1 {
		t.Fatalf("钉旧版: %+v %v", b2, err)
	}
	// 默认钉最新
	b3, _ := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	if b3.SlotSpecVersion != 2 {
		t.Fatalf("默认应钉最新 v2: %+v", b3)
	}
	// 超谱系拒
	if _, err := svc.PrepareBinding(ctx, inst, "ag_a", 3); !errors.Is(err, ErrInvalidSpecVersion) {
		t.Fatalf("超谱系 err = %v", err)
	}
}

// TestPrepareBindingAgentGate Agent 校验：未知 404 语义；非 active 拒（挂起/退役）。
func TestPrepareBindingAgentGate(t *testing.T) {
	svc, _ := newBindingService(t)
	ctx := context.Background()
	inst := mustInstance(t, svc)
	if _, err := svc.PrepareBinding(ctx, inst, "ag_missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 Agent err = %v", err)
	}
	for _, id := range []string{"ag_suspended", "ag_retired"} {
		if _, err := svc.PrepareBinding(ctx, inst, id, 0); !errors.Is(err, ErrAgentNotActive) {
			t.Fatalf("%s err = %v, want ErrAgentNotActive", id, err)
		}
	}
	if _, err := svc.PrepareBinding(ctx, "si_missing", "ag_a", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知实例 err = %v", err)
	}
}

// TestActivateBindingAndSingleActive 激活与单 active：二任同实例激活被拒；
// 释放后接任成功（换人 = 新 Binding）。
func TestActivateBindingAndSingleActive(t *testing.T) {
	svc, _ := newBindingService(t)
	ctx := context.Background()
	inst := mustInstance(t, svc)

	first, _ := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	if _, err := svc.ActivateBinding(ctx, first.BindingID); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if first, _, _ := svc.GetBinding(ctx, first.BindingID); first.Status != BindingActive {
		t.Fatalf("状态 = %s", first.Status)
	}

	// 同实例第二任 prepare 允许（succession 预备），activate 被单 active 拒
	second, err := svc.PrepareBinding(ctx, inst, "ag_b", 0)
	if err != nil {
		t.Fatalf("二任 prepare: %v", err)
	}
	if _, err := svc.ActivateBinding(ctx, second.BindingID); !errors.Is(err, ErrActiveBindingExists) {
		t.Fatalf("二任 activate err = %v, want ErrActiveBindingExists", err)
	}

	// 释放首任（含交接备注）→ 二任激活成功
	res, err := svc.ReleaseBinding(ctx, first.BindingID, "交接：在办事项 A 待续")
	if err != nil || res.Binding.Status != BindingReleased || len(res.Outstanding) != 0 {
		t.Fatalf("release: %+v %v", res, err)
	}
	if _, err := svc.ActivateBinding(ctx, second.BindingID); err != nil {
		t.Fatalf("接任 activate: %v", err)
	}
	got, _, _ := svc.GetBinding(ctx, first.BindingID)
	if got.HandoffNote != "交接：在办事项 A 待续" {
		t.Fatalf("交接备注未入档: %q", got.HandoffNote)
	}
}

// TestActivateAgentSuspendedSincePrepare prepare 后 Agent 被挂起 → activate 拒。
func TestActivateAgentSuspendedSincePrepare(t *testing.T) {
	svc, agents := newBindingService(t)
	ctx := context.Background()
	inst := mustInstance(t, svc)
	b, _ := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	agents.mu.Lock()
	agents.status["ag_a"] = "suspended"
	agents.mu.Unlock()
	if _, err := svc.ActivateBinding(ctx, b.BindingID); !errors.Is(err, ErrAgentNotActive) {
		t.Fatalf("err = %v, want ErrAgentNotActive", err)
	}
}

// TestReleaseFlows 释放路径族：ready 单段；active 两段一调用（事件恰
// draining/released 各一）；终态全拒；备注超长拒。
func TestReleaseFlows(t *testing.T) {
	svc, _ := newBindingService(t)
	ctx := context.Background()
	inst := mustInstance(t, svc)

	// ready → released 单段
	neverActive, _ := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	res, err := svc.ReleaseBinding(ctx, neverActive.BindingID, "放弃任职")
	if err != nil || res.Binding.Status != BindingReleased {
		t.Fatalf("ready 释放: %+v %v", res, err)
	}
	evs, _, _ := svc.BindingEvents(ctx, neverActive.BindingID)
	if !equalStrs(bindingTypes(evs), []string{EventCreated, EventStatusChanged, EventStatusChanged}) {
		t.Fatalf("ready 释放事件序: %v", bindingTypes(evs))
	}
	// released 终态：activate/release 全拒
	if _, err := svc.ActivateBinding(ctx, neverActive.BindingID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("released activate err = %v", err)
	}
	if _, err := svc.ReleaseBinding(ctx, neverActive.BindingID, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("released release err = %v", err)
	}

	// active → draining → released 两段一调用
	b2, _ := svc.PrepareBinding(ctx, inst, "ag_b", 0)
	svc.ActivateBinding(ctx, b2.BindingID)
	res, err = svc.ReleaseBinding(ctx, b2.BindingID, "")
	if err != nil || res.Binding.Status != BindingReleased {
		t.Fatalf("active 释放: %+v %v", res, err)
	}
	evs, _, _ = svc.BindingEvents(ctx, b2.BindingID)
	want := []string{EventCreated, EventStatusChanged, EventStatusChanged, EventStatusChanged, EventStatusChanged}
	if !equalStrs(bindingTypes(evs), want) { // created + ready + active + 两段释放
		t.Fatalf("active 释放事件序: %v", bindingTypes(evs))
	}
	if p := statusPayload(t, evs[4]); p["from"] != BindingDraining || p["to"] != BindingReleased {
		t.Fatalf("released 事件: %v", p)
	}

	// 备注超长拒（4096 上限）
	b3, _ := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	if _, err := svc.ReleaseBinding(ctx, b3.BindingID, maxRunes(4097)); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("超长备注 err = %v", err)
	}
}

// fakeDrain 排空门测试件。
type fakeDrain struct {
	mu    sync.Mutex
	items []string
}

func (f *fakeDrain) Outstanding(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.items...), nil
}

// TestReleaseDrainGateBlocked 排空门非空：停留 draining + 未处置清单；清空后
// 重试 release 补终段（draining 事件不重复）。
func TestReleaseDrainGateBlocked(t *testing.T) {
	mem := NewMemStore()
	var n atomic.Int64
	agents := &fakeAgents{status: map[string]string{"ag_a": "active"}}
	drain := &fakeDrain{items: []string{"mission:m1 在途", "workitem:w2 未交付"}}
	svc := New(mem, WithNow(fixedNow),
		WithNewInstanceID(func() string { return "si_d" + itoa(n.Add(1)) }),
		WithNewNamespace(func() string { return "slotns_d" + itoa(n.Add(1)) }),
		WithNewBindingID(func() string { return "bd_d" + itoa(n.Add(1)) }),
		WithAgentLookup(agents), WithDrain(drain))
	ctx := context.Background()
	if _, err := svc.Create(ctx, "slot.duty", validContent()); err != nil {
		t.Fatalf("谱系: %v", err)
	}
	instView, err := svc.CreateInstance(ctx, "slot.duty", "")
	if err != nil {
		t.Fatalf("建实例: %v", err)
	}
	inst := instView.SlotInstanceID
	b, _ := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	svc.ActivateBinding(ctx, b.BindingID)

	res, err := svc.ReleaseBinding(ctx, b.BindingID, "")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Binding.Status != BindingDraining || !equalStrs(res.Outstanding, drain.items) {
		t.Fatalf("draining 停留: %+v", res)
	}
	// 重试仍非空 → draining 事件不重复
	svc.ReleaseBinding(ctx, b.BindingID, "")
	evs, _, _ := svc.BindingEvents(ctx, b.BindingID)
	drainingCount := 0
	for _, e := range evs {
		if p := statusPayload(t, e); p["to"] == BindingDraining {
			drainingCount++
		}
	}
	if drainingCount != 1 {
		t.Fatalf("draining 事件 %d 条, want 1", drainingCount)
	}
	// 门清空 → 重试完成
	drain.mu.Lock()
	drain.items = nil
	drain.mu.Unlock()
	res, err = svc.ReleaseBinding(ctx, b.BindingID, "")
	if err != nil || res.Binding.Status != BindingReleased || len(res.Outstanding) != 0 {
		t.Fatalf("门清后完成: %+v %v", res, err)
	}
}

// fakeReadiness 准入门测试件（0.5.5 预埋）。
type fakeReadiness struct {
	mu      sync.Mutex
	missing []string
}

func (f *fakeReadiness) Check(context.Context, Binding) (bool, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.missing) == 0, append([]string(nil), f.missing...), nil
}

// TestPrepareReadinessBlocked 准入门非空 → blocked（0.5.4 生产不可达，
// 注入钉住 0.5.5 语义）；清空后服务层重试 → ready。
func TestPrepareReadinessBlocked(t *testing.T) {
	mem := NewMemStore()
	var n atomic.Int64
	agents := &fakeAgents{status: map[string]string{"ag_a": "active"}}
	gate := &fakeReadiness{missing: []string{"工具 documents.write 缺失"}}
	svc := New(mem, WithNow(fixedNow),
		WithNewInstanceID(func() string { return "si_r" + itoa(n.Add(1)) }),
		WithNewNamespace(func() string { return "slotns_r" + itoa(n.Add(1)) }),
		WithNewBindingID(func() string { return "bd_r" + itoa(n.Add(1)) }),
		WithAgentLookup(agents), WithReadiness(gate))
	ctx := context.Background()
	svc.Create(ctx, "slot.duty", validContent())
	instView, err := svc.CreateInstance(ctx, "slot.duty", "")
	if err != nil {
		t.Fatalf("建实例: %v", err)
	}
	inst := instView.SlotInstanceID

	b, err := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if b.Status != BindingBlocked {
		t.Fatalf("status = %s, want blocked", b.Status)
	}
	// blocked 不可激活
	if _, err := svc.ActivateBinding(ctx, b.BindingID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("blocked activate err = %v", err)
	}
	// 门清空 → 重试（0.5.5 动作）→ ready
	gate.mu.Lock()
	gate.missing = nil
	gate.mu.Unlock()
	b, err = svc.RetryPrepare(ctx, b.BindingID)
	if err != nil || b.Status != BindingReady {
		t.Fatalf("重试: %+v %v", b, err)
	}
	evs, _, _ := svc.BindingEvents(ctx, b.BindingID)
	if !equalStrs(bindingTypes(evs), []string{EventCreated, EventStatusChanged, EventStatusChanged, EventStatusChanged}) {
		t.Fatalf("blocked 重试事件序: %v", bindingTypes(evs))
	}
}

// TestConcurrentActivateSingleWinner 并发激活两 Binding 于同实例：恰一胜者。
func TestConcurrentActivateSingleWinner(t *testing.T) {
	svc, _ := newBindingService(t)
	ctx := context.Background()
	inst := mustInstance(t, svc)
	b1, _ := svc.PrepareBinding(ctx, inst, "ag_a", 0)
	b2, _ := svc.PrepareBinding(ctx, inst, "ag_b", 0)

	var wg sync.WaitGroup
	wins := 0
	var mu sync.Mutex
	for _, id := range []string{b1.BindingID, b2.BindingID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := svc.ActivateBinding(ctx, id); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrActiveBindingExists) && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("并发激活 err = %v", err)
			}
		}(id)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("胜者 %d, want 1", wins)
	}
	list, _ := svc.ListBindings(ctx, inst)
	active := 0
	for _, b := range list {
		if b.Status == BindingActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active 数 = %d, want 1", active)
	}
}

// TestListBindingsFilter 列表过滤与创建序。
func TestListBindingsFilter(t *testing.T) {
	svc, _ := newBindingService(t)
	ctx := context.Background()
	inst1 := mustInstance(t, svc)
	svc.Create(ctx, "slot.other", validContent())
	other, err := svc.CreateInstance(ctx, "slot.other", "")
	if err != nil {
		t.Fatalf("建第二实例: %v", err)
	}
	inst2 := other.SlotInstanceID

	svc.PrepareBinding(ctx, inst1, "ag_a", 0)
	svc.PrepareBinding(ctx, inst2, "ag_b", 0)
	svc.PrepareBinding(ctx, inst1, "ag_b", 0)

	all, _ := svc.ListBindings(ctx, "")
	if len(all) != 3 {
		t.Fatalf("全量 = %d", len(all))
	}
	only1, _ := svc.ListBindings(ctx, inst1)
	if len(only1) != 2 {
		t.Fatalf("过滤 = %d", len(only1))
	}
	for _, b := range only1 {
		if b.SlotInstanceID != inst1 {
			t.Fatalf("过滤泄漏: %+v", b)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
