// Package slot 职责域（RFC-0018 §5 / ADR-0019，0.5.2）。
// SlotSpec 是"承担什么职责、需要什么能力、如何交付"的版本化模板：不可变
// 版本行（(slot_spec_id, version) 主键），修订即追加，历史永不清除。模板只
// 描述能力要求（required/optional 分层），实际连接由 0.5.4 Binding 解析；
// 本版不含 SlotInstance / Binding / 准入行为。
package slot

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 领域错误。
var (
	// ErrInvalidSpec 内容或谱系 ID 不合法（校验单源见 validate）。
	ErrInvalidSpec = errors.New("slot: 模板内容不合法")
	// ErrSpecExists 谱系已存在（建谱系唯一冲突；演进走 AppendVersion）。
	ErrSpecExists = errors.New("slot: 谱系已存在")
	// ErrNotFound 谱系不存在（兼容既有语义）。
	ErrNotFound = errors.New("slot: 谱系不存在")
	// ErrInstanceNotFound 职责实例不存在（任职路径细分）。
	ErrInstanceNotFound = fmt.Errorf("%w：实例不存在", ErrNotFound)
	// ErrAgentNotFound Agent 不存在（任职路径细分）。
	ErrAgentNotFound = fmt.Errorf("%w：Agent 不存在", ErrNotFound)
	// ErrBindingNotFound 任职记录不存在（细分）。
	ErrBindingNotFound = fmt.Errorf("%w：任职记录不存在", ErrNotFound)
)

const SchemaVersion = 1

// SkillRef 版本化工作方法引用（仅引用——可审查，不含可执行体）。
type SkillRef struct {
	Ref     string `json:"ref"`
	Version int    `json:"version"`
}

// CapabilityList 能力/上下文域要求分层：required 缺失即 blocked、optional
// 缺失按声明降级（AT-07 准入预埋，0.5.5 消费）；两者不得相交。
type CapabilityList struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

// DelegationPolicy 委派策略；缺省语义 = 不允许委派（安全缺省）。
type DelegationPolicy struct {
	Allowed  []string `json:"allowed"`
	MaxDepth int      `json:"max_depth"`
}

// RuntimeRequirements 运行能力要求（能力名，非安装路径）。
type RuntimeRequirements struct {
	Required        []string `json:"required"`
	WorkspaceAccess string   `json:"workspace_access"` // none | read | read_write
}

// HandoffRequirements 交接必备件。
type HandoffRequirements struct {
	Required []string `json:"required"`
}

// SpecContent 模板内容（RFC-0018 §5.1 正式定形）。
type SpecContent struct {
	Name           string               `json:"name"`
	Responsibility string               `json:"responsibility"`
	Deliverables   []string             `json:"deliverables"`
	Skills         []SkillRef           `json:"skills,omitempty"`
	Tools          *CapabilityList      `json:"tools,omitempty"`
	Context        *CapabilityList      `json:"context,omitempty"`
	Delegation     *DelegationPolicy    `json:"delegation,omitempty"`
	Runtime        *RuntimeRequirements `json:"runtime,omitempty"`
	Handoff        *HandoffRequirements `json:"handoff,omitempty"`
}

// SlotSpec 不可变版本行（schema: SlotSpecView——内容字段平铺）。
type SlotSpec struct {
	SlotSpecID    string `json:"slot_spec_id"`
	Version       int    `json:"version"`
	SchemaVersion int    `json:"schema_version"`
	SpecContent
	CreatedAt string `json:"created_at"`
}

