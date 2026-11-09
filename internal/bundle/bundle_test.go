// bundle_test.go —— 0.7.5 / ADR-0042：范围选择、摘要与脱敏报告的域级测试。
package bundle

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixedNow() string { return "2026-11-08T12:00:00Z" }

// fakePorts 端口桩：map 驱动 + 查询留痕（授权口径断言用）。
type fakePorts struct {
	agents   map[string]AgentFacts
	slots    map[string][]SlotFact
	missions map[string][]MissionFact
	ckpts    map[string]CheckpointFact
	memories map[string][]MemoryFact // "ns|scope" → 全量（任意状态）
	queried  []string                // Memories 实际查询的 "ns|scope"（过滤先行断言）
}

func (f *fakePorts) Agent(_ context.Context, id string) (AgentFacts, bool, error) {
	a, ok := f.agents[id]
	return a, ok, nil
}

func (f *fakePorts) Slots(_ context.Context, id string) ([]SlotFact, error) {
	return f.slots[id], nil
}

func (f *fakePorts) Missions(_ context.Context, id string) ([]MissionFact, error) {
	return f.missions[id], nil
}

func (f *fakePorts) Checkpoint(_ context.Context, missionID string) (CheckpointFact, bool, error) {
	c, ok := f.ckpts[missionID]
	return c, ok, nil
}

func (f *fakePorts) Memories(_ context.Context, ns, scope string) ([]MemoryFact, error) {
	f.queried = append(f.queried, ns+"|"+scope)
	return f.memories[ns+"|"+scope], nil
}

func agentA() AgentFacts {
	return AgentFacts{
		AgentID: "ag_ok", DisplayName: "小助", Adapter: "codex",
		Status: "active", Source: "manual", CreatedAt: "2026-10-01T00:00:00Z",
		Aliases:     AliasFacts{BotID: "bot_x", ProfileIDs: []string{"codex::bot_x"}, ParticipantIDs: []string{"par_x"}},
		Executables: []ExecutableRef{{ExecutableID: "exe_1", Runtime: "wsl", Distro: "codex", Channel: "stable", Enabled: true}},
	}
}

func memItem(id, ns, scope, status, sens, validTo string) MemoryFact {
	return MemoryFact{
		MemoryID: id, Version: 1, Namespace: ns, ScopeID: scope, Kind: "semantic",
		Title: "t-" + id, Content: "c-" + id, Status: status, Sensitivity: sens,
		ValidTo: validTo, Confidence: 1, Visibility: "namespace", SourceRefs: []string{},
		CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-01T00:00:00Z",
	}
}

func missionFact(id, status string) MissionFact {
	return MissionFact{
		MissionID: id, ContractVersion: 2, Status: status, OwnerAgentID: "ag_ok",
		Requester: "par_owner", Contract: ContractFacts{Objective: "obj-" + id},
		CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-01T00:00:00Z",
	}
}

func newTestService(t *testing.T, fp *fakePorts) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc := New(fp.ports(), dir, "0.7.5-dev", WithNow(fixedNow), WithNewID(func() string { return "bdl_test01" }))
	return svc, dir
}

func (f *fakePorts) ports() Ports {
	return Ports{Agent: f.Agent, Slots: f.Slots, Missions: f.Missions, Checkpoint: f.Checkpoint, Memories: f.Memories}
}

// readZip 解包到 map[path]content。
func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "\\") {
			t.Fatalf("包内路径不合法（须相对 + / 分隔 + 无 ..）: %q", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = raw
	}
	return out
}

// ---- 范围选择 ----

func TestExportScopeValidation(t *testing.T) {
	fp := &fakePorts{agents: map[string]AgentFacts{"ag_ok": agentA()}}
	svc, _ := newTestService(t, fp)
	ctx := context.Background()

	cases := []struct {
		name string
		req  ScopeRequest
		want error
	}{
		{"未知命名空间", ScopeRequest{Namespaces: []string{"agent", "global"}}, ErrInvalid},
		{"重复命名空间", ScopeRequest{Namespaces: []string{"agent", "agent"}}, ErrInvalid},
		{"room 不开放", ScopeRequest{Namespaces: []string{"room"}}, ErrInvalid},
		{"slot 不开放", ScopeRequest{Namespaces: []string{"slot"}}, ErrInvalid},
		{"显式任务重复", ScopeRequest{MissionIDs: []string{"mi_a", "mi_a"}}, ErrInvalid},
		{"显式任务不存在", ScopeRequest{MissionIDs: []string{"mi_ghost"}}, ErrInvalid},
	}
	for _, tc := range cases {
		_, err := svc.Export(ctx, "ag_ok", tc.req)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v want %v", tc.name, err, tc.want)
		}
	}

	_, err := svc.Export(ctx, "ag_ghost", ScopeRequest{})
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("agent 404: got %v", err)
	}
}

