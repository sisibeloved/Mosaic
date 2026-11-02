package httpapi

// 0.6.8 交付包端点（ADR-0031）：提交（submit_result 不自动送验——Mission 状态/版本
// 不变 + Run 流 delivery_submitted）、列表倒序、单包、409 族（stale_generation /
// version_conflict / run_not_deliverable / mission_canceled）、400/404/503。
// 真 mission + run + delivery 域（runTestServer 装配）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/delivery"
	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

var errBoom = errors.New("执行器崩溃：exit 1")

// ---- delivery 域端口 fakes（端点级：真 mission/run 服务适配）----

type epDeliveryMissions struct{ svc *mission.Service }

func (p epDeliveryMissions) MissionFacts(ctx context.Context, id string) (delivery.MissionFacts, bool, error) {
	m, ok, err := p.svc.Get(ctx, id)
	if err != nil || !ok {
		return delivery.MissionFacts{}, ok, err
	}
	return delivery.MissionFacts{MissionID: m.MissionID, ContractVersion: m.ContractVersion, Status: m.Status}, true, nil
}

type epDeliveryRuns struct{ svc *missionrun.Service }

func (p epDeliveryRuns) RunFacts(ctx context.Context, id string) (delivery.RunFacts, bool, error) {
	r, ok, err := p.svc.GetRun(ctx, id)
	if err != nil || !ok {
		return delivery.RunFacts{}, ok, err
	}
	return delivery.RunFacts{RunID: r.RunID, MissionID: r.MissionID, Attempt: r.Attempt, Generation: r.Generation,
		State: r.State, Resolution: r.Resolution}, true, nil
}

type runDoc struct {
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	Attempt    int    `json:"attempt"`
	Generation int    `json:"generation"`
}

func executeOnce(t *testing.T, base, mi string) runDoc {
	t.Helper()
	resp := postSlot(t, base+"/v1/missions/"+mi+"/runs", "")
	var rd runDoc
	_ = json.NewDecoder(resp.Body).Decode(&rd)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("执行: %d %+v", resp.StatusCode, rd)
	}
	return rd
}

func submitBody(runID string, gen, ver int) string {
	return `{"run_id":"` + runID + `","generation":` + itoa(int64(gen)) + `,"expected_version":` + itoa(int64(ver)) + `,` +
		`"artifact_refs":["workspace://out/report.md"],` +
		`"acceptance_mapping":[{"criterion":"a","evidence":"CI 绿"}],` +
		`"verification_evidence":"本地复跑","open_items":["待核"],"risks":[],"notes":"首交"}`
}

