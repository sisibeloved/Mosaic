// Package review 送验/裁定编排（0.6.9 / ADR-0032）：以交付包为锚的必需交付项
// 检查（AT-10）+ mission 域验收状态机委托。测试先行。
package review

import (
	"context"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

type stubAgents struct{}

func (stubAgents) AgentExists(context.Context, string) (bool, error) { return true, nil }
func (stubAgents) AgentStatus(context.Context, string) (string, bool, error) {
	return mission.AgentStatusActive, true, nil
}

type fakePackages struct{ facts map[string]PackageFacts }

func (f fakePackages) PackageFacts(_ context.Context, id string) (PackageFacts, bool, error) {
	p, ok := f.facts[id]
	return p, ok, nil
}

func newSvc(t *testing.T) (*Service, *mission.Service, *fakePackages) {
	t.Helper()
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-11-02T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_r" }),
		mission.WithAgentLookup(stubAgents{}))
	pk := &fakePackages{facts: map[string]PackageFacts{}}
	return New(mSvc, pk, nil), mSvc, pk
}

func readyMission(t *testing.T, mSvc *mission.Service, reviewer string) mission.Mission {
	t.Helper()
	m, err := mSvc.Create(context.Background(), "ag_a", mission.Content{
		Objective: "验收测试", Scope: "s",
		AcceptanceCriteria: []string{"七端点全绿", "CAS 冲突可见"},
		DeliverableSpecs:   []string{"测试报告"},
		SourceRefs:         []string{"r:1"},
		Reviewer:           reviewer,
	}, nil)
	if err != nil || m.Status != mission.StatusReady {
		t.Fatalf("建 ready: %+v %v", m, err)
	}
	return m
}

func pkg(id, missionID string, version int, refs []string, mapping map[string]string) PackageFacts {
	var entries []MappingEntry
	for c, e := range mapping {
		entries = append(entries, MappingEntry{Criterion: c, Evidence: e})
	}
	return PackageFacts{PackageID: id, MissionID: missionID, ContractVersion: version, ArtifactRefs: refs, AcceptanceMapping: entries}
}

// TestRequestReviewRequiredItems AT-10：缺验收证据 → missing_required_items
// 携缺项；成果引用空亦缺项；补齐 → reviewing + 锚。
func TestRequestReviewRequiredItems(t *testing.T) {
	svc, mSvc, pk := newSvc(t)
	ctx := context.Background()
	m := readyMission(t, mSvc, "")

	pk.facts["dlv_partial"] = pkg("dlv_partial", m.MissionID, 1, []string{"ws://a"},
		map[string]string{"七端点全绿": "CI 绿", "CAS 冲突可见": ""})
	_, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_partial")
	var missing *MissingRequiredItemsError
	if !errors.As(err, &missing) || !errors.Is(err, ErrMissingRequiredItems) {
		t.Fatalf("缺证据须 missing_required_items: %v", err)
	}
	if len(missing.Items) != 1 || missing.Items[0] != "acceptance:CAS 冲突可见" {
		t.Fatalf("缺项清单: %v", missing.Items)
	}
	if got, _, _ := mSvc.Get(ctx, m.MissionID); got.Status != mission.StatusReady {
		t.Fatalf("拒绝不得改状态: %+v", got)
	}

	// 条文未覆盖（映射缺整条）+ 成果引用空 → 两项缺失
	pk.facts["dlv_norefs"] = pkg("dlv_norefs", m.MissionID, 1, nil, map[string]string{"七端点全绿": "ok"})
	_, err = svc.RequestReview(ctx, m.MissionID, 1, "dlv_norefs")
	if !errors.As(err, &missing) || len(missing.Items) != 2 {
		t.Fatalf("两项缺失: %v", err)
	}

	// 补齐 → reviewing
	pk.facts["dlv_full"] = pkg("dlv_full", m.MissionID, 1, []string{"ws://a"},
		map[string]string{"七端点全绿": "CI 绿", "CAS 冲突可见": "并发测试日志"})
	out, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_full")
	if err != nil || out.Status != mission.StatusReviewing || out.ReviewPackageID != "dlv_full" {
		t.Fatalf("补齐送验: %+v %v", out, err)
	}
}

// TestRequestReviewPackageGuards 包未知 / 不属 Mission / 契约陈旧 / Mission 未知。
func TestRequestReviewPackageGuards(t *testing.T) {
	svc, mSvc, pk := newSvc(t)
	ctx := context.Background()
	m := readyMission(t, mSvc, "")
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_none"); !errors.Is(err, ErrPackageNotFound) {
		t.Fatalf("包未知: %v", err)
	}
	pk.facts["dlv_other"] = pkg("dlv_other", "mi_other", 1, []string{"x"}, map[string]string{"七端点全绿": "a", "CAS 冲突可见": "b"})
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_other"); !errors.Is(err, ErrPackageMismatch) {
		t.Fatalf("包不属 Mission: %v", err)
	}
	pk.facts["dlv_stale"] = pkg("dlv_stale", m.MissionID, 7, []string{"x"}, map[string]string{"七端点全绿": "a", "CAS 冲突可见": "b"})
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_stale"); !errors.Is(err, ErrStalePackage) {
		t.Fatalf("包契约陈旧: %v", err)
	}
	if _, err := svc.RequestReview(ctx, "mi_none", 1, "dlv_stale"); !errors.Is(err, mission.ErrMissionNotFound) {
		t.Fatalf("Mission 未知: %v", err)
	}
	// 版本冲突经 mission 域（包与契约一致但调用方版本陈旧）
	pk.facts["dlv_ok"] = pkg("dlv_ok", m.MissionID, 1, []string{"x"}, map[string]string{"七端点全绿": "a", "CAS 冲突可见": "b"})
	if _, err := svc.RequestReview(ctx, m.MissionID, 9, "dlv_ok"); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("版本冲突: %v", err)
	}
}

// TestReviewDelegates 裁定委托 mission 域：接受 → accepted；退回 → ready。
func TestReviewDelegates(t *testing.T) {
	svc, mSvc, pk := newSvc(t)
	ctx := context.Background()
	m := readyMission(t, mSvc, "qa")
	pk.facts["dlv_ok"] = pkg("dlv_ok", m.MissionID, 1, []string{"x"}, map[string]string{"七端点全绿": "a", "CAS 冲突可见": "b"})
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_ok"); err != nil {
		t.Fatalf("送验: %v", err)
	}
	if _, err := svc.Review(ctx, m.MissionID, 1, 1, mission.ReviewDecision{Decision: mission.ReviewAccepted, Reviewer: "nope"}); !errors.Is(err, mission.ErrReviewNotAuthorized) {
		t.Fatalf("验收人不符: %v", err)
	}
	out, err := svc.Review(ctx, m.MissionID, 1, 1, mission.ReviewDecision{Decision: mission.ReviewReturned, Reviewer: "qa", Reason: "补证"})
	if err != nil || out.Status != mission.StatusReady || out.ReviewPackageID != "" {
		t.Fatalf("退回: %+v %v", out, err)
	}
	svc.RequestReview(ctx, m.MissionID, 1, "dlv_ok") // 第二轮（round 2）
	out, err = svc.Review(ctx, m.MissionID, 1, 2, mission.ReviewDecision{Decision: mission.ReviewAccepted, Reviewer: "qa"})
	if err != nil || out.Status != mission.StatusAccepted || out.ReviewPackageID != "dlv_ok" {
		t.Fatalf("接受: %+v %v", out, err)
	}
}
