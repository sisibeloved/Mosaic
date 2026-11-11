// Package bundle Portable Agent Bundle 导出（0.7.5 / ADR-0042，RFC-0018 §8）
// 与导入预检（0.7.6 / ADR-0043——import.go）。
//
// 导出语义与边界：
//   - 导出 = 只读快照打 zip（不停源 Agent、不加锁、不改任何域状态——
//     RFC §8.3"普通备份/导出"行）；各源顺序独立读取，水位记录合并时刻
//     （非单事务快照——导入侧 0.7.6 以任务级 version 比对治理，ADR §5）；
//   - 范围选择：命名空间 ⊆ {agent, workspace, mission}（room/slot 授权口径
//     未建，v1 不开放）；mission_ids 缺省 = 自动圈定非终态 owned（draft 不
//     带走）；显式数组逐条校验归属，不符整体拒绝（透明不静默缩圈）；
//   - 过滤先前的结构化应用：未授权 scope 根本不进入查询（ADR §3）；
//     sensitive 默认省略（只 ID 进报告，不泄内容/标题）；
//   - 摘要：manifest.json 逐文件 sha256（完整性检查用——不等同于可信
//     签名）；manifest 自身不在 files 内（自摘要在数学上不闭合）；
//   - 产物在库外：数据目录 bundles/<bundle_id>/{mosaic-agent-bundle.zip,
//     export.json 边车}；零 DB migration（backup 同例）。
//
// 依赖方向：不 import personalagent/slot/mission/checkpoint/memory 任一域
// ——Ports 全闭包 + 域内投影类型（execctx CheckpointFacts 先例）。
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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// 包内布局常量（RFC §8.2 v1——固定文件序，manifest 收尾）。
const (
	Format      = "mosaic-agent-bundle/1"
	zipName     = "mosaic-agent-bundle.zip"
	sidecarName = "export.json"
	nsAgent     = "agent"
	nsWorkspace = "workspace"
	nsMission   = "mission"
)

var (
	ErrInvalid       = errors.New("bundle: invalid")
	ErrAgentNotFound = errors.New("bundle: agent not found")
	ErrNotFound      = errors.New("bundle: not found")
)

var bundleIDRe = regexp.MustCompile(`^bdl_[0-9a-z_]+$`)

// ---- 投影类型（app 闭包映射；字段同构各域）----

// ExecutableRef 可执行项引用（一审 #4：ExecutableID 为可移植投影
// adapter@runtime[:distro]——注册表路径型标识不随包；无凭据）。
type ExecutableRef struct {
	ExecutableID string `json:"executable_id"`
	Runtime      string `json:"runtime"`
	Distro       string `json:"distro"`
	Channel      string `json:"channel"`
	Enabled      bool   `json:"enabled"`
}

// AliasFacts 三向别名（schema: IdentityAgent.aliases）。
type AliasFacts struct {
	BotID          string   `json:"bot_id"`
	ProfileIDs     []string `json:"profile_ids"`
	ParticipantIDs []string `json:"participant_ids"`
}

// AgentFacts 身份事实（agent/profile.json 全量——不含认证凭据）。
type AgentFacts struct {
	AgentID     string          `json:"agent_id"`
	DisplayName string          `json:"display_name"`
	Adapter     string          `json:"adapter"`
	Status      string          `json:"status"`
	Source      string          `json:"source"`
	CreatedAt   string          `json:"created_at"`
	Aliases     AliasFacts      `json:"aliases"`
	Executables []ExecutableRef `json:"executables"`
}

// SlotSpecFacts SlotSpec 内容投影（slot.SpecContent 同构中性镜像）。
type SlotSpecFacts struct {
	Name           string           `json:"name"`
	Responsibility string           `json:"responsibility"`
	Deliverables   []string         `json:"deliverables"`
	Skills         []SkillRef       `json:"skills,omitempty"`
	Tools          *CapabilityFacts `json:"tools,omitempty"`
	Context        *CapabilityFacts `json:"context,omitempty"`
	Delegation     *DelegationFacts `json:"delegation,omitempty"`
	Runtime        *RuntimeReqFacts `json:"runtime,omitempty"`
	Handoff        *HandoffFacts    `json:"handoff,omitempty"`
}

// SkillRef 技能引用。
type SkillRef struct {
	Ref     string `json:"ref"`
	Version int    `json:"version"`
}

