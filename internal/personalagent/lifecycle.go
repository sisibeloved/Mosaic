package personalagent

// 生命周期状态机（RFC-0018 §3.2 / ADR-0018，0.5.1）：
// active ↔ suspended；active|suspended → retiring → retired（唯一终态）。
// 状态经 Store.TransitionAgent 事务转移（投影 + status_changed 事件同落）；
// 同步只建不改——suspended/retired 不因 bot_id 重现复活。

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// CommitmentChecker 承诺门端口（RFC-0018："retiring 停止接新任务，处置所有
// 承诺后才能 retired"）。0.5.1 无 Mission/Binding/Goal 域——生产装配不注入
// （恒空集，retire 同调用完成 retiring→retired）；0.5.2+/0.6 各域接入后
// 注入实际检查器，retiring 停留至清单清空。
type CommitmentChecker interface {
	// Outstanding 返回该 Agent 未处置承诺的可展示清单；空集 = 可完成退役。
	Outstanding(ctx context.Context, agentID string) ([]string, error)
}

// WithCommitments 注入承诺门检查器（缺省 nil = 恒空集）。
func WithCommitments(c CommitmentChecker) Option { return func(s *Service) { s.commitments = c } }

// LifecycleResult 转移结果（schema: LifecycleResult）。
type LifecycleResult struct {
	View        AgentView `json:"agent"`
	Outstanding []string  `json:"outstanding_commitments"`
}

// Suspend active → suspended。
func (s *Service) Suspend(ctx context.Context, agentID, reason string) (LifecycleResult, error) {
	return s.simpleTransition(ctx, agentID, []string{StatusActive}, StatusSuspended, reason)
}

// Resume suspended → active。
func (s *Service) Resume(ctx context.Context, agentID, reason string) (LifecycleResult, error) {
	return s.simpleTransition(ctx, agentID, []string{StatusSuspended}, StatusActive, reason)
}

// Retire active|suspended → retiring →（承诺门空）retired。已处 retiring 时
// 不重复记 retiring 事件，只重过承诺门；retired 为终态全拒。
func (s *Service) Retire(ctx context.Context, agentID, reason string) (LifecycleResult, error) {
	cur, ok, err := s.store.AgentByID(ctx, agentID)
	if err != nil {
		return LifecycleResult{}, fmt.Errorf("personalagent: retire 前置读取: %w", err)
	}
	if !ok {
		return LifecycleResult{}, ErrAgentNotFound
	}
	switch cur.Status {
	case StatusActive, StatusSuspended:
		if _, done, err := s.store.TransitionAgent(ctx, agentID,
			[]string{cur.Status}, StatusRetiring, reason, s.now()); err != nil {
			return LifecycleResult{}, fmt.Errorf("personalagent: 入 retiring: %w", err)
		} else if !done {
			// 读后竞态：状态已被并发转移改写——按可见拒绝处理
			return LifecycleResult{}, fmt.Errorf("%w: %s → retiring", ErrInvalidTransition, cur.Status)
		}
	case StatusRetiring:
		// 已在 retiring：重过承诺门即可，不重复记事件
	case StatusRetired:
		return LifecycleResult{}, fmt.Errorf("%w: retired 为终态", ErrInvalidTransition)
	default:
		return LifecycleResult{}, fmt.Errorf("%w: 未知状态 %q", ErrInvalidTransition, cur.Status)
	}

	outstanding := []string{}
	if s.commitments != nil {
		items, err := s.commitments.Outstanding(ctx, agentID)
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("personalagent: 承诺门: %w", err)
		}
		if len(items) > 0 {
			outstanding = items
		}
	}
	if len(outstanding) == 0 {
		if _, done, err := s.store.TransitionAgent(ctx, agentID,
			[]string{StatusRetiring}, StatusRetired, reason, s.now()); err != nil {
			return LifecycleResult{}, fmt.Errorf("personalagent: 完成 retired: %w", err)
		} else if !done {
			return LifecycleResult{}, fmt.Errorf("%w: retiring → retired", ErrInvalidTransition)
		}
	}
	return s.lifecycleResult(ctx, agentID, outstanding)
}

// CreateManual 手动建身份（source=manual、active 出生、无别名——为 0.5.2+
// Slot 任职 / Worker 晋升预留主体）。
func (s *Service) CreateManual(ctx context.Context, displayName, adapter string) (*AgentView, error) {
	name := strings.TrimSpace(displayName)
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
		return nil, fmt.Errorf("%w: display_name 须 1–64 字", ErrInvalidAgent)
	}
	adapter = strings.TrimSpace(adapter)
	if utf8.RuneCountInString(adapter) > 32 {
		return nil, fmt.Errorf("%w: adapter 过长（≤32 字）", ErrInvalidAgent)
	}
	id := s.newAgentID()
	if _, err := s.store.EnsureAgent(ctx, Agent{
		AgentID: id, DisplayName: name, Adapter: adapter,
		Status: StatusActive, Source: "manual", CreatedAt: s.now(),
	}); err != nil {
		return nil, fmt.Errorf("personalagent: 手动建: %w", err)
	}
	view, ok, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok { // 不可能（刚建行）；防御
		return nil, ErrAgentNotFound
	}
	return view, nil
}

// Events 事件流读取（found=false = 身份不存在）。
func (s *Service) Events(ctx context.Context, agentID string) ([]AgentEvent, bool, error) {
	if _, ok, err := s.store.AgentByID(ctx, agentID); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, nil
	}
	evs, err := s.store.AgentEvents(ctx, agentID)
	if err != nil {
		return nil, false, fmt.Errorf("personalagent: events: %w", err)
	}
	if evs == nil {
		evs = []AgentEvent{} // JSON 呈现为 [] 而非 null
	}
	return evs, true, nil
}

// simpleTransition 单步转移（suspend/resume）。
func (s *Service) simpleTransition(ctx context.Context, agentID string, allowedFrom []string, to, reason string) (LifecycleResult, error) {
	_, done, err := s.store.TransitionAgent(ctx, agentID, allowedFrom, to, reason, s.now())
	if err != nil {
		return LifecycleResult{}, fmt.Errorf("personalagent: 转移: %w", err)
	}
	if !done {
		// 行缺失与状态不匹配在此合流：前者极罕见（无删除路径），统一按
		// invalid_transition 可见拒绝；调用方（HTTP）先经 404 判定。
		return LifecycleResult{}, ErrInvalidTransition
	}
	return s.lifecycleResult(ctx, agentID, nil)
}

// lifecycleResult 组装转移结果视图（Get 含惰性同步，幂等无副作用）。
func (s *Service) lifecycleResult(ctx context.Context, agentID string, outstanding []string) (LifecycleResult, error) {
	if outstanding == nil {
		outstanding = []string{}
	}
	view, ok, err := s.Get(ctx, agentID)
	if err != nil {
		return LifecycleResult{}, err
	}
	if !ok {
		return LifecycleResult{}, ErrAgentNotFound
	}
	return LifecycleResult{View: *view, Outstanding: outstanding}, nil
}
