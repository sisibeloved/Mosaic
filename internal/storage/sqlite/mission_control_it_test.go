//go:build it

// IT 层（ADR-0029 / 0.6.6）：控制状态机经真实 SQLite——Pause/Resume CAS、
// MarkCanceling/FinishCancel 两段、事件留痕、与执行投影（TransitionStatus）
// 的互斥、Run canceled 终态出未终态口径。测试先行。

package sqlite

import (
	"context"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

// TestMissionControlStoreIT 暂停/恢复 CAS + 两段取消 + 事件链。
func TestMissionControlStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	m, err := store.CreateMission(ctx, missionRow(mission.StatusReady, "ag_it"), nil)
	if err != nil {
		t.Fatalf("建契约: %v", err)
	}

	// Pause：版本不匹配 ok=false；正确 → paused + 事件
	if _, ok, _ := store.PauseMission(ctx, m.MissionID, 9, "", "t1"); ok {
		t.Fatal("版本不匹配暂停应 ok=false")
	}
	paused, ok, err := store.PauseMission(ctx, m.MissionID, 1, "等依赖", "t1")
	if err != nil || !ok || paused.Status != mission.StatusPaused {
		t.Fatalf("暂停: ok=%v err=%v status=%s", ok, err, paused.Status)
	}
	// paused 上重复暂停（版本匹配）ok=false（幂等由服务层先读实现——存储面
	// 只认 ready/running）
	if _, ok, _ := store.PauseMission(ctx, m.MissionID, 1, "", "t2"); ok {
		t.Fatal("存储面 paused 不可再暂停（服务层幂等先读）")
	}
	// Resume：非 paused ok=false；paused → ready
	if _, ok, _ := store.ResumeMission(ctx, m.MissionID, 9, "t3"); ok {
		t.Fatal("版本不匹配恢复应 ok=false")
	}
	resumed, ok, err := store.ResumeMission(ctx, m.MissionID, 1, "t3")
	if err != nil || !ok || resumed.Status != mission.StatusReady {
		t.Fatalf("恢复: ok=%v err=%v status=%s", ok, err, resumed.Status)
	}

	// running → paused（在途暂停入口）+ TransitionStatus 互斥：paused 上
	// running 投影无操作（changed=false）
	if _, ok, _ := store.PauseMission(ctx, m.MissionID, 1, "", "t4"); !ok {
		t.Fatal("再停（ready → paused）")
	}
	ev := mission.Event{MissionID: m.MissionID, EventType: mission.EventStatusChanged,
		Payload: []byte(`{}`), OccurredAt: "t4"}
	if changed, err := store.TransitionStatus(ctx, m.MissionID, mission.StatusReady, mission.StatusRunning, ev); err != nil || changed {
		t.Fatalf("paused 不可被调度投影: changed=%v err=%v", changed, err)
	}
	if changed, err := store.TransitionStatus(ctx, m.MissionID, mission.StatusPaused, mission.StatusReady, ev); err != nil || !changed {
		t.Fatalf("paused → ready 投影应可用（resume 存储）: changed=%v err=%v", changed, err)
	}

	// 两段取消：MarkCanceling（版本 CAS）→ FinishCancel（canceling → canceled）
	if _, ok, _ := store.MarkCanceling(ctx, m.MissionID, 9, "", "t5"); ok {
		t.Fatal("版本不匹配停止意图应 ok=false")
	}
	cing, ok, err := store.MarkCanceling(ctx, m.MissionID, 1, "计划变更", "t5")
	if err != nil || !ok || cing.Status != mission.StatusCanceling {
		t.Fatalf("停止意图: ok=%v err=%v status=%s", ok, err, cing.Status)
	}
	// canceling 上修订 CAS 仍可用（投影保持由服务层保证——存储面版本行照落）
	next := m
	next.ContractVersion = 2
	next.Status = mission.StatusCanceling
	next.UpdatedAt = "t5b"
	if _, ok, _ := store.ReviseMission(ctx, m.MissionID, 1, next); !ok {
		t.Fatal("canceling 修订应可用（服务层保持投影）")
	}
	done, ok, err := store.FinishCancel(ctx, m.MissionID, "计划变更", "t6")
	if err != nil || !ok || done.Status != mission.StatusCanceled {
		t.Fatalf("收尾: ok=%v err=%v status=%s", ok, err, done.Status)
	}
	if _, ok, _ := store.FinishCancel(ctx, m.MissionID, "", "t7"); ok {
		t.Fatal("canceled 上重复收尾应 ok=false（幂等由服务层先读）")
	}
	if _, ok, _ := store.MarkCanceling(ctx, m.MissionID, 2, "", "t8"); ok {
		t.Fatal("终态停止意图应 ok=false")
	}
	// 事件链：created + paused + ready(resume) + paused + ready(transition) +
	// canceling + contract_revised + canceled
	evs, _, _ := store.MissionEvents(ctx, m.MissionID)
	var tos []string
	for _, e := range evs {
		if e.EventType == mission.EventStatusChanged {
			tos = append(tos, string(e.Payload))
		}
	}
	if len(tos) != 6 {
		t.Fatalf("status_changed 应 6 条（pause/resume/pause/transition-resume/canceling/canceled）: %v", tos)
	}

	// draft 不可暂停（存储面只认 ready/running）
	dRow := missionRow(mission.StatusDraft, "")
	dRow.MissionID = "mi_it_draft"
	d, err := store.CreateMission(ctx, dRow, nil)
	if err != nil {
		t.Fatalf("建 draft: %v", err)
	}
	if _, ok, _ := store.PauseMission(ctx, d.MissionID, 1, "", "t9"); ok {
		t.Fatal("draft 不可暂停")
	}
	// draft 可两段取消（非终态皆可）
	if _, ok, _ := store.MarkCanceling(ctx, d.MissionID, 1, "", "t9"); !ok {
		t.Fatal("draft 可发停止意图")
	}
	if _, ok, _ := store.FinishCancel(ctx, d.MissionID, "", "t10"); !ok {
		t.Fatal("draft 可收尾")
	}
}

