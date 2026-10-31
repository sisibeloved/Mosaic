package httpapi

// 0.6.5 执行端点（ADR-0028）：手动调度全链（真实 mission + run 域小端口
// fakes——echo 执行器）、409 族、Run 历史/详情/事件、调度器诊断、503。
import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

// ---- run 域端口 fakes（端点级）----

type epRunMissions struct{ svc *mission.Service }

func (p epRunMissions) ListReady(ctx context.Context) ([]missionrun.MissionRef, error) {
	list, err := p.svc.ListMissions(ctx)
	if err != nil {
		return nil, err
	}
	var out []missionrun.MissionRef
	for _, m := range list {
		if m.Status == mission.StatusReady {
			out = append(out, missionrun.MissionRef{MissionID: m.MissionID, OwnerAgentID: m.OwnerAgentID})
		}
	}
	return out, nil
}
func (p epRunMissions) SetRunning(ctx context.Context, id string) (bool, error) {
	return p.svc.MarkRunning(ctx, id)
}
func (p epRunMissions) SetReadyFromRunning(ctx context.Context, id, reason string) error {
	return p.svc.MarkReadyFromRunning(ctx, id, reason)
}

type epRunBindings struct{ agentID string }

func (f epRunBindings) ActiveOf(_ context.Context, agentID string) (missionrun.BindingFacts, bool, error) {
	if agentID == f.agentID {
		return missionrun.BindingFacts{BindingID: "bd_ep", AgentID: agentID, SlotSpecID: "ss_ep"}, true, nil
	}
	return missionrun.BindingFacts{}, false, nil
}

type epRunRuntimes struct{ has bool }

func (f epRunRuntimes) FirstReady(context.Context) (missionrun.RuntimeRef, bool, error) {
	if f.has {
		return missionrun.RuntimeRef{RuntimeID: "rtx_ep", EnvironmentID: "env_ep"}, true, nil
	}
	return missionrun.RuntimeRef{}, false, nil
}

type epRunLeases struct {
	acquired int
	released int
	fenceOK  bool
}

func (f *epRunLeases) Acquire(context.Context, string, string, int, map[string]int) (missionrun.Lease, error) {
	f.acquired++
	return missionrun.Lease{LeaseID: "lse_ep", FenceToken: "fnc_ep", Generation: f.acquired, EnvironmentID: "env_ep"}, nil
}
func (f *epRunLeases) Release(context.Context, string) error { f.released++; return nil }
func (f *epRunLeases) CheckFence(context.Context, string, string) (bool, string, error) {
	return f.fenceOK, "", nil
}

type epRunManifests struct{ blocked bool }

func (f epRunManifests) Build(context.Context, string, string) (missionrun.Manifest, error) {
	if f.blocked {
		return missionrun.Manifest{State: "blocked", ReasonCode: "readiness_not_met"}, nil
	}
	return missionrun.Manifest{ManifestID: "cmf_ep", State: "assembled"}, nil
}

type epRunExecutor struct {
	block string
	err   error
}

func (f epRunExecutor) Submit(context.Context, missionrun.ExecutionInput) (string, error) {
	return f.block, f.err
}

