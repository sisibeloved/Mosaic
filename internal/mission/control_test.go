package mission

// 0.6.6 Mission 暂停/恢复/取消（ADR-0029）：控制状态机——ready|running →
// paused → ready；非终态 → canceling → canceled；CAS/幂等/拒绝族；修订
// 保持执行控制投影。测试先行。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestPauseResumeCycle 暂停/恢复闭环：ready → paused（事件留痕）→ ready；
// paused 后 MarkRunning 拒绝（阻止新调度——调度面只认 ready）。
func TestPauseResumeCycle(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)

	out, err := svc.Pause(ctx, m.MissionID, 1, "等外部依赖")
	if err != nil || out.Status != StatusPaused || out.ContractVersion != 1 {
		t.Fatalf("暂停: %+v %v", out, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	last := evs[len(evs)-1]
	if last.EventType != EventStatusChanged || !strings.Contains(string(last.Payload), `"from":"ready"`) ||
		!strings.Contains(string(last.Payload), `"to":"paused"`) ||
		!strings.Contains(string(last.Payload), `"reason":"等外部依赖"`) {
		t.Fatalf("暂停事件失真: %+v %s", last, last.Payload)
	}
	// 暂停后调度投影拒转（paused 非 ready——SetRunning 条件更新无操作）
	if changed, err := svc.MarkRunning(ctx, m.MissionID); err != nil || changed {
		t.Fatalf("paused 不可被调度: changed=%v err=%v", changed, err)
	}
	if got, _, _ := svc.Get(ctx, m.MissionID); got.Status != StatusPaused {
		t.Fatalf("状态须保持 paused: %+v", got)
	}

	resumed, err := svc.Resume(ctx, m.MissionID, 1)
	if err != nil || resumed.Status != StatusReady {
		t.Fatalf("恢复: %+v %v", resumed, err)
	}
	if changed, err := svc.MarkRunning(ctx, m.MissionID); err != nil || !changed {
		t.Fatalf("恢复后可调度: changed=%v err=%v", changed, err)
	}
}

// TestPauseRunningAndProjectionPause running → paused（在途暂停入口）；此后
// run 域回 ready 投影须幂等无操作（Mission 停 paused——ADR-0029 决策 2）。
func TestPauseRunningAndProjectionPause(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
	if changed, _ := svc.MarkRunning(ctx, m.MissionID); !changed {
		t.Fatal("预置 running 失败")
	}

	out, err := svc.Pause(ctx, m.MissionID, 1, "")
	if err != nil || out.Status != StatusPaused {
		t.Fatalf("running → paused: %+v %v", out, err)
	}
	// run 终理化回调：running → ready 投影在 paused 上必须无操作
	if err := svc.MarkReadyFromRunning(ctx, m.MissionID, "run_canceled"); err != nil {
		t.Fatalf("投影不应报错: %v", err)
	}
	got, _, _ := svc.Get(ctx, m.MissionID)
	if got.Status != StatusPaused {
		t.Fatalf("paused 须保持（投影幂等无操作）: %+v", got)
	}
}

// TestPauseIdempotentAndRejects 幂等/拒绝族：已 paused（版本匹配）返回当前；
// draft 不可暂停；canceled/canceling 拒绝；版本冲突。
func TestPauseIdempotentAndRejects(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	ready, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
	draft, _ := svc.Create(ctx, "", fullContent(), nil) // unassigned → draft

	// 幂等：重复暂停返回当前，不重复记事件
	if _, err := svc.Pause(ctx, ready.MissionID, 1, ""); err != nil {
		t.Fatalf("首停: %v", err)
	}
	out, err := svc.Pause(ctx, ready.MissionID, 1, "再按一次")
	if err != nil || out.Status != StatusPaused {
		t.Fatalf("幂等暂停: %+v %v", out, err)
	}
	evs, _, _ := svc.MissionEvents(ctx, ready.MissionID)
	pausedEvents := 0
	for _, e := range evs {
		if strings.Contains(string(e.Payload), `"to":"paused"`) {
			pausedEvents++
		}
	}
	if pausedEvents != 1 {
		t.Fatalf("幂等不得重复记暂停事件: %d", pausedEvents)
	}
	// 幂等但版本不匹配 → 冲突
	if _, err := svc.Pause(ctx, ready.MissionID, 9, ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本冲突: %v", err)
	}

	// draft 拒绝（从未被调度）
	if _, err := svc.Pause(ctx, draft.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("draft 不可暂停: %v", err)
	}

	// 终态/停止中拒绝
	if _, err := svc.MarkCanceling(ctx, ready.MissionID, 1, ""); err != nil {
		t.Fatalf("预置 canceling: %v", err)
	}
	if _, err := svc.Pause(ctx, ready.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("canceling 不可暂停: %v", err)
	}
	if _, err := svc.FinishCancel(ctx, ready.MissionID, "收尾"); err != nil {
		t.Fatalf("收尾: %v", err)
	}
	if _, err := svc.Pause(ctx, ready.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("canceled 不可暂停: %v", err)
	}
}

// TestResumeRejects 恢复拒绝族：非 paused（ready 版本匹配幂等返回；draft/
// running/canceled 拒绝）；版本冲突。
func TestResumeRejects(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil) // ready

	// ready + 版本匹配 → 幂等返回当前（重试安全）
	out, err := svc.Resume(ctx, m.MissionID, 1)
	if err != nil || out.Status != StatusReady {
		t.Fatalf("ready 幂等恢复: %+v %v", out, err)
	}
	// ready + 版本不匹配 → 冲突
	if _, err := svc.Resume(ctx, m.MissionID, 9); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本冲突: %v", err)
	}

	draft, _ := svc.Create(ctx, "", fullContent(), nil)
	if _, err := svc.Resume(ctx, draft.MissionID, 1); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("draft 不可恢复: %v", err)
	}
	if _, err := svc.Resume(ctx, "mi_none", 1); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("未知 Mission: %v", err)
	}
}

