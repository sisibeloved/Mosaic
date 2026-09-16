// MemStore：DocStore/DocCASStore/DocEventReader/DocSearcher 的内存实现——
// UT 测试件与嵌入式场景共用。非持久化；并发安全；语义与 SQLite 实现一致
// （同批原子、version 按文档连续、游标即 version）。
package doc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// MemStore 内存文档事件与回执存储。
type MemStore struct {
	mu          sync.Mutex
	byDoc       map[string][]protocol.DocEnvelope // 文档视图（追加序即 version 序）
	receipts    map[string]*CommandReceipt        // key: tenant|idem|kind
	tombstones  map[string]string                 // doc_id → reason（删除留痕）
	nextVersion map[string]int64
}

// NewMemStore 构造空存储。
func NewMemStore() *MemStore {
	return &MemStore{
		byDoc:       map[string][]protocol.DocEnvelope{},
		receipts:    map[string]*CommandReceipt{},
		tombstones:  map[string]string{},
		nextVersion: map[string]int64{},
	}
}

func docReceiptKey(tenant, idem, kind string) string {
	return tenant + "|" + idem + "|" + kind
}

func (m *MemStore) appendLocked(envs []protocol.DocEnvelope, rc *CommandReceipt) ([]protocol.DocEnvelope, error) {
	// 预检整批唯一性：与 SQLite 整批回滚语义一致（部分追加 = 假原子）
	seen := map[string]struct{}{}
	for _, env := range envs {
		if _, dup := seen[env.EventID]; dup {
			return nil, ErrDuplicateEvent
		}
		seen[env.EventID] = struct{}{}
	}
	for _, existing := range m.byDoc {
		for _, e := range existing {
			if _, dup := seen[e.EventID]; dup {
				return nil, ErrDuplicateEvent
			}
		}
	}
	if len(envs) > 0 {
		docID := envs[0].DocID
		for i := range envs {
			if envs[i].DocID != docID {
				return nil, fmt.Errorf("doc: 一批事件必须同 doc（%s vs %s）", docID, envs[i].DocID)
			}
		}
	}
	if rc != nil {
		key := docReceiptKey(rc.TenantID, rc.IdempotencyKey, rc.CommandKind)
		if _, exists := m.receipts[key]; exists {
			return nil, ErrDuplicateReceipt
		}
		// 乐观并发在追加临界区内强制（与 SQLite 事务内校验同语义）：
		// 先查回执（竞态重放优先于版本冲突），再校验期望版本，最后才落事件。
		if len(envs) > 0 && rc.ExpectedDocVersion != m.nextVersion[envs[0].DocID] {
			return nil, fmt.Errorf("%w: expected=%d current=%d",
				ErrVersionConflict, rc.ExpectedDocVersion, m.nextVersion[envs[0].DocID])
		}
	}
	out := make([]protocol.DocEnvelope, len(envs))
	for i := range envs {
		env := envs[i]
		m.nextVersion[env.DocID]++
		env.Version = m.nextVersion[env.DocID]
		m.byDoc[env.DocID] = append(m.byDoc[env.DocID], env)
		out[i] = env
	}
	if rc != nil {
		key := docReceiptKey(rc.TenantID, rc.IdempotencyKey, rc.CommandKind)
		cp := *rc
		if len(out) > 0 {
			cp.DocVersion = out[len(out)-1].Version
		}
		m.receipts[key] = &cp
	}
	return out, nil
}

// AppendDocEvents 实现 DocStore。
func (m *MemStore) AppendDocEvents(ctx context.Context, envs []protocol.DocEnvelope) ([]protocol.DocEnvelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendLocked(envs, nil)
}

// AppendDocEventsWithReceipt 实现 DocStore。
func (m *MemStore) AppendDocEventsWithReceipt(ctx context.Context, envs []protocol.DocEnvelope, rc CommandReceipt) ([]protocol.DocEnvelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 与 SQLite 一致：事件撞车（同 event id）也报回执冲突（并发同命令竞态后到者）
	out, err := m.appendLocked(envs, &rc)
	if err == ErrDuplicateEvent {
		return nil, ErrDuplicateReceipt
	}
	return out, err
}

// AppendDocEventsIf 实现 DocCASStore：当前版本不符即拒绝（同临界区判定）。
func (m *MemStore) AppendDocEventsIf(ctx context.Context, envs []protocol.DocEnvelope, expectedDocVersion int64) ([]protocol.DocEnvelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(envs) > 0 && expectedDocVersion != m.nextVersion[envs[0].DocID] {
		return nil, fmt.Errorf("%w: expected=%d current=%d",
			ErrVersionConflict, expectedDocVersion, m.nextVersion[envs[0].DocID])
	}
	return m.appendLocked(envs, nil)
}

