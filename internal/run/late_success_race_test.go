package run

// 0.6.10 复审 #1/#2 + 四审 #4（ADR-0033 v0.4）：停止已登记后执行结果先被消费
// ——成功正文不得发布（completed/result_block），迟到结果只进 late_result 审计；
// 已取出的结果交还收尾——审计、释放恰一次，不空等 stopGrace。
//
// 确定性同步屏障（四审 #4：休眠构造在本地几乎总让停止分支先消费、对旧实现
// 不构成鉴别）：先登记停止意图——cause 非空、cancel 为 no-op（runCtx 不取消，
// select 的 Done 分支不可就绪）——再放行执行器结果；select **必**消费结果分支。
// 旧实现（结果分支不查停止意图）30/30 发布正文；新实现 30/30 按停止终理化。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// barrierExec 不看执行上下文——阻塞至测试放行后返回成功正文或错误。
type barrierExec struct {
	release chan struct{}
	fail    error
}

func (e *barrierExec) Submit(_ context.Context, _ ExecutionInput) (string, error) {
	<-e.release
	if e.fail != nil {
		return "", e.fail
	}
	return "迟到结果正文", nil
}

func stopRegisteredThenResult(t *testing.T, i int, fail error) {
	t.Helper()
	f := newFixture(t)
	f.svc.stopGrace = 5 * time.Second // 复审 #2：收尾若空等通道会显性超时
	exec := &barrierExec{release: make(chan struct{}), fail: fail}
	f.svc.executor = exec
	ctx := context.Background()
	done := make(chan Run, 1)
	go func() {
		r, _ := f.svc.ExecuteMission(ctx, MissionRef{MissionID: "mi_a", OwnerAgentID: "ag_x"})
		done <- r
	}()
	running := waitForRunState(t, f.svc, "mi_a", StateRunning)

	// 控制编排先持久 paused（fake 对齐真实条件投影语义）。
	f.missions.mu.Lock()
	f.missions.status["mi_a"] = "paused"
	f.missions.mu.Unlock()
	// 同步屏障：先登记停止意图（cause 非空；cancel 换成 no-op——runCtx 不取消），
	// 再放行执行结果——select 只能消费结果分支。
	f.svc.stops.mu.Lock()
	ent, ok := f.svc.stops.m[running.RunID]
	if !ok {
		f.svc.stops.mu.Unlock()
		t.Fatalf("第 %d 次：Run 未注册停止表", i)
	}
	ent.cause = "pause"
	ent.cancel = func() {}
	f.svc.stops.m[running.RunID] = ent
	f.svc.stops.mu.Unlock()
	begin := time.Now()
	close(exec.release)

	var r Run
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("第 %d 次：执行须返回", i)
	}
	if r.State != StateCanceled {
		t.Fatalf("第 %d 次：停止在场时结果不得发布：state=%s block=%q err=%q", i, r.State, r.ResultBlock, r.ErrorText)
	}
	if r.ResultBlock != "" {
		t.Fatalf("第 %d 次：迟到正文只审计不发布：block=%q", i, r.ResultBlock)
	}
	// 事件：stop_requested / canceled / late_result 各恰一次；审计携结果形态。
	evs, _, _ := f.svc.RunEventsByRun(ctx, r.RunID)
	counts := map[string]int{}
	var late string
	for _, e := range evs {
		counts[e.EventType]++
		if e.EventType == EventLateResult {
			raw, _ := json.Marshal(e.Payload)
			late = string(raw)
		}
	}
	if counts[EventStopRequested] != 1 || counts[EventCanceled] != 1 || counts[EventLateResult] != 1 {
		t.Fatalf("第 %d 次：事件计数 stop=%d canceled=%d late=%d", i, counts[EventStopRequested], counts[EventCanceled], counts[EventLateResult])
	}
	wantOutcome := `"outcome":"ok"`
	wantPreview := "迟到结果正文"
	if fail != nil {
		wantOutcome, wantPreview = `"outcome":"error"`, fail.Error()
	}
	if !strings.Contains(late, wantOutcome) || !strings.Contains(late, wantPreview) || !strings.Contains(late, `"published":false`) {
		t.Fatalf("第 %d 次：迟到审计载荷 %s", i, late)
	}
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

func TestStopRegisteredBeforeLateSuccess(t *testing.T) {
	for i := 0; i < 30; i++ {
		stopRegisteredThenResult(t, i, nil)
	}
}

func TestStopRegisteredBeforeLateError(t *testing.T) {
	for i := 0; i < 30; i++ {
		stopRegisteredThenResult(t, i, errors.New("适配器迟到失败"))
	}
}
