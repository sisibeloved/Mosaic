package httpapi

// 0.6.3 本地运行时端点（ADR-0026）：同步发现、环境 CRUD（目录占用/校验）、
// 准备（默认环境/换绑/失败保持绑定）、探测转移（unhealthy 恒 200）、事件流、
// 404/400/409 族、未装配 503。
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mosaicruntime "github.com/sisibeloved/Mosaic/internal/runtime"
)

type epProber struct {
	result mosaicruntime.ProbeResult
	err    error
}

func (f *epProber) Probe(_ context.Context, _ mosaicruntime.ProbeTarget) (mosaicruntime.ProbeResult, error) {
	return f.result, f.err
}

type epFS struct{ err error }

func (f *epFS) EnsureDir(string) error { return f.err }

func localRuntimeTestServer(t *testing.T) (*httptest.Server, *epProber, *mosaicruntime.Service) {
	t.Helper()
	prober := &epProber{result: mosaicruntime.ProbeResult{OK: true, Version: "1.2.3", Login: "logged_in"}}
	var n int64
	svc := mosaicruntime.New(mosaicruntime.NewMemStore(), prober, &epFS{},
		mosaicruntime.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		mosaicruntime.WithNewIDs(func() string { n++; return "rtx_api" + itoa(n) },
			func() string { n++; return "env_api" + itoa(n) }),
		mosaicruntime.WithDiscoverySource(func(_ context.Context) ([]mosaicruntime.DiscoveredEntry, error) {
			return []mosaicruntime.DiscoveredEntry{{
				Adapter: "codex", Host: mosaicruntime.HostNative, Path: "/usr/local/bin/codex",
				Version: "1.0.0", Login: "logged_in",
			}}, nil
		}))
	ts := httptest.NewServer(New(Deps{Runtimes: svc}))
	t.Cleanup(ts.Close)
	return ts, prober, svc
}

