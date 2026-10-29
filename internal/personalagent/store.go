package personalagent

import (
	"context"
	"errors"
)

// ErrAliasConflict 别名已指向另一 Agent——不合并、不覆盖，调用方记待处理
// （RFC-0018 §13.2.2：不按同名或同 adapter 自动合并）。
var ErrAliasConflict = errors.New("personalagent: 别名已指向另一 Agent")

// Store 身份域持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
type Store interface {
	// EnsureAgent 幂等建 Agent（按 agent_id）；返回是否新建。
	EnsureAgent(ctx context.Context, a Agent) (created bool, err error)
	// ListAgents 全量 Agent，按创建序（插入序）。
	ListAgents(ctx context.Context) ([]Agent, error)
	// AgentByAlias 别名解析；未登记 ok=false。
	AgentByAlias(ctx context.Context, kind AliasKind, value string) (agentID string, ok bool, err error)
	// AddAlias 幂等登记别名：同键同目标 no-op（added=false）；同键异目标
	// ErrAliasConflict。返回是否新增。
	AddAlias(ctx context.Context, kind AliasKind, value, agentID string) (added bool, err error)
	// AliasesOf 某 Agent 的全部别名行。
	AliasesOf(ctx context.Context, agentID string) ([]Alias, error)
}
