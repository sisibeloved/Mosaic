package httpapi

// 0.6.4 租约端点（ADR-0027）：领取/互斥 409 族/续租/释放/围栏校验四关/
// 列表过滤/404 族/未装配 503。
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
	mosaicruntime "github.com/sisibeloved/Mosaic/internal/runtime"
)

type epMissions struct{ states map[string]string }

func (f epMissions) MissionState(_ context.Context, id string) (string, bool, error) {
	st, ok := f.states[id]
	return st, ok, nil
}

// leaseTestServer：就绪运行时 + 主体端口（mi_ok ready / mi_cx canceled）。
func leaseTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	prober := &epProber{result: mosaicruntime.ProbeResult{OK: true, Version: "1.0.0", Login: "logged_in"}}
	var n int64
	svc := mosaicruntime.New(mosaicruntime.NewMemStore(), prober, &epFS{},
		mosaicruntime.WithNow(func() string { return "2026-10-30T12:00:00.000000000Z" }),
		mosaicruntime.WithNewIDs(func() string { n++; return "rtx_api" + itoa(n) },
			func() string { n++; return "env_api" + itoa(n) }),
		mosaicruntime.WithNewLeaseID(func() string { n++; return "lse_api" + itoa(n) }),
		mosaicruntime.WithNewFenceToken(func() string { n++; return "fnc_api" + itoa(n) }),
		mosaicruntime.WithMissions(epMissions{states: map[string]string{
			"mi_ok": mission.StatusReady, "mi_cx": mission.StatusCanceled,
		}}),
		mosaicruntime.WithDiscoverySource(func(_ context.Context) ([]mosaicruntime.DiscoveredEntry, error) {
			return []mosaicruntime.DiscoveredEntry{{Adapter: "codex", Host: mosaicruntime.HostNative,
				Path: "/bin/codex", Version: "1.0.0", Login: "logged_in"}}, nil
		}))
	ctx := context.Background()
	if _, _, err := svc.SyncFromSource(ctx); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if _, err := svc.EnsureDefaultEnvironment(ctx, "默认", "/tmp/lease-api-ws"); err != nil {
		t.Fatalf("默认环境: %v", err)
	}
	list, _ := svc.ListRuntimes(ctx)
	rt, err := svc.Prepare(ctx, list[0].RuntimeID, "")
	if err != nil || rt.State != mosaicruntime.StateReady {
		t.Fatalf("准备: %+v %v", rt, err)
	}
	ts := httptest.NewServer(New(Deps{Runtimes: svc}))
	t.Cleanup(ts.Close)
	return ts, rt.RuntimeID
}

