package httpapi

// 0.6.7 unknown Run 核验处置端点（ADR-0030）：POST /v1/mission-runs/{id}/resolve
// ——真 run 域 + 真 mission 域（恢复播种 → unknown + waiting）；200 视图携
// resolution、Mission 回 ready；幂等/409 族/400/404/503；诊断面 unknown 计数。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

// seedUnknownViaRecovery 播种"进程重启前在执行"的 Run 并走真恢复 → unknown +
// Mission waiting。
func seedUnknownViaRecovery(t *testing.T, h *runHandles, mSvc *mission.Service, mi string) string {
	t.Helper()
	ctx := context.Background()
	if changed, err := mSvc.MarkRunning(ctx, mi); err != nil || !changed {
		t.Fatalf("预置 running: %v %v", changed, err)
	}
	r, err := h.runStore.CreateRun(ctx, missionrun.Run{
		RunID: "run_crash", MissionID: mi, Attempt: 1, LeaseID: "lse_ep",
		State: missionrun.StateRunning, Generation: 1, RequestedAt: "t", StartedAt: "t", UpdatedAt: "t",
	}, 0, 0, "")
	if err != nil {
		t.Fatalf("播种: %v", err)
	}
	if _, err := h.runSvc.RecoverUnfinished(ctx); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	return r.RunID
}

func TestResolveRunEndpoint(t *testing.T) {
	h := &runHandles{}
	ts, mSvc, _ := runTestServer(t, func(c *runTestCfg) { c.capture = h })
	mi := readyMission(t, mSvc, "ag_r1")
	runID := seedUnknownViaRecovery(t, h, mSvc, mi)
	ctx := context.Background()

	// 恢复后：Run unknown、Mission waiting(reconciliation)——视图可见
	get, _ := http.Get(ts.URL + "/v1/mission-runs/" + runID)
	var rv struct {
		State      string `json:"state"`
		Resolution string `json:"resolution"`
	}
	_ = json.NewDecoder(get.Body).Decode(&rv)
	get.Body.Close()
	if rv.State != "unknown" || rv.Resolution != "" {
		t.Fatalf("恢复后视图: %+v", rv)
	}
	mget, _ := http.Get(ts.URL + "/v1/missions/" + mi)
	var mv struct {
		Status        string `json:"status"`
		WaitingReason string `json:"waiting_reason"`
	}
	_ = json.NewDecoder(mget.Body).Decode(&mv)
	mget.Body.Close()
	if mv.Status != "waiting" || mv.WaitingReason != "reconciliation" {
		t.Fatalf("Mission 视图: %+v", mv)
	}
	// 诊断面 unknown 计数
	st, _ := http.Get(ts.URL + "/v1/scheduler/status")
	var sd struct {
		Unresolved int `json:"unresolved_unknown_runs"`
	}
	_ = json.NewDecoder(st.Body).Decode(&sd)
	st.Body.Close()
	if sd.Unresolved != 1 {
		t.Fatalf("诊断未处置计数: %+v", sd)
	}

	// 400：非法结论 / 备注超限 / 缺 outcome
	for name, body := range map[string]string{
		"非法结论": `{"outcome":"maybe"}`,
		"缺结论":  `{}`,
	} {
		r := postSlot(t, ts.URL+"/v1/mission-runs/"+runID+"/resolve", body)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d", name, r.StatusCode)
		}
		r.Body.Close()
	}

	// 200：处置 → state 仍 unknown、resolution 落；Mission 回 ready 清原因
	r := postSlot(t, ts.URL+"/v1/mission-runs/"+runID+"/resolve", `{"outcome":"verified_completed","note":"核对工作区产出齐全"}`)
	var out struct {
		State          string `json:"state"`
		Resolution     string `json:"resolution"`
		ResolutionNote string `json:"resolution_note"`
		ResolvedAt     string `json:"resolved_at"`
	}
	_ = json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || out.State != "unknown" || out.Resolution != "verified_completed" ||
		out.ResolutionNote != "核对工作区产出齐全" || out.ResolvedAt == "" {
		t.Fatalf("处置: %d %+v", r.StatusCode, out)
	}
	got, _, _ := mSvc.Get(ctx, mi)
	if got.Status != mission.StatusReady || got.WaitingReason != "" {
		t.Fatalf("处置后 Mission: %+v", got)
	}
	st, _ = http.Get(ts.URL + "/v1/scheduler/status")
	_ = json.NewDecoder(st.Body).Decode(&sd)
	st.Body.Close()
	if sd.Unresolved != 0 {
		t.Fatalf("处置后计数归零: %+v", sd)
	}

	// 幂等：同结论 200；异结论 409 run_already_resolved
	r = postSlot(t, ts.URL+"/v1/mission-runs/"+runID+"/resolve", `{"outcome":"verified_completed"}`)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("幂等重复: %d", r.StatusCode)
	}
	r.Body.Close()
	r = postSlot(t, ts.URL+"/v1/mission-runs/"+runID+"/resolve", `{"outcome":"discarded"}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "run_already_resolved" {
		t.Fatalf("异结论: %d", r.StatusCode)
	}

	// 处置后可再执行（手动触发 → 201 completed attempt 2）
	ex := postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	var runDoc struct {
		RunID   string `json:"run_id"`
		State   string `json:"state"`
		Attempt int    `json:"attempt"`
	}
	_ = json.NewDecoder(ex.Body).Decode(&runDoc)
	ex.Body.Close()
	if ex.StatusCode != http.StatusCreated || runDoc.State != "completed" || runDoc.Attempt != 2 {
		t.Fatalf("处置后再执行: %d %+v", ex.StatusCode, runDoc)
	}
	// 非 unknown → 409 run_not_unknown
	r = postSlot(t, ts.URL+"/v1/mission-runs/"+runDoc.RunID+"/resolve", `{"outcome":"discarded"}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "run_not_unknown" {
		t.Fatalf("非 unknown: %d", r.StatusCode)
	}
	// 404
	r = postSlot(t, ts.URL+"/v1/mission-runs/run_none/resolve", `{"outcome":"discarded"}`)
	if r.StatusCode != http.StatusNotFound || errCodeOf(t, r) != "run_not_found" {
		t.Fatalf("未知: %d", r.StatusCode)
	}
}

// TestResolveRunUnavailable run 域未装配 → 503 run_unavailable。
func TestResolveRunUnavailable(t *testing.T) {
	bare := httptest.NewServer(New(Deps{}))
	t.Cleanup(bare.Close)
	r := postSlot(t, bare.URL+"/v1/mission-runs/run_x/resolve", `{"outcome":"discarded"}`)
	if r.StatusCode != http.StatusServiceUnavailable || errCodeOf(t, r) != "run_unavailable" {
		t.Fatalf("未装配应 503: %d", r.StatusCode)
	}
}

// TestWaitingBlocksManualRun waiting 期间手动触发 → 409 mission_not_ready（端点
// 前置门携当前状态）——禁止盲目重跑在端点面可见。
func TestWaitingBlocksManualRun(t *testing.T) {
	h := &runHandles{}
	ts, mSvc, _ := runTestServer(t, func(c *runTestCfg) { c.capture = h })
	mi := readyMission(t, mSvc, "ag_r1")
	seedUnknownViaRecovery(t, h, mSvc, mi)
	ex := postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	if ex.StatusCode != http.StatusConflict || errCodeOf(t, ex) != "mission_not_ready" {
		t.Fatalf("waiting 期间手动触发须 409 mission_not_ready: %d", ex.StatusCode)
	}
}
