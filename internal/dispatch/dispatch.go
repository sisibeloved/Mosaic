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
	"time"

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
	// 职责引用（0.6.13 / ADR-0036 §2）：二选一，与 OwnerAgentID 互斥（并存
	// 结构拒绝——显式 Agent ≠ 责任已移交，语义须先明确）。寻址结果写入
	// OwnerAgentID（决策存档全文随 Request 落库——投递复核据此重查）。
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
	Request         ResolveRequest `json:"request"` // 投递输入（存档全文）
	ExpiresAt       string         `json:"expires_at"`
	CreatedAt       string         `json:"created_at"`
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
	CommittedAt     string   `json:"committed_at"`
}

// Missions 领域服务端口（*mission.Service 实现；dispatch 编排不复制域逻辑）。
type Missions interface {
	Create(ctx context.Context, ownerAgentID string, c mission.Content, src *mission.RoomSource) (mission.Mission, error)
	// Get / Revise 续接（0.6.13 / ADR-0036 §1 mission_update）：复核读现值 +
	// CAS 修订（归属锚由网关以服务端读值填充——owner 不变）。
	Get(ctx context.Context, missionID string) (mission.Mission, bool, error)
	Revise(ctx context.Context, missionID string, expectedVersion int, expectedOwner, ownerAgentID string, c mission.Content) (mission.Mission, error)
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
}

