package mission

// MemStore 契约内存实现（UT/端点测试件；语义与 SQLite 一致：版本行不可变、
// CAS 串行化、创建序摘要、事件 append-only）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
)

type MemStore struct {
	mu       sync.Mutex
	missions []Mission            // 创建序投影
	versions map[string][]Mission // 升序版本行
	events   map[string][]Event
	sources  map[[2]string]string // (room_id, task_id) → mission_id（ADR-0024 唯一映射）
	bySource map[string][2]string // mission_id → (room_id, task_id)
	seq      int64
}

func NewMemStore() *MemStore {
	return &MemStore{
		versions: map[string][]Mission{}, events: map[string][]Event{},
		sources: map[[2]string]string{}, bySource: map[string][2]string{},
	}
}

func (m *MemStore) CreateMission(_ context.Context, ms Mission, src *RoomSource) (Mission, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if src != nil {
		key := [2]string{src.RoomID, src.TaskID}
		if ex, taken := m.sources[key]; taken {
			return Mission{}, fmt.Errorf("%w: (%s, %s) 已映射 %s", ErrSourceAlreadyMapped, src.RoomID, src.TaskID, ex)
		}
		m.sources[key] = ms.MissionID
		m.bySource[ms.MissionID] = key
	}
	m.missions = append(m.missions, ms)
	m.versions[ms.MissionID] = append(m.versions[ms.MissionID], snapshot(ms))
	m.appendEventLocked(ms.MissionID, EventCreated, map[string]string{
		"contract_version": strconv.Itoa(ms.ContractVersion), "status": ms.Status,
	}, ms.CreatedAt)
	return ms, nil
}

// MissionIDBySource 来源映射查询；未映射 ok=false。
func (m *MemStore) MissionIDBySource(_ context.Context, roomID, taskID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.sources[[2]string{roomID, taskID}]
	return id, ok, nil
}

func (m *MemStore) ReviseMission(_ context.Context, missionID string, expectedVersion int, next Mission) (Mission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.indexOfLocked(missionID)
	if !ok {
		return Mission{}, false, ErrMissionNotFound
	}
	cur := m.missions[idx]
	if cur.ContractVersion != expectedVersion || cur.Status == StatusCanceled {
		return Mission{}, false, nil
	}
	prevStatus := cur.Status
	m.missions[idx] = next
	m.versions[missionID] = append(m.versions[missionID], snapshot(next))
	m.appendEventLocked(missionID, EventContractRevised, map[string]string{
		"from_version": strconv.Itoa(cur.ContractVersion), "to_version": strconv.Itoa(next.ContractVersion),
	}, next.UpdatedAt)
	if prevStatus != next.Status {
		m.appendEventLocked(missionID, EventStatusChanged, map[string]string{
			"from": prevStatus, "to": next.Status,
		}, next.UpdatedAt)
	}
	return next, true, nil
}

// PauseMission CAS 暂停（存储面只认 ready/running——幂等先读在服务层）。
func (m *MemStore) PauseMission(_ context.Context, missionID string, expectedVersion int, reason, occurredAt string) (Mission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.indexOfLocked(missionID)
	if !ok {
		return Mission{}, false, ErrMissionNotFound
	}
	cur := m.missions[idx]
	if cur.ContractVersion != expectedVersion || (cur.Status != StatusReady && cur.Status != StatusRunning) {
		return Mission{}, false, nil
	}
	return m.flipStatusLocked(idx, cur.Status, StatusPaused, reason, occurredAt), true, nil
}

// ResumeMission CAS 恢复（paused → ready）。
func (m *MemStore) ResumeMission(_ context.Context, missionID string, expectedVersion int, occurredAt string) (Mission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.indexOfLocked(missionID)
	if !ok {
		return Mission{}, false, ErrMissionNotFound
	}
	cur := m.missions[idx]
	if cur.ContractVersion != expectedVersion || cur.Status != StatusPaused {
		return Mission{}, false, nil
	}
	return m.flipStatusLocked(idx, cur.Status, StatusReady, "", occurredAt), true, nil
}

