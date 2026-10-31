// memstore 租约面（0.6.4 / ADR-0027）——与 SQLite 同语义：互斥判定在
// 锁内、代次 = 当代 MAX+1、惰性到期经时间解析比较（字符串比较不可靠）。
package runtime

import (
	"context"
	"sort"
	"time"
)

func (m *MemStore) AcquireLease(_ context.Context, lease Lease) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 工作区独占按规范身份（workspace_key）计——同一物理目录的路径别名
	// 不得绕过（审校 P2#2）；空 key 为历史别名行，不参与新排他。
	acqEnv, acqOK := m.envs[lease.EnvironmentID]
	acqKey := ""
	if acqOK {
		acqKey = acqEnv.WorkspaceKey
	}
	for id, l := range m.leases {
		if l.State != LeaseStateActive {
			continue
		}
		if l.RuntimeID == lease.RuntimeID {
			return Lease{}, ErrLeaseConflictRuntime
		}
		if acqKey != "" {
			if env, ok := m.envs[l.EnvironmentID]; ok && env.WorkspaceKey == acqKey {
				return Lease{}, ErrLeaseConflictWorkspace
			}
		}
		_ = id
	}
	lease.Generation = m.maxLeaseGenerationLocked(lease.RuntimeID) + 1
	m.leases[lease.LeaseID] = lease
	return lease, nil
}

func (m *MemStore) UpdateLease(_ context.Context, lease Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.leases[lease.LeaseID]; !ok {
		return ErrLeaseNotFound
	}
	m.leases[lease.LeaseID] = lease
	return nil
}

// RenewLease 原子续租（审校 P1）：锁内核对 state=active，只更新续租字段
// ——并发到期先落则 ErrLeaseInactive，不复活 active、不改原令牌。
func (m *MemStore) RenewLease(_ context.Context, leaseID, newExpiresAt, now string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[leaseID]
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	if l.State != LeaseStateActive {
		return Lease{}, ErrLeaseInactive
	}
	l.ExpiresAt = newExpiresAt
	l.UpdatedAt = now
	m.leases[leaseID] = l
	return l, nil
}

// ReleaseLease 原子释放（同型防护）：只更新终态字段。
func (m *MemStore) ReleaseLease(_ context.Context, leaseID, now string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[leaseID]
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	if l.State != LeaseStateActive {
		return Lease{}, ErrLeaseInactive
	}
	l.State = LeaseStateReleased
	l.ReleasedAt = now
	l.UpdatedAt = now
	m.leases[leaseID] = l
	return l, nil
}

func (m *MemStore) LeaseByID(_ context.Context, leaseID string) (Lease, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[leaseID]
	return l, ok, nil
}

func (m *MemStore) ListLeases(_ context.Context, state, runtimeID string) ([]Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Lease, 0, len(m.leases))
	for _, l := range m.leases {
		if state != LeaseStateAll && state != "" && l.State != state {
			continue
		}
		if runtimeID != "" && l.RuntimeID != runtimeID {
			continue
		}
		out = append(out, l)
	}
	// 领取序倒序（acquired_at 降序，同刻按 lease_id 降序兜底——确定性）
	sort.Slice(out, func(i, j int) bool {
		if out[i].AcquiredAt != out[j].AcquiredAt {
			return out[i].AcquiredAt > out[j].AcquiredAt
		}
		return out[i].LeaseID > out[j].LeaseID
	})
	return out, nil
}

func (m *MemStore) MaxLeaseGeneration(_ context.Context, runtimeID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxLeaseGenerationLocked(runtimeID), nil
}

func (m *MemStore) maxLeaseGenerationLocked(runtimeID string) int {
	max := 0
	for _, l := range m.leases {
		if l.RuntimeID == runtimeID && l.Generation > max {
			max = l.Generation
		}
	}
	return max
}

func (m *MemStore) ExpireOverdue(_ context.Context, now string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nowT, err := time.Parse(time.RFC3339Nano, now)
	if err != nil {
		return nil
	}
	for id, l := range m.leases {
		if l.State != LeaseStateActive {
			continue
		}
		exp, err := time.Parse(time.RFC3339Nano, l.ExpiresAt)
		if err != nil || exp.After(nowT) {
			continue
		}
		l.State = LeaseStateExpired
		l.UpdatedAt = now
		m.leases[id] = l
	}
	return nil
}