func TestDeliveryEndpoints(t *testing.T) {
	ts, mSvc, _ := runTestServer(t)
	mi := readyMission(t, mSvc, "ag_r1")
	rd := executeOnce(t, ts.URL, mi)
	if rd.State != "completed" {
		t.Fatalf("预置 completed: %+v", rd)
	}
	before, _, _ := mSvc.Get(t.Context(), mi)

	// 201 提交 → 包视图溯源 + Mission 零副作用 + Run 流回执
	r := postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", submitBody(rd.RunID, rd.Generation, before.ContractVersion))
	var pkg struct {
		PackageID       string   `json:"package_id"`
		MissionID       string   `json:"mission_id"`
		RunID           string   `json:"run_id"`
		Attempt         int      `json:"attempt"`
		Generation      int      `json:"generation"`
		ContractVersion int      `json:"contract_version"`
		ArtifactRefs    []string `json:"artifact_refs"`
		Risks           []string `json:"risks"`
		ActionReceipts  []string `json:"action_receipts"`
		AcceptanceMap   []struct {
			Criterion string `json:"criterion"`
			Evidence  string `json:"evidence"`
		} `json:"acceptance_mapping"`
		SubmittedAt string `json:"submitted_at"`
	}
	_ = json.NewDecoder(r.Body).Decode(&pkg)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated || pkg.PackageID == "" || pkg.RunID != rd.RunID || pkg.Attempt != rd.Attempt ||
		pkg.Generation != rd.Generation || pkg.ContractVersion != before.ContractVersion || len(pkg.AcceptanceMap) != 1 ||
		pkg.Risks == nil || pkg.ActionReceipts == nil || pkg.SubmittedAt == "" {
		t.Fatalf("提交: %d %+v", r.StatusCode, pkg)
	}
	after, _, _ := mSvc.Get(t.Context(), mi)
	if after.Status != before.Status || after.ContractVersion != before.ContractVersion || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("提交须对 Mission 零副作用: before=%+v after=%+v", before, after)
	}
	if after.Status != mission.StatusReady {
		t.Fatalf("不自动送验——仍 ready: %s", after.Status)
	}
	evs, _ := http.Get(ts.URL + "/v1/mission-runs/" + rd.RunID + "/events")
	var evDoc struct {
		Events []struct {
			EventType string         `json:"event_type"`
			Payload   map[string]any `json:"payload"`
		} `json:"events"`
	}
	_ = json.NewDecoder(evs.Body).Decode(&evDoc)
	evs.Body.Close()
	var receipt bool
	for _, e := range evDoc.Events {
		if e.EventType == "delivery_submitted" && e.Payload["package_id"] == pkg.PackageID {
			receipt = true
		}
	}
	if !receipt {
		t.Fatalf("Run 流须有 delivery_submitted 回执: %+v", evDoc.Events)
	}
	// mission_events 不新增（契约流不混入交付）
	mevs, _ := http.Get(ts.URL + "/v1/missions/" + mi + "/events")
	var mevDoc struct {
		Events []struct {
			EventType string `json:"event_type"`
		} `json:"events"`
	}
	_ = json.NewDecoder(mevs.Body).Decode(&mevDoc)
	mevs.Body.Close()
	for _, e := range mevDoc.Events {
		if e.EventType != "created" && e.EventType != "status_changed" {
			t.Fatalf("mission_events 不得混入交付事件: %+v", mevDoc.Events)
		}
	}

	// 补证：同 Run 再提交 → 新包；列表倒序首项为最新；单包 GET
	r2 := postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", submitBody(rd.RunID, rd.Generation, before.ContractVersion))
	var pkg2 struct {
		PackageID string `json:"package_id"`
	}
	_ = json.NewDecoder(r2.Body).Decode(&pkg2)
	r2.Body.Close()
	if r2.StatusCode != http.StatusCreated || pkg2.PackageID == pkg.PackageID {
		t.Fatalf("补证新包: %d %+v", r2.StatusCode, pkg2)
	}
	list, _ := http.Get(ts.URL + "/v1/missions/" + mi + "/delivery-packages")
	var listDoc struct {
		Packages []struct {
			PackageID string `json:"package_id"`
		} `json:"packages"`
	}
	_ = json.NewDecoder(list.Body).Decode(&listDoc)
	list.Body.Close()
	if len(listDoc.Packages) != 2 || listDoc.Packages[0].PackageID != pkg2.PackageID {
		t.Fatalf("列表倒序: %+v", listDoc.Packages)
	}
	one, _ := http.Get(ts.URL + "/v1/delivery-packages/" + pkg.PackageID)
	if one.StatusCode != http.StatusOK {
		t.Fatalf("单包: %d", one.StatusCode)
	}
	one.Body.Close()
	nf, _ := http.Get(ts.URL + "/v1/delivery-packages/dlv_none")
	if nf.StatusCode != http.StatusNotFound || errCodeOf(t, nf) != "package_not_found" {
		t.Fatalf("未知包: %d", nf.StatusCode)
	}

	// 409 族
	for name, tc := range map[string]struct {
		body string
		code string
	}{
		"代次陈旧": {submitBody(rd.RunID, rd.Generation+1, before.ContractVersion), "stale_generation"},
		"版本冲突": {submitBody(rd.RunID, rd.Generation, before.ContractVersion+5), "version_conflict"},
	} {
		rr := postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", tc.body)
		if rr.StatusCode != http.StatusConflict || errCodeOf(t, rr) != tc.code {
			t.Fatalf("%s 应 409 %s: %d", name, tc.code, rr.StatusCode)
		}
	}
	// 400：形状
	bad := postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", `{"run_id":"`+rd.RunID+`","generation":`+itoa(int64(rd.Generation))+`,"expected_version":1,"artifact_refs":[],"acceptance_mapping":[{"criterion":"","evidence":""}]}`)
	if bad.StatusCode != http.StatusBadRequest || errCodeOf(t, bad) != "invalid_delivery" {
		t.Fatalf("形状不合法应 400: %d", bad.StatusCode)
	}
	// 404：Run 未知 / Mission 未知
	nr := postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", submitBody("run_none", 1, before.ContractVersion))
	if nr.StatusCode != http.StatusNotFound || errCodeOf(t, nr) != "run_not_found" {
		t.Fatalf("Run 未知: %d", nr.StatusCode)
	}
	nm := postSlot(t, ts.URL+"/v1/missions/mi_none/delivery-packages", submitBody(rd.RunID, rd.Generation, 1))
	if nm.StatusCode != http.StatusNotFound || errCodeOf(t, nm) != "mission_not_found" {
		t.Fatalf("Mission 未知: %d", nm.StatusCode)
	}
	nl, _ := http.Get(ts.URL + "/v1/missions/mi_none/delivery-packages")
	if nl.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 Mission 列表: %d", nl.StatusCode)
	}
	nl.Body.Close()
}

