// Package missioncontrol 任务控制编排（0.6.6 / ADR-0029）：mission 状态机
// CAS + run 停止面组合——暂停（持久先行 + 异步停止）、恢复（回调度列）、
// 取消（两段：canceling → 停止 → canceled，有界等待）。
package missioncontrol

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

// ---- 小端口 fakes（run 域导出接口的本包实现）----

// stubAgents mission.AgentLookup 桩（owner 存在性）。
type stubAgents struct{ ids map[string]bool }

func (a stubAgents) AgentExists(_ context.Context, id string) (bool, error) {
	return a.ids[id], nil
}

type ctlMissions struct{ svc *mission.Service }

func (p ctlMissions) ListReady(ctx context.Context) ([]missionrun.MissionRef, error) {
	list, err := p.svc.ListMissions(ctx)
	if err != nil {
		return nil, err
	}
	var out []missionrun.MissionRef
	for _, m := range list {
		if m.Status == mission.StatusReady {
			out = append(out, missionrun.MissionRef{MissionID: m.MissionID, OwnerAgentID: m.OwnerAgentID})
		}
	}
	return out, nil
}
func (p ctlMissions) SetRunning(ctx context.Context, id string) (bool, error) {
	return p.svc.MarkRunning(ctx, id)
}
func (p ctlMissions) SetReadyFromRunning(ctx context.Context, id, reason string) error {
	return p.svc.MarkReadyFromRunning(ctx, id, reason)
}
func (p ctlMissions) StatusOf(ctx context.Context, id string) (string, bool, error) {
	m, ok, err := p.svc.Get(ctx, id)
	if err != nil || !ok {
		return "", ok, err
	}
	return m.Status, true, nil
}
func (p ctlMissions) FinishCanceling(ctx context.Context, id, reason string) error {
	_, err := p.svc.FinishCancel(ctx, id, reason)
	return err
}

type ctlBindings struct{}

func (ctlBindings) ActiveOf(_ context.Context, agentID string) (missionrun.BindingFacts, bool, error) {
	if agentID == "ag_ctl" {
		return missionrun.BindingFacts{BindingID: "bd_ctl", AgentID: agentID}, true, nil
	}
	return missionrun.BindingFacts{}, false, nil
}

type ctlRuntimes struct{}

func (ctlRuntimes) FirstReady(context.Context) (missionrun.RuntimeRef, bool, error) {
	return missionrun.RuntimeRef{RuntimeID: "rtx_ctl", EnvironmentID: "env_ctl"}, true, nil
}

type ctlLeases struct {
	mu       sync.Mutex
	released int
}

func (f *ctlLeases) Acquire(context.Context, string, string, int, map[string]int) (missionrun.Lease, error) {
	return missionrun.Lease{LeaseID: "lse_ctl", FenceToken: "fnc_ctl", Generation: 1, EnvironmentID: "env_ctl"}, nil
}
func (f *ctlLeases) Release(context.Context, string) error {
	f.mu.Lock()
	f.released++
	f.mu.Unlock()
	return nil
}
func (f *ctlLeases) CheckFence(context.Context, string, string) (bool, string, error) {
	return true, "", nil
}

type ctlManifests struct{}

func (ctlManifests) Build(context.Context, string, string) (missionrun.Manifest, error) {
	return missionrun.Manifest{ManifestID: "cmf_ctl", State: "assembled"}, nil
}

// ctlBlockExec 阻塞至 ctx 取消的执行器（在途停止面）。
type ctlBlockExec struct {
	started chan struct{}
}

