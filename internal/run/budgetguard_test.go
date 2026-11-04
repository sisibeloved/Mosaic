package run

// 0.6.13 审校修复回归（执行预算面）：#4 墙钟监控独立于执行器阻塞、#5/#8 权威
// 快照（RefOf）——陈旧放宽预算不生效、#14 CreateRun 事务内次数核对（并发孪生）、
// #15 期限时刻比较（小数位数不定）。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestWallClockMonitorIndependentOfExecutor #4：Start 启动独立墙钟监控 ticker
// ——扫描面拉长至一小时（不驱动），阻塞执行器跨多个扫描周期，监控仍按
// deadline 停止（修复前：单一 ticker 同步串行，执行器阻塞连带卡住过期扫描，
// Run 滞留 running）。
func TestWallClockMonitorIndependentOfExecutor(t *testing.T) {
	missions := &fakeMissions{refs: map[string]MissionRef{}, status: map[string]string{}, waitingReason: map[string]string{}}
	missions.refs["mi_s"] = MissionRef{MissionID: "mi_s", OwnerAgentID: "ag_x", WallClockSeconds: 60}
	exec := &stopRacingExec{started: make(chan struct{})}
	clock := &mutableClock{now: "2026-11-03T12:00:00.000000000Z"}
	var n int
	svc := New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_x": {BindingID: "bd_x", AgentID: "ag_x"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		&fakeLeases{fenceOK: true},
		fakeManifests{state: "assembled", layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{"objective": "独立监控", "acceptance_criteria": []string{"a"}}},
		}},
		exec,
		WithNow(clock.Now),
		WithNewRunID(func() string { n++; return "run_m" + itoa(n) }),
		WithScanInterval(time.Hour),               // 扫描面不驱动——监控独立性
		WithWallClockInterval(5*time.Millisecond), // 监控面亚秒级
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	defer svc.Stop()

	done := make(chan Run, 1)
	go func() {
		r, _ := svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_s", OwnerAgentID: "ag_x"})
		done <- r
	}()
	<-exec.started
	waitForRunState(t, svc, "mi_s", StateRunning)

	// 越过 deadline → 独立监控按 RunID 直达停止（无 ScanOnce 参与）
	clock.Set("2026-11-03T12:01:30.000000000Z")
	select {
	case r := <-done:
		if r.State != StateCanceled {
			t.Fatalf("墙钟到期终态须 canceled: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("独立监控未按 deadline 停止（执行器阻塞连带卡住监控）")
	}
	if st := missions.status["mi_s"]; st != "ready" {
		t.Fatalf("停止后 Mission 回 ready: %v", st)
	}
}

// TestExecuteMissionAuthoritativeBudget #5/#8：启动前经 RefOf 重读权威预算——
// 调用方传入的陈旧放宽值（MaxRuns 99 / 无墙钟）不生效：deadline 按现行契约
// 落快照，第二次执行按现行上限拒绝。
func TestExecuteMissionAuthoritativeBudget(t *testing.T) {
	f := newFixture(t)
	f.exec.block = "ok"
	ctx := context.Background()
	// 权威：墙钟 60s + 上限 1（调用方 ref 携陈旧放宽值——须被覆盖）
	f.missions.refs["mi_ab"] = MissionRef{MissionID: "mi_ab", OwnerAgentID: "ag_x",
		SlotInstanceID: "", WallClockSeconds: 60, MaxRuns: 1}
	staleRef := MissionRef{MissionID: "mi_ab", OwnerAgentID: "ag_x", MaxRuns: 99}

	r1, err := f.svc.ExecuteMission(ctx, staleRef)
	if err != nil {
		t.Fatalf("首次执行: %v", err)
	}
	if r1.Deadline == "" {
		t.Fatalf("墙钟按权威快照落 deadline（陈旧 0 不得生效）: %+v", r1)
	}
	if _, err := f.svc.ExecuteMission(ctx, staleRef); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("上限按权威值拒绝（陈旧 99 不得生效）: %v", err)
	}
}

