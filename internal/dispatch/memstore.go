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

func (m *MemStore) ClaimKey(_ context.Context, r Receipt) (Receipt, bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.CallerRef + "|" + r.Scope + "|" + r.IdempotencyKey
	if ex, taken := m.receipts[key]; taken {
		if ex.RequestDigest != r.RequestDigest {
			return ex, true, false, nil // 异摘要：写入前退出
		}
		return ex, false, ex.DomainCommandID != "", nil // 同摘要：完结即重放；pending 重驱动
	}
	m.receipts[key] = r
	return Receipt{}, false, false, nil
}

func (m *MemStore) FinalizeReceipt(_ context.Context, r Receipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receipts[r.CallerRef+"|"+r.Scope+"|"+r.IdempotencyKey] = r
	return nil
}

// ReleaseKey 回滚未生效命令对键的认领（0.6.13 审校 #11：领域拒绝释放 pending
// 同摘要行——修正后可重试；只删 pending，不碰已完结回执）。
func (m *MemStore) ReleaseKey(_ context.Context, callerRef, scope, key, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := callerRef + "|" + scope + "|" + key
	if ex, taken := m.receipts[k]; taken && ex.DomainCommandID == "" && ex.RequestDigest == digest {
		delete(m.receipts, k)
	}
	return nil
}

// ClaimByKey 认领行查询（含 pending——三审 #6 处置入口只读面）。
func (m *MemStore) ClaimByKey(_ context.Context, callerRef, scope, key string) (Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.receipts[callerRef+"|"+scope+"|"+key]
	return r, ok, nil
}

// ReleaseClaim 遗留认领处置（四审 #4/#8：临界区内判定+删除——仅 update 类
// （route 空）pending 同摘要可释放）。
func (m *MemStore) ReleaseClaim(_ context.Context, callerRef, scope, key, digest string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := callerRef + "|" + scope + "|" + key
	ex, taken := m.receipts[k]
	if !taken {
		return "not_found", nil
	}
	if ex.DomainCommandID != "" {
		return "finalized", nil
	}
	if ex.RequestDigest != digest {
		return "digest_mismatch", nil
	}
	if ex.RouteID != "" {
		return "not_releasable", nil
	}
	delete(m.receipts, k)
	return "released", nil
}

// ReceiptByKey 幂等键查**已提交**回执（0.6.13 审校 #11：pending 认领是事务
// 内部事实，公开查询不可见）；未完结 ok=false。
func (m *MemStore) ReceiptByKey(_ context.Context, callerRef, scope, key string) (Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.receipts[callerRef+"|"+scope+"|"+key]
	if ok && r.DomainCommandID == "" {
		return Receipt{}, false, nil
	}
	return r, ok, nil
}
