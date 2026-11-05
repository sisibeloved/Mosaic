// Package memory 分域记忆基石（RFC-0018 §7.1/§7.2 / ADR-0037 / 0.7.0）：
// MemoryItem 归属恰一命名空间（agent/room/workspace/mission/slot）并锚定
// 归属对象；类型（semantic/episodic/procedural）与权限域正交。本件为显式
// 写入面——策展状态机（0.7.1）、授权过滤与 FTS 召回（0.7.2）、迁移包
// （0.7.5+）在表结构与端口上递进。房间收束胶囊（RFC-0007）保持房间事件
// 流权威，不经本域复制；room 命名空间条目是显式写入的房间派生记忆。
package memory

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Namespace 记忆命名空间（RFC §7.1 数据分层）。
type Namespace string

const (
	NSAgent     Namespace = "agent"
	NSRoom      Namespace = "room"
	NSWorkspace Namespace = "workspace"
	NSMission   Namespace = "mission"
	NSSlot      Namespace = "slot"
)

// WorkspaceScope 个人版单工作区事实锚（ADR-0037 §1）：workspace 命名空间
// 的 scope_id 恒为 default；多设备（0.10）与 Bundle（0.7.5+）不受影响。
const WorkspaceScope = "default"

// Kind 记忆类型（RFC §7.1：semantic/episodic/procedural——知识/经历/方法，
// 与权限域正交）。
type Kind string

const (
	KindSemantic   Kind = "semantic"
	KindEpisodic   Kind = "episodic"
	KindProcedural Kind = "procedural"
)

// Sensitivity 敏感度（0.7.5 Bundle 脱敏消费；0.7.0 仅记录与透出）。
type Sensitivity string

const (
	SensNormal    Sensitivity = "normal"
	SensSensitive Sensitivity = "sensitive"
)

// Status 策展状态（RFC §7.2：proposed→accepted 状态机随 0.7.1；0.7.0 显式
// 写入恒 active）。
type Status string

const (
	StatusActive     Status = "active"
	StatusProposed   Status = "proposed"
	StatusSuperseded Status = "superseded"
)

// 错误族（httpapi 映射：ErrInvalid→400 invalid_request、ErrScopeNotFound→
// 409 memory_scope_not_found、ErrNotFound→404 memory_not_found）。
var (
	ErrInvalid       = errors.New("memory: invalid item")
	ErrScopeNotFound = errors.New("memory: scope anchor not found")
	ErrNotFound      = errors.New("memory: item not found")
	// 0.7.1 / ADR-0038：策展错误族（httpapi 映射 409）。
	ErrVersionConflict = errors.New("memory: version conflict")
	ErrTerminal        = errors.New("memory: item terminal")
)

// Item 分域记忆项（§7.2 拟定字段的 0.7.0 形态：表结构一次带全，策展/召回
// 字段由域固定默认——写入面只开放稳定子集）。
type Item struct {
	MemoryID    string
	Version     int
	Namespace   Namespace
	ScopeID     string
	Kind        Kind
	Title       string
	Content     string
	SourceRefs  []string
	ObservedAt  string
	ValidFrom   string // 空 = 不限
	ValidTo     string // 空 = 不限
	Confidence  float64
	Status      Status
	Visibility  string // 0.7.0 恒 "namespace"（授权细化随 0.7.2）
	Sensitivity Sensitivity
	Supersedes  []string // 替代链（0.7.1 supersede 动作写入）
	EditHistory []EditRecord
	CreatedAt   string
	UpdatedAt   string
}

// EditRecord 版本留痕（0.7.1 / ADR-0038 §2——append-only，恰一条/次；
// cause 区分 revise/reject/withdraw/supersede/accept）。
type EditRecord struct {
	Version    int    `json:"version"`
	Cause      string `json:"cause"`
	Note       string `json:"note,omitempty"`
	EditedBy   string `json:"edited_by"`
	OccurredAt string `json:"occurred_at"`
}

