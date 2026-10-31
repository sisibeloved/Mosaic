// Package runtime：本地 Environment / Runtime（0.6.3 / ADR-0026）。
// Runtime 是一份可执行实例的 Harness、主机、能力与健康状态（发现自
// harness 注册表——sync 单向搬运不回写）；Environment 是命名工作区目录
// （0.6.3 子集）。探测/准备结果是数据不是错误：unhealthy 恒可返回携
// note。登录态是记录事实非 ready 门——"当前可执行"的调度裁定随 0.6.5
// （RFC §9.2 三种判断分层）。
//
// 包名 runtime 为 RFC §6 拟定；引用标准库 runtime 的文件沿用 goruntime
// 别名惯例。
package runtime

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// 执行面（沿用 harness Runtime 词；字段名 host 免与域包名撞）。
const (
	HostNative = "native"
	HostWSL    = "wsl"
)

// Runtime 状态 0.6.3 子集（busy/draining/stopped 随 0.6.4/0.6.5；
// 准备是同步操作，provisioning 不落库——不把瞬时态伪装成事实）。
const (
	StateDiscovered = "discovered"
	StateReady      = "ready"
	StateUnhealthy  = "unhealthy"
)

// 事件类型（runtime_events 流）。
const (
	EventDiscovered    = "discovered"
	EventHealthChecked = "health_checked"
	EventStateChanged  = "state_changed"
)

// 领域错误。
var (
	// ErrInvalidEnvironment 环境参数不合法（名称/目录缺失超限/目录不可创建）。
	ErrInvalidEnvironment = errors.New("runtime: 环境不合法")
	// ErrEnvironmentDirTaken 工作区目录已被其他环境占用。
	ErrEnvironmentDirTaken = errors.New("runtime: 工作区目录已被占用")
	// ErrEnvironmentNotFound 环境不存在（含缺省环境缺位）。
	ErrEnvironmentNotFound = errors.New("runtime: 环境不存在")
	// ErrRuntimeNotFound 运行时不存在。
	ErrRuntimeNotFound = errors.New("runtime: 运行时不存在")
	// ErrWorkspaceUnavailable 准备时工作区目录不可用。
	ErrWorkspaceUnavailable = errors.New("runtime: 工作区目录不可用")
)

