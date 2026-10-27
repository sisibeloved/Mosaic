// 0.4.1 Agent 版本检测与更新（负责人指令 2026-10-27："本机使用的 CLI 通常不是
// 最新的"——原 0.7.0 生态项前移）。通道按家族分发面实证（2026-10-27 本机）：
//   - claude：npm 分发面 @anthropic-ai/claude-code（2.1.172 实装 vs 最新 2.1.311
//     实测即过期佐证）；官方自更新子命令 `claude update|upgrade`（help 实证
//     "Check for updates and install if available"）——更新优先走自更新（原生
//     安装位 ~/.local/bin 不受 npm 管辖，npm i -g 更新不到它）。
//   - codex：npm @openai/codex（0.153.4 实装）；无自更新子命令——npm i -g。
//   - minimax：npm @minimax-ai/code（0.7.0 实装）；无自更新子命令——npm i -g。
//   - kimi：官方安装器分发（~/.kimi-code/bin，非 npm）——manual 指引。
//   - zcode：桌面版应用内自更新 / CLI 官方 release 重装——manual 指引。
//
// npm 调用形态（实证钉死）：绝对路径直跑 npm 失败（shebang `/usr/bin/env node`
// 需登录 shell 的 nvm PATH）；`sh -lc` 会解析到 Windows 互操作 npm
// （/mnt/d/.../nodejs/npm——dash 登录不读 bash 的 nvm 注入）。一律 `bash -lc`
// （登录环境与人类用户终端一致：nvm 前置、npmrc 镜像生效）；Windows native 面
// 走 `cmd /c npm …`（npm.cmd）。
package harness

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	goruntime "runtime"
)

// ErrNoUpdateChannel 该家族无自动更新通道（kimi/zcode manual 形态）。
var ErrNoUpdateChannel = errors.New("harness: 该 CLI 无自动更新通道（官方安装器/桌面应用分发），请按指引手动更新")

// updateChannelSpec 家族更新通道。
type updateChannelSpec struct {
	npmPackage string // npm 分发面（最新版查询 npm view；空 = 非 npm 分发）
	selfUpdate bool   // 官方自更新子命令（claude update）——更新优先走它
	manualNote string // manual 通道指引（can_update=false 时 UI 呈现）
}

var updateChannels = map[string]updateChannelSpec{
	"claude":  {npmPackage: "@anthropic-ai/claude-code", selfUpdate: true},
	"codex":   {npmPackage: "@openai/codex"},
	"minimax": {npmPackage: "@minimax-ai/code"},
	"kimi": {
		manualNote: "Kimi Code 由官方安装器分发（~/.kimi-code/bin）：请用官方安装命令重装最新版，登录态与配置保留",
	},
	"zcode": {
		manualNote: "ZCode 桌面版请在应用内检查更新（自动升级）；独立 CLI（release tarball）请重跑官方安装脚本",
	},
}

func (s updateChannelSpec) channel() string {
	switch {
	case s.selfUpdate:
		return "self-update"
	case s.npmPackage != "":
		return "npm"
	default:
		return "manual"
	}
}

// UpdateCheck 版本检测结果（GET update-check 响应体）。
type UpdateCheck struct {
	Adapter        string `json:"adapter"`
	CurrentVersion string `json:"current_version"`          // 注册表探测原文（如 "2.1.172 (Claude Code)"）
	CurrentSemver  string `json:"current_semver,omitempty"` // 原文中解析出的 x.y.z（解析不出为空）
	Latest         string `json:"latest,omitempty"`         // 上游最新版（检测失败为空）
	UpToDate       bool   `json:"up_to_date"`
	CanCheck       bool   `json:"can_check"`  // 有 npm 分发面（可查最新版）
	CanUpdate      bool   `json:"can_update"` // 有自动更新通道（npm / 自更新）
	Channel        string `json:"channel"`    // npm | self-update | manual
	Note           string `json:"note,omitempty"`
	CheckedAt      string `json:"checked_at"` // RFC3339
}

// UpdateRun 更新执行结果（POST update 响应体）。
type UpdateRun struct {
	Status     string `json:"status"` // ok
	NewVersion string `json:"new_version"`
	OutputTail string `json:"output_tail,omitempty"` // 更新命令输出尾（排障可见）
}

// cachedLatest 最新版查询缓存（npm view 是网络往返——设置页展开不重复打注册表；
// 镜像源首查慢启动实测 ~10s，命中缓存秒回）。
type cachedLatest struct {
	version string
	at      time.Time
}