// CreateCmd 显式写入命令（0.7.0 稳定子集——人工记忆操作或系统事实记录）。
type CreateCmd struct {
	Namespace   Namespace
	ScopeID     string
	Kind        Kind
	Title       string
	Content     string
	SourceRefs  []string
	ObservedAt  string
	ValidFrom   string
	ValidTo     string
	Confidence  *float64 // nil = 1.0（人工显式写入满置信）
	Sensitivity Sensitivity
	// 0.7.1：默认 active（高可信明确指令）；不确定候选留 proposed 待策展。
	Status Status
}

// Anchors 归属锚存在性闭包（app 装配组合各域 store；memory 不 import 各域
// ——依赖方向同 dispatchSlotLookups 模式）。nil 闭包 = 该命名空间锚校验
// 跳过（测试装配）；生产装配四者恒置。
type Anchors struct {
	AgentExists        func(ctx context.Context, id string) (bool, error)
	RoomExists         func(ctx context.Context, id string) (bool, error)
	MissionExists      func(ctx context.Context, id string) (bool, error)
	SlotInstanceExists func(ctx context.Context, id string) (bool, error)
}

// ListFilter 列表过滤（namespace 必填——跨域列表不存在，越域内容与存在性
// 不泄漏；Store 应用全部过滤/排序（创建序降序）/游标/limit）。
type ListFilter struct {
	Namespace Namespace
	ScopeID   string
	Kind      Kind
	Status    Status
	Limit     int
	Before    string // 上一页末条 memory_id；空 = 首页
	// 0.7.1 时效口径（ADR-0038 §3）：Status=active 且 IncludeExpired=false
	// 时排除 valid_to < Now（RFC3339 时刻比较）；Now 由 Service 时钟单源
	// 传入。proposed/superseded 显式口径不过滤（策展审视看全量）。
	Now            string
	IncludeExpired bool
}

// Store 存储端口（MemoryBackend 的 0.7.0 子集——Propose/Accept/Revise/
// Search/Export/Import/Forget 分别归 0.7.1/0.7.2/0.7.5–0.7.8，不预建空方法）。
type Store interface {
	CreateItem(ctx context.Context, it Item) (Item, error)
	ItemByID(ctx context.Context, memoryID string) (Item, bool, error)
	ListItems(ctx context.Context, f ListFilter) ([]Item, error)
	// UpdateItem CAS 原位替换（version 核对失败 ok=false；edit_history 由
	// 域构造传入——存储无策展语义）。
	UpdateItem(ctx context.Context, memoryID string, expectedVersion int, next Item) (Item, bool, error)
	// SupersedeItem 单事务双动作（ADR-0038 §2：旧终结 + 新建 supersedes 链；
	// CAS 失败 ok=false 整体不落）。
	SupersedeItem(ctx context.Context, oldID string, expectedVersion int, oldNext, replacement Item) (Item, Item, bool, error)
}

// Service 分域记忆服务（域校验单源 + 锚门 + 默认填充）。
type Service struct {
	store   Store
	anchors Anchors
	now     func() string
}

// NewService now 为时钟单源（RFC3339 字符串）。
func NewService(store Store, anchors Anchors, now func() string) *Service {
	if now == nil {
		now = func() string { return time.Now().UTC().Format(time.RFC3339) }
	}
	return &Service{store: store, anchors: anchors, now: now}
}

const (
	maxScopeIDRunes  = 128
	maxTitleRunes    = 200
	maxContentRunes  = 20000
	maxSourceRefs    = 16
	maxSourceRefRune = 500
	// DefaultListLimit / MaxListLimit 列表分页上限。
	DefaultListLimit = 50
	MaxListLimit     = 100
)

