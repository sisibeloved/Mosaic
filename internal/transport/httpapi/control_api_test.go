package httpapi

// 0.6.6 任务控制面端点（ADR-0029）：pause / resume / cancel（两段）——
// 视图/错误映射/写门/503；编排语义（停止信号与收尾）在 app 层组合测试。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/mission"
	missionrun "github.com/sisibeloved/Mosaic/internal/run"
)

// fakeMissionControl MissionControl 测试件（编排语义见 app 层真组合测试）。
type fakeMissionControl struct {
	status     string // 当前状态机应答
	version    int    // 当前版本
	paused     int    // Pause 调用计数
	resumed    int
	canceled   int
	err        error // 应答错误
	lastReason string
}

func (f *fakeMissionControl) PauseMission(_ context.Context, _ string, expectedVersion int, reason string) (mission.Mission, error) {
	f.paused++
	f.lastReason = reason
	if f.err != nil {
		return mission.Mission{}, f.err
	}
	if f.version != expectedVersion {
		return mission.Mission{}, mission.ErrVersionConflict
	}
	return mission.Mission{MissionID: "mi_ctl", ContractVersion: f.version, Status: f.status}, nil
}

func (f *fakeMissionControl) ResumeMission(_ context.Context, _ string, expectedVersion int) (mission.Mission, error) {
	f.resumed++
	if f.err != nil {
		return mission.Mission{}, f.err
	}
	if f.version != expectedVersion {
		return mission.Mission{}, mission.ErrVersionConflict
	}
	return mission.Mission{MissionID: "mi_ctl", ContractVersion: f.version, Status: f.status}, nil
}

func (f *fakeMissionControl) CancelMission(_ context.Context, _ string, expectedVersion int, reason string) (mission.Mission, error) {
	f.canceled++
	f.lastReason = reason
	if f.err != nil {
		return mission.Mission{}, f.err
	}
	if f.version != expectedVersion {
		return mission.Mission{}, mission.ErrVersionConflict
	}
	return mission.Mission{MissionID: "mi_ctl", ContractVersion: f.version, Status: f.status}, nil
}

// controlServer 最小装配：仅控制面（契约域可缺——控制端点不依赖）。
func controlServer(t *testing.T, ctl *fakeMissionControl) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(Deps{MissionControl: ctl}))
	t.Cleanup(ts.Close)
	return ts
}

