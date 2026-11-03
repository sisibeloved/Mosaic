package run

// 停止与执行器取消同时就绪的 select 竞态（CI macOS 实证：run 41559933317——
// 0.6.6 暂停路径在途 Run 被记 failed"context canceled" 而非 canceled）：停止
// 取消 runCtx 后执行器即刻返回 ctx.Err()，done 与 runCtx.Done() 同就绪、select
// 随机选分支——无论选中哪个，停止意图在场时终态必须 canceled。

import (
	"context"
	"testing"
	"time"
)

// stopRacingExec 阻塞至执行上下文取消，随即返回 ctx.Err()（真实取消面）——
// 令 done 与 runCtx.Done() 在同一窗口就绪，命中竞态。
type stopRacingExec struct {
	started chan struct{}
}

func (e *stopRacingExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	close(e.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func TestStopBeatsExecutorCancelRace(t *testing.T) {
	for i := 0; i < 12; i++ {
		f := newFixture(t)
		exec := &stopRacingExec{started: make(chan struct{})}
		f.svc.executor = exec
		ctx := context.Background()
		done := make(chan Run, 1)
		go func() {
			r, _ := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
			done <- r
		}()
		waitForRunState(t, f.svc, "mi_a", StateRunning)

		// 控制编排先持久 paused（fake 对齐真实条件投影语义），再发停止信号。
		f.missions.mu.Lock()
		f.missions.status["mi_a"] = "paused"
		f.missions.mu.Unlock()
		if n, err := f.svc.SignalStopByMission(ctx, "mi_a", "pause"); err != nil || n != 1 {
			t.Fatalf("第 %d 次停止信号: n=%d err=%v", i, n, err)
		}
		select {
		case r := <-done:
			if r.State != StateCanceled {
				t.Fatalf("第 %d 次：停止意图在场时终态须 canceled（select 竞态不得记 failed）：state=%s err=%q", i, r.State, r.ErrorText)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次：停止后执行须返回", i)
		}
	}
}
