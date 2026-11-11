// bundle_import_api_test.go —— 0.7.6 / ADR-0043：Bundle 导入预检三端点
// （导出→上传→报告的端到端流 + 门/冲突/装配语义）。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/bundle"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func newBundleImportTestServer(t *testing.T, ownerToken string, withSvc bool) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	fp := &fakeBundlePorts{
		agents: map[string]bundle.AgentFacts{"ag_ok": {
			AgentID: "ag_ok", DisplayName: "小助", Adapter: "codex", Status: "active",
		}},
		missions: map[string][]bundle.MissionFact{"ag_ok": {{
			MissionID: "mi_a", ContractVersion: 1, Status: "running", OwnerAgentID: "ag_ok",
		}}},
		memories: map[string][]bundle.MemoryFact{
			"agent|ag_ok": {
				{MemoryID: "mem_n", Namespace: "agent", ScopeID: "ag_ok", Status: "active", Sensitivity: "normal"},
			},
		},
	}
	deps := Deps{
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: ownerToken,
	}
	if withSvc {
		deps.BundleSVC = bundle.New(fp.ports(), dir, "0.7.5-dev",
			bundle.WithNow(func() string { return "2026-11-08T12:00:00Z" }),
			bundle.WithNewID(func() string { return "bdl_api01" }))
		deps.BundleImportSVC = bundle.NewImporter(fakeBundleImportPorts{}.ports(), dir, "0.7.6-dev",
			bundle.WithImporterNow(func() string { return "2026-11-10T12:00:00Z" }),
			bundle.WithImporterNewID(func() string { return "bdi_it01" }))
	}
	handler := New(deps)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, dir
}

// fakeBundleImportPorts IT 对照桩：实例无同 ID Agent/任务；无已注册可执行项。
type fakeBundleImportPorts struct{}

func (f fakeBundleImportPorts) ports() bundle.ImportPorts {
	return bundle.ImportPorts{
		AgentExists: func(_ context.Context, _ string) (bool, error) { return false, nil },
		MissionState: func(_ context.Context, _ string) (bundle.MissionState, bool, error) {
			return bundle.MissionState{}, false, nil
		},
		Harnesses: func(context.Context) ([]string, error) { return nil, nil },
	}
}

// bundleUpload multipart 上传（字段 file）。
// errorCode 读错误信封的 code（{"error": {code, message}}）。
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e.Error.Code
}

// bundleUpload multipart 上传（字段 file）。
func bundleUpload(t *testing.T, url, token string, zipBytes []byte) *http.Response {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "mosaic-agent-bundle.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestBundleImportEndpoints(t *testing.T) {
	ts, dir := newBundleImportTestServer(t, "tok", true)

	// 先经真实导出面产包（端到端：导出 → 上传导入）。
	resp := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("export: %d", resp.StatusCode)
	}
	zipBytes, err := os.ReadFile(filepath.Join(dir, "bundles", "bdl_api01", "mosaic-agent-bundle.zip"))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}

	// 上传 → 201：status 恒 inactive、verdict eligible、检查面自足。
	uresp := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", zipBytes)
	if uresp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(uresp.Body)
		t.Fatalf("upload: %d %s", uresp.StatusCode, body)
	}
	var iv map[string]any
	if err := json.NewDecoder(uresp.Body).Decode(&iv); err != nil {
		t.Fatal(err)
	}
	if iv["import_id"] != "bdi_it01" || iv["bundle_id"] != "bdl_api01" || iv["agent_id"] != "ag_ok" {
		t.Fatalf("标识: %v", iv)
	}
	if iv["status"] != "inactive" || iv["verdict"] != "eligible_with_warnings" {
		t.Fatalf("status/verdict: %v / %v", iv["status"], iv["verdict"])
	}
	// 夹具无检查点——recovery_limits warn 是预期恢复限制（非阻断）。
	if blockers, _ := iv["blockers"].([]any); len(blockers) != 0 {
		t.Fatalf("blockers: %v", blockers)
	}
	checks, _ := iv["checks"].([]any)
	if len(checks) < 10 {
		t.Fatalf("checks 过少: %d", len(checks))
	}
	src, _ := iv["source"].(map[string]any)
	if src["format"] != bundle.Format {
		t.Fatalf("source: %v", src)
	}

	// 列表（实例级）。
	lresp, err := bundleGet(ts.URL+"/v1/bundle-imports", "tok")
	if err != nil || lresp.StatusCode != http.StatusOK {
		t.Fatalf("list: %v %d", err, lresp.StatusCode)
	}
	t.Cleanup(func() { lresp.Body.Close() })
	var lv map[string]any
	_ = json.NewDecoder(lresp.Body).Decode(&lv)
	imports, _ := lv["imports"].([]any)
	if len(imports) != 1 {
		t.Fatalf("imports: %v", imports)
	}

	// 详情。
	dresp, err := bundleGet(ts.URL+"/v1/bundle-imports/bdi_it01", "tok")
	if err != nil || dresp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %v %d", err, dresp.StatusCode)
	}
	t.Cleanup(func() { dresp.Body.Close() })
	var dv map[string]any
	_ = json.NewDecoder(dresp.Body).Decode(&dv)
	if dv["bundle_id"] != "bdl_api01" {
		t.Fatalf("detail: %v", dv["bundle_id"])
	}
}

func TestBundleImportGates(t *testing.T) {
	ts, dir := newBundleImportTestServer(t, "tok", true)

	// 非 zip：422 bundle_unsafe（安全门整体拒绝）。
	resp := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", []byte("这不是 zip"))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unsafe: %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "bundle_unsafe" {
		t.Fatalf("code: %v", code)
	}

	// 缺 file 字段：400。
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormField("notfile")
	_, _ = fw.Write([]byte("x"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/bundle-imports", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Owner-Token", "tok")
	nresp, err := http.DefaultClient.Do(req)
	if err != nil || nresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("file_field_required: %v %d", err, nresp.StatusCode)
	}
	nresp.Body.Close()

	// 非 multipart：400。
	jresp := bundlePost(t, ts.URL+"/v1/bundle-imports", "tok", map[string]any{})
	if jresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("multipart_required: %d", jresp.StatusCode)
	}

	// 无 token：401。
	zipBytes := exportForImport(t, ts, dir)
	uresp := bundleUpload(t, ts.URL+"/v1/bundle-imports", "", zipBytes)
	if uresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token 门: %d", uresp.StatusCode)
	}

	// 同源重复导入：409（指向既有记录）。
	if r := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", zipBytes); r.StatusCode != http.StatusCreated {
		t.Fatalf("首次导入: %d", r.StatusCode)
	}
	r2 := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", zipBytes)
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("重复导入: %d", r2.StatusCode)
	}
	if code := errorCode(t, r2); code != "bundle_already_imported" {
		t.Fatalf("code: %v", code)
	}

	// 详情不存在。
	if r, err := bundleGet(ts.URL+"/v1/bundle-imports/bdi_nope", "tok"); err != nil || r.StatusCode != http.StatusNotFound {
		t.Fatalf("detail 404: %v %d", err, r.StatusCode)
	} else {
		r.Body.Close()
	}
}

// exportForImport 经导出端点产包并读回字节。
func exportForImport(t *testing.T, ts *httptest.Server, dir string) []byte {
	t.Helper()
	resp := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("export: %d", resp.StatusCode)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bundles", "bdl_api01", "mosaic-agent-bundle.zip"))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	return raw
}

func TestBundleImportUnavailable(t *testing.T) {
	ts, _ := newBundleImportTestServer(t, "tok", false)
	resp := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", []byte("x"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未装配 503: %d", resp.StatusCode)
	}
}
