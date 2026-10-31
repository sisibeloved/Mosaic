// Package execctx：执行上下文清单（0.6.2 / ADR-0025）。
// ContextManifest 固定本次执行输入的引用与版本并**存档装配内容**（0.6.5
// Runtime 执行存档输入，不采信现场重装）；ContextReceipt 记交付账目不复制
// 完整 Prompt。装配确定性：同状态必同 digest——重建重放原清单，状态变化
// 追加新行（每 Mission 保留"各时点会交付什么"的历史）。授权过滤先于排序、
// 摘要与计数（RFC-0018 §7.3）：非 public 事件不进入、不计数、不列名。
//
// 与 internal/contextx（RFC-0007 房间对话七层组装）不是同一对象：那是
// 讨论房间消息流的对话上下文；本包是 Mission 执行输入的事实存档。
package execctx

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/sisibeloved/Mosaic/internal/mission"
	"github.com/sisibeloved/Mosaic/internal/slot"
)

// 装配政策版本（层表/配额/渲染规则任何变更须升版——回执与清单同源记录）。
const PolicyVersion = "context-assembly/1"

// 预算（RFC-0007 §3.1.4 沿用 RFC-0002：总输入上限 256 KiB；适配器声明
// 上限取较小者随 0.6.3 Runtime 落）。可选层独立配额：近期消息 64 KiB、
// 单条上限 8 KiB（超限取头部并如实记 truncated_messages）。
const (
	TotalBudgetBytes         = 256 << 10
	RecentMessagesQuotaBytes = 64 << 10
	MessageCapBytes          = 8 << 10
)

// 清单状态（blocked 是数据不是错误——probe 纪律，ADR-0025 决策 4）。
const (
	StateAssembled = "assembled"
	StateBlocked   = "blocked"
)

// 层角色与状态。
const (
	RoleRequired  = "required"
	RoleOptional  = "optional"
	LayerIncluded = "included"
	LayerOmitted  = "omitted"
)

// 层名（RFC §7.3 组装顺序；decision_capsule/checkpoint/memory 为未建模层
// ——账本如实记 omitted，内容随 0.6.5/0.6.11/0.7 增列）。
const (
	LayerConstraints     = "constraints"
	LayerResponsibility  = "responsibility"
	LayerContract        = "contract"
	LayerDecisionCapsule = "decision_capsule"
	LayerCheckpoint      = "checkpoint"
	LayerMemory          = "memory"
	LayerSources         = "sources"
	LayerRecentMessages  = "recent_messages"
	LayerTools           = "tools"
)

// blocked 原因（固定优先级，先命中先记录）。
const (
	ReasonOwnerMismatch      = "owner_mismatch"       // 执行者须为契约负责人
	ReasonBindingNotActive   = "binding_not_active"   // 任职 ∉ {ready, active}
	ReasonReadinessNotMet    = "readiness_not_met"    // 授权交集缺必需项（层内携明细）
	ReasonRequiredOverBudget = "required_over_budget" // 必需层合计超总预算
)

// omitted 原因。
const (
	ReasonNoSourceRoom               = "no_source_room"
	ReasonCheckpointUnavailable      = "checkpoint_unavailable"
	ReasonMemoryUnavailable          = "memory_unavailable"
	ReasonDecisionCapsuleUnavailable = "decision_capsule_unavailable"
)

var bindingIDPattern = regexp.MustCompile(`^bd_[0-9A-Za-z_-]+$`)

// 领域错误。
var (
	// ErrInvalidRequest 装配请求不合法（binding_id 缺失/格式）。
	ErrInvalidRequest = errors.New("execctx: 装配请求不合法")
	// ErrMissionNotFound Mission 不存在。
	ErrMissionNotFound = errors.New("execctx: Mission 不存在")
	// ErrBindingNotFound 任职不存在。
	ErrBindingNotFound = errors.New("execctx: 任职不存在")
	// ErrMissionCanceled Mission 已取消（终态无执行语义）。
	ErrMissionCanceled = errors.New("execctx: Mission 已取消")
	// ErrManifestExists 同 (mission_id, digest) 清单已存（确定性重建命中）。
	ErrManifestExists = errors.New("execctx: 清单已存在")
)