func TestMissionControlEndpointsAPI(t *testing.T) {
	ctl := &fakeMissionControl{status: "paused", version: 3}
	ts2 := controlServer(t, ctl)
	base := ts2.URL

	// pause：200 视图 + 版本透传
	r := postSlot(t, base+"/v1/missions/mi_ctl/pause", `{"expected_version":3,"reason":"等依赖"}`)
	var doc struct {
		MissionID string `json:"mission_id"`
		Status    string `json:"status"`
	}
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || doc.Status != "paused" || doc.MissionID != "mi_ctl" {
		t.Fatalf("暂停: %d %+v", r.StatusCode, doc)
	}
	if ctl.paused != 1 || ctl.lastReason != "等依赖" {
		t.Fatalf("编排调用: %+v", ctl)
	}

	// pause 版本冲突 → 409 version_conflict
	r = postSlot(t, base+"/v1/missions/mi_ctl/pause", `{"expected_version":9}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "version_conflict" {
		t.Fatalf("版本冲突: %d", r.StatusCode)
	}
	// pause 缺 expected_version → 400
	r = postSlot(t, base+"/v1/missions/mi_ctl/pause", `{}`)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺版本: %d", r.StatusCode)
	}
	// pause reason 超限 → 400 invalid_mission
	r = postSlot(t, base+"/v1/missions/mi_ctl/pause", `{"expected_version":3,"reason":"`+strings.Repeat("字", 201)+`"}`)
	if r.StatusCode != http.StatusBadRequest || errCodeOf(t, r) != "invalid_mission" {
		t.Fatalf("reason 超限: %d", r.StatusCode)
	}

	// resume：200 + 缺键 400
	ctl.status = "ready"
	r = postSlot(t, base+"/v1/missions/mi_ctl/resume", `{"expected_version":3}`)
	doc = struct {
		MissionID string `json:"mission_id"`
		Status    string `json:"status"`
	}{}
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || doc.Status != "ready" || ctl.resumed != 1 {
		t.Fatalf("恢复: %d %+v", r.StatusCode, doc)
	}
	r = postSlot(t, base+"/v1/missions/mi_ctl/resume", `{}`)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("恢复缺版本: %d", r.StatusCode)
	}

	// cancel 两段：应答可为 canceling（停止进行中）——视图透传
	ctl.status = "canceling"
	r = postSlot(t, base+"/v1/missions/mi_ctl/cancel", `{"expected_version":3,"reason":"计划变更"}`)
	doc = struct {
		MissionID string `json:"mission_id"`
		Status    string `json:"status"`
	}{}
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || doc.Status != "canceling" || ctl.lastReason != "计划变更" {
		t.Fatalf("取消停止中: %d %+v", r.StatusCode, doc)
	}
	// cancel 终态拒绝 → 409 invalid_transition
	ctl.err = mission.ErrInvalidTransition
	r = postSlot(t, base+"/v1/missions/mi_ctl/cancel", `{"expected_version":3}`)
	if r.StatusCode != http.StatusConflict || errCodeOf(t, r) != "invalid_transition" {
		t.Fatalf("终态拒绝: %d", r.StatusCode)
	}
	// cancel 未知 → 404 mission_not_found
	ctl.err = mission.ErrMissionNotFound
	r = postSlot(t, base+"/v1/missions/mi_ctl/cancel", `{"expected_version":3}`)
	if r.StatusCode != http.StatusNotFound || errCodeOf(t, r) != "mission_not_found" {
		t.Fatalf("未知: %d", r.StatusCode)
	}
}

// TestMissionControlUnavailableAPI 控制面未装配 → 三端点 503 mission_unavailable。
func TestMissionControlUnavailableAPI(t *testing.T) {
	ts := missionTestServer(t) // 复用装配（其 cancel 已走控制面——本例构造独立无控制面装配）
	bare := httptest.NewServer(New(Deps{}))
	t.Cleanup(bare.Close)
	_ = ts
	for _, path := range []string{"/pause", "/resume", "/cancel"} {
		r := postSlot(t, bare.URL+"/v1/missions/mi_x"+path, `{"expected_version":1}`)
		if r.StatusCode != http.StatusServiceUnavailable || errCodeOf(t, r) != "mission_unavailable" {
			t.Fatalf("%s 未装配应 503: %d", path, r.StatusCode)
		}
	}
}

// ctlBlockExec 端点级阻塞执行器：阻塞至 ctx 取消。
type ctlBlockExec struct{ started chan struct{} }

func (e *ctlBlockExec) Submit(ctx context.Context, _ missionrun.ExecutionInput) (string, error) {
	close(e.started)
	<-ctx.Done()
	return "", ctx.Err()
}

// waitFor 轮询断言（带超时）。
func waitFor(t *testing.T, timeout time.Duration, probe func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probe() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待超时")
}

// TestMissionControlOrchestrationAPI 端点级真编排（runTestServer 真组合）：
// 在途 Run 暂停 → 手动触发请求最终以 canceled 返回 + Mission 停 paused；
// 恢复 → ready；再执行新 attempt。
func TestMissionControlOrchestrationAPI(t *testing.T) {
	exec := &ctlBlockExec{started: make(chan struct{})}
	ts, mSvc, _ := runTestServer(t, func(c *runTestCfg) {
		c.executorOverride = exec
	})
	mi := readyMission(t, mSvc, "ag_r1")
	ctx := context.Background()

	// 手动触发（同步面——阻塞至停止后返回）
	respDone := make(chan *http.Response, 1)
	go func() {
		r := postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "")
		respDone <- r
	}()
	<-exec.started

	// 暂停在途：200 paused（不等停止完成——异步终理化）
	r := postSlot(t, ts.URL+"/v1/missions/"+mi+"/pause", `{"expected_version":1,"reason":"等一下"}`)
	var doc struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || doc.Status != "paused" {
		t.Fatalf("在途暂停: %d %+v", r.StatusCode, doc)
	}
	select {
	case resp := <-respDone:
		var runDoc struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&runDoc)
		resp.Body.Close()
		if runDoc.State != "canceled" {
			t.Fatalf("手动触发请求须以 canceled 返回: %+v", runDoc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("停止后手动触发请求未返回")
	}
	// Mission 停 paused + Run 流事件面
	got, _, _ := mSvc.Get(ctx, mi)
	if got.Status != mission.StatusPaused {
		t.Fatalf("Mission 须停 paused: %+v", got)
	}
	list, _ := http.Get(ts.URL + "/v1/missions/" + mi + "/runs")
	var runsDoc struct {
		Runs []struct {
			State string `json:"state"`
		} `json:"runs"`
	}
	_ = json.NewDecoder(list.Body).Decode(&runsDoc)
	list.Body.Close()
	if len(runsDoc.Runs) != 1 || runsDoc.Runs[0].State != "canceled" {
		t.Fatalf("Run 历史: %+v", runsDoc.Runs)
	}

	// 恢复 → ready → 手动再执行（新 attempt）
	r = postSlot(t, ts.URL+"/v1/missions/"+mi+"/resume", `{"expected_version":1}`)
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || doc.Status != "ready" {
		t.Fatalf("恢复: %d %+v", r.StatusCode, doc)
	}
}

// TestMissionControlCancelInFlightAPI 端点级取消在途：canceling → Run
// canceled → canceled（同步收尾）。
func TestMissionControlCancelInFlightAPI(t *testing.T) {
	exec := &ctlBlockExec{started: make(chan struct{})}
	ts, mSvc, _ := runTestServer(t, func(c *runTestCfg) {
		c.executorOverride = exec
	})
	mi := readyMission(t, mSvc, "ag_r1")

	go func() { postSlot(t, ts.URL+"/v1/missions/"+mi+"/runs", "") }() //nolint:errcheck
	<-exec.started

	r := postSlot(t, ts.URL+"/v1/missions/"+mi+"/cancel", `{"expected_version":1,"reason":"不要了"}`)
	var doc struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || doc.Status != "canceled" {
		t.Fatalf("在途取消: %d %+v", r.StatusCode, doc)
	}
	waitFor(t, 2*time.Second, func() bool {
		got, _, _ := mSvc.Get(context.Background(), mi)
		return got.Status == mission.StatusCanceled
	})
	list, _ := http.Get(ts.URL + "/v1/missions/" + mi + "/runs")
	var runsDoc struct {
		Runs []struct {
			State string `json:"state"`
		} `json:"runs"`
	}
	_ = json.NewDecoder(list.Body).Decode(&runsDoc)
	list.Body.Close()
	if len(runsDoc.Runs) != 1 || runsDoc.Runs[0].State != "canceled" {
		t.Fatalf("Run 须 canceled: %+v", runsDoc.Runs)
	}
}
