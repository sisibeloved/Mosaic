// memstore：runtime.Store 内存测试件（UT/端点测试）。
package runtime

import (
	"context"
	"sort"
	"sync"
)

// MemStore 内存实现（并发安全；事件 seq 全表单调）。
type MemStore struct {
	mu       sync.Mutex
	nextSeq  int64
	runtimes map[string]Runtime
	events   []Event
	envs     map[string]Environment
	dirTaken map[string]string // workspace_dir → environment_id
	leases   map[string]Lease
}

// NewMemStore 建空库。
func NewMemStore() *MemStore {
	return &MemStore{runtimes: map[string]Runtime{}, envs: map[string]Environment{}, dirTaken: map[string]string{}, leases: map[string]Lease{}}
}

// SyncRuntimes 批量单事务语义（内存等价）：新行插入（并为真正插入的行
// 生成 discovered 事件）、既有行按发现幂等键只替换事实（无孤儿事件）。
func (m *MemStore) SyncRuntimes(_ context.Context, now string, rows []Runtime) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	created, refreshed := 0, 0
	for _, r := range rows {
		if existing, ok := m.byDiscoveryKey(r); ok {
			refreshed++
			existing.Channel = r.Channel
			existing.Version = r.Version
			existing.Login = r.Login
			existing.UpdatedAt = r.UpdatedAt
			m.runtimes[existing.RuntimeID] = existing
			continue
		}
		created++
		m.runtimes[r.RuntimeID] = r
		m.nextSeq++
		m.events = append(m.events, Event{
			Seq: m.nextSeq, RuntimeID: r.RuntimeID, EventType: EventDiscovered,
			Payload: map[string]any{"version": r.Version, "login": r.Login}, OccurredAt: now,
		})
	}
	return created, refreshed, nil
}

// byDiscoveryKey 按 (adapter,host,distro,path) 找既有行（发现幂等键）。
func (m *MemStore) byDiscoveryKey(r Runtime) (Runtime, bool) {
	for _, e := range m.runtimes {
		if e.Adapter == r.Adapter && e.Host == r.Host && e.Distro == r.Distro && e.Path == r.Path {
			return e, true
		}
	}
	return Runtime{}, false
}

// UpdateRuntime 全行替换 + 事件。
func (m *MemStore) UpdateRuntime(_ context.Context, r Runtime, events []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runtimes[r.RuntimeID] = r
	for _, e := range events {
		m.nextSeq++
		e.Seq = m.nextSeq
		m.events = append(m.events, e)
	}
	return nil
}

// RuntimeByID 单查；未知 ok=false。
func (m *MemStore) RuntimeByID(_ context.Context, runtimeID string) (Runtime, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runtimes[runtimeID]
	return r, ok, nil
}

// ListRuntimes 全量（发现序——按 RuntimeID 稳定排序的插入序近似；测试小规模）。
func (m *MemStore) ListRuntimes(_ context.Context) ([]Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Runtime, 0, len(m.runtimes))
	for _, r := range m.runtimes {
		out = append(out, r)
	}
	return out, nil
}

// RuntimeEvents 事件流（seq 升序）。
func (m *MemStore) RuntimeEvents(_ context.Context, runtimeID string) ([]Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events {
		if e.RuntimeID == runtimeID {
			out = append(out, e)
		}
	}
	return out, true, nil
}

// CreateEnvironment 建环境；workspace_dir 撞键 ErrEnvironmentDirTaken。
func (m *MemStore) CreateEnvironment(_ context.Context, e Environment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 判重按规范工作区身份（EvalSymlinks 键）计——路径别名同一物理目录
	// 不得双注册（审校 P2#2）；空键（历史行）回退目录串判重。
	key := e.WorkspaceKey
	if key == "" {
		key = e.WorkspaceDir
	}
	if _, taken := m.dirTaken[key]; taken {
		return ErrEnvironmentDirTaken
	}
	m.envs[e.EnvironmentID] = e
	m.dirTaken[key] = e.EnvironmentID
	return nil
}

// EnvironmentByID 单查；未知 ok=false。
func (m *MemStore) EnvironmentByID(_ context.Context, environmentID string) (Environment, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[environmentID]
	return e, ok, nil
}

// ListEnvironments 环境列表（默认在前，其余按 EnvironmentID 稳定序——
// 与 SQLite 实现同口径，map 遍历序不可依赖）。
func (m *MemStore) ListEnvironments(_ context.Context) ([]Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Environment, 0, len(m.envs))
	for _, e := range m.envs {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].EnvironmentID < out[j].EnvironmentID
	})
	return out, nil
}

// DefaultEnvironment 默认环境（恰一语义由装配保证；库内取 is_default）。
func (m *MemStore) DefaultEnvironment(_ context.Context) (Environment, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.envs {
		if e.IsDefault {
			return e, true, nil
		}
	}
	return Environment{}, false, nil
}