func TestExportDefaultScopeAndAutoMissions(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		missions: map[string][]MissionFact{"ag_ok": {
			missionFact("mi_draft", "draft"),
			missionFact("mi_run", "running"),
			missionFact("mi_wait", "waiting"),
			missionFact("mi_done", "accepted"), // 终态——自动圈定不含
		}},
		memories: map[string][]MemoryFact{
			"agent|ag_ok":       {memItem("mem_priv", "agent", "ag_ok", "active", "normal", "")},
			"workspace|default": {memItem("mem_ws", "workspace", "default", "active", "normal", "")},
		},
	}
	svc, _ := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// 默认命名空间 = agent + workspace；mission 不在默认集。
	wantNS := []string{"agent", "workspace"}
	if fmt.Sprint(view.Scope.Namespaces) != fmt.Sprint(wantNS) {
		t.Fatalf("默认命名空间: got %v want %v", view.Scope.Namespaces, wantNS)
	}
	// 自动圈定 = 非终态 owned（draft 排除；accepted 终态排除）。
	if fmt.Sprint(view.Scope.MissionIDs) != fmt.Sprint([]string{"mi_run", "mi_wait"}) {
		t.Fatalf("自动任务圈定: got %v", view.Scope.MissionIDs)
	}
	if view.Scope.MissionSelection != "auto" {
		t.Fatalf("mission_selection: %s", view.Scope.MissionSelection)
	}
	// mission 命名空间不在范围 → mission scope 结构性不查询（过滤先行）。
	for _, q := range fp.queried {
		if strings.HasPrefix(q, "mission|") {
			t.Fatalf("mission 命名空间未被选择却被查询: %s", q)
		}
	}
	if view.Counts.Memories != 2 || view.Counts.Missions != 2 {
		t.Fatalf("counts: %+v", view.Counts)
	}
}

func TestExportExplicitMissionNotOwnedRejected(t *testing.T) {
	fp := &fakePorts{
		agents:   map[string]AgentFacts{"ag_ok": agentA(), "ag_b": agentA()},
		missions: map[string][]MissionFact{"ag_ok": {missionFact("mi_own", "ready")}},
	}
	svc, _ := newTestService(t, fp)
	// mi_b 属于 ag_b——对 ag_ok 的导出是越权选择，整体拒绝。
	if _, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{MissionIDs: []string{"mi_own", "mi_b"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非所属任务: got %v", err)
	}
	// 显式选择终态任务合法（归档场景）。
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{MissionIDs: []string{"mi_own"}})
	if err != nil {
		t.Fatal(err)
	}
	if view.Scope.MissionSelection != "explicit" || len(view.Scope.MissionIDs) != 1 {
		t.Fatalf("显式圈定: %+v", view.Scope)
	}
}

// ---- 脱敏与口径 ----

func TestExportSensitiveOmittedByDefault(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		memories: map[string][]MemoryFact{
			"agent|ag_ok": {
				memItem("mem_n1", "agent", "ag_ok", "active", "normal", ""),
				memItem("mem_s1", "agent", "ag_ok", "active", "sensitive", ""),
				memItem("mem_s2", "agent", "ag_ok", "active", "sensitive", ""),
			},
		},
	}
	svc, dir := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Redaction.OmittedSensitive.Count != 2 {
		t.Fatalf("敏感省略计数: %d", view.Redaction.OmittedSensitive.Count)
	}
	if fmt.Sprint(view.Redaction.OmittedSensitive.ItemIDs) != fmt.Sprint([]string{"mem_s1", "mem_s2"}) {
		t.Fatalf("敏感省略 ID: %v", view.Redaction.OmittedSensitive.ItemIDs)
	}
	if view.Counts.Memories != 1 {
		t.Fatalf("入包计数不含省略项: %+v", view.Counts)
	}
	zc := readZip(t, filepath.Join(dir, "bundles", "bdl_test01", zipName))
	if strings.Contains(string(zc["memory/items.ndjson"]), "mem_s1") {
		t.Fatal("敏感记忆默认不得入包")
	}
	// 报告只带 ID 不带内容/标题。
	rr, _ := json.Marshal(view.Redaction)
	if strings.Contains(string(rr), "t-mem_s1") || strings.Contains(string(rr), "c-mem_s1") {
		t.Fatal("脱敏报告不得泄敏感标题/内容")
	}
}

