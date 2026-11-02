package review

// 0.6.9 审校修复（ADR-0032 v0.2）：#1 纯空白证据不入账（TrimSpace）、#12 预填
// 下一版本号与修订交错不得跳标准（expected_version 须等于读到的版本）、#2 在途/
// 未处置 Run 拦送验（ErrRunsUnsettled）。测试先行。

import (
	"context"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

type fakeRuns struct{ inflight, unresolved int }

func (f fakeRuns) Unsettled(context.Context, string) (int, int, error) {
	return f.inflight, f.unresolved, nil
}

func newSvcWithRuns(t *testing.T, runs Runs) (*Service, *mission.Service, *fakePackages) {
	t.Helper()
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-11-02T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_r" }),
		mission.WithAgentLookup(stubAgents{}))
	pk := &fakePackages{facts: map[string]PackageFacts{}}
	return New(mSvc, pk, runs), mSvc, pk
}

// TestWhitespaceEvidenceRejected 审校 #1：证据为纯空白（空格/制表）按缺项拒绝；
// 实质证据通过；缺项清单列出该标准。
func TestWhitespaceEvidenceRejected(t *testing.T) {
	svc, mSvc, pk := newSvcWithRuns(t, fakeRuns{})
	ctx := context.Background()
	m := readyMission(t, mSvc, "")
	pk.facts["dlv_ws"] = pkg("dlv_ws", m.MissionID, 1, []string{"ws://a"}, map[string]string{
		"七端点全绿":    "CI 绿",
		"CAS 冲突可见": "   ",
	})
	_, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_ws")
	var missing *MissingRequiredItemsError
	if !errors.As(err, &missing) {
		t.Fatalf("纯空白证据须按缺项拒绝: %v", err)
	}
	found := false
	for _, it := range missing.Items {
		if it == "acceptance:CAS 冲突可见" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺项清单须列出该标准: %v", missing.Items)
	}
	// 制表符同罪
	pk.facts["dlv_tab"] = pkg("dlv_tab", m.MissionID, 1, []string{"ws://a"}, map[string]string{
		"七端点全绿":    "\t\n",
		"CAS 冲突可见": "ok",
	})
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_tab"); !errors.As(err, &missing) {
		t.Fatalf("制表空白同罪: %v", err)
	}
	// 实质证据（含首尾空白）通过
	pk.facts["dlv_ok"] = pkg("dlv_ok", m.MissionID, 1, []string{"ws://a"}, map[string]string{
		"七端点全绿":    "  CI 绿  ",
		"CAS 冲突可见": "ok",
	})
	if _, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_ok"); err != nil {
		t.Fatalf("实质证据应通过: %v", err)
	}
}

// TestRequestReviewVersionPrecheck 审校 #12：expected_version 不等于读到的版本 →
// 立即冲突（不做事完备性检查——防预填下一版本号与修订交错跳标准）。
func TestRequestReviewVersionPrecheck(t *testing.T) {
	svc, mSvc, pk := newSvcWithRuns(t, fakeRuns{})
	ctx := context.Background()
	m := readyMission(t, mSvc, "")
	pk.facts["dlv_v2"] = pkg("dlv_v2", m.MissionID, 1, []string{"x"}, map[string]string{
		"七端点全绿": "a", "CAS 冲突可见": "b",
	})
	// 调用方预填 v2（下一版本）而读到 v1 → 冲突，而非以 v1 标准校验后按 v2 CAS
	if _, err := svc.RequestReview(ctx, m.MissionID, 2, "dlv_v2"); !errors.Is(err, mission.ErrVersionConflict) {
		t.Fatalf("版本预读不符须冲突: %v", err)
	}
	if got, _, _ := mSvc.Get(ctx, m.MissionID); got.Status != mission.StatusReady {
		t.Fatalf("不得改状态: %+v", got)
	}
}

// TestRequestReviewUnsettledRuns 审校 #2：在途或未处置 unknown Run 拦送验——
// 待核验完成再放行（409 runs_unsettled 语义）。
func TestRequestReviewUnsettledRuns(t *testing.T) {
	for name, runs := range map[string]fakeRuns{
		"在途 Run":      {inflight: 1},
		"未处置 unknown": {unresolved: 1},
	} {
		svc, mSvc, pk := newSvcWithRuns(t, runs)
		ctx := context.Background()
		m := readyMission(t, mSvc, "")
		pk.facts["dlv_ok"] = pkg("dlv_ok", m.MissionID, 1, []string{"x"}, map[string]string{
			"七端点全绿": "a", "CAS 冲突可见": "b",
		})
		_, err := svc.RequestReview(ctx, m.MissionID, 1, "dlv_ok")
		if !errors.Is(err, ErrRunsUnsettled) {
			t.Fatalf("%s 须拦送验: %v", name, err)
		}
		if got, _, _ := mSvc.Get(ctx, m.MissionID); got.Status != mission.StatusReady {
			t.Fatalf("%s 不得改状态: %+v", name, got)
		}
	}
	// 无残留照常送验
	svc, mSvc, pk := newSvcWithRuns(t, fakeRuns{})
	m := readyMission(t, mSvc, "")
	pk.facts["dlv_ok"] = pkg("dlv_ok", m.MissionID, 1, []string{"x"}, map[string]string{
		"七端点全绿": "a", "CAS 冲突可见": "b",
	})
	if _, err := svc.RequestReview(context.Background(), m.MissionID, 1, "dlv_ok"); err != nil {
		t.Fatalf("无残留应放行: %v", err)
	}
}