// TestCreateRunBudgetCapConcurrent #14（存储事务面）：已有 1 次终态 Run +
// maxRuns=1，两个并发建行——恰零胜者（旧实现分离检查可双双建行超额）。
func TestCreateRunBudgetCapConcurrent(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	// 播种 1 次终态 Run（无在途——并发探针不拦）
	seeded := Run{RunID: "run_used", MissionID: "mi_race", Attempt: 1, LeaseID: "lse_r",
		State: StateCompleted, RequestedAt: "t", UpdatedAt: "t"}
	if _, err := store.CreateRun(ctx, seeded, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.CreateRun(ctx, Run{
				RunID: "run_twin" + itoa(i), MissionID: "mi_race", LeaseID: "lse_t" + itoa(i),
				State: StatePending, RequestedAt: "t", UpdatedAt: "t",
			}, 1, 0, "")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("并发孪生 %d 须被事务内上限拒绝: %v", i, err)
		}
	}
	runs, _ := store.ListRunsByMission(ctx, "mi_race")
	if len(runs) != 1 {
		t.Fatalf("不得超额建行: %d", len(runs))
	}
}

// TestListExpiredRunningFractional #15（memstore 时刻比较）：now=.11Z 时
// 已过期的 .1Z 命中、未来的 .111Z 不命中（字符串比较恰给出相反结果——
// '.11Z' < '.1Z' 且 '.111Z' < '.11Z'）。
func TestListExpiredRunningFractional(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	mk := func(id, missionID, deadline string) {
		r := Run{RunID: id, MissionID: missionID, Attempt: 1, LeaseID: "lse_f",
			State: StateRunning, RequestedAt: "t", UpdatedAt: "t", Deadline: deadline}
		if _, err := store.CreateRun(ctx, r, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	mk("run_past", "mi_fp", "2026-11-03T12:00:00.1Z")     // 已过期（0.1s）
	mk("run_future", "mi_ff", "2026-11-03T12:00:00.111Z") // 未来（0.111s）
	got, err := store.ListExpiredRunning(ctx, "2026-11-03T12:00:00.11Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != "run_past" {
		t.Fatalf("时刻比较须恰命中已过期行: %+v", got)
	}
}

// TestExecuteMissionSlotInstanceBinding #1（run 面）：slot 意图任务按**实例**固定
// 任职——执行 Run 的 BindingID 取该实例的 active 任职（同人双任职不串用）；
// 任职换人/失效 → ErrNoExecutor 语义拒绝（任务停留 ready 重新派发）。
func TestExecuteMissionSlotInstanceBinding(t *testing.T) {
	missions := &fakeMissions{refs: map[string]MissionRef{}, status: map[string]string{}, waitingReason: map[string]string{}}
	missions.refs["mi_sb"] = MissionRef{MissionID: "mi_sb", OwnerAgentID: "ag_x", SlotInstanceID: "si_rev"}
	bindings := fakeBindings{
		active:     map[string]BindingFacts{"ag_x": {BindingID: "bd_ed", AgentID: "ag_x", SlotSpecID: "slot.edit"}},
		byInstance: map[string]BindingFacts{"si_rev": {BindingID: "bd_rev", AgentID: "ag_x", SlotSpecID: "slot.review"}},
	}
	var n int
	svc := New(NewMemStore(), missions, bindings,
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_t", EnvironmentID: "env_t"}},
		&fakeLeases{fenceOK: true},
		fakeManifests{state: "assembled", layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{"objective": "审校职责", "acceptance_criteria": []string{"a"}}},
		}},
		&fakeExecutor{block: "ok"},
		WithNow(func() string { return "2026-11-03T12:00:00.000000000Z" }),
		WithNewRunID(func() string { n++; return "run_sb" + itoa(n) }),
		WithSchedulerDisabled())
	ctx := context.Background()
	r1, err := svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_sb", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("slot 意图执行: %v", err)
	}
	if r1.BindingID != "bd_rev" {
		t.Fatalf("须按实例固定任职（审校 #1——不得取同人首个 active 串用）: %+v", r1)
	}
	// 换绑（实例任职换人）→ 拒启动；显式路径的同人另一任职不顶替
	missions.refs["mi_sb2"] = MissionRef{MissionID: "mi_sb2", OwnerAgentID: "ag_x", SlotInstanceID: "si_rev"}
	bindings.byInstance["si_rev"] = BindingFacts{BindingID: "bd_rev2", AgentID: "ag_other"}
	if _, err := svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_sb2", OwnerAgentID: "ag_x"}); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("任职换人须 ErrNoExecutor 拒启动: %v", err)
	}
}
