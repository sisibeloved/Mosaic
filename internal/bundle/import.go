// import.go —— 0.7.6 / ADR-0043：Bundle 导入预检（RFC-0018 §8.2/§8.3）。
//
// 语义与边界：
//   - 安全校验（RFC"导入先检查"六项）：格式/大小/压缩展开上限/路径穿越/
//     符号链接/引用闭包——任一不过整体拒绝（ErrUnsafe），不留记录；
//   - 兼容性报告：对照本实例事实的分级 findings（pass/info/warn/block，
//     block = 激活前必须解决——0.7.7 消费 blockers[]）——非阻断；
//   - 默认 inactive（RFC §8.3）：导入产物 = 待核验只读副本（zip + 报告边车
//     落数据目录 bundle-imports/），不进库、不建域状态、不执行包内代码、
//     不启用 MCP/工具、不自动唤醒——零 migration（backup/0.7.5 同例）；
//   - 完整性 ≠ 可信：sha256 防损坏不防恶意（RFC §8.2"不等同于可信签名"）。
package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 导入面上限（ADR-0043 §2——包是纯文本事实，此余量足够且封死 zip 炸弹）。
const (
	MaxZipBytes     = 64 << 20  // 压缩体上限
	MaxExpandedByte = 256 << 20 // 展开总量上限
	MaxFileBytes    = 256 << 20 // 单文件上限
	MaxEntries      = 64        // 条目数上限（v1 恰 9 文件）
	MaxNDJSONLines  = 200_000   // 单 ndjson 文件行数上限
)

// 导入面常量。
const (
	importDirName     = "bundle-imports"
	importSidecarName = "import.json" // 导入边车（≠ 导出侧 export.json——同目录族不同生命周期）
	importStatus      = "inactive"    // 0.7.6 恒 inactive——激活归 0.7.7
	verdictEligib     = "eligible"
	verdictWarning    = "eligible_with_warnings"
	verdictBlocked    = "blocked"
)

var (
	// ErrUnsafe 安全门失败（聚合明细在错误串内）。
	ErrUnsafe = errors.New("bundle: unsafe package")
	// ErrTooLarge 超大小上限（压缩体）。
	ErrTooLarge = errors.New("bundle: package too large")
	// ErrConflict 同源包重复导入。
	ErrConflict = errors.New("bundle: already imported")
)

var importIDRe = regexp.MustCompile(`^bdi_[0-9a-z_]+$`)

// ---- 包内容模型（导入侧解析目标——严格解码，"不理解即拒绝"）----

// importManifest manifest.json 的导入侧形态（0.7.5 导出同构）。
type importManifest struct {
	Format         string             `json:"format"`
	BundleID       string             `json:"bundle_id"`
	Agent          manifestAgent      `json:"agent"`
	Scope          ScopeView          `json:"scope"`
	ExportedAt     string             `json:"exported_at"`
	ProductVersion string             `json:"product_version,omitempty"`
	Watermarks     manifestWatermarks `json:"watermarks"`
	Files          []FileEntry        `json:"files"`
}

// manifestAgent manifest.agent 投影（身份三元组事实）。
type manifestAgent struct {
	AgentID     string     `json:"agent_id"`
	DisplayName string     `json:"display_name"`
	Adapter     string     `json:"adapter"`
	Aliases     AliasFacts `json:"aliases"`
}

// manifestWatermarks 水位（任务级 version + 命名空间计数）。
type manifestWatermarks struct {
	Missions     []watermarkMission `json:"missions"`
	MemoryCounts map[string]int     `json:"memory_counts"`
}

// watermarkMission 任务级水位条目。
type watermarkMission struct {
	MissionID         string `json:"mission_id"`
	ContractVersion   int    `json:"contract_version"`
	CheckpointVersion int    `json:"checkpoint_version,omitempty"`
	CheckpointSource  string `json:"checkpoint_source,omitempty"`
}

// slotsFile slots/specs.json 形态。
type slotsFile struct {
	Bindings []SlotFact `json:"bindings"`
}

// commitmentsFile work/commitments.json 形态（default_on_import 恒 paused）。
type commitmentsFile struct {
	DefaultOnImport string        `json:"default_on_import"`
	Missions        []MissionFact `json:"missions"`
}

// contextEntry context/manifests.ndjson 行形态。
type contextEntry struct {
	MissionID       string           `json:"mission_id"`
	ContractVersion int              `json:"contract_version"`
	ArtifactRefs    []string         `json:"artifact_refs"`
	Checkpoint      *contextCkptRef  `json:"checkpoint"`
	RoomSource      *RoomSourceFacts `json:"room_source"`
}

// contextCkptRef 上下文清单里的检查点引用。
type contextCkptRef struct {
	CheckpointID      string `json:"checkpoint_id"`
	CheckpointVersion int    `json:"checkpoint_version"`
	Source            string `json:"source"`
}

// dependenciesFile dependencies.json 形态（harness 引用 + 槽位运行要求）。
type dependenciesFile struct {
	ProductVersion string          `json:"product_version"`
	Harness        []ExecutableRef `json:"harness"`
	Slots          []depSlotEntry  `json:"slots"`
	Notes          []string        `json:"notes"`
}

