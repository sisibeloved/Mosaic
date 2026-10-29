package personalagent

import (
	"context"
	"sync"
)

// MemStore 身份域内存实现（UT/端点测试件；语义与 SQLite 实现一致：插入序、
// 别名唯一、冲突语义、原子认领）。并发同步测试件——整体互斥。
type MemStore struct {
	mu      sync.Mutex
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
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.agents {
		if ex.AgentID == a.AgentID {
			return false, nil
		}
	}
	m.agents = append(m.agents, a)
	return true, nil
}

func (m *MemStore) ListAgents(_ context.Context) ([]Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Agent(nil), m.agents...), nil
}

func (m *MemStore) AgentByAlias(_ context.Context, kind AliasKind, value string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byVal, ok := m.aliasTo[kind]
	if !ok {
		return "", false, nil
	}
	id, ok := byVal[value]
	return id, ok, nil
}

func (m *MemStore) AddAlias(_ context.Context, kind AliasKind, value, agentID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Alias(nil), m.ofAgent[agentID]...), nil
}

// EnsureAgentWithAlias 原子 get-or-create（端口语义见 store.go；整体临界区实现）。
func (m *MemStore) EnsureAgentWithAlias(_ context.Context, a Agent, kind AliasKind, value string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if byVal, ok := m.aliasTo[kind]; ok {
		if owner, exists := byVal[value]; exists {
			return owner, false, nil
		}
	}
	for _, ex := range m.agents { // 候选已存在（同 agent_id 重放）：只补认领
		if ex.AgentID == a.AgentID {
			m.claimAliasLocked(kind, value, a.AgentID)
			return a.AgentID, false, nil
		}
	}
	m.agents = append(m.agents, a)
	m.claimAliasLocked(kind, value, a.AgentID)
	return a.AgentID, true, nil
}

func (m *MemStore) claimAliasLocked(kind AliasKind, value, agentID string) {
	byVal, ok := m.aliasTo[kind]
	if !ok {
		byVal = map[string]string{}
		m.aliasTo[kind] = byVal
	}
	if _, exists := byVal[value]; exists {
		return
	}
	byVal[value] = agentID
	m.ofAgent[agentID] = append(m.ofAgent[agentID], Alias{Kind: kind, Value: value})
}
