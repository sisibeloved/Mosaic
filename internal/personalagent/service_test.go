package personalagent

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

// ---- 测试件 ----

type fakeRegistry struct {
	exes   []ExecutableSnapshot
	mu     sync.Mutex // 并发同步测试件：ApplyAgentIDs 写侧互斥
	applie map[string]string // ApplyAgentIDs 收到的 exeID → agentID
}

func (f *fakeRegistry) Snapshot() []ExecutableSnapshot {
	return append([]ExecutableSnapshot(nil), f.exes...)
}

func (f *fakeRegistry) ApplyAgentIDs(ids map[string]string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applie == nil {
		f.applie = map[string]string{}
	}
	for k, v := range ids {
		f.applie[k] = v
	}
	return len(ids) > 0, nil
}

// applied 并发安全读取。
func (f *fakeRegistry) applied(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applie[id]
}

func seqID(prefix ...string) func() string {
	n := 0
	return func() string {
		n++
		return "ag_test_" + string(rune('0'+n))
	}
}

func fixedNow() string { return "2026-10-28T10:00:00.000Z" }

func newTestService(t *testing.T, reg Registry) (*Service, *MemStore) {
	t.Helper()
	store := NewMemStore()
	return New(store, reg, WithNewAgentID(seqID()), WithNow(fixedNow)), store
}

// ---- 同步语义族（ADR-0017 §3）----

// TestSyncCreatesAgentsAndAliases 首次同步：每个唯一 BotID 一个 Agent，
// 三别名齐备（bot_id / profile_id / participant_id），可执行项回写 agent_id。
func TestSyncCreatesAgentsAndAliases(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "codex@native:/usr/local/bin/codex", BotID: "codex@native:/usr/local/bin/codex", Adapter: "codex", Runtime: "native", Channel: "cli", Enabled: true},
		{ExeID: "codex@native:/opt/codex", BotID: "codex@native:/opt/codex", Adapter: "codex", Runtime: "native", Channel: "app:codex-desktop", Enabled: false},
		{ExeID: "kimi@wsl:Ubuntu:/home/u/bin/kimi", BotID: "kimi@wsl:Ubuntu:/home/u/bin/kimi", Adapter: "kimi", Runtime: "wsl", Distro: "Ubuntu", Enabled: true},
	}}
	svc, store := newTestService(t, reg)

	rep, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Created != 3 || rep.AliasesAdded != 9 {
		t.Fatalf("report = %+v，want created=3 aliases=9", rep)
	}
	if len(rep.Conflicts) != 0 {
		t.Fatalf("意外冲突: %v", rep.Conflicts)
	}
	agents, _ := store.ListAgents(context.Background())
	if len(agents) != 3 {
		t.Fatalf("agents = %d, want 3", len(agents))
	}
	for _, a := range agents {
		if a.Status != "active" || a.Source != "migration" {
			t.Errorf("agent %s status/source = %s/%s", a.AgentID, a.Status, a.Source)
		}
	}
	// 回写：三个可执行项各得 agent_id，且同 BotID 不同路径不同身份
	if len(reg.applie) != 3 {
		t.Fatalf("ApplyAgentIDs 收到 %d 项, want 3", len(reg.applie))
	}
	ids := map[string]bool{}
	for _, ag := range reg.applie {
		ids[ag] = true
	}
	if len(ids) != 3 {
		t.Fatalf("三可执行项应映射到三个不同 agent，得 %v", reg.applie)
	}
}

// TestSyncIdempotent 幂等：连跑两次，第二次零新建、零新别名、AgentID 不变。
func TestSyncIdempotent(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "codex", Runtime: "native"},
		{ExeID: "e2", BotID: "b2", Adapter: "kimi", Runtime: "native"},
	}}
	svc, store := newTestService(t, reg)
	if _, err := svc.SyncFromHarness(context.Background()); err != nil {
		t.Fatalf("sync1: %v", err)
	}
	before, _ := store.ListAgents(context.Background())

	rep, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync2: %v", err)
	}
	if rep.Created != 0 || rep.AliasesAdded != 0 {
		t.Fatalf("第二次同步应零效果，得 %+v", rep)
	}
	after, _ := store.ListAgents(context.Background())
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("agent 集合漂移: %+v → %+v", before, after)
	}
}