// CapabilityFacts 能力要求分层（required 缺失即 blocked / optional 声明降级）。
type CapabilityFacts struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

// DelegationFacts 委派策略。
type DelegationFacts struct {
	Allowed  []string `json:"allowed"`
	MaxDepth int      `json:"max_depth"`
}

// RuntimeReqFacts 运行能力要求（能力名，非安装路径）。
type RuntimeReqFacts struct {
	Required        []string `json:"required"`
	WorkspaceAccess string   `json:"workspace_access"`
}

// HandoffFacts 交接必备件。
type HandoffFacts struct {
	Required []string `json:"required"`
}

// SlotFact 现任职责（active Binding + 所需 SlotSpec 版本与内容）。
type SlotFact struct {
	BindingID       string        `json:"binding_id"`
	SlotInstanceID  string        `json:"slot_instance_id"`
	SlotSpecID      string        `json:"slot_spec_id"`
	SlotSpecVersion int           `json:"slot_spec_version"`
	Status          string        `json:"status"`
	Spec            SlotSpecFacts `json:"spec"`
}

// RoomSourceFacts 房间交办来源（原对象映射）。
type RoomSourceFacts struct {
	RoomID string `json:"room_id"`
	TaskID string `json:"task_id"`
}

// ContractFacts 任务契约（mission.Content 同构）。
type ContractFacts struct {
	Objective          string          `json:"objective"`
	Scope              string          `json:"scope"`
	Constraints        []string        `json:"constraints"`
	AcceptanceCriteria []string        `json:"acceptance_criteria"`
	DeliverableSpecs   []string        `json:"deliverable_specs"`
	SourceRefs         []string        `json:"source_refs"`
	ReturnTargets      []string        `json:"return_targets"`
	Budget             BudgetFacts     `json:"budget"`
	Delegation         DelegationFacts `json:"delegation"`
	Reviewer           string          `json:"reviewer"`
}

// BudgetFacts 执行预算（mission.Budget 同构）。
type BudgetFacts struct {
	Concurrency         int `json:"concurrency"`
	MaxRuns             int `json:"max_runs"`
	MaxWallClockSeconds int `json:"max_wall_clock_seconds"`
	MaxRetries          int `json:"max_retries"`
	MaxDelegationDepth  int `json:"max_delegation_depth"`
}

// MissionFact 任务承诺事实（work/commitments.json 元素）。
type MissionFact struct {
	MissionID       string           `json:"mission_id"`
	ContractVersion int              `json:"contract_version"`
	Status          string           `json:"status"`
	OwnerAgentID    string           `json:"owner_agent_id"`
	Requester       string           `json:"requester"`
	RoomSource      *RoomSourceFacts `json:"room_source,omitempty"`
	Contract        ContractFacts    `json:"contract"`
	CreatedAt       string           `json:"created_at"`
	UpdatedAt       string           `json:"updated_at"`
}

// CheckpointFact 续办检查点（LatestForResume 投影——0.7.4 口径复用）。
type CheckpointFact struct {
	CheckpointID      string            `json:"checkpoint_id"`
	RunID             string            `json:"run_id"`
	MissionID         string            `json:"mission_id"`
	MissionVersion    int               `json:"mission_version"`
	CheckpointVersion int               `json:"checkpoint_version"`
	Source            string            `json:"source"` // ""=业务快照 / "system"=终态摘要
	CompletedSteps    []string          `json:"completed_steps"`
	PendingSteps      []string          `json:"pending_steps"`
	ArtifactRefs      []string          `json:"artifact_refs"`
	ActionReceipts    []string          `json:"action_receipts"`
	UncertainActions  []string          `json:"uncertain_actions"`
	WaitingConditions []string          `json:"waiting_conditions"`
	SourceWatermarks  map[string]string `json:"source_watermarks"`
	ProgressSummary   string            `json:"progress_summary"`
	CreatedAt         string            `json:"created_at"`
}

// EditRecordFact 策展留痕（memory.EditRecord 同构）。
type EditRecordFact struct {
	Version    int    `json:"version"`
	Cause      string `json:"cause"`
	Note       string `json:"note,omitempty"`
	EditedBy   string `json:"edited_by"`
	OccurredAt string `json:"occurred_at"`
}

