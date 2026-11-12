// forget_api_test.go —— 0.7.8 / ADR-0045：Forget 端点 + 包生命周期删除端点 IT
// （TDD 红段先行——报告组合/错误族/activated 409）。
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/bundle"
	"github.com/sisibeloved/Mosaic/internal/memory"
	"github.com/sisibeloved/Mosaic/internal/room"
)

// newForgetTestServer Forget 编排闭包（真 memory 服务 + 真导出包扫描）+
// 既有导入测试装配（导出/导入服务同数据目录）。
func newForgetTestServer(t *testing.T) (*httptest.Server, string, *memory.MemStore) {
	t.Helper()
	dir := t.TempDir()
	store := memory.NewMemStore()
	memSvc := memory.NewService(store, memory.Anchors{}, fixedMemNow)
	fp := &fakeBundlePorts{
		agents: map[string]bundle.AgentFacts{"ag_ok": {AgentID: "ag_ok", DisplayName: "小助", Adapter: "codex", Status: "active"}},
	}
	exportPorts := fp.ports()
	// 记忆端口连真服务（导出须含刚创建的条目——包扫描断言前提）。
	exportPorts.Memories = func(ctx context.Context, ns, scopeID string) ([]bundle.MemoryFact, error) {
		res, err := memSvc.List(ctx, memory.ListFilter{
			Namespace: memory.Namespace(ns), ScopeID: scopeID, Status: memory.StatusActive,
			Limit: 100, IncludeExpired: true,
		})
		if err != nil {
			return nil, err
		}
		out := make([]bundle.MemoryFact, 0, len(res.Items))
		for _, it := range res.Items {
			out = append(out, bundle.MemoryFact{
				MemoryID: it.MemoryID, Version: it.Version, Namespace: string(it.Namespace),
				ScopeID: it.ScopeID, Kind: string(it.Kind), Title: it.Title, Content: it.Content,
				SourceRefs: it.SourceRefs, ObservedAt: it.ObservedAt, ValidFrom: it.ValidFrom, ValidTo: it.ValidTo,
				Confidence: it.Confidence, Status: string(it.Status), Visibility: it.Visibility,
				Sensitivity: string(it.Sensitivity), Supersedes: it.Supersedes,
				CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
			})
		}
		return out, nil
	}
	bundleSvc := bundle.New(exportPorts, dir, "0.7.8-dev",
		bundle.WithNow(func() string { return "2026-11-12T12:00:00Z" }),
		bundle.WithNewID(func() string { return "bdl_f01" }))
	importer := bundle.NewImporter(fakeBundleImportPorts{}.ports(), dir, "0.7.8-dev",
		bundle.WithImporterNow(func() string { return "2026-11-12T12:00:00Z" }),
		bundle.WithImporterNewID(func() string { return "bdi_f01" }))
	handler := New(Deps{
		MemorySVC:       memSvc,
		BundleSVC:       bundleSvc,
		BundleImportSVC: importer,
		MemoryForget: func(ctx context.Context, memoryID, note string) (memory.DeletionReport, error) {
			// 编排：墓碑化（域）+ 导出/导入包扫描（bundle 域）+ 报告组装
			//（审校 #4：本机导入 zip 同为受管理副本）。
			gone, err := memSvc.Forget(ctx, memoryID, note)
			if err != nil {
				return memory.DeletionReport{}, err
			}
			contained, err := bundleSvc.ExportsContaining(ctx, []string{memoryID, "mem_src_tail"})
			if err != nil {
				return memory.DeletionReport{}, err
			}
			imports, err := importer.ImportsContaining(ctx, []string{memoryID})
			if err != nil {
				return memory.DeletionReport{}, err
			}
			return memory.DeletionReportOf(gone, contained, imports), nil
		},
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: "tok",
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, dir, store
}

func TestForgetEndpoint(t *testing.T) {
	ts, dir, _ := newForgetTestServer(t)

	// 造记忆 → 导出包含它的包 → Forget 报告列托管包。
	it := memPost(t, ts.URL+"/v1/memory/items", "tok", map[string]any{
		"namespace": "agent", "scope_id": "ag_ok", "kind": "semantic",
		"title": "配方", "content": "隐私正文", "observed_at": "2026-11-01T00:00:00Z",
	})
	if it.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", it.StatusCode)
	}
	var created map[string]any
	_ = json.NewDecoder(it.Body).Decode(&created)
	memID := created["memory_id"].(string)
	if r := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", map[string]any{}); r.StatusCode != http.StatusCreated {
		t.Fatalf("export: %d", r.StatusCode)
	}
	zipBytes, err := os.ReadFile(filepath.Join(dir, "bundles", "bdl_f01", "mosaic-agent-bundle.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if r := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", zipBytes); r.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", r.StatusCode)
	}

	resp := memPost(t, ts.URL+"/v1/memory/items/"+memID+"/forget", "tok", map[string]any{"note": "隐私清除"})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("forget: %d %s", resp.StatusCode, body)
	}
	var rep map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep["memory_id"] != memID {
		t.Fatalf("报告: %v", rep)
	}
	bundles, _ := rep["contained_in_bundles"].([]any)
	if len(bundles) != 1 || bundles[0] != "bdl_f01" {
		t.Fatalf("托管包清单: %v", bundles)
	}
	imports, _ := rep["contained_in_imports"].([]any)
	if len(imports) != 1 || imports[0] != "bdi_f01" {
		t.Fatalf("本机导入 zip 清单（审校 #4——受管理副本不漏报）: %v", imports)
	}
	ooa, _ := rep["out_of_reach"].([]any)
	if len(ooa) == 0 {
		t.Fatalf("不可达注记: %v", rep)
	}

	// 404 / 401。
	if r := memPost(t, ts.URL+"/v1/memory/items/mem_ghost/forget", "tok", map[string]any{}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("404: %d", r.StatusCode)
	}
	if r := memPost(t, ts.URL+"/v1/memory/items/"+memID+"/forget", "", map[string]any{}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("401: %d", r.StatusCode)
	}
}