// 寻址来源与 blocked 原因码（0.6.13）。
const (
	SelectionSlotBinding   = "slot_binding"
	ReasonSlotNoInstance   = "slot_no_instance"
	ReasonSlotAmbiguous    = "slot_ambiguous"
	ReasonSlotUnassigned   = "slot_unassigned"
	ReasonSlotInstanceGone = "slot_instance_not_found"
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
	// FinalizeReceipt 填 domain_command_id/committed_at（幂等 UPDATE——
	// 认领者创建成功后完结回执）。
	FinalizeReceipt(ctx context.Context, r Receipt) error
	// ReceiptByKey 幂等键查回执；未投递 ok=false。
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
	// 0.6.13 / ADR-0036 §2：职责寻址先行——解析结果作为 owner 进决策存档；
	// 寻址失败是 blocked 决策（恒 200），端口缺失/互斥违反是结构错误（400）。
	selection := "room_default"
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
		agent, code, ok := s.resolveSlot(ctx, &req)
		if !ok {
			d := Decision{
				RouteID: s.newRouteID(), DecisionVersion: 1, RequestDigest: digest(req),
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
		req.OwnerAgentID = agent
		selection = SelectionSlotBinding
	} else if req.OwnerAgentID != "" {
		selection = "explicit_owner"
	}
	now := s.now()
	d := Decision{
		RouteID:         s.newRouteID(),
		DecisionVersion: 1,
		RequestDigest:   digest(req),
		Status:          "resolved",
		SelectionSource: selection,
		TargetRefs:      []string{"room:" + req.RoomSource.RoomID + ":" + req.RoomSource.TaskID},
		Request:         req,
		ExpiresAt:       expiryAfter(now, DecisionTTL),
		CreatedAt:       now,
	}
	if code, summary, ok := s.precheck(ctx, req); !ok {
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

// resolveSlot 职责寻址（§4.5）：显式实例或模板→scope 内唯一实例 → active 任职 →
// Agent。无实例 / 多实例歧义 / 无 active 任职（不自动激活 ready Binding）各 blocked。
func (s *Service) resolveSlot(ctx context.Context, req *ResolveRequest) (agent, reasonCode string, ok bool) {
	instanceID := req.SlotInstanceID
	if instanceID == "" {
		instances, err := s.lookups.Slots.SlotInstancesOfSpec(ctx, req.SlotSpecID)
		if err != nil {
			return "", "slot_lookup_failed", false
		}
		switch len(instances) {
		case 0:
			return "", ReasonSlotNoInstance, false
		case 1:
			instanceID = instances[0]
		default:
			return "", ReasonSlotAmbiguous, false // 同名模板跨项目多实例——禁止猜选，须显式指实例
		}
	}
	exists, err := s.lookups.Slots.SlotInstanceExists(ctx, instanceID)
	if err != nil {
		return "", "slot_lookup_failed", false
	}
	if !exists {
		return "", ReasonSlotInstanceGone, false
	}
	a, active, err := s.lookups.Slots.ActiveBindingAgent(ctx, instanceID)
	if err != nil {
		return "", "slot_lookup_failed", false
	}
	if !active {
		return "", ReasonSlotUnassigned, false // 待任职——不能自动激活 ready Binding
	}
	return a, "", true
}

// precheck 预检单源（Resolve 与 Dispatch 复核同源——目标变化即 stale）：
// 房间任务卡存在、映射未被占、owner（若指定）存在、契约内容合法。
func (s *Service) precheck(ctx context.Context, req ResolveRequest) (code, summary string, ok bool) {
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
				fmt.Sprintf("(%s, %s) 已交办为 %s——重试请走原 Mission", req.RoomSource.RoomID, req.RoomSource.TaskID, existing), false
		}
	}
	if req.OwnerAgentID != "" {
		if s.lookups.Agents == nil {
			return "agent_unavailable", "身份查询端口未装配", false
		}
		exists, err := s.lookups.Agents.AgentExists(ctx, req.OwnerAgentID)
		if err == nil && !exists {
			return "agent_not_found", "负责人 Agent 不存在（" + req.OwnerAgentID + "）", false
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
	// pending 同摘要：本调用即认领持有者或恢复重驱动，继续

	// 0.6.13 / ADR-0036 §2：slot 寻址决策的投递复核——任职换人即目标漂移
	//（§4.6"不能只改路由缓存"）；显式 owner 路径复核不变（precheck）。
	if d.Request.SlotInstanceID != "" || d.Request.SlotSpecID != "" {
		if s.lookups.Slots == nil {
			return Receipt{}, false, fmt.Errorf("%w: 职责寻址端口未装配（无法复核 slot 决策）", ErrRouteStale)
		}
		agent, code, ok := s.resolveSlot(ctx, &d.Request)
		if !ok {
			return Receipt{}, false, fmt.Errorf("%w: 职责复核不再满足（%s）——重新解析", ErrRouteStale, code)
		}
		if agent != d.Request.OwnerAgentID {
			return Receipt{}, false, fmt.Errorf("%w: 实例任职已换人（%s → %s）——重新解析", ErrRouteStale, d.Request.OwnerAgentID, agent)
		}
	}

	if code, summary, ok := s.precheck(ctx, d.Request); !ok {
		// 映射被占（含崩溃窗口孤儿）单列可见恢复；其余状态变化 → 重新解析
		if code == "source_already_mapped" {
			return Receipt{}, false, fmt.Errorf("%w: %s", mission.ErrSourceAlreadyMapped, summary)
		}
		return Receipt{}, false, fmt.Errorf("%w: 预检不再满足（%s：%s）", ErrRouteStale, code, summary)
	}
	m, err := s.missions.Create(ctx, d.Request.OwnerAgentID, d.Request.Contract, &d.Request.RoomSource)
	if err != nil {
		// 孪生已创建（pending 认领未完结的恢复重驱动）：完结回执后重放
		if errors.Is(err, mission.ErrSourceAlreadyMapped) {
			if prior, found, rerr := s.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, cmd.IdempotencyKey); rerr == nil && found && prior.RequestDigest == d.RequestDigest && prior.DomainCommandID != "" {
				return prior, true, nil
			}
		}
		return Receipt{}, false, err
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

// Update 网关续接（§10.3 / ADR-0036 §1）：幂等先行（同 create 路径 ClaimKey 机制
// ——同键同摘要重放原回执、同键异载荷冲突、pending 恢复重驱动）→ 服务端复核
// （任务存在 + owner active）→ revise（CAS + 归属锚取服务端读值，owner 不变）
// → 回执（domain_command_id = mission:<id>:revise:v<新版本>）。领域拒绝原样
// 透传（版本冲突/归属锚/终态/冻结窗/契约非法——网关不吞并）。无 route 决策行
// （无寻址面，竞态防护即 CAS + 归属锚）。
func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (Receipt, bool, error) {
	if !idempotencyKeyPattern.MatchString(cmd.IdempotencyKey) {
		return Receipt{}, false, fmt.Errorf("%w: idempotency_key 须 [A-Za-z0-9._:-]{1,128}", ErrInvalidRequest)
	}
	dg := updateDigest(cmd)
	// 已完结回执先于一切复核（重试不受此后状态变化影响——同 create 次序）
	if prior, found, err := s.store.ReceiptByKey(ctx, CallerOwner, ScopeWorkspace, cmd.IdempotencyKey); err != nil {
		return Receipt{}, false, err
	} else if found {
		if prior.RequestDigest != dg {
			return Receipt{}, false, fmt.Errorf("%w: 键 %s 已绑定其他请求", ErrIdempotencyConflict, cmd.IdempotencyKey)
		}
		if prior.DomainCommandID != "" {
			return prior, true, nil
		}
		// pending 同摘要 → 恢复重驱动（落到下方认领与修订）
	}
	claim := Receipt{
		ReceiptID:      s.newReceiptID(),
		CallerRef:      CallerOwner,
		Scope:          ScopeWorkspace,
		IdempotencyKey: cmd.IdempotencyKey,
		RequestID:      cmd.RequestID,
		RequestDigest:  dg,
		TargetRefs:     []string{cmd.MissionID},
	}
	if existing, conflict, finalized, err := s.store.ClaimKey(ctx, claim); err != nil {
		return Receipt{}, false, err
	} else if conflict {
		return Receipt{}, false, fmt.Errorf("%w: 键 %s 已绑定其他请求", ErrIdempotencyConflict, cmd.IdempotencyKey)
	} else if finalized {
		return existing, true, nil
	}
	// 服务端复核：任务存在 + owner active（续接不发往已停身份）
	cur, found, err := s.missions.Get(ctx, cmd.MissionID)
	if err != nil {
		return Receipt{}, false, err
	}
	if !found {
		return Receipt{}, false, mission.ErrMissionNotFound
	}
	if s.lookups.Agents != nil {
		if st, ok, aerr := s.lookups.Agents.AgentStatus(ctx, cur.OwnerAgentID); aerr == nil && ok && st != mission.AgentStatusActive {
			return Receipt{}, false, fmt.Errorf("%w: 负责人 %s 非 active（%s）", ErrTargetNotActive, cur.OwnerAgentID, st)
		}
	}
	// 修订：归属锚取服务端读值（网关非浏览器——读值即可信）；owner 不变。
	up, err := s.missions.Revise(ctx, cmd.MissionID, cmd.ExpectedVersion, cur.OwnerAgentID, cur.OwnerAgentID, cmd.Contract)
	if err != nil {
		return Receipt{}, false, err // 领域拒绝透传（version_conflict/owner_mismatch/invalid_transition/invalid_mission）
	}
	out := claim
	out.DomainCommandID = fmt.Sprintf("%s:revise:v%d", up.MissionID, up.ContractVersion)
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