// depSlotEntry 依赖面的槽位条目（0.7.5 depsOfSlots 同构）。
type depSlotEntry struct {
	SlotSpecID      string   `json:"slot_spec_id"`
	Version         int      `json:"version"`
	BindingStatus   string   `json:"binding_status"`
	RuntimeRequired []string `json:"runtime_required,omitempty"`
	WorkspaceAccess string   `json:"workspace_access,omitempty"`
	ToolsRequired   []string `json:"tools_required,omitempty"`
}

// contentFiles 九文件精确集合（v1 固定布局——layout 门白名单）。
var bundleFiles = map[string]bool{
	"manifest.json":      true,
	"agent/profile.json": true, "slots/specs.json": true, "memory/items.ndjson": true,
	"work/checkpoints.ndjson": true, "work/commitments.json": true, "context/manifests.ndjson": true,
	"dependencies.json": true, "redaction-report.json": true,
}

// contentFileOrder 八个内容文件（manifest 之外——digest 门集合）。
var contentFileOrder = []string{
	"agent/profile.json", "slots/specs.json", "memory/items.ndjson",
	"work/checkpoints.ndjson", "work/commitments.json", "context/manifests.ndjson",
	"dependencies.json", "redaction-report.json",
}

// safeContent 安全门通过后的解析产物（兼容性检查输入）。
type safeContent struct {
	manifest     importManifest
	profile      AgentFacts
	memories     []MemoryFact
	ckpts        []CheckpointFact
	commitments  commitmentsFile
	ctxEntries   []contextEntry
	deps         dependenciesFile
	redaction    RedactionReport
	zipSizeBytes int64
}

// ---- 视图（JSON 形状即 API 契约形状）----