// runTestServer 构造带 run 域的测试面。
func runTestServer(t *testing.T, opts ...func(*runTestCfg)) (*httptest.Server, *mission.Service, *epRunLeases) {
	t.Helper()
	cfg := &runTestCfg{executorBlock: "echo 执行结果正文", hasRuntime: true, fenceOK: true}
	for _, opt := range opts {
		opt(cfg)
	}
	var seq int64
	ident := personalagent.New(personalagent.NewMemStore(), emptyIdentityRegistry{},
		personalagent.WithNewAgentID(func() string { seq++; return "ag_r" + itoa(seq) }),
		personalagent.WithNow(func() string { return "2026-10-31T12:00:00.000Z" }))
	mSvc := mission.New(mission.NewMemStore(),
		mission.WithNow(func() string { return "2026-10-31T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { seq++; return "mi_r" + itoa(seq) }),
		mission.WithAgentLookup(missionIdentAgents{ident}))
	// 预建 owner agent（ag_r1 = ident 首个生成 ID）——owner 存在性经身份域
	if _, err := ident.CreateManual(context.Background(), "执行者", ""); err != nil {
		t.Fatalf("建 owner agent: %v", err)
	}
	leases := &epRunLeases{fenceOK: cfg.fenceOK}
	runSvc := missionrun.New(missionrun.NewMemStore(),
		epRunMissions{mSvc},
		epRunBindings{agentID: "ag_r1"},
		epRunRuntimes{has: cfg.hasRuntime},
		leases,
		epRunManifests{blocked: cfg.manifestBlocked},
		epRunExecutor{block: cfg.executorBlock, err: cfg.executorErr},
		missionrun.WithNow(func() string { return "2026-10-31T12:00:00.000Z" }),
		missionrun.WithNewRunID(func() string { seq++; return "run_r" + itoa(seq) }),
		missionrun.WithSchedulerDisabled())
	ts := httptest.NewServer(New(Deps{Missions: mSvc, MissionRuns: runSvc, Identity: ident}))
	t.Cleanup(ts.Close)
	return ts, mSvc, leases
}

type runTestCfg struct {
	executorBlock   string
	executorErr     error
	hasRuntime      bool
	manifestBlocked bool
	fenceOK         bool
}

func readyMission(t *testing.T, mSvc *mission.Service, owner string) string {
	t.Helper()
	m, err := mSvc.Create(context.Background(), owner, mission.Content{
		Objective: "执行测试", Scope: "s",
		AcceptanceCriteria: []string{"a"}, DeliverableSpecs: []string{"d"},
		SourceRefs: []string{"r:1"}, Budget: mission.DefaultBudget(),
	}, nil)
	if err != nil {
		t.Fatalf("建 Mission: %v", err)
	}
	return m.MissionID
}

func TestRunEndpoints(t *testing.T) {
	ts, mSvc, leases := runTestServer(t)
	mi := readyMission(t, mSvc, "ag_r1")

	// 手动调度 → 201 completed（echo 全链）
	resp := postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	var runDoc struct {
		RunID       string `json:"run_id"`
		State       string `json:"state"`
		Attempt     int    `json:"attempt"`
		ResultBlock string `json:"result_block"`
		LeaseID     string `json:"lease_id"`
		ManifestID  string `json:"manifest_id"`
		FenceToken  string `json:"fence_token"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("调度期望 201，得到 %d", resp.StatusCode)
	}
	decodeBody(t, resp, &runDoc)
	if runDoc.State != "completed" || runDoc.ResultBlock == "" || runDoc.Attempt != 1 ||
		runDoc.LeaseID == "" || runDoc.ManifestID == "" || runDoc.FenceToken == "" {
		t.Fatalf("Run 事实: %+v", runDoc)
	}
	if leases.released != 1 {
		t.Fatalf("执行完成须释放租约: %d", leases.released)
	}

	// 再执行 → attempt 2
	resp = postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	decodeBody(t, resp, &runDoc)
	if runDoc.Attempt != 2 {
		t.Fatalf("再执行 attempt 2: %+v", runDoc)
	}

	// Run 历史 + 详情 + 事件
	resp, _ = http.Get(ts.URL + "/v1/missions/" + mi + "/runs")
	var listDoc struct {
		Runs []struct {
			RunID string `json:"run_id"`
		} `json:"runs"`
	}
	decodeBody(t, resp, &listDoc)
	if len(listDoc.Runs) != 2 {
		t.Fatalf("Run 历史: %+v", listDoc)
	}
	resp, _ = http.Get(ts.URL + "/v1/mission-runs/" + runDoc.RunID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Run 详情: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/v1/mission-runs/" + runDoc.RunID + "/events")
	var evDoc struct {
		Events []struct {
			EventType string `json:"event_type"`
		} `json:"events"`
	}
	decodeBody(t, resp, &evDoc)
	if len(evDoc.Events) != 3 { // attempt_requested + started + completed
		t.Fatalf("Run 事件: %+v", evDoc.Events)
	}

	// 调度器诊断面
	resp, _ = http.Get(ts.URL + "/v1/scheduler/status")
	var stDoc struct {
		LaunchedRuns int64 `json:"launched_runs"`
		ActiveRuns   int   `json:"active_runs"`
	}
	decodeBody(t, resp, &stDoc)
	if stDoc.LaunchedRuns != 2 || stDoc.ActiveRuns != 0 {
		t.Fatalf("诊断面: %+v", stDoc)
	}

	// 404 族
	resp, _ = http.Get(ts.URL + "/v1/mission-runs/run_none")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "run_not_found" {
		t.Fatalf("未知 Run: %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/missions/mi_none/runs", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "mission_not_found" {
		t.Fatalf("未知 Mission: %d/%s", resp.StatusCode, code)
	}
}

// TestRunEndpointsNoExecutor 无 ready Runtime → 409 no_executor。
func TestRunEndpointsNoExecutor(t *testing.T) {
	ts, mSvc, _ := runTestServer(t, func(c *runTestCfg) { c.hasRuntime = false })
	mi := readyMission(t, mSvc, "ag_r1")
	resp := postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusConflict || code != "no_executor" {
		t.Fatalf("无运行时: %d/%s", resp.StatusCode, code)
	}
}

// TestRunEndpointsManifestBlocked 清单 blocked → 409 manifest_blocked。
func TestRunEndpointsManifestBlocked(t *testing.T) {
	ts, mSvc, leases := runTestServer(t, func(c *runTestCfg) { c.manifestBlocked = true })
	mi := readyMission(t, mSvc, "ag_r1")
	resp := postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusConflict || code != "manifest_blocked" {
		t.Fatalf("清单 blocked: %d/%s", resp.StatusCode, code)
	}
	if leases.released != 1 {
		t.Fatalf("blocked 须释放租约: %d", leases.released)
	}
}

// TestRunUnavailable 未装配 → 503。
func TestRunUnavailable(t *testing.T) {
	ts := httptest.NewServer(New(Deps{}))
	t.Cleanup(ts.Close)
	resp := postSlot(t, ts.URL+"/v1/missions/mi_x/runs", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "run_unavailable" {
		t.Fatalf("调度期望 503，得到 %d/%s", resp.StatusCode, code)
	}
	for _, path := range []string{"/v1/missions/mi_x/runs", "/v1/mission-runs/run_x",
		"/v1/mission-runs/run_x/events", "/v1/scheduler/status"} {
		resp, _ := http.Get(ts.URL + path)
		if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "run_unavailable" {
			t.Fatalf("%s 期望 503，得到 %d/%s", path, resp.StatusCode, code)
		}
	}
}

// 保留 slot 引用（编译需要——BindingFacts 端点 fake 已不再直接用 slot）。
var _ = slot.BindingActive