// TestDeliveryNotDeliverableAndCanceled failed Run → 409 run_not_deliverable；
// 取消后的 Mission → 409 mission_canceled（读面照常）。
func TestDeliveryNotDeliverableAndCanceled(t *testing.T) {
	ts, mSvc, _ := runTestServer(t, func(c *runTestCfg) { c.executorErr = errBoom })
	mi := readyMission(t, mSvc, "ag_r1")
	rd := executeOnce(t, ts.URL, mi)
	if rd.State != "failed" {
		t.Fatalf("预置 failed: %+v", rd)
	}
	m, _, _ := mSvc.Get(t.Context(), mi)
	r := postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", submitBody(rd.RunID, rd.Generation, m.ContractVersion))
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "run_not_deliverable" {
		t.Fatalf("failed 不可交付: %d", r.StatusCode)
	}
	// 取消（无在途直达 canceled）→ 提交拒绝 mission_canceled；列表照常 200
	c := postSlot(t, ts.URL+"/v1/missions/"+mi+"/cancel", `{"expected_version":`+itoa(int64(m.ContractVersion))+`}`)
	c.Body.Close()
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/delivery-packages", submitBody(rd.RunID, rd.Generation, m.ContractVersion))
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "mission_canceled" {
		t.Fatalf("canceled 拒绝提交: %d", r.StatusCode)
	}
	list, _ := http.Get(ts.URL + "/v1/missions/" + mi + "/delivery-packages")
	if list.StatusCode != http.StatusOK {
		t.Fatalf("canceled 读面照常: %d", list.StatusCode)
	}
	list.Body.Close()
}

// TestDeliveryUnavailable 交付域未装配 → 503 delivery_unavailable。
func TestDeliveryUnavailable(t *testing.T) {
	bare := httptest.NewServer(New(Deps{}))
	t.Cleanup(bare.Close)
	r := postSlot(t, bare.URL+"/v1/missions/mi_x/delivery-packages", submitBody("run_x", 1, 1))
	if r.StatusCode != http.StatusServiceUnavailable || errCodeOf(t, r) != "delivery_unavailable" {
		t.Fatalf("未装配应 503: %d", r.StatusCode)
	}
	g, _ := http.Get(bare.URL + "/v1/delivery-packages/dlv_x")
	if g.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("单包未装配应 503: %d", g.StatusCode)
	}
	g.Body.Close()
}
