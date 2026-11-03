package app

// 0.6.12 退役承诺处置（ADR-0035 §4）：missionCommitments 适配器——非终态
// owned Mission = 未处置承诺（accepted/canceled 不计；在途交接仍计入——
// owner 未翻转、回执未落）。测试先行（红→绿）。

import (
	"context"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// anyAgents 测试桩：任意 id 存在且 active。
type anyAgents struct{}

func (anyAgents) AgentExists(context.Context, string) (bool, error) { return true, nil }
func (anyAgents) AgentStatus(context.Context, string) (string, bool, error) {
	return mission.AgentStatusActive, true, nil
}

func TestMissionCommitmentsOutstanding(t *testing.T) {
	mem := mission.NewMemStore()
	ctx := context.Background()
	full := mission.Content{
		Objective: "目标", Scope: "范围",
		AcceptanceCriteria: []string{"验收"}, DeliverableSpecs: []string{"交付"}, SourceRefs: []string{"src:1"},
	}
	mk := func(owner, obj string) mission.Mission {
		m, err := mission.New(mem, mission.WithNewMissionID(func() string { return "mi_" + obj }),
			mission.WithAgentLookup(anyAgents{})).Create(ctx, owner, func() mission.Content { c := full; c.Objective = obj; return c }(), nil)
		if err != nil {
			t.Fatalf("建 %s: %v", obj, err)
		}
		return m
	}
	owned := mk("ag_a", "在途任务")
	_ = mk("ag_b", "他人任务")
	term := mk("ag_a", "已验收任务")
	// term → accepted：送验 + 裁定
	svc := mission.New(mem, mission.WithNewMissionID(func() string { return "mi_x" }), mission.WithAgentLookup(anyAgents{}))
	if _, err := svc.RequestReview(ctx, term.MissionID, 1, "dlv_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordReview(ctx, term.MissionID, 1, 1, mission.ReviewDecision{Decision: mission.ReviewAccepted}); err != nil {
		t.Fatal(err)
	}

	c := missionCommitments{missions: mem}
	out, err := c.Outstanding(ctx, "ag_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("ag_a 未处置承诺须恰 1 项（在途），得 %d: %v", len(out), out)
	}
	if !strings.HasPrefix(out[0], owned.MissionID+"（ready）") {
		t.Fatalf("展示串须为 mission_id（status）objective: %q", out[0])
	}

	// 在途交接（pending）仍计入：owner 未翻转
	of, err := svc.HandoverOffer(ctx, owned.MissionID, 1, "ag_b", "")
	if err != nil || of.PendingOfferID == "" {
		t.Fatal(err)
	}
	out, _ = c.Outstanding(ctx, "ag_a")
	if len(out) != 1 {
		t.Fatalf("在途交接仍计入（回执未落）: %v", out)
	}
	// 承接落地 → 承诺转移
	if _, err := svc.HandoverAccept(ctx, owned.MissionID, 1, of.PendingOfferID, ""); err != nil {
		t.Fatal(err)
	}
	if out, _ = c.Outstanding(ctx, "ag_a"); len(out) != 0 {
		t.Fatalf("承接后 ag_a 清空: %v", out)
	}
	if out, _ = c.Outstanding(ctx, "ag_b"); len(out) != 2 {
		t.Fatalf("ag_b 接手 2 项（在途 + 他人任务）: %v", out)
	}
	// 无人认领的不入任何清单
	if out, _ := c.Outstanding(ctx, "ag_none"); len(out) != 0 {
		t.Fatalf("无主不入清单: %v", out)
	}
}