// TestCancelTwoStageTerminal 两段取消：非终态 → canceling（事件留痕）→
// canceled；重复取消/终态修订拒绝；FinishCancel 幂等；事件链完整。
func TestCancelTwoStageTerminal(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)

	// 版本冲突先行
	if _, err := svc.MarkCanceling(ctx, m.MissionID, 9, ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("错误版本应冲突: %v", err)
	}
	cing, err := svc.MarkCanceling(ctx, m.MissionID, 1, "计划变更")
	if err != nil || cing.Status != StatusCanceling {
		t.Fatalf("停止意图: %+v %v", cing, err)
	}
	// canceling 续驱幂等（版本匹配返回当前——重复 cancel 不报错）
	again, err := svc.MarkCanceling(ctx, m.MissionID, 1, "续驱")
	if err != nil || again.Status != StatusCanceling {
		t.Fatalf("续驱幂等: %+v %v", again, err)
	}

	out, err := svc.FinishCancel(ctx, m.MissionID, "计划变更")
	if err != nil || out.Status != StatusCanceled {
		t.Fatalf("收尾终态: %+v %v", out, err)
	}
	// FinishCancel 幂等（已 canceled 返回当前）
	dup, err := svc.FinishCancel(ctx, m.MissionID, "")
	if err != nil || dup.Status != StatusCanceled {
		t.Fatalf("重复收尾幂等: %+v %v", dup, err)
	}

	evs, _, _ := svc.MissionEvents(ctx, m.MissionID)
	var chain []string
	for _, e := range evs {
		if e.EventType == EventStatusChanged {
			chain = append(chain, string(e.Payload))
		}
	}
	if len(chain) != 2 || !strings.Contains(chain[0], `"to":"canceling"`) || !strings.Contains(chain[1], `"to":"canceled"`) {
		t.Fatalf("事件链失真: %v", chain)
	}

	// 终态拒绝族
	if _, err := svc.MarkCanceling(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态重复取消应拒绝: %v", err)
	}
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", fullContent()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态修订应拒绝: %v", err)
	}
	if _, err := svc.Pause(ctx, m.MissionID, 1, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态暂停应拒绝: %v", err)
	}
}

// TestFinishCancelRejects FinishCancel 前置校验：非 canceling（ready）→
// 转移拒绝；未知 → 404。
func TestFinishCancelRejects(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
	if _, err := svc.FinishCancel(ctx, m.MissionID, "x"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("非 canceling 收尾应拒绝: %v", err)
	}
	if _, err := svc.FinishCancel(ctx, "mi_none", "x"); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("未知 Mission: %v", err)
	}
}

// TestRevisePreservesControlStates 修订保持执行控制投影（ADR-0029 决策 5）：
// running/paused/canceling 中修订 → 状态保持（只落新版本行）；draft/ready
// 照旧完备性重判。
func TestRevisePreservesControlStates(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()

	for name, pre := range map[string]func(m string){
		"running":   func(m string) { svc.MarkRunning(ctx, m) },
		"paused":    func(m string) { svc.Pause(ctx, m, 1, "") },
		"canceling": func(m string) { svc.MarkCanceling(ctx, m, 1, "") },
	} {
		m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
		pre(m.MissionID)
		c := fullContent()
		c.Objective = "修订后的目标"
		out, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", c)
		if err != nil {
			t.Fatalf("%s 修订: %v", name, err)
		}
		if out.Status != StatusReady && out.Status != StatusRunning && out.Status != StatusPaused && out.Status != StatusCanceling {
			t.Fatalf("修订不应改控制态: %+v", out)
		}
		if out.Status != statusOf2(name) {
			t.Fatalf("%s 修订后状态须保持（得到 %s）", name, out.Status)
		}
		if out.ContractVersion != 2 || out.Objective != "修订后的目标" {
			t.Fatalf("新版本行须落: %+v", out)
		}
	}

	// draft/ready 照旧重判（回归）
	d, _ := svc.Create(ctx, "", fullContent(), nil)
	up, err := svc.Revise(ctx, d.MissionID, 1, "ag_a", fullContent())
	if err != nil || up.Status != StatusReady {
		t.Fatalf("draft 补齐照旧 ready: %+v %v", up, err)
	}
}

func statusOf2(name string) string {
	switch name {
	case "running":
		return StatusRunning
	case "paused":
		return StatusPaused
	default:
		return StatusCanceling
	}
}

// TestPauseValidation reason 上限沿用取消口径（≤200 字）。
func TestPauseValidation(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
	long := strings.Repeat("字", 201)
	if _, err := svc.Pause(ctx, m.MissionID, 1, long); !errors.Is(err, ErrInvalidMission) {
		t.Fatalf("reason 超限: %v", err)
	}
}
