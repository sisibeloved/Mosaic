// forget_bundle_test.go —— 0.7.8 / ADR-0045：删除传播与旧包防复活的域级测试
// （TDD 红段先行——包扫描/包删除/预检墓碑 finding/激活防复活）。
package bundle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportsContaining 托管包扫描（ADR-0045 §2）：含该条目的导出包清单。
func TestExportsContaining(t *testing.T) {
	fp := exportFixture()
	_, dir := newTestService(t, fp)
	view, err := (New(fp.ports(), dir, "0.7.8-dev", WithNow(fixedNow), WithNewID(func() string { return "bdl_t01" }))).
		Export(context.Background(), "ag_ok", ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(fp.ports(), dir, "0.7.8-dev", WithNow(fixedNow))
	got, err := svc.ExportsContaining(context.Background(), []string{"mem_n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != view.BundleID {
		t.Fatalf("含 mem_n 的包: %v", got)
	}
	if got, _ := svc.ExportsContaining(context.Background(), []string{"mem_ghost"}); len(got) != 0 {
		t.Fatalf("不含: %v", got)
	}
}

// TestDeleteExportLifecycle 导出包删除（ADR-0045 §4）：目录清除 + 幂等 404。
func TestDeleteExportLifecycle(t *testing.T) {
	fp := exportFixture()
	svc, dir := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "bundles", view.BundleID)
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	dv, err := svc.DeleteExport(context.Background(), view.BundleID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if dv.BundleID != view.BundleID || len(dv.DeletedFiles) == 0 || len(dv.OutOfReach) == 0 {
		t.Fatalf("视图: %+v", dv)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("目录须清除: %v", err)
	}
	if _, err := svc.DeleteExport(context.Background(), view.BundleID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("再删 404: %v", err)
	}
	if _, err := svc.DeleteExport(context.Background(), "../evil"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("白名单: %v", err)
	}
}

// TestImportDeleteLifecycle 导入记录删除：inactive 可删（含 attempt 清理）；
// activated → 409（激活产物是独立域状态——撤销激活不在本件）。
func TestImportDeleteLifecycle(t *testing.T) {
	raw := mustExport(t)
	imp, dir := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// attempt 留痕文件预置（删除须一并清理）。
	if err := os.WriteFile(filepath.Join(dir, importDirName, "bdi_t01", attemptName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	dv, err := imp.Delete("bdi_t01")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if dv.ImportID != "bdi_t01" || len(dv.DeletedFiles) == 0 {
		t.Fatalf("视图: %+v", dv)
	}
	if _, err := os.Stat(filepath.Join(dir, importDirName, "bdi_t01")); !os.IsNotExist(err) {
		t.Fatalf("目录须清除: %v", err)
	}
	if _, err := imp.Delete("bdi_t01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("再删 404: %v", err)
	}
	// 已激活记录删除 → 409。
	imp2, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp2.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	view, err := imp2.Get("bdi_t01")
	if err != nil {
		t.Fatal(err)
	}
	view.Status = "activated"
	view.Activation = &ActivationView{ImportID: "bdi_t01", Status: "activated"}
	if err := imp2.writeSidecar("bdi_t01", view); err != nil {
		t.Fatal(err)
	}
	if _, err := imp2.Delete("bdi_t01"); !errors.Is(err, ErrConflict) {
		t.Fatalf("activated 409: %v", err)
	}
}

// TestPrecheckTombstoneHits 预检墓碑 finding（ADR-0045 §3）：包内记忆命中
// 本实例墓碑 → tombstone_hits warn（列 ID——激活时将拒绝复活）。
func TestPrecheckTombstoneHits(t *testing.T) {
	raw := mustExport(t)
	ip := emptyImportPorts()
	ip.tombstones = []string{"mem_n"} // mem_n 已 Forget
	imp, _ := newImporterForTest(t, ip)
	iv, err := imp.Import(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	f := finding(t, iv, "tombstone_hits")
	if f.Result != "warn" || !strings.Contains(f.Detail, "mem_n") {
		t.Fatalf("tombstone_hits: %+v", f)
	}
	if iv.Verdict != verdictWarning {
		t.Fatalf("verdict: %s", iv.Verdict)
	}
	// 未命中 → pass。
	imp2, _ := newImporterForTest(t, emptyImportPorts())
	iv2, err := imp2.Import(context.Background(), bytes.NewReader(mustExport(t)))
	if err != nil {
		t.Fatal(err)
	}
	if f := finding(t, iv2, "tombstone_hits"); f.Result != "pass" {
		t.Fatalf("未命中 pass: %+v", f)
	}
}

// TestActivateTombstoneSkip 激活防复活（ADR-0045 §3）：两把键（包内原 ID /
// 激活 origin 标记）命中墓碑 → 跳过不复活；未命中正常写。
func TestActivateTombstoneSkip(t *testing.T) {
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// 命中包内原 ID（mem_n 已 Forget）。
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	ports.tombstoneKeys = []string{"mem_n"}
	view, err := (NewActivation(imp, ports.ports())).Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if view.Counts.MemoriesCreated != 2 || view.Counts.MemoriesSkipped != 1 {
		t.Fatalf("墓碑命中跳过: %+v", view.Counts)
	}
	for _, id := range ports.memWrites {
		if id == "mem_n" {
			t.Fatalf("已删正文不得复活: %v", ports.memWrites)
		}
	}
	// 命中激活 origin 标记（bundle:bdl_x:mem_w——上次激活产物已 Forget）。
	imp2, _ := newImporterForTest(t, emptyImportPorts())
	raw2 := mustExport(t)
	if _, err := imp2.Import(context.Background(), bytes.NewReader(raw2)); err != nil {
		t.Fatal(err)
	}
	ports2 := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	ports2.tombstoneKeys = []string{"bundle:bdl_test01:mem_w"}
	view2, err := (NewActivation(imp2, ports2.ports())).Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if view2.Counts.MemoriesSkipped != 1 {
		t.Fatalf("origin 键命中跳过: %+v", view2.Counts)
	}
}

// TestActivateCrossBundleTombstone 0.7.8 审校 #2：不同导出包绕过墓碑的反例
// ——包内条目携带来源标记（bundle:ANOTHER:mem_w，稳定身份尾段 mem_w），
// 本实例墓碑以不同 bundle_id 标记同源记忆（bundle:OLD:mem_w）→ 尾段归一
// 命中，激活跳过不复活。
func TestActivateCrossBundleTombstone(t *testing.T) {
	fp := activationFixture()
	mem := fp.memories["workspace|default"][0]
	mem.SourceRefs = []string{"bundle:bdl_ANOTHER:mem_w"} // 上一跳来源标记（跨导出稳定）
	fp.memories["workspace|default"] = []MemoryFact{mem}
	raw, _ := exportZip(t, fp, ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	ports.tombstoneKeys = []string{"bundle:bdl_OLD:mem_w"} // 墓碑侧：不同 bundle_id 同源
	view, err := (NewActivation(imp, ports.ports())).Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if view.Counts.MemoriesSkipped != 1 {
		t.Fatalf("跨包同源墓碑须命中跳过: %+v", view.Counts)
	}
	for _, id := range ports.memWrites {
		if id == "mem_w" {
			t.Fatalf("已删正文不得经另一份包复活: %v", ports.memWrites)
		}
	}
}
