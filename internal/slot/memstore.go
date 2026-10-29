package slot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
)

// MemStore 职责模板内存实现（UT/端点测试件；语义与 SQLite 实现一致：
// 版本不可变、追加互斥取号、创建序摘要）。
type MemStore struct {
	mu        sync.Mutex
	specs     []SlotSpec     // 创建序（首版插入序）
	instances []SlotInstance // 创建序
	bindings  []Binding      // 创建序
	bevents   []BindingEvent
	bseq      int64
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

// ---- SlotInstance（ADR-0020，0.5.3；MemStore 实现）----

func (m *MemStore) CreateInstance(_ context.Context, inst SlotInstance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.specs {
		if ex.SlotSpecID == inst.SlotSpecID {
			// 谱系存在（服务层已校验；此处兜底）——落实例并校命名空间唯一
			for _, in := range m.instances {
				if in.KnowledgeNamespace == inst.KnowledgeNamespace {
					return fmt.Errorf("slot: 命名空间唯一约束（%s）", inst.KnowledgeNamespace)
				}
			}
			m.instances = append(m.instances, inst)
			return nil
		}
	}
	return ErrNotFound
}

func (m *MemStore) ListInstances(_ context.Context) ([]SlotInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SlotInstance(nil), m.instances...), nil
}

func (m *MemStore) InstanceByID(_ context.Context, slotInstanceID string) (SlotInstance, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, in := range m.instances {
		if in.SlotInstanceID == slotInstanceID {
			return in, true, nil
		}
	}
	return SlotInstance{}, false, nil
}

func (m *MemStore) LatestByLineage(_ context.Context) (map[string]InstanceSpecNow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]InstanceSpecNow{}
	for _, ex := range m.specs {
		now, ok := out[ex.SlotSpecID]
		if !ok || ex.Version > now.LatestVersion {
			out[ex.SlotSpecID] = InstanceSpecNow{Name: ex.Name, LatestVersion: ex.Version}
		}
	}
	return out, nil
}

// ---- Binding 任职（ADR-0021，0.5.4；MemStore 实现）----

func (m *MemStore) CreateBinding(_ context.Context, b Binding) (Binding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindings = append(m.bindings, b)
	payload, _ := json.Marshal(map[string]string{
		"agent_id": b.AgentID, "slot_instance_id": b.SlotInstanceID,
		"slot_spec_version": strconv.Itoa(b.SlotSpecVersion),
	})
	m.bseq++
	m.bevents = append(m.bevents, BindingEvent{
		Seq: m.bseq, BindingID: b.BindingID, EventType: EventCreated,
		Payload: payload, OccurredAt: b.CreatedAt,
	})
	return b, nil
}

func (m *MemStore) BindingByID(_ context.Context, bindingID string) (Binding, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.bindings {
		if b.BindingID == bindingID {
			return b, true, nil
		}
	}
	return Binding{}, false, nil
}

func (m *MemStore) ListBindings(_ context.Context, instanceID string) ([]Binding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Binding{}
	for _, b := range m.bindings {
		if instanceID == "" || b.SlotInstanceID == instanceID {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *MemStore) TransitionBinding(_ context.Context, bindingID string, allowedFrom []string, to string, extras map[string]string, occurredAt string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, b := range m.bindings {
		if b.BindingID == bindingID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", false, ErrNotFound
	}
	from := m.bindings[idx].Status
	if !allowedIn(allowedFrom, from) {
		return "", false, nil
	}
	if to == BindingActive { // 单 active 互斥（模拟部分唯一索引）
		for _, b := range m.bindings {
			if b.SlotInstanceID == m.bindings[idx].SlotInstanceID && b.Status == BindingActive {
				return "", false, ErrActiveBindingExists
			}
		}
	}
	m.bindings[idx].Status = to
	m.bindings[idx].UpdatedAt = occurredAt
	m.appendBindingChangedLocked(bindingID, from, to, extras, occurredAt)
	return from, true, nil
}

func (m *MemStore) ReleaseBinding(_ context.Context, bindingID string, handoffNote, occurredAt string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, b := range m.bindings {
		if b.BindingID == bindingID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", false, ErrNotFound
	}
	from := m.bindings[idx].Status
	if from != BindingActive {
		return "", false, nil
	}
	extras := map[string]string{}
	if handoffNote != "" {
		extras["handoff_note"] = handoffNote
	}
	m.appendBindingChangedLocked(bindingID, from, BindingDraining, extras, occurredAt)
	m.appendBindingChangedLocked(bindingID, BindingDraining, BindingReleased, nil, occurredAt)
	m.bindings[idx].Status = BindingReleased
	m.bindings[idx].HandoffNote = handoffNote
	m.bindings[idx].UpdatedAt = occurredAt
	return from, true, nil
}

func (m *MemStore) BindingEvents(_ context.Context, bindingID string) ([]BindingEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []BindingEvent
	for _, ev := range m.bevents {
		if ev.BindingID == bindingID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (m *MemStore) appendBindingChangedLocked(bindingID, from, to string, extras map[string]string, occurredAt string) {
	payload := map[string]string{"from": from, "to": to}
	for k, v := range extras {
		payload[k] = v
	}
	raw, _ := json.Marshal(payload)
	m.bseq++
	m.bevents = append(m.bevents, BindingEvent{
		Seq: m.bseq, BindingID: bindingID, EventType: EventStatusChanged,
		Payload: raw, OccurredAt: occurredAt,
	})
}