func (e *ctlBlockExec) Submit(ctx context.Context, _ missionrun.ExecutionInput) (string, error) {
	close(e.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func newControl(t *testing.T, exec missionrun.Executor) (*Service, *mission.Service, *ctlLeases, *missionrun.MemStore) {
	t.Helper()
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-10-31T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_ctl" }),
		mission.WithAgentLookup(stubAgents{map[string]bool{"ag_ctl": true}}))
	leases := &ctlLeases{}
	runStore := missionrun.NewMemStore()
	runSvc := missionrun.New(runStore,
		ctlMissions{mSvc}, ctlBindings{}, ctlRuntimes{}, leases, ctlManifests{}, exec,
		missionrun.WithNow(func() string { return "2026-10-31T12:00:00.000Z" }),
		missionrun.WithNewRunID(func() string { return "run_ctl" }),
		missionrun.WithSchedulerDisabled())
	return New(mSvc, runSvc, WithCancelWait(300*time.Millisecond)), mSvc, leases, runStore
}

func readyCtlMission(t *testing.T, mSvc *mission.Service) string {
	t.Helper()
	m, err := mSvc.Create(context.Background(), "ag_ctl", mission.Content{
		Objective: "控制面测试", Scope: "s",
		AcceptanceCriteria: []string{"a"}, DeliverableSpecs: []string{"d"},
		SourceRefs: []string{"r:1"}, Budget: mission.DefaultBudget(),
	}, nil)
	if err != nil {
		t.Fatalf("建 Mission: %v", err)
	}
	return m.MissionID
}

// TestPauseBlocksSchedulingAndStopsInFlight 暂停全链：ready 暂停 → 调度
// 不再拾取；在途暂停 → Run canceled + Mission 停 paused；恢复 → ready。
func TestPauseBlocksSchedulingAndStopsInFlight(t *testing.T) {
	exec := &ctlBlockExec{started: make(chan struct{})}
	s, mSvc, leases, _ := newControl(t, exec)
	ctx := context.Background()

	// ready 暂停（阻止新调度）：随后 run 域 ListReady 不含它
	mi := readyCtlMission(t, mSvc)
	out, err := s.PauseMission(ctx, mi, 1, "先等依赖")
	if err != nil || out.Status != mission.StatusPaused {
		t.Fatalf("暂停: %+v %v", out, err)
	}
	refs, _ := ctlMissions{mSvc}.ListReady(ctx)
	if len(refs) != 0 {
		t.Fatalf("paused 不得入调度列: %+v", refs)
	}
	// 恢复 → 回调度列
	resumed, err := s.ResumeMission(ctx, mi, 1)
	if err != nil || resumed.Status != mission.StatusReady {
		t.Fatalf("恢复: %+v %v", resumed, err)
	}
	refs, _ = ctlMissions{mSvc}.ListReady(ctx)
	if len(refs) != 1 {
		t.Fatalf("恢复后应可调度: %+v", refs)
	}

	// 在途暂停：执行中 Pause → Run canceled + Mission paused + 租约释放
	mi2 := readyCtlMission(t, mSvc)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.runs.ExecuteMission(ctx, missionrun.MissionRef{MissionID: mi2, OwnerAgentID: "ag_ctl"})
	}()
	<-exec.started
	paused, err := s.PauseMission(ctx, mi2, 1, "在途暂停")
	if err != nil || paused.Status != mission.StatusPaused {
		t.Fatalf("在途暂停: %+v %v", paused, err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("停止后执行须返回")
	}
	runs, _ := s.runs.ListRuns(ctx, mi2)
	if len(runs) != 1 || runs[0].State != missionrun.StateCanceled {
		t.Fatalf("在途 Run 须 canceled: %+v", runs)
	}
	leases.mu.Lock()
	rel := leases.released
	leases.mu.Unlock()
	if rel < 1 {
		t.Fatal("停止须释放租约")
	}
	got, _, _ := mSvc.Get(ctx, mi2)
	if got.Status != mission.StatusPaused {
		t.Fatalf("Mission 须停 paused: %+v", got)
	}
}

// TestCancelNoInFlightDirect 无在途：canceling → canceled 同步直达（事件链
// 两段留痕）；重复取消终态拒绝。
func TestCancelNoInFlightDirect(t *testing.T) {
	exec := &ctlBlockExec{started: make(chan struct{})}
	s, mSvc, _, _ := newControl(t, exec)
	ctx := context.Background()
	mi := readyCtlMission(t, mSvc)

	out, err := s.CancelMission(ctx, mi, 1, "计划变更")
	if err != nil || out.Status != mission.StatusCanceled {
		t.Fatalf("无在途直达取消: %+v %v", out, err)
	}
	evs, _, _ := mSvc.MissionEvents(ctx, mi)
	var tos []string
	for _, e := range evs {
		if e.EventType == mission.EventStatusChanged {
			tos = append(tos, string(e.Payload))
		}
	}
	if len(tos) != 2 || !strings.Contains(tos[0], `"to":"canceling"`) || !strings.Contains(tos[1], `"to":"canceled"`) {
		t.Fatalf("事件链须两段: %v", tos)
	}
	if _, err := s.CancelMission(ctx, mi, 1, ""); !errors.Is(err, mission.ErrInvalidTransition) {
		t.Fatalf("终态重复取消: %v", err)
	}
	if _, err := s.CancelMission(ctx, mi, 9, ""); !errors.Is(err, mission.ErrInvalidTransition) {
		t.Fatalf("终态下版本不匹配仍终态拒绝: %v", err)
	}
}

// TestCancelInFlightStops 取消在途：停止被响应 → 同步收尾 canceled。
func TestCancelInFlightStops(t *testing.T) {
	exec := &ctlBlockExec{started: make(chan struct{})}
	s, mSvc, _, _ := newControl(t, exec)
	ctx := context.Background()
	mi := readyCtlMission(t, mSvc)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.runs.ExecuteMission(ctx, missionrun.MissionRef{MissionID: mi, OwnerAgentID: "ag_ctl"})
	}()
	<-exec.started

	out, err := s.CancelMission(ctx, mi, 1, "不要了")
	if err != nil || out.Status != mission.StatusCanceled {
		t.Fatalf("在途取消: %+v %v", out, err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("停止后执行须返回")
	}
	runs, _ := s.runs.ListRuns(ctx, mi)
	if runs[0].State != missionrun.StateCanceled {
		t.Fatalf("Run 须 canceled: %+v", runs[0])
	}
	got, _, _ := mSvc.Get(ctx, mi)
	if got.Status != mission.StatusCanceled {
		t.Fatalf("Mission 须 canceled: %+v", got)
	}
}

// TestCancelTimeoutReturnsCanceling 有界等待超时（孤儿 Run 形态——注册表
// 无条目的未终态 Run，如进程重启残留：停止信号 0 命中，未终态永不清零）：
// 返回 canceling 不挂起；续驱（再次 cancel）幂等不报错。注：在途停止的
// 终理化不依赖执行器响应取消（域内 select 即触发）——执行器不响应时
// Run 亦即时 canceled（结果经缓冲通道丢弃）。
func TestCancelTimeoutReturnsCanceling(t *testing.T) {
	exec := &ctlBlockExec{started: make(chan struct{})}
	s, mSvc, _, runStore := newControl(t, exec)
	ctx := context.Background()
	mi := readyCtlMission(t, mSvc)
	// 播种孤儿未终态 Run（不经 ExecuteMission——注册表无条目）
	if _, err := runStore.CreateRun(ctx, missionrun.Run{
		RunID: "run_orphan", MissionID: mi, Attempt: 1,
		LeaseID: "lse_x", State: missionrun.StateRunning,
		RequestedAt: "t", UpdatedAt: "t",
	}); err != nil {
		t.Fatalf("播种孤儿 Run: %v", err)
	}

	start := time.Now()
	out, err := s.CancelMission(ctx, mi, 1, "超时面")
	if err != nil {
		t.Fatalf("超时取消不应错误: %v", err)
	}
	if out.Status != mission.StatusCanceling {
		t.Fatalf("超时应答 canceling: %+v", out)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("有界等待失真: %v", elapsed)
	}
	// 续驱幂等（canceling 上版本匹配 → 返回当前，不报错）
	again, err := s.CancelMission(ctx, mi, 1, "续驱")
	if err != nil || again.Status != mission.StatusCanceling {
		t.Fatalf("续驱: %+v %v", again, err)
	}
}

// TestPauseCancelErrorFamily 错误族透传：未知 404 域错误、版本冲突。
func TestPauseCancelErrorFamily(t *testing.T) {
	exec := &ctlBlockExec{started: make(chan struct{})}
	s, mSvc, _, _ := newControl(t, exec)
	ctx := context.Background()
	mi := readyCtlMission(t, mSvc)

	if _, err := s.PauseMission(ctx, "mi_none", 1, ""); !errors.Is(err, mission.ErrMissionNotFound) {
		t.Fatalf("未知暂停: %v", err)
	}
	if _, err := s.ResumeMission(ctx, "mi_none", 1); !errors.Is(err, mission.ErrMissionNotFound) {
		t.Fatalf("未知恢复: %v", err)
	}
	if _, err := s.CancelMission(ctx, "mi_none", 1, ""); !errors.Is(err, mission.ErrMissionNotFound) {
		t.Fatalf("未知取消: %v", err)
	}
	if _, err := s.PauseMission(ctx, mi, 9, ""); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("暂停版本冲突: %v", err)
	}
	if _, err := s.CancelMission(ctx, mi, 9, ""); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("取消版本冲突: %v", err)
	}
	if out, err := s.ResumeMission(ctx, mi, 1); err != nil || out.Status != mission.StatusReady {
		t.Fatalf("ready 恢复幂等（版本匹配返回当前）: %+v %v", out, err)
	}
}
