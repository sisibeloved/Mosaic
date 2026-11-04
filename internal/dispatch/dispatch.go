// Package dispatch 轻量级智能体网关首切片（RFC-0018 §4.4–4.8/§11.2/§11.2.1
// / ADR-0024，0.6.1）：确定性解析 → 复核投递 → 回执。只做 create_mission
// 房间交办路径（无语义选择/续接/IM 入口）；caller_ref/scope 服务端推导；
// 幂等以 (caller_ref, scope, idempotency_key) 唯一约束兜底——同键重放原
// 回执、同键异载荷拒绝，重试不得变成新派单。
package dispatch

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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// 调用方与作用域（0.6.1 固定：本地 owner 单用户——请求体不得自称）。
const (
	CallerOwner       = "owner"
	ScopeWorkspace    = "workspace"
	KindCreateMission = "create_mission"
	// DecisionTTL 决策有效期（过期投递 409 route_stale 重新解析）。
	DecisionTTL = 10 * time.Minute
)

// 领域错误。
var (
	// ErrInvalidRequest 请求不合法（kind 枚举/幂等键模式/结构缺失）。
	ErrInvalidRequest = errors.New("dispatch: 请求不合法")
	// ErrRouteNotFound 决策不存在（未知 route_id）。
	ErrRouteNotFound = errors.New("dispatch: 路由决策不存在")
	// ErrRouteStale 决策过期或目标变化（重新解析）。
	ErrRouteStale = errors.New("dispatch: 决策过期或目标已变化")
	// ErrIdempotencyConflict 同键异载荷（幂等键冲突——重试不得变新派单）。
	ErrIdempotencyConflict = errors.New("dispatch: 幂等键冲突（同键异载荷）")
	// ErrReceiptNotFound 回执不存在（不等于领域动作一定未发生）。
	ErrReceiptNotFound = errors.New("dispatch: 回执不存在")
	// ErrTargetNotActive 续接目标身份非 active（0.6.13 / ADR-0036 §1：续接指令
	// 不发往已停身份——suspended/retiring/retired 须先恢复或换人）。
	ErrTargetNotActive = errors.New("dispatch: 目标负责人非 active")
)

// ResolveRequest 解析请求（RouteRequest 的 0.6.1 确定性子集）：房间来源 +
// 契约内容 + 可选 owner。决策存档本请求全文，投递以此为准。
type ResolveRequest struct {
	Kind         string             `json:"kind"`
	RoomSource   mission.RoomSource `json:"room_source"`
	OwnerAgentID string             `json:"owner_agent_id,omitempty"`
	// 职责引用（0.6.13 / ADR-0036 §2；审校 #10 修订）：二选一，与 OwnerAgentID
	// 互斥（并存结构拒绝——显式 Agent ≠ 责任已移交，语义须先明确）。Request
	// 存**原始请求**（slot 引用原样、不写解析 owner——摘要对同一请求稳定）；
	// 解析所得实例/Agent 作为决策事实另存 Decision.Resolved*（投递复核据此重查）。
	SlotInstanceID string          `json:"slot_instance_id,omitempty"`
	SlotSpecID     string          `json:"slot_spec_id,omitempty"`
	Contract       mission.Content `json:"contract"`
}

// Decision 路由决策（RouteDecision 的 0.6.1 子集 + 请求存档）。
type Decision struct {
	RouteID         string         `json:"route_id"`
	DecisionVersion int            `json:"decision_version"`
	RequestDigest   string         `json:"request_digest"`
	Status          string         `json:"status"` // resolved | blocked
	SelectionSource string         `json:"selection_source"`
	ReasonCode      string         `json:"reason_code"`
	ReasonSummary   string         `json:"reason_summary"`
	TargetRefs      []string       `json:"target_refs"`
	Request         ResolveRequest `json:"request"` // 投递输入（原始请求存档——审校 #10）
	// ResolvedAgentID / ResolvedInstanceID 解析事实（0.6.13 审校 #10：slot 寻址
	// 所得另存——模板引用解析出的实例也记 ResolvedInstanceID，投递复核按
	// 实例+任职双核对；非 slot 寻址为空串）。
	ResolvedAgentID    string `json:"resolved_agent_id,omitempty"`
	ResolvedInstanceID string `json:"resolved_instance_id,omitempty"`
	ExpiresAt          string `json:"expires_at"`
	CreatedAt          string `json:"created_at"`
}

// DispatchCmd 投递命令（执行决策存档请求；幂等键全局唯一）。
type DispatchCmd struct {
	RouteID        string `json:"route_id"`
	IdempotencyKey string `json:"idempotency_key"`
	RequestID      string `json:"request_id,omitempty"`
}

