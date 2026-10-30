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
)

// ResolveRequest 解析请求（RouteRequest 的 0.6.1 确定性子集）：房间来源 +
// 契约内容 + 可选 owner。决策存档本请求全文，投递以此为准。
type ResolveRequest struct {
	Kind         string             `json:"kind"`
	RoomSource   mission.RoomSource `json:"room_source"`
	OwnerAgentID string             `json:"owner_agent_id,omitempty"`
	Contract     mission.Content    `json:"contract"`
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
}

// Lookups 复核端口（预检与复核同源）：owner 存在性 / 房间任务卡存在性 / 映射查询。
type Lookups struct {
	Agents  mission.AgentLookup
	Sources mission.SourceLookup
	Mapped  func(ctx context.Context, roomID, taskID string) (string, bool, error)
}

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
	now := s.now()
	d := Decision{
		RouteID:         s.newRouteID(),
		DecisionVersion: 1,
		RequestDigest:   digest(req),
		Status:          "resolved",
		SelectionSource: "room_default",
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
