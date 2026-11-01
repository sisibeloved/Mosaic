package run

// 0.6.6 审校复现（run 域 P1×3 + P2×2）：
//
//   - P1 收尾窗口起表过早：30s 清理上下文在执行开始时创建——31 秒任务完成
//     时窗口已耗尽，围栏检查/终态写入/租约释放全部随过期上下文静默失败
//     （Run 滞留 running、租约滞留 active）。窗口须自收尾时刻起算。
//   - P1 停止句柄注册过晚：注册在 SetRunning/围栏复核之后——其间暂停已 CAS
//     生效，SignalStopByMission 扫描可见 Run 但注册表无句柄（信号丢失），
//     执行照启、结果照落。注册须先于 Mission 投影（CreateRun 后即注册）。
//   - P1 租约早释放：停止路径终态写入后立即释放租约——执行器 goroutine
//     可能仍在收尾写工作区，新执行随即领取同一工作区。释放须后于执行器
//     退出（有界等待）。
//   - P2 停止终态写库失败被吞：finalizeStopped 对 UpdateRun 失败静默返回，
//     调用方拿到"成功"而 Run 卡 running。须上抛 ErrTerminalWrite。
//   - P2 重启漏收尾：Run 已终态（写库成功）但进程在控制面 FinishCancel 前
//     退出——Mission 滞 canceling 且无未终态 Run，恢复面无从发现。恢复须
//     按状态枚举兜底兑现。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// eventLog 有序事件日志（释放/执行器退出的先后序断言用）。
type eventLog struct {
	mu sync.Mutex
	v  []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.v = append(l.v, s)
}

func (l *eventLog) slice() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.v...)
}

// ignoreCancelExec 取消后仍滞留 work 段才返回（模拟 CLI 收尾写工作区）。
type ignoreCancelExec struct {
	log     *eventLog
	work    time.Duration
	started chan struct{}
}

func (e *ignoreCancelExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	close(e.started)
	<-ctx.Done()
	time.Sleep(e.work)
	e.log.add("exec_return")
	return "", ctx.Err()
}

// gateExec 真实工作开始前若已被取消则立即返回（不启动——对齐真实执行器：
// ctx 已取消时子进程不拉起）。
type gateExec struct {
	started chan struct{}
	ran     bool
	mu      sync.Mutex
}

func (e *gateExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	e.mu.Lock()
	e.ran = true
	e.mu.Unlock()
	close(e.started)
	<-ctx.Done()
	return "", ctx.Err()
}

// TestCleanupWindowStartsAtFinalize P1：清理窗口自收尾起算——执行时长超过
// 窗口预算的任务完成后，终态写入/租约释放不得因过期上下文静默失败。
func TestCleanupWindowStartsAtFinalize(t *testing.T) {
	f := newFixture(t)
	f.svc.executor = &slowOkExec{delay: 60 * time.Millisecond}
	f.svc.cleanupBudget = 20 * time.Millisecond // 窗口 < 执行时长

	r, err := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	if err != nil {
		t.Fatalf("执行: %v", err)
	}
	if r.State != StateCompleted || r.ResultBlock == "" {
		t.Fatalf("31 秒任务完成后不得滞留失败态（窗口须自收尾起算）: %+v", r)
	}
	if f.leases.released < 1 {
		t.Fatal("租约须释放（过期窗口吞掉释放即泄漏至 TTL）")
	}
	runs, _ := f.svc.ListRuns(context.Background(), "mi_a")
	if len(runs) != 1 || runs[0].State != StateCompleted {
		t.Fatalf("库面终态: %+v", runs)
	}
	if f.missions.status["mi_a"] != "ready" {
		t.Fatalf("投影: %v", f.missions.status)
	}
}

// pauseHookLeases 在首次围栏复核时注入并发暂停（模拟真实交错：Mission CAS
// 已翻转 paused + 停止信号已扫描发出——此刻注册表若无句柄即信号丢失）。
type pauseHookLeases struct {
	*fakeLeases
	hook func()
	done bool
}

func (p *pauseHookLeases) CheckFence(ctx context.Context, leaseID, token string) (bool, string, error) {
	if !p.done {
		p.done = true
		p.hook()
	}
	return p.fakeLeases.CheckFence(ctx, leaseID, token)
}

// TestPauseDuringPreExecSignalsHit P1：暂停在 pre-execution 阶段（投影已
// running、句柄未注册的旧窗口）落地——信号必须命中（n=1），执行不得真实
// 启动，结果不得落库。
func TestPauseDuringPreExecSignalsHit(t *testing.T) {
	f := newFixture(t)
	exec := &gateExec{started: make(chan struct{})}
	f.svc.executor = exec
	signalN := 0
	leases := &pauseHookLeases{fakeLeases: f.leases, hook: func() {
		f.missions.mu.Lock()
		f.missions.status["mi_a"] = "paused" // 控制面 CAS 先行
		f.missions.mu.Unlock()
		n, err := f.svc.SignalStopByMission(context.Background(), "mi_a", "pause")
		if err == nil {
			signalN = n
		}
	}}
	f.svc.leases = leases

	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		done <- r
	}()
	var r Run
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("执行未返回")
	}
	if signalN != 1 {
		t.Fatalf("pre-execution 窗口的暂停信号必须命中句柄（n=%d）——注册须先于 Mission 投影", signalN)
	}
	exec.mu.Lock()
	ran := exec.ran
	exec.mu.Unlock()
	if ran {
		t.Fatal("暂停已生效，执行不得真实启动")
	}
	if r.ResultBlock != "" || r.State == StateCompleted {
		t.Fatalf("暂停后不得保存结果: %+v", r)
	}
	if r.State != StateFailed && r.State != StateCanceled {
		t.Fatalf("Run 须终态（failed/canceled 均合法——无执行事实）: %+v", r)
	}
	if f.leases.released < 1 {
		t.Fatal("租约须释放")
	}
	f.missions.mu.Lock()
	st := f.missions.status["mi_a"]
	f.missions.mu.Unlock()
	if st != "paused" {
		t.Fatalf("Mission 须停 paused: %s", st)
	}
}

