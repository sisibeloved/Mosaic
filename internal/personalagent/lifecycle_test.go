package personalagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ---- 生命周期状态机（ADR-0018 / 0.5.1）----

// lifecycleFixture 单 exe 注册表 + 已同步一次的服务件；返回 agentID。
func lifecycleFixture(t *testing.T) (*Service, *fakeRegistry, string) {
	t.Helper()
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "codex@native:/usr/local/bin/codex", BotID: "codex@native:/usr/local/bin/codex", Adapter: "codex", Runtime: "native", Channel: "cli", Enabled: true},
	}}
	svc, _ := newTestService(t, reg)
	if _, err := svc.SyncFromHarness(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	agents, err := svc.List(context.Background())
	if err != nil || len(agents) != 1 {
		t.Fatalf("list: %v agents=%d（want 1）", err, len(agents))
	}
	return svc, reg, agents[0].AgentID
}

// statusChangedOf 解出 status_changed 事件的 payload。
func statusChangedOf(t *testing.T, ev AgentEvent) (from, to, reason string) {
	t.Helper()
	var p struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload 解码: %v", err)
	}
	return p.From, p.To, p.Reason
}

// createdPayloadOf 解出 created 事件的 payload。
func createdPayloadOf(t *testing.T, ev AgentEvent) (source, adapter, name string) {
	t.Helper()
	var p struct {
		Source      string `json:"source"`
		Adapter     string `json:"adapter"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload 解码: %v", err)
	}
	return p.Source, p.Adapter, p.DisplayName
}

// eventTypes 事件类型序列。
func eventTypes(evs []AgentEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.EventType
	}
	return out
}

// TestLifecycleSuspendResumeRoundTrip suspend/resume 往返：状态与事件序列正确，
// reason 记入事件 payload。
func TestLifecycleSuspendResumeRoundTrip(t *testing.T) {
	svc, _, id := lifecycleFixture(t)
	ctx := context.Background()

	res, err := svc.Suspend(ctx, id, "休假")
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if res.View.Status != StatusSuspended {
		t.Fatalf("suspend 后 status=%s", res.View.Status)
	}
	if len(res.Outstanding) != 0 {
		t.Fatalf("suspend 不涉承诺门: %v", res.Outstanding)
	}
	res, err = svc.Resume(ctx, id, "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.View.Status != StatusActive {
		t.Fatalf("resume 后 status=%s", res.View.Status)
	}

	evs, ok, err := svc.Events(ctx, id)
	if err != nil || !ok {
		t.Fatalf("events: %v ok=%v", err, ok)
	}
	want := []string{EventCreated, EventStatusChanged, EventStatusChanged}
	if !equalStrings(eventTypes(evs), want) {
		t.Fatalf("事件序列 = %v, want %v", eventTypes(evs), want)
	}
	if from, to, reason := statusChangedOf(t, evs[1]); from != StatusActive || to != StatusSuspended || reason != "休假" {
		t.Fatalf("suspend 事件 = %s→%s reason=%q", from, to, reason)
	}
	if from, to, _ := statusChangedOf(t, evs[2]); from != StatusSuspended || to != StatusActive {
		t.Fatalf("resume 事件 = %s→%s", from, to)
	}
	if evs[1].Seq <= evs[0].Seq || evs[2].Seq <= evs[1].Seq {
		t.Fatalf("seq 非升序: %v", evs)
	}
}

// TestLifecycleRetireCompletesWhenNoCommitments 承诺门空（0.5.1 生产缺省）：
// retire 一调用经 retiring 至 retired；两段事件各恰一条。
func TestLifecycleRetireCompletesWhenNoCommitments(t *testing.T) {
	svc, _, id := lifecycleFixture(t)
	ctx := context.Background()

	res, err := svc.Retire(ctx, id, "清理旧身份")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if res.View.Status != StatusRetired {
		t.Fatalf("retire 后 status=%s（承诺门空应即完）", res.View.Status)
	}
	if len(res.Outstanding) != 0 {
		t.Fatalf("outstanding = %v, want 空", res.Outstanding)
	}
	evs, _, _ := svc.Events(ctx, id)
	want := []string{EventCreated, EventStatusChanged, EventStatusChanged}
	if !equalStrings(eventTypes(evs), want) {
		t.Fatalf("事件序列 = %v, want %v", eventTypes(evs), want)
	}
	if from, to, reason := statusChangedOf(t, evs[1]); from != StatusActive || to != StatusRetiring || reason != "清理旧身份" {
		t.Fatalf("retiring 事件 = %s→%s reason=%q", from, to, reason)
	}
	if from, to, _ := statusChangedOf(t, evs[2]); from != StatusRetiring || to != StatusRetired {
		t.Fatalf("retired 事件 = %s→%s", from, to)
	}
}

// TestLifecycleRetireFromSuspended suspended 也可退役（active|suspended → retiring）。
func TestLifecycleRetireFromSuspended(t *testing.T) {
	svc, _, id := lifecycleFixture(t)
	ctx := context.Background()
	if _, err := svc.Suspend(ctx, id, ""); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	res, err := svc.Retire(ctx, id, "")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if res.View.Status != StatusRetired {
		t.Fatalf("status=%s, want retired", res.View.Status)
	}
	evs, _, _ := svc.Events(ctx, id)
	if from, to, _ := statusChangedOf(t, evs[len(evs)-2]); from != StatusSuspended || to != StatusRetiring {
		t.Fatalf("retiring 事件 = %s→%s（suspended 应可入 retiring）", from, to)
	}
}

// fakeCommitments 承诺门测试件。
type fakeCommitments struct {
	items []string
}

func (f *fakeCommitments) Outstanding(context.Context, string) ([]string, error) {
	return append([]string(nil), f.items...), nil
}

// TestLifecycleRetireBlockedByCommitments 承诺门非空：停留 retiring + 返回
// 未处置清单；门清空后再 retire 完成且 retiring 事件不重复记。
func TestLifecycleRetireBlockedByCommitments(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "kimi@wsl:Ubuntu:/home/u/bin/kimi", BotID: "kimi@wsl:Ubuntu:/home/u/bin/kimi", Adapter: "kimi", Runtime: "wsl", Distro: "Ubuntu", Enabled: true},
	}}
	store := NewMemStore()
	gate := &fakeCommitments{items: []string{"mission:m1 未交付", "binding:b1 未排空"}}
	svc := New(store, reg, WithNewAgentID(seqID()), WithNow(fixedNow), WithCommitments(gate))
	ctx := context.Background()
	if _, err := svc.SyncFromHarness(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	agents, _ := store.ListAgents(ctx)
	id := agents[0].AgentID

	res, err := svc.Retire(ctx, id, "")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if res.View.Status != StatusRetiring {
		t.Fatalf("status=%s, want retiring（承诺未处置）", res.View.Status)
	}
	if !equalStrings(res.Outstanding, gate.items) {
		t.Fatalf("outstanding = %v, want %v", res.Outstanding, gate.items)
	}
	// 再次 retire：门仍非空 → 停留，retiring 事件不重复
	if _, err := svc.Retire(ctx, id, ""); err != nil {
		t.Fatalf("二度 retire: %v", err)
	}
	evs, _, _ := svc.Events(ctx, id)
	retiringCount := 0
	for _, e := range evs {
		if _, to, _ := statusChangedOf(t, e); to == StatusRetiring {
			retiringCount++
		}
	}
	if retiringCount != 1 {
		t.Fatalf("retiring 事件 %d 条, want 1（重复 retire 不重复记）", retiringCount)
	}
	// 门清空 → 完成
	gate.items = nil
	res, err = svc.Retire(ctx, id, "")
	if err != nil {
		t.Fatalf("门清后 retire: %v", err)
	}
	if res.View.Status != StatusRetired || len(res.Outstanding) != 0 {
		t.Fatalf("status=%s outstanding=%v, want retired/空", res.View.Status, res.Outstanding)
	}
}

// TestLifecycleInvalidTransitions 非法转移一律拒绝（不静默幂等）：
// 终态全拒；状态不匹配拒；未知身份 not found。
func TestLifecycleInvalidTransitions(t *testing.T) {
	svc, _, id := lifecycleFixture(t)
	ctx := context.Background()

	if _, err := svc.Resume(ctx, id, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume(active) err=%v, want ErrInvalidTransition", err)
	}
	if _, err := svc.Suspend(ctx, id, ""); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.Suspend(ctx, id, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("suspend(suspended) err=%v, want ErrInvalidTransition（重放须可见拒绝）", err)
	}
	if _, err := svc.Retire(ctx, id, ""); err != nil {
		t.Fatalf("retire(suspended): %v", err)
	}
	// retired 终态：一切转移拒绝
	if _, err := svc.Suspend(ctx, id, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("suspend(retired) err=%v", err)
	}
	if _, err := svc.Resume(ctx, id, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume(retired) err=%v", err)
	}
	if _, err := svc.Retire(ctx, id, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retire(retired) err=%v", err)
	}
	if _, err := svc.Suspend(ctx, "ag_missing", ""); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("suspend(unknown) err=%v, want ErrAgentNotFound", err)
	}
	// 拒绝不落任何事件
	evs, _, _ := svc.Events(ctx, id)
	if n := len(evs); n != 4 { // created + suspend + retiring + retired
		t.Fatalf("事件 %d 条, want 4（非法转移不落事件）", n)
	}
}

// TestSyncDoesNotResurrectLifecycle 同步只建不改：retired/suspended 不因
// bot_id 重现（重扫/重装/移除再添）复活；新 BotID 仍生 active。
func TestSyncDoesNotResurrectLifecycle(t *testing.T) {
	svc, reg, id := lifecycleFixture(t)
	ctx := context.Background()
	if _, err := svc.Suspend(ctx, id, ""); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.SyncFromHarness(ctx); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	view, ok, _ := svc.Get(ctx, id)
	if !ok || view.Status != StatusSuspended {
		t.Fatalf("re-sync 后 status=%v ok=%v, want suspended", view.Status, ok)
	}
	if _, err := svc.Retire(ctx, id, ""); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// 可执行项移除后再添（同 botID 重现）→ 仍 retired
	reg.exes = nil
	if _, err := svc.SyncFromHarness(ctx); err != nil {
		t.Fatalf("移除后 sync: %v", err)
	}
	reg.exes = []ExecutableSnapshot{
		{ExeID: "codex@native:/usr/local/bin/codex", BotID: "codex@native:/usr/local/bin/codex", Adapter: "codex", Runtime: "native", Enabled: true},
	}
	if _, err := svc.SyncFromHarness(ctx); err != nil {
		t.Fatalf("重添后 sync: %v", err)
	}
	view, ok, _ = svc.Get(ctx, id)
	if !ok || view.Status != StatusRetired {
		t.Fatalf("bot_id 重现后 status=%v, want retired（不复活）", view.Status)
	}

	// 新 BotID → 新 Agent 以 active 出生
	reg.exes = append(reg.exes, ExecutableSnapshot{
		ExeID: "codex@native:/opt/codex", BotID: "codex@native:/opt/codex", Adapter: "codex", Runtime: "native", Enabled: false,
	})
	if _, err := svc.SyncFromHarness(ctx); err != nil {
		t.Fatalf("新 BotID sync: %v", err)
	}
	views, _ := svc.List(ctx)
	if len(views) != 2 {
		t.Fatalf("agents=%d, want 2", len(views))
	}
	for _, v := range views {
		if v.AgentID == id && v.Status != StatusRetired {
			t.Fatalf("旧身份被改写: %s", v.Status)
		}
		if v.AgentID != id && v.Status != StatusActive {
			t.Fatalf("新身份非 active 出生: %s", v.Status)
		}
	}
}

// TestCreatedEventInvariant created 事件唯一性不变量：多轮同步、多可执行项
// 归一、手动建——每 Agent 恰一条 created。
func TestCreatedEventInvariant(t *testing.T) {
	svc, _, id := lifecycleFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := svc.SyncFromHarness(ctx); err != nil {
			t.Fatalf("sync#%d: %v", i, err)
		}
	}
	if _, err := svc.CreateManual(ctx, "长期助手", ""); err != nil {
		t.Fatalf("manual: %v", err)
	}
	views, _ := svc.List(ctx)
	if len(views) != 2 {
		t.Fatalf("agents=%d, want 2", len(views))
	}
	for _, v := range views {
		evs, ok, _ := svc.Events(ctx, v.AgentID)
		if !ok {
			t.Fatalf("events 缺 %s", v.AgentID)
		}
		created := 0
		for _, e := range evs {
			if e.EventType == EventCreated {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("%s created 事件 %d 条, want 1", v.AgentID, created)
		}
		if evs[0].EventType != EventCreated {
			t.Fatalf("%s 首事件 = %s, want created", v.AgentID, evs[0].EventType)
		}
		if source, _, name := createdPayloadOf(t, evs[0]); v.AgentID == id {
			if source != "migration" || name != "Codex" {
				t.Fatalf("迁移 created payload = %s/%s", source, name)
			}
		}
	}
}

// TestCreateManualValidation 手动建：空名/超长名拒绝；adapter 可空；
// source=manual、active 出生、created 事件同落。
func TestCreateManualValidation(t *testing.T) {
	svc, _, _ := lifecycleFixture(t)
	ctx := context.Background()
	if _, err := svc.CreateManual(ctx, "  ", ""); err == nil {
		t.Fatalf("空白名应拒绝")
	}
	if _, err := svc.CreateManual(ctx, strings.Repeat("名", 65), ""); err == nil {
		t.Fatalf("超长名应拒绝")
	}
	view, err := svc.CreateManual(ctx, "长期助手", "codex")
	if err != nil {
		t.Fatalf("manual: %v", err)
	}
	if view.Source != "manual" || view.Status != StatusActive || view.DisplayName != "长期助手" || view.Adapter != "codex" {
		t.Fatalf("manual 形状 = %+v", view.Agent)
	}
	if len(view.Executables) != 0 || view.Aliases.BotID != "" {
		t.Fatalf("manual 无别名/无挂靠: %+v", view)
	}
	// 手动身份不因同步消失/改写
	if _, err := svc.SyncFromHarness(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, ok, _ := svc.Get(ctx, view.AgentID)
	if !ok || got.Source != "manual" || got.Status != StatusActive {
		t.Fatalf("sync 后 manual 身份 = %+v ok=%v", got.Agent, ok)
	}
}

// TestConcurrentTransitionsSingleWinner 并发同域转移：8 路 suspend 恰一胜者，
// 事件恰一条，终态一致。
func TestConcurrentTransitionsSingleWinner(t *testing.T) {
	svc, _, id := lifecycleFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var wins atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Suspend(ctx, id, ""); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("并发 suspend err=%v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("胜者 %d 个, want 1", wins.Load())
	}
	view, _, _ := svc.Get(ctx, id)
	if view.Status != StatusSuspended {
		t.Fatalf("终态 = %s, want suspended", view.Status)
	}
	evs, _, _ := svc.Events(ctx, id)
	changed := 0
	for _, e := range evs {
		if e.EventType == EventStatusChanged {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("status_changed 事件 %d 条, want 1", changed)
	}
}

func equalStrings(a, b []string) bool {
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
