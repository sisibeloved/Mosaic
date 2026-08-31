// MemStore：AtomicStore/EventReader 的内存实现——UT 测试件与嵌入式场景共用。
// 非持久化；并发安全；语义与 SQLite 实现一致（同批原子、seq 按房连续、游标按全局位）。
package room

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// MemStore 内存事件与回执存储。
type MemStore struct {
	mu       sync.Mutex
	events   []protocol.Envelope            // 全局提交序
	byRoom   map[string][]protocol.Envelope // 房间视图
	receipts map[string]*CommandReceipt     // key: tenant|idem|kind
	claims   map[string]claimEntry          // key: room|stimulus（ClaimStore）
	nextSeq  map[string]int64
}

// claimEntry 声明行（信封 + 持久位，重驱动排序依据）。
type claimEntry struct {
	envelope []byte
	position int64
}

// NewMemStore 构造空存储。
func NewMemStore() *MemStore {
	return &MemStore{
		byRoom:   map[string][]protocol.Envelope{},
		receipts: map[string]*CommandReceipt{},
		claims:   map[string]claimEntry{},
		nextSeq:  map[string]int64{},
	}
}

func claimKey(roomID, stimulusID string) string { return roomID + "|" + stimulusID }

// ClaimStimulus 实现 ClaimStore。
func (m *MemStore) ClaimStimulus(_ context.Context, roomID, stimulusID string, envelope []byte, position int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := claimKey(roomID, stimulusID)
	if _, exists := m.claims[key]; exists {
		return false, nil
	}
	m.claims[key] = claimEntry{envelope: append([]byte(nil), envelope...), position: position}
	return true, nil
}

// DeleteClaim 实现 ClaimStore。
func (m *MemStore) DeleteClaim(_ context.Context, roomID, stimulusID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.claims, claimKey(roomID, stimulusID))
	return nil
}

// PendingClaims 实现 ClaimStore：按持久位升序（四轮复审 #14：恢复顺序 = 到达顺序）。
func (m *MemStore) PendingClaims(_ context.Context) ([]StimulusClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StimulusClaim, 0, len(m.claims))
	for key, c := range m.claims {
		roomID, stimulusID, _ := strings.Cut(key, "|")
		out = append(out, StimulusClaim{
			RoomID: roomID, StimulusEventID: stimulusID,
			Envelope: append([]byte(nil), c.envelope...), Position: c.position,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, nil
}

func receiptKey(tenant, idem, kind string) string {
	return tenant + "|" + idem + "|" + kind
}

func (m *MemStore) appendLocked(envs []protocol.Envelope, rc *CommandReceipt) ([]protocol.Envelope, error) {
	// 预检整批唯一性：与 SQLite 整批回滚语义一致（部分追加 = 假原子）
	seen := map[string]struct{}{}
	for _, env := range envs {
		if _, dup := seen[env.EventID]; dup {
			return nil, ErrDuplicateEvent
		}
		seen[env.EventID] = struct{}{}
	}
	for _, e := range m.events {
		if _, dup := seen[e.EventID]; dup {
			return nil, ErrDuplicateEvent
		}
	}
	if rc != nil {
		key := receiptKey(rc.TenantID, rc.IdempotencyKey, rc.CommandKind)
		if _, exists := m.receipts[key]; exists {
			return nil, ErrDuplicateReceipt
		}
		// 乐观并发在追加临界区内强制（与 SQLite 事务内校验同语义）：
		// 先查回执（竞态重放优先于版本冲突），再校验期望版本，最后才落事件。
		if len(envs) > 0 && rc.ExpectedRoomVersion != m.nextSeq[envs[0].RoomID] {
			return nil, fmt.Errorf("%w: expected=%d current=%d",
				ErrVersionConflict, rc.ExpectedRoomVersion, m.nextSeq[envs[0].RoomID])
		}
	}
	out := make([]protocol.Envelope, len(envs))
	for i := range envs {
		env := envs[i]
		m.nextSeq[env.RoomID]++
		env.Seq = m.nextSeq[env.RoomID]
		m.events = append(m.events, env)
		m.byRoom[env.RoomID] = append(m.byRoom[env.RoomID], env)
		out[i] = env
	}
	if rc != nil {
		key := receiptKey(rc.TenantID, rc.IdempotencyKey, rc.CommandKind)
		cp := *rc
		if len(out) > 0 {
			cp.RoomVersion = out[len(out)-1].Seq
		}
		m.receipts[key] = &cp
	}
	return out, nil
}

// AppendEvents 实现 AtomicStore。
func (m *MemStore) AppendEvents(ctx context.Context, envs []protocol.Envelope) ([]protocol.Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendLocked(envs, nil)
}

// AppendEventsIf 实现 CASStore（四轮复审 #12）：当前版本不符即拒绝（同临界区判定）。
func (m *MemStore) AppendEventsIf(ctx context.Context, envs []protocol.Envelope, expectedRoomVersion int64) ([]protocol.Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(envs) > 0 && expectedRoomVersion != m.nextSeq[envs[0].RoomID] {
		return nil, fmt.Errorf("%w: expected=%d current=%d",
			ErrVersionConflict, expectedRoomVersion, m.nextSeq[envs[0].RoomID])
	}
	return m.appendLocked(envs, nil)
}

// AppendWithReceipt 实现 AtomicStore。
func (m *MemStore) AppendWithReceipt(ctx context.Context, envs []protocol.Envelope, rc CommandReceipt) ([]protocol.Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 与 SQLite 一致：事件撞车（同 event id）也报回执冲突（并发同命令竞态后到者）
	out, err := m.appendLocked(envs, &rc)
	if err == ErrDuplicateEvent {
		return nil, ErrDuplicateReceipt
	}
	return out, err
}

// LookupReceipt 实现 AtomicStore；未命中 (nil, nil)。
func (m *MemStore) LookupReceipt(ctx context.Context, tenant, idem, kind string) (*CommandReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rc, ok := m.receipts[receiptKey(tenant, idem, kind)]; ok {
		cp := *rc
		return &cp, nil
	}
	return nil, nil
}

// RoomVersion 实现 AtomicStore。
func (m *MemStore) RoomVersion(ctx context.Context, roomID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextSeq[roomID], nil
}

// RoomExists 实现 AtomicStore。
func (m *MemStore) RoomExists(ctx context.Context, roomID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.RoomID == roomID && e.Type == protocol.EventRoomCreated {
			return true, nil
		}
	}
	return false, nil
}

// RoomEvents 房间事件快照（测试与投影消费；返回副本）。
func (m *MemStore) RoomEvents(roomID string) []protocol.Envelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]protocol.Envelope(nil), m.byRoom[roomID]...)
}

// EventsAfter 实现 EventReader：全局位 = 全局提交序下标 + 1。
func (m *MemStore) EventsAfter(ctx context.Context, roomID, cursor string, limit int) ([]StoredEvent, string, error) {
	pos, err := protocol.DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var events []StoredEvent
	for i := int(pos); i < len(m.events) && len(events) < limit; i++ {
		env := m.events[i]
		if env.RoomID != roomID {
			continue
		}
		events = append(events, StoredEvent{Envelope: env, Cursor: protocol.EncodeCursor(int64(i + 1))})
	}
	var next string
	if len(events) == limit && int(pos)+len(events) < len(m.events) {
		next = events[len(events)-1].Cursor
	}
	return events, next, nil
}