// TestSyncSameBotIDMergesToOneAgent 同 BotID 双可执行项（重绑残窗/防御路径）
// → 归一 Agent；派生别名不重复登记；两可执行项指向同一 agent_id。
func TestSyncSameBotIDMergesToOneAgent(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "claude@native:/old/path", BotID: "claude-b", Adapter: "claude", Runtime: "native"},
		{ExeID: "claude@wsl:Ubuntu:/new/path", BotID: "claude-b", Adapter: "claude", Runtime: "wsl", Distro: "Ubuntu"},
	}}
	svc, store := newTestService(t, reg)
	rep, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Created != 1 {
		t.Fatalf("created = %d, want 1", rep.Created)
	}
	agents, _ := store.ListAgents(context.Background())
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	aliases, _ := store.AliasesOf(context.Background(), agents[0].AgentID)
	if len(aliases) != 3 {
		t.Fatalf("别名应为 3（bot/profile/participant 各一），得 %v", aliases)
	}
	if reg.applie["claude@native:/old/path"] != agents[0].AgentID || reg.applie["claude@wsl:Ubuntu:/new/path"] != agents[0].AgentID {
		t.Fatalf("双可执行项应同指 %s，得 %v", agents[0].AgentID, reg.applie)
	}
}

// TestSyncNewExeLater 后发现的可执行项：仅新增对应 Agent，既有不动。
func TestSyncNewExeLater(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "codex", Runtime: "native"},
	}}
	svc, _ := newTestService(t, reg)
	if _, err := svc.SyncFromHarness(context.Background()); err != nil {
		t.Fatalf("sync1: %v", err)
	}
	reg.exes = append(reg.exes, ExecutableSnapshot{ExeID: "e2", BotID: "b2", Adapter: "zcode", Runtime: "native"})
	rep, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync2: %v", err)
	}
	if rep.Created != 1 {
		t.Fatalf("created = %d, want 1", rep.Created)
	}
	if got, ok := reg.applie["e2"]; !ok || got != "ag_test_2" {
		t.Fatalf("新可执行项 agent_id = %v %v, want ag_test_2", got, ok)
	}
}

// TestSyncAliasConflictNotMerged 别名碰撞指向另一 Agent：不合并、不覆盖，
// 记冲突 note 待人工处置（RFC-0018 §13.2.2"不按同名自动合并"）。
func TestSyncAliasConflictNotMerged(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "codex", Runtime: "native"},
	}}
	svc, store := newTestService(t, reg)
	// 预置：另一 Agent 已占用 b1 的派生 participant 别名
	store.EnsureAgent(context.Background(), Agent{AgentID: "ag_other", DisplayName: "X", Adapter: "echo", Status: "active", Source: "manual", CreatedAt: fixedNow()})
	store.AddAlias(context.Background(), AliasParticipantID, ParticipantIDOf("codex", "b1"), "ag_other")

	rep, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(rep.Conflicts) != 1 {
		t.Fatalf("应记 1 条冲突，得 %v", rep.Conflicts)
	}
	agents, _ := store.ListAgents(context.Background())
	if len(agents) != 2 {
		t.Fatalf("两个 Agent 并存（不合并），得 %d", len(agents))
	}
	// 碰撞别名仍归原主
	if got, _, _ := store.AgentByAlias(context.Background(), AliasParticipantID, ParticipantIDOf("codex", "b1")); got != "ag_other" {
		t.Fatalf("碰撞别名被改写: %s", got)
	}
}