// Receipt 投递回执（DispatchReceipt 的 0.6.1 子集）。
type Receipt struct {
	ReceiptID       string   `json:"receipt_id"`
	CallerRef       string   `json:"caller_ref"`
	Scope           string   `json:"scope"`
	IdempotencyKey  string   `json:"idempotency_key"`
	RequestID       string   `json:"request_id"`
	RequestDigest   string   `json:"request_digest"`
	RouteID         string   `json:"route_id"`
	DecisionVersion int      `json:"decision_version"`
	DomainCommandID string   `json:"domain_command_id"` // = mission_id
	TargetRefs      []string `json:"target_refs"`
	ReturnRefs      []string `json:"return_refs"`
	// Reason 续接说明（0.6.13 审校 #16：mission_update 的 reason 随回执持久化
	// 并读回；create 路径为空串）。
	Reason      string `json:"reason,omitempty"`
	CommittedAt string `json:"committed_at"`
}

// Missions 领域服务端口（*mission.Service 实现；dispatch 编排不复制域逻辑）。
type Missions interface {
	Create(ctx context.Context, ownerAgentID string, c mission.Content, src *mission.RoomSource) (mission.Mission, error)
	// CreateGated 带事务门建单（0.6.13 审校 #1/#2 + 复审 #3/#7）：职责意图五核
	// 与模板唯一性锚、RequireOwnerActive 身份门随建单进同一写事务核对（漂移
	// → mission.ErrSlotIntentViolated / ErrOwnerGateViolated → route_stale）。
	CreateGated(ctx context.Context, ownerAgentID string, c mission.Content, src *mission.RoomSource, gate *mission.CreationGate) (mission.Mission, error)
	// Get / Revise 续接（0.6.13 / ADR-0036 §1 mission_update）：复核读现值 +
	// CAS 修订（归属锚由网关以服务端读值填充——owner 不变）。
	Get(ctx context.Context, missionID string) (mission.Mission, bool, error)
	Revise(ctx context.Context, missionID string, expectedVersion int, expectedOwner, ownerAgentID string, c mission.Content) (mission.Mission, error)
}

// AtomicUpdater Missions 端口的可选原子扩展（0.6.13 审校 #3/#11/#12/#13：
// 续接的认领 + 复核 + 修订 + 回执**同一写事务**——回执失败即整体回滚、领域
// 拒绝不占键、恢复重驱动沿用原回执行）。app 装配以 SQLite 实装注入；无此
// 面时（内存件）退化为服务层分步 + 拒绝回滚补偿。
type AtomicUpdater interface {
	UpdateMissionAtomic(ctx context.Context, cmd UpdateCmd, dg, now, newReceiptID string) (Receipt, bool, error)
}

// Lookups 复核端口（预检与复核同源）：owner 存在性 / 房间任务卡存在性 / 映射查询 /
// 职责寻址（0.6.13）。Slots nil = slot 引用不可用（带 slot 的请求结构拒绝——宁拒）。
type Lookups struct {
	Agents  mission.AgentLookup
	Sources mission.SourceLookup
	Mapped  func(ctx context.Context, roomID, taskID string) (string, bool, error)
	Slots   SlotLookups
}

// SlotLookups 职责寻址端口（0.6.13 / ADR-0036 §2；RFC-0018 §4.5：
// scope → SlotInstance → active Binding → AgentID）。app 装配适配 slot 域
// （依赖方向同 Agents/Sources——dispatch 不 import slot）。
type SlotLookups interface {
	// SlotInstanceExists 实例存在性。
	SlotInstanceExists(ctx context.Context, instanceID string) (bool, error)
	// SlotInstancesOfSpec scope 内该模板的实例集（空 = 无实例；>1 = 歧义）。
	SlotInstancesOfSpec(ctx context.Context, specID string) ([]string, error)
	// ActiveBindingAgent 实例的 active 任职 Agent（active=false = 无 active
	// 任职——不自动激活 ready Binding，§4.5 明文）。
	ActiveBindingAgent(ctx context.Context, instanceID string) (agentID string, active bool, err error)
	// GrantAdmission 授权准入（0.6.13 审校 #7，RFC §4.6：active 任职钉版职责
	// 要求 vs 授权档案——复用 slot 准入门单源；ok=false 携缺失清单）。查询
	// 失败向上传播（预检 blocked、投递 route_stale，不吞）。
	GrantAdmission(ctx context.Context, instanceID, agentID string) (ok bool, missing []string, err error)
}

// 寻址来源与 blocked 原因码（0.6.13）。
const (
	SelectionSlotBinding   = "slot_binding"
	ReasonSlotNoInstance   = "slot_no_instance"
	ReasonSlotAmbiguous    = "slot_ambiguous"
	ReasonSlotUnassigned   = "slot_unassigned"
	ReasonSlotInstanceGone = "slot_instance_not_found"
	// ReasonSlotGrantMissing 授权准入不满足（0.6.13 审校 #7：必需工具/技能/上下文
	// 被撤销——旧决策不得派发，§4.6 授权撤销后拒绝旧决策）。
	ReasonSlotGrantMissing = "slot_grant_missing"
	// ReasonAgentNotActive 负责人身份非 active（0.6.13 审校 #7：suspended/retiring/
	// retired 不接新交办——预检 blocked，投递复核 route_stale）。
	ReasonAgentNotActive = "agent_not_active"
)