// LookupDocReceipt 实现 DocStore；未命中 (nil, nil)。
func (m *MemStore) LookupDocReceipt(ctx context.Context, tenant, idem, kind string) (*CommandReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rc, ok := m.receipts[docReceiptKey(tenant, idem, kind)]; ok {
		cp := *rc
		return &cp, nil
	}
	return nil, nil
}

// DocVersion 实现 DocStore。
func (m *MemStore) DocVersion(ctx context.Context, docID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextVersion[docID], nil
}

// DocExists 实现 DocStore。
func (m *MemStore) DocExists(ctx context.Context, docID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.byDoc[docID] {
		if e.Type == protocol.EventDocCreated {
			return true, nil
		}
	}
	return false, nil
}

// DeleteDoc 实现 DocStore 删除级联：事件/回执全清，落墓碑（reason 取
// doc.deleted 载荷——与 SQLite 同事后读取口径）。
func (m *MemStore) DeleteDoc(ctx context.Context, docID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	reason := ""
	for _, e := range m.byDoc[docID] {
		if e.Type == protocol.EventDocDeleted {
			var p protocol.DocDeletedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				reason = p.Reason
			}
		}
	}
	m.tombstones[docID] = reason
	delete(m.byDoc, docID)
	delete(m.nextVersion, docID)
	for k, rc := range m.receipts {
		if rc.DocID == docID {
			delete(m.receipts, k)
		}
	}
	return nil
}

// Tombstone 墓碑查询（测试断言与墓碑读路径共用；未删除返回 ("", false)）。
func (m *MemStore) Tombstone(docID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reason, ok := m.tombstones[docID]
	return reason, ok
}

// DocEvents 文档事件快照（测试与投影消费；返回副本）。
func (m *MemStore) DocEvents(docID string) []protocol.DocEnvelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]protocol.DocEnvelope(nil), m.byDoc[docID]...)
}

// DocEventsAfter 实现 DocEventReader：游标即 version（ADR-0014）。
func (m *MemStore) DocEventsAfter(ctx context.Context, docID, cursor string, limit int) ([]StoredDocEvent, string, error) {
	pos, err := protocol.DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var events []StoredDocEvent
	for _, env := range m.byDoc[docID] {
		if env.Version <= pos {
			continue
		}
		if len(events) >= limit {
			break
		}
		events = append(events, StoredDocEvent{Envelope: env, Cursor: protocol.EncodeCursor(env.Version)})
	}
	var next string
	if len(events) == limit {
		next = events[len(events)-1].Cursor
	}
	return events, next, nil
}

// SearchDocs 实现 DocSearcher 线性语义基准：当前态标题 + 正文子串、大小写不敏感、
// 每文档一条（version 倒序、doc_id 升序兜底）、limit 1..100 默认 20。
// 与 SQLite FTS 实现的语义差（FTS 累积历史修订文本、按索引行归并）不影响
// 服务层 UT——生产检索行为以 SQLite 实现与 fts IT 为准。
func (m *MemStore) SearchDocs(ctx context.Context, query string, limit int) ([]DocSearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []DocSearchHit{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	needle := strings.ToLower(query)
	m.mu.Lock()
	defer m.mu.Unlock()
	hits := []DocSearchHit{}
	for docID, events := range m.byDoc {
		stored := make([]StoredDocEvent, 0, len(events))
		for _, env := range events {
			stored = append(stored, StoredDocEvent{Envelope: env})
		}
		state, err := ProjectDoc(stored)
		if err != nil || state.Version == 0 {
			continue
		}
		var sb strings.Builder
		for _, b := range state.Blocks {
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		}
		if !strings.Contains(strings.ToLower(state.Title), needle) &&
			!strings.Contains(strings.ToLower(sb.String()), needle) {
			continue
		}
		last := events[len(events)-1]
		hits = append(hits, DocSearchHit{
			DocID: docID, EventID: last.EventID, Version: state.Version,
			Actor: last.Actor.ParticipantID, Title: state.Title,
			Body: sb.String(), OccurredAt: last.OccurredAt,
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Version != hits[j].Version {
			return hits[i].Version > hits[j].Version
		}
		return hits[i].DocID < hits[j].DocID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}
