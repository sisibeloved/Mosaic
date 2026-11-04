package run

// 0.6.9 审校修复（ADR-0032 v0.2 决策延伸）：#2 恢复面——Run 落库后、Mission 投影
// running 前崩溃（残留 ready）也进 waiting；accepted/canceled 终态封处置入口。
// 测试先行。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestRecoverReadyResidueToWaiting 审校 #2：ready Mission + 未终态 Run（落库与
// 投影两步间隙崩溃）→ 恢复置 unknown + Mission 进 waiting（不再滞留 ready）。
func TestRecoverReadyResidueToWaiting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seeded := Run{RunID: "run_seed", MissionID: "mi_a", Attempt: 1,
		LeaseID: "lse_seed", State: StateRunning, RequestedAt: "t", UpdatedAt: "t"}
	if _, err := f.svc.store.CreateRun(ctx, seeded, 0, 0, ""); err != nil {
		t.Fatalf("播种: %v", err)
	}
	f.missions.status["mi_a"] = "ready" // 投影未发生（残留 ready）

	if _, err := f.svc.RecoverUnfinished(ctx); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	got, _, _ := f.svc.GetRun(ctx, "run_seed")
	if got.State != StateUnknown {
		t.Fatalf("残留 Run 须 unknown: %+v", got)
	}
	if f.missions.status["mi_a"] != "waiting" || f.missions.waitingReason["mi_a"] != "reconciliation" {
		t.Fatalf("ready 残留也须进 waiting: %v / %v", f.missions.status, f.missions.waitingReason)
	}
}

// TestResolveRunTerminalMission 审校 #2：Mission 终态（accepted / canceled）封
// 处置入口——不可在验收完成后改写处置事实。
func TestResolveRunTerminalMission(t *testing.T) {
	for _, st := range []string{"accepted", "canceled"} {
		f := newFixture(t)
		ctx := context.Background()
		seeded := Run{RunID: "run_seed", MissionID: "mi_a", Attempt: 1,
			LeaseID: "lse_seed", State: StateRunning, RequestedAt: "t", UpdatedAt: "t"}
		f.svc.store.CreateRun(ctx, seeded, 0, 0, "")
		f.missions.status["mi_a"] = "running"
		f.svc.RecoverUnfinished(ctx) // → unknown + waiting
		f.missions.mu.Lock()
		f.missions.status["mi_a"] = st
		f.missions.mu.Unlock()

		if _, err := f.svc.ResolveRun(ctx, "run_seed", ResolutionDiscarded, ""); !errors.Is(err, ErrMissionTerminal) {
			t.Fatalf("%s 终态须拒处置: %v", st, err)
		}
		got, _, _ := f.svc.GetRun(ctx, "run_seed")
		if got.Resolution != "" || !strings.Contains(got.ErrorText, "结果未知") {
			t.Fatalf("%s 处置不得落: %+v", st, got)
		}
	}
}
