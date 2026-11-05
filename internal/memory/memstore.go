// Package memory 内存件（UT / 内存装配）：过滤/排序（创建序降序）/游标/
// limit 语义与 sqlite 实现同源（ListFilter 契约单源在 Store 接口注释）。
package memory

import (
	"context"
	"sync"
)

// MemStore 内存存储（创建序 = 追加序——与 sqlite rowid 同语义）。
type MemStore struct {
	mu    sync.Mutex
	items []Item // 升序追加
	byID  map[string]Item
}

// NewMemStore 构造。
func NewMemStore() *MemStore {
	return &MemStore{byID: map[string]Item{}}
}

// CreateItem 落库（memory_id 冲突 = 编程错误，panic 面）。
func (m *MemStore) CreateItem(_ context.Context, it Item) (Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append(m.items, it)
	m.byID[it.MemoryID] = it
	return it, nil
}

// ItemByID 单查。
func (m *MemStore) ItemByID(_ context.Context, memoryID string) (Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.byID[memoryID]
	return it, ok, nil
}

// ListItems 过滤 + 创建序降序 + 游标（before 之后的更早条目）+ limit+1 条
// （第 limit+1 条供 service 判 next_cursor）。before 未知（已删除/游标失效）
// = 空页——与 sqlite 实现同口径。
func (m *MemStore) ListItems(_ context.Context, f ListFilter) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 降序遍历（追加序倒排）；before 命中后开始收集。
	out := make([]Item, 0, f.Limit+1)
	started := f.Before == ""
	for i := len(m.items) - 1; i >= 0; i-- {
		it := m.items[i]
		if !started {
			// 一审 #4：游标只在本域内定位——外域同 ID 不生效（空页，
			// 与未知 ID/游标失效同语义，越域存在性不泄漏）。
			if it.MemoryID == f.Before && it.Namespace == f.Namespace {
				started = true
			}
			continue
		}
		if it.Namespace != f.Namespace {
			continue
		}
		if f.ScopeID != "" && it.ScopeID != f.ScopeID {
			continue
		}
		if f.Kind != "" && it.Kind != f.Kind {
			continue
		}
		if f.Status != "" && it.Status != f.Status {
			continue
		}
		out = append(out, it)
		if len(out) >= f.Limit+1 {
			break
		}
	}
	return out, nil
}
