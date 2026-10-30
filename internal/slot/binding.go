package slot

// Binding 任职状态机（RFC-0018 §3.1/§3.2/§5.2 / ADR-0021，0.5.4）。
// preparing →（readiness 门）ready → active →（排空门）released；
// 准备失败 → blocked → preparing（0.5.5 起真实，服务层转移先行）。
// 单 active：同实例最多一位在任（存储层部分唯一索引兜底，AT-13）。

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Binding 状态。
const (
	BindingPreparing = "preparing"
	BindingReady     = "ready"
	BindingActive    = "active"
	BindingDraining  = "draining"
	BindingReleased  = "released"
	BindingBlocked   = "blocked"
)

// 领域错误。
var (
	// ErrAgentNotActive Agent 非 active（suspended/retired 不接任职）。
	ErrAgentNotActive = errors.New("slot: Agent 非 active，不能任职")
	// ErrInvalidSpecVersion 指定模板版本超谱系。
	ErrInvalidSpecVersion = errors.New("slot: 模板版本超谱系")
	// ErrActiveBindingExists 同实例已有 active 任职（单 active 不变量）。
	ErrActiveBindingExists = errors.New("slot: 实例已有在任（单 active）")
	// ErrInvalidTransition 任职转移非法（可见拒绝，不静默幂等）。
	ErrInvalidTransition = errors.New("slot: 非法任职转移")
	// ErrInvalidBinding 任职参数不合法（交接备注超长等）。
	ErrInvalidBinding = errors.New("slot: 任职参数不合法")
	// ErrReadinessNotMet 激活时点授权不再满足模板必需项（准入两时点复核，
	// 审校 P1#2：ready 只代表 prepare 时点就绪，授权此后可被撤销）。
	ErrReadinessNotMet = errors.New("slot: 授权不再满足模板必需项，不能激活")
)