func TestExportSensitiveExplicitInclude(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		memories: map[string][]MemoryFact{
			"agent|ag_ok": {memItem("mem_s1", "agent", "ag_ok", "active", "sensitive", "")},
		},
	}
	svc, dir := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{IncludeSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if view.Counts.Memories != 1 || view.Redaction.OmittedSensitive.Count != 0 {
		t.Fatalf("显式携带: %+v", view.Counts)
	}
	zc := readZip(t, filepath.Join(dir, "bundles", "bdl_test01", zipName))
	if !strings.Contains(string(zc["memory/items.ndjson"]), "mem_s1") {
		t.Fatal("include_sensitive=true 应入包")
	}
}

func TestExportExpiryAndStatusRules(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		memories: map[string][]MemoryFact{
			"agent|ag_ok": {
				memItem("mem_ok", "agent", "ag_ok", "active", "normal", ""),
				memItem("mem_exp", "agent", "ag_ok", "active", "normal", "2026-11-08T11:00:00Z"),       // 早于 now
				memItem("mem_alive", "agent", "ag_ok", "active", "normal", "2026-11-08T12:00:00.900Z"), // 恰好含小数秒边界——晚于 now
				memItem("mem_prop", "agent", "ag_ok", "proposed", "normal", ""),
				memItem("mem_sup", "agent", "ag_ok", "superseded", "normal", ""),
			},
		},
	}
	svc, _ := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	r := view.Redaction.ExcludedByRule
	if r.Expired != 1 || r.Proposed != 1 || r.Superseded != 1 {
		t.Fatalf("口径排除计数: %+v", r)
	}
	if view.Counts.Memories != 2 { // mem_ok + mem_alive
		t.Fatalf("入包计数: %+v", view.Counts)
	}
}

func TestExportMissionMemoriesFollowSelection(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		missions: map[string][]MissionFact{"ag_ok": {
			missionFact("mi_a", "ready"), missionFact("mi_b", "ready"),
		}},
		memories: map[string][]MemoryFact{
			"mission|mi_a": {memItem("mem_ma", "mission", "mi_a", "active", "normal", "")},
			"mission|mi_b": {memItem("mem_mb", "mission", "mi_b", "active", "normal", "")},
		},
	}
	svc, _ := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok",
		ScopeRequest{Namespaces: []string{"mission"}, MissionIDs: []string{"mi_a"}})
	if err != nil {
		t.Fatal(err)
	}
	if view.Counts.Memories != 1 {
		t.Fatalf("mission 记忆只随选中任务: %+v", view.Counts)
	}
	for _, q := range fp.queried {
		if q == "mission|mi_b" {
			t.Fatal("未选任务的 mission 记忆被查询（过滤先行）")
		}
	}
}

// ---- 检查点与恢复限制 ----

func TestExportCheckpointSelectionAndLimits(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		missions: map[string][]MissionFact{"ag_ok": {
			missionFact("mi_a", "running"), missionFact("mi_b", "running"),
		}},
		ckpts: map[string]CheckpointFact{
			"mi_a": {CheckpointID: "ckp_a", RunID: "run_a", MissionID: "mi_a", MissionVersion: 2,
				CheckpointVersion: 3, Source: "system", ProgressSummary: "半程",
				CompletedSteps: []string{"s1"}, PendingSteps: []string{"s2"},
				ArtifactRefs: []string{"att_1234567890"}, SourceWatermarks: map[string]string{}, CreatedAt: "2026-11-08T10:00:00Z"},
		},
	}
	svc, dir := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Counts.Checkpoints != 1 {
		t.Fatalf("检查点计数: %+v", view.Counts)
	}
	if len(view.Redaction.RecoveryLimits) != 1 || !strings.Contains(view.Redaction.RecoveryLimits[0], "mi_b") {
		t.Fatalf("无检查点任务的恢复限制: %v", view.Redaction.RecoveryLimits)
	}
	zc := readZip(t, filepath.Join(dir, "bundles", "bdl_test01", zipName))
	if !strings.Contains(string(zc["work/checkpoints.ndjson"]), "ckp_a") {
		t.Fatal("检查点应入包")
	}
	var cm []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(zc["context/manifests.ndjson"])), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("manifests.ndjson 行不合法: %v", err)
		}
		cm = append(cm, m)
	}
	if len(cm) != 2 {
		t.Fatalf("context/manifests 每任务一条: %d", len(cm))
	}
}