// TestRunCanceledTerminalIT Run canceled 终态出未终态口径：canceled 后可
// 再建（attempt+1），ListUnfinished 不含 canceled。
func TestRunCanceledTerminalIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	r, err := store.CreateRun(ctx, seedRun("run_c1", "mi_it2", 0, missionrun.StateRunning))
	if err != nil {
		t.Fatalf("首建: %v", err)
	}
	r.State = missionrun.StateCanceled
	r.ErrorText = "停止请求在途"
	r.FinishedAt = "2026-10-31T10:01:00.000000000Z"
	r.UpdatedAt = "2026-10-31T10:01:00.000000000Z"
	if err := store.UpdateRun(ctx, r, []missionrun.Event{{
		RunID: r.RunID, EventType: missionrun.EventStopRequested, OccurredAt: "2026-10-31T10:01:00.000000000Z",
		Payload: map[string]any{"cause": "pause"},
	}, {
		RunID: r.RunID, EventType: missionrun.EventCanceled, OccurredAt: "2026-10-31T10:01:00.000000000Z",
		Payload: map[string]any{"cause": "pause"},
	}}); err != nil {
		t.Fatalf("canceled 终态: %v", err)
	}
	if unfinished, _ := store.ListUnfinished(ctx); len(unfinished) != 0 {
		t.Fatalf("canceled 不属未终态: %+v", unfinished)
	}
	// canceled 后可再建（互斥只锁 pending/running）
	r2, err := store.CreateRun(ctx, seedRun("run_c2", "mi_it2", 0, missionrun.StateRunning))
	if err != nil || r2.Attempt != 2 {
		t.Fatalf("canceled 后再建: %+v %v", r2, err)
	}
}