// invalidErr 域校验错误（包装 ErrInvalid——调用方按错误族映射 400）。
func invalidErr(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

// validate 域校验单源（字段格式/枚举/长度/时间序）。详情错误经 fmt 包装
// ErrInvalid——调用方（httpapi）按错误族映射 400。
func (c CreateCmd) validate() error {
	switch c.Namespace {
	case NSAgent, NSRoom, NSWorkspace, NSMission, NSSlot:
	default:
		return invalidErr("namespace 未知：" + string(c.Namespace))
	}
	switch c.Kind {
	case KindSemantic, KindEpisodic, KindProcedural:
	default:
		return invalidErr("kind 未知：" + string(c.Kind))
	}
	if c.Sensitivity == "" {
		c.Sensitivity = SensNormal
	}
	switch c.Sensitivity {
	case SensNormal, SensSensitive:
	default:
		return invalidErr("sensitivity 未知：" + string(c.Sensitivity))
	}
	scope := strings.TrimSpace(c.ScopeID)
	if scope == "" || utf8.RuneCountInString(scope) > maxScopeIDRunes {
		return invalidErr("scope_id 须为 1–128 字符")
	}
	if c.Namespace == NSWorkspace && scope != WorkspaceScope {
		return invalidErr("workspace 命名空间 scope_id 恒为 " + WorkspaceScope + "（单工作区事实）")
	}
	// 0.7.1：status 可选 active|proposed（默认 active；superseded 只能经
	// 策展动作到达——写入面不得直接造终结态）。
	switch c.Status {
	case "", StatusActive, StatusProposed:
	default:
		return invalidErr("status 写入面只接受 active|proposed")
	}
	return validateFields(c.Title, c.Content, c.SourceRefs, c.ObservedAt, c.ValidFrom, c.ValidTo, c.Confidence)
}

// validateFields 字段级校验（Create 与 Revise 共用单源）。
func validateFields(title, content string, sourceRefs []string, observedAt, validFrom, validTo string, confidence *float64) error {
	if t := strings.TrimSpace(title); t == "" || utf8.RuneCountInString(t) > maxTitleRunes {
		return invalidErr("title 须为 1–200 字符")
	}
	if strings.TrimSpace(content) == "" || utf8.RuneCountInString(content) > maxContentRunes {
		return invalidErr("content 须为 1–20000 字符")
	}
	if len(sourceRefs) > maxSourceRefs {
		return invalidErr("source_refs 至多 16 条")
	}
	for _, ref := range sourceRefs {
		r := strings.TrimSpace(ref)
		if r == "" || utf8.RuneCountInString(r) > maxSourceRefRune {
			return invalidErr("source_ref 须为 1–500 字符")
		}
	}
	if _, err := time.Parse(time.RFC3339, observedAt); err != nil {
		return invalidErr("observed_at 须为 RFC3339")
	}
	var from, to time.Time
	if validFrom != "" {
		var err error
		if from, err = time.Parse(time.RFC3339, validFrom); err != nil {
			return invalidErr("valid_from 须为 RFC3339")
		}
	}
	if validTo != "" {
		var err error
		if to, err = time.Parse(time.RFC3339, validTo); err != nil {
			return invalidErr("valid_to 须为 RFC3339")
		}
	}
	if !from.IsZero() && !to.IsZero() && !to.After(from) {
		return invalidErr("valid_to 须晚于 valid_from")
	}
	if confidence != nil && (*confidence < 0 || *confidence > 1) {
		return invalidErr("confidence 须在 [0,1]")
	}
	return nil
}

// Create 显式写入（校验→锚门→默认填充→落库）。锚对象不存在 ErrScopeNotFound。
func (s *Service) Create(ctx context.Context, cmd CreateCmd) (Item, error) {
	if err := cmd.validate(); err != nil {
		return Item{}, err
	}
	if err := s.checkAnchor(ctx, cmd.Namespace, strings.TrimSpace(cmd.ScopeID)); err != nil {
		return Item{}, err
	}
	confidence := 1.0
	if cmd.Confidence != nil {
		confidence = *cmd.Confidence
	}
	sensitivity := cmd.Sensitivity
	if sensitivity == "" {
		sensitivity = SensNormal
	}
	// 0.7.1：status 默认 active；proposed 为策展候选（写入面不得直接造
	// 终结态——superseded 只经策展动作到达）。
	status := cmd.Status
	if status == "" {
		status = StatusActive
	}
	now := s.now()
	it := Item{
		MemoryID:  "mem_" + newToken(),
		Version:   1,
		Namespace: cmd.Namespace,
		ScopeID:   strings.TrimSpace(cmd.ScopeID),
		Kind:      cmd.Kind,
		Title:     strings.TrimSpace(cmd.Title),
		Content:   cmd.Content,
		// 一审 #1：规范化非 nil——省略/空数组的 source_refs 落库与读回
		// 均为空数组（JSON []），不产生 null（页面 .length 崩溃面）。
		SourceRefs:  append([]string{}, cmd.SourceRefs...),
		ObservedAt:  cmd.ObservedAt,
		ValidFrom:   cmd.ValidFrom,
		ValidTo:     cmd.ValidTo,
		Confidence:  confidence,
		Status:      status,
		Visibility:  "namespace",
		Sensitivity: sensitivity,
		Supersedes:  []string{},
		EditHistory: []EditRecord{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return s.store.CreateItem(ctx, it)
}

// Get 单查；未知 ErrNotFound。
func (s *Service) Get(ctx context.Context, memoryID string) (Item, error) {
	it, ok, err := s.store.ItemByID(ctx, memoryID)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		return Item{}, ErrNotFound
	}
	return it, nil
}

// List 按命名空间隔离列表（缺 namespace / limit 越界 ErrInvalid；status 缺省
// active——与 0.7.1 召回口径衔接）。Store 返回至多 limit+1 条，本方法截取
// limit 条并以第 limit+1 条算 next_cursor（无更多为空）。
func (s *Service) List(ctx context.Context, f ListFilter) ([]Item, string, error) {
	switch f.Namespace {
	case NSAgent, NSRoom, NSWorkspace, NSMission, NSSlot:
	default:
		return nil, "", invalidErr("namespace 必填且须为五命名空间之一")
	}
	if f.Kind != "" {
		switch f.Kind {
		case KindSemantic, KindEpisodic, KindProcedural:
		default:
			return nil, "", invalidErr("kind 未知：" + string(f.Kind))
		}
	}
	if f.Status == "" {
		f.Status = StatusActive
	}
	switch f.Status {
	case StatusActive, StatusProposed, StatusSuperseded:
	default:
		return nil, "", invalidErr("status 未知：" + string(f.Status))
	}
	if f.Limit <= 0 {
		f.Limit = DefaultListLimit
	}
	if f.Limit > MaxListLimit {
		return nil, "", invalidErr("limit 至多 100")
	}
	// 0.7.1 时效口径（ADR-0038 §3）：now 由时钟单源填充——active 默认口径
	// 排除 valid_to < now（Store 下推）；proposed/superseded 显式口径与
	// IncludeExpired 不过滤（策展审视/历史解释看全量）。
	if f.Now == "" {
		f.Now = s.now()
	}
	items, err := s.store.ListItems(ctx, f)
	if err != nil {
		return nil, "", err
	}
	if len(items) > f.Limit {
		// next_cursor = 本页末条——客户端直传作下一页 before。
		return items[:f.Limit], items[f.Limit-1].MemoryID, nil
	}
	return items, "", nil
}

// checkAnchor 锚存在性门（workspace 恒过——单工作区事实；nil 闭包跳过——
// 测试装配）。锚查询错误透传（不吞并——同域事实性校验纪律）。
func (s *Service) checkAnchor(ctx context.Context, ns Namespace, scopeID string) error {
	var exists func(ctx context.Context, id string) (bool, error)
	switch ns {
	case NSAgent:
		exists = s.anchors.AgentExists
	case NSRoom:
		exists = s.anchors.RoomExists
	case NSMission:
		exists = s.anchors.MissionExists
	case NSSlot:
		exists = s.anchors.SlotInstanceExists
	case NSWorkspace:
		return nil
	}
	if exists == nil {
		return nil
	}
	ok, err := exists(ctx, scopeID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrScopeNotFound
	}
	return nil
}

// newToken 生成式后缀（时间毫秒 36 进制 + 8 字节随机 hex——与 dispatch
// 域生成式同族）。
func newToken() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}
