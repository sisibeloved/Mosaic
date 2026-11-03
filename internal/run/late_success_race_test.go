package run

// 0.6.10 复审 #1/#2（ADR-0033 v0.3）：停止与执行结果同时就绪的 select 竞态——
// 成功正文不得发布（completed/result_block），迟到正文只进 late_result 审计；
// 已取出的结果交还收尾（放回缓冲通道）——审计、释放恰一次，不空等 stopGrace。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// lateSuccessExec 停止取消后仍"跑完"返回正文（真实 CLI 迟到面）——done 与
// runCtx.Done() 同就绪，命中竞态窗口。
type lateSuccessExec struct {
	started chan struct{}
}

func (e *lateSuccessExec) Submit(ctx context.Context, _ ExecutionInput) (string, error) {
	close(e.started)
	<-ctx.Done()
	time.Sleep(time.Millisecond)
	return "迟到结果正文", nil
}

func TestStopBeatsLateSuccessRace(t *testing.T) {
	for i := 0; i < 12; i++ {
		begin := time.Now()
		f := newFixture(t)
		f.svc.stopGrace = 5 * time.Second // 复审 #2：若收尾空等通道会显性超时
		exec := &lateSuccessExec{started: make(chan struct{})}
		f.svc.executor = exec
		ctx := context.Background()
		done := make(chan Run, 1)
		go func() {
			r, _ := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
			done <- r
		}()
		waitForRunState(t, f.svc, "mi_a", StateRunning)

		f.missions.mu.Lock()
		f.missions.status["mi_a"] = "paused"
		f.missions.mu.Unlock()
		if n, err := f.svc.SignalStopByMission(ctx, "mi_a", "pause"); err != nil || n != 1 {
			t.Fatalf("第 %d 次停止信号: n=%d err=%v", i, n, err)
		}
		var r Run
		select {
		case r = <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次：停止后执行须返回", i)
		}
		// 复审 #1：停止在场——不得 completed、迟到正文不得入 result_block。
		if r.State != StateCanceled {
			t.Fatalf("第 %d 次：停止在场时成功正文不得发布：state=%s block=%q", i, r.State, r.ResultBlock)
		}
		if r.ResultBlock != "" {
			t.Fatalf("第 %d 次：迟到正文只审计不发布：block=%q", i, r.ResultBlock)
		}
		// 迟到正文只进 late_result 审计（outcome=ok、preview 携正文、published=false）。
		evs, _, _ := f.svc.RunEventsByRun(ctx, r.RunID)
		var late string
		hasStop, hasCanceled := false, false
		for _, e := range evs {
			switch e.EventType {
			case EventStopRequested:
				hasStop = true
			case EventCanceled:
				hasCanceled = true
			case EventLateResult:
				raw, _ := json.Marshal(e.Payload)
				late = string(raw)
			}
		}
		if !hasStop || !hasCanceled || late == "" ||
			!strings.Contains(late, "迟到结果正文") || !strings.Contains(late, `"published":false`) {
			t.Fatalf("第 %d 次：审计面不完整：stop=%v canceled=%v late=%s", i, hasStop, hasCanceled, late)
		}
		// 复审 #2：结果交还收尾——释放恰一次且不空等（stopGrace=5s，须远早于它）。
		f.leases.mu.Lock()
		rel := f.leases.released
		f.leases.mu.Unlock()
		if rel != 1 {
			t.Fatalf("第 %d 次：租约须恰释放一次：%d", i, rel)
		}
		if el := time.Since(begin); el > 2*time.Second {
			t.Fatalf("第 %d 次：收尾不得空等 stopGrace（耗时 %v）", i, el)
		}
	}
}