// CheckResult 单项检查结果（result: pass | info | warn | block）。
type CheckResult struct {
	Check  string `json:"check"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// ImportSource 包来源事实（格式/版本/导出时刻）。
type ImportSource struct {
	Format         string `json:"format"`
	ProductVersion string `json:"product_version"`
	ExportedAt     string `json:"exported_at"`
}

// ImportView 导入登记全量视图（创建响应/详情；边车 import.json 即其序列化）。
type ImportView struct {
	ImportID   string          `json:"import_id"`
	BundleID   string          `json:"bundle_id"`
	AgentID    string          `json:"agent_id"`
	Status     string          `json:"status"` // inactive | activated（0.7.7 激活后）
	Verdict    string          `json:"verdict"`
	Blockers   []string        `json:"blockers"`
	ImportedAt string          `json:"imported_at"`
	SizeBytes  int64           `json:"size_bytes"`
	ZipPath    string          `json:"zip_path"`
	Source     ImportSource    `json:"source"`
	Scope      ScopeView       `json:"scope"`
	Counts     CountsView      `json:"counts"`
	Checks     []CheckResult   `json:"checks"`
	Redaction  RedactionReport `json:"redaction"`
	Activation *ActivationView `json:"activation,omitempty"` // 0.7.7 激活结果（ID 映射一等公民）
}

// ImportSummary 导入摘要（列表元素）。
type ImportSummary struct {
	ImportID   string     `json:"import_id"`
	BundleID   string     `json:"bundle_id"`
	AgentID    string     `json:"agent_id"`
	Status     string     `json:"status"`
	Verdict    string     `json:"verdict"`
	ImportedAt string     `json:"imported_at"`
	SizeBytes  int64      `json:"size_bytes"`
	Counts     CountsView `json:"counts"`
}

// ---- 服务 ----

// MissionState 本实例任务对照事实（漂移判定）。
type MissionState struct {
	ContractVersion int
	Status          string
}

// ImportPorts 导入对照端口（本实例事实——app 闭包映射；nil 端口 = 无该项检查）。
type ImportPorts struct {
	AgentExists  func(ctx context.Context, agentID string) (bool, error)
	MissionState func(ctx context.Context, missionID string) (MissionState, bool, error)
	Harnesses    func(ctx context.Context) ([]string, error) // 本实例可执行项 portable 标识集
	// MemoryTombstoneHits 墓碑命中核对（0.7.8 / ADR-0045 §3——防复活）：
	// keys = 包内记忆原 ID；返回命中子集（nil 端口 = 无墓碑面，finding 略）。
	MemoryTombstoneHits func(ctx context.Context, keys []string) ([]string, error)
}

// importCaps 大小边界（生产取常量；UT 可收窄触发门）。
type importCaps struct {
	zip, expanded, file, entries, lines int64
}

func defaultImportCaps() importCaps {
	return importCaps{zip: MaxZipBytes, expanded: MaxExpandedByte, file: MaxFileBytes, entries: MaxEntries, lines: MaxNDJSONLines}
}

// ImportService 导入预检服务。
type ImportService struct {
	ports   ImportPorts
	dir     string
	product string
	now     func() string
	newID   func() string
	caps    importCaps
	mu      sync.Mutex // 查重~登记段互斥（一审 #2：并发同源导入必有一方 409）
}

// ImportOption 导入服务装配选项。
type ImportOption func(*ImportService)

// WithImporterNow 时钟单源（测试注入）。
func WithImporterNow(f func() string) ImportOption { return func(s *ImportService) { s.now = f } }

// WithImporterNewID import_id 生成单源（测试注入）。
func WithImporterNewID(f func() string) ImportOption { return func(s *ImportService) { s.newID = f } }

// NewImporter 构造（dataDir = 数据目录；productVersion 进 source_version 对照）。
func NewImporter(p ImportPorts, dataDir, productVersion string, opts ...ImportOption) *ImportService {
	s := &ImportService{
		ports: p, dir: dataDir, product: productVersion, caps: defaultImportCaps(),
		now:   func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newID: defaultImportID,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// defaultImportID bdi_<ts36>_<hex8>（bdl_/bkp_ 同族）。
func defaultImportID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "bdi_" + strconv.FormatInt(time.Now().Unix(), 36) + "_rand"
	}
	return "bdi_" + strconv.FormatInt(time.Now().Unix(), 36) + "_" + hex.EncodeToString(b[:])
}

// Import 上传 → 安全门 → 兼容报告 → inactive 登记（编排见 ADR-0043 §1）。
func (s *ImportService) Import(ctx context.Context, r io.Reader) (ImportView, error) {
	// 1) staging 落盘（限流——超上限整体拒绝）。
	root := filepath.Join(s.dir, importDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return ImportView{}, fmt.Errorf("bundle: mkdir imports: %w", err)
	}
	staging := filepath.Join(root, "incoming-"+randHex(6)+".tmp")
	if err := s.writeStaging(staging, r); err != nil {
		return ImportView{}, err
	}
	fail := func(err error) (ImportView, error) {
		_ = os.Remove(staging)
		return ImportView{}, err
	}

	// 2) 安全门（任一违规聚合拒绝）。
	content, violations := s.runGates(staging)
	if len(violations) > 0 {
		return fail(fmt.Errorf("%w：%s", ErrUnsafe, strings.Join(violations, "；")))
	}

	// 3) 幂等：同源 bundle_id 重复导入 → 409 指向既有记录。查重与登记
	// 同临界区（一审 #2：无锁窗口下两并发请求可同时查空并各自登记）；
	// staging/安全门在锁外（重 IO 无共享态）。
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, exists, err := s.findByBundleID(content.manifest.BundleID); err != nil {
		return fail(err)
	} else if exists {
		return fail(fmt.Errorf("%w: bundle_id=%s 已登记为 %s", ErrConflict, content.manifest.BundleID, prior))
	}

	// 4) 兼容性报告（对照本实例事实——非阻断）。
	checks, verdict, blockers := s.compatibility(ctx, content)

	// 5) 登记（staging 原子 rename 入位 + 边车）。
	importID := s.newID()
	bdir := filepath.Join(root, importID)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return fail(fmt.Errorf("bundle: mkdir import: %w", err))
	}
	if err := os.Rename(staging, filepath.Join(bdir, zipName)); err != nil {
		_ = os.RemoveAll(bdir)
		return fail(fmt.Errorf("bundle: place zip: %w", err))
	}
	view := ImportView{
		ImportID: importID, BundleID: content.manifest.BundleID, AgentID: content.profile.AgentID,
		Status: importStatus, Verdict: verdict, Blockers: blockers, ImportedAt: s.now(),
		SizeBytes: content.zipSizeBytes, ZipPath: importDirName + "/" + importID + "/" + zipName,
		Source: ImportSource{
			Format: content.manifest.Format, ProductVersion: content.manifest.ProductVersion,
			ExportedAt: content.manifest.ExportedAt,
		},
		Scope:  content.manifest.Scope,
		Counts: CountsView{Memories: len(content.memories), Missions: len(content.commitments.Missions), Checkpoints: len(content.ckpts)},
		Checks: checks, Redaction: content.redaction,
	}
	if err := writeImportSidecar(filepath.Join(bdir, importSidecarName), view); err != nil {
		_ = os.RemoveAll(bdir)
		return ImportView{}, err
	}
	return view, nil
}

// writeStaging 限流落盘（上限 +1 探测——超限即 ErrTooLarge）。
func (s *ImportService) writeStaging(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("bundle: open staging: %w", err)
	}
	n, err := io.Copy(f, io.LimitReader(r, s.caps.zip+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("bundle: staging copy: %w", err)
	}
	if n > s.caps.zip {
		_ = os.Remove(path)
		return fmt.Errorf("%w: 压缩体 %d 字节超上限 %d", ErrTooLarge, n, s.caps.zip)
	}
	return nil
}

// ---- 安全门（ADR-0043 §3）----

// runGates 六门全检；返回解析产物与违规清单（空 = 通过）。
func (s *ImportService) runGates(staging string) (*safeContent, []string) {
	var v []string
	add := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }

	st, err := os.Stat(staging)
	if err != nil {
		return nil, []string{fmt.Sprintf("zip: %v", err)}
	}
	zr, err := zip.OpenReader(staging)
	if err != nil {
		return nil, []string{fmt.Sprintf("zip_corrupt: %v", err)}
	}
	defer zr.Close()
	if st.Size() > s.caps.zip {
		add("size: 压缩体 %d 字节超上限 %d", st.Size(), s.caps.zip)
	}
	if int64(len(zr.File)) > s.caps.entries {
		add("layout: 条目数 %d 超上限 %d", len(zr.File), s.caps.entries)
		return nil, v
	}

	// 门 1+2+3：条目名（路径安全 + 精确白名单 + 无重复）与模式（普通文件）。
	seen := map[string]bool{}
	var declaredTotal int64
	for _, f := range zr.File {
		if !validEntryName(f.Name) {
			add("path: 条目名不合法（须包内相对路径、/ 分隔、无 ../绝对路径/反斜杠）：%q", f.Name)
			continue
		}
		if !bundleFiles[f.Name] {
			add("layout: 未知文件（v1 固定九文件布局）：%q", f.Name)
			continue
		}
		if seen[f.Name] {
			add("layout: 重复条目：%q", f.Name)
			continue
		}
		seen[f.Name] = true
		if !f.FileInfo().Mode().IsRegular() {
			add("symlink: 条目非普通文件（符号链接/目录/设备拒绝）：%q", f.Name)
			continue
		}
		declaredTotal += int64(f.UncompressedSize64)
	}
	for name := range bundleFiles {
		if !seen[name] {
			add("layout: 缺少必需文件：%q", name)
		}
	}
	if len(v) > 0 {
		return nil, v
	}

	// 门 4：压缩展开上限（声明量先行——实际量在读取时复核）。
	if declaredTotal > s.caps.expanded {
		add("expansion: 声明展开总量 %d 字节超上限 %d", declaredTotal, s.caps.expanded)
		return nil, v
	}

	// 读取全部条目（实际上限量 + 行数限复核）。
	raw := map[string][]byte{}
	var actualTotal int64
	for _, f := range zr.File {
		data, err := readEntry(f, s.caps.file)
		if err != nil {
			add("expansion: 读取 %q 超单文件上限：%v", f.Name, err)
			continue
		}
		actualTotal += int64(len(data))
		if actualTotal > s.caps.expanded {
			add("expansion: 实际展开总量超上限 %d", s.caps.expanded)
			break
		}
		raw[f.Name] = data
	}
	if len(v) > 0 {
		return nil, v
	}
	for _, name := range []string{"memory/items.ndjson", "work/checkpoints.ndjson", "context/manifests.ndjson"} {
		if n := ndjsonLineCount(raw[name]); n > s.caps.lines {
			add("expansion: %q 行数 %d 超上限 %d", name, n, s.caps.lines)
		}
	}

	// 门 5：manifest——严格解码 + 格式 + 必需字段。
	var man importManifest
	if err := decodeStrict(raw["manifest.json"], &man); err != nil {
		add("format: manifest 解析失败（未知字段/结构不合规即拒绝）：%v", err)
		return nil, v
	}
	if man.Format != Format {
		add("format: 格式 %q 不受支持（v1 仅同版本 %s；前向迁移待定义）", man.Format, Format)
	}
	if man.BundleID == "" || !bundleIDRe.MatchString(man.BundleID) {
		add("format: manifest.bundle_id 缺失或不合规")
	}
	if man.Agent.AgentID == "" {
		add("format: manifest.agent.agent_id 缺失")
	}
	if _, err := time.Parse(time.RFC3339, man.ExportedAt); err != nil {
		add("format: exported_at 非 RFC3339：%q", man.ExportedAt)
	}
	if len(man.Files) == 0 {
		add("format: manifest.files 缺失")
	}

	// 门 6：摘要——files 恰覆盖八个内容文件且逐一相符。
	digestSet := map[string]bool{}
	for _, e := range man.Files {
		if e.Path == "manifest.json" {
			add("digest: manifest 自身不得入 files（自摘要在数学上不闭合）")
			continue
		}
		if !bundleFiles[e.Path] || digestSet[e.Path] {
			add("digest: files 引用未知/重复文件：%q", e.Path)
			continue
		}
		digestSet[e.Path] = true
		data, ok := raw[e.Path]
		if !ok {
			add("digest: files 引用的文件不在包内：%q", e.Path)
			continue
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != e.SHA256 {
			add("digest: %q sha256 不符（manifest %s vs 实际 %s）", e.Path, e.SHA256, got)
		}
		if int64(len(data)) != e.Bytes {
			add("digest: %q 字节数不符（manifest %d vs 实际 %d）", e.Path, e.Bytes, len(data))
		}
	}
	for _, p := range contentFileOrder {
		if !digestSet[p] {
			add("digest: files 缺少内容文件摘要：%q", p)
		}
	}
	if len(v) > 0 {
		return nil, v
	}

	// 门 7：内容模式（严格解码逐文件）。
	var profile AgentFacts
	var slots slotsFile
	var commitments commitmentsFile
	var deps dependenciesFile
	var redaction RedactionReport
	if err := decodeStrict(raw["agent/profile.json"], &profile); err != nil {
		add("content: agent/profile.json：%v", err)
	}
	if err := decodeStrict(raw["slots/specs.json"], &slots); err != nil {
		add("content: slots/specs.json：%v", err)
	}
	if err := decodeStrict(raw["work/commitments.json"], &commitments); err != nil {
		add("content: work/commitments.json：%v", err)
	}
	if err := decodeStrict(raw["dependencies.json"], &deps); err != nil {
		add("content: dependencies.json：%v", err)
	}
	if err := decodeStrict(raw["redaction-report.json"], &redaction); err != nil {
		add("content: redaction-report.json：%v", err)
	} else if err := redactionRequired(redaction); err != nil {
		// 一审 #5：DisallowUnknownFields 不查必需字段——{} 也能过解码，
		// 而响应契约要求数组字段（null 数组会打崩消费端 .length）。
		add("content: redaction-report.json：%v", err)
	}
	memories, errs := parseNDJSON[MemoryFact](raw["memory/items.ndjson"])
	if len(errs) > 0 {
		add("content: memory/items.ndjson：%s", strings.Join(errs, "；"))
	}
	ckpts, errs := parseNDJSON[CheckpointFact](raw["work/checkpoints.ndjson"])
	if len(errs) > 0 {
		add("content: work/checkpoints.ndjson：%s", strings.Join(errs, "；"))
	}
	ctxEntries, errs := parseNDJSON[contextEntry](raw["context/manifests.ndjson"])
	if len(errs) > 0 {
		add("content: context/manifests.ndjson：%s", strings.Join(errs, "；"))
	}
	if len(v) > 0 {
		return nil, v
	}

	// 门 8：引用闭包与一致性（ADR-0043 §3 closure 行）。
	if profile.AgentID != man.Agent.AgentID {
		add("closure: manifest 与 profile 的 agent_id 不一致（%s vs %s）", man.Agent.AgentID, profile.AgentID)
	}
	if commitments.DefaultOnImport != "paused" {
		add("closure: default_on_import=%q 非 paused（RFC §8.2 承诺导入默认暂停）", commitments.DefaultOnImport)
	}
	missionSet := map[string]bool{}
	for i, m := range commitments.Missions {
		if m.MissionID == "" {
			add("closure: commitments[%d] 缺 mission_id", i)
			continue
		}
		if missionSet[m.MissionID] {
			add("closure: commitments 任务重复：%s", m.MissionID)
			continue
		}
		if m.ContractVersion < 1 {
			add("closure: %s contract_version %d < 1", m.MissionID, m.ContractVersion)
		}
		missionSet[m.MissionID] = true
	}
	ckptSeen := map[string]bool{}
	ckptByMission := map[string]CheckpointFact{}
	memSeen := map[string]bool{}
	for _, c := range ckpts {
		if !missionSet[c.MissionID] {
			add("closure: 检查点引用承诺外任务：%s", c.MissionID)
			continue
		}
		if ckptSeen[c.MissionID] {
			add("closure: 任务重复检查点（每任务至多一条 LatestForResume）：%s", c.MissionID)
			continue
		}
		ckptSeen[c.MissionID] = true
		ckptByMission[c.MissionID] = c
		if c.CheckpointVersion < 1 {
			add("closure: %s checkpoint_version %d < 1", c.MissionID, c.CheckpointVersion)
		}
	}
	ctxSeen := map[string]bool{}
	for _, e := range ctxEntries {
		if !missionSet[e.MissionID] {
			add("closure: 上下文清单引用承诺外任务：%s", e.MissionID)
			continue
		}
		if ctxSeen[e.MissionID] {
			add("closure: 上下文清单任务重复：%s", e.MissionID)
			continue
		}
		ctxSeen[e.MissionID] = true
		// 一审 #3：上下文的检查点引用须落在包内检查点上且版本一致——
		// 清空 checkpoints.ndjson 的断链包不得以"引用完整"面目过门。
		if ref := e.Checkpoint; ref != nil {
			c, ok := ckptByMission[e.MissionID]
			switch {
			case !ok:
				add("closure: 上下文引用的检查点不在包内（%s：%s）", e.MissionID, ref.CheckpointID)
			case c.CheckpointID != ref.CheckpointID || c.CheckpointVersion != ref.CheckpointVersion:
				add("closure: 上下文检查点引用不一致（%s：%s v%d vs 包内 %s v%d）",
					e.MissionID, ref.CheckpointID, ref.CheckpointVersion, c.CheckpointID, c.CheckpointVersion)
			}
		}
	}
	for _, it := range memories {
		switch it.Namespace {
		case nsAgent, nsWorkspace, nsMission:
		default:
			add("closure: 记忆命名空间未知（%s：%s）", it.MemoryID, it.Namespace)
			continue
		}
		if it.Namespace == nsMission && !missionSet[it.ScopeID] {
			add("closure: mission 域记忆引用承诺外任务（%s：%s）", it.MemoryID, it.ScopeID)
			continue
		}
		if it.Status != "active" {
			add("closure: 包内记忆非 active（导出口径只携有效项——%s：%s）", it.MemoryID, it.Status)
			continue
		}
		if it.Sensitivity != "normal" && it.Sensitivity != "sensitive" {
			add("closure: 记忆敏感度未知（%s：%s）", it.MemoryID, it.Sensitivity)
			continue
		}
		if memSeen[it.MemoryID] {
			add("closure: 记忆重复：%s", it.MemoryID)
			continue
		}
		memSeen[it.MemoryID] = true
	}
	for _, w := range man.Watermarks.Missions {
		if w.MissionID != "" && !missionSet[w.MissionID] {
			add("closure: 水位引用承诺外任务：%s", w.MissionID)
		}
		// 一审 #3 同因：水位声明的检查点版本须与包内相符；未声明而包内有
		// （或反之）同为断链。
		if w.MissionID == "" {
			continue
		}
		c, ok := ckptByMission[w.MissionID]
		if w.CheckpointVersion > 0 && (!ok || c.CheckpointVersion != w.CheckpointVersion) {
			add("closure: 水位声明检查点 v%d 但包内不符（%s）", w.CheckpointVersion, w.MissionID)
		}
		if w.CheckpointVersion == 0 && ok {
			add("closure: 水位未声明检查点但包内有（%s）", w.MissionID)
		}
	}
	for _, ns := range man.Scope.Namespaces {
		switch ns {
		case nsAgent, nsWorkspace, nsMission:
		default:
			add("closure: manifest.scope 命名空间未知：%s", ns)
		}
	}
	// 二审 #1：依赖清单与身份可执行项一致性——导出侧两处同源（同为
	// agent.Executables 投影），矛盾或缺失即包损坏/篡改，结构性拒绝
	//（否则清空 dependencies.harness 可绕过依赖可用性对照）。
	profileExecs, depExecs := map[string]bool{}, map[string]bool{}
	for _, e := range profile.Executables {
		if e.ExecutableID != "" {
			profileExecs[e.ExecutableID] = true
		}
	}
	for _, e := range deps.Harness {
		if e.ExecutableID != "" {
			depExecs[e.ExecutableID] = true
		}
	}
	for id := range profileExecs {
		if !depExecs[id] {
			add("closure: 依赖清单缺失身份可执行项（%s）——dependencies.json 与 agent/profile.json 矛盾", id)
		}
	}
	for id := range depExecs {
		if !profileExecs[id] {
			add("closure: 依赖清单含身份外可执行项（%s）——dependencies.json 与 agent/profile.json 矛盾", id)
		}
	}
	if len(v) > 0 {
		return nil, v
	}
	return &safeContent{
		manifest: man, profile: profile, memories: memories, ckpts: ckpts,
		commitments: commitments, ctxEntries: ctxEntries, deps: deps, redaction: redaction,
		zipSizeBytes: st.Size(),
	}, nil
}

// ---- 兼容性报告（ADR-0043 §4）----

// compatibility 对照本实例事实产出 findings；返回 (checks, verdict, blockers)。
func (s *ImportService) compatibility(ctx context.Context, c *safeContent) ([]CheckResult, string, []string) {
	var checks []CheckResult
	blockers := []string{}
	warned, blocked := false, false
	add := func(check, result, detail string) {
		checks = append(checks, CheckResult{Check: check, Result: result, Detail: detail})
		switch result {
		case "warn":
			warned = true
		case "block":
			blocked = true
			blockers = append(blockers, check)
		}
	}

	// 安全门通过摘要（恒 pass——失败到不了这里；保留让使用者看见已过）。
	add("zip_structure", "pass", "布局/路径/普通文件/展开上限全过（九文件精确集合）")
	add("integrity", "pass", "逐文件 sha256 与字节数和 manifest 摘要相符")
	add("reference_closure", "pass", "引用闭包完整（检查点/上下文/任务记忆/水位 ⊆ 承诺任务集）")

	// 来源版本对照（info——格式门已裁定兼容性，此行是事实面）。
	add("source_version", "info", fmt.Sprintf("来源实例 %s → 本实例 %s（格式 %s 同版本）",
		orDash(c.manifest.ProductVersion), orDash(s.product), c.manifest.Format))

	// 身份冲突（block——同身份双写防线起点，激活前须确认源端已撤销执行权）。
	if s.ports.AgentExists != nil {
		exists, err := s.ports.AgentExists(ctx, c.profile.AgentID)
		if err != nil {
			// 一审 #6：必要对照失败 = 必要证据缺失——block（非 warn），
			// 激活前重跑预检取得确定性结论。
			add("identity_conflict", "block", fmt.Sprintf("对照失败：%v——激活前须重跑预检", err))
		} else if exists {
			add("identity_conflict", "block",
				fmt.Sprintf("本实例已存在同 ID Agent（%s）——激活前须确认源端已撤销执行权并按 agent_id 主键重映射（0.7.7）", c.profile.AgentID))
		} else {
			add("identity_conflict", "pass", "本实例无同 ID Agent——可作为该身份承接")
		}
	}

	// 任务冲突（漂移 block / 同版重复 warn / 全新 pass）。
	if s.ports.MissionState != nil {
		var drift, dup []string
		for _, m := range c.commitments.Missions {
			local, ok, err := s.ports.MissionState(ctx, m.MissionID)
			if err != nil {
				drift = append(drift, fmt.Sprintf("%s：对照失败 %v", m.MissionID, err))
				continue
			}
			if !ok {
				continue
			}
			if local.ContractVersion != m.ContractVersion {
				drift = append(drift, fmt.Sprintf("%s：本实例 v%d vs 包内 v%d（漂移）", m.MissionID, local.ContractVersion, m.ContractVersion))
			} else {
				dup = append(dup, fmt.Sprintf("%s：同版本重复——激活时按既有任务去重", m.MissionID))
			}
		}
		switch {
		case len(drift) > 0:
			add("mission_conflict", "block", strings.Join(drift, "；"))
		case len(dup) > 0:
			add("mission_conflict", "warn", strings.Join(dup, "；"))
		default:
			add("mission_conflict", "pass", fmt.Sprintf("%d 个任务在本实例均不存在——全新承接", len(c.commitments.Missions)))
		}
	}

	// 依赖可用性（缺 = block——"必要证据缺失就阻止激活"；凭据本就不随包）。
	if s.ports.Harnesses != nil {
		local, err := s.ports.Harnesses(ctx)
		if err != nil {
			// 一审 #6 同因：对照失败 = 证据缺失——block，激活前重跑预检。
			add("harness_availability", "block", fmt.Sprintf("对照失败：%v——激活前须重跑预检", err))
		} else {
			have := map[string]bool{}
			for _, h := range local {
				have[h] = true
			}
			// 二审 #1：对照面 = dependencies.harness 与 profile.executables
			// 的并集（一致性门已保证两处同集——此处并集为防御纵深，清单
			// 被清空也不得静默缩圈）。
			refs := map[string]bool{}
			for _, e := range c.deps.Harness {
				if e.ExecutableID != "" {
					refs[e.ExecutableID] = true
				}
			}
			for _, e := range c.profile.Executables {
				if e.ExecutableID != "" {
					refs[e.ExecutableID] = true
				}
			}
			var wanted, missing []string
			for id := range refs {
				wanted = append(wanted, id)
				if !have[id] {
					missing = append(missing, id)
				}
			}
			sort.Strings(wanted)
			sort.Strings(missing)
			switch {
			case len(wanted) == 0:
				add("harness_availability", "info", "包内无可执行项引用")
			case len(missing) > 0:
				add("harness_availability", "block",
					fmt.Sprintf("本实例缺少可执行项：%s——激活前须在本实例注册（凭据不随包，目标环境须自行配置授权）", strings.Join(missing, "、")))
			default:
				add("harness_availability", "pass", fmt.Sprintf("包内 %d 项可执行引用本实例均已注册", len(wanted)))
			}
		}
	}

	// 墓碑命中（warn——0.7.8 / ADR-0045 §3：包内记忆已在本实例 Forget——
	// "受管理副本不复活已删正文"，激活时将拒绝复活；列 ID 待确认）。
	if s.ports.MemoryTombstoneHits != nil {
		ids := make([]string, 0, len(c.memories))
		for _, it := range c.memories {
			ids = append(ids, it.MemoryID)
		}
		hits, err := s.ports.MemoryTombstoneHits(ctx, ids)
		if err != nil {
			add("tombstone_hits", "warn", fmt.Sprintf("墓碑核对失败：%v", err))
		} else if len(hits) > 0 {
			add("tombstone_hits", "warn",
				fmt.Sprintf("%d 条已在本实例遗忘（%s）——激活时拒绝复活（不复活已删正文）", len(hits), strings.Join(hits, "、")))
		} else {
			add("tombstone_hits", "pass", "无墓碑命中")
		}
	}

	// 途中过期（warn——包可能旅行多日，激活时按 0.7.1 时效口径重新排除）。
	nowT, _ := time.Parse(time.RFC3339, s.now())
	var expired []string
	for _, it := range c.memories {
		if it.ValidTo == "" {
			continue
		}
		if vt, err := time.Parse(time.RFC3339, it.ValidTo); err == nil && vt.Before(nowT) {
			expired = append(expired, it.MemoryID)
		}
	}
	if len(expired) > 0 {
		add("expired_in_transit", "warn", fmt.Sprintf("%d 条记忆时效已过（%s）——激活时按时效口径重新排除", len(expired), strings.Join(expired, "、")))
	} else {
		add("expired_in_transit", "pass", "包内记忆时效均有效")
	}

	// 敏感携带（warn——接收方信任确认）。一审 #4：以包内实际载荷判定，
	// scope 开关只是导出侧声明——载荷与声明不一致时明示（不静默按声明放行）。
	sensCount := 0
	for _, it := range c.memories {
		if it.Sensitivity == "sensitive" {
			sensCount++
		}
	}
	switch {
	case sensCount > 0 && c.manifest.Scope.IncludeSensitive:
		add("sensitive_payload", "warn",
			fmt.Sprintf("包内含 %d 条敏感记忆（导出时显式 opt-in）——接收方信任确认后再激活", sensCount))
	case sensCount > 0:
		add("sensitive_payload", "warn",
			fmt.Sprintf("包内含 %d 条敏感记忆但 scope.include_sensitive=false——载荷与声明不一致（来源可疑或包被改动），人工核验后再激活", sensCount))
	default:
		add("sensitive_payload", "pass", "无敏感记忆随包")
	}

	// 恢复限制（warn——透传包内脱敏报告）。
	if limits := c.redaction.RecoveryLimits; len(limits) > 0 {
		add("recovery_limits", "warn", strings.Join(limits, "；"))
	} else {
		add("recovery_limits", "pass", "无逐任务恢复限制")
	}

	// 房间交办来源（info——原房间不在本实例，回传目标需重绑定）。
	if n := roomSourced(c.commitments.Missions); n > 0 {
		add("room_source_unbound", "info", fmt.Sprintf("%d 个任务为房间交办——原房间不在本实例，回传目标需重绑定", n))
	} else {
		add("room_source_unbound", "pass", "无房间交办来源")
	}

	// 别名投影说明（info——恒定事实）。
	add("alias_projection", "info", "别名体系为可移植投影——原始派生别名按 agent_id 主键在激活时重映射（ADR-0042 二审 #1）")

	verdict := verdictEligib
	switch {
	case blocked:
		verdict = verdictBlocked
	case warned:
		verdict = verdictWarning
	}
	return checks, verdict, blockers
}

// ---- 列表/详情 ----

// List 导入列表（新→旧；边车投影，坏边车跳过不阻断）。
func (s *ImportService) List() ([]ImportSummary, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, importDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return []ImportSummary{}, nil
		}
		return nil, err
	}
	var out []ImportSummary
	for _, e := range entries {
		if !e.IsDir() || !importIDRe.MatchString(e.Name()) {
			continue
		}
		v, ok, err := s.readSidecar(e.Name())
		if err != nil || !ok {
			continue
		}
		out = append(out, ImportSummary{
			ImportID: v.ImportID, BundleID: v.BundleID, AgentID: v.AgentID,
			Status: v.Status, Verdict: v.Verdict, ImportedAt: v.ImportedAt,
			SizeBytes: v.SizeBytes, Counts: v.Counts,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ImportedAt != out[j].ImportedAt {
			return out[i].ImportedAt > out[j].ImportedAt
		}
		return out[i].ImportID > out[j].ImportID
	})
	return out, nil
}

// Get 导入详情（import_id 先过白名单再拼路径——防穿越纪律）。
func (s *ImportService) Get(importID string) (ImportView, error) {
	if !importIDRe.MatchString(importID) {
		return ImportView{}, fmt.Errorf("%w: import_id 不合法", ErrInvalid)
	}
	v, ok, err := s.readSidecar(importID)
	if err != nil {
		return ImportView{}, err
	}
	if !ok {
		return ImportView{}, ErrNotFound
	}
	return v, nil
}

// writeSidecar 回写边车（激活面消费——status/activation 更新）。
func (s *ImportService) writeSidecar(importID string, view ImportView) error {
	return writeImportSidecar(filepath.Join(s.dir, importDirName, importID, importSidecarName), view)
}

// writeImportSidecar 边车落盘（0600 + 尾换行）。
func writeImportSidecar(path string, view ImportView) error {
	raw, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("bundle: import sidecar: %w", err)
	}
	return nil
}

// readSidecar 读边车；ok=false = 记录不存在。
func (s *ImportService) readSidecar(importID string) (ImportView, bool, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, importDirName, importID, importSidecarName))
	if err != nil {
		if os.IsNotExist(err) {
			return ImportView{}, false, nil
		}
		return ImportView{}, false, err
	}
	var v ImportView
	if err := json.Unmarshal(raw, &v); err != nil {
		return ImportView{}, false, fmt.Errorf("bundle: import 边车损坏：%s", importID)
	}
	return v, true, nil
}

// findByBundleID 同源包查重（返回既有 import_id；无 = ok=false）。
func (s *ImportService) findByBundleID(bundleID string) (string, bool, error) {
	list, err := s.List()
	if err != nil {
		return "", false, err
	}
	for _, item := range list {
		if item.BundleID == bundleID {
			return item.ImportID, true, nil
		}
	}
	return "", false, nil
}

// ---- 工具 ----

// validEntryName 包内相对路径白名单前置校验（生成侧保证的导入侧强制）。
func validEntryName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	if strings.Contains(name, "..") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return false
		}
	}
	return true
}

// readEntry 读单条目（单文件上限——实际量复核，声明量可伪造）。
func readEntry(f *zip.File, cap int64) ([]byte, error) {
	if int64(f.UncompressedSize64) > cap {
		return nil, fmt.Errorf("声明 %d 字节超上限 %d", f.UncompressedSize64, cap)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > cap {
		return nil, fmt.Errorf("实际 %d 字节超上限 %d", len(data), cap)
	}
	return data, nil
}

// redactionRequired 脱敏报告必需字段校验（一审 #5——数组字段缺失时
// JSON 反序列化为 null，响应契约（与前端消费）都按数组形态）。
func redactionRequired(r RedactionReport) error {
	if r.OmittedSensitive.ItemIDs == nil {
		return errors.New("omitted_sensitive.item_ids 缺失（须为数组）")
	}
	if r.RecoveryLimits == nil {
		return errors.New("recovery_limits 缺失（须为数组）")
	}
	if r.Notes == nil {
		return errors.New("notes 缺失（须为数组）")
	}
	return nil
}

// decodeStrict 严格 JSON（拒收未知字段与尾随内容——"不理解即拒绝"）。
// 二审 #2：More() 是"容器还有没有元素"的语义（遇 ] 或 } 恒 false），
// 不能证明顶层已结束——尾随判定须以第二次 Decode 严格 io.EOF 为准。
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("尾随内容")
	}
	return nil
}

// parseNDJSON 逐行严格解析（空行容忍——导出侧尾换行）。一审 P1：扫描式
// 切行（切片引用零分配）——bytes.Split 为每行分配切片头，高压缩空行载荷
// 可在行数上限生效前放大出数 GiB 分配。
func parseNDJSON[T any](raw []byte) ([]T, []string) {
	var out []T
	var errs []string
	lineNo, start := 0, 0
	for i := 0; i <= len(raw); i++ {
		if i < len(raw) && raw[i] != '\n' {
			continue
		}
		lineNo++
		line := raw[start:i] // 引用原缓冲，不复制
		start = i + 1
		if !hasNonSpace(line) {
			continue
		}
		var item T
		if err := decodeStrict(line, &item); err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 行：%v", lineNo, err))
			continue
		}
		out = append(out, item)
	}
	return out, errs
}

// ndjsonLineCount 非空行计数（行数上限门；扫描式零分配——一审 P1 同因）。
func ndjsonLineCount(raw []byte) int64 {
	var n int64
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i < len(raw) && raw[i] != '\n' {
			continue
		}
		if hasNonSpace(raw[start:i]) {
			n++
		}
		start = i + 1
	}
	return n
}

// hasNonSpace 行内是否含非空白字节（空/空格/制表/回车皆空）。
func hasNonSpace(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\r' {
			return true
		}
	}
	return false
}

// roomSourced 房间交办来源任务计数。
func roomSourced(missions []MissionFact) int {
	n := 0
	for _, m := range missions {
		if m.RoomSource != nil {
			n++
		}
	}
	return n
}

// orDash 空串占位。
func orDash(s string) string {
	if s == "" {
		return "（未记录）"
	}
	return s
}

// randHex n 字节随机 hex（staging 命名）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
