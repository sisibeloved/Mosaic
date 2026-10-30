package dispatch

// MemStore 网关内存实现（UT/端点测试件；语义与 SQLite 一致：决策/回执存取、
// (caller, scope, key) 唯一约束仿真）。

import (
	"context"
	"sync"
)

type MemStore struct {
	mu        sync.Mutex
	decisions map[string]Decision
	receipts  map[string]Receipt // key: caller|scope|idempotencyKey
}

func NewMemStore() *MemStore {
	return &MemStore{decisions: map[string]Decision{}, receipts: map[string]Receipt{}}
}

func (m *MemStore) SaveDecision(_ context.Context, d Decision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions[d.RouteID] = d
	return nil
}

func (m *MemStore) DecisionByID(_ context.Context, routeID string) (Decision, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.decisions[routeID]
	return d, ok, nil
}

func (m *MemStore) SaveReceipt(_ context.Context, r Receipt) (Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.CallerRef + "|" + r.Scope + "|" + r.IdempotencyKey
	if ex, taken := m.receipts[key]; taken {
		return ex, true, nil
	}
	m.receipts[key] = r
	return Receipt{}, false, nil
}

func (m *MemStore) ReceiptByKey(_ context.Context, callerRef, scope, key string) (Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.receipts[callerRef+"|"+scope+"|"+key]
	return r, ok, nil
}
