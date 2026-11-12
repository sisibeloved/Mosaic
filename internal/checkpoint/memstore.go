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
	seq   []Checkpoint            // 插入序（LatestOfMission 跨 Run 取最新）
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
	m.seq = append(m.seq, cp)
	return cp, nil
}

// LatestSystemOfMission 最新系统终态摘要（插入序倒查 source=system）。
func (m *MemStore) LatestSystemOfMission(_ context.Context, missionID string) (Checkpoint, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.seq) - 1; i >= 0; i-- {
		if m.seq[i].MissionID == missionID && m.seq[i].Source == "system" {
			return m.seq[i], true, nil
		}
	}
	return Checkpoint{}, false, nil
}

// LatestOfMission 任务级最新（插入序末位——与 sqlite rowid DESC 同语义）。
// CheckpointsOfMission 任务级全量（任意 source，降序——复审 #4）。
func (m *MemStore) CheckpointsOfMission(_ context.Context, missionID string) ([]Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Checkpoint
	for i := len(m.seq) - 1; i >= 0; i-- {
		if m.seq[i].MissionID == missionID {
			out = append(out, m.seq[i])
		}
	}
	return out, nil
}

func (m *MemStore) LatestOfMission(_ context.Context, missionID string) (Checkpoint, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.seq) - 1; i >= 0; i-- {
		if m.seq[i].MissionID == missionID && m.seq[i].Source != "system" {
			return m.seq[i], true, nil
		}
	}
	return Checkpoint{}, false, nil
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
