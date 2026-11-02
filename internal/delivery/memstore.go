// memstore：delivery.Store 内存测试件（append-only；事务语义 = 单临界区：守卫 →
// 回执汇 → 写包，任一步失败整体不落；按真实时间倒序）。
package delivery

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// GuardFunc 事务时 Mission 真值（version, status, ok）——测试件据此模拟"读-写
// 之间被并发修订/取消"（审校 #1）。
type GuardFunc func(missionID string) (version int, status string, ok bool)

// ReceiptSink 回执汇（写包同临界区内调用——失败则包不落，审校 #2）。
type ReceiptSink func(ctx context.Context, runID, eventType string, payload map[string]any) error

// MemStore 内存实现。
type MemStore struct {
	mu    sync.Mutex
	rows  map[string]Package
	seq   int // 插入序（同一时刻的稳定倒序）
	ord   map[string]int
	guard GuardFunc
	sink  ReceiptSink
}

// NewMemStore 建空库（无守卫 = 放行；无回执汇 = 丢弃回执）。
func NewMemStore() *MemStore { return &MemStore{rows: map[string]Package{}, ord: map[string]int{}} }

// WithGuard 注入事务时 Mission 真值源。
func (m *MemStore) WithGuard(g GuardFunc) *MemStore { m.guard = g; return m }

// WithReceiptSink 注入回执汇。
func (m *MemStore) WithReceiptSink(s ReceiptSink) *MemStore { m.sink = s; return m }

// CreatePackage 守卫 → 撞键 → 回执 → 写包（单临界区，整体成败）。
func (m *MemStore) CreatePackage(ctx context.Context, p Package, receipt Receipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.guard != nil {
		version, status, ok := m.guard(p.MissionID)
		if !ok {
			return ErrMissionNotFound
		}
		if status == missionStatusCanceled {
			return fmt.Errorf("%w: 不再受理交付", ErrMissionCanceled)
		}
		if version != p.ContractVersion {
			return fmt.Errorf("%w: 期望 v%d，当前 v%d", ErrVersionConflict, p.ContractVersion, version)
		}
	}
	if _, exists := m.rows[p.PackageID]; exists {
		return fmt.Errorf("delivery: 交付包 %s 已存在", p.PackageID)
	}
	if m.sink != nil {
		if err := m.sink(ctx, receipt.RunID, receipt.EventType, receipt.Payload); err != nil {
			return fmt.Errorf("delivery: 回执写入失败（包不落）: %w", err)
		}
	}
	m.seq++
	m.rows[p.PackageID] = p
	m.ord[p.PackageID] = m.seq
	return nil
}

// ListPackagesByMission 真实时间倒序（同刻按插入序倒序）。
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
			return LaterThan(out[i].SubmittedAt, out[j].SubmittedAt)
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
