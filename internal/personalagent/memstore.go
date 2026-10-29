package personalagent

import "context"

// MemStore 身份域内存实现（UT/端点测试件；语义与 SQLite 实现一致：插入序、
// 别名唯一、冲突语义）。
type MemStore struct {
	agents  []Agent
	aliasTo map[AliasKind]map[string]string // kind → value → agentID
	ofAgent map[string][]Alias
}

func NewMemStore() *MemStore {
	return &MemStore{
		aliasTo: map[AliasKind]map[string]string{},
		ofAgent: map[string][]Alias{},
	}
}

func (m *MemStore) EnsureAgent(_ context.Context, a Agent) (bool, error) {
	for _, ex := range m.agents {
		if ex.AgentID == a.AgentID {
			return false, nil
		}
	}
	m.agents = append(m.agents, a)
	return true, nil
}

func (m *MemStore) ListAgents(_ context.Context) ([]Agent, error) {
	return append([]Agent(nil), m.agents...), nil
}

func (m *MemStore) AgentByAlias(_ context.Context, kind AliasKind, value string) (string, bool, error) {
	byVal, ok := m.aliasTo[kind]
	if !ok {
		return "", false, nil
	}
	id, ok := byVal[value]
	return id, ok, nil
}

func (m *MemStore) AddAlias(_ context.Context, kind AliasKind, value, agentID string) (bool, error) {
	byVal, ok := m.aliasTo[kind]
	if !ok {
		byVal = map[string]string{}
		m.aliasTo[kind] = byVal
	}
	if prev, exists := byVal[value]; exists {
		if prev != agentID {
			return false, ErrAliasConflict
		}
		return false, nil
	}
	byVal[value] = agentID
	m.ofAgent[agentID] = append(m.ofAgent[agentID], Alias{Kind: kind, Value: value})
	return true, nil
}

func (m *MemStore) AliasesOf(_ context.Context, agentID string) ([]Alias, error) {
	return append([]Alias(nil), m.ofAgent[agentID]...), nil
}
