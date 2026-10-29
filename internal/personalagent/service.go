package personalagent

import (
	"context"
	"errors"
	"fmt"
)

// Registry 身份域视角的注册表端口（生产实现经 NewRegistryAdapter 适配
// harness.Registry；测试用 fake）。
type Registry interface {
	// Snapshot 注册表快照（确定性顺序由实现保证——生产为 adapter→priority→path）。
	Snapshot() []ExecutableSnapshot
	// ApplyAgentIDs 回写 exeID → agentID（发现表对身份的引用，RFC-0018 §13.2.3）；
	// 返回是否有变更与落盘错误。
	ApplyAgentIDs(ids map[string]string) (bool, error)
}

// Service 身份域服务：同步（注册表 → 身份表，只增不改）、查询、别名解析。
// 单向数据流：注册表是发现事实的来源，身份表是身份权威；身份表永不回写
// 注册表的身份字段（bot_id 等），杜绝双权威（RFC-0018 §13.2.3）。
type Service struct {
	store       Store
	reg         Registry
	newAgentID  func() string
	now         func() string
	commitments CommitmentChecker // 承诺门（ADR-0018；缺省 nil = 恒空集）
}

// Option 装配选项（测试注入确定性 ID/时钟；生产缺省随机 ID + 真实时钟）。
type Option func(*Service)

// WithNewAgentID 注入 AgentID 生成器（缺省：ag_ + 时间有序 + 随机后缀）。
func WithNewAgentID(f func() string) Option { return func(s *Service) { s.newAgentID = f } }

