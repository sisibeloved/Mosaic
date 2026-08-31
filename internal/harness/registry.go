// Package harness：宿主层可执行程序管理（RFC-0002 双层 Profile 管理的宿主面）。
// 发现（自动扫描 native + WSL）→ 登录态探测 → 摘要登记 → 手动配置共存 → 启用门控。
// 登录态是硬门：未登录的可执行程序不得启用为 agent 座位。
package harness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runtime 可执行程序的运行面。
type Runtime string

const (
	RuntimeNative Runtime = "native" // 宿主直接执行
	RuntimeWSL    Runtime = "wsl"    // Windows 宿主经 wsl.exe 在发行版内执行
)

// 登录态。
const (
	LoginLoggedIn  = "logged_in"
	LoginLoggedOut = "logged_out"
	LoginUnknown   = "unknown"
)

// 登记来源。
const (
	SourceAuto   = "auto_scan"
	SourceManual = "manual"
)

// Executable 宿主登记的一个可执行程序（tenant 侧 Profile 从中选取实例化）。
// 同一家族可多实例共存（多安装位置 / 桌面应用 bundled 面），实例间配置与会话独立，
// 以 Channel 区分渠道、Priority 表达家族裁定优先级（负责人 2026-08-31）。
type Executable struct {
	ID           string `json:"id"`
	Adapter      string `json:"adapter"` // codex | kimi | zcode
	Runtime      string `json:"runtime"` // native | wsl
	Distro       string `json:"distro,omitempty"`
	Path         string `json:"path"`
	Version      string `json:"version,omitempty"`
	Digest       string `json:"digest,omitempty"` // 二进制摘要（RFC-0002 宿主层登记）
	Login        string `json:"login_state"`      // logged_in | logged_out | unknown
	Source       string `json:"source"`           // auto_scan | manual
	Channel      string `json:"channel"`          // cli | app:codex-desktop | app:kimi-work（空值按 cli 处理）
	Priority     int    `json:"priority"`         // PriorityFor 计算；数值小者优先
	DiscoveredAt string `json:"discovered_at"`
	Enabled      bool   `json:"enabled"`
}

// Registry 域错误。
var (
	// ErrLoginRequired 启用被登录门控拒绝。
	ErrLoginRequired = fmt.Errorf("harness: 未登录，不可启用（先完成 CLI 登录）")
	// ErrNotFound 登记 ID 不存在。
	ErrNotFound = fmt.Errorf("harness: 可执行程序不存在")
	// ErrInvalidEntry 手动登记项校验失败。
	ErrInvalidEntry = fmt.Errorf("harness: 登记项不合法")
)

// Runner 探测命令的执行面（native 直执行；wsl 经 wsl.exe 包装）。UT 用 fake。
type Runner interface {
	LookPath(ctx context.Context, runtime Runtime, distro, binary string) (string, bool)
	Run(ctx context.Context, runtime Runtime, distro string, args []string) (stdout string, exitCode int, err error)
	Home(ctx context.Context, runtime Runtime, distro string) string
	Exists(ctx context.Context, runtime Runtime, distro, path string) bool
	Digest(ctx context.Context, runtime Runtime, distro, path string) (string, error)
	WSLDistros(ctx context.Context) []string
	// Glob 按通配模式展开绝对路径（native 用 filepath.Glob；wsl 用 shell glob）。
	// 版本管理器（nvm/fnm/volta）把 CLI 装在版本化目录——登录 shell 之外唯一可靠的发现方式。
	Glob(ctx context.Context, runtime Runtime, distro, pattern string) []string
	// RunWithDir 在把 binDir 前置到 PATH 后执行（nvm 布局：CLI 是 #!/usr/bin/env node
	// 脚本，其 node 运行时在同目录——探测必须带上）。
	RunWithDir(ctx context.Context, runtime Runtime, distro, binDir string, args []string) (stdout string, exitCode int, err error)
}

// ProbeSpec 单个 adapter 的探测规格（来源：Harness 调研 2026-08-25 实证）。
type ProbeSpec struct {
	Adapter        string
	Binary         string
	VersionArgs    []string
	LoginCmd       []string // 命令式登录探测（codex）
	LoginOKPattern string   // 输出含此串且 exit 0 → logged_in
	CredFile       string   // 凭证文件式探测（kimi）：相对家目录，存在即 logged_in
	// KnownDirGlobs：常见安装位置（相对家目录的 glob 模式，展开后拼 /binary）。
	// 文件系统事实驱动，不依赖 shell 初始化——nvm/fnm/volta 等版本管理器的唯一可靠发现面。
	KnownDirGlobs []string
	// AppGlobs：桌面应用内 bundled CLI 的发现位置（绝对 glob，仅 native 面展开）。
	// 命中实例带渠道标签，参与家族优先级（PriorityFor）。
	AppGlobs []AppGlob
}