// Binding 任职记录（投影行；事件流见 binding_events）。
type Binding struct {
	BindingID       string `json:"binding_id"`
	AgentID         string `json:"agent_id"`
	SlotInstanceID  string `json:"slot_instance_id"`
	SlotSpecVersion int    `json:"slot_spec_version"`
	Scope           string `json:"scope"`
	Status          string `json:"status"`
	HandoffNote     string `json:"handoff_note"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// BindingEvent binding_events 流事件（schema: BindingEvent）。
type BindingEvent struct {
	Seq        int64           `json:"seq"`
	BindingID  string          `json:"binding_id"`
	EventType  string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
	OccurredAt string          `json:"occurred_at"`
}

// ReleaseResult 释放结果（schema: BindingReleaseResult）。
type ReleaseResult struct {
	Binding     Binding  `json:"binding"`
	Outstanding []string `json:"outstanding"`
}

// ReadinessChecker 准入门（0.5.5 权限交集/能力核对实装——ADR-0022）：核对
// 候选任职的就绪性并产出完整报告（缺失 → blocked；降级与有效权限入报告与
// 事件）。生产装配 GrantReadinessChecker；缺省 nil = 恒通过（保留给
// 无要求场景与既有注入测试）。
type ReadinessChecker interface {
	// Check 返回 readiness report（blocked 判定与事件 payload 的单源）。
	Check(ctx context.Context, candidate Binding) (ReadinessReport, error)
}

// DrainChecker 排空门（0.6 Mission 域在途工作的挂靠点）：返回该任职未处置
// 的在途清单。0.5.4 生产缺省 nil = 恒空（release 一次调用至 released）。
type DrainChecker interface {
	Outstanding(ctx context.Context, bindingID string) ([]string, error)
}

// AgentLookup 身份域查询端口（slot 不 import personalagent——依赖方向与
// harness 适配器同口径；生产注入身份服务）。
type AgentLookup interface {
	// AgentStatus 返回 Agent 状态；未知 ok=false。
	AgentStatus(ctx context.Context, agentID string) (status string, ok bool, err error)
}

// 任职事件类型（binding_events 流；与 agent_events 同形）。
const (
	EventCreated       = "created"
	EventStatusChanged = "status_changed"
)

// allowedIn 来源集合判定。
func allowedIn(allowedFrom []string, status string) bool {
	for _, f := range allowedFrom {
		if f == status {
			return true
		}
	}
	return false
}

// newBindingID 缺省任职 ID：bd_ + 时间有序毫秒（36 进制）+ 随机后缀。
func newBindingID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return "bd_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// PrepareBinding 建任职（= prepare_binding，§11）：实例/Agent 校验 → 版本
// 钉住（缺省谱系最新）→ 行创建（preparing + created 事件）→ readiness 门 →
// ready 或 blocked（missing 记入事件）。Agent 须存在且 active。
func (s *Service) PrepareBinding(ctx context.Context, slotInstanceID, agentID string, specVersion int) (Binding, error) {
	inst, ok, err := s.store.InstanceByID(ctx, slotInstanceID)
	if err != nil {
		return Binding{}, err
	}
	if !ok {
		return Binding{}, ErrInstanceNotFound
	}
	status, ok, err := s.agentStatusOf(ctx, agentID)
	if err != nil {
		return Binding{}, err
	}
	if !ok {
		return Binding{}, ErrAgentNotFound
	}
	if status != "active" {
		return Binding{}, fmt.Errorf("%w: %s（%s）", ErrAgentNotActive, agentID, status)
	}
	latest, ok, err := s.store.Latest(ctx, inst.SlotSpecID)
	if err != nil {
		return Binding{}, err
	}
	if !ok {
		return Binding{}, ErrNotFound // 谱系缺失（病态，无删除路径）
	}
	version := latest.Version // 缺省（0）：钉谱系最新
	if specVersion != 0 {
		if specVersion < 1 || specVersion > latest.Version {
			return Binding{}, fmt.Errorf("%w: %d ∉ [1, %d]（%s）", ErrInvalidSpecVersion, specVersion, latest.Version, inst.SlotSpecID)
		}
		version = specVersion
	}
	// 门判定前置（审校 P2#3）：建行与 created + preparing→target 两事件
	// 同一事务一次提交——中断/失败零残留（不留无法推进的 preparing 死记录）
	target, payload, err := s.runAdmission(ctx, Binding{
		AgentID: agentID, SlotInstanceID: slotInstanceID, SlotSpecVersion: version, Scope: inst.Scope,
	})
	if err != nil {
		return Binding{}, err
	}
	b, err := s.store.CreateBinding(ctx, Binding{
		BindingID:       s.newBindingID(),
		AgentID:         agentID,
		SlotInstanceID:  slotInstanceID,
		SlotSpecVersion: version,
		Scope:           inst.Scope,
		Status:          target, // 终态直接落库（created + preparing→target 同事务）
		CreatedAt:       s.now(),
		UpdatedAt:       s.now(),
	}, target, payload)
	if err != nil {
		return Binding{}, fmt.Errorf("slot: 建任职: %w", err)
	}
	return b, nil
}

// RetryPrepare blocked → preparing →（门）ready/blocked（0.5.5 动作；
// 服务层与注入测试先行）。仅 blocked 可重试。
func (s *Service) RetryPrepare(ctx context.Context, bindingID string) (Binding, error) {
	b, ok, err := s.store.BindingByID(ctx, bindingID)
	if err != nil {
		return Binding{}, err
	}
	if !ok {
		return Binding{}, ErrBindingNotFound
	}
	if b.Status != BindingBlocked {
		return Binding{}, fmt.Errorf("%w: %s → preparing（仅 blocked 可重试）", ErrInvalidTransition, b.Status)
	}
	// 单事务重试（审校 P2#3）：blocked → preparing → target 两事件一次提交，
	// 中断不留 preparing 死记录
	target, payload, err := s.runAdmission(ctx, b)
	if err != nil {
		return Binding{}, err
	}
	b, done, err := s.store.RetryBindingPreparation(ctx, bindingID, target, payload, s.now())
	if err != nil {
		return Binding{}, err
	}
	if !done {
		return Binding{}, fmt.Errorf("%w: blocked → preparing", ErrInvalidTransition)
	}
	return b, nil
}

// ActivateBinding 激活（ready → active）：Agent 仍须 active 且授权仍满足
// 钉住版本必需项（两时点复核，审校 P1#2——撤权后激活可见拒绝）；单 active
// 由存储层部分唯一索引兜底（并发恰一胜者，败者 ErrActiveBindingExists）。
func (s *Service) ActivateBinding(ctx context.Context, bindingID string) (Binding, error) {
	b, ok, err := s.store.BindingByID(ctx, bindingID)
	if err != nil {
		return Binding{}, err
	}
	if !ok {
		return Binding{}, ErrBindingNotFound
	}
	status, ok, err := s.agentStatusOf(ctx, b.AgentID)
	if err != nil {
		return Binding{}, err
	}
	if !ok {
		return Binding{}, ErrNotFound
	}
	if status != "active" {
		return Binding{}, fmt.Errorf("%w: %s（%s）", ErrAgentNotActive, b.AgentID, status)
	}
	// 状态前置判定：非 ready 一律 invalid_transition（错误优先级先于复核）
	if b.Status != BindingReady {
		return Binding{}, fmt.Errorf("%w: %s → active", ErrInvalidTransition, b.Status)
	}
	// 准入两时点复核（审校 P1#2）：按钉住版本重算交集，必需项缺失 → 409
	// 可见拒绝（与 Agent active 两时点校验同口径；复核拒绝不落状态不留痕，
	// 修复路径仍是改授权后激活）
	target, payload, err := s.runAdmission(ctx, b)
	if err != nil {
		return Binding{}, err
	}
	if target == BindingBlocked {
		return Binding{}, fmt.Errorf("%w: %s", ErrReadinessNotMet, payload["missing"])
	}
	_, done, err := s.store.TransitionBinding(ctx, bindingID,
		[]string{BindingReady}, BindingActive, nil, s.now())
	if err != nil {
		return Binding{}, err
	}
	if !done {
		return Binding{}, fmt.Errorf("%w: %s → active", ErrInvalidTransition, b.Status)
	}
	b.Status = BindingActive
	b.UpdatedAt = s.now()
	return b, nil
}

// ReleaseBinding 释放：active →（排空门）released（门空时两段事件与终态
// 同一事务一次提交；非空停留 draining 返回未处置清单）；ready → released
// 单段（从未在任，无物可排空）；draining 重试只补终段。released 终态全拒。
func (s *Service) ReleaseBinding(ctx context.Context, bindingID, handoffNote string) (ReleaseResult, error) {
	if len([]rune(handoffNote)) > 4096 {
		return ReleaseResult{}, fmt.Errorf("%w: handoff_note ≤4096 字", ErrInvalidBinding)
	}
	b, ok, err := s.store.BindingByID(ctx, bindingID)
	if err != nil {
		return ReleaseResult{}, err
	}
	if !ok {
		return ReleaseResult{}, ErrBindingNotFound
	}

	outstanding := []string{}
	switch b.Status {
	case BindingReady:
		// 从未在任：单段直释
		if _, done, err := s.store.TransitionBinding(ctx, bindingID,
			[]string{BindingReady}, BindingReleased, releasePayload(handoffNote), s.now()); err != nil {
			return ReleaseResult{}, err
		} else if !done {
			return ReleaseResult{}, fmt.Errorf("%w: ready → released", ErrInvalidTransition)
		}
	case BindingActive:
		if s.drain != nil {
			items, err := s.drain.Outstanding(ctx, bindingID)
			if err != nil {
				return ReleaseResult{}, fmt.Errorf("slot: 排空门: %w", err)
			}
			if len(items) > 0 {
				outstanding = items
			}
		}
		if len(outstanding) > 0 {
			// 停留 draining（在途未处置）；重试只补终段，draining 事件不重复
			if _, done, err := s.store.TransitionBinding(ctx, bindingID,
				[]string{BindingActive}, BindingDraining, releasePayload(handoffNote), s.now()); err != nil {
				return ReleaseResult{}, err
			} else if !done {
				return ReleaseResult{}, fmt.Errorf("%w: active → draining", ErrInvalidTransition)
			}
		} else if _, done, err := s.store.ReleaseBinding(ctx, bindingID, handoffNote, s.now()); err != nil {
			return ReleaseResult{}, err
		} else if !done {
			return ReleaseResult{}, fmt.Errorf("%w: active → released", ErrInvalidTransition)
		}
	case BindingDraining:
		if s.drain != nil {
			items, err := s.drain.Outstanding(ctx, bindingID)
			if err != nil {
				return ReleaseResult{}, fmt.Errorf("slot: 排空门: %w", err)
			}
			if len(items) > 0 {
				outstanding = items
			}
		}
		if len(outstanding) > 0 {
			break // 仍在途：停留（不重复记事件）
		}
		if _, done, err := s.store.TransitionBinding(ctx, bindingID,
			[]string{BindingDraining}, BindingReleased, releasePayload(handoffNote), s.now()); err != nil {
			return ReleaseResult{}, err
		} else if !done {
			return ReleaseResult{}, fmt.Errorf("%w: draining → released", ErrInvalidTransition)
		}
	default:
		return ReleaseResult{}, fmt.Errorf("%w: %s 不可释放", ErrInvalidTransition, b.Status)
	}

	got, _, err := s.store.BindingByID(ctx, bindingID)
	if err != nil || !ok {
		return ReleaseResult{}, err
	}
	return ReleaseResult{Binding: got, Outstanding: outstanding}, nil
}

// ListBindings 任职列表（创建序）；instanceID 非空时按实例过滤。
func (s *Service) ListBindings(ctx context.Context, instanceID string) ([]Binding, error) {
	return s.store.ListBindings(ctx, instanceID)
}

// GetBinding 单查；未知 ok=false。
func (s *Service) GetBinding(ctx context.Context, bindingID string) (Binding, bool, error) {
	return s.store.BindingByID(ctx, bindingID)
}

// BindingEvents 事件流读取；未知 ok=false。
func (s *Service) BindingEvents(ctx context.Context, bindingID string) ([]BindingEvent, bool, error) {
	if _, ok, err := s.store.BindingByID(ctx, bindingID); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, nil
	}
	evs, err := s.store.BindingEvents(ctx, bindingID)
	if err != nil {
		return nil, false, err
	}
	if evs == nil {
		evs = []BindingEvent{}
	}
	return evs, true, nil
}

// runAdmission 准入门判定（0.5.5 单源）：nil 门 = 恒过；报告缺失 → blocked
// 且 missing/degraded 记入事件 payload（空清单省键）；门自身故障上抛不吞。
func (s *Service) runAdmission(ctx context.Context, candidate Binding) (string, map[string]string, error) {
	if s.readiness == nil {
		return BindingReady, nil, nil
	}
	rep, err := s.readiness.Check(ctx, candidate)
	if err != nil {
		return "", nil, fmt.Errorf("slot: 准入门: %w", err)
	}
	if rep.Ready {
		if len(rep.Degraded) > 0 { // ready 仍记降级（AT-07：降级有报告）
			return BindingReady, map[string]string{"degraded": joinStrings(rep.Degraded)}, nil
		}
		return BindingReady, nil, nil
	}
	payload := map[string]string{"missing": joinStrings(rep.Missing)}
	if len(rep.Degraded) > 0 {
		payload["degraded"] = joinStrings(rep.Degraded)
	}
	return BindingBlocked, payload, nil
}

func (s *Service) agentStatusOf(ctx context.Context, agentID string) (string, bool, error) {
	if s.agentLookup == nil {
		return "", false, fmt.Errorf("slot: 未装配 AgentLookup（任职不可用）")
	}
	return s.agentLookup.AgentStatus(ctx, agentID)
}

func releasePayload(note string) map[string]string {
	if note == "" {
		return nil
	}
	return map[string]string{"handoff_note": note}
}

func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "；"
		}
		out += s
	}
	return out
}
