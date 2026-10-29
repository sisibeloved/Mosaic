package personalagent

import (
	"context"
	"errors"
)

// ErrAliasConflict 别名已指向另一 Agent——不合并、不覆盖，调用方记待处理
// （RFC-0018 §13.2.2：不按同名或同 adapter 自动合并）。
var ErrAliasConflict = errors.New("personalagent: 别名已指向另一 Agent")

// ErrAgentNotFound Agent 行不存在（转移/事件读取前置）。
var ErrAgentNotFound = errors.New("personalagent: Agent 不存在")

// ErrInvalidTransition 生命周期转移非法（当前状态不在动作允许的来源集，
// 含 retired 终态全拒）——不静默幂等，重放方须重取终态（ADR-0018 §1）。
var ErrInvalidTransition = errors.New("personalagent: 非法生命周期转移")

// ErrInvalidAgent 手动建身份参数不合法（空名/超长）。
var ErrInvalidAgent = errors.New("personalagent: Agent 参数不合法")

// Store 身份域持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
type Store interface {
	// EnsureAgent 幂等建 Agent（按 agent_id）；新建时 created 事件同事务落库。
	EnsureAgent(ctx context.Context, a Agent) (created bool, err error)
	// EnsureAgentWithAlias 原子 get-or-create（审校 P1#1）：建 Agent 行与认领
	// (kind, value) 别名在同一事务——别名已被占（含并发竞争）时回读既有归属者
	// 并丢弃候选行（created=false，ownerID=胜者）；成功则两行与 created 事件
	// 同落（created=true，ownerID=候选）。保证任一时点要么"整行落库"要么
	// "什么都不留"。
	EnsureAgentWithAlias(ctx context.Context, a Agent, kind AliasKind, value string) (ownerID string, created bool, err error)
	// ListAgents 全量 Agent，按创建序（插入序）。
	ListAgents(ctx context.Context) ([]Agent, error)
	// AgentByID 单查 Agent 行。
	AgentByID(ctx context.Context, agentID string) (Agent, bool, error)
	// AgentByAlias 别名解析；未登记 ok=false。
	AgentByAlias(ctx context.Context, kind AliasKind, value string) (agentID string, ok bool, err error)
	// AddAlias 幂等登记别名：同键同目标 no-op（added=false）；同键异目标
	// ErrAliasConflict。返回是否新增。
	AddAlias(ctx context.Context, kind AliasKind, value, agentID string) (added bool, err error)
	// AliasesOf 某 Agent 的全部别名行。
	AliasesOf(ctx context.Context, agentID string) ([]Alias, error)
	// TransitionAgent 事务转移（ADR-0018 §2）：当前状态在 allowedFrom 内才
	// 更新 agents 投影并追加 status_changed 事件（from=实际来源）；ok=false
	// = 状态不匹配（未动投影未落事件）；行缺失 ErrAgentNotFound。
	TransitionAgent(ctx context.Context, agentID string, allowedFrom []string, to, reason, occurredAt string) (from string, ok bool, err error)
	// AgentEvents 某 Agent 的事件流（seq 升序）。
	AgentEvents(ctx context.Context, agentID string) ([]AgentEvent, error)
}
