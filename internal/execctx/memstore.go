// memstore：execctx.Store 内存测试件（UT/端点测试）。
package execctx

import (
	"context"
	"sync"
)

type memRow struct {
	m   Manifest
	r   Receipt
	seq int
}

// MemStore 内存实现（并发安全；装配序即 seq 序）。
type MemStore struct {
	mu       sync.Mutex
	next     int
	byID     map[string]memRow
	byDigest map[string]string // mission_id + "\x1f" + digest → manifest_id
}

// NewMemStore 建空库。
func NewMemStore() *MemStore {
	return &MemStore{byID: map[string]memRow{}, byDigest: map[string]string{}}
}

// CreateManifest 落库（清单与回执同"事务"）；(mission_id, digest) 撞键
// ErrManifestExists。
func (m *MemStore) CreateManifest(_ context.Context, man Manifest, r Receipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dk := man.MissionID + "\x1f" + man.Digest
	if _, ok := m.byDigest[dk]; ok {
		return ErrManifestExists
	}
	m.next++
	m.byID[man.ManifestID] = memRow{m: man, r: r, seq: m.next}
	m.byDigest[dk] = man.ManifestID
	return nil
}

// ManifestByDigest 按 (mission_id, digest) 取；不存在 ok=false。
func (m *MemStore) ManifestByDigest(_ context.Context, missionID, digest string) (Manifest, Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byDigest[missionID+"\x1f"+digest]
	if !ok {
		return Manifest{}, Receipt{}, false, nil
	}
	row := m.byID[id]
	return row.m, row.r, true, nil
}

// ManifestByID 单查（含回执）；不存在 ok=false。
func (m *MemStore) ManifestByID(_ context.Context, manifestID string) (Manifest, Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.byID[manifestID]
	if !ok {
		return Manifest{}, Receipt{}, false, nil
	}
	return row.m, row.r, true, nil
}

// ManifestsByMission 装配历史（创建序倒序）。
func (m *MemStore) ManifestsByMission(_ context.Context, missionID string) ([]Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Manifest
	for _, row := range m.byID {
		if row.m.MissionID == missionID {
			out = append(out, row.m)
		}
	}
	// 冒泡足够（测试规模小）；seq 大者在前。
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if m.byID[out[j].ManifestID].seq > m.byID[out[i].ManifestID].seq {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if out == nil {
		out = []Manifest{}
	}
	return out, nil
}