// Store 持久化端口（SQLite：migration v14；测试件：memstore）。
type Store interface {
	// SaveDecision 决策落库（rd_）。
	SaveDecision(ctx context.Context, d Decision) error
	// DecisionByID 单查决策；未知 ok=false。
	DecisionByID(ctx context.Context, routeID string) (Decision, bool, error)
	// ClaimKey 原子认领幂等键并绑定摘要（审校 P1：在领域创建之前——
	// pending 回执行，domain_command_id 空）。撞键返回已存行：
	//   conflict=true   摘要不同（异载荷——写入前退出，零副作用）
	//   finalized=true  摘要相同且已完结（直接重放原回执——审校 P2#4，
	//                   同键同摘要即同一请求，路由不同也重放）
	//   双 false        摘要相同的 pending 认领（并发孪生/崩溃恢复重驱动）
	ClaimKey(ctx context.Context, r Receipt) (existing Receipt, conflict bool, finalized bool, err error)
	// FinalizeReceipt 填 domain_command_id/committed_at/target_refs（幂等
	// UPDATE——认领者创建成功后完结回执）。
	FinalizeReceipt(ctx context.Context, r Receipt) error
	// ReleaseKey 回滚未生效命令对键的认领（0.6.13 审校 #11：领域拒绝后释放
	// pending 行——确定失败的请求修正后可重试；只删同摘要 pending，不碰已
	// 完结回执）。
	ReleaseKey(ctx context.Context, callerRef, scope, key, digest string) error
	// ReceiptByKey 幂等键查**已提交**回执（0.6.13 审校 #11：pending 认领是
	// 事务内部事实，公开查询不可见——防"虚假已提交"）；未完结 ok=false。
	ReceiptByKey(ctx context.Context, callerRef, scope, key string) (Receipt, bool, error)
}

// Service 网关服务。
type Service struct {
	store        Store
	missions     Missions
	lookups      Lookups
	now          func() string
	newRouteID   func() string
	newReceiptID func() string
}

// Option 装配选项。
type Option func(*Service)

// WithNow 注入时钟。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewRouteID 注入决策 ID 生成器。
func WithNewRouteID(f func() string) Option { return func(s *Service) { s.newRouteID = f } }

// WithNewReceiptID 注入回执 ID 生成器。
func WithNewReceiptID(f func() string) Option { return func(s *Service) { s.newReceiptID = f } }

