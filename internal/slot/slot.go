// Package slot 职责域（RFC-0018 §5 / ADR-0019，0.5.2）。
// SlotSpec 是"承担什么职责、需要什么能力、如何交付"的版本化模板：不可变
// 版本行（(slot_spec_id, version) 主键），修订即追加，历史永不清除。模板只
// 描述能力要求（required/optional 分层），实际连接由 0.5.4 Binding 解析；
// 本版不含 SlotInstance / Binding / 准入行为。
package slot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 领域错误。
var (
	// ErrInvalidSpec 内容或谱系 ID 不合法（校验单源见 validate）。
	ErrInvalidSpec = errors.New("slot: 模板内容不合法")
	// ErrSpecExists 谱系已存在（建谱系唯一冲突；演进走 AppendVersion）。
	ErrSpecExists = errors.New("slot: 谱系已存在")
	// ErrNotFound 谱系不存在。
	ErrNotFound = errors.New("slot: 谱系不存在")
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
}

// Service 职责模板服务：校验单源 + 版本化存取。
type Service struct {
	store Store
	now   func() string
}

// Option 装配选项（测试注入确定性时钟）。
type Option func(*Service)

// WithNow 注入时钟（缺省 RFC3339Nano UTC）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// New 构造服务。
func New(store Store, opts ...Option) *Service {
	s := &Service{store: store, now: func() string { return nowUTC() }}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

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