// ScanOptions 扫描选项。
type ScanOptions struct {
	IncludeWSL   bool          // 扫描 WSL 发行版（Windows 宿主自动开启；他处默认关）
	ProbeTimeout time.Duration // 单探测命令超时（默认 5s）
}

// probeTimeout 归一化超时。
func (o ScanOptions) probeTimeout() time.Duration {
	if o.ProbeTimeout <= 0 {
		return 5 * time.Second
	}
	return o.ProbeTimeout
}

// exeID 稳定 ID：adapter@runtime[:distro]:path（同程序重复扫描幂等 upsert）。
func exeID(e Executable) string {
	host := e.Runtime
	if e.Distro != "" {
		host += ":" + e.Distro
	}
	return fmt.Sprintf("%s@%s:%s", e.Adapter, host, e.Path)
}

// probeLogin 探测登录态：命令式优先，其次凭证文件，皆无则 unknown。
func probeLogin(ctx context.Context, r Runner, spec ProbeSpec, runtime Runtime, distro, binDir, path string) string {
	if len(spec.LoginCmd) > 0 && spec.LoginOKPattern != "" {
		ctx, cancel := context.WithTimeout(ctx, ScanOptions{}.probeTimeout())
		defer cancel()
		args := append([]string{path}, spec.LoginCmd...)
		out, code, err := r.RunWithDir(ctx, runtime, distro, binDir, args)
		if err == nil && code == 0 && containsFold(out, spec.LoginOKPattern) {
			return LoginLoggedIn
		}
		if err == nil {
			return LoginLoggedOut
		}
		return LoginUnknown
	}
	if spec.CredFile != "" {
		home := r.Home(ctx, runtime, distro)
		if home != "" && r.Exists(ctx, runtime, distro, home+"/"+spec.CredFile) {
			return LoginLoggedIn
		}
		return LoginLoggedOut
	}
	return LoginUnknown
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOfFold(s, sub) >= 0)
}

func indexOfFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// probeExecutable 对已知路径的可执行程序做完整探测（版本/登录/摘要）。
// 探测命令在可执行文件所在目录前置 PATH 的环境下运行（版本管理器布局的 node/CLI 同目录）。
func probeExecutable(ctx context.Context, r Runner, spec ProbeSpec, runtime Runtime, distro, path string) Executable {
	binDir := parentDir(path)
	exe := Executable{
		Adapter:      spec.Adapter,
		Runtime:      string(runtime),
		Distro:       distro,
		Path:         path,
		Login:        probeLogin(ctx, r, spec, runtime, distro, binDir, path),
		DiscoveredAt: time.Now().UTC().Format(time.RFC3339),
	}
	if len(spec.VersionArgs) > 0 {
		vctx, cancel := context.WithTimeout(ctx, ScanOptions{}.probeTimeout())
		defer cancel()
		args := append([]string{path}, spec.VersionArgs...)
		if out, _, err := r.RunWithDir(vctx, runtime, distro, binDir, args); err == nil {
			exe.Version = firstLine(out)
		}
	}
	if d, err := r.Digest(ctx, runtime, distro, path); err == nil {
		exe.Digest = d
	}
	exe.ID = exeID(exe)
	return exe
}

func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return ""
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return trimSpace(s[:i])
		}
	}
	return trimSpace(s)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r' || s[end-1] == '\n') {
		end--
	}
	return s[start:end]
}

// Registry 持久化登记表（JSON 文件）。
type Registry struct {
	mu   sync.Mutex
	path string
	exes []Executable
}

// LoadOrCreate 从 path 装载（不存在则建空表）。
func LoadOrCreate(path string) (*Registry, error) {
	reg := &Registry{path: path}
	raw, err := osReadFile(path)
	if err != nil {
		if osIsNotExist(err) {
			return reg, nil
		}
		return nil, fmt.Errorf("harness: read registry: %w", err)
	}
	var doc struct {
		Executables []Executable `json:"executables"`
	}
	if err := jsonUnmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("harness: parse registry: %w", err)
	}
	reg.exes = doc.Executables
	return reg, nil
}

