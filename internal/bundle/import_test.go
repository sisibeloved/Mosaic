// import_test.go —— 0.7.6 / ADR-0043：导入预检的域级测试（安全门族/兼容族/导出往返）。
package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixedLaterNow() string { return "2026-11-10T12:00:00Z" }

// fakeImportPorts 对照端口桩（本实例事实；*Err = 端口失败注入——一审 #6）。
type fakeImportPorts struct {
	agents       map[string]bool
	missions     map[string]MissionState
	harnesses    []string
	agentsErr    error
	harnessesErr error
	tombstones   []string // 墓碑命中注入（0.7.8 预检 finding 测试）
}

func (f *fakeImportPorts) ports() ImportPorts {
	return ImportPorts{
		AgentExists: func(_ context.Context, agentID string) (bool, error) {
			if f.agentsErr != nil {
				return false, f.agentsErr
			}
			return f.agents[agentID], nil
		},
		MissionState: func(_ context.Context, missionID string) (MissionState, bool, error) {
			st, ok := f.missions[missionID]
			return st, ok, nil
		},
		Harnesses: func(context.Context) ([]string, error) {
			if f.harnessesErr != nil {
				return nil, f.harnessesErr
			}
			return f.harnesses, nil
		},
		MemoryTombstoneHits: func(_ context.Context, keys []string) ([]string, error) {
			set := map[string]bool{}
			for _, t := range f.tombstones {
				set[t] = true
			}
			var out []string
			for _, k := range keys {
				if set[k] {
					out = append(out, k)
				}
			}
			return out, nil
		},
	}
}

// exportFixture 标准导出夹具：ag_ok + mi_a（在办，带检查点）+ agent/workspace 记忆。
func exportFixture() *fakePorts {
	fp := &fakePorts{
		agents:   map[string]AgentFacts{"ag_ok": agentA()},
		missions: map[string][]MissionFact{"ag_ok": {missionFact("mi_a", "running")}},
		ckpts: map[string]CheckpointFact{"mi_a": {
			CheckpointID: "ck_1", RunID: "run_1", MissionID: "mi_a", MissionVersion: 2,
			CheckpointVersion: 3, Source: "", CompletedSteps: []string{"s1"}, PendingSteps: []string{"s2"},
			CreatedAt: "2026-11-08T10:00:00Z",
		}},
		memories: map[string][]MemoryFact{
			"agent|ag_ok":       {memItem("mem_n", "agent", "ag_ok", "active", "normal", "")},
			"workspace|default": {memItem("mem_w", "workspace", "default", "active", "normal", "")},
		},
	}
	return fp
}

func emptyImportPorts() *fakeImportPorts {
	return &fakeImportPorts{agents: map[string]bool{}, missions: map[string]MissionState{}, harnesses: []string{"exe_1"}}
}

// exportZip 用导出 Service 产出一个合法包字节流。
func exportZip(t *testing.T, fp *fakePorts, req ScopeRequest) ([]byte, View) {
	t.Helper()
	svc, dir := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", req)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bundles", view.BundleID, zipName))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	return raw, view
}

func mustExport(t *testing.T) []byte {
	t.Helper()
	raw, _ := exportZip(t, exportFixture(), ScopeRequest{})
	return raw
}

// newImporterForTest 标准导入服务（固定 ID/时钟；caps 可覆写）。
func newImporterForTest(t *testing.T, ip *fakeImportPorts) (*ImportService, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewImporter(ip.ports(), dir, "0.7.6-dev",
		WithImporterNow(fixedLaterNow), WithImporterNewID(func() string { return "bdi_t01" }))
	return s, dir
}

// zipEntry 可定制条目（模式/声明尺寸——门族注入用）。
type zipEntry struct {
	name     string
	data     []byte
	mode     fs.FileMode
	declared uint64 // 非 0 时以 CreateRaw 落声明尺寸（writer 不覆写）
}

// unpackZip 解包为 map。
func unpackZip(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry: %v", err)
		}
		data := &bytes.Buffer{}
		if _, err := data.ReadFrom(rc); err != nil {
			t.Fatalf("read entry: %v", err)
		}
		rc.Close()
		out[f.Name] = data.Bytes()
	}
	return out
}

