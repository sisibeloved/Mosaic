// Package memory 内存件（UT / 内存装配）：过滤/排序（创建序降序）/游标/
// limit 语义与 sqlite 实现同源（ListFilter 契约单源在 Store 接口注释）。
package memory

import (
	"context"
	"strings"
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

// UpdateItem CAS 原位替换（**版本 + 状态**双基准——一审 #2；任一不符
// ok=false）。
func (m *MemStore) UpdateItem(_ context.Context, memoryID string, expectedVersion int, expectedStatus Status, next Item) (Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].MemoryID != memoryID {
			continue
		}
		if m.items[i].Version != expectedVersion || m.items[i].Status != expectedStatus {
			return Item{}, false, nil
		}
		m.items[i] = next
		m.byID[memoryID] = next
		return next, true, nil
	}
	return Item{}, false, nil
}

// SupersedeItem 双动作（内存件分步 + 失败回滚补偿——0.6.13 续接原子面同
// 模式；事务语义由调用方域层保证：CAS 失败整体不落）。
func (m *MemStore) SupersedeItem(_ context.Context, oldID string, expectedVersion int, expectedStatus Status, oldNext, replacement Item) (Item, Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i := range m.items {
		if m.items[i].MemoryID == oldID {
			idx = i
			break
		}
	}
	if idx < 0 || m.items[idx].Version != expectedVersion || m.items[idx].Status != expectedStatus {
		return Item{}, Item{}, false, nil
	}
	m.items[idx] = oldNext
	m.byID[oldID] = oldNext
	// 持锁直写（不可调 CreateItem——mutex 不可重入会死锁）；内存件插入
	// 无约束错误面，无需回滚补偿。
	m.items = append(m.items, replacement)
	m.byID[replacement.MemoryID] = replacement
	return oldNext, replacement, true, nil
}

// ListItems 过滤 +（可选）召回匹配 + 创建序降序 + 游标（before 之后的更早
// 条目）+ limit+1 条（第 limit+1 条供 service 判 next_cursor；召回模式 limit
// 条）。before 未知（已删除/游标失效）= 空页——与 sqlite 实现同口径。
// 0.7.2 召回（ADR-0039 §3）：子串匹配（大小写不敏感——与 sqlite LIKE 回退
// 基准同语义）；total 为过滤+匹配全量计数；排序保持创建序降序（语义子集——
// bm25 序由 sqlite 实现独占）。
func (m *MemStore) ListItems(_ context.Context, f ListFilter) ([]Item, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 降序遍历（追加序倒排）。total = 公共过滤+匹配的**全量计数**（一审 #3：
	// 不含 before/limit——每页恒定）；before 只作用于收集起点。二审 #1：游标
	// 在 **namespace 隔离后、其余过滤前**定位——游标条自身被撤回/过期
	//（status/时效过滤掉）不影响续页（门仍可打开）；外域同 ID 仍不生效。
	out := make([]Item, 0, f.Limit+1)
	total := 0
	started := f.Before == ""
	for i := len(m.items) - 1; i >= 0; i-- {
		it := m.items[i]
		if it.Namespace != f.Namespace {
			continue
		}
		// passes = 公共过滤+匹配命中（计数判据——与收集门解耦）。
		passes := true
		if f.ScopeID != "" && it.ScopeID != f.ScopeID {
			passes = false
		} else if f.Kind != "" && it.Kind != f.Kind {
			passes = false
		} else if f.Status != "" && it.Status != f.Status {
			passes = false
		} else if f.Status == StatusActive && !f.IncludeExpired && expired(it.ValidTo, f.Now) {
			// 0.7.1 时效口径：active 且未显式包含时排除 valid_to < now。
			passes = false
		} else if f.Query != "" &&
			!strings.Contains(strings.ToLower(it.Title+"\n"+it.Content), strings.ToLower(f.Query)) {
			// 0.7.2 召回匹配（过滤先行——授权/时效过滤完成后才计 total）。
			passes = false
		}
		if passes {
			total++ // 全量计数（每页恒定——含游标门前与收集截断后的条目）
		}
		if !started {
			if it.MemoryID == f.Before {
				started = true
			}
			continue // before 之前的（更新的）条目不收集
		}
		if !passes {
			continue
		}
		if f.Query != "" {
			if len(out) >= f.Limit {
				continue // 召回模式无游标——只截断
			}
		} else if len(out) >= f.Limit+1 {
			// 非召回模式不 break——total 已继续计满，只停止收集
			//（limit+1 供 service 判 next_cursor）。
			continue
		}
		out = append(out, it)
	}
	return out, total, nil
}