// MemoryFact 记忆条目（memory.Item 同构——敏感性/状态/时效/血缘全字段）。
type MemoryFact struct {
	MemoryID    string           `json:"memory_id"`
	Version     int              `json:"version"`
	Namespace   string           `json:"namespace"`
	ScopeID     string           `json:"scope_id"`
	Kind        string           `json:"kind"`
	Title       string           `json:"title"`
	Content     string           `json:"content"`
	SourceRefs  []string         `json:"source_refs"`
	ObservedAt  string           `json:"observed_at"`
	ValidFrom   string           `json:"valid_from"`
	ValidTo     string           `json:"valid_to"`
	Confidence  float64          `json:"confidence"`
	Status      string           `json:"status"`
	Visibility  string           `json:"visibility"`
	Sensitivity string           `json:"sensitivity"`
	Supersedes  []string         `json:"supersedes"`
	EditHistory []EditRecordFact `json:"edit_history"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
}

// ---- 视图（JSON 形状即 API 契约形状——backup Summary 先例）----

// FileEntry 包内文件摘要条目（manifest.files 与 View.Files 同构）。
type FileEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// ScopeView 实际导出范围（mission_ids 为解析后实际集合——自动圈定也展开）。
type ScopeView struct {
	Namespaces       []string `json:"namespaces"`
	MissionSelection string   `json:"mission_selection"` // auto | explicit
	MissionIDs       []string `json:"mission_ids"`
	IncludeSensitive bool     `json:"include_sensitive"`
}

// CountsView 入包计数（被排除/省略项不入——只计随包事实）。
type CountsView struct {
	Memories    int `json:"memories"`
	Missions    int `json:"missions"`
	Checkpoints int `json:"checkpoints"`
}

// OmittedView 敏感省略明细（只 ID 不内容/标题）。
type OmittedView struct {
	Count   int      `json:"count"`
	ItemIDs []string `json:"item_ids"`
}

// RuleExcludes 口径排除计数。
type RuleExcludes struct {
	Expired    int `json:"expired"`
	Proposed   int `json:"proposed"`
	Superseded int `json:"superseded"`
}

// RedactionReport 脱敏报告（包内 redaction-report.json 同构内联）。
type RedactionReport struct {
	OmittedSensitive OmittedView  `json:"omitted_sensitive"`
	ExcludedByRule   RuleExcludes `json:"excluded_by_rule"`
	RecoveryLimits   []string     `json:"recovery_limits"`
	ArtifactPolicy   string       `json:"artifact_policy"` // v1 恒 refs_only
	Notes            []string     `json:"notes"`
}

// View 导出全量视图（创建响应/详情；边车 export.json 即其序列化）。
type View struct {
	BundleID       string          `json:"bundle_id"`
	AgentID        string          `json:"agent_id"`
	Format         string          `json:"format"`
	CreatedAt      string          `json:"created_at"`
	SizeBytes      int64           `json:"size_bytes"`
	ZipPath        string          `json:"zip_path"`
	ProductVersion string          `json:"product_version,omitempty"`
	Scope          ScopeView       `json:"scope"`
	Counts         CountsView      `json:"counts"`
	Files          []FileEntry     `json:"files"`
	Redaction      RedactionReport `json:"redaction"`
}

// Summary 导出摘要（列表元素）。
type Summary struct {
	BundleID  string     `json:"bundle_id"`
	AgentID   string     `json:"agent_id"`
	CreatedAt string     `json:"created_at"`
	SizeBytes int64      `json:"size_bytes"`
	Counts    CountsView `json:"counts"`
}

// ---- 服务 ----

// Ports 数据源闭包（app.go 组合各域；nil = 未装配面由调用方保证不触达）。
type Ports struct {
	Agent      func(ctx context.Context, agentID string) (AgentFacts, bool, error)
	Slots      func(ctx context.Context, agentID string) ([]SlotFact, error)
	Missions   func(ctx context.Context, agentID string) ([]MissionFact, error)
	Checkpoint func(ctx context.Context, missionID string) (CheckpointFact, bool, error)
	// Memories 返回该 scope 全量（任意状态——报告需见被排除项；导出口径
	// 在本域单点执行）。
	Memories func(ctx context.Context, namespace, scopeID string) ([]MemoryFact, error)
}

// ScopeRequest 导出范围请求（原始形态——nil 语义在域内解析）。
type ScopeRequest struct {
	Namespaces       []string // nil = 默认 [agent, workspace]
	MissionIDs       []string // nil = 自动（非终态 owned）
	IncludeSensitive bool
}

// Service 导出服务。
type Service struct {
	ports   Ports
	dir     string // 数据目录（bundles/ 落其下）
	product string
	now     func() string
	newID   func() string
}

// Option 装配选项。
type Option func(*Service)

// WithNow 时钟单源（测试注入）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewID bundle_id 生成单源（测试注入）。
func WithNewID(f func() string) Option { return func(s *Service) { s.newID = f } }

// New 构造（dataDir = 数据目录；productVersion 进 manifest 来源实例）。
func New(p Ports, dataDir, productVersion string, opts ...Option) *Service {
	s := &Service{
		ports: p, dir: dataDir, product: productVersion,
		now:   func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newID: defaultBundleID,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// defaultBundleID bdl_<ts36>_<hex8>（backup bkp_ 同族）。
func defaultBundleID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "bdl_" + strconv.FormatInt(time.Now().Unix(), 36) + "_rand"
	}
	return "bdl_" + strconv.FormatInt(time.Now().Unix(), 36) + "_" + hex.EncodeToString(b[:])
}

// nonTerminal 非终态集合（自动圈定口径；draft 属未派发草稿不带走——ADR §2）。
var nonTerminal = map[string]bool{
	"ready": true, "running": true, "waiting": true, "paused": true, "canceling": true,
}

// Export 创建导出（编排见 ADR §7）。
func (s *Service) Export(ctx context.Context, agentID string, req ScopeRequest) (View, error) {
	// 1) 身份。
	agent, ok, err := s.ports.Agent(ctx, agentID)
	if err != nil {
		return View{}, fmt.Errorf("bundle: agent port: %w", err)
	}
	if !ok {
		return View{}, ErrAgentNotFound
	}
	// 2) 命名空间。
	namespaces := req.Namespaces
	if namespaces == nil {
		namespaces = []string{nsAgent, nsWorkspace}
	}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		switch ns {
		case nsAgent, nsWorkspace, nsMission:
		default:
			return View{}, fmt.Errorf("%w: 命名空间未知或暂不开放导出：%s（v1 ⊆ agent|workspace|mission）", ErrInvalid, ns)
		}
		if seen[ns] {
			return View{}, fmt.Errorf("%w: 命名空间重复：%s", ErrInvalid, ns)
		}
		seen[ns] = true
	}
	// 3) 任务圈定。
	all, err := s.ports.Missions(ctx, agentID)
	if err != nil {
		return View{}, fmt.Errorf("bundle: missions port: %w", err)
	}
	byID := make(map[string]MissionFact, len(all))
	for _, m := range all {
		byID[m.MissionID] = m
	}
	var selected []MissionFact
	selection := "auto"
	if req.MissionIDs == nil {
		for _, m := range all {
			// 一审 #5：owner 复核（详情时刻事实）——wiring 摘要/详情两步读取间
			// 任务可能已交接；非所属任务静默跳过（自动圈定的竞态保守语义）。
			if nonTerminal[m.Status] && m.OwnerAgentID == agentID {
				selected = append(selected, m)
			}
		}
	} else {
		selection = "explicit"
		picked := map[string]bool{}
		for _, id := range req.MissionIDs {
			if picked[id] {
				return View{}, fmt.Errorf("%w: 任务重复：%s", ErrInvalid, id)
			}
			picked[id] = true
			m, ok := byID[id]
			if !ok {
				return View{}, fmt.Errorf("%w: 任务不存在或非当前负责：%s", ErrInvalid, id)
			}
			// 一审 #5：显式圈定遇已交接任务整体拒绝（透明不静默缩圈——与
			// "任务不存在或非当前负责"同语义）。
			if m.OwnerAgentID != agentID {
				return View{}, fmt.Errorf("%w: 任务不存在或非当前负责：%s", ErrInvalid, id)
			}
			selected = append(selected, m)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].MissionID < selected[j].MissionID })
	selectedIDs := make([]string, len(selected))
	for i, m := range selected {
		selectedIDs[i] = m.MissionID
	}

	// 4) 记忆：scope 对集合 → 逐 scope 全量拉取 → 域内口径过滤（§3）。
	type nsScope struct{ ns, scope string }
	var pairs []nsScope
	for _, ns := range namespaces {
		switch ns {
		case nsAgent:
			pairs = append(pairs, nsScope{nsAgent, agentID})
		case nsWorkspace:
			pairs = append(pairs, nsScope{nsWorkspace, "default"})
		case nsMission:
			for _, id := range selectedIDs {
				pairs = append(pairs, nsScope{nsMission, id})
			}
		}
	}
	nowT, _ := time.Parse(time.RFC3339, s.now())
	report := RedactionReport{
		OmittedSensitive: OmittedView{ItemIDs: []string{}},
		RecoveryLimits:   []string{},
		ArtifactPolicy:   "refs_only",
		Notes: []string{
			"凭据/令牌/原生会话材料不随包（设计边界——目标环境须自行配置授权）",
			"artifact_refs 保留标识不携文件——目标侧可访问性未判定（refs_only）",
			"无 embedding 随包（Mosaic 记忆无向量索引）",
		},
	}
	if req.IncludeSensitive {
		report.Notes = append(report.Notes, "include_sensitive=true：敏感记忆经显式选择随包")
	}
	var carried []MemoryFact
	memCounts := map[string]int{}
	for _, p := range pairs {
		items, err := s.ports.Memories(ctx, p.ns, p.scope)
		if err != nil {
			return View{}, fmt.Errorf("bundle: memories port(%s|%s): %w", p.ns, p.scope, err)
		}
		for _, it := range items {
			switch {
			case it.Status == "proposed":
				report.ExcludedByRule.Proposed++
				continue
			case it.Status == "superseded":
				report.ExcludedByRule.Superseded++
				continue
			case it.Status != "active":
				// 未知状态保守排除并计数到 superseded 桶外——按枚举只应有上述三种；
				// 防御性归入 proposed 桶不可读，这里直接按 expired 之外的显式拒绝。
				return View{}, fmt.Errorf("%w: 记忆状态未知：%s（%s）", ErrInvalid, it.Status, it.MemoryID)
			}
			if it.ValidTo != "" {
				if vt, err := time.Parse(time.RFC3339, it.ValidTo); err == nil && vt.Before(nowT) {
					report.ExcludedByRule.Expired++
					continue
				} // 解析失败保守包含（0.7.1 SQL julianday NULL 保守同义）
			}
			if it.Sensitivity == "sensitive" && !req.IncludeSensitive {
				report.OmittedSensitive.Count++
				report.OmittedSensitive.ItemIDs = append(report.OmittedSensitive.ItemIDs, it.MemoryID)
				continue
			}
			carried = append(carried, it)
			memCounts[p.ns]++
		}
	}

	// 5) 检查点（LatestForResume 口径——端口已按 0.7.4 选取）。
	var ckpts []CheckpointFact
	for _, id := range selectedIDs {
		fact, ok, err := s.ports.Checkpoint(ctx, id)
		if err != nil {
			return View{}, fmt.Errorf("bundle: checkpoint port(%s): %w", id, err)
		}
		if !ok {
			report.RecoveryLimits = append(report.RecoveryLimits,
				fmt.Sprintf("%s 无任何检查点——导入后从契约裸启动", id))
			continue
		}
		ckpts = append(ckpts, fact)
	}

	// 6) 任职。
	slots, err := s.ports.Slots(ctx, agentID)
	if err != nil {
		return View{}, fmt.Errorf("bundle: slots port: %w", err)
	}

	// 7) 渲染（固定文件序；ndjson 行序 = scope 序 + 查询序）。
	rendered := map[string][]byte{}
	addJSON := func(path string, v any) error {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		rendered[path] = append(raw, '\n')
		return nil
	}
	if err := addJSON("agent/profile.json", agent); err != nil {
		return View{}, err
	}
	if err := addJSON("slots/specs.json", map[string]any{"bindings": nonNilSlots(slots)}); err != nil {
		return View{}, err
	}
	var memLines bytes.Buffer
	for _, it := range carried {
		line, err := json.Marshal(it)
		if err != nil {
			return View{}, err
		}
		memLines.Write(line)
		memLines.WriteByte('\n')
	}
	rendered["memory/items.ndjson"] = memLines.Bytes()

	var ckptLines bytes.Buffer
	ckptByMission := map[string]CheckpointFact{}
	for _, c := range ckpts {
		line, err := json.Marshal(c)
		if err != nil {
			return View{}, err
		}
		ckptLines.Write(line)
		ckptLines.WriteByte('\n')
		ckptByMission[c.MissionID] = c
	}
	rendered["work/checkpoints.ndjson"] = ckptLines.Bytes()

	if err := addJSON("work/commitments.json", map[string]any{
		"default_on_import": "paused", // RFC §8.2：承诺导入默认暂停
		"missions":          selected,
	}); err != nil {
		return View{}, err
	}

	var ctxLines bytes.Buffer
	for _, m := range selected {
		entry := map[string]any{
			"mission_id":       m.MissionID,
			"contract_version": m.ContractVersion,
			"artifact_refs":    artifactRefsOf(m, ckptByMission),
		}
		if c, ok := ckptByMission[m.MissionID]; ok {
			entry["checkpoint"] = map[string]any{
				"checkpoint_id": c.CheckpointID, "checkpoint_version": c.CheckpointVersion, "source": c.Source,
			}
		} else {
			entry["checkpoint"] = nil
		}
		if m.RoomSource != nil {
			entry["room_source"] = m.RoomSource
		} else {
			entry["room_source"] = nil
		}
		line, err := json.Marshal(entry)
		if err != nil {
			return View{}, err
		}
		ctxLines.Write(line)
		ctxLines.WriteByte('\n')
	}
	rendered["context/manifests.ndjson"] = ctxLines.Bytes()

	deps := map[string]any{
		"product_version": s.product,
		"harness":         agent.Executables,
		"slots":           depsOfSlots(slots),
		"notes":           []string{"凭据不随包——目标环境须自行配置 harness 授权"},
	}
	if err := addJSON("dependencies.json", deps); err != nil {
		return View{}, err
	}
	if err := addJSON("redaction-report.json", report); err != nil {
		return View{}, err
	}

	// 8) manifest 收尾（摘要不含自身）。
	order := []string{
		"agent/profile.json", "slots/specs.json", "memory/items.ndjson",
		"work/checkpoints.ndjson", "work/commitments.json", "context/manifests.ndjson",
		"dependencies.json", "redaction-report.json",
	}
	files := make([]FileEntry, 0, len(order))
	for _, p := range order {
		sum := sha256.Sum256(rendered[p])
		files = append(files, FileEntry{Path: p, Bytes: int64(len(rendered[p])), SHA256: hex.EncodeToString(sum[:])})
	}
	bundleID := s.newID()
	createdAt := s.now()
	wmMissions := make([]map[string]any, 0, len(selected))
	for _, m := range selected {
		e := map[string]any{"mission_id": m.MissionID, "contract_version": m.ContractVersion}
		if c, ok := ckptByMission[m.MissionID]; ok {
			e["checkpoint_version"] = c.CheckpointVersion
			e["checkpoint_source"] = c.Source
		}
		wmMissions = append(wmMissions, e)
	}
	scopeView := ScopeView{
		Namespaces: namespaces, MissionSelection: selection,
		MissionIDs: selectedIDs, IncludeSensitive: req.IncludeSensitive,
	}
	manifest := map[string]any{
		"format":          Format,
		"bundle_id":       bundleID,
		"agent":           map[string]any{"agent_id": agent.AgentID, "display_name": agent.DisplayName, "adapter": agent.Adapter, "aliases": agent.Aliases},
		"scope":           scopeView,
		"exported_at":     createdAt,
		"product_version": s.product,
		"watermarks": map[string]any{
			"missions":      wmMissions,
			"memory_counts": memCounts,
		},
		"files": files,
	}
	manRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return View{}, err
	}
	rendered["manifest.json"] = append(manRaw, '\n')

	// 9) 落盘：tmp zip → 同目录 rename（原子）→ 边车（zip 含 manifest 收尾）。
	bdir := filepath.Join(s.dir, "bundles", bundleID)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return View{}, fmt.Errorf("bundle: mkdir: %w", err)
	}
	zipPath := filepath.Join(bdir, zipName)
	tmp := zipPath + ".tmp"
	if err := writeZip(tmp, append(append([]string{}, order...), "manifest.json"), rendered); err != nil {
		_ = os.RemoveAll(bdir)
		return View{}, err
	}
	if err := os.Rename(tmp, zipPath); err != nil {
		_ = os.RemoveAll(bdir)
		return View{}, fmt.Errorf("bundle: rename: %w", err)
	}
	st, err := os.Stat(zipPath)
	if err != nil {
		return View{}, err
	}
	view := View{
		BundleID: bundleID, AgentID: agentID, Format: Format, CreatedAt: createdAt,
		SizeBytes: st.Size(), ZipPath: "bundles/" + bundleID + "/" + zipName,
		ProductVersion: s.product, Scope: scopeView,
		Counts: CountsView{Memories: len(carried), Missions: len(selected), Checkpoints: len(ckpts)},
		Files:  files, Redaction: report,
	}
	sidecar, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return View{}, err
	}
	if err := os.WriteFile(filepath.Join(bdir, sidecarName), append(sidecar, '\n'), 0o600); err != nil {
		return View{}, fmt.Errorf("bundle: sidecar: %w", err)
	}
	return view, nil
}

// writeZip 固定序写包（包内相对路径 / 分隔——生成侧保证，无 .. 无绝对）。
func writeZip(dest string, order []string, rendered map[string][]byte) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("bundle: open tmp: %w", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, name := range order {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write(rendered[name]); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// List 某 Agent 的导出列表（新→旧；边车投影，坏边车跳过不阻断）。
func (s *Service) List(agentID string) ([]Summary, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "bundles"))
	if err != nil {
		if os.IsNotExist(err) {
			return []Summary{}, nil
		}
		return nil, err
	}
	var out []Summary
	for _, e := range entries {
		if !e.IsDir() || !bundleIDRe.MatchString(e.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, "bundles", e.Name(), sidecarName))
		if err != nil {
			continue
		}
		var v View
		if json.Unmarshal(raw, &v) != nil || v.AgentID != agentID {
			continue
		}
		out = append(out, Summary{BundleID: v.BundleID, AgentID: v.AgentID, CreatedAt: v.CreatedAt, SizeBytes: v.SizeBytes, Counts: v.Counts})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].BundleID > out[j].BundleID
	})
	return out, nil
}

// Get 导出详情（bundle_id 先过白名单再拼路径——防穿越，backup 纪律）。
func (s *Service) Get(bundleID string) (View, error) {
	if !bundleIDRe.MatchString(bundleID) {
		return View{}, fmt.Errorf("%w: bundle_id 不合法", ErrInvalid)
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, "bundles", bundleID, sidecarName))
	if err != nil {
		if os.IsNotExist(err) {
			return View{}, ErrNotFound
		}
		return View{}, err
	}
	var v View
	if err := json.Unmarshal(raw, &v); err != nil {
		return View{}, fmt.Errorf("bundle: sidecar 损坏：%s", bundleID)
	}
	return v, nil
}

// nonNilSlots 空数组兜底（nil → []——0.7.0 一审 #1 同纪律）。
func nonNilSlots(v []SlotFact) []SlotFact {
	if v == nil {
		return []SlotFact{}
	}
	return v
}

// artifactRefsOf 任务的工件引用（契约来源 + 检查点来源并集去重保序）。
func artifactRefsOf(m MissionFact, ckpts map[string]CheckpointFact) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(refs []string) {
		for _, r := range refs {
			if r != "" && !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	add(m.Contract.SourceRefs)
	if c, ok := ckpts[m.MissionID]; ok {
		add(c.ArtifactRefs)
	}
	return out
}

// depsOfSlots 依赖面的槽位投影（版本 + 运行要求 + 工具要求）。
func depsOfSlots(slots []SlotFact) []map[string]any {
	out := make([]map[string]any, 0, len(slots))
	for _, sl := range slots {
		e := map[string]any{
			"slot_spec_id": sl.SlotSpecID, "version": sl.SlotSpecVersion,
			"binding_status": sl.Status,
		}
		if sl.Spec.Runtime != nil {
			e["runtime_required"] = sl.Spec.Runtime.Required
			e["workspace_access"] = sl.Spec.Runtime.WorkspaceAccess
		}
		if sl.Spec.Tools != nil {
			e["tools_required"] = sl.Spec.Tools.Required
		}
		out = append(out, e)
	}
	return out
}