// MarkCanceling CAS 停止意图（非终态 → canceling）。
func (m *MemStore) MarkCanceling(_ context.Context, missionID string, expectedVersion int, reason, occurredAt string) (Mission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.indexOfLocked(missionID)
	if !ok {
		return Mission{}, false, ErrMissionNotFound
	}
	cur := m.missions[idx]
	if cur.ContractVersion != expectedVersion || cur.Status == StatusCanceled {
		return Mission{}, false, nil
	}
	return m.flipStatusLocked(idx, cur.Status, StatusCanceling, reason, occurredAt), true, nil
}

// FinishCancel 收尾（canceling → canceled，无版本守卫）。
func (m *MemStore) FinishCancel(_ context.Context, missionID, reason, occurredAt string) (Mission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.indexOfLocked(missionID)
	if !ok {
		return Mission{}, false, ErrMissionNotFound
	}
	cur := m.missions[idx]
	if cur.Status != StatusCanceling {
		return Mission{}, false, nil
	}
	return m.flipStatusLocked(idx, cur.Status, StatusCanceled, reason, occurredAt), true, nil
}

// flipStatusLocked 条件状态转移 + status_changed 事件（调用方持锁并已
// 完成 CAS 前置判定）。
func (m *MemStore) flipStatusLocked(idx int, from, to, reason, occurredAt string) Mission {
	cur := m.missions[idx]
	cur.Status = to
	cur.UpdatedAt = occurredAt
	m.missions[idx] = cur
	payload := map[string]string{"from": from, "to": to}
	if reason != "" {
		payload["reason"] = reason
	}
	m.appendEventLocked(cur.MissionID, EventStatusChanged, payload, occurredAt)
	return cur
}

func (m *MemStore) MissionByID(_ context.Context, missionID string) (Mission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.indexOfLocked(missionID)
	if !ok {
		return Mission{}, false, nil
	}
	out := m.missions[idx]
	if key, has := m.bySource[missionID]; has {
		rs := RoomSource{RoomID: key[0], TaskID: key[1]}
		out.RoomSource = &rs
	}
	return out, true, nil
}

func (m *MemStore) ListMissions(_ context.Context) ([]Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Summary, 0, len(m.missions))
	for _, ms := range m.missions {
		out = append(out, Summary{
			MissionID: ms.MissionID, ContractVersion: ms.ContractVersion, Status: ms.Status,
			OwnerAgentID: ms.OwnerAgentID, Objective: ms.Objective,
			CreatedAt: ms.CreatedAt, UpdatedAt: ms.UpdatedAt,
		})
	}
	return out, nil
}

func (m *MemStore) MissionVersions(_ context.Context, missionID string) ([]Mission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs, ok := m.versions[missionID]
	if !ok {
		return nil, false, nil
	}
	out := append([]Mission(nil), vs...)
	if key, has := m.bySource[missionID]; has {
		for i := range out {
			rs := RoomSource{RoomID: key[0], TaskID: key[1]}
			out[i].RoomSource = &rs
		}
	}
	return out, true, nil
}

func (m *MemStore) MissionEvents(_ context.Context, missionID string) ([]Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.versions[missionID]; !ok {
		return nil, false, nil
	}
	return append([]Event(nil), m.events[missionID]...), true, nil
}

// TransitionStatus 条件状态投影（幂等无操作；事件 Seq 由存储赋）。
func (m *MemStore) TransitionStatus(_ context.Context, missionID, from, to string, event Event) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.indexOfLocked(missionID)
	if !ok || m.missions[i].Status != from {
		return false, nil
	}
	if m.missions[i].Status == StatusCanceled {
		return false, nil
	}
	m.missions[i].Status = to
	m.seq++
	event.Seq = m.seq
	m.events[missionID] = append(m.events[missionID], event)
	return true, nil
}

func (m *MemStore) indexOfLocked(missionID string) (int, bool) {
	for i, ms := range m.missions {
		if ms.MissionID == missionID {
			return i, true
		}
	}
	return 0, false
}

// snapshot 版本行（updated_at 对齐该版本时间，作 MissionView 返回）。
func snapshot(ms Mission) Mission {
	out := ms
	out.UpdatedAt = ms.UpdatedAt
	return out
}

func (m *MemStore) appendEventLocked(missionID, eventType string, payload map[string]string, occurredAt string) {
	m.seq++
	raw, _ := json.Marshal(payload)
	m.events[missionID] = append(m.events[missionID], Event{
		Seq: m.seq, MissionID: missionID, EventType: eventType,
		Payload: raw, OccurredAt: occurredAt,
	})
}