const updateCheckTTL = 30 * time.Minute

// latestCacheKey 缓存键 = 运行面 + 包名（同包不同发行版/宿主的 npm 环境独立查询）。
func latestCacheKey(rt Runtime, distro, pkg string) string {
	return string(rt) + "|" + distro + "|" + pkg
}

// CheckUpdate 对照上游最新版（结果缓存 30 分钟；npm view 网络往返经登录 shell——
// 见包注释实证）。检测失败不缓存（网络瞬态不该钉死 30 分钟），Note 携原因。
func (r *Registry) CheckUpdate(ctx context.Context, id string, runner Runner) (UpdateCheck, error) {
	exe, err := r.executableOf(id)
	if err != nil {
		return UpdateCheck{}, err
	}
	spec := updateChannels[exe.Adapter]
	now := time.Now()
	check := UpdateCheck{
		Adapter:        exe.Adapter,
		CurrentVersion: exe.Version,
		Channel:        spec.channel(),
		CanCheck:       spec.npmPackage != "",
		CanUpdate:      spec.selfUpdate || spec.npmPackage != "",
		CheckedAt:      now.UTC().Format(time.RFC3339),
	}
	check.CurrentSemver = extractSemver(exe.Version)
	if spec.manualNote != "" {
		check.Note = spec.manualNote
	}
	if !check.CanCheck {
		check.UpToDate = true // 无检测面不标红（manual 通道由 Note 指引）
		return check, nil
	}
	if runner == nil {
		check.Note = "无探测执行面（ProbeRunner 未配置）"
		return check, nil
	}

	key := latestCacheKey(Runtime(exe.Runtime), exe.Distro, spec.npmPackage)
	r.mu.Lock()
	cached, ok := r.latestCache[key]
	r.mu.Unlock()
	if ok && now.Sub(cached.at) < updateCheckTTL {
		check.Latest = cached.version
		check.UpToDate = compareSemver(check.CurrentSemver, check.Latest) >= 0
		return check, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, code, err := runner.Run(ctx, Runtime(exe.Runtime), exe.Distro,
		npmArgs(Runtime(exe.Runtime), "view "+spec.npmPackage+" version"))
	if err != nil || code != 0 {
		check.Note = fmt.Sprintf("最新版查询失败（npm view 退出码 %d）：%s", code, firstLine(out))
		return check, nil // 环境态：返回可呈现的部分结果，不 5xx
	}
	latest := strings.TrimSpace(lastLine(out))
	if !semverLike(latest) {
		check.Note = "最新版查询输出不可解析：" + firstLine(out)
		return check, nil
	}
	r.mu.Lock()
	if r.latestCache == nil {
		r.latestCache = map[string]cachedLatest{}
	}
	r.latestCache[key] = cachedLatest{version: latest, at: now}
	r.mu.Unlock()
	check.Latest = latest
	check.UpToDate = compareSemver(check.CurrentSemver, latest) >= 0
	return check, nil
}

// RunUpdate 执行更新并回填注册表版本（扫描只在启动时跑——更新后的版本探测由
// 本方法显式触发，RefreshVersion 持久化）。更新中不持锁（npm i -g 可达分钟级）。
// 无自动通道（manual）返回 ErrNoUpdateChannel（HTTP 409 面）。
func (r *Registry) RunUpdate(ctx context.Context, id string, runner Runner) (UpdateRun, error) {
	exe, err := r.executableOf(id)
	if err != nil {
		return UpdateRun{}, err
	}
	if runner == nil {
		return UpdateRun{}, errors.New("harness: 无探测执行面（ProbeRunner 未配置）")
	}
	spec := updateChannels[exe.Adapter]
	if spec.selfUpdate {
		// 官方自更新（claude update）：登录 shell 包装（与用户终端同环境）。
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		out, code, err := runner.Run(ctx, Runtime(exe.Runtime), exe.Distro,
			loginShellArgs(Runtime(exe.Runtime), shellQuote(exe.Path)+" update"))
		return r.collectUpdate(ctx, runner, exe, out, code, err)
	}
	if spec.npmPackage != "" {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		out, code, err := runner.Run(ctx, Runtime(exe.Runtime), exe.Distro,
			npmArgs(Runtime(exe.Runtime), "install -g "+spec.npmPackage+"@latest"))
		return r.collectUpdate(ctx, runner, exe, out, code, err)
	}
	return UpdateRun{}, fmt.Errorf("%w：%s", ErrNoUpdateChannel, spec.manualNote)
}

// collectUpdate 更新命令收口：失败显式报错（携输出尾）；成功重探版本并持久化。
func (r *Registry) collectUpdate(ctx context.Context, runner Runner, exe Executable, out string, code int, err error) (UpdateRun, error) {
	if err != nil {
		return UpdateRun{}, fmt.Errorf("harness: 更新命令执行失败：%w", err)
	}
	if code != 0 {
		return UpdateRun{}, fmt.Errorf("harness: 更新命令退出码 %d：%s", code, lastLines(out, 5))
	}
	// 版本回探：与启动扫描同一探测口径（[path, --version]）。
	vctx, vcancel := context.WithTimeout(ctx, 30*time.Second)
	defer vcancel()
	vout, vcode, verr := runner.Run(vctx, Runtime(exe.Runtime), exe.Distro, []string{exe.Path, "--version"})
	if verr != nil || vcode != 0 {
		return UpdateRun{}, fmt.Errorf("harness: 更新后版本探测失败（退出码 %d）：%s", vcode, firstLine(vout))
	}
	version := strings.TrimSpace(firstLine(vout))
	if err := r.RefreshVersion(exe.ID, version); err != nil {
		return UpdateRun{}, err
	}
	return UpdateRun{Status: "ok", NewVersion: version, OutputTail: lastLines(out, 3)}, nil
}

// RefreshVersion 更新注册表版本字段并持久化（更新命令收口用；镜像 UpdateRuntime
// 的锁内变更 + saveLocked 纪律）。
func (r *Registry) RefreshVersion(id, version string) error {
	if len(version) > 120 {
		return fmt.Errorf("%w: version 超 120 字", ErrInvalidEntry)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.exes {
		if r.exes[i].ID == id {
			r.exes[i].Version = version
			return r.saveLocked()
		}
	}
	return ErrNotFound
}

// executableOf 按 ID 取注册项副本（锁内拷出，后续长操作不持锁）。
func (r *Registry) executableOf(id string) (Executable, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.exes {
		if r.exes[i].ID == id {
			return r.exes[i], nil
		}
	}
	return Executable{}, ErrNotFound
}

// npmArgs npm 调用 argv（按运行面）：WSL/Linux/macOS 走 bash -lc（登录 shell 的
// nvm PATH 与 npmrc 镜像——包注释实证：绝对路径 npm 缺 node、sh -lc 解析到
// Windows 互操作 npm）；Windows native 走 cmd /c（npm.cmd）。
func npmArgs(rt Runtime, sub string) []string {
	return loginShellArgs(rt, "npm "+sub)
}

func loginShellArgs(rt Runtime, cmd string) []string {
	if rt == RuntimeNative && goruntimeOS == "windows" {
		return []string{"cmd", "/c", cmd}
	}
	return []string{"bash", "-lc", cmd}
}

// goruntimeOS 单测可替换面（loginShellArgs 的 Windows native 分支按宿主 OS）。
var goruntimeOS = goruntime.GOOS

// ---- semver 最小面（不引外部依赖；登记项版本串异构原文先抽取再比对）----

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// extractSemver 从探测原文抽取首个 x.y.z（"2.1.172 (Claude Code)" → "2.1.172"；
// "codex-cli 0.153.4" → "0.153.4"；抽取不出返回空——比对按无当前版本处理）。
func extractSemver(raw string) string {
	return semverRe.FindString(raw)
}

// semverLike 形状校验（npm view 输出行）。
func semverLike(s string) bool {
	return s != "" && semverRe.FindString(s) == s
}

// compareSemver 语义化版本比较（a>b 返回 1；无法解析的侧视为更小——保守提示
// 有更新而非静默；预发布后缀（-beta）按剥离处理，精确预发布序不展开——更新
// 徽标场景够用，偏差方向为多提示一次更新）。
func compareSemver(a, b string) int {
	if a == b {
		return 0
	}
	pa, pb := semverParts(a), semverParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func semverParts(s string) [3]int {
	s = semverRe.FindString(s) // 无匹配 = [0,0,0]（视为最小）
	var out [3]int
	if s == "" {
		return out
	}
	for i, p := range strings.SplitN(s, ".", 3) {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out
}

// ---- 输出行小工具（lastLine/lastLines 为本文件新增；firstLine 复用 registry.go）----

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