// TestAgentSurvivesExeRemoval 可执行项消失不删 Agent（身份独立于部署位置）。
func TestAgentSurvivesExeRemoval(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "codex", Runtime: "native"},
		{ExeID: "e2", BotID: "b2", Adapter: "kimi", Runtime: "native"},
	}}
	svc, _ := newTestService(t, reg)
	svc.SyncFromHarness(context.Background())
	reg.exes = reg.exes[:1] // kimi 消失
	if _, err := svc.SyncFromHarness(context.Background()); err != nil {
		t.Fatalf("sync2: %v", err)
	}
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("Agent 不随可执行项消失，want 2 got %d", len(views))
	}
}

// TestListJoinsExecutables List 视图：可执行引用现算连接（channel 空值按 cli、
// enabled 如实），Agent 按创建序稳定输出。
func TestListJoinsExecutables(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "codex", Runtime: "native", Enabled: true},
		{ExeID: "e1b", BotID: "b1", Adapter: "codex", Runtime: "wsl", Distro: "Ubuntu", Channel: "app:codex-desktop", Enabled: false},
		{ExeID: "e2", BotID: "b2", Adapter: "kimi", Runtime: "native", Enabled: false},
	}}
	svc, _ := newTestService(t, reg)
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2", len(views))
	}
	var codex *AgentView
	for i := range views {
		if views[i].Adapter == "codex" {
			codex = &views[i]
		}
	}
	if codex == nil {
		t.Fatal("codex agent 缺失")
	}
	if len(codex.Executables) != 2 {
		t.Fatalf("codex executables = %d, want 2", len(codex.Executables))
	}
	if codex.Executables[0].Channel != "cli" {
		t.Fatalf("空 channel 应按 cli 呈现，得 %q", codex.Executables[0].Channel)
	}
	if codex.Executables[1].Channel != "app:codex-desktop" || codex.Executables[1].Enabled {
		t.Fatalf("第二引用字段错: %+v", codex.Executables[1])
	}
	if codex.Aliases.BotID != "b1" || len(codex.Aliases.ProfileIDs) != 1 || len(codex.Aliases.ParticipantIDs) != 1 {
		t.Fatalf("别名集错: %+v", codex.Aliases)
	}
	if codex.DisplayName != "Codex" {
		t.Fatalf("display_name = %q, want Codex", codex.DisplayName)
	}
}

// TestResolveAlias 三向别名解析统一到同一 AgentID；未知别名 ok=false。
func TestResolveAlias(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "codex", Runtime: "native"},
	}}
	svc, _ := newTestService(t, reg)
	svc.SyncFromHarness(context.Background())
	byBot, ok, err := svc.ResolveAlias(context.Background(), AliasBotID, "b1")
	if err != nil || !ok {
		t.Fatalf("bot_id 解析失败: %v %v", ok, err)
	}
	byProf, ok, _ := svc.ResolveAlias(context.Background(), AliasProfileID, ProfileIDOf("codex", "b1"))
	if !ok || byProf != byBot {
		t.Fatalf("profile 解析 %q ≠ bot %q", byProf, byBot)
	}
	byPar, ok, _ := svc.ResolveAlias(context.Background(), AliasParticipantID, ParticipantIDOf("codex", "b1"))
	if !ok || byPar != byBot {
		t.Fatalf("participant 解析 %q ≠ bot %q", byPar, byBot)
	}
	if _, ok, _ := svc.ResolveAlias(context.Background(), AliasBotID, "missing"); ok {
		t.Fatal("未知别名应 ok=false")
	}
}

// TestGetAgent 单查：命中与 404 面。
func TestGetAgent(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "kimi", Runtime: "native"},
	}}
	svc, _ := newTestService(t, reg)
	svc.SyncFromHarness(context.Background())
	v, ok, err := svc.Get(context.Background(), "ag_test_1")
	if err != nil || !ok || v.AgentID != "ag_test_1" {
		t.Fatalf("get: %v %v %+v", ok, err, v)
	}
	if _, ok, _ := svc.Get(context.Background(), "ag_none"); ok {
		t.Fatal("不存在应 ok=false")
	}
}
