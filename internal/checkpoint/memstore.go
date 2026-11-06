// Package checkpoint 内存件（UT / 内存装配）：版本原子分配与降序语义
// 与 sqlite 实现同源。
package checkpoint

import (
	"context"
	"sort"
	"sync"
)

// MemStore 内存存储。
type MemStore struct {
	mu    sync.Mutex
	byRun map[string][]Checkpoint // 升序追加（版本递增）
}

// NewMemStore 构造。
func NewMemStore() *MemStore { return &MemStore{byRun: map[string][]Checkpoint{}} }

// CreateCheckpoint 版本原子分配（锁内 MAX+1——与 sqlite 事务内同语义）。
func (m *MemStore) CreateCheckpoint(_ context.Context, cp Checkpoint) (Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.byRun[cp.RunID]
	cp.CheckpointVersion = len(list) + 1
	m.byRun[cp.RunID] = append(list, cp)
	return cp, nil
}

// CheckpointsOfRun 降序（最新在前）。
func (m *MemStore) CheckpointsOfRun(_ context.Context, runID string) ([]Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := append([]Checkpoint{}, m.byRun[runID]...)
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].CheckpointVersion > list[j].CheckpointVersion
	})
	return list, nil
}