// SpecSummary 谱系摘要（schema: SlotSpecSummary）。
type SpecSummary struct {
	SlotSpecID    string `json:"slot_spec_id"`
	Name          string `json:"name"`
	LatestVersion int    `json:"latest_version"`
	VersionCount  int    `json:"version_count"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// Store 持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
type Store interface {
	// CreateSpec 建谱系（首版）；谱系已存在 ErrSpecExists。
	CreateSpec(ctx context.Context, spec SlotSpec) (SlotSpec, error)
	// AppendVersion 追加下一不可变版本（version=latest+1，BEGIN IMMEDIATE
	// 临界区内取号——并发追加串行化为连续版本）；谱系未知 ErrNotFound。
	AppendVersion(ctx context.Context, slotSpecID string, content SpecContent, now string) (SlotSpec, error)
	// ListSummaries 谱系摘要（创建序）。
	ListSummaries(ctx context.Context) ([]SpecSummary, error)
	// Latest 最新版本；谱系未知 ok=false。
	Latest(ctx context.Context, slotSpecID string) (SlotSpec, bool, error)
	// Versions 全版本升序；谱系未知 ok=false。
	Versions(ctx context.Context, slotSpecID string) ([]SlotSpec, bool, error)
	// CreateInstance 建职责实例（knowledge_namespace 唯一约束兜底生成器碰撞）。
	CreateInstance(ctx context.Context, inst SlotInstance) error
	// ListInstances 实例全量（创建序）。
	ListInstances(ctx context.Context) ([]SlotInstance, error)
	// InstanceByID 单查实例；未知 ok=false。
	InstanceByID(ctx context.Context, slotInstanceID string) (SlotInstance, bool, error)
	// LatestByLineage 各谱系最新版聚合（实例列表的现势连接单源）。
	LatestByLineage(ctx context.Context) (map[string]InstanceSpecNow, error)
	// CreateBinding 建任职行（preparing + created 事件同事务）。
	CreateBinding(ctx context.Context, b Binding) (Binding, error)
	// BindingByID 单查任职；未知 ok=false。
	BindingByID(ctx context.Context, bindingID string) (Binding, bool, error)
	// ListBindings 任职列表（创建序）；instanceID 非空时按实例过滤。
	ListBindings(ctx context.Context, instanceID string) ([]Binding, error)
	// TransitionBinding 事务转移：状态在 allowedFrom 内才更新投影并追加
	// status_changed 事件（extras 并入事件 payload）；激活撞单 active 部分
	// 唯一索引 → ErrActiveBindingExists；ok=false = 状态不匹配。
	TransitionBinding(ctx context.Context, bindingID string, allowedFrom []string, to string, extras map[string]string, occurredAt string) (from string, ok bool, err error)
	// ReleaseBinding 原子释放（active → draining → released 两段事件与终态
	// 同一事务一次提交，含交接备注入投影）；ok=false = 非 active。
	ReleaseBinding(ctx context.Context, bindingID string, handoffNote, occurredAt string) (from string, ok bool, err error)
	// BindingEvents 任职事件流（seq 升序）。
	BindingEvents(ctx context.Context, bindingID string) ([]BindingEvent, error)
}

// Service 职责模板服务：校验单源 + 版本化存取。
type Service struct {
	store         Store
	now           func() string
	newInstanceID func() string
	newNamespace  func() string
	newBindingID  func() string
	readiness     ReadinessChecker // 准入门（0.5.5；缺省 nil = 恒通过）
	drain         DrainChecker     // 排空门（0.6；缺省 nil = 恒空）
	agentLookup   AgentLookup      // 身份域查询（nil = 任职端点不可用）
}

// Option 装配选项（测试注入确定性时钟）。
type Option func(*Service)

// WithNow 注入时钟（缺省 RFC3339Nano UTC）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// New 构造服务。
func New(store Store, opts ...Option) *Service {
	s := &Service{
		store:         store,
		now:           func() string { return nowUTC() },
		newInstanceID: newInstanceID,
		newNamespace:  newNamespace,
		newBindingID:  newBindingID,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithNewInstanceID 注入实例 ID 生成器（缺省：si_ + 时间有序 + 随机后缀）。
func WithNewInstanceID(f func() string) Option { return func(s *Service) { s.newInstanceID = f } }

// WithNewNamespace 注入知识命名空间生成器（缺省：slotns_ + 时间有序 + 随机）。
func WithNewNamespace(f func() string) Option { return func(s *Service) { s.newNamespace = f } }

// WithNewBindingID 注入任职 ID 生成器（缺省：bd_ + 时间有序 + 随机后缀）。
func WithNewBindingID(f func() string) Option { return func(s *Service) { s.newBindingID = f } }

// WithReadiness 注入准入门（0.5.5 权限交集/能力核对；缺省 nil = 恒通过）。
func WithReadiness(c ReadinessChecker) Option { return func(s *Service) { s.readiness = c } }

// WithDrain 注入排空门（0.6 在途工作；缺省 nil = 恒空）。
func WithDrain(c DrainChecker) Option { return func(s *Service) { s.drain = c } }

// WithAgentLookup 注入身份域查询端口（任职需要 Agent 存在且 active）。
func WithAgentLookup(l AgentLookup) Option { return func(s *Service) { s.agentLookup = l } }

// Create 建谱系（首版 version=1）。
func (s *Service) Create(ctx context.Context, slotSpecID string, c SpecContent) (SlotSpec, error) {
	if err := validateSlotSpecID(slotSpecID); err != nil {
		return SlotSpec{}, err
	}
	if err := validateContent(c); err != nil {
		return SlotSpec{}, err
	}
	return s.store.CreateSpec(ctx, SlotSpec{
		SlotSpecID: slotSpecID, Version: 1, SchemaVersion: SchemaVersion,
		SpecContent: normalize(c), CreatedAt: s.now(),
	})
}

// AppendVersion 追加下一不可变版本（同内容仍新版本——快照语义）。
func (s *Service) AppendVersion(ctx context.Context, slotSpecID string, c SpecContent) (SlotSpec, error) {
	if err := validateContent(c); err != nil {
		return SlotSpec{}, err
	}
	return s.store.AppendVersion(ctx, slotSpecID, normalize(c), s.now())
}

// List 谱系摘要列表。
func (s *Service) List(ctx context.Context) ([]SpecSummary, error) {
	return s.store.ListSummaries(ctx)
}

// Latest 最新版本；谱系未知 ok=false。
func (s *Service) Latest(ctx context.Context, slotSpecID string) (SlotSpec, bool, error) {
	return s.store.Latest(ctx, slotSpecID)
}

// Versions 全版本升序；谱系未知 ok=false。
func (s *Service) Versions(ctx context.Context, slotSpecID string) ([]SlotSpec, bool, error) {
	return s.store.Versions(ctx, slotSpecID)
}

// ---- 校验（单源：必填/上限/模式/枚举/required-optional 不相交） ----

var (
	specIDPattern     = regexp.MustCompile(`^slot\.[a-z0-9][a-z0-9._-]{0,62}$`)
	skillRefPattern   = regexp.MustCompile(`^[a-zA-Z0-9/_.-]{1,128}$`)
	capabilityPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

func validateSlotSpecID(id string) error {
	if !specIDPattern.MatchString(id) {
		return fmt.Errorf("%w: slot_spec_id 须为 slot. 前缀小写 slug（%s）", ErrInvalidSpec, id)
	}
	return nil
}

func validateContent(c SpecContent) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Name)); n < 1 || n > 64 {
		return fmt.Errorf("%w: name 须 1–64 字", ErrInvalidSpec)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Responsibility)); n < 1 || n > 512 {
		return fmt.Errorf("%w: responsibility 须 1–512 字", ErrInvalidSpec)
	}
	if len(c.Deliverables) < 1 || len(c.Deliverables) > 16 {
		return fmt.Errorf("%w: deliverables 须 1–16 项", ErrInvalidSpec)
	}
	for i, d := range c.Deliverables {
		if n := utf8.RuneCountInString(strings.TrimSpace(d)); n < 1 || n > 256 {
			return fmt.Errorf("%w: deliverables[%d] 须 1–256 字", ErrInvalidSpec, i)
		}
	}
	if len(c.Skills) > 16 {
		return fmt.Errorf("%w: skills ≤16", ErrInvalidSpec)
	}
	for i, sk := range c.Skills {
		if !skillRefPattern.MatchString(sk.Ref) {
			return fmt.Errorf("%w: skills[%d].ref 非法（%s）", ErrInvalidSpec, i, sk.Ref)
		}
		if sk.Version < 1 {
			return fmt.Errorf("%w: skills[%d].version ≥1", ErrInvalidSpec, i)
		}
	}
	if err := validateCaps("tools", c.Tools); err != nil {
		return err
	}
	if err := validateCaps("context", c.Context); err != nil {
		return err
	}
	if c.Delegation != nil {
		for _, a := range c.Delegation.Allowed {
			if a != "existing_agent" && a != "ephemeral_worker" {
				return fmt.Errorf("%w: delegation.allowed 非法枚举（%s）", ErrInvalidSpec, a)
			}
		}
		if c.Delegation.MaxDepth < 0 || c.Delegation.MaxDepth > 8 {
			return fmt.Errorf("%w: delegation.max_depth 须 0–8", ErrInvalidSpec)
		}
	}
	if c.Runtime != nil {
		if err := validateNames("runtime.required", c.Runtime.Required, 16); err != nil {
			return err
		}
		switch c.Runtime.WorkspaceAccess {
		case "none", "read", "read_write":
		default:
			// 空串同拒（审校 P2#3）：契约枚举三值封闭，runtime 段一旦出现，
			// workspace_access 必须显式取值——缺失经零值到达此处，不得落库
			return fmt.Errorf("%w: runtime.workspace_access 须 none|read|read_write（%q）", ErrInvalidSpec, c.Runtime.WorkspaceAccess)
		}
	}
	if c.Handoff != nil {
		if err := validateNames("handoff.required", c.Handoff.Required, 16); err != nil {
			return err
		}
	}
	return nil
}

func validateCaps(field string, cl *CapabilityList) error {
	if cl == nil {
		return nil
	}
	if err := validateNames(field+".required", cl.Required, 32); err != nil {
		return err
	}
	if err := validateNames(field+".optional", cl.Optional, 32); err != nil {
		return err
	}
	set := map[string]bool{}
	for _, v := range cl.Required {
		set[v] = true
	}
	for _, v := range cl.Optional {
		if set[v] {
			return fmt.Errorf("%w: %s required 与 optional 相交（%s）", ErrInvalidSpec, field, v)
		}
	}
	return nil
}

func validateNames(field string, names []string, max int) error {
	if len(names) > max {
		return fmt.Errorf("%w: %s ≤%d 项", ErrInvalidSpec, field, max)
	}
	for i, n := range names {
		if !capabilityPattern.MatchString(n) {
			return fmt.Errorf("%w: %s[%d] 非法能力名（%s）", ErrInvalidSpec, field, i, n)
		}
	}
	return nil
}

// normalize 入库前规范化（trim 人类输入字段；已出现的可选段内空清单落 []
// 而非 null——读面形状稳定）。
func normalize(c SpecContent) SpecContent {
	c.Name = strings.TrimSpace(c.Name)
	c.Responsibility = strings.TrimSpace(c.Responsibility)
	for i, d := range c.Deliverables {
		c.Deliverables[i] = strings.TrimSpace(d)
	}
	c.Tools = fillCaps(c.Tools)
	c.Context = fillCaps(c.Context)
	if c.Delegation != nil {
		c.Delegation.Allowed = fillNames(c.Delegation.Allowed)
	}
	if c.Runtime != nil {
		c.Runtime.Required = fillNames(c.Runtime.Required)
	}
	if c.Handoff != nil {
		c.Handoff.Required = fillNames(c.Handoff.Required)
	}
	return c
}

func fillCaps(cl *CapabilityList) *CapabilityList {
	if cl == nil {
		return nil
	}
	cl.Required = fillNames(cl.Required)
	cl.Optional = fillNames(cl.Optional)
	return cl
}

func fillNames(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- SlotInstance 职责实例（RFC-0018 §3.1/§5.2 / ADR-0020，0.5.3）----

// ErrInvalidInstance 实例参数不合法（标题校验）。
var ErrInvalidInstance = errors.New("slot: 实例参数不合法")

// SlotInstance 职责实例行：某作用域内实际承担的一份具体职责。
// 引用模板谱系不固定版本（Binding 才固定）；实例跨任职保留（§3.1）。
type SlotInstance struct {
	SlotInstanceID     string `json:"slot_instance_id"`
	SlotSpecID         string `json:"slot_spec_id"`
	Scope              string `json:"scope"` // workspace（goal/mission 随 0.6）
	Title              string `json:"title"` // 缺省 = 实例化时模板最新名存档
	KnowledgeNamespace string `json:"knowledge_namespace"`
	CreatedFromVersion int    `json:"created_from_version"` // 审计事实（非钉住）
	CreatedAt          string `json:"created_at"`
}

// InstanceSpecNow 模板现势连接（现算——随模板演进；实例自身字段存档不受影响）。
type InstanceSpecNow struct {
	Name          string `json:"name"`
	LatestVersion int    `json:"latest_version"`
}

// SlotInstanceView 实例视图（schema: SlotInstanceView）。
type SlotInstanceView struct {
	SlotInstance
	Spec InstanceSpecNow `json:"spec"`
}

// newInstanceID 缺省实例 ID：si_ + 时间有序毫秒（36 进制）+ 随机后缀（ag_ 同族）。
func newInstanceID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return "si_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// newNamespace 缺省知识命名空间：slotns_ + 时间有序 + 随机——全局唯一、
// 终身稳定、永不重派（生成器碰撞由库级 UNIQUE 约束兜底）。
func newNamespace() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return "slotns_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// CreateInstance 实例化职责：谱系存在性校验 → 生成实例 ID 与知识命名空间；
// 标题缺省取模板最新版名称存档（模板后续演进不回写）。
func (s *Service) CreateInstance(ctx context.Context, slotSpecID, title string) (SlotInstanceView, error) {
	// 显式提供但空白/超长 → 无效（"" = 未提供，走缺省——两态可区分）
	if title != "" {
		title = strings.TrimSpace(title)
		if title == "" || utf8.RuneCountInString(title) > 64 {
			return SlotInstanceView{}, fmt.Errorf("%w: title 须 1–64 字", ErrInvalidInstance)
		}
	}
	latest, ok, err := s.store.Latest(ctx, slotSpecID)
	if err != nil {
		return SlotInstanceView{}, err
	}
	if !ok {
		return SlotInstanceView{}, ErrNotFound
	}
	if title == "" {
		title = latest.Name // 缺省 = 实例化时模板最新名存档
	}
	inst := SlotInstance{
		SlotInstanceID:     s.newInstanceID(),
		SlotSpecID:         slotSpecID,
		Scope:              "workspace",
		Title:              title,
		KnowledgeNamespace: s.newNamespace(),
		CreatedFromVersion: latest.Version,
		CreatedAt:          s.now(),
	}
	if err := s.store.CreateInstance(ctx, inst); err != nil {
		return SlotInstanceView{}, fmt.Errorf("slot: 建实例: %w", err)
	}
	return SlotInstanceView{SlotInstance: inst, Spec: InstanceSpecNow{Name: latest.Name, LatestVersion: latest.Version}}, nil
}

// ListInstances 实例列表（创建序），视图携带模板现势连接。
// 读取顺序（审校 P2#1）：先实例后模板——模板版本只追加，故后读的现势对
// 先读的每个实例恒满足 latest ≥ created_from_version（先读模板的反序会在
// 两读之间被并发创建/追加打出"空名/v0/自 v2 却最新 v1"的矛盾快照）。
func (s *Service) ListInstances(ctx context.Context) ([]SlotInstanceView, error) {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	specs, err := s.store.LatestByLineage(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SlotInstanceView, 0, len(instances))
	for _, inst := range instances {
		now := specs[inst.SlotSpecID]
		// 防御钳制：病态场景（谱系行缺失等不可路径）不外显矛盾版本
		if now.LatestVersion < inst.CreatedFromVersion {
			now.LatestVersion = inst.CreatedFromVersion
		}
		out = append(out, SlotInstanceView{SlotInstance: inst, Spec: now})
	}
	return out, nil
}

// GetInstance 单查实例视图；未知 ok=false。
func (s *Service) GetInstance(ctx context.Context, slotInstanceID string) (SlotInstanceView, bool, error) {
	inst, ok, err := s.store.InstanceByID(ctx, slotInstanceID)
	if err != nil || !ok {
		return SlotInstanceView{}, false, err
	}
	var now InstanceSpecNow
	latest, specOK, err := s.store.Latest(ctx, inst.SlotSpecID)
	if err != nil {
		return SlotInstanceView{}, false, fmt.Errorf("slot: 实例现势读取: %w", err) // 审校 P2#2：读取失败传播，不伪装成功详情
	}
	if specOK { // 谱系缺失（病态，无删除路径）留零值——与列表同口径
		now = InstanceSpecNow{Name: latest.Name, LatestVersion: latest.Version}
		if now.LatestVersion < inst.CreatedFromVersion {
			now.LatestVersion = inst.CreatedFromVersion
		}
	}
	return SlotInstanceView{SlotInstance: inst, Spec: now}, true, nil
}