// TestStopReleasesLeaseAfterExecutorExit P1：停止路径租约释放后于执行器退出
// ——执行器取消后仍滞留 120ms（收尾写工作区），释放不得先行。
func TestStopReleasesLeaseAfterExecutorExit(t *testing.T) {
	f := newFixture(t)
	log := &eventLog{}
	exec := &ignoreCancelExec{log: log, work: 120 * time.Millisecond, started: make(chan struct{})}
	f.svc.executor = exec
	f.leases.releaseLog = log
	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		done <- r
	}()
	<-exec.started
	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "paused"
	f.missions.mu.Unlock()
	f.svc.SignalStopByMission(context.Background(), "mi_a", "pause")
	select {
	case r := <-done:
		if r.State != StateCanceled {
			t.Fatalf("Run 须 canceled: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("停止后未返回")
	}
	seq := log.slice()
	// 序断言：exec_return 必须先于 release
	ri, xi := -1, -1
	for i, s := range seq {
		if s == "release" {
			ri = i
		}
		if s == "exec_return" {
			xi = i
		}
	}
	if xi < 0 || ri < 0 || ri < xi {
		t.Fatalf("租约释放须后于执行器退出: %v", seq)
	}
}

// failCancelStore 终态写 canceled 时失败一次（停止路径写库故障注入）。
type failCancelStore struct {
	Store
}

func (s failCancelStore) UpdateRun(ctx context.Context, r Run, events []Event) error {
	if r.State == StateCanceled {
		return errors.New("disk full")
	}
	return s.Store.UpdateRun(ctx, r, events)
}

// TestStopTerminalWriteErrorSurfaced P2：停止终态写库失败不得吞错——调用方
// 须见 ErrTerminalWrite（Run 滞留 running + 租约滞留 active 的事实对齐，
// 恢复面兜底收尾）。
func TestStopTerminalWriteErrorSurfaced(t *testing.T) {
	f := newFixture(t)
	f.svc.executor = newPauseExec()
	f.svc.store = failCancelStore{Store: f.svc.store}
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		if !errors.Is(err, ErrTerminalWrite) {
			t.Errorf("写库失败须上抛 ErrTerminalWrite: %+v %v", r, err)
		}
	}()
	waitForRunState(t, f.svc, "mi_a", StateRunning)
	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "paused"
	f.missions.mu.Unlock()
	f.svc.SignalStopByMission(ctx, "mi_a", "pause")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("停止后未返回")
	}
	if f.leases.released != 0 {
		t.Fatal("写库失败时不得释放租约（Run 未终态——恢复面统一收尾）")
	}
}

// TestRecoverCancelingWithoutUnfinished P2：Mission 滞 canceling 且无未终态
// Run（Run 已终态、进程在控制面收尾前退出）——恢复面须按状态枚举兜底兑现。
func TestRecoverCancelingWithoutUnfinished(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// 播种：Run 已 canceled 终态 + Mission 滞 canceling（收尾未兑现）
	seeded := Run{RunID: "run_term", MissionID: "mi_a", Attempt: 1,
		LeaseID: "lse_term", State: StateCanceled, RequestedAt: "t", UpdatedAt: "t"}
	if _, err := f.svc.store.CreateRun(ctx, seeded); err != nil {
		t.Fatalf("播种: %v", err)
	}
	f.missions.status["mi_a"] = "canceling"

	if _, err := f.svc.RecoverUnfinished(ctx); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if f.missions.finishCancelCalls != 1 {
		t.Fatalf("滞留 canceling 须兜底收尾（得到 %d 次）", f.missions.finishCancelCalls)
	}
	if f.missions.status["mi_a"] != "canceled" {
		t.Fatalf("Mission 须兑现 canceled: %v", f.missions.status)
	}
	// Run 事实不动（已终态——不重写）
	got, _, _ := f.svc.GetRun(ctx, "run_term")
	if got.UpdatedAt != "t" {
		t.Fatalf("已终态 Run 不得重写: %+v", got)
	}
}

// TestRecoverPausedWithoutUnfinished 对照：paused 且无未终态 Run——停 paused
// （合法长驻态，不收尾）。
func TestRecoverPausedWithoutUnfinished(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.missions.status["mi_a"] = "paused"
	if _, err := f.svc.RecoverUnfinished(ctx); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if f.missions.status["mi_a"] != "paused" {
		t.Fatalf("paused 须保持: %v", f.missions.status)
	}
	if f.missions.finishCancelCalls != 0 {
		t.Fatal("paused 不触发收尾")
	}
}