// RoomEvent 房间事件读路径 DTO（适配器全量透传，本包做授权过滤——过滤
// 先于排序与计数是本域不变量；adapter 预滤会让不变量失去测试钉子）。
type RoomEvent struct {
	Seq           int64  `json:"seq"`
	EventID       string `json:"event_id"`
	OccurredAt    string `json:"occurred_at"`
	Type          string `json:"type"`
	Visibility    string `json:"visibility"`
	ParticipantID string `json:"participant_id"`
	ActorKind     string `json:"actor_kind"`
	Body          string `json:"body"`
}

// ---- 层内容（typed——json.Marshal 字段序固定，digest 可复算） ----

// DelegationView 有效委派策略（模板∩契约；任一不允许即不允许——安全缺省）。
type DelegationView struct {
	Allowed  []string `json:"allowed"`
	MaxDepth int      `json:"max_depth"`
}

// ConstraintsContent 约束与有效权限：readiness 报告（ADR-0022 交集单源，
// 含缺失/降级明细）+ 有效格栅 + 有效工具 + 有效委派 + 预算声明。
type ConstraintsContent struct {
	Readiness                slot.ReadinessReport `json:"readiness"`
	EffectiveWorkspaceAccess string               `json:"effective_workspace_access"`
	EffectiveTools           []string             `json:"effective_tools"`
	Delegation               DelegationView       `json:"delegation"`
	Budget                   mission.Budget       `json:"budget"`
}

// ResponsibilityContent 当前职责：任职 + 钉版本模板职责文本。
type ResponsibilityContent struct {
	SpecName         string          `json:"spec_name"`
	Responsibility   string          `json:"responsibility"`
	SpecDeliverables []string        `json:"spec_deliverables"`
	SpecSkills       []slot.SkillRef `json:"spec_skills"`
	Binding          slot.Binding    `json:"binding"`
}

// ContractContent 任务契约/验收：钉住版本行的全量契约。
type ContractContent struct {
	mission.Mission
}

// ToolsContent 工具接口：有效工具清单（模板声明序 × 授权交集）。
type ToolsContent struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

// SourcesContent 证据引用：source_refs 原文 + 结构化来源（内容解析随 0.6.5+）。
type SourcesContent struct {
	Refs       []string            `json:"refs"`
	RoomSource *mission.RoomSource `json:"room_source"`
}

// MessageEntry 近期消息条目。
type MessageEntry struct {
	Seq           int64  `json:"seq"`
	EventID       string `json:"event_id"`
	OccurredAt    string `json:"occurred_at"`
	ParticipantID string `json:"participant_id"`
	ActorKind     string `json:"actor_kind"`
	Body          string `json:"body"`
}

// RecentMessagesContent 近期消息窗（时序旧→新；配额内从新往旧装满）。
type RecentMessagesContent struct {
	Messages []MessageEntry `json:"messages"`
}

// Layer 层账本条目。
type Layer struct {
	Name              string `json:"name"`
	Role              string `json:"role"`
	State             string `json:"state"`
	Bytes             int    `json:"bytes"`
	ReasonCode        string `json:"reason_code,omitempty"`
	Dropped           int    `json:"dropped,omitempty"`
	TruncatedMessages int    `json:"truncated_messages,omitempty"`
	Content           any    `json:"content,omitempty"`
}

// Watermark 水位向量分量（跨流水位；契约/模板版本即水位已单列于清单）。
type Watermark struct {
	Stream   string `json:"stream"` // room:{room_id}
	Position int64  `json:"position"`
}

// Manifest 执行上下文清单（存档装配内容——Runtime 消费对象）。
type Manifest struct {
	ManifestID      string      `json:"manifest_id"`
	MissionID       string      `json:"mission_id"`
	ContractVersion int         `json:"contract_version"`
	BindingID       string      `json:"binding_id"`
	AgentID         string      `json:"agent_id"`
	State           string      `json:"state"`
	ReasonCode      string      `json:"reason_code"`
	Digest          string      `json:"digest"`
	PolicyVersion   string      `json:"policy_version"`
	Layers          []Layer     `json:"layers"`
	Watermarks      []Watermark `json:"watermarks"`
	TotalBytes      int         `json:"total_bytes"`
	CreatedAt       string      `json:"created_at"`
}

// AccountingEntry 回执交付账目条目——只记账不复制内容。
type AccountingEntry struct {
	Name              string `json:"name"`
	State             string `json:"state"`
	Bytes             int    `json:"bytes"`
	ReasonCode        string `json:"reason_code,omitempty"`
	Dropped           int    `json:"dropped,omitempty"`
	TruncatedMessages int    `json:"truncated_messages,omitempty"`
}

