// memstore：delivery.Store 内存测试件（append-only；倒序列表）。
package delivery

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// MemStore 内存实现。
type MemStore struct {
	mu   sync.Mutex
	rows map[string]Package
	seq  int // 插入序（同一时刻的稳定倒序）
	ord  map[string]int
}

// NewMemStore 建空库。
func NewMemStore() *MemStore { return &MemStore{rows: map[string]Package{}, ord: map[string]int{}} }

// CreatePackage 建行（主键撞键报错）。
func (m *MemStore) CreatePackage(_ context.Context, p Package) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.rows[p.PackageID]; exists {
		return fmt.Errorf("delivery: 交付包 %s 已存在", p.PackageID)
	}
	m.seq++
	m.rows[p.PackageID] = p
	m.ord[p.PackageID] = m.seq
	return nil
}

// ListPackagesByMission submitted_at 倒序（同刻按插入序倒序）。
func (m *MemStore) ListPackagesByMission(_ context.Context, missionID string) ([]Package, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Package{}
	for _, p := range m.rows {
		if p.MissionID == missionID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SubmittedAt != out[j].SubmittedAt {
			return out[i].SubmittedAt > out[j].SubmittedAt
		}
		return m.ord[out[i].PackageID] > m.ord[out[j].PackageID]
	})
	return out, nil
}

// PackageByID 单查。
func (m *MemStore) PackageByID(_ context.Context, packageID string) (Package, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[packageID]
	return p, ok, nil
}
