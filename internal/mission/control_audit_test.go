package mission

// 0.6.6 审校复现（P1 并发修订覆盖 / P2 恢复漏完备性重判）：
//
//   - P1：Revise 服务层先读后写，读得 ready 后暂停/取消落地（不递增
//     contract_version），存储面只 CAS 版本即放行——next.Status=ready 把
//     paused/canceling 盖回 ready，Mission 重新进入调度、停止意图丢失。
//     存储面须在事务内保持执行控制状态（服务层读到什么是非权威的）。
//   - P2：暂停期间修订删除验收条件（控制投影保持 paused），Resume 无条件
//     paused → ready——不完备契约直接入调度列；恢复须按当前契约重判完备性
//     （ready | draft）。

import (
	"context"
	"testing"
)

// TestReviseStaleReadPreservesControlStatus 存储面契约（P1 根因）：服务层
// 携带的 next.Status 是旧读快照（此处直接构造 Status=ready 的陈旧 next，
// 等价于"读到 ready 后暂停落地"的竞态窗口）——事务内现行状态为
// running/paused/canceling 时，落库状态必须保持现行控制状态。
func TestReviseStaleReadPreservesControlStatus(t *testing.T) {
	for _, st := range []string{StatusRunning, StatusPaused, StatusCanceling} {
		svc, mem := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
		ctx := context.Background()
		m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
		// 播种控制状态（经正规转移面；版本不递增）
		seed := map[string]func(){
			StatusRunning:   func() { _, _ = svc.MarkRunning(ctx, m.MissionID) },
			StatusPaused:    func() { _, _ = svc.Pause(ctx, m.MissionID, 1, "") },
			StatusCanceling: func() { _, _ = svc.MarkCanceling(ctx, m.MissionID, 1, "") },
		}
		seed[st]()
		if got, _, _ := svc.Get(ctx, m.MissionID); got.Status != st {
			t.Fatalf("%s 播种失败: %s", st, got.Status)
		}

		// 陈旧 next：服务层从 ready 读快照计算（statusOf → ready），版本
		// 递增合法（暂停/取消不动版本）——CAS 版本面对这种交错是放行的。
		next := m
		next.ContractVersion = 2
		next.Status = StatusReady // 陈旧投影——不得覆盖现行控制状态
		out, ok, err := mem.ReviseMission(ctx, m.MissionID, 1, next)
		if err != nil || !ok {
			t.Fatalf("%s 修订应落库: ok=%v err=%v", st, ok, err)
		}
		if out.Status != st {
			t.Fatalf("%s：陈旧修订投影不得覆盖控制状态（得到 %s）", st, out.Status)
		}
		if got, _, _ := svc.Get(ctx, m.MissionID); got.Status != st {
			t.Fatalf("%s：库面状态须保持（得到 %s）", st, got.Status)
		}
	}
}

// TestResumeRejudgesCompleteness 恢复重判完备性（P2）：暂停期间修订删除
// 验收条件（paused 保持），Resume 不得直通 ready——须落 draft（完备性
// 重判），补齐验收后再 Resume 才回 ready。
func TestResumeRejudgesCompleteness(t *testing.T) {
	svc, _ := newSvc(t, fakeAgents{ids: map[string]bool{"ag_a": true}})
	ctx := context.Background()
	m, _ := svc.Create(ctx, "ag_a", fullContent(), nil)
	if _, err := svc.Pause(ctx, m.MissionID, 1, "暂停修订"); err != nil {
		t.Fatalf("暂停: %v", err)
	}
	dropped := fullContent()
	dropped.AcceptanceCriteria = nil // 修订删除验收条件 → 不完备
	if _, err := svc.Revise(ctx, m.MissionID, 1, "ag_a", dropped); err != nil {
		t.Fatalf("修订: %v", err)
	}
	if got, _, _ := svc.Get(ctx, m.MissionID); got.Status != StatusPaused {
		t.Fatalf("修订须保持 paused（得到 %s）", got.Status)
	}

	out, err := svc.Resume(ctx, m.MissionID, 2)
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if out.Status != StatusDraft {
		t.Fatalf("不完备契约恢复须落 draft（得到 %s）——ready 会把缺验收的契约直接送进调度", out.Status)
	}
	// draft 不入调度列（ListReady 只取 ready）
	if changed, _ := svc.MarkRunning(ctx, m.MissionID); changed {
		t.Fatal("draft 不可被调度")
	}

	// 补齐验收（draft 修订回完备）：draft 无暂停面，修订本身即回 ready
	// （完备性重判双向；Resume 不递增版本——期望 v2）。
	restored := fullContent()
	if _, err := svc.Revise(ctx, m.MissionID, 2, "ag_a", restored); err != nil {
		t.Fatalf("补齐修订: %v", err)
	}
	if got, _, _ := svc.Get(ctx, m.MissionID); got.Status != StatusReady {
		t.Fatalf("补齐后须回 ready（得到 %s）", got.Status)
	}
}
