// memstore：run.Store 内存测试件。
package run

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MemStore 内存实现。
type MemStore struct {
	mu      sync.Mutex
	nextSeq int64
	runs    map[string]Run
	events  []Event
	// missionStatus 事务内任务状态钩子（复审 R1/R7：CreateRun 核对 ready、
	// ResolveRun 核对终态——SQLite 同款语义的内存件；nil = 放行）。
	missionStatus func(missionID string) (string, bool)
}

// NewMemStore 建空库。
func NewMemStore() *MemStore { return &MemStore{runs: map[string]Run{}} }

// WithMissionStatus 注入任务状态源（测试装配）。
func (m *MemStore) WithMissionStatus(f func(missionID string) (string, bool)) *MemStore {
	m.missionStatus = f
	return m
}

// CreateRun 建行（每 Mission 至多一个未终态）+ attempt_requested 事件。
func (m *MemStore) CreateRun(_ context.Context, r Run) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.runs {
		if e.MissionID == r.MissionID && (e.State == StatePending || e.State == StateRunning) {
			return Run{}, ErrRunExists
		}
	}
	// 复审 R1：任务须仍是 ready（过期调度拒绝；状态源未装配 = 放行——简单件）
	if m.missionStatus != nil {
		if st, ok := m.missionStatus(r.MissionID); ok && st != "ready" && st != "running" {
			return Run{}, fmt.Errorf("%w（当前 %s）——过期调度拒绝", ErrMissionNotReady, st)
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

// ListLegacyResidues 旧版（0.6.5/0.6.6）崩溃残留终态行（failed + 前缀）。
func (m *MemStore) ListLegacyResidues(_ context.Context, failedPrefix string) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Run{}
	for _, r := range m.runs {
		if r.State == StateFailed && strings.HasPrefix(r.ErrorText, failedPrefix) {
			out = append(out, r)
		}
	}
	return out, nil
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

// ResolveRun 条件处置（0.6.7）：state=unknown 且未处置才落三列 + resolved 事件。
func (m *MemStore) ResolveRun(_ context.Context, runID, resolution, note, resolvedAt string) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return Run{}, false, ErrRunNotFound
	}
	// 复审 R7：写事务内终态核对（状态源未装配 = 放行——简单件）
	if m.missionStatus != nil {
		if st, ok := m.missionStatus(r.MissionID); ok && (st == "accepted" || st == "canceled") {
			return Run{}, false, fmt.Errorf("%w（Mission %s）", ErrMissionTerminal, st)
		}
	}
	if r.State != StateUnknown || r.Resolution != "" {
		return Run{}, false, nil
	}
	r.Resolution = resolution
	r.ResolutionNote = note
	r.ResolvedAt = resolvedAt
	r.UpdatedAt = resolvedAt
	m.runs[runID] = r
	m.appendLocked(Event{RunID: runID, EventType: EventResolved, OccurredAt: resolvedAt,
		Payload: map[string]any{"outcome": resolution, "note": note}})
	return r, true, nil
}

// AppendRunEvents 事件追加（不重写行）。
func (m *MemStore) AppendRunEvents(_ context.Context, runID string, events []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[runID]; !ok {
		return ErrRunNotFound
	}
	for _, e := range events {
		m.appendLocked(e)
	}
	return nil
}

// ListUnresolvedUnknown 未处置 unknown。
func (m *MemStore) ListUnresolvedUnknown(_ context.Context) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Run
	for _, r := range m.runs {
		if r.State == StateUnknown && r.Resolution == "" {
			out = append(out, r)
		}
	}
	return out, nil
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