// Receipt 交付回执（run_id 随 0.6.5 Run 对象存在时增列——不落空壳）。
type Receipt struct {
	ReceiptID     string            `json:"receipt_id"`
	ManifestID    string            `json:"manifest_id"`
	Digest        string            `json:"digest"`
	PolicyVersion string            `json:"policy_version"`
	Accounting    []AccountingEntry `json:"accounting"`
	TotalBytes    int               `json:"total_bytes"`
	CreatedAt     string            `json:"created_at"`
}

// BuildResult 装配结果——Replayed=true 为确定性重建命中既有清单。
type BuildResult struct {
	Manifest Manifest `json:"manifest"`
	Receipt  Receipt  `json:"receipt"`
	Replayed bool     `json:"replayed"`
}

// BindingFacts 任职事实：任职 + 其钉住版本（SlotSpecVersion）模板版本行。
type BindingFacts struct {
	Binding slot.Binding
	Spec    slot.SlotSpec
}

// Missions 契约读端口（单源 mission.Service）。
type Missions interface {
	Get(ctx context.Context, missionID string) (mission.Mission, bool, error)
}

// Bindings 任职事实端口（app 装配：任职→实例→钉版本模板行一次解析）。
type Bindings interface {
	Facts(ctx context.Context, bindingID string) (BindingFacts, bool, error)
}

// Readiness 准入报告端口（ADR-0022 GrantReadinessChecker 复用——
// 就绪判定单源；工具清单不在此重算，经 Grants 端口取授权档案）。
type Readiness interface {
	Check(ctx context.Context, candidate slot.Binding) (slot.ReadinessReport, error)
}

// Grants 授权档案读端口（工具清单 = 模板声明 ∩ 授权——清单是库存不是
// 就绪判定：required 缺失 → blocked 仍由 readiness 报告单源裁决）。
type Grants interface {
	GrantByAgent(ctx context.Context, agentID string) (slot.AgentGrant, bool, error)
}

// RoomReader 房间事件读端口（app 装配：EventReader 跨页累积全量后映射）。
type RoomReader interface {
	RoomEvents(ctx context.Context, roomID string) ([]RoomEvent, error)
}

// Ports 外部事实端口。
type Ports struct {
	Missions  Missions
	Bindings  Bindings
	Readiness Readiness
	Grants    Grants
	Rooms     RoomReader
}

// Store 持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
type Store interface {
	// CreateManifest 清单与回执同事务落库；(mission_id, digest) 撞键
	// ErrManifestExists（确定性重建——服务层转重放）。
	CreateManifest(ctx context.Context, m Manifest, r Receipt) error
	// ManifestByDigest 按 (mission_id, digest) 取清单与回执；不存在 ok=false。
	ManifestByDigest(ctx context.Context, missionID, digest string) (Manifest, Receipt, bool, error)
	// ManifestByID 单查（含回执）；不存在 ok=false。
	ManifestByID(ctx context.Context, manifestID string) (Manifest, Receipt, bool, error)
	// ManifestsByMission 装配历史（创建序倒序）。
	ManifestsByMission(ctx context.Context, missionID string) ([]Manifest, error)
}

// Option 服务选项。
type Option func(*Service)

// WithNow 注入时钟（缺省 UTC RFC3339Nano）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewIDs 注入清单/回执 ID 生成器（测试定序用）。
func WithNewIDs(manifest, receipt func() string) Option {
	return func(s *Service) { s.newManifestID, s.newReceiptID = manifest, receipt }
}

// Service 执行上下文装配服务。
type Service struct {
	store         Store
	ports         Ports
	now           func() string
	newManifestID func() string
	newReceiptID  func() string
}

