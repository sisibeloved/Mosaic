package personalagent

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// 审校修复件（ADR-0017 v0.2）：#1 并发/中断原子性、#3 跨身份碰撞上报、
// 删除可执行项后别名仍可解析的契约补强。

// TestSyncConcurrentSingleIdentity 并发首次同步（P1#1）：同一新 BotID 上多路
// SyncFromHarness 并发竞争，最终恰一个 Agent、别名唯一、全部回写一致、零冲突——
// 竞争失败者回读胜者，不产生重复或孤立身份。
func TestSyncConcurrentSingleIdentity(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "codex", Runtime: "native"},
		{ExeID: "e2", BotID: "b2", Adapter: "kimi", Runtime: "native"},
	}}
	store := NewMemStore()
	var n atomic.Int64
	svc := New(store, reg, WithNewAgentID(func() string {
		// 每路各取不同候选号——并发下两路都可能走到"建行"分支（审校 P1 场景），
		// 原子认领必须只允许一个候选落库，另一路回读胜者
		return "ag_race_" + strconv.FormatInt(n.Add(1), 10)
	}), WithNow(fixedNow))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.SyncFromHarness(context.Background()); err != nil {
				t.Errorf("并发同步: %v", err)
			}
		}()
	}
	wg.Wait()

	agents, _ := store.ListAgents(context.Background())
	if len(agents) != 2 {
		t.Fatalf("并发后应恰 2 个 Agent（b1/b2 各一），得 %d: %+v", len(agents), agents)
	}
	// 别名唯一归属：每个 bot_id 恰指向一个 Agent，且可解析
	for _, bot := range []string{"b1", "b2"} {
		owner, ok, _ := store.AgentByAlias(context.Background(), AliasBotID, bot)
		if !ok || owner == "" {
			t.Fatalf("bot %s 未认领", bot)
		}
		aliases, _ := store.AliasesOf(context.Background(), owner)
		if len(aliases) != 3 {
			t.Fatalf("agent %s 别名应 3 条，得 %v", owner, aliases)
		}
	}
	// 回写一致：同 bot 的可执行项全部指向同一胜者
	if reg.applied("e1") == "" || reg.applied("e2") == "" {
		t.Fatalf("回写缺失")
	}
	owner1, _, _ := store.AgentByAlias(context.Background(), AliasBotID, "b1")
	if reg.applied("e1") != owner1 {
		t.Fatalf("e1 回写 %s ≠ 胜者 %s", reg.applied("e1"), owner1)
	}
	// 无孤立身份：每个落库 Agent 都被别名认领（原子性——建行必随认领）
	for _, a := range agents {
		aliases, _ := store.AliasesOf(context.Background(), a.AgentID)
		if len(aliases) == 0 {
			t.Fatalf("孤立 Agent（无任何别名认领）: %+v", a)
		}
	}
}

// TestDerivedAliasCollisionAcrossBotsReported 跨身份派生碰撞（P2#3）：
// 同 adapter 下 "/a/b" 与 "/a_b" 经 sanitize 得同一派生别名——第二身份必须
// 进入冲突检测并出现在 Conflicts（不得被本轮 seenAlias 去重静默吞掉），
// 别名归属不被改写，两个 Agent 并存不合并。
func TestDerivedAliasCollisionAcrossBotsReported(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "codex@native:/opt/a/b/codex", BotID: "codex@native:/opt/a/b/codex", Adapter: "codex", Runtime: "native"},
		{ExeID: "codex@native:/opt/a_b/codex", BotID: "codex@native:/opt/a_b/codex", Adapter: "codex", Runtime: "native"},
	}}
	svc, store := newTestService(t, reg)

	// 先钉住前提：两个 BotID 的派生 participant 别名确实同值（sanitize 归一化）
	p1, p2 := ParticipantIDOf("codex", reg.exes[0].BotID), ParticipantIDOf("codex", reg.exes[1].BotID)
	if p1 != p2 {
		t.Fatalf("测试前提不成立：派生别名不同值 %s vs %s", p1, p2)
	}

	rep, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(rep.Conflicts) == 0 {
		t.Fatal("跨身份派生碰撞必须上报 Conflicts（当前被静默吞掉）")
	}
	agents, _ := store.ListAgents(context.Background())
	if len(agents) != 2 {
		t.Fatalf("两个 BotID = 两个 Agent（不合并），得 %d", len(agents))
	}
	// 派生别名归属第一个身份，第二个身份无派生别名但 bot 别名在
	first, _, _ := store.AgentByAlias(context.Background(), AliasBotID, reg.exes[0].BotID)
	if got, _, _ := store.AgentByAlias(context.Background(), AliasParticipantID, p1); got != first {
		t.Fatalf("碰撞别名归属被改写: %s", got)
	}
	second, _, _ := store.AgentByAlias(context.Background(), AliasBotID, reg.exes[1].BotID)
	secondAliases, _ := store.AliasesOf(context.Background(), second)
	if len(secondAliases) != 1 || secondAliases[0].Kind != AliasBotID {
		t.Fatalf("第二身份应仅存 bot 别名，得 %v", secondAliases)
	}
	// 重跑：冲突持续上报（不得只报一次后静默）
	rep2, _ := svc.SyncFromHarness(context.Background())
	if len(rep2.Conflicts) == 0 {
		t.Fatal("重跑碰撞仍应上报")
	}
}

// TestRemovedExeAliasesStillResolve 删除可执行项后（审校覆盖缺口补强）：
// 该身份三类别名仍可解析、视图 executables 收为 []——身份独立于部署位置的
// 既有契约（ADR-0017 §4）。
func TestRemovedExeAliasesStillResolve(t *testing.T) {
	reg := &fakeRegistry{exes: []ExecutableSnapshot{
		{ExeID: "e1", BotID: "b1", Adapter: "zcode", Runtime: "native"},
		{ExeID: "e2", BotID: "b2", Adapter: "kimi", Runtime: "native"},
	}}
	svc, _ := newTestService(t, reg)
	svc.SyncFromHarness(context.Background())
	reg.exes = reg.exes[:1] // zcode 消失
	svc.SyncFromHarness(context.Background())

	// e2（kimi/b2）被移除；断言对象 = 消失的那个身份
	owner, ok, _ := svc.ResolveAlias(context.Background(), AliasBotID, "b2")
	if !ok || owner == "" {
		t.Fatal("消失可执行项的 bot_id 别名应仍可解析")
	}
	for _, al := range []struct {
		kind  AliasKind
		value string
	}{
		{AliasProfileID, ProfileIDOf("kimi", "b2")},
		{AliasParticipantID, ParticipantIDOf("kimi", "b2")},
	} {
		got, ok, _ := svc.ResolveAlias(context.Background(), al.kind, al.value)
		if !ok || got != owner {
			t.Fatalf("%s 别名解析断裂: %v %s", al.kind, ok, got)
		}
	}
	views, _ := svc.List(context.Background())
	for _, v := range views {
		if v.AgentID == owner {
			if len(v.Executables) != 0 {
				t.Fatalf("消失后 executables 应为 []，得 %v", v.Executables)
			}
			return
		}
	}
	t.Fatal("视图缺失该 Agent")
}