// TestRuntimeEndpoints 全链：同步 → 列表 → 环境（默认/占用/校验）→
// 准备与探测转移 → 事件流 → 错误族。
func TestRuntimeEndpoints(t *testing.T) {
	ts, prober, svc := localRuntimeTestServer(t)

	// 同步发现
	resp := postSlot(t, ts.URL+"/v1/runtimes/sync", "")
	var syncDoc struct {
		Created   int `json:"created"`
		Refreshed int `json:"refreshed"`
	}
	decodeBody(t, resp, &syncDoc)
	if syncDoc.Created != 1 || syncDoc.Refreshed != 0 {
		t.Fatalf("同步计数错误: %+v", syncDoc)
	}
	// 再同步幂等
	resp = postSlot(t, ts.URL+"/v1/runtimes/sync", "")
	decodeBody(t, resp, &syncDoc)
	if syncDoc.Created != 0 || syncDoc.Refreshed != 1 {
		t.Fatalf("重同步计数错误: %+v", syncDoc)
	}

	// 列表
	resp, _ = http.Get(ts.URL + "/v1/runtimes")
	var listDoc struct {
		Runtimes []struct {
			RuntimeID string `json:"runtime_id"`
			State     string `json:"state"`
			Version   string `json:"version"`
		} `json:"runtimes"`
	}
	decodeBody(t, resp, &listDoc)
	if len(listDoc.Runtimes) != 1 || listDoc.Runtimes[0].State != "discovered" {
		t.Fatalf("列表错误: %+v", listDoc.Runtimes)
	}
	rtID := listDoc.Runtimes[0].RuntimeID

	// 无默认环境准备 → 404 environment_not_found
	resp = postSlot(t, ts.URL+"/v1/runtimes/"+rtID+"/prepare", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "environment_not_found" {
		t.Fatalf("无默认环境期望 404/environment_not_found，得到 %d/%s", resp.StatusCode, code)
	}

	// 建默认环境（空体名 → 400；占用 → 409；正常 → 201）
	resp = postSlot(t, ts.URL+"/v1/environments", `{"name":"","workspace_dir":"/tmp/ws-api"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusBadRequest || code != "invalid_environment" {
		t.Fatalf("空名期望 400/invalid_environment，得到 %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/environments", `{"name":"甲","workspace_dir":"/tmp/ws-api"}`)
	var envDoc struct {
		EnvironmentID string `json:"environment_id"`
		IsDefault     bool   `json:"is_default"`
	}
	decodeBody(t, resp, &envDoc)
	if resp.StatusCode != http.StatusCreated || envDoc.IsDefault {
		t.Fatalf("建环境（HTTP 面非默认——默认由装配确保）: %d %+v", resp.StatusCode, envDoc)
	}
	// 默认环境经装配面确保（启动语义等价）
	defEnv, err := svc.EnsureDefaultEnvironment(context.Background(), "默认工作区", "/tmp/ws-default")
	if err != nil {
		t.Fatalf("确保默认环境: %v", err)
	}
	resp = postSlot(t, ts.URL+"/v1/environments", `{"name":"乙","workspace_dir":"/tmp/ws-api"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusConflict || code != "workspace_dir_taken" {
		t.Fatalf("目录占用期望 409/workspace_dir_taken，得到 %d/%s", resp.StatusCode, code)
	}

	// 准备（默认环境缺省）→ ready
	resp = postSlot(t, ts.URL+"/v1/runtimes/"+rtID+"/prepare", "")
	var rtDoc struct {
		State          string `json:"state"`
		EnvironmentID  string `json:"environment_id"`
		Version        string `json:"version"`
		LastHealthNote string `json:"last_health_note"`
	}
	decodeBody(t, resp, &rtDoc)
	if rtDoc.State != "ready" || rtDoc.EnvironmentID != defEnv.EnvironmentID || rtDoc.Version != "1.2.3" {
		t.Fatalf("准备（缺省默认环境）: %+v", rtDoc)
	}

	// 探测失败 → unhealthy 恒 200 携 note
	prober.result = mosaicruntime.ProbeResult{OK: false, Login: "unknown", Note: "version_probe_failed"}
	resp = postSlot(t, ts.URL+"/v1/runtimes/"+rtID+"/health-check", "")
	decodeBody(t, resp, &rtDoc)
	if resp.StatusCode != http.StatusOK || rtDoc.State != "unhealthy" || rtDoc.LastHealthNote != "version_probe_failed" {
		t.Fatalf("探测失败应 200 unhealthy: %d %+v", resp.StatusCode, rtDoc)
	}
	// 探测恢复（已绑环境）→ ready
	prober.result = mosaicruntime.ProbeResult{OK: true, Version: "1.3.0", Login: "logged_in"}
	resp = postSlot(t, ts.URL+"/v1/runtimes/"+rtID+"/health-check", "")
	decodeBody(t, resp, &rtDoc)
	if rtDoc.State != "ready" || rtDoc.Version != "1.3.0" {
		t.Fatalf("恢复应 ready: %+v", rtDoc)
	}

	// 事件流
	resp, _ = http.Get(ts.URL + "/v1/runtimes/" + rtID + "/events")
	var evDoc struct {
		Events []struct {
			EventType string         `json:"event_type"`
			Payload   map[string]any `json:"payload"`
		} `json:"events"`
	}
	decodeBody(t, resp, &evDoc)
	if len(evDoc.Events) != 7 { // discovered + prepare(health+state) + 失败(health+state) + 恢复(health+state)
		t.Fatalf("事件数错误: %d %+v", len(evDoc.Events), evDoc.Events)
	}

	// 详情 + 环境列表/详情 + 404 族
	resp, _ = http.Get(ts.URL + "/v1/runtimes/" + rtID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("详情: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/v1/environments")
	var envList struct {
		Environments []struct {
			EnvironmentID string `json:"environment_id"`
			IsDefault     bool   `json:"is_default"`
		} `json:"environments"`
	}
	decodeBody(t, resp, &envList)
	if len(envList.Environments) != 2 || !envList.Environments[0].IsDefault {
		t.Fatalf("环境列表（默认在前）: %+v", envList)
	}
	resp, _ = http.Get(ts.URL + "/v1/environments/" + envDoc.EnvironmentID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("环境详情: %d", resp.StatusCode)
	}
	for path, want := range map[string]string{
		"/v1/runtimes/rtx_none":        "runtime_not_found",
		"/v1/runtimes/rtx_none/events": "runtime_not_found",
		"/v1/environments/env_none":    "environment_not_found",
	} {
		resp, _ = http.Get(ts.URL + path)
		if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != want {
			t.Fatalf("%s 期望 404/%s，得到 %d/%s", path, want, resp.StatusCode, code)
		}
	}
	resp = postSlot(t, ts.URL+"/v1/runtimes/rtx_none/prepare", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "runtime_not_found" {
		t.Fatalf("未知运行时准备期望 404，得到 %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/runtimes/"+rtID+"/prepare", `{"environment_id":"env_none"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "environment_not_found" {
		t.Fatalf("未知环境准备期望 404，得到 %d/%s", resp.StatusCode, code)
	}
}

// TestRuntimeUnavailable 未装配 → 503 runtime_unavailable。
func TestRuntimeUnavailable(t *testing.T) {
	ts := httptest.NewServer(New(Deps{}))
	t.Cleanup(ts.Close)
	for _, path := range []string{"/v1/runtimes", "/v1/runtimes/rtx_x",
		"/v1/runtimes/rtx_x/events", "/v1/environments", "/v1/environments/env_x"} {
		resp, _ := http.Get(ts.URL + path)
		if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "runtime_unavailable" {
			t.Fatalf("%s 期望 503/runtime_unavailable，得到 %d/%s", path, resp.StatusCode, code)
		}
	}
	for _, path := range []string{"/v1/runtimes/sync", "/v1/runtimes/rtx_x/health-check", "/v1/runtimes/rtx_x/prepare"} {
		resp := postSlot(t, ts.URL+path, "")
		if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "runtime_unavailable" {
			t.Fatalf("%s 期望 503/runtime_unavailable，得到 %d/%s", path, resp.StatusCode, code)
		}
	}
	resp := postSlot(t, ts.URL+"/v1/environments", `{"name":"n","workspace_dir":"/tmp/x"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "runtime_unavailable" {
		t.Fatalf("建环境期望 503，得到 %d/%s", resp.StatusCode, code)
	}
}

// decodeBody 单次解码（响应体只可读一次）。
func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
}
