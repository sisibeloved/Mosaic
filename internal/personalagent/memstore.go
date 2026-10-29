package personalagent

import (
	"context"
	"encoding/json"
	"sync"
)

// MemStore 身份域内存实现（UT/端点测试件；语义与 SQLite 实现一致：插入序、
// 别名唯一、冲突语义、原子认领、建行即落 created 事件、转移 = 投影 + 事件）。
// 并发同步测试件——整体互斥。
type MemStore struct {
	mu      sync.Mutex
	agents  []Agent
	aliasTo map[AliasKind]map[string]string // kind → value → agentID
	ofAgent map[string][]Alias
	events  []AgentEvent
	seq     int64
}

func NewMemStore() *MemStore {
	return &MemStore{
		aliasTo: map[AliasKind]map[string]string{},
		ofAgent: map[string][]Alias{},
	}
}

// appendCreatedLocked 建行事实落流（调用方持锁）。
func (m *MemStore) appendCreatedLocked(a Agent) {
	payload, _ := json.Marshal(createdPayload{Source: a.Source, Adapter: a.Adapter, DisplayName: a.DisplayName})
	m.seq++
	m.events = append(m.events, AgentEvent{
		Seq: m.seq, AgentID: a.AgentID, EventType: EventCreated,
		Payload: payload, OccurredAt: a.CreatedAt,
	})
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
	m.appendCreatedLocked(a)
	return true, nil
}

func (m *MemStore) ListAgents(_ context.Context) ([]Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Agent(nil), m.agents...), nil
}

func (m *MemStore) AgentByID(_ context.Context, agentID string) (Agent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.agents {
		if ex.AgentID == agentID {
			return ex, true, nil
		}
	}
	return Agent{}, false, nil
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
	m.appendCreatedLocked(a)
	m.claimAliasLocked(kind, value, a.AgentID)
	return a.AgentID, true, nil
}

// TransitionAgent 转移（端口语义见 store.go；整体临界区实现）。
func (m *MemStore) TransitionAgent(_ context.Context, agentID string, allowedFrom []string, to, reason, occurredAt string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.indexOfLocked(agentID)
	if idx < 0 {
		return "", false, ErrAgentNotFound
	}
	from := m.agents[idx].Status
	if !allowedIn(allowedFrom, from) {
		return "", false, nil
	}
	m.agents[idx].Status = to
	m.appendChangedLocked(agentID, from, to, reason, occurredAt)
	return from, true, nil
}

// RetireAgent 原子退役（端口语义见 store.go；整体临界区——两段事件一次落）。
func (m *MemStore) RetireAgent(_ context.Context, agentID string, allowedFrom []string, reason, occurredAt string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.indexOfLocked(agentID)
	if idx < 0 {
		return "", false, ErrAgentNotFound
	}
	from := m.agents[idx].Status
	if !allowedIn(allowedFrom, from) {
		return "", false, nil
	}
	m.appendChangedLocked(agentID, from, StatusRetiring, reason, occurredAt)
	m.appendChangedLocked(agentID, StatusRetiring, StatusRetired, reason, occurredAt)
	m.agents[idx].Status = StatusRetired
	return from, true, nil
}

func (m *MemStore) indexOfLocked(agentID string) int {
	for i, ex := range m.agents {
		if ex.AgentID == agentID {
			return i
		}
	}
	return -1
}

func allowedIn(allowedFrom []string, status string) bool {
	for _, f := range allowedFrom {
		if f == status {
			return true
		}
	}
	return false
}

// appendChangedLocked status_changed 事件落流（调用方持锁）。
func (m *MemStore) appendChangedLocked(agentID, from, to, reason, occurredAt string) {
	payload, _ := json.Marshal(statusChangedPayload{From: from, To: to, Reason: reason})
	m.seq++
	m.events = append(m.events, AgentEvent{
		Seq: m.seq, AgentID: agentID, EventType: EventStatusChanged,
		Payload: payload, OccurredAt: occurredAt,
	})
}

// AgentEvents 某 Agent 的事件流（seq 升序——全表 append 天然有序）。
func (m *MemStore) AgentEvents(_ context.Context, agentID string) ([]AgentEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AgentEvent
	for _, ev := range m.events {
		if ev.AgentID == agentID {
			out = append(out, ev)
		}
	}
	return out, nil
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