// WithNow 注入时钟（缺省：RFC3339Nano UTC）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// New 构造服务。
func New(store Store, reg Registry, opts ...Option) *Service {
	s := &Service{
		store:      store,
		reg:        reg,
		newAgentID: newAgentID,
		now:        nowUTC,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SyncReport 一次同步的效果账目。
type SyncReport struct {
	Created      int      // 新建 Agent 数
	AliasesAdded int      // 新登记别名数
	Conflicts    []string // 别名碰撞（指向另一 Agent）——不合并，待人工处置
	AppliedIDs   bool     // 可执行项 agent_id 回写是否落盘
}

// SyncFromHarness 幂等同步（ADR-0017 §3）：注册表快照 → 身份表。
// 等价键 = bot_id 别名（ADR-0013 重绑语义的延伸：BotID 相等即同一 Agent）。
// 只增不改：可执行项消失不删 Agent；别名碰撞记 Conflicts 不合并；
// 同名 / 同 adapter 从不构成合并依据（RFC-0018 §13.2.2）。
func (s *Service) SyncFromHarness(ctx context.Context) (SyncReport, error) {
	rep := SyncReport{}
	exes := s.reg.Snapshot()

	agentOf := map[string]string{} // botID → agentID（本次同步内缓存）
	mapping := map[string]string{} // exeID → agentID（回写注册表）
	// 去重按 (kind, value, 目标 Agent) 三元组判同（审校 P2#3）：同 Agent 的同值
	// 别名在多可执行项间只计一次；目标不同（跨身份 sanitize 碰撞）不得跳过——
	// 必须照常进入 AddAlias 的冲突检测并上报 Conflicts。
	seenAlias := map[AliasKind]map[string]string{} // value → 已处理的 agentID

	addAlias := func(kind AliasKind, value, agentID string) error {
		if seenAlias[kind] == nil {
			seenAlias[kind] = map[string]string{}
		}
		if prev, done := seenAlias[kind][value]; done && prev == agentID {
			return nil // 同 Agent 同值：本轮已处理（同 botID 多 exe 场景）
		}
		seenAlias[kind][value] = agentID
		added, err := s.store.AddAlias(ctx, kind, value, agentID)
		if err != nil {
			if errors.Is(err, ErrAliasConflict) {
				rep.Conflicts = append(rep.Conflicts,
					fmt.Sprintf("别名 %s %s 已指向另一 Agent——不合并，待人工处置", kind, value))
				return nil
			}
			return err
		}
		if added {
			rep.AliasesAdded++
		}
		return nil
	}

	for _, e := range exes {
		agentID, ok := agentOf[e.BotID]
		if !ok {
			existing, found, err := s.store.AgentByAlias(ctx, AliasBotID, e.BotID)
			if err != nil {
				return rep, fmt.Errorf("personalagent: bot 别名查询: %w", err)
			}
			if found {
				agentID = existing
			} else {
				// 建行与 bot_id 认领同一事务（审校 P1#1）：并发竞争失败者回读胜者，
				// 中断不留半行；全程只用成功认领的 AgentID 派生别名与回写注册表。
				candidate := s.newAgentID()
				owner, created, err := s.store.EnsureAgentWithAlias(ctx, Agent{
					AgentID:     candidate,
					DisplayName: DisplayNameOf(e.Adapter),
					Adapter:     e.Adapter,
					Status:      "active",
					Source:      "migration",
					CreatedAt:   s.now(),
				}, AliasBotID, e.BotID)
				if err != nil {
					return rep, fmt.Errorf("personalagent: 建 Agent: %w", err)
				}
				agentID = owner
				if created {
					rep.Created++
					rep.AliasesAdded++ // bot_id 认领随建行原子发生
				}
			}
			agentOf[e.BotID] = agentID
		}
		mapping[e.ExeID] = agentID
		// 派生别名按 (adapter, botID) 定值——同 botID 跨 adapter 的病态场景下
		// 两组派生别名都挂同一 Agent（bot_id 等价键裁决），不产生歧义
		if err := addAlias(AliasProfileID, ProfileIDOf(e.Adapter, e.BotID), agentID); err != nil {
			return rep, err
		}
		if err := addAlias(AliasParticipantID, ParticipantIDOf(e.Adapter, e.BotID), agentID); err != nil {
			return rep, err
		}
	}

	changed, err := s.reg.ApplyAgentIDs(mapping)
	if err != nil {
		return rep, fmt.Errorf("personalagent: 回写 agent_id: %w", err)
	}
	rep.AppliedIDs = changed
	return rep, nil
}

// List 全量 AgentView（创建序）。读前执行幂等同步——手动登记/删除可执行项
// 后无需挂钩子即最终一致；同步失败时降级用存量身份（身份只增，存量即最近
// 的权威快照），读面不因同步瞬时故障不可用。
func (s *Service) List(ctx context.Context) ([]AgentView, error) {
	_, _ = s.SyncFromHarness(ctx) // 降级语义见上：读面可用性优先
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("personalagent: list: %w", err)
	}
	refsByAgent := map[string][]ExecutableRef{}
	for _, e := range s.reg.Snapshot() {
		agentID, ok, err := s.store.AgentByAlias(ctx, AliasBotID, e.BotID)
		if err != nil || !ok {
			continue // 身份缺失（同步降级窗口）——引用留空，不阻塞读面
		}
		channel := e.Channel
		if channel == "" {
			channel = "cli" // 注册表口径：空值按 cli 呈现（与座位面一致）
		}
		refsByAgent[agentID] = append(refsByAgent[agentID], ExecutableRef{
			ExecutableID: e.ExeID,
			Runtime:      e.Runtime,
			Distro:       e.Distro,
			Channel:      channel,
			Enabled:      e.Enabled,
		})
	}
	out := make([]AgentView, 0, len(agents))
	for _, a := range agents {
		aliases, err := s.store.AliasesOf(ctx, a.AgentID)
		if err != nil {
			return nil, fmt.Errorf("personalagent: aliases: %w", err)
		}
		set := AliasSet{ProfileIDs: []string{}, ParticipantIDs: []string{}}
		for _, al := range aliases {
			switch al.Kind {
			case AliasBotID:
				set.BotID = al.Value
			case AliasProfileID:
				set.ProfileIDs = append(set.ProfileIDs, al.Value)
			case AliasParticipantID:
				set.ParticipantIDs = append(set.ParticipantIDs, al.Value)
			}
		}
		refs := refsByAgent[a.AgentID]
		if refs == nil {
			refs = []ExecutableRef{} // JSON 呈现为 [] 而非 null
		}
		out = append(out, AgentView{Agent: a, Aliases: set, Executables: refs})
	}
	return out, nil
}

// Get 单查 AgentView。
func (s *Service) Get(ctx context.Context, agentID string) (*AgentView, bool, error) {
	views, err := s.List(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range views {
		if views[i].AgentID == agentID {
			return &views[i], true, nil
		}
	}
	return nil, false, nil
}

// ResolveAlias 旧 ID → AgentID 解析（房间事件 participantID / 会话 profileID /
// 注册表 botID 三处 ID 的统一入口；未登记 ok=false）。
func (s *Service) ResolveAlias(ctx context.Context, kind AliasKind, value string) (string, bool, error) {
	return s.store.AgentByAlias(ctx, kind, value)
}

// AgentByID 单查 Agent 行（外部域任职校验等经端口消费；0.5.4）。
func (s *Service) AgentByID(ctx context.Context, agentID string) (Agent, bool, error) {
	return s.store.AgentByID(ctx, agentID)
}