// saveLocked 原子落盘（tmp + rename）：半截写的注册表会让下次启动直接失败——
// 崩溃窗口内要么旧表要么新表，绝不留截断 JSON。
func (r *Registry) saveLocked() error {
	raw, err := jsonMarshalIndent(struct {
		Executables []Executable `json:"executables"`
	}{r.exes})
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := osWriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return osRename(tmp, r.path)
}

// sortByPriority 规范排序：(adapter, priority, path)——设置页展示顺序与座位顺序
// 共用同一事实源；家族优先级高者在前，同优先级按路径定序（确定性）。
func sortByPriority(exes []Executable) {
	sort.SliceStable(exes, func(i, j int) bool {
		if exes[i].Adapter != exes[j].Adapter {
			return exes[i].Adapter < exes[j].Adapter
		}
		if exes[i].Priority != exes[j].Priority {
			return exes[i].Priority < exes[j].Priority
		}
		return exes[i].Path < exes[j].Path
	})
}

// List 返回登记快照（副本，按 adapter/优先级/路径排序）。
func (r *Registry) List() []Executable {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Executable, len(r.exes))
	copy(out, r.exes)
	sortByPriority(out)
	return out
}

// upsertLocked 按 ID 合并：auto 刷新探测字段；manual 保留来源与渠道覆盖，但刷新
// 登录/版本/摘要。优先级每次入库重算（PriorityFor），家族裁定调整即刻生效。
func (r *Registry) upsertLocked(exe Executable) {
	if exe.Channel == "" {
		exe.Channel = ChannelCLI
	}
	for i := range r.exes {
		if r.exes[i].ID == exe.ID {
			prev := r.exes[i]
			exe.Enabled = prev.Enabled // 启用状态跨扫描保留（登录门控在 SetEnabled 把关）
			if prev.Source == SourceManual {
				exe.Source = SourceManual
				if prev.Channel != "" {
					exe.Channel = prev.Channel // 手动项的渠道覆盖不被扫描冲掉
				}
			}
			exe.Priority = PriorityFor(exe.Adapter, exe.Channel)
			r.exes[i] = exe
			return
		}
	}
	exe.Priority = PriorityFor(exe.Adapter, exe.Channel)
	r.exes = append(r.exes, exe)
}