func TestLeaseEndpoints(t *testing.T) {
	ts, rtID := leaseTestServer(t)

	// 领取 → 201（代次 1、围栏令牌、缺省限额）
	resp := postSlot(t, ts.URL+"/v1/runtime-leases", `{"runtime_id":"`+rtID+`","mission_id":"mi_ok"}`)
	var l struct {
		LeaseID    string         `json:"lease_id"`
		Generation int            `json:"generation"`
		FenceToken string         `json:"fence_token"`
		State      string         `json:"state"`
		Workspace  string         `json:"workspace_ref"`
		Limits     map[string]int `json:"resource_limits"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("领取期望 201，得到 %d", resp.StatusCode)
	}
	decodeBody(t, resp, &l)
	if l.State != "active" || l.Generation != 1 || l.FenceToken == "" || l.Limits["concurrency"] != 1 {
		t.Fatalf("租约事实: %+v", l)
	}

	// 互斥 409 族 + 前置 404/409 族
	resp = postSlot(t, ts.URL+"/v1/runtime-leases", `{"runtime_id":"`+rtID+`","mission_id":"mi_ok"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusConflict || code != "lease_conflict_runtime" {
		t.Fatalf("容量互斥: %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/runtime-leases", `{"runtime_id":"rtx_none","mission_id":"mi_ok"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "runtime_not_found" {
		t.Fatalf("未知运行时: %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/runtime-leases", `{"runtime_id":"`+rtID+`","mission_id":"mi_none"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "mission_not_found" {
		t.Fatalf("未知主体: %d/%s", resp.StatusCode, code)
	}
	// TTL 界限 400；拼错字段 400
	resp = postSlot(t, ts.URL+"/v1/runtime-leases", `{"runtime_id":"`+rtID+`","mission_id":"mi_ok","ttl_seconds":30}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusBadRequest || code != "invalid_lease" {
		t.Fatalf("TTL 下界: %d/%s", resp.StatusCode, code)
	}

	// 围栏校验：有效 → 错令牌
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/"+l.LeaseID+"/check-fence", `{"fence_token":"`+l.FenceToken+`"}`)
	var fc struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason"`
	}
	decodeBody(t, resp, &fc)
	if !fc.Valid || fc.Reason != "" {
		t.Fatalf("有效围栏: %+v", fc)
	}
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/"+l.LeaseID+"/check-fence", `{"fence_token":"fnc_wrong"}`)
	decodeBody(t, resp, &fc)
	if fc.Valid || fc.Reason != "fence_token_mismatch" {
		t.Fatalf("错令牌: %+v", fc)
	}

	// 续租（空体缺省 TTL）→ 200
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/"+l.LeaseID+"/renew", "")
	var rn struct {
		State string `json:"state"`
	}
	decodeBody(t, resp, &rn)
	if resp.StatusCode != http.StatusOK || rn.State != "active" {
		t.Fatalf("续租: %d %+v", resp.StatusCode, rn)
	}

	// 释放 → released；重复释放/续租 409；围栏 lease_not_active
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/"+l.LeaseID+"/release", "")
	var rel struct {
		State      string `json:"state"`
		ReleasedAt string `json:"released_at"`
	}
	decodeBody(t, resp, &rel)
	if rel.State != "released" || rel.ReleasedAt == "" {
		t.Fatalf("释放: %+v", rel)
	}
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/"+l.LeaseID+"/release", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusConflict || code != "lease_inactive" {
		t.Fatalf("重复释放: %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/"+l.LeaseID+"/renew", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusConflict || code != "lease_inactive" {
		t.Fatalf("释放后续租: %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/"+l.LeaseID+"/check-fence", `{"fence_token":"`+l.FenceToken+`"}`)
	decodeBody(t, resp, &fc)
	if fc.Valid || fc.Reason != "lease_not_active" {
		t.Fatalf("释放后围栏: %+v", fc)
	}

	// 再领取（代次 2）+ 列表过滤 + 详情 + 404 族
	resp = postSlot(t, ts.URL+"/v1/runtime-leases", `{"runtime_id":"`+rtID+`","mission_id":"mi_ok","ttl_seconds":120}`)
	decodeBody(t, resp, &l)
	if l.Generation != 2 {
		t.Fatalf("再领代次应 2: %+v", l)
	}
	resp, _ = http.Get(ts.URL + "/v1/runtime-leases?state=active")
	var listDoc struct {
		Leases []struct {
			LeaseID string `json:"lease_id"`
		} `json:"leases"`
	}
	decodeBody(t, resp, &listDoc)
	if len(listDoc.Leases) != 1 || listDoc.Leases[0].LeaseID != l.LeaseID {
		t.Fatalf("活跃过滤: %+v", listDoc)
	}
	resp, _ = http.Get(ts.URL + "/v1/runtime-leases/" + l.LeaseID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("详情: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/v1/runtime-leases/lse_none")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "lease_not_found" {
		t.Fatalf("未知租约: %d/%s", resp.StatusCode, code)
	}
	resp = postSlot(t, ts.URL+"/v1/runtime-leases/lse_none/release", "")
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusNotFound || code != "lease_not_found" {
		t.Fatalf("未知租约释放: %d/%s", resp.StatusCode, code)
	}
}

// TestLeaseUnavailable 未装配 → 503 runtime_unavailable。
func TestLeaseUnavailable(t *testing.T) {
	ts := httptest.NewServer(New(Deps{}))
	t.Cleanup(ts.Close)
	resp := postSlot(t, ts.URL+"/v1/runtime-leases", `{"runtime_id":"rtx_x","mission_id":"mi_x"}`)
	if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "runtime_unavailable" {
		t.Fatalf("领取期望 503，得到 %d/%s", resp.StatusCode, code)
	}
	for _, path := range []string{"/v1/runtime-leases", "/v1/runtime-leases/lse_x"} {
		resp, _ := http.Get(ts.URL + path)
		if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "runtime_unavailable" {
			t.Fatalf("%s 期望 503，得到 %d/%s", path, resp.StatusCode, code)
		}
	}
	for _, path := range []string{"/v1/runtime-leases/lse_x/renew", "/v1/runtime-leases/lse_x/release"} {
		resp := postSlot(t, ts.URL+path, "")
		if code, _ := errOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || code != "runtime_unavailable" {
			t.Fatalf("%s 期望 503，得到 %d/%s", path, resp.StatusCode, code)
		}
	}
}

// 保留 decodeBody 引用（定义在 runtime_api_test.go）。
var _ = json.NewDecoder
