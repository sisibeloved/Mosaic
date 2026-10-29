package slot

import (
	"context"
	"sync"
)

// MemStore 职责模板内存实现（UT/端点测试件；语义与 SQLite 实现一致：
// 版本不可变、追加互斥取号、创建序摘要）。
type MemStore struct {
	mu    sync.Mutex
	specs []SlotSpec // 创建序（首版插入序）
}

func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) CreateSpec(_ context.Context, spec SlotSpec) (SlotSpec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.specs {
		if ex.SlotSpecID == spec.SlotSpecID {
			return SlotSpec{}, ErrSpecExists
		}
	}
	m.specs = append(m.specs, spec)
	return spec, nil
}

func (m *MemStore) AppendVersion(_ context.Context, slotSpecID string, content SpecContent, now string) (SlotSpec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	latest := 0
	found := false
	for _, ex := range m.specs {
		if ex.SlotSpecID == slotSpecID {
			found = true
			if ex.Version > latest {
				latest = ex.Version
			}
		}
	}
	if !found {
		return SlotSpec{}, ErrNotFound
	}
	spec := SlotSpec{
		SlotSpecID: slotSpecID, Version: latest + 1, SchemaVersion: SchemaVersion,
		SpecContent: content, CreatedAt: now,
	}
	m.specs = append(m.specs, spec)
	return spec, nil
}

func (m *MemStore) ListSummaries(_ context.Context) ([]SpecSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byID := map[string]*SpecSummary{}
	var order []string
	for _, ex := range m.specs {
		s, ok := byID[ex.SlotSpecID]
		if !ok {
			s = &SpecSummary{SlotSpecID: ex.SlotSpecID, CreatedAt: ex.CreatedAt}
			byID[ex.SlotSpecID] = s
			order = append(order, ex.SlotSpecID)
		}
		s.Name = ex.Name
		s.LatestVersion = ex.Version
		s.VersionCount++
		s.UpdatedAt = ex.CreatedAt
	}
	out := make([]SpecSummary, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

func (m *MemStore) Latest(_ context.Context, slotSpecID string) (SlotSpec, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out SlotSpec
	found := false
	for _, ex := range m.specs {
		if ex.SlotSpecID == slotSpecID && (!found || ex.Version > out.Version) {
			out, found = ex, true
		}
	}
	return out, found, nil
}

func (m *MemStore) Versions(_ context.Context, slotSpecID string) ([]SlotSpec, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SlotSpec
	for _, ex := range m.specs {
		if ex.SlotSpecID == slotSpecID {
			out = append(out, ex)
		}
	}
	// 插入序天然升序（版本号单调追加）
	return out, out != nil, nil
}