// Scan 自动扫描：native 全部探测规格 + （开启时）各 WSL 发行版。
// 发现顺序：PATH 解析 → 已知安装位置 glob → 桌面应用 bundled 位置（native 面）；
// 同规格枚举全部实例（多实例并存，配置/会话独立——负责人裁定 2026-08-31）。
// 扫描失败的单项跳过（探测命令超时/缺失不是致命错误）。
func (r *Registry) Scan(ctx context.Context, runner Runner, probes []ProbeSpec, opts ScanOptions) error {
	scanRuntime := func(runtime Runtime, distro string) {
		home := runner.Home(ctx, runtime, distro)
		for _, spec := range probes {
			for _, exe := range discoverExecutables(ctx, runner, spec, runtime, distro, home) {
				exe.Source = SourceAuto
				r.mu.Lock()
				r.upsertLocked(exe)
				r.mu.Unlock()
			}
		}
	}

	scanRuntime(RuntimeNative, "")
	if opts.IncludeWSL {
		for _, distro := range runner.WSLDistros(ctx) {
			scanRuntime(RuntimeWSL, distro)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
}

// discoverExecutables 单规格枚举全部实例：PATH + 已知目录 glob +（native 面）App 位置；
// 按路径去重，命中即完整探测。渠道：PATH/目录发现为 cli，App 位置带各自渠道标签。
func discoverExecutables(ctx context.Context, runner Runner, spec ProbeSpec, runtime Runtime, distro, home string) []Executable {
	var out []Executable
	seen := map[string]bool{}
	add := func(path, channel string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		exe := probeExecutable(ctx, runner, spec, runtime, distro, path)
		exe.Channel = channel
		out = append(out, exe)
	}

	if path, ok := runner.LookPath(ctx, runtime, distro, spec.Binary); ok {
		add(path, ChannelCLI)
	}
	if home != "" {
		for _, pattern := range spec.KnownDirGlobs {
			for _, dir := range runner.Glob(ctx, runtime, distro, home+"/"+pattern) {
				if candidate, ok := firstExistingBinary(ctx, runner, runtime, distro, dir, spec.Binary); ok {
					add(candidate, ChannelCLI)
				}
			}
		}
	}
	if runtime == RuntimeNative {
		// 桌面应用 bundled 面只在宿主原生运行面存在（WSL 发行版内无 Windows 应用）
		for _, ag := range spec.AppGlobs {
			for _, pattern := range ag.Patterns {
				for _, dir := range runner.Glob(ctx, runtime, distro, expandAppPattern(pattern, home)) {
					if candidate, ok := firstExistingBinary(ctx, runner, runtime, distro, dir, spec.Binary); ok {
						add(candidate, ag.Channel)
					}
				}
			}
		}
	}
	return out
}

// firstExistingBinary 目录内二进制存在性探测：裸名优先，其次 .exe（Windows 应用/安装
// 形态的真实可执行文件；npm 目录的扩展名 shim 维持既有 LookPath 行为不改）。
func firstExistingBinary(ctx context.Context, runner Runner, runtime Runtime, distro, dir, binary string) (string, bool) {
	for _, name := range []string{binary, binary + ".exe"} {
		candidate := dir + "/" + name
		if runner.Exists(ctx, runtime, distro, candidate) {
			return candidate, true
		}
	}
	return "", false
}

// expandAppPattern 展开 App 位置占位：{LOCALAPPDATA} 按宿主家目录推导，{PROGRAMFILES}
// 取 Windows 约定常量（被企业策略改写的情形归手动登记——探测失败不致命）。
func expandAppPattern(pattern, home string) string {
	if strings.Contains(pattern, "{LOCALAPPDATA}") {
		pattern = strings.ReplaceAll(pattern, "{LOCALAPPDATA}", strings.TrimSuffix(home, "/")+"/AppData/Local")
	}
	if strings.Contains(pattern, "{PROGRAMFILES}") {
		pattern = strings.ReplaceAll(pattern, "{PROGRAMFILES}", "C:/Program Files")
	}
	return pattern
}

// AddManual 手动登记（负责人要求 2）：校验必填与已知家族，随后做完整探测。
// entry.Channel 可显式指定实例渠道（App 形态在自动扫描位置未实证前的进入路径），
// 空值按 cli；非法渠道串拒绝。
func (r *Registry) AddManual(ctx context.Context, runner Runner, entry Executable) error {
	if entry.Adapter == "" || entry.Runtime == "" || entry.Path == "" {
		return fmt.Errorf("%w: adapter/runtime/path 必填", ErrInvalidEntry)
	}
	if entry.Runtime != string(RuntimeNative) && entry.Runtime != string(RuntimeWSL) {
		return fmt.Errorf("%w: runtime 必须为 native|wsl", ErrInvalidEntry)
	}
	channel := entry.Channel
	if channel == "" {
		channel = ChannelCLI
	}
	if !validChannel(channel) {
		return fmt.Errorf("%w: channel 必须为 cli 或 app:<name>，got %q", ErrInvalidEntry, entry.Channel)
	}
	var spec *ProbeSpec
	for i := range BuiltinProbes {
		if BuiltinProbes[i].Adapter == entry.Adapter {
			spec = &BuiltinProbes[i]
		}
	}
	if spec == nil {
		return fmt.Errorf("%w: 未知 adapter %q", ErrInvalidEntry, entry.Adapter)
	}
	exe := probeExecutable(ctx, runner, *spec, Runtime(entry.Runtime), entry.Distro, entry.Path)
	exe.Source = SourceManual
	exe.Channel = channel
	if entry.Version != "" {
		exe.Version = entry.Version // 手动项允许显式版本覆盖（探测失败时）
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upsertLocked(exe)
	return r.saveLocked()
}

// validChannel 渠道串形态：cli 或 app:<小写字母/数字/连字符>。
func validChannel(c string) bool {
	if c == ChannelCLI {
		return true
	}
	rest, ok := strings.CutPrefix(c, "app:")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// SetEnabled 启用门控（负责人要求 3）：仅 logged_in 可启用；禁用不设限。
func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.exes {
		if r.exes[i].ID == id {
			if enabled && r.exes[i].Login != LoginLoggedIn {
				return fmt.Errorf("%w: %s（%s）", ErrLoginRequired, r.exes[i].Adapter, r.exes[i].Path)
			}
			r.exes[i].Enabled = enabled
			return r.saveLocked()
		}
	}
	return ErrNotFound
}

// EnabledList 已启用的可执行程序（agent 座位候选；排序口径同 List——
// 优先级高者优先入座，同分平局的确定性由路径序保证）。
func (r *Registry) EnabledList() []Executable {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Executable
	for _, e := range r.exes {
		if e.Enabled {
			out = append(out, e)
		}
	}
	sortByPriority(out)
	return out
}