// buildZipEntries 按序打包（允许重名/恶意名/伪造声明——门族注入）。
func buildZipEntries(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		if e.declared != 0 {
			h.UncompressedSize64 = e.declared
			h.CompressedSize64 = uint64(len(e.data))
			w, err := zw.CreateRaw(h)
			if err != nil {
				t.Fatalf("raw create %s: %v", e.name, err)
			}
			if _, err := w.Write(e.data); err != nil {
				t.Fatalf("raw write %s: %v", e.name, err)
			}
			continue
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("create %s: %v", e.name, err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// buildZip map 形态重打包（名排序）。
func buildZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	entries := make([]zipEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, zipEntry{name: n, data: files[n]})
	}
	return buildZipEntries(t, entries)
}

// repackManifest 内容文件变更后重算 manifest.files 摘要（让门族越过 digest 打到后续门）。
func repackManifest(t *testing.T, files map[string][]byte) {
	t.Helper()
	var man importManifest
	if err := decodeStrict(files["manifest.json"], &man); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	man.Files = man.Files[:0]
	for _, p := range contentFileOrder {
		data := files[p]
		sum := sha256.Sum256(data)
		man.Files = append(man.Files, FileEntry{Path: p, Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	files["manifest.json"] = append(raw, '\n')
}

// ---- 导出 → 导入往返 ----

func TestImportRoundTrip(t *testing.T) {
	fp := exportFixture()
	raw, view := exportZip(t, fp, ScopeRequest{})
	imp, dir := newImporterForTest(t, emptyImportPorts())

	iv, err := imp.Import(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if iv.ImportID != "bdi_t01" || iv.BundleID != view.BundleID || iv.AgentID != "ag_ok" {
		t.Fatalf("标识: %+v", iv)
	}
	if iv.Status != "inactive" {
		t.Fatalf("status 须恒 inactive: %s", iv.Status)
	}
	if iv.Verdict != verdictEligib {
		t.Fatalf("verdict: %s（checks: %v）", iv.Verdict, iv.Checks)
	}
	if len(iv.Blockers) != 0 {
		t.Fatalf("blockers: %v", iv.Blockers)
	}
	// 计数与导出侧一致（agent+workspace 正常项 = 2）。
	if iv.Counts != (CountsView{Memories: 2, Missions: 1, Checkpoints: 1}) {
		t.Fatalf("counts: %+v（导出侧 %+v）", iv.Counts, view.Counts)
	}
	if iv.Source.Format != Format || iv.Source.ExportedAt != fixedNow() {
		t.Fatalf("source: %+v", iv.Source)
	}
	// 落盘：zip + 边车。
	if _, err := os.Stat(filepath.Join(dir, importDirName, "bdi_t01", zipName)); err != nil {
		t.Fatalf("zip 落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, importDirName, "bdi_t01", importSidecarName)); err != nil {
		t.Fatalf("导入边车须为 import.json: %v", err)
	}
	got, err := imp.Get("bdi_t01")
	if err != nil || got.BundleID != view.BundleID {
		t.Fatalf("get: %v %+v", err, got)
	}
	list, err := imp.List()
	if err != nil || len(list) != 1 || list[0].Verdict != verdictEligib || list[0].Status != "inactive" {
		t.Fatalf("list: %v %+v", err, list)
	}
	// 检查面自足：安全门三行 + 兼容 findings 全在。
	keys := map[string]bool{}
	for _, c := range iv.Checks {
		keys[c.Check] = true
	}
	for _, want := range []string{
		"zip_structure", "integrity", "reference_closure", "source_version",
		"identity_conflict", "mission_conflict", "harness_availability", "expired_in_transit",
		"sensitive_payload", "recovery_limits", "room_source_unbound", "alias_projection",
	} {
		if !keys[want] {
			t.Fatalf("缺少检查项 %s（有 %v）", want, keys)
		}
	}
}

func TestImportNotAZip(t *testing.T) {
	imp, _ := newImporterForTest(t, &fakeImportPorts{})
	_, err := imp.Import(context.Background(), strings.NewReader("这不是 zip"))
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "zip_corrupt") {
		t.Fatalf("err: %v", err)
	}
}

// ---- 安全门族（改包注入）----

func TestImportSafetyGates(t *testing.T) {
	base := mustExport(t)
	// mutate：改内容 map（必要时 repack 摘要）→ 重打包；extra = 追加条目。
	mutate := func(t *testing.T, fn func(t *testing.T, files map[string][]byte) []zipEntry) []byte {
		t.Helper()
		files := unpackZip(t, base)
		extra := fn(t, files)
		entries := make([]zipEntry, 0, len(files)+1)
		for n, d := range files {
			entries = append(entries, zipEntry{name: n, data: d})
		}
		return buildZipEntries(t, append(entries, extra...))
	}
	cases := []struct {
		name     string
		mutate   func(t *testing.T, files map[string][]byte) []zipEntry
		contains string
	}{
		{"未知文件", func(t *testing.T, f map[string][]byte) []zipEntry {
			return []zipEntry{{name: "extra.txt", data: []byte("x")}}
		}, "未知文件"},
		{"缺少文件", func(t *testing.T, f map[string][]byte) []zipEntry {
			delete(f, "redaction-report.json")
			return nil
		}, "缺少必需文件"},
		{"路径穿越", func(t *testing.T, f map[string][]byte) []zipEntry {
			return []zipEntry{{name: "../evil.txt", data: []byte("x")}}
		}, "evil"},
		{"绝对路径", func(t *testing.T, f map[string][]byte) []zipEntry {
			return []zipEntry{{name: "/etc/passwd", data: []byte("x")}}
		}, "path"},
		{"反斜杠", func(t *testing.T, f map[string][]byte) []zipEntry {
			return []zipEntry{{name: "a\\b.txt", data: []byte("x")}}
		}, "path"},
		{"重复条目", func(t *testing.T, f map[string][]byte) []zipEntry {
			return []zipEntry{{name: "dependencies.json", data: f["dependencies.json"]}}
		}, "重复条目"},
		{"摘要不符", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["memory/items.ndjson"] = append(append([]byte{}, f["memory/items.ndjson"]...), '\n')
			return nil
		}, "digest"},
		{"字节数不符", func(t *testing.T, f map[string][]byte) []zipEntry {
			var man importManifest
			if err := decodeStrict(f["manifest.json"], &man); err != nil {
				t.Fatal(err)
			}
			man.Files[0].Bytes++
			raw, _ := json.MarshalIndent(man, "", "  ")
			f["manifest.json"] = append(raw, '\n')
			return nil
		}, "字节数不符"},
		{"格式不受支持", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["manifest.json"] = bytes.ReplaceAll(f["manifest.json"], []byte(`"mosaic-agent-bundle/1"`), []byte(`"mosaic-agent-bundle/2"`))
			return nil
		}, "format"},
		{"manifest 未知字段", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["manifest.json"] = bytes.Replace(f["manifest.json"], []byte("{\n"), []byte("{\n  \"future_field\": 1,\n"), 1)
			return nil
		}, "format"},
		{"承诺非暂停", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["work/commitments.json"] = bytes.ReplaceAll(f["work/commitments.json"], []byte(`"paused"`), []byte(`"running"`))
			repackManifest(t, f)
			return nil
		}, "default_on_import"},
		{"检查点引用承诺外任务", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["work/checkpoints.ndjson"] = bytes.ReplaceAll(f["work/checkpoints.ndjson"], []byte("mi_a"), []byte("msn_ghost"))
			repackManifest(t, f)
			return nil
		}, "检查点引用承诺外任务"},
		{"记忆非 active", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["memory/items.ndjson"] = bytes.ReplaceAll(f["memory/items.ndjson"], []byte(`"status":"active"`), []byte(`"status":"proposed"`))
			repackManifest(t, f)
			return nil
		}, "非 active"},
		{"任务域记忆越界", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["memory/items.ndjson"] = bytes.ReplaceAll(f["memory/items.ndjson"], []byte(`"namespace":"agent","scope_id":"ag_ok"`), []byte(`"namespace":"mission","scope_id":"msn_ghost"`))
			repackManifest(t, f)
			return nil
		}, "承诺外任务"},
		{"承诺任务重复", func(t *testing.T, f map[string][]byte) []zipEntry {
			var cf commitmentsFile
			if err := decodeStrict(f["work/commitments.json"], &cf); err != nil {
				t.Fatal(err)
			}
			cf.Missions = append(cf.Missions, cf.Missions[0])
			raw, _ := json.MarshalIndent(cf, "", "  ")
			f["work/commitments.json"] = append(raw, '\n')
			repackManifest(t, f)
			return nil
		}, "任务重复"},
		{"身份不一致", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["agent/profile.json"] = bytes.ReplaceAll(f["agent/profile.json"], []byte(`"ag_ok"`), []byte(`"ag_other"`))
			repackManifest(t, f)
			return nil
		}, "agent_id 不一致"},
		{"检查点断链", func(t *testing.T, f map[string][]byte) []zipEntry {
			// 一审 #3：清空检查点但保留上下文/水位引用——断链包不得过闭包门。
			f["work/checkpoints.ndjson"] = nil
			repackManifest(t, f)
			return nil
		}, "上下文引用的检查点不在包内"},
		{"水位版本不符", func(t *testing.T, f map[string][]byte) []zipEntry {
			f["manifest.json"] = bytes.ReplaceAll(f["manifest.json"], []byte(`"checkpoint_version": 3`), []byte(`"checkpoint_version": 9`))
			return nil
		}, "水位声明检查点"},
		{"脱敏报告缺字段", func(t *testing.T, f map[string][]byte) []zipEntry {
			// 一审 #5：{} 可过严格解码（不查必需字段）——数组字段 null 会打崩消费端。
			f["redaction-report.json"] = []byte("{}")
			repackManifest(t, f)
			return nil
		}, "redaction-report.json"},
		{"依赖清单缺失", func(t *testing.T, f map[string][]byte) []zipEntry {
			// 二审 #1：清空 dependencies.harness（profile 仍有 exe_1）——
			// 清单与身份可执行项矛盾，不得静默缩圈绕过依赖对照。
			var df dependenciesFile
			if err := decodeStrict(f["dependencies.json"], &df); err != nil {
				t.Fatalf("夹具解析: %v", err)
			}
			df.Harness = nil
			raw, _ := json.MarshalIndent(df, "", "  ")
			f["dependencies.json"] = append(raw, '\n')
			repackManifest(t, f)
			return nil
		}, "依赖清单缺失身份可执行项"},
		{"依赖清单矛盾", func(t *testing.T, f map[string][]byte) []zipEntry {
			// 二审 #1 反向：依赖清单含 profile 外可执行项。
			var df dependenciesFile
			if err := decodeStrict(f["dependencies.json"], &df); err != nil {
				t.Skipf("夹具形态变化: %v", err)
			}
			for i := range df.Harness {
				df.Harness[i].ExecutableID = "exe_ghost"
			}
			raw, _ := json.MarshalIndent(df, "", "  ")
			f["dependencies.json"] = append(raw, '\n')
			repackManifest(t, f)
			return nil
		}, "依赖清单含身份外可执行项"},
		{"尾随内容", func(t *testing.T, f map[string][]byte) []zipEntry {
			// 二审 #2：More() 遇 ] 恒 false——合法 manifest 后追加 ] 与额外
			// 对象，json.Valid 判非法而原实现放行。
			f["manifest.json"] = append(append([]byte{}, f["manifest.json"]...), []byte(`]{"extra":1}`)...)
			return nil
		}, "format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mutate(t, tc.mutate)
			imp, _ := newImporterForTest(t, &fakeImportPorts{})
			_, err := imp.Import(context.Background(), bytes.NewReader(raw))
			if !errors.Is(err, ErrUnsafe) {
				t.Fatalf("须 ErrUnsafe: %v", err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("违规明细 %q 不含 %q", err.Error(), tc.contains)
			}
			// 拒绝不留痕。
			list, _ := imp.List()
			if len(list) != 0 {
				t.Fatalf("拒绝后不得留记录: %v", list)
			}
		})
	}
}