func TestBundleDeleteEndpoints(t *testing.T) {
	ts, dir, _ := newForgetTestServer(t)
	if r := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", map[string]any{}); r.StatusCode != http.StatusCreated {
		t.Fatalf("export: %d", r.StatusCode)
	}
	zipBytes, err := os.ReadFile(filepath.Join(dir, "bundles", "bdl_f01", "mosaic-agent-bundle.zip"))
	if err != nil {
		t.Fatal(err)
	}

	// 导出包删除：200 + 目录清除 + 404 复删。
	dresp, err := doDelete(ts.URL+"/v1/bundle-exports/bdl_f01", "tok")
	if err != nil || dresp.StatusCode != http.StatusOK {
		t.Fatalf("delete export: %v %d", err, dresp.StatusCode)
	}
	t.Cleanup(func() { dresp.Body.Close() })
	if _, err := os.Stat(filepath.Join(dir, "bundles", "bdl_f01")); !os.IsNotExist(err) {
		t.Fatalf("目录须清除")
	}
	if r, _ := doDelete(ts.URL+"/v1/bundle-exports/bdl_f01", "tok"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("复删 404: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 导入记录删除：inactive 200；再导入激活后 409。
	if r := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", zipBytes); r.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", r.StatusCode)
	}
	iresp, err := doDelete(ts.URL+"/v1/bundle-imports/bdi_f01", "tok")
	if err != nil || iresp.StatusCode != http.StatusOK {
		t.Fatalf("delete import: %v %d", err, iresp.StatusCode)
	}
	t.Cleanup(func() { iresp.Body.Close() })
}

func TestBundleDeleteActivatedConflict(t *testing.T) {
	ts, dir, _ := newForgetTestServer(t)
	if r := bundlePost(t, ts.URL+"/v1/agents/ag_ok/bundle-exports", "tok", map[string]any{}); r.StatusCode != http.StatusCreated {
		t.Fatalf("export: %d", r.StatusCode)
	}
	zipBytes, _ := os.ReadFile(filepath.Join(dir, "bundles", "bdl_f01", "mosaic-agent-bundle.zip"))
	if r := bundleUpload(t, ts.URL+"/v1/bundle-imports", "tok", zipBytes); r.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", r.StatusCode)
	}
	// 手工置 activated（激活端点依赖激活面装配——此处直测删除门语义）。
	imp := bundle.NewImporter(fakeBundleImportPorts{}.ports(), dir, "0.7.8-dev",
		bundle.WithImporterNow(func() string { return "2026-11-12T12:00:00Z" }),
		bundle.WithImporterNewID(func() string { return "bdi_f01" }))
	view, err := imp.Get("bdi_f01")
	if err != nil {
		t.Fatal(err)
	}
	view.Status = "activated"
	view.Activation = &bundle.ActivationView{ImportID: "bdi_f01", Status: "activated"}
	// 手工写边车（httpapi 包不可触 bundle 私有方法——IT 直写文件）。
	raw, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle-imports", "bdi_f01", "import.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, _ := doDelete(ts.URL+"/v1/bundle-imports/bdi_f01", "tok"); r.StatusCode != http.StatusConflict {
		t.Fatalf("activated 409: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

func doDelete(url, token string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	req.Header.Set("X-Owner-Token", token)
	return http.DefaultClient.Do(req)
}
