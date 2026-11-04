package run

// 0.6.7 发布前补齐审校（对远端基线 6107d83 的四项缺口，语义在其 ADR-0030
// 之内）：
//
//   - 停止超窗租约（0.6.6 审校遗留 P1）：finalizeStopped 宽限超窗后仍无条件
//     释放租约——执行器未退出时新执行可同时写工作区
//   - 处置投影吞错：ResolveRun 的 ClearWaiting 失败被吞——Mission 卡
//     waiting 无感知，且同结论幂等重试不再补投影（双死角）
//   - 字节截断：truncateS 按字节切中文产出无效 UTF-8 且提前丢内容
//   - 旧库升级残留：v0.6.5/0.6.6 的"failed+结果未知"终态行升级后无人认领——
//     Mission 卡 running 无出口；ready 形态更被调度器自动重跑（禁盲目重跑洞）

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- 停止超窗租约 ----

// TestStopGraceExpiryHoldsLease：grace 超窗执行器未退出 → 租约不得释放
// （独占性优先；执行器退出、迟到审计完成后补释放；跨进程由 TTL 兜底）。
func TestStopGraceExpiryHoldsLease(t *testing.T) {
	f := newFixture(t)
	log := &eventLog{}
	exec := &ignoreCancelExec{log: log, work: 200 * time.Millisecond, started: make(chan struct{})}
	f.svc.executor = exec
	f.svc.stopGrace = 30 * time.Millisecond
	f.leases.releaseLog = log
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.svc.ExecuteMission(context.Background(), MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
	}()
	<-exec.started
	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "paused"
	f.missions.mu.Unlock()
	f.svc.SignalStopByMission(context.Background(), "mi_a", "pause")
	<-done
	if f.leases.released != 0 {
		t.Fatalf("grace 超窗执行器未退出时不得释放租约（releases=%d）——新执行可同写工作区", f.leases.released)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.leases.mu.Lock()
		n := f.leases.released
		f.leases.mu.Unlock()
		if n >= 1 {
			return // 执行器退出（含迟到审计）后补释放
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("执行器退出后须补释放租约")
}

// ---- 处置投影吞错 ----

type failClearWaiting struct {
	*fakeMissions
	calls     atomic.Int32
	failFirst atomic.Bool
}

func (m *failClearWaiting) ClearWaiting(ctx context.Context, id string) error {
	if m.failFirst.CompareAndSwap(true, false) {
		return errors.New("db: disk I/O error")
	}
	m.calls.Add(1)
	return m.fakeMissions.ClearWaiting(ctx, id)
}

// TestResolveRunProjectionErrorSurfaced：处置落库成功但 waiting 清除失败 →
// 调用方必须见错误（事实说明：处置已落库、Mission 卡 waiting）；同结论幂等
// 重试须补投影（不能因幂等短路而永不清 waiting）。
func TestResolveRunProjectionErrorSurfaced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seedRemoteUnknown(t, f, "mi_a", "run_pe")
	wrapped := &failClearWaiting{fakeMissions: f.missions}
	wrapped.failFirst.Store(true)
	f.svc.missions = wrapped

	if _, err := f.svc.ResolveRun(ctx, "run_pe", ResolutionVerifiedFailed, "核验"); err == nil || !strings.Contains(err.Error(), "投影失败") {
		t.Fatalf("投影失败必须上抛（Mission 卡 waiting 无出口）: %v", err)
	}
	// 同结论幂等重试：补投影（ClearWaiting 再次被调）并成功
	if _, err := f.svc.ResolveRun(ctx, "run_pe", ResolutionVerifiedFailed, "核验"); err != nil {
		t.Fatalf("幂等重试: %v", err)
	}
	if wrapped.calls.Load() != 1 {
		t.Fatalf("幂等重试须补投影（ClearWaiting 调用数 %d）", wrapped.calls.Load())
	}
	if f.missions.status["mi_a"] != "ready" {
		t.Fatalf("补投影后 Mission 须回 ready: %v", f.missions.status)
	}
}

// ---- rune 截断 ----

// TestTruncateSRunes：上限按 rune 计——中文不得产出无效 UTF-8、不得提前
// 丢内容（8000 字节 ≈ 2666 字）。
func TestTruncateSRunes(t *testing.T) {
	got := truncateS(strings.Repeat("汉", 9000), 8000)
	if strings.ToValidUTF8(got, "") != got {
		t.Fatal("按字节截断产出无效 UTF-8")
	}
	if len([]rune(got)) != 8000 {
		t.Fatalf("rune 截断须对齐上限（得到 %d 字）", len([]rune(got)))
	}
	if truncateS("ascii-only", 4) != "asci" {
		t.Fatal("ASCII 快路径不变")
	}
}

// ---- 旧库升级残留 ----

// seedLegacy067 播种 v0.6.5/0.6.6 升级形态：failed 终态 + "结果未知"文案
// （0.6.5/0.6.6 恢复面所写）+ Mission 处于给定状态。
func seedLegacy067(t *testing.T, f fixture, mi, runID, missionStatus string) {
	t.Helper()
	ctx := context.Background()
	r := Run{RunID: runID, MissionID: mi, Attempt: 1, LeaseID: "lse_" + runID,
		State: StateRunning, RequestedAt: "t0", UpdatedAt: "t0"}
	if _, err := f.svc.store.CreateRun(ctx, r, 0, 0); err != nil {
		t.Fatalf("播种: %v", err)
	}
	r.State = StateFailed
	r.ErrorText = "进程重启时在执行——结果未知；请确认后手动再执行（不会自动重跑）"
	r.FinishedAt = "t1"
	r.UpdatedAt = "t1"
	if err := f.svc.store.UpdateRun(ctx, r, nil); err != nil {
		t.Fatalf("legacy: %v", err)
	}
	f.missions.status[mi] = missionStatus
}

// TestLegacyResidueConverged：旧库残留升级后恢复面必须认领——行转 unknown
// （与崩溃新语义一致，可处置）+ Mission 进 waiting（running/ready 两形态都
// 不得自动重跑；ready 形态此前被调度器直接重跑——禁盲目重跑洞）。
func TestLegacyResidueConverged(t *testing.T) {
	for _, tc := range []struct{ missionStatus string }{
		{"running"}, // 0.6.5/0.6.6 恢复后的常态：Mission 卡 running 无出口
		{"ready"},   // 升级前经暂停→恢复绕道：调度器会自动重跑（洞）
	} {
		f := newFixture(t)
		ctx := context.Background()
		seedLegacy067(t, f, "mi_a", "run_leg", tc.missionStatus)
		if _, err := f.svc.RecoverUnfinished(ctx); err != nil {
			t.Fatalf("恢复: %v", err)
		}
		got, _, _ := f.svc.GetRun(ctx, "run_leg")
		if got.State != StateUnknown {
			t.Fatalf("[%s] 旧库残留行须转 unknown（得到 %s）——否则处置面拒收（只认 unknown）", tc.missionStatus, got.State)
		}
		if tc.missionStatus == "running" {
			if f.missions.status["mi_a"] != "waiting" {
				t.Fatalf("[running] Mission 须进 waiting（得到 %s）——出调度列等处置", f.missions.status["mi_a"])
			}
		} else {
			// 0.6.9 审校 #2：ready 残留形态同样进 waiting（与 running 形态一致——
			// 送验面前置拦截 + 单一形态；执行面围栏 residue_unresolved 仍留作兜底）
			if f.missions.status["mi_a"] != "waiting" {
				t.Fatalf("[ready] Mission 须进 waiting（得到 %s）", f.missions.status["mi_a"])
			}
			// waiting 形态：执行被拒（SetRunning 条件不符——Run 置 failed 不启动）
			r, err := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
			if err != nil || r.State != StateFailed || f.exec.calls != 0 {
				t.Fatalf("[ready] 未处置残留上执行必须被拒（err=%v state=%s calls=%d）", err, r.State, f.exec.calls)
			}
		}
		// 处置出口打通：unknown → verified_completed → 调度放行
		if _, err := f.svc.ResolveRun(ctx, "run_leg", ResolutionVerifiedCompleted, "升级后核验"); err != nil {
			t.Fatalf("[%s] 处置: %v", tc.missionStatus, err)
		}
		if f.missions.status["mi_a"] != "ready" {
			t.Fatalf("[%s] 处置后 Mission 须在调度列（ready）（得到 %s）", tc.missionStatus, f.missions.status["mi_a"])
		}
	}
}

// seedRemoteUnknown 播种远端语义的 unknown 残留（Run unknown + Mission waiting）。
func seedRemoteUnknown(t *testing.T, f fixture, mi, runID string) {
	t.Helper()
	ctx := context.Background()
	r := Run{RunID: runID, MissionID: mi, Attempt: 1, LeaseID: "lse_" + runID,
		State: StateRunning, RequestedAt: "t0", UpdatedAt: "t0"}
	if _, err := f.svc.store.CreateRun(ctx, r, 0, 0); err != nil {
		t.Fatalf("播种: %v", err)
	}
	r.State = StateUnknown
	r.ErrorText = "进程重启时在执行——结果未知"
	if err := f.svc.store.UpdateRun(ctx, r, nil); err != nil {
		t.Fatalf("unknown: %v", err)
	}
	f.missions.status[mi] = "waiting"
}