// New 构造服务。
func New(store Store, missions Missions, lookups Lookups, opts ...Option) *Service {
	s := &Service{
		store: store, missions: missions, lookups: lookups,
		now:          func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newRouteID:   func() string { return "rd_" + randomSuffix() },
		newReceiptID: func() string { return "dpr_" + randomSuffix() },
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

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// digest 请求规范化摘要（Go struct 序列化字段序确定）：解析、投递、回执
// 三处一致，防"决策后改请求"。
func digest(req ResolveRequest) string {
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Resolve 解析与预检（不创建任务）：任一前置不满足 → blocked 决策（恒有
// 决策可返回，blocked 不是错误）；决策落库，10 分钟内可投递。
func (s *Service) Resolve(ctx context.Context, req ResolveRequest) (Decision, error) {
	if req.Kind != KindCreateMission {
		return Decision{}, fmt.Errorf("%w: kind 当前仅 create_mission（%s）", ErrInvalidRequest, req.Kind)
	}
	// 摘要先行（0.6.13 审校 #10）：对**原始请求**计算——解析改写不进摘要，
	// 换绑后重解析同一请求摘要稳定（同键可重放已提交回执）。
	dg := digest(req)
	// 0.6.13 / ADR-0036 §2（审校 #10 修订）：职责寻址先行——解析结果作为
	// **决策事实**另存（Resolved*），Request 保持原始；寻址失败是 blocked 决策
	//（恒 200），端口缺失/互斥违反是结构错误（400）。
	selection := "room_default"
	var resolvedAgent, resolvedInstance string
	if req.SlotInstanceID != "" || req.SlotSpecID != "" {
		if req.OwnerAgentID != "" {
			return Decision{}, fmt.Errorf("%w: owner_agent_id 与 slot 引用互斥（显式 Agent ≠ 责任已移交——语义须先明确）", ErrInvalidRequest)
		}
		if req.SlotInstanceID != "" && req.SlotSpecID != "" {
			return Decision{}, fmt.Errorf("%w: slot_instance_id 与 slot_spec_id 二选一", ErrInvalidRequest)
		}
		if s.lookups.Slots == nil {
			return Decision{}, fmt.Errorf("%w: 职责寻址端口未装配（slot 引用不可用）", ErrInvalidRequest)
		}
		agent, instance, code, ok := s.resolveSlot(ctx, &req)
		if !ok {
			d := Decision{
				RouteID: s.newRouteID(), DecisionVersion: 1, RequestDigest: dg,
				Status: "blocked", SelectionSource: selection,
				ReasonCode: code, ReasonSummary: "职责寻址不满足（" + code + "）",
				TargetRefs: []string{}, Request: req,
				ExpiresAt: expiryAfter(s.now(), DecisionTTL), CreatedAt: s.now(),
			}
			if err := s.store.SaveDecision(ctx, d); err != nil {
				return Decision{}, err
			}
			return d, nil
		}
		resolvedAgent, resolvedInstance = agent, instance
		selection = SelectionSlotBinding
	} else if req.OwnerAgentID != "" {
		selection = "explicit_owner"
	}
	now := s.now()
	d := Decision{
		RouteID:         s.newRouteID(),
		DecisionVersion: 1,
		RequestDigest:   dg,
		Status:          "resolved",
		SelectionSource: selection,
		TargetRefs:      []string{"room:" + req.RoomSource.RoomID + ":" + req.RoomSource.TaskID},
		Request:         req,
		ResolvedAgentID: resolvedAgent, ResolvedInstanceID: resolvedInstance,
		ExpiresAt: expiryAfter(now, DecisionTTL),
		CreatedAt: now,
	}
	if code, summary, ok := s.precheck(ctx, req, resolvedAgent, resolvedInstance); !ok {
		d.Status = "blocked"
		d.ReasonCode = code
		d.ReasonSummary = summary
		d.TargetRefs = []string{}
	}
	if err := s.store.SaveDecision(ctx, d); err != nil {
		return Decision{}, err
	}
	return d, nil
}

// resolveSlot 职责寻址（§4.5；审校 #10 修订：不改写请求——解析所得作为返回
// 值交决策事实）：显式实例或模板→scope 内唯一实例 → active 任职 → Agent。
// 无实例 / 多实例歧义 / 无 active 任职（不自动激活 ready Binding）各 blocked。
// 模板引用解析出的实例一并返回（投递复核按实例核对——模板实例集漂移即 stale）。
func (s *Service) resolveSlot(ctx context.Context, req *ResolveRequest) (agent, instance, reasonCode string, ok bool) {
	instanceID := req.SlotInstanceID
	if instanceID == "" {
		instances, err := s.lookups.Slots.SlotInstancesOfSpec(ctx, req.SlotSpecID)
		if err != nil {
			return "", "", "slot_lookup_failed", false
		}
		switch len(instances) {
		case 0:
			return "", "", ReasonSlotNoInstance, false
		case 1:
			instanceID = instances[0]
		default:
			return "", "", ReasonSlotAmbiguous, false // 同名模板跨项目多实例——禁止猜选，须显式指实例
		}
	}
	exists, err := s.lookups.Slots.SlotInstanceExists(ctx, instanceID)
	if err != nil {
		return "", "", "slot_lookup_failed", false
	}
	if !exists {
		return "", "", ReasonSlotInstanceGone, false
	}
	a, active, err := s.lookups.Slots.ActiveBindingAgent(ctx, instanceID)
	if err != nil {
		return "", "", "slot_lookup_failed", false
	}
	if !active {
		return "", "", ReasonSlotUnassigned, false // 待任职——不能自动激活 ready Binding
	}
	return a, instanceID, "", true
}

// precheck 预检单源（Resolve 与 Dispatch 复核同源——目标变化即 stale）：
// 房间任务卡存在、映射未被占、负责人（显式或寻址所得）**在册且 active**
// （0.6.13 审校 #7：暂停身份不接新交办）、slot 寻址授权准入满足（§4.6 授权
// 撤销后拒绝旧决策）、契约内容合法。resolvedAgent/resolvedInstance 为 slot
// 寻址决策事实（Resolve 时为本次解析值；Dispatch 复核时为决策存档值）。
func (s *Service) precheck(ctx context.Context, req ResolveRequest, resolvedAgent, resolvedInstance string) (code, summary string, ok bool) {
	if s.lookups.Sources == nil {
		return "source_unavailable", "来源查询端口未装配", false
	}
	exists, err := s.lookups.Sources.RoomTaskExists(ctx, req.RoomSource.RoomID, req.RoomSource.TaskID)
	if err != nil {
		return "source_unavailable", "来源查询失败：" + err.Error(), false
	}
	if !exists {
		return "task_not_found",
			fmt.Sprintf("房间任务卡不存在（%s / %s）", req.RoomSource.RoomID, req.RoomSource.TaskID), false
	}
	if s.lookups.Mapped != nil {
		if existing, mapped, err := s.lookups.Mapped(ctx, req.RoomSource.RoomID, req.RoomSource.TaskID); err == nil && mapped {
			return "source_already_mapped",
				fmt.Sprintf("(%s, %s) 已交办为 %s——追加要求走 /v1/dispatch/update", req.RoomSource.RoomID, req.RoomSource.TaskID, existing), false
		}
	}
	// 负责人生命周期（审校 #7）：显式与寻址路径同规——存在且 active。查询
	// 失败如实 blocked（不吞——状态不可知不预检通过）。
	owner := req.OwnerAgentID
	if owner == "" {
		owner = resolvedAgent
	}
	if owner != "" {
		if s.lookups.Agents == nil {
			return "agent_unavailable", "身份查询端口未装配", false
		}
		exists, aerr := s.lookups.Agents.AgentExists(ctx, owner)
		if aerr != nil {
			return "agent_lookup_failed", "身份查询失败：" + aerr.Error(), false
		}
		if !exists {
			return "agent_not_found", "负责人 Agent 不存在（" + owner + "）", false
		}
		if st, ok2, serr := s.lookups.Agents.AgentStatus(ctx, owner); serr != nil {
			return "agent_lookup_failed", "身份状态查询失败：" + serr.Error(), false
		} else if ok2 && st != mission.AgentStatusActive {
			return ReasonAgentNotActive, "负责人身份非 active（" + owner + "：" + st + "）——恢复或换人后再交办", false
		}
	}
	// slot 授权准入（审校 #7）：寻址所得任职须满足钉版职责要求（必需工具/
	// 技能/上下文被撤销 → blocked，投递复核同源 route_stale）。
	if resolvedInstance != "" && s.lookups.Slots != nil {
		admitted, missing, gerr := s.lookups.Slots.GrantAdmission(ctx, resolvedInstance, resolvedAgent)
		if gerr != nil {
			return "slot_lookup_failed", "授权准入查询失败：" + gerr.Error(), false
		}
		if !admitted {
			return ReasonSlotGrantMissing,
				fmt.Sprintf("授权准入不满足（实例 %s / %s）：缺 %s——补授权后重新解析", resolvedInstance, resolvedAgent, strings.Join(missing, "、")), false
		}
	}
	if err := mission.ValidateContent(req.Contract); err != nil {
		return "invalid_mission", "契约不合法：" + err.Error(), false
	}
	return "", "", true
}

// Dispatch 复核投递（审校 P1/P2#4 版）：决策载入与时效检查 → **原子认领
// 幂等键并绑定摘要**（异摘要创建前退出零副作用；同摘要已完结重放原回执
// ——路由不同亦然，摘要即请求身份；pending 同摘要重驱动恢复）→ 复核预检
// （变化 route_stale）→ 执行 mission 创建（决策存档请求）→ 完结回执。
// 已接受的两事务恢复窗口保留：创建后完结前崩溃 → 重试命中 pending 认领
// 重驱动，映射撞键回显已建 mission_id 可见恢复，不产生重复派单。
func (s *Service) Dispatch(ctx context.Context, cmd DispatchCmd) (Receipt, bool, error) {
	if !idempotencyKeyPattern.MatchString(cmd.IdempotencyKey) {
		return Receipt{}, false, fmt.Errorf("%w: idempotency_key 须 [A-Za-z0-9._:-]{1,128}", ErrInvalidRequest)
	}
	d, found, err := s.store.DecisionByID(ctx, cmd.RouteID)
	if err != nil {
		return Receipt{}, false, err
	}

	// 已提交回执先于决策时效查验（§11.2.1 次序）：重试不得因决策此后
	// 过期/blocked 而失去重放（审校复核——投递成功改变预检状态是常态）
	prior, hasPrior, err := s.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, cmd.IdempotencyKey)
	if err != nil {
		return Receipt{}, false, err
	}
	if hasPrior {
		if !found {
			// 键已绑定而路由未知：不可能是合法重放（重放携带原路由）
			return Receipt{}, false, fmt.Errorf("%w: 键 %s 已绑定其他请求", ErrIdempotencyConflict, cmd.IdempotencyKey)
		}
		if prior.RequestDigest != d.RequestDigest {
			return Receipt{}, false, fmt.Errorf("%w: 键 %s 已绑定摘要 %s（异载荷）", ErrIdempotencyConflict, cmd.IdempotencyKey, prior.RequestDigest[:min(12, len(prior.RequestDigest))])
		}
		if prior.DomainCommandID != "" {
			return prior, true, nil // 同摘要已完结 → 重放原回执（P2#4：路由不同亦然）
		}
		// pending 同摘要 → 恢复重驱动（落到下方认领与创建）
	}
	if !found {
		return Receipt{}, false, ErrRouteNotFound
	}
	if expired(d.ExpiresAt, s.now()) {
		return Receipt{}, false, fmt.Errorf("%w: 决策已过期（%s），请重新解析", ErrRouteStale, d.ExpiresAt)
	}
	if d.Status != "resolved" {
		// blocked 决策不可投递（执行条件不足不启动——§11.2）；未认领键
		return Receipt{}, false, fmt.Errorf("%w: 决策为 blocked（%s：%s），不满足执行条件", ErrRouteStale, d.ReasonCode, d.ReasonSummary)
	}

	// 原子认领（先于一切领域副作用）：摘要在此刻绑定幂等键——并发孪生/
	// 异摘要在此处分胜负（审校 P1：异摘要创建前退出零副作用）
	claim := Receipt{
		ReceiptID:       s.newReceiptID(),
		CallerRef:       CallerOwner,
		Scope:           ScopeWorkspace,
		IdempotencyKey:  cmd.IdempotencyKey,
		RequestID:       cmd.RequestID,
		RequestDigest:   d.RequestDigest,
		RouteID:         d.RouteID,
		DecisionVersion: d.DecisionVersion,
		TargetRefs:      d.TargetRefs,
		CommittedAt:     "",
	}
	existing, conflict, finalized, err := s.store.ClaimKey(ctx, claim)
	if err != nil {
		return Receipt{}, false, err
	}
	if conflict {
		return Receipt{}, false, fmt.Errorf("%w: 键 %s 已绑定摘要 %s（异载荷）", ErrIdempotencyConflict, cmd.IdempotencyKey, existing.RequestDigest[:min(12, len(existing.RequestDigest))])
	}
	if finalized {
		return existing, true, nil // 同摘要已完结 → 重放（并发路径兜底）
	}
	// 复审 #8：pending 采纳原行——receipt_id/request_id 沿用首认领（换绑拒绝
	// 后换路由重投的成功响应与重放/按键查询读到同一份持久化回执）。
	if existing.ReceiptID != "" {
		claim.ReceiptID = existing.ReceiptID
		claim.RequestID = existing.RequestID
	}
	// 拒绝即释放认领（复审 #8：确定失败的请求不占键——复核/预检/建单拒绝后
	// 换新决策同键重投不被旧认领绑死；同摘要 pending 无领域事实，删除安全）。
	fail := func(err error) (Receipt, bool, error) {
		_ = s.store.ReleaseKey(ctx, CallerOwner, ScopeWorkspace, cmd.IdempotencyKey, d.RequestDigest)
		return Receipt{}, false, err
	}

	// 0.6.13 / ADR-0036 §2（审校 #2/#10 修订）：slot 寻址决策的投递复核——
	// 重解析原始请求，任职与实例**双核对**决策事实（模板实例集漂移即 stale，
	// §4.6"不能只改路由缓存"）；显式 owner 路径复核不变（precheck）。读侧
	// 快失败，权威核对在 CreateWithSlot 建单事务内（审校 #2：换绑不得插进
	// 复核与建单之间）。
	if d.Request.SlotInstanceID != "" || d.Request.SlotSpecID != "" {
		if s.lookups.Slots == nil {
			return Receipt{}, false, fmt.Errorf("%w: 职责寻址端口未装配（无法复核 slot 决策）", ErrRouteStale)
		}
		agent, instance, code, ok := s.resolveSlot(ctx, &d.Request)
		if !ok {
			return fail(fmt.Errorf("%w: 职责复核不再满足（%s）——重新解析", ErrRouteStale, code))
		}
		if agent != d.ResolvedAgentID || instance != d.ResolvedInstanceID {
			return fail(fmt.Errorf("%w: 解析事实已漂移（任职 %s→%s / 实例 %s→%s）——重新解析",
				ErrRouteStale, d.ResolvedAgentID, agent, d.ResolvedInstanceID, instance))
		}
	}

	if code, summary, ok := s.precheck(ctx, d.Request, d.ResolvedAgentID, d.ResolvedInstanceID); !ok {
		// 映射被占（含崩溃窗口孤儿）单列可见恢复；其余状态变化 → 重新解析
		if code == "source_already_mapped" {
			return Receipt{}, false, fmt.Errorf("%w: %s", mission.ErrSourceAlreadyMapped, summary)
		}
		return fail(fmt.Errorf("%w: 预检不再满足（%s：%s）", ErrRouteStale, code, summary))
	}
	// 建单：网关交办一律走事务门（复审 #3/#7）——slot 寻址携意图五核 + 模板
	// 唯一性锚（SpecID = 原始模板引用）；显式 owner 交办携 RequireOwnerActive
	// 身份门（预检后退役/暂停不得落入新承诺）。门拒绝 → route_stale 重新解析。
	var m mission.Mission
	if d.ResolvedInstanceID != "" {
		m, err = s.missions.CreateGated(ctx, d.ResolvedAgentID, d.Request.Contract, &d.Request.RoomSource,
			&mission.CreationGate{Slot: &mission.SlotIntent{
				InstanceID: d.ResolvedInstanceID, AgentID: d.ResolvedAgentID, SpecID: d.Request.SlotSpecID,
			}})
	} else {
		m, err = s.missions.CreateGated(ctx, d.Request.OwnerAgentID, d.Request.Contract, &d.Request.RoomSource,
			&mission.CreationGate{RequireOwnerActive: d.Request.OwnerAgentID != ""})
	}
	if err != nil {
		// 孪生已创建（pending 认领未完结的恢复重驱动）：完结回执后重放
		if errors.Is(err, mission.ErrSourceAlreadyMapped) {
			if prior, found, rerr := s.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, cmd.IdempotencyKey); rerr == nil && found && prior.RequestDigest == d.RequestDigest && prior.DomainCommandID != "" {
				return prior, true, nil
			}
		}
		if errors.Is(err, mission.ErrSlotIntentViolated) || errors.Is(err, mission.ErrOwnerGateViolated) {
			return fail(fmt.Errorf("%w: %v", ErrRouteStale, err))
		}
		return fail(err)
	}
	out := claim
	out.DomainCommandID = m.MissionID
	out.ReturnRefs = d.Request.Contract.ReturnTargets
	out.CommittedAt = s.now()
	if err := s.store.FinalizeReceipt(ctx, out); err != nil {
		return Receipt{}, false, err
	}
	return out, false, nil
}

// UpdateCmd 续接命令（§10.3 mission_update / ADR-0036 §1）：全量替换契约，
// owner 不变（换人走交接 0.6.12 或修订命令面显式改）。
type UpdateCmd struct {
	MissionID       string
	ExpectedVersion int
	IdempotencyKey  string
	RequestID       string
	Contract        mission.Content
	Reason          string
}

// updateDigest 续接命令摘要（幂等键绑定的请求身份——同键同摘要即同一请求）。
func updateDigest(cmd UpdateCmd) string {
	raw, _ := json.Marshal(struct {
		UpdateCmd
		Contract mission.Content `json:"contract"`
	}{cmd, cmd.Contract})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// validateUpdate 续接命令域校验（0.6.13 审校 #17：进入幂等认领前——无效请求
// 400 修正后重试，不得被认领键绑死；与 OpenAPI DispatchUpdateRequest 同口径）。
func validateUpdate(cmd UpdateCmd) error {
	if !idempotencyKeyPattern.MatchString(cmd.IdempotencyKey) {
		return fmt.Errorf("%w: idempotency_key 须 [A-Za-z0-9._:-]{1,128}", ErrInvalidRequest)
	}
	if cmd.MissionID == "" || cmd.ExpectedVersion < 1 {
		return fmt.Errorf("%w: mission_id 必填且 expected_version ≥1（得 %q / %d）", ErrInvalidRequest, cmd.MissionID, cmd.ExpectedVersion)
	}
	if utf8.RuneCountInString(cmd.RequestID) > 128 {
		return fmt.Errorf("%w: request_id ≤128 字", ErrInvalidRequest)
	}
	if utf8.RuneCountInString(cmd.Reason) > 280 {
		return fmt.Errorf("%w: reason ≤280 字", ErrInvalidRequest)
	}
	return nil
}

// Update 网关续接（§10.3 / ADR-0036 §1；0.6.13 审校 #3/#11/#12/#13 重写）：
// 校验 → **同一写事务**[认领 → 复核（任务存在 + owner 在册且 active——事务
// 内身份门，暂停/未指派/读取失败皆拒）→ revise（CAS + 归属锚取事务内读值，
// owner 不变）→ 回执（reason 随行持久化，receipt_id 沿用 pending 原行，
// target_refs 携修订后版本）]。任一步失败整体回滚——领域拒绝不占键（修正后
// 可重试）、修订已生效而回执失败不再出现（同事务提交）。领域拒绝原样透传
// （版本冲突/归属锚/终态/冻结窗/契约非法——网关不吞并）。无 route 决策行
// （无寻址面，竞态防护即 CAS + 归属锚）。SQLite 实装 UpdateMissionAtomic；
// 内存件退化为分步 + 拒绝回滚补偿（ReleaseKey）+ pending 采纳原行。
func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (Receipt, bool, error) {
	if err := validateUpdate(cmd); err != nil {
		return Receipt{}, false, err
	}
	dg := updateDigest(cmd)
	// #3：原子面优先（认领+复核+修订+回执同一写事务——app 装配注入 SQLite 实装）
	if au, ok := s.missions.(AtomicUpdater); ok {
		return au.UpdateMissionAtomic(ctx, cmd, dg, s.now(), s.newReceiptID())
	}
	// 已完结回执先于一切复核（重试不受此后状态变化影响；pending 认领是事务
	// 内部事实——公开查询不可见，#11，经 ClaimKey 分辨重驱动）。
	if prior, found, err := s.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, cmd.IdempotencyKey); err != nil {
		return Receipt{}, false, err
	} else if found {
		if prior.RequestDigest != dg {
			return Receipt{}, false, fmt.Errorf("%w: 键 %s 已绑定其他请求", ErrIdempotencyConflict, cmd.IdempotencyKey)
		}
		return prior, true, nil
	}
	claim := Receipt{
		ReceiptID:      s.newReceiptID(),
		CallerRef:      CallerOwner,
		Scope:          ScopeWorkspace,
		IdempotencyKey: cmd.IdempotencyKey,
		RequestID:      cmd.RequestID,
		RequestDigest:  dg,
		Reason:         cmd.Reason,
		TargetRefs:     []string{cmd.MissionID},
	}
	existing, conflict, finalized, err := s.store.ClaimKey(ctx, claim)
	if err != nil {
		return Receipt{}, false, err
	}
	if conflict {
		return Receipt{}, false, fmt.Errorf("%w: 键 %s 已绑定其他请求", ErrIdempotencyConflict, cmd.IdempotencyKey)
	}
	if finalized {
		return existing, true, nil
	}
	// #13 + 复审 #12：pending 采纳原行——receipt_id/request_id 沿用首认领
	//（恢复重驱动与后续重放给出同一回执）；reason 空值（v30 旧列缺省）回落
	// 本次命令值——摘要等值已证明同一请求（reason 在摘要内）。
	if existing.ReceiptID != "" {
		claim.ReceiptID = existing.ReceiptID
		claim.RequestID = existing.RequestID
		if existing.Reason == "" {
			claim.Reason = cmd.Reason
		}
	}
	// 拒绝即回滚认领（#11：确定失败的请求不占键——修正后重试不再 409 幂等冲突）
	fail := func(err error) (Receipt, bool, error) {
		_ = s.store.ReleaseKey(ctx, CallerOwner, ScopeWorkspace, cmd.IdempotencyKey, dg)
		return Receipt{}, false, err
	}
	// 服务端复核：任务存在 + owner 在册且 active（#12：查询失败即错——不吞；
	// 未指派/未在册/非 active 按续接资格明确拒绝）。
	cur, found, err := s.missions.Get(ctx, cmd.MissionID)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(mission.ErrMissionNotFound)
	}
	if cur.OwnerAgentID == "" {
		return fail(fmt.Errorf("%w: 任务未指派负责人（%s）——先交办或交接", ErrTargetNotActive, cmd.MissionID))
	}
	if s.lookups.Agents == nil {
		return fail(fmt.Errorf("%w: %v", ErrTargetNotActive, mission.ErrAgentStatusUnavailable))
	}
	st, aok, aerr := s.lookups.Agents.AgentStatus(ctx, cur.OwnerAgentID)
	if aerr != nil {
		return fail(fmt.Errorf("dispatch: 负责人状态查询失败（%s）：%w", cur.OwnerAgentID, aerr))
	}
	if !aok {
		return fail(fmt.Errorf("%w: 负责人 %s 不在册", ErrTargetNotActive, cur.OwnerAgentID))
	}
	if st != mission.AgentStatusActive {
		return fail(fmt.Errorf("%w: 负责人 %s 非 active（%s）", ErrTargetNotActive, cur.OwnerAgentID, st))
	}
	// 修订：归属锚取服务端读值（网关非浏览器——读值即可信）；owner 不变。
	up, err := s.missions.Revise(ctx, cmd.MissionID, cmd.ExpectedVersion, cur.OwnerAgentID, cur.OwnerAgentID, cmd.Contract)
	if err != nil {
		// 复审 #11（对齐原子面）：版本恰 expected+1 且现行内容与载荷等值 =
		// 该命令的修订确已提交（认领后完结前中断/旧版残留）——完结回执而非
		// 释放键（可能已提交的键不得删除）；不等值不能证明，走拒绝释放。
		if errors.Is(err, mission.ErrVersionConflict) {
			if now, found2, gerr := s.missions.Get(ctx, cmd.MissionID); gerr == nil && found2 &&
				now.ContractVersion == cmd.ExpectedVersion+1 && mission.SameContent(now.Content, cmd.Contract) {
				up = now
			} else {
				return fail(err)
			}
		} else {
			return fail(err) // 领域拒绝透传（owner_mismatch/invalid_transition/invalid_mission）
		}
	}
	out := claim
	out.DomainCommandID = fmt.Sprintf("%s:revise:v%d", up.MissionID, up.ContractVersion)
	// #16：修订后版本写入目标引用（可凭回执定位契约版本行）。
	out.TargetRefs = []string{"mission:" + up.MissionID + ":v" + strconv.Itoa(up.ContractVersion)}
	out.ReturnRefs = up.Content.ReturnTargets
	out.CommittedAt = s.now()
	if err := s.store.FinalizeReceipt(ctx, out); err != nil {
		return Receipt{}, false, err
	}
	return out, false, nil
}

// ReceiptByKey 回执查询（get_dispatch_receipt）。
func (s *Service) ReceiptByKey(ctx context.Context, key string) (Receipt, bool, error) {
	return s.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, key)
}

func expiryAfter(nowRFC string, ttl time.Duration) string {
	t, err := time.Parse(time.RFC3339Nano, nowRFC)
	if err != nil {
		t = time.Now().UTC()
	}
	return t.Add(ttl).UTC().Format(time.RFC3339Nano)
}

func expired(expiresRFC, nowRFC string) bool {
	exp, err1 := time.Parse(time.RFC3339Nano, expiresRFC)
	now, err2 := time.Parse(time.RFC3339Nano, nowRFC)
	if err1 != nil || err2 != nil {
		return true // 时间不可解析按过期处理（安全缺省：重新解析）
	}
	return !now.Before(exp)
}