// New 装配服务。
func New(store Store, p Ports, opts ...Option) *Service {
	s := &Service{
		store:         store,
		ports:         p,
		now:           func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newManifestID: func() string { return "cmf_" + randomSuffix() },
		newReceiptID:  func() string { return "cxr_" + randomSuffix() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func randomSuffix() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// Build 装配并存档（ADR-0025 决策 2/4/5）。blocked 恒为结果不是错误；
// 错误出口仅：请求不合法、Mission/任职不存在、Mission 已取消、端口故障。
func (s *Service) Build(ctx context.Context, missionID, bindingID string) (BuildResult, error) {
	if !bindingIDPattern.MatchString(bindingID) {
		return BuildResult{}, fmt.Errorf("%w：binding_id 须 bd_ 前缀（%q）", ErrInvalidRequest, bindingID)
	}
	m, ok, err := s.ports.Missions.Get(ctx, missionID)
	if err != nil {
		return BuildResult{}, fmt.Errorf("execctx: 读契约失败：%w", err)
	}
	if !ok {
		return BuildResult{}, ErrMissionNotFound
	}
	if m.Status == mission.StatusCanceled {
		return BuildResult{}, ErrMissionCanceled
	}
	facts, ok, err := s.ports.Bindings.Facts(ctx, bindingID)
	if err != nil {
		return BuildResult{}, fmt.Errorf("execctx: 读任职失败：%w", err)
	}
	if !ok {
		return BuildResult{}, ErrBindingNotFound
	}
	report, err := s.ports.Readiness.Check(ctx, facts.Binding)
	if err != nil {
		return BuildResult{}, fmt.Errorf("execctx: 准入核对失败：%w", err)
	}
	grant, _, err := s.ports.Grants.GrantByAgent(ctx, facts.Binding.AgentID)
	if err != nil {
		return BuildResult{}, fmt.Errorf("execctx: 读授权档案失败：%w", err)
	}

	layers, watermarks, err := s.assemble(ctx, m, facts, report, grant)
	if err != nil {
		return BuildResult{}, err
	}

	// blocked 优先级（ADR-0025 决策 4——固定序，先命中先记录）。
	state, reason := StateAssembled, ""
	switch {
	case facts.Binding.AgentID != m.OwnerAgentID:
		state, reason = StateBlocked, ReasonOwnerMismatch
	case facts.Binding.Status != slot.BindingReady && facts.Binding.Status != slot.BindingActive:
		state, reason = StateBlocked, ReasonBindingNotActive
	case !report.Ready:
		state, reason = StateBlocked, ReasonReadinessNotMet
	case requiredBytes(layers) > TotalBudgetBytes:
		state, reason = StateBlocked, ReasonRequiredOverBudget
	}

	now := s.now()
	manifest := Manifest{
		ManifestID: s.newManifestID(), MissionID: m.MissionID, ContractVersion: m.ContractVersion,
		BindingID: facts.Binding.BindingID, AgentID: facts.Binding.AgentID,
		State: state, ReasonCode: reason, PolicyVersion: PolicyVersion,
		Layers: layers, Watermarks: watermarks, TotalBytes: totalBytes(layers), CreatedAt: now,
	}
	manifest.Digest = digestOf(manifest, facts.Binding.SlotSpecVersion)
	receipt := Receipt{
		ReceiptID: s.newReceiptID(), ManifestID: manifest.ManifestID, Digest: manifest.Digest,
		PolicyVersion: PolicyVersion, Accounting: accountingOf(layers), TotalBytes: manifest.TotalBytes, CreatedAt: now,
	}
	if err := s.store.CreateManifest(ctx, manifest, receipt); err != nil {
		if errors.Is(err, ErrManifestExists) {
			// digest 即身份：确定性重建命中 → 重放原清单与回执。
			em, er, ok, gerr := s.store.ManifestByDigest(ctx, missionID, manifest.Digest)
			if gerr != nil {
				return BuildResult{}, fmt.Errorf("execctx: 重放查询失败：%w", gerr)
			}
			if ok {
				return BuildResult{Manifest: em, Receipt: er, Replayed: true}, nil
			}
			return BuildResult{}, fmt.Errorf("execctx: 撞键清单不可查（%s/%s）", missionID, manifest.Digest)
		}
		return BuildResult{}, fmt.Errorf("execctx: 清单落库失败：%w", err)
	}
	return BuildResult{Manifest: manifest, Receipt: receipt}, nil
}

// Get 清单详情（含回执）。
func (s *Service) Get(ctx context.Context, manifestID string) (Manifest, Receipt, bool, error) {
	return s.store.ManifestByID(ctx, manifestID)
}

// ListByMission 装配历史（创建序倒序；Mission 未知 ErrMissionNotFound）。
func (s *Service) ListByMission(ctx context.Context, missionID string) ([]Manifest, error) {
	if _, ok, err := s.ports.Missions.Get(ctx, missionID); err != nil {
		return nil, fmt.Errorf("execctx: 读契约失败：%w", err)
	} else if !ok {
		return nil, ErrMissionNotFound
	}
	return s.store.ManifestsByMission(ctx, missionID)
}

// assemble 按 RFC §7.3 组装顺序产出层账本（blocked 也存完整账本）。
func (s *Service) assemble(ctx context.Context, m mission.Mission, facts BindingFacts, report slot.ReadinessReport, grant slot.AgentGrant) ([]Layer, []Watermark, error) {
	spec := facts.Spec.SpecContent

	constraints := ConstraintsContent{
		Readiness:                report,
		EffectiveWorkspaceAccess: report.EffectiveWorkspaceAccess,
		EffectiveTools:           effectiveToolsOf(spec, grant),
		Delegation:               effectiveDelegation(spec.Delegation, m.Delegation),
		Budget:                   m.Budget,
	}
	responsibility := ResponsibilityContent{
		SpecName: spec.Name, Responsibility: spec.Responsibility,
		SpecDeliverables: spec.Deliverables, SpecSkills: spec.Skills,
		Binding: facts.Binding,
	}
	tools := ToolsContent{
		Required: intersectCaps(spec.Tools.Required, grant.Tools),
		Optional: intersectCaps(spec.Tools.Optional, grant.Tools),
	}
	layers := []Layer{
		includedLayer(LayerConstraints, RoleRequired, constraints),
		includedLayer(LayerResponsibility, RoleRequired, responsibility),
		includedLayer(LayerContract, RoleRequired, ContractContent{Mission: m}),
		omittedLayer(LayerDecisionCapsule, ReasonDecisionCapsuleUnavailable),
		omittedLayer(LayerCheckpoint, ReasonCheckpointUnavailable),
		omittedLayer(LayerMemory, ReasonMemoryUnavailable),
		includedLayer(LayerSources, RoleOptional, SourcesContent{Refs: m.SourceRefs, RoomSource: m.RoomSource}),
	}

	var watermarks []Watermark
	if m.RoomSource == nil {
		layers = append(layers, omittedLayer(LayerRecentMessages, ReasonNoSourceRoom))
	} else {
		events, err := s.ports.Rooms.RoomEvents(ctx, m.RoomSource.RoomID)
		if err != nil {
			return nil, nil, fmt.Errorf("execctx: 读来源房间失败：%w", err)
		}
		content, dropped, truncated, wms := assembleMessages(events, m.RoomSource.RoomID)
		watermarks = wms
		l := includedLayer(LayerRecentMessages, RoleOptional, content)
		l.Dropped, l.TruncatedMessages = dropped, truncated
		layers = append(layers, l)
	}

	layers = append(layers, includedLayer(LayerTools, RoleRequired, tools))
	return layers, watermarks, nil
}

// assembleMessages 授权过滤先于排序与计数：只收 public 可见 message.posted；
// 从新往旧装满配额即停（dropped = 被裁的更旧条数）；单条超限取头部记截断；
// 呈现时序旧→新；水位 = 实际交付的最新 seq（交付前沿——非 public 不参与）。
func assembleMessages(events []RoomEvent, roomID string) (RecentMessagesContent, int, int, []Watermark) {
	var pub []RoomEvent
	for _, e := range events {
		if e.Visibility == "public" && e.Type == "message.posted" {
			pub = append(pub, e)
		}
	}
	var sel []MessageEntry
	used, dropped, truncated := 0, 0, 0
	for i := len(pub) - 1; i >= 0; i-- {
		e := pub[i]
		body, cut := capBody(e.Body)
		entry := MessageEntry{
			Seq: e.Seq, EventID: e.EventID, OccurredAt: e.OccurredAt,
			ParticipantID: e.ParticipantID, ActorKind: e.ActorKind, Body: body,
		}
		sz := jsonSize(entry)
		if len(sel) > 0 && used+sz > RecentMessagesQuotaBytes {
			dropped = i + 1 // 索引 0..i（更旧）被配额裁剪
			break
		}
		sel = append(sel, entry)
		used += sz
		if cut {
			truncated++
		}
	}
	for i, j := 0, len(sel)-1; i < j; i, j = i+1, j-1 {
		sel[i], sel[j] = sel[j], sel[i]
	}
	var wms []Watermark
	if len(sel) > 0 {
		wms = append(wms, Watermark{Stream: "room:" + roomID, Position: sel[len(sel)-1].Seq})
	}
	return RecentMessagesContent{Messages: sel}, dropped, truncated, wms
}

// capBody 单条上限（UTF-8 边界回退——不撕裂多字节字符）。
func capBody(body string) (string, bool) {
	if len(body) <= MessageCapBytes {
		return body, false
	}
	b := []byte(body)[:MessageCapBytes]
	for len(b) > 0 {
		if r, _ := utf8.DecodeLastRune(b); r != utf8.RuneError {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b), true
}

func includedLayer(name, role string, content any) Layer {
	return Layer{Name: name, Role: role, State: LayerIncluded, Bytes: jsonSize(content), Content: content}
}

func omittedLayer(name, reason string) Layer {
	return Layer{Name: name, Role: RoleOptional, State: LayerOmitted, ReasonCode: reason}
}

func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func requiredBytes(layers []Layer) int {
	n := 0
	for _, l := range layers {
		if l.Role == RoleRequired && l.State == LayerIncluded {
			n += l.Bytes
		}
	}
	return n
}

func totalBytes(layers []Layer) int {
	n := 0
	for _, l := range layers {
		if l.State == LayerIncluded {
			n += l.Bytes
		}
	}
	return n
}

func accountingOf(layers []Layer) []AccountingEntry {
	out := make([]AccountingEntry, 0, len(layers))
	for _, l := range layers {
		out = append(out, AccountingEntry{
			Name: l.Name, State: l.State, Bytes: l.Bytes, ReasonCode: l.ReasonCode,
			Dropped: l.Dropped, TruncatedMessages: l.TruncatedMessages,
		})
	}
	return out
}

// digestOf 清单摘要：层账本（含内容）+ 水位 + 钉住版本——同状态必同摘要
// （typed 内容 json.Marshal 字段序固定；created_at 与 ID 不参与）。
func digestOf(m Manifest, slotSpecVersion int) string {
	in := struct {
		MissionID       string      `json:"mission_id"`
		ContractVersion int         `json:"contract_version"`
		BindingID       string      `json:"binding_id"`
		SlotSpecVersion int         `json:"slot_spec_version"`
		Layers          []Layer     `json:"layers"`
		Watermarks      []Watermark `json:"watermarks"`
	}{m.MissionID, m.ContractVersion, m.BindingID, slotSpecVersion, m.Layers, m.Watermarks}
	b, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// effectiveDelegation 有效委派：模板∩契约（任一不允许即不允许——安全缺省）。
func effectiveDelegation(spec *slot.DelegationPolicy, m mission.DelegationPolicy) DelegationView {
	if spec == nil || len(spec.Allowed) == 0 || len(m.Allowed) == 0 {
		return DelegationView{Allowed: []string{}, MaxDepth: 0}
	}
	set := make(map[string]bool, len(m.Allowed))
	for _, a := range m.Allowed {
		set[a] = true
	}
	var allowed []string
	for _, a := range spec.Allowed {
		if set[a] {
			allowed = append(allowed, a)
		}
	}
	if len(allowed) == 0 {
		return DelegationView{Allowed: []string{}, MaxDepth: 0}
	}
	depth := spec.MaxDepth
	if m.MaxDepth < depth {
		depth = m.MaxDepth
	}
	return DelegationView{Allowed: allowed, MaxDepth: depth}
}

// effectiveToolsOf 有效工具清单（声明序：required 在前、optional 在后，
// 各自 ∩ 授权集——无声明或无授权即空，不虚构能力）。
func effectiveToolsOf(spec slot.SpecContent, grant slot.AgentGrant) []string {
	out := intersectCaps(specRequiredTools(spec), grant.Tools)
	return append(out, intersectCaps(specOptionalTools(spec), grant.Tools)...)
}

func specRequiredTools(spec slot.SpecContent) []string {
	if spec.Tools == nil {
		return nil
	}
	return spec.Tools.Required
}

func specOptionalTools(spec slot.SpecContent) []string {
	if spec.Tools == nil {
		return nil
	}
	return spec.Tools.Optional
}

// intersectCaps 声明序保留 ∩ 授权集（有效工具——readiness 报告单源）。
func intersectCaps(declared []string, effective []string) []string {
	if len(declared) == 0 {
		return []string{}
	}
	set := make(map[string]bool, len(effective))
	for _, t := range effective {
		set[t] = true
	}
	out := make([]string, 0, len(declared))
	for _, t := range declared {
		if set[t] {
			out = append(out, t)
		}
	}
	return out
}