// ---- 摘要与包布局 ----

func TestExportZipLayoutAndDigests(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		slots: map[string][]SlotFact{"ag_ok": {{
			BindingID: "bnd_1", SlotInstanceID: "slt_1", SlotSpecID: "spec_w", SlotSpecVersion: 2,
			Status: "active", Spec: SlotSpecFacts{Name: "写作", Responsibility: "负责成文",
				Deliverables: []string{"周报"}, Runtime: &RuntimeReqFacts{Required: []string{"fs.read"}, WorkspaceAccess: "read"}},
		}}},
		missions: map[string][]MissionFact{"ag_ok": {missionFact("mi_a", "running")}},
		ckpts:    map[string]CheckpointFact{"mi_a": {CheckpointID: "ckp_a", MissionID: "mi_a", MissionVersion: 2, CheckpointVersion: 1, SourceWatermarks: map[string]string{}}},
		memories: map[string][]MemoryFact{
			"agent|ag_ok":       {memItem("mem_1", "agent", "ag_ok", "active", "normal", "")},
			"workspace|default": {memItem("mem_2", "workspace", "default", "active", "normal", "")},
			"mission|mi_a":      {memItem("mem_3", "mission", "mi_a", "active", "normal", "")},
		},
	}
	svc, dir := newTestService(t, fp)
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{
		Namespaces: []string{"agent", "workspace", "mission"}, MissionIDs: []string{"mi_a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	zc := readZip(t, filepath.Join(dir, "bundles", "bdl_test01", zipName))
	wantFiles := []string{
		"agent/profile.json", "slots/specs.json", "memory/items.ndjson",
		"work/checkpoints.ndjson", "work/commitments.json", "context/manifests.ndjson",
		"dependencies.json", "redaction-report.json", "manifest.json",
	}
	if len(zc) != len(wantFiles) {
		t.Fatalf("包内文件集: got %d want %d (%v)", len(zc), len(wantFiles), keysOf(zc))
	}
	for _, wf := range wantFiles {
		if _, ok := zc[wf]; !ok {
			t.Fatalf("缺包内文件 %s（现有 %v）", wf, keysOf(zc))
		}
	}
	// ndjson 每行可解析 + 行序 = 查询序（agent → workspace → mission）。
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(zc["memory/items.ndjson"])), "\n") {
		var m MemoryFact
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("items.ndjson 行不合法: %v", err)
		}
		order = append(order, m.MemoryID)
	}
	if fmt.Sprint(order) != fmt.Sprint([]string{"mem_1", "mem_2", "mem_3"}) {
		t.Fatalf("记忆行序: %v", order)
	}
	// manifest：逐文件摘要与实际内容相符；manifest 自身不在 files 内。
	var man struct {
		Files []FileEntry `json:"files"`
	}
	if err := json.Unmarshal(zc["manifest.json"], &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Files) != len(wantFiles)-1 {
		t.Fatalf("manifest files 应为 8（不含自身）: %d", len(man.Files))
	}
	for _, fe := range man.Files {
		raw, ok := zc[fe.Path]
		if !ok {
			t.Fatalf("manifest 摘要指向不存在的文件: %s", fe.Path)
		}
		sum := sha256.Sum256(raw)
		if fe.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("摘要不符: %s", fe.Path)
		}
		if fe.Bytes != int64(len(raw)) {
			t.Fatalf("字节数不符: %s", fe.Path)
		}
	}
	// View.files 与 manifest.files 同源。
	if len(view.Files) != len(man.Files) {
		t.Fatalf("view files: %d vs manifest %d", len(view.Files), len(man.Files))
	}
	// 水位：任务级契约/检查点版本 + 命名空间计数。
	var manFull map[string]any
	_ = json.Unmarshal(zc["manifest.json"], &manFull)
	wm, _ := manFull["watermarks"].(map[string]any)
	if wm == nil {
		t.Fatal("manifest 缺 watermarks")
	}
	ms, _ := wm["missions"].([]any)
	if len(ms) != 1 {
		t.Fatalf("水位任务数: %v", ms)
	}
	mc, _ := wm["memory_counts"].(map[string]any)
	if mc["agent"].(float64) != 1 || mc["workspace"].(float64) != 1 || mc["mission"].(float64) != 1 {
		t.Fatalf("水位记忆计数: %v", mc)
	}
	// commitments：契约快照 + 导入默认暂停标记。
	if !strings.Contains(string(zc["work/commitments.json"]), `"mi_a"`) ||
		!strings.Contains(string(zc["work/commitments.json"]), "paused") {
		t.Fatal("commitments 应含任务契约与默认暂停标记")
	}
	// dependencies：harness + slot 运行要求、无凭据面。
	if !strings.Contains(string(zc["dependencies.json"]), "exe_1") ||
		!strings.Contains(string(zc["dependencies.json"]), "fs.read") {
		t.Fatal("dependencies 应含 harness 与 slot 运行要求")
	}
	// agent/profile.json：身份 + 别名 + 可执行引用。
	if !strings.Contains(string(zc["agent/profile.json"]), "bot_x") || !strings.Contains(string(zc["agent/profile.json"]), "exe_1") {
		t.Fatal("profile 应含别名与可执行项引用")
	}
}

