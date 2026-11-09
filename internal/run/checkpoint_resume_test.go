// 续办测试（0.7.4 / ADR-0041）：终态自动落盘（best-effort）+ Inline 透传。
package run

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCkptSaver struct {
	calls []string // runID|missionID|summary
	fail  bool
}

func (f *fakeCkptSaver) SaveTerminal(_ context.Context, runID, missionID, summary string) error {
	if f.fail {
		return errors.New("boom")
	}
	f.calls = append(f.calls, runID+"|"+missionID+"|"+summary)
	return nil
}

// TerminalCheckpointSummary 截断与格式。
func TestTerminalCheckpointSummary(t *testing.T) {
	s := TerminalCheckpointSummary("run_1", "completed", "一切正常")
	if len(s) > 500 || !strings.HasPrefix(s, "Run ") || !strings.Contains(s, "终态") {
		t.Fatalf("摘要格式错：%q", s)
	}
	// 超长截断。
	long := string(make([]rune, 900))
	if got := TerminalCheckpointSummary("run_1", "failed", long); len([]rune(got)) != 500 {
		t.Fatalf("应截 500，得 %d", len([]rune(got)))
	}
}

// 终态自动落盘：接线后 finalize 落一条；写失败不阻断终态（best-effort）。
func TestFinalizeSavesTerminalCheckpoint(t *testing.T) {
	ctx := context.Background()
	// 完成 + 落盘成功。
	saver := &fakeCkptSaver{}
	svc := newResumeService(t, saver)
	r, lease := newResumeRun(t, svc)
	if err := svc.finalizeRun(ctx, &r, lease, StateCompleted, "", "结果正文", ""); err != nil {
		t.Fatal(err)
	}
	if len(saver.calls) != 1 {
		t.Fatalf("应恰一条落盘，得 %d", len(saver.calls))
	}
	if !strings.HasPrefix(saver.calls[0], "run_r1|mi_r|") {
		t.Fatalf("锚错：%s", saver.calls[0])
	}
	// failed + 落盘失败——终态不受阻。
	saver2 := &fakeCkptSaver{fail: true}
	svc2 := newResumeService(t, saver2)
	r2, lease2 := newResumeRun(t, svc2)
	if err := svc2.finalizeRun(ctx, &r2, lease2, StateFailed, "执行错误", "", ""); err != nil {
		t.Fatalf("写失败不得阻断终态：%v", err)
	}
	if r2.State != StateFailed {
		t.Fatalf("终态应 failed：%s", r2.State)
	}
}

// 未接线（nil）不落盘不报错——既有装配兼容。
func TestFinalizeNilSaverNoop(t *testing.T) {
	svc := newResumeService(t, nil)
	r, lease := newResumeRun(t, svc)
	if err := svc.finalizeRun(context.Background(), &r, lease, StateCompleted, "", "ok", ""); err != nil {
		t.Fatal(err)
	}
}

// newResumeService 最小可 finalize 服务（budgetguard 同款端口组装）。
func newResumeService(t *testing.T, saver *fakeCkptSaver) *Service {
	t.Helper()
	missions := &fakeMissions{refs: map[string]MissionRef{}, status: map[string]string{}}
	missions.refs["mi_r"] = MissionRef{MissionID: "mi_r", OwnerAgentID: "ag_r"}
	opts := []Option{WithNow(func() string { return "2026-11-06T12:00:00Z" })}
	if saver != nil {
		opts = append(opts, WithCheckpointSaver(saver))
	}
	return New(NewMemStore(), missions,
		fakeBindings{active: map[string]BindingFacts{"ag_r": {BindingID: "bd_r"}}},
		&fakeRuntimes{ready: RuntimeRef{RuntimeID: "rtx_r"}},
		&fakeLeases{fenceOK: true},
		fakeManifests{state: "assembled"},
		&noopExec{},
		opts...)
}

// newResumeRun 直插一行 running Run + 有效 lease（不经 ExecuteMission——
// finalize 面单测）。
func newResumeRun(t *testing.T, svc *Service) (Run, Lease) {
	t.Helper()
	ctx := context.Background()
	r := Run{RunID: "run_r1", MissionID: "mi_r", State: StateRunning,
		ManifestID: "cmf_r", RuntimeID: "rtx_r", BindingID: "bd_r", AgentID: "ag_r",
		Attempt: 1, Generation: 1, RequestedAt: "2026-11-06T12:00:00Z"}
	if _, err := svc.store.CreateRun(ctx, r, 32, 0, ""); err != nil {
		t.Fatal(err)
	}
	lease, err := svc.leases.Acquire(ctx, "rtx_r", r.MissionID, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.LeaseID = lease.LeaseID
	return r, lease
}

// 一审 #3：失败路径（finalizeFailed）同样落系统摘要。
func TestFinalizeFailedSavesCheckpoint(t *testing.T) {
	saver := &fakeCkptSaver{}
	svc := newResumeService(t, saver)
	r, _ := newResumeRun(t, svc)
	if err := svc.finalizeFailed(context.Background(), &r, "执行错误", r.LeaseID); err != nil {
		t.Fatal(err)
	}
	if len(saver.calls) != 1 || !strings.Contains(saver.calls[0], "failed") {
		t.Fatalf("失败路径应落摘要：%v", saver.calls)
	}
}

// noopExec 空 Agent 执行器（finalize 单测不用到）。
type noopExec struct{}

func (*noopExec) Submit(context.Context, ExecutionInput) (string, error) { return "", nil }

// manifestInline checkpoint 层透传（resume_checkpoint 键）。
func TestManifestInlineCheckpointPassthrough(t *testing.T) {
	m := Manifest{
		ManifestID: "cmf_1",
		Layers: []ManifestLayer{
			{Name: "contract", Content: map[string]any{"objective": "o"}},
			{Name: "checkpoint", Content: map[string]any{
				"checkpoint_id": "ckp_9", "progress_summary": "过半",
				"contract_drift": true,
			}},
		},
	}
	inline := manifestInline(m)
	rc, ok := inline["resume_checkpoint"].(map[string]any)
	if !ok || rc["checkpoint_id"] != "ckp_9" || rc["contract_drift"] != true {
		t.Fatalf("resume_checkpoint 透传错：%v", inline["resume_checkpoint"])
	}
}