// Environment 命名工作区目录（0.6.3 子集——依赖/数据卷等随 0.6.4+ 增列）。
type Environment struct {
	EnvironmentID string `json:"environment_id"` // env_ 前缀生成式
	Name          string `json:"name"`
	WorkspaceDir  string `json:"workspace_dir"`
	IsDefault     bool   `json:"is_default"` // 全库恰一（部分唯一索引；装配启动确保）
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// Runtime 运行时记录：镜像 harness 登记事实 + 本域状态与绑定。
type Runtime struct {
	RuntimeID      string `json:"runtime_id"` // rtx_ 前缀；(adapter,host,distro,path) 唯一发现幂等键
	Adapter        string `json:"adapter"`
	Host           string `json:"host"` // native | wsl
	Distro         string `json:"distro"`
	Path           string `json:"path"`
	Channel        string `json:"channel"`
	Version        string `json:"version"` // 最近探测版本
	Login          string `json:"login"`   // logged_in | logged_out | unknown
	State          string `json:"state"`   // discovered | ready | unhealthy
	EnvironmentID  string `json:"environment_id"`
	LastHealthAt   string `json:"last_health_at"`
	LastHealthNote string `json:"last_health_note"`
	DiscoveredAt   string `json:"discovered_at"`
	UpdatedAt      string `json:"updated_at"`
}

// Event runtime_events 流事件。
type Event struct {
	Seq        int64          `json:"seq"`
	RuntimeID  string         `json:"runtime_id"`
	EventType  string         `json:"event_type"`
	Payload    map[string]any `json:"payload"`
	OccurredAt string         `json:"occurred_at"`
}

// DiscoveredEntry 发现源行（harness 注册表镜像——app 适配搬运）。
type DiscoveredEntry struct {
	Adapter string
	Host    string
	Distro  string
	Path    string
	Channel string
	Version string
	Login   string
}

// ProbeTarget 探测目标（app 适配映射 harness 登记行）。
type ProbeTarget struct {
	Adapter string
	Host    string
	Distro  string
	Path    string
	Channel string
	Bundle  string
}

// ProbeResult 探测结果：OK = 文件存在且版本探测非空（失败 note 指明
// executable_gone / version_probe_failed——由适配器判定）。
type ProbeResult struct {
	OK      bool
	Version string
	Login   string
	Note    string
}

// Prober 探测端口（app 适配 harness.Reprobe——只读不回写注册表）。
type Prober interface {
	Probe(ctx context.Context, target ProbeTarget) (ProbeResult, error)
}

// FS 工作区目录端口（os 适配 MkdirAll）。
type FS interface {
	EnsureDir(dir string) error
}

// DiscoverySource 发现源（app 适配 harness.List）。
type DiscoverySource func(ctx context.Context) ([]DiscoveredEntry, error)

// Store 持久化端口（SQLite 实现：internal/storage/sqlite；测试件：memstore）。
type Store interface {
	// SyncRuntimes 批量单事务：新行插入 + 既有行仅刷新镜像事实（不动
	// 状态/绑定/发现时间）；discovered 事件由存储在事务内只为真正插入
	// 的行生成（并发首次同步的落败方 upsert 不产生孤儿事件——审校 P2#6）。
	SyncRuntimes(ctx context.Context, now string, rows []Runtime) (created, refreshed int, err error)
	// UpdateRuntime 全行替换 + 事件同事务。
	UpdateRuntime(ctx context.Context, r Runtime, events []Event) error
	RuntimeByID(ctx context.Context, runtimeID string) (Runtime, bool, error)
	ListRuntimes(ctx context.Context) ([]Runtime, error)
	RuntimeEvents(ctx context.Context, runtimeID string) ([]Event, bool, error)
	// CreateEnvironment 建环境；workspace_dir 撞键 ErrEnvironmentDirTaken。
	CreateEnvironment(ctx context.Context, e Environment) error
	EnvironmentByID(ctx context.Context, environmentID string) (Environment, bool, error)
	ListEnvironments(ctx context.Context) ([]Environment, error)
	DefaultEnvironment(ctx context.Context) (Environment, bool, error)
}

// Option 服务选项。
type Option func(*Service)

// WithNow 注入时钟（缺省 UTC RFC3339Nano）。
func WithNow(f func() string) Option { return func(s *Service) { s.now = f } }

// WithNewIDs 注入运行时/环境 ID 生成器（测试定序用）。
func WithNewIDs(runtimeID, environmentID func() string) Option {
	return func(s *Service) { s.newRuntimeID, s.newEnvironmentID = runtimeID, environmentID }
}

// WithDiscoverySource 注入发现源（SyncFromSource 用；harness.List 适配）。
func WithDiscoverySource(src DiscoverySource) Option {
	return func(s *Service) { s.discovery = src }
}

// Service 本地运行时服务。
type Service struct {
	store            Store
	prober           Prober
	fs               FS
	discovery        DiscoverySource
	now              func() string
	newRuntimeID     func() string
	newEnvironmentID func() string
	// locks 按运行时串行化读-探测-写全程（审校 P2#4：迟到的健康检查
	// 不得以旧状态整行覆盖期间完成的准备——撤销 ready 与新绑定）。
	locks sync.Map // runtime_id → *sync.Mutex
}

func (s *Service) lockOf(runtimeID string) *sync.Mutex {
	m, _ := s.locks.LoadOrStore(runtimeID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// New 装配服务。
func New(store Store, prober Prober, fs FS, opts ...Option) *Service {
	s := &Service{
		store: store, prober: prober, fs: fs,
		now:              func() string { return time.Now().UTC().Format(time.RFC3339Nano) },
		newRuntimeID:     func() string { return "rtx_" + randomSuffix() },
		newEnvironmentID: func() string { return "env_" + randomSuffix() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func randomSuffix() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}

// SyncFromSource 从发现源同步（HTTP sync 端点 / 启动扫描后调用）。
func (s *Service) SyncFromSource(ctx context.Context) (created, refreshed int, err error) {
	if s.discovery == nil {
		return 0, 0, fmt.Errorf("runtime: 发现源未装配")
	}
	entries, err := s.discovery(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("runtime: 读发现源失败：%w", err)
	}
	return s.Sync(ctx, entries)
}

// Sync 发现同步（批量单事务；新行 discovered 事件，既有行只刷新事实）。
func (s *Service) Sync(ctx context.Context, entries []DiscoveredEntry) (created, refreshed int, err error) {
	now := s.now()
	existing, err := s.store.ListRuntimes(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("runtime: 读运行时失败：%w", err)
	}
	byKey := make(map[string]Runtime, len(existing))
	for _, r := range existing {
		byKey[keyOf(r.Adapter, r.Host, r.Distro, r.Path)] = r
	}
	var rows []Runtime
	for _, e := range entries {
		key := keyOf(e.Adapter, e.Host, e.Distro, e.Path)
		if old, ok := byKey[key]; ok {
			// 既有行：只刷新镜像事实——状态/绑定/发现时间是本域事实，不被同步覆盖
			old.Channel = e.Channel
			old.Version = e.Version
			old.Login = e.Login
			old.UpdatedAt = now
			rows = append(rows, old)
			continue
		}
		rows = append(rows, Runtime{
			RuntimeID: s.newRuntimeID(),
			Adapter:   e.Adapter, Host: e.Host, Distro: e.Distro, Path: e.Path,
			Channel: e.Channel, Version: e.Version, Login: normalizeLogin(e.Login),
			State: StateDiscovered, DiscoveredAt: now, UpdatedAt: now,
		})
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}
	// discovered 事件由存储在事务内只为真正插入的行生成（并发首次同步
	// 的落败方行 ID 未入库，不产生孤儿事件——审校 P2#6）
	return s.store.SyncRuntimes(ctx, now, rows)
}

func keyOf(adapter, host, distro, path string) string {
	return adapter + "\x1f" + host + "\x1f" + distro + "\x1f" + path
}

func normalizeLogin(login string) string {
	if login == "" {
		return "unknown"
	}
	return login
}

// HealthCheck 实时探测（RFC §9.2 "当前可执行"复核面）。转移：成功且
// unhealthy 且已绑环境 → ready（恢复）；成功且 discovered → 保持（ready
// 须绑环境经 prepare）；失败 → unhealthy（note 可见）。
func (s *Service) HealthCheck(ctx context.Context, runtimeID string) (Runtime, error) {
	mu := s.lockOf(runtimeID)
	mu.Lock()
	defer mu.Unlock() // 读-探测-写全程持锁（审校 P2#4）
	r, ok, err := s.store.RuntimeByID(ctx, runtimeID)
	if err != nil {
		return Runtime{}, fmt.Errorf("runtime: 读运行时失败：%w", err)
	}
	if !ok {
		return Runtime{}, ErrRuntimeNotFound
	}
	result, err := s.prober.Probe(ctx, ProbeTarget{
		Adapter: r.Adapter, Host: r.Host, Distro: r.Distro, Path: r.Path, Channel: r.Channel,
	})
	if err != nil {
		return Runtime{}, fmt.Errorf("runtime: 探测失败：%w", err)
	}
	now := s.now()
	events := []Event{{
		RuntimeID: r.RuntimeID, EventType: EventHealthChecked, OccurredAt: now,
		Payload: map[string]any{"ok": result.OK, "version": result.Version, "login": result.Login, "note": noteOf(result)},
	}}
	r.Version, r.Login = result.Version, normalizeLogin(result.Login)
	r.LastHealthAt, r.LastHealthNote = now, noteOf(result)
	if result.OK {
		// 恢复：unhealthy 且已绑环境 → ready；未绑（含 discovered）不得经探测升 ready
		if r.State == StateUnhealthy && r.EnvironmentID != "" {
			events = append(events, stateChanged(r, StateReady, "probe_recovered", now))
			r.State = StateReady
		}
	} else if r.State != StateUnhealthy {
		events = append(events, stateChanged(r, StateUnhealthy, result.Note, now))
		r.State = StateUnhealthy
	}
	r.UpdatedAt = now
	if err := s.store.UpdateRuntime(ctx, r, events); err != nil {
		return Runtime{}, fmt.Errorf("runtime: 探测结果落库失败：%w", err)
	}
	return r, nil
}

// Prepare 准备：绑定环境（缺省默认；成功才换绑）→ 确保工作区目录 →
// 探测 → ready / unhealthy（结果态是数据不是错误）。
func (s *Service) Prepare(ctx context.Context, runtimeID, environmentID string) (Runtime, error) {
	mu := s.lockOf(runtimeID)
	mu.Lock()
	defer mu.Unlock() // 与健康检查互斥（审校 P2#4）
	r, ok, err := s.store.RuntimeByID(ctx, runtimeID)
	if err != nil {
		return Runtime{}, fmt.Errorf("runtime: 读运行时失败：%w", err)
	}
	if !ok {
		return Runtime{}, ErrRuntimeNotFound
	}
	var env Environment
	if environmentID == "" {
		env, ok, err = s.store.DefaultEnvironment(ctx)
		if err != nil {
			return Runtime{}, fmt.Errorf("runtime: 读默认环境失败：%w", err)
		}
	} else {
		env, ok, err = s.store.EnvironmentByID(ctx, environmentID)
		if err != nil {
			return Runtime{}, fmt.Errorf("runtime: 读环境失败：%w", err)
		}
	}
	if !ok {
		return Runtime{}, ErrEnvironmentNotFound
	}
	if err := s.fs.EnsureDir(env.WorkspaceDir); err != nil {
		return Runtime{}, fmt.Errorf("%w：%s（%v）", ErrWorkspaceUnavailable, env.WorkspaceDir, err)
	}
	result, err := s.prober.Probe(ctx, ProbeTarget{
		Adapter: r.Adapter, Host: r.Host, Distro: r.Distro, Path: r.Path, Channel: r.Channel,
	})
	if err != nil {
		return Runtime{}, fmt.Errorf("runtime: 探测失败：%w", err)
	}
	now := s.now()
	events := []Event{{
		RuntimeID: r.RuntimeID, EventType: EventHealthChecked, OccurredAt: now,
		Payload: map[string]any{"ok": result.OK, "version": result.Version, "login": result.Login, "note": noteOf(result)},
	}}
	r.Version, r.Login = result.Version, normalizeLogin(result.Login)
	r.LastHealthAt, r.LastHealthNote = now, noteOf(result)
	if result.OK {
		// 成功才换绑（失败保持原绑定——ready 语义 = 绑定 + 已探测）
		r.EnvironmentID = env.EnvironmentID
		events = append(events, stateChanged(r, StateReady, "prepared:"+env.EnvironmentID, now))
		r.State = StateReady
	} else if r.State != StateUnhealthy {
		events = append(events, stateChanged(r, StateUnhealthy, result.Note, now))
		r.State = StateUnhealthy
	}
	r.UpdatedAt = now
	if err := s.store.UpdateRuntime(ctx, r, events); err != nil {
		return Runtime{}, fmt.Errorf("runtime: 准备结果落库失败：%w", err)
	}
	return r, nil
}

// CreateEnvironment 建环境（创建时验证/创建工作区目录）。
func (s *Service) CreateEnvironment(ctx context.Context, name, workspaceDir string) (Environment, error) {
	return s.createEnvironment(ctx, name, workspaceDir, false)
}

// EnsureDefaultEnvironment 确保默认环境存在（幂等——已存在即返回；
// 装配启动调用：全库恰一，部分唯一索引兜底并发）。
func (s *Service) EnsureDefaultEnvironment(ctx context.Context, name, workspaceDir string) (Environment, error) {
	if env, ok, err := s.store.DefaultEnvironment(ctx); err != nil {
		return Environment{}, err
	} else if ok {
		return env, nil
	}
	return s.createEnvironment(ctx, name, workspaceDir, true)
}

func (s *Service) createEnvironment(ctx context.Context, name, workspaceDir string, isDefault bool) (Environment, error) {
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
		return Environment{}, fmt.Errorf("%w：名称须 1–64 字（%d）", ErrInvalidEnvironment, n)
	}
	// 规范化目录标识（审校 P2#5）：/dir 与 /dir/. 是同一目录——判重与
	// 保存统一在 Clean 之后；去空白后为空先拒（Clean("") 会得 "."）
	stripped := trimSpaces(workspaceDir)
	if utf8.RuneCountInString(stripped) < 1 {
		return Environment{}, fmt.Errorf("%w：工作区目录不得为空", ErrInvalidEnvironment)
	}
	trimmed := filepath.Clean(stripped)
	if n := utf8.RuneCountInString(trimmed); n < 1 || n > 512 {
		return Environment{}, fmt.Errorf("%w：工作区目录须 1–512 字", ErrInvalidEnvironment)
	}
	if err := s.fs.EnsureDir(trimmed); err != nil {
		return Environment{}, fmt.Errorf("%w：目录不可创建 %s（%v）", ErrInvalidEnvironment, trimmed, err)
	}
	now := s.now()
	env := Environment{
		EnvironmentID: s.newEnvironmentID(), Name: name, WorkspaceDir: trimmed,
		IsDefault: isDefault, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateEnvironment(ctx, env); err != nil {
		return Environment{}, err
	}
	return env, nil
}

// GetEnvironment 环境详情。
func (s *Service) GetEnvironment(ctx context.Context, environmentID string) (Environment, bool, error) {
	return s.store.EnvironmentByID(ctx, environmentID)
}

// ListEnvironments 环境列表。
func (s *Service) ListEnvironments(ctx context.Context) ([]Environment, error) {
	return s.store.ListEnvironments(ctx)
}

// DefaultEnv 默认环境查询（存在性由装配确保）。
func (s *Service) DefaultEnv(ctx context.Context) (Environment, bool, error) {
	return s.store.DefaultEnvironment(ctx)
}

// RuntimeByID 运行时单查。
func (s *Service) RuntimeByID(ctx context.Context, runtimeID string) (Runtime, bool, error) {
	return s.store.RuntimeByID(ctx, runtimeID)
}

// ListRuntimes 运行时列表。
func (s *Service) ListRuntimes(ctx context.Context) ([]Runtime, error) {
	return s.store.ListRuntimes(ctx)
}

// GetRuntime 运行时详情。
func (s *Service) GetRuntime(ctx context.Context, runtimeID string) (Runtime, bool, error) {
	return s.store.RuntimeByID(ctx, runtimeID)
}

// RuntimeEventsByRuntime 事件流（存在性先行——未知运行时 ok=false）。
func (s *Service) RuntimeEventsByRuntime(ctx context.Context, runtimeID string) ([]Event, bool, error) {
	if _, ok, err := s.store.RuntimeByID(ctx, runtimeID); err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, nil
	}
	return s.store.RuntimeEvents(ctx, runtimeID)
}

func stateChanged(r Runtime, to, reason, now string) Event {
	return Event{
		RuntimeID: r.RuntimeID, EventType: EventStateChanged, OccurredAt: now,
		Payload: map[string]any{"from": r.State, "to": to, "reason": reason},
	}
}

func noteOf(result ProbeResult) string {
	if result.OK {
		if result.Note != "" {
			return result.Note
		}
		return "探测成功"
	}
	if result.Note == "" {
		return "version_probe_failed"
	}
	return result.Note
}

func trimSpaces(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