// ---- 边车与读取面 ----

// 一审 #5：交接竞态的域级复核——wiring 摘要/详情两步读取间任务可能已交接
// （MissionFact.OwnerAgentID 是详情时刻事实）。自动圈定跳过非所属；显式圈定
// 拒绝（与"任务不存在或非当前负责"同语义——不静默缩圈）。
func TestExportOwnerRecheckAfterHandover(t *testing.T) {
	handed := missionFact("mi_gone", "running")
	handed.OwnerAgentID = "ag_other" // 详情时刻 owner 已变
	stays := missionFact("mi_keep", "running")
	fp := &fakePorts{
		agents:   map[string]AgentFacts{"ag_ok": agentA()},
		missions: map[string][]MissionFact{"ag_ok": {handed, stays}},
		memories: map[string][]MemoryFact{
			"mission|mi_gone": {memItem("mem_g", "mission", "mi_gone", "active", "normal", "")},
		},
	}
	svc, _ := newTestService(t, fp)

	// 自动圈定：已交接任务不入选（连带其 mission 记忆不被查询）。
	view, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(view.Scope.MissionIDs) != fmt.Sprint([]string{"mi_keep"}) {
		t.Fatalf("自动圈定应跳过已交接任务: %v", view.Scope.MissionIDs)
	}
	for _, q := range fp.queried {
		if q == "mission|mi_gone" {
			t.Fatal("已交接任务的 mission 记忆被查询")
		}
	}

	// 显式圈定：已交接任务整体拒绝。
	if _, err := svc.Export(context.Background(), "ag_ok",
		ScopeRequest{MissionIDs: []string{"mi_keep", "mi_gone"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("显式圈定已交接任务应拒绝: %v", err)
	}
}

func TestListAndGet(t *testing.T) {
	fp := &fakePorts{
		agents: map[string]AgentFacts{"ag_ok": agentA()},
		memories: map[string][]MemoryFact{
			"agent|ag_ok": {memItem("mem_1", "agent", "ag_ok", "active", "normal", "")},
		},
	}
	svc, dir := newTestService(t, fp)
	if _, err := svc.Export(context.Background(), "ag_ok", ScopeRequest{}); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List("ag_ok")
	if err != nil || len(list) != 1 || list[0].BundleID != "bdl_test01" {
		t.Fatalf("list: %v %v", list, err)
	}
	got, err := svc.Get("bdl_test01")
	if err != nil || got.BundleID != "bdl_test01" || got.Counts.Memories != 1 {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := svc.Get("bdl_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在: %v", err)
	}
	// 白名单拒绝（防穿越）。
	if _, err := svc.Get("../evil"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("路径穿越: %v", err)
	}
	// 边车在库外：bundles/<id>/export.json。
	if _, err := os.Stat(filepath.Join(dir, "bundles", "bdl_test01", sidecarName)); err != nil {
		t.Fatalf("边车: %v", err)
	}
	// zip 路径相对数据目录。
	if got.ZipPath != "bundles/bdl_test01/mosaic-agent-bundle.zip" {
		t.Fatalf("zip_path: %s", got.ZipPath)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
