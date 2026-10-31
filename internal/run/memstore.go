// memstore：run.Store 内存测试件。
package run

import (
	"context"
	"sort"
	"sync"
)

// MemStore 内存实现。
type MemStore struct {
	mu      sync.Mutex
	nextSeq int64
	runs    map[string]Run
	events  []Event
}

// NewMemStore 建空库。
func NewMemStore() *MemStore { return &MemStore{runs: map[string]Run{}} }

// CreateRun 建行（每 Mission 至多一个未终态）+ attempt_requested 事件。
func (m *MemStore) CreateRun(_ context.Context, r Run) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.runs {
		if e.MissionID == r.MissionID && (e.State == StatePending || e.State == StateRunning) {
			return Run{}, ErrRunExists
		}
	}
	r.Attempt = m.maxAttemptLocked(r.MissionID) + 1
	m.runs[r.RunID] = r
	m.appendLocked(Event{RunID: r.RunID, EventType: EventAttemptRequested,
		Payload:    map[string]any{"attempt": r.Attempt, "lease_id": r.LeaseID, "manifest_id": r.ManifestID},
		OccurredAt: r.RequestedAt})
	return r, nil
}

// UpdateRun 全行替换 + 事件。
func (m *MemStore) UpdateRun(_ context.Context, r Run, events []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.RunID]; !ok {
		return ErrRunNotFound
	}
	m.runs[r.RunID] = r
	for _, e := range events {
		m.appendLocked(e)
	}
	return nil
}

// RunByID 单查。
func (m *MemStore) RunByID(_ context.Context, runID string) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	return r, ok, nil
}

// ListRunsByMission attempt 降序（新在前）。
func (m *MemStore) ListRunsByMission(_ context.Context, missionID string) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Run
	for _, r := range m.runs {
		if r.MissionID == missionID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt > out[j].Attempt })
	return out, nil
}

// ListUnfinished 未终态行。
func (m *MemStore) ListUnfinished(_ context.Context) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Run
	for _, r := range m.runs {
		if r.State == StatePending || r.State == StateRunning {
			out = append(out, r)
		}
	}
	return out, nil
}

// RunEvents 事件流（seq 升序）。
func (m *MemStore) RunEvents(_ context.Context, runID string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events {
		if e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// NextAttempt 下一 attempt。
func (m *MemStore) NextAttempt(_ context.Context, missionID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxAttemptLocked(missionID) + 1, nil
}

func (m *MemStore) maxAttemptLocked(missionID string) int {
	max := 0
	for _, r := range m.runs {
		if r.MissionID == missionID && r.Attempt > max {
			max = r.Attempt
		}
	}
	return max
}

func (m *MemStore) appendLocked(e Event) {
	m.nextSeq++
	e.Seq = m.nextSeq
	m.events = append(m.events, e)
}