func TestImportSymlinkEntry(t *testing.T) {
	files := unpackZip(t, mustExport(t))
	entries := make([]zipEntry, 0, len(files))
	for n, d := range files {
		if n == "memory/items.ndjson" {
			entries = append(entries, zipEntry{name: n, data: d, mode: fs.ModeSymlink | 0o777})
			continue
		}
		entries = append(entries, zipEntry{name: n, data: d})
	}
	imp, _ := newImporterForTest(t, &fakeImportPorts{})
	_, err := imp.Import(context.Background(), bytes.NewReader(buildZipEntries(t, entries)))
	if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err: %v", err)
	}
}

// ---- 展开与大小上限（caps 收窄触发）----

func TestImportSizeAndExpansionCaps(t *testing.T) {
	raw := mustExport(t)

	t.Run("压缩体超上限", func(t *testing.T) {
		imp, _ := newImporterForTest(t, &fakeImportPorts{})
		imp.caps.zip = 64
		_, err := imp.Import(context.Background(), bytes.NewReader(raw))
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("条目数超上限", func(t *testing.T) {
		imp, _ := newImporterForTest(t, &fakeImportPorts{})
		imp.caps.entries = 8
		_, err := imp.Import(context.Background(), bytes.NewReader(raw))
		if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "条目数") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("声明展开总量超上限", func(t *testing.T) {
		imp, _ := newImporterForTest(t, &fakeImportPorts{})
		imp.caps.expanded = 100
		_, err := imp.Import(context.Background(), bytes.NewReader(raw))
		if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "声明展开总量") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("单文件声明超上限", func(t *testing.T) {
		imp, _ := newImporterForTest(t, &fakeImportPorts{})
		imp.caps.file = 10
		_, err := imp.Import(context.Background(), bytes.NewReader(raw))
		if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "单文件上限") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("ndjson 行数超上限", func(t *testing.T) {
		files := unpackZip(t, raw)
		line := files["memory/items.ndjson"]
		files["memory/items.ndjson"] = append(append([]byte{}, line...), line...) // 行数翻倍
		repackManifest(t, files)
		imp, _ := newImporterForTest(t, &fakeImportPorts{})
		imp.caps.lines = 2
		_, err := imp.Import(context.Background(), bytes.NewReader(buildZip(t, files)))
		if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "行数") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("声明尺寸伪造膨胀", func(t *testing.T) {
		files := unpackZip(t, raw)
		entries := make([]zipEntry, 0, len(files))
		for n, d := range files {
			e := zipEntry{name: n, data: d}
			if n == "memory/items.ndjson" {
				e.declared = 512 << 20 // CreateRaw 落声明 512MiB——声明门先行
			}
			entries = append(entries, e)
		}
		imp, _ := newImporterForTest(t, &fakeImportPorts{})
		_, err := imp.Import(context.Background(), bytes.NewReader(buildZipEntries(t, entries)))
		if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "上限") {
			t.Fatalf("err: %v", err)
		}
	})
}

// ---- 兼容族 ----

func TestImportCompatibility(t *testing.T) {
	t.Run("身份冲突 block", func(t *testing.T) {
		ip := emptyImportPorts()
		ip.agents["ag_ok"] = true
		imp, _ := newImporterForTest(t, ip)
		iv, err := imp.Import(context.Background(), bytes.NewReader(mustExport(t)))
		if err != nil {
			t.Fatal(err)
		}
		if iv.Verdict != verdictBlocked || len(iv.Blockers) != 1 || iv.Blockers[0] != "identity_conflict" {
			t.Fatalf("verdict=%s blockers=%v", iv.Verdict, iv.Blockers)
		}
	})
	t.Run("任务漂移 block", func(t *testing.T) {
		ip := emptyImportPorts()
		ip.missions["mi_a"] = MissionState{ContractVersion: 5, Status: "running"}
		imp, _ := newImporterForTest(t, ip)
		iv, err := imp.Import(context.Background(), bytes.NewReader(mustExport(t)))
		if err != nil {
			t.Fatal(err)
		}
		if iv.Verdict != verdictBlocked || !containsStr(iv.Blockers, "mission_conflict") {
			t.Fatalf("verdict=%s blockers=%v", iv.Verdict, iv.Blockers)
		}
	})
	t.Run("同版重复 warn", func(t *testing.T) {
		ip := emptyImportPorts()
		ip.missions["mi_a"] = MissionState{ContractVersion: 2, Status: "running"}
		imp, _ := newImporterForTest(t, ip)
		iv, err := imp.Import(context.Background(), bytes.NewReader(mustExport(t)))
		if err != nil {
			t.Fatal(err)
		}
		if iv.Verdict != verdictWarning || len(iv.Blockers) != 0 {
			t.Fatalf("verdict=%s blockers=%v", iv.Verdict, iv.Blockers)
		}
		if finding(t, iv, "mission_conflict").Result != "warn" {
			t.Fatalf("mission_conflict: %+v", finding(t, iv, "mission_conflict"))
		}
	})
	t.Run("依赖缺失 block", func(t *testing.T) {
		ip := emptyImportPorts()
		ip.harnesses = []string{}
		imp, _ := newImporterForTest(t, ip)
		iv, err := imp.Import(context.Background(), bytes.NewReader(mustExport(t)))
		if err != nil {
			t.Fatal(err)
		}
		if iv.Verdict != verdictBlocked || !containsStr(iv.Blockers, "harness_availability") {
			t.Fatalf("verdict=%s blockers=%v", iv.Verdict, iv.Blockers)
		}
		if !strings.Contains(finding(t, iv, "harness_availability").Detail, "exe_1") {
			t.Fatalf("明细须列出缺失项: %+v", finding(t, iv, "harness_availability"))
		}
	})
	t.Run("途中过期 warn", func(t *testing.T) {
		fp := exportFixture()
		fp.memories["agent|ag_ok"] = append(fp.memories["agent|ag_ok"],
			memItem("mem_exp", "agent", "ag_ok", "active", "normal", "2026-11-09T00:00:00Z")) // 导出时刻有效
		raw, _ := exportZip(t, fp, ScopeRequest{})
		imp, _ := newImporterForTest(t, emptyImportPorts())
		iv, err := imp.Import(context.Background(), bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		f := finding(t, iv, "expired_in_transit")
		if f.Result != "warn" || !strings.Contains(f.Detail, "mem_exp") {
			t.Fatalf("expired_in_transit: %+v", f)
		}
		if iv.Verdict != verdictWarning {
			t.Fatalf("verdict: %s", iv.Verdict)
		}
	})
	t.Run("敏感携带 warn", func(t *testing.T) {
		fp := exportFixture()
		fp.memories["agent|ag_ok"] = append(fp.memories["agent|ag_ok"],
			memItem("mem_s2", "agent", "ag_ok", "active", "sensitive", ""))
		raw, _ := exportZip(t, fp, ScopeRequest{IncludeSensitive: true})
		imp, _ := newImporterForTest(t, emptyImportPorts())
		iv, err := imp.Import(context.Background(), bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if f := finding(t, iv, "sensitive_payload"); f.Result != "warn" {
			t.Fatalf("sensitive_payload: %+v", f)
		}
	})
	t.Run("无检查点恢复限制 warn", func(t *testing.T) {
		fp := exportFixture()
		delete(fp.ckpts, "mi_a")
		raw, _ := exportZip(t, fp, ScopeRequest{})
		imp, _ := newImporterForTest(t, emptyImportPorts())
		iv, err := imp.Import(context.Background(), bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		f := finding(t, iv, "recovery_limits")
		if f.Result != "warn" || !strings.Contains(f.Detail, "mi_a") {
			t.Fatalf("recovery_limits: %+v", f)
		}
	})
	t.Run("无对照端口不阻断", func(t *testing.T) {
		dir := t.TempDir()
		imp := NewImporter(ImportPorts{}, dir, "0.7.6-dev",
			WithImporterNow(fixedLaterNow), WithImporterNewID(func() string { return "bdi_nil" }))
		iv, err := imp.Import(context.Background(), bytes.NewReader(mustExport(t)))
		if err != nil {
			t.Fatal(err)
		}
		if iv.Verdict != verdictEligib {
			t.Fatalf("verdict: %s（%v）", iv.Verdict, iv.Checks)
		}
	})
}

func finding(t *testing.T, iv ImportView, check string) CheckResult {
	t.Helper()
	for _, c := range iv.Checks {
		if c.Check == check {
			return c
		}
	}
	t.Fatalf("无检查项 %s", check)
	return CheckResult{}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- 幂等 / 查面 ----

func TestImportDuplicateRejected(t *testing.T) {
	raw := mustExport(t)
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	_, err := imp.Import(context.Background(), bytes.NewReader(raw))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "bdi_t01") {
		t.Fatalf("err: %v", err)
	}
	list, _ := imp.List()
	if len(list) != 1 {
		t.Fatalf("重复导入不得双记录: %v", list)
	}
}

func TestImportGetWhitelist(t *testing.T) {
	imp, _ := newImporterForTest(t, &fakeImportPorts{})
	if _, err := imp.Get("../evil"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err: %v", err)
	}
	if _, err := imp.Get("bdi_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err: %v", err)
	}
	if _, err := imp.List(); err != nil { // 空目录零记录不报错
		t.Fatalf("list: %v", err)
	}
}

// ---- 一审修复钉住（P1 + P2#2/#4/#6）----

// TestNDJSONCountNoAlloc P1：空行载荷的行计数/解析须零额外分配——
// bytes.Split 为每行分配切片头，1MiB 换行 ≈ 24MiB、近展开上限的空行
// 包可放大数 GiB 并绕过行数门。
func TestNDJSONCountNoAlloc(t *testing.T) {
	buf := bytes.Repeat([]byte("\n"), 1<<20)
	if n := ndjsonLineCount(buf); n != 0 {
		t.Fatalf("纯换行须计 0 行: %d", n)
	}
	if allocs := testing.AllocsPerRun(10, func() { ndjsonLineCount(buf) }); allocs != 0 {
		t.Fatalf("行计数须零分配: %v", allocs)
	}
	items, errs := parseNDJSON[MemoryFact](buf)
	if len(items) != 0 || len(errs) != 0 {
		t.Fatalf("空行解析: %v %v", items, errs)
	}
	// 混合载荷行为等价（行号/空行容忍不回归）。
	mixed := append(append([]byte{}, []byte("\n{\"memory_id\":\"m1\"}\n\n")...), []byte("{\"memory_id\":\"m2\"}\n")...)
	got, errs := parseNDJSON[MemoryFact](mixed)
	if len(errs) != 0 || len(got) != 2 || got[0].MemoryID != "m1" || got[1].MemoryID != "m2" {
		t.Fatalf("混合解析: %+v %v", got, errs)
	}
}

// TestImportConcurrentDuplicate 一审 #2：并发同源导入恰一成功一 409——
// 查重与登记须原子（锁语义对任意调度成立，无需窗口同步）。
func TestImportConcurrentDuplicate(t *testing.T) {
	raw := mustExport(t)
	imp, _ := newImporterForTest(t, emptyImportPorts())
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := imp.Import(context.Background(), bytes.NewReader(raw))
			results <- err
		}()
	}
	conflicts := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("只允许 ErrConflict: %v", err)
			}
			conflicts++
		}
	}
	if conflicts != 1 {
		t.Fatalf("须恰一次冲突（实际 %d）", conflicts)
	}
	list, _ := imp.List()
	if len(list) != 1 {
		t.Fatalf("重复导入不得双记录: %v", list)
	}
}

// TestImportSensitiveMismatch 一审 #4：敏感判定以实际载荷为准——载荷有
// 敏感而 scope 声明 false 时不得按声明放行为 pass。
func TestImportSensitiveMismatch(t *testing.T) {
	files := unpackZip(t, mustExport(t))
	files["memory/items.ndjson"] = bytes.ReplaceAll(files["memory/items.ndjson"],
		[]byte(`"sensitivity":"normal"`), []byte(`"sensitivity":"sensitive"`))
	repackManifest(t, files)
	imp, _ := newImporterForTest(t, emptyImportPorts())
	iv, err := imp.Import(context.Background(), bytes.NewReader(buildZip(t, files)))
	if err != nil {
		t.Fatalf("非阻断语义不变: %v", err)
	}
	f := finding(t, iv, "sensitive_payload")
	if f.Result != "warn" || !strings.Contains(f.Detail, "不一致") {
		t.Fatalf("sensitive_payload: %+v", f)
	}
}

// TestImportPortFailureBlocks 一审 #6：必要对照（身份/依赖）查询失败 =
// 必要证据缺失——block 且进 blockers，不得降级 warn。
func TestImportPortFailureBlocks(t *testing.T) {
	ip := emptyImportPorts()
	ip.agentsErr = errors.New("registry down")
	ip.harnessesErr = errors.New("registry down")
	imp, _ := newImporterForTest(t, ip)
	iv, err := imp.Import(context.Background(), bytes.NewReader(mustExport(t)))
	if err != nil {
		t.Fatal(err)
	}
	if iv.Verdict != verdictBlocked {
		t.Fatalf("verdict: %s", iv.Verdict)
	}
	if !containsStr(iv.Blockers, "identity_conflict") || !containsStr(iv.Blockers, "harness_availability") {
		t.Fatalf("blockers: %v", iv.Blockers)
	}
	if !strings.Contains(finding(t, iv, "identity_conflict").Detail, "重跑预检") {
		t.Fatalf("identity_conflict: %+v", finding(t, iv, "identity_conflict"))
	}
}
