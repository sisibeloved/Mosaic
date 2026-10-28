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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	goruntime "runtime"
)

// ErrNoUpdateChannel 该家族无自动更新通道（自建打包/官方安装器/桌面应用分发）。
var ErrNoUpdateChannel = errors.New("harness: 该 CLI 无自动更新通道（自建打包/官方安装器/桌面应用分发），请按指引手动更新")

// updateChannelSpec 家族更新通道。
type updateChannelSpec struct {
	npmPackage string // npm 分发面（最新版查询 npm view；空 = 非 npm 分发）
	// gitRepo 上游源码仓（owner/repo——最新版查询 git ls-remote --tags；自建
	// 分发面的检测通道）。zcode 实证（2026-10-27 负责人问询轮）：官方 release
	// 仅桌面安装包（mac dmg / win exe，v3.14.3），无 Linux CLI 产物——本机 CLI
	// 为基于官方源码自建打包（负责人口径），检测面只能对齐源码 tag；更新面
	// manual（自建产物 Mosaic 不做替换）。
	gitRepo string
	// selfUpdateArgs 官方自更新子命令（更新优先走它——原生安装位/官方安装器
	// 分发不受本机 npm 管辖）：claude ["update"]；kimi ["upgrade","--yes"]
	//（2026-10-27 验收反馈轮实证：kimi 无 npm 包 @kimi-code/cli 不存在，但
	// 官方子命令 upgrade|update 在席，-y 免确认无头执行）。
	selfUpdateArgs []string
	manualNote     string // 无检测面/无自动通道时的指引（check note 呈现）
}

var updateChannels = map[string]updateChannelSpec{
	"claude":  {npmPackage: "@anthropic-ai/claude-code", selfUpdateArgs: []string{"update"}},
	"codex":   {npmPackage: "@openai/codex"},
	"minimax": {npmPackage: "@minimax-ai/code"},
	"kimi": {
		selfUpdateArgs: []string{"upgrade", "--yes"},
		manualNote:     "Kimi Code 官方安装器分发（~/.kimi-code/bin，无 npm 包）——无独立最新版查询面；「更新到最新」经 kimi upgrade 幂等执行（已是最新则无动作），登录态与配置保留",
	},
	"zcode": {
		gitRepo:    "zai-org/ZCode",
		manualNote: "ZCode CLI 为基于官方源码自建打包（官方仅发布桌面安装包，无 Linux CLI 产物）——请拉取上游源码走自有打包管线重装；桌面版请在应用内检查更新（自动升级）",
	},
}

func (s updateChannelSpec) channel() string {
	switch {
	case len(s.selfUpdateArgs) > 0:
		return "self-update"
	case s.npmPackage != "":
		return "npm"
	case s.gitRepo != "":
		return "releases"
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
	// Comparable 版本方案可比（两侧 semver 同 major 系——zcode 实录：安装侧
	// 0.16.9 是桌面内核版方案、上游源码 tag 3.x，跨方案比对必恒"有更新"误报）。
	// 不可比时 up_to_date 恒 true（不标红），note 说明方案差异，仅展示 latest。
	Comparable bool   `json:"comparable"`
	CanCheck   bool   `json:"can_check"`  // 有检测面（npm 分发 / 源码 tag）
	CanUpdate  bool   `json:"can_update"` // 有自动更新通道（npm / 自更新）
	Channel    string `json:"channel"`    // npm | self-update | releases | manual
	Note       string `json:"note,omitempty"`
	CheckedAt  string `json:"checked_at"` // RFC3339
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

// CheckUpdate 对照上游最新版（结果缓存 30 分钟；npm view / git ls-remote 网络
// 往返经登录 shell——见包注释实证）。检测失败不缓存（网络瞬态不该钉死 30
// 分钟），Note 携原因。
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
		CanCheck:       spec.npmPackage != "" || spec.gitRepo != "",
		CanUpdate:      len(spec.selfUpdateArgs) > 0 || spec.npmPackage != "",
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

	// 检测面分发：npm 分发 → npm view；源码仓家族（zcode）→ 桌面产品更新源
	// 优先（app-update.yml 声明的自建 feed），不可达回退 git ls-remote --tags。
	latest, srcNote := "", ""
	var cacheKey string
	if spec.npmPackage != "" {
		cacheKey = latestCacheKey(Runtime(exe.Runtime), exe.Distro, spec.npmPackage)
		r.mu.Lock()
		cached, ok := r.latestCache[cacheKey]
		r.mu.Unlock()
		if ok && now.Sub(cached.at) < updateCheckTTL {
			check.Latest = cached.version
			check.UpToDate, check.Comparable = verdict(check.CurrentSemver, cached.version)
			return check, nil
		}
		qctx, qcancel := context.WithTimeout(ctx, 60*time.Second)
		defer qcancel()
		out, code, err := runner.Run(qctx, Runtime(exe.Runtime), exe.Distro,
			loginShellArgs(Runtime(exe.Runtime), "npm view "+spec.npmPackage+" version"))
		if err != nil || code != 0 {
			check.Note = fmt.Sprintf("最新版查询失败（npm view 退出码 %d）：%s", code, firstLine(out))
			return check, nil // 环境态：返回可呈现的部分结果，不 5xx
		}
		latest = strings.TrimSpace(lastLine(out))
		if !semverLike(latest) {
			check.Note = "最新版查询输出不可解析：" + firstLine(out)
			return check, nil
		}
		srcNote = ""
	} else {
		var qerr error
		latest, cacheKey, srcNote, qerr = r.gitRepoLatest(ctx, runner, exe, spec, now)
		if qerr != nil {
			check.Note = qerr.Error()
			return check, nil
		}
	}
	r.mu.Lock()
	if r.latestCache == nil {
		r.latestCache = map[string]cachedLatest{}
	}
	r.latestCache[cacheKey] = cachedLatest{version: latest, at: now}
	r.mu.Unlock()
	check.Latest = latest
	check.UpToDate, check.Comparable = verdict(check.CurrentSemver, latest)
	if !check.Comparable {
		scheme := ""
		if check.CurrentSemver != "" {
			scheme = "——安装侧 " + majorOf(check.CurrentSemver) + ".x 系 vs 上游 " + majorOf(latest) + ".x 系"
		}
		check.Note = strings.TrimSpace("上游最新（" + strings.TrimSpace(srcNote) + "）：" + latest +
			"（版本方案不同" + scheme + "，不可比不判更新）。" + check.Note)
	} else if srcNote != "" {
		check.Note = strings.TrimSpace("版本信号来源：" + srcNote + "。" + check.Note)
	}
	return check, nil
}

// gitRepoLatest 自建分发家族（zcode）的版本信号：两层取优。
//   - 优先：桌面产品更新源——注册表内同家族桌面渠道（app:zcode-desktop，native
//     Windows 安装）声明的 <install>/resources/app-update.yml 的 url（electron-
//     updater generic feed），GET <url>/latest.yml 取 version。负责人问询轮实证
//     （2026-10-27）：本机桌面已 3.16.3 而开源仓库 releases 仅 v3.14.3 / tags
//     仅 v3.15.1——产品通道领先于 OSS 元数据，源码 tag 单独作 latest 会拿"历史
//     数据"。feed 是自建 infra（实录 http://localhost:8081，离线常态）——不可
//     达时如实回退。
//   - 回退：git ls-remote --tags 最新 semver tag，信号来源在 note 标注（产品
//     通道可能领先）。
//
// 返回 (latest, 缓存键, 信号来源说明, error)。缓存命中直接返回（来源沿用）。
func (r *Registry) gitRepoLatest(ctx context.Context, runner Runner, exe Executable, spec updateChannelSpec, now time.Time) (latest, cacheKey, srcNote string, err error) {
	// 第一层：桌面 feed。
	if url, ok := r.desktopFeedURL(); ok {
		cacheKey = latestCacheKey(RuntimeNative, "", "feed:"+url)
		r.mu.Lock()
		cached, hit := r.latestCache[cacheKey]
		r.mu.Unlock()
		if hit && now.Sub(cached.at) < updateCheckTTL {
			return cached.version, cacheKey, "产品更新源（桌面 app-update.yml → " + url + "，缓存）", nil
		}
		if v, ferr := fetchFeedVersion(ctx, url); ferr == nil {
			return v, cacheKey, "产品更新源（桌面 app-update.yml → " + url + "）", nil
		}
		// feed 不可达：回退源码 tag，note 说明产品通道情况。
		srcNote = "开源仓库源码 tag——产品更新源（" + url + "）当前不可达，自建分发通道可能领先此值"
	} else {
		srcNote = "开源仓库源码 tag（未发现桌面版安装，产品更新源不可知）"
	}

	// 第二层：git tags（缓存键切回 tag 面——feed 键只缓存 feed 结果，不混装）。
	key := latestCacheKey(Runtime(exe.Runtime), exe.Distro, "git:"+spec.gitRepo)
	cacheKey = key
	r.mu.Lock()
	cached, hit := r.latestCache[key]
	r.mu.Unlock()
	if hit && now.Sub(cached.at) < updateCheckTTL {
		return cached.version, key, srcNote, nil
	}
	qctx, qcancel := context.WithTimeout(ctx, 60*time.Second)
	defer qcancel()
	out, code, rerr := runner.Run(qctx, Runtime(exe.Runtime), exe.Distro,
		loginShellArgs(Runtime(exe.Runtime), "git ls-remote --tags --refs https://github.com/"+spec.gitRepo+".git"))
	if rerr != nil || code != 0 {
		return "", key, "", fmt.Errorf("最新版查询失败（git ls-remote 退出码 %d）：%s", code, firstLine(out))
	}
	latest = latestGitTag(out)
	if latest == "" {
		return "", key, "", errors.New("源码 tag 查询无可解析版本（refs/tags 无 semver 形态 tag）")
	}
	return latest, key, srcNote, nil
}

// desktopFeedURL 定位桌面渠道安装声明的更新源：app:zcode-desktop 注册项安装
// 目录下 resources/app-update.yml 的 `url:` 行（electron-builder 产物，实录
// "provider: generic / url: http://localhost:8081"）。文件读取走宿主 os 包
// （native Windows 安装路径；服务端不在 Windows 宿主时读不到即跳过——如实）。
func (r *Registry) desktopFeedURL() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.exes {
		if e.Adapter != "zcode" || e.Channel != ChannelAppZcodeDesktop || e.Runtime != string(RuntimeNative) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(e.Path), "resources", "app-update.yml"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "url:"); ok {
				if u := strings.TrimSpace(v); u != "" {
					return u, true
				}
			}
		}
	}
	return "", false
}

// fetchFeedVersion GET <feed>/latest.yml 取 version 行（electron-updater 清单，
// 实录形状 "version: 3.16.3"）。直连不套 env 代理（自建 feed 常为回环/内网
// 地址），5s 超时。
func fetchFeedVersion(ctx context.Context, feedURL string) (string, error) {
	qctx, qcancel := context.WithTimeout(ctx, 5*time.Second)
	defer qcancel()
	req, err := http.NewRequestWithContext(qctx, http.MethodGet, strings.TrimRight(feedURL, "/")+"/latest.yml", nil)
	if err != nil {
		return "", err
	}
	resp, err := feedHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("feed HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "version:"); ok {
			if ver := strings.TrimSpace(v); semverLike(ver) {
				return ver, nil
			}
		}
	}
	return "", errors.New("latest.yml 无可解析 version 行")
}

var feedHTTPClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// verdict 版本判定：同 major 系才可比（comparable=true 时 up_to_date 为语义化
// 比较结果；跨系不可比 → up_to_date=true 不标红，由调用方 note 说明）。
func verdict(current, latest string) (upToDate, comparable bool) {
	if current == "" || latest == "" || majorOf(current) != majorOf(latest) {
		return true, false
	}
	return compareSemver(current, latest) >= 0, true
}

func majorOf(v string) string {
	if i := strings.IndexByte(v, '.'); i > 0 {
		return v[:i]
	}
	return v
}

// latestGitTag 解析 git ls-remote --tags --refs 输出：行尾 refs/tags/<tag>，
// tag 剥 v 前缀后须为 semver 形态；取最大（semver 比较，非字典序）。
// 实证输出形状（zai-org/ZCode，2026-10-27）：
//
//	7e7bd…  refs/tags/v3.14.3
//	9ab8a…  refs/tags/v3.15.1
func latestGitTag(out string) string {
	best := ""
	for _, line := range strings.Split(out, "\n") {
		i := strings.LastIndex(line, "refs/tags/")
		if i < 0 {
			continue
		}
		tag := strings.TrimPrefix(line[i+len("refs/tags/"):], "v")
		if !semverLike(tag) {
			continue // 非 semver tag（rc/nightly/中文名等）不参与
		}
		if best == "" || compareSemver(tag, best) > 0 {
			best = tag
		}
	}
	return best
}

// RunUpdate 执行更新并回填注册表版本（同步原语——UT/简单调用面；产品面的
// 过程可视化走 StartUpdate 流式任务）。无自动通道（manual）返回
// ErrNoUpdateChannel（HTTP 409 面）。
func (r *Registry) RunUpdate(ctx context.Context, id string, runner Runner) (UpdateRun, error) {
	exe, err := r.executableOf(id)
	if err != nil {
		return UpdateRun{}, err
	}
	if runner == nil {
		return UpdateRun{}, errors.New("harness: 无探测执行面（ProbeRunner 未配置）")
	}
	argv := buildUpdateCmd(exe)
	if argv == nil {
		return UpdateRun{}, fmt.Errorf("%w：%s", ErrNoUpdateChannel, updateChannels[exe.Adapter].manualNote)
	}
	ctx, cancel := context.WithTimeout(ctx, updateJobTimeout)
	defer cancel()
	out, code, err := runner.Run(ctx, Runtime(exe.Runtime), exe.Distro, argv)
	return r.collectUpdate(ctx, runner, exe, out, code, err)
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

// ---- 更新任务（0.4.1 过程可视化）：异步执行 + 逐行管道转发 + 订阅重放 ----

// UpdateEvent 更新任务事件（SSE data 载荷）。
type UpdateEvent struct {
	Kind       string `json:"kind"` // line（过程日志行）| done（成功终态）| failed（失败终态）
	Line       string `json:"line,omitempty"`
	NewVersion string `json:"new_version,omitempty"`
	Error      string `json:"error,omitempty"`
}

// updateLogCap 过程日志行上限（npm install 噪声可上千行——环形保留尾部，
// 重放有界）。订阅通道缓冲与之对齐（慢消费者丢行不阻塞更新进程；终态前
// 消费者断开由 unsubscribe 回收）。
const (
	updateLogCap     = 1000
	updateSubBuffer  = 1024
	updateJobTimeout = 5 * time.Minute
)

type updateJob struct {
	mu    sync.Mutex
	lines []string
	done  bool
	final UpdateEvent
	subs  map[chan UpdateEvent]struct{}
}

func (j *updateJob) emit(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return
	}
	j.lines = append(j.lines, line)
	if len(j.lines) > updateLogCap {
		j.lines = j.lines[len(j.lines)-updateLogCap:]
	}
	ev := UpdateEvent{Kind: "line", Line: line}
	for ch := range j.subs {
		select {
		case ch <- ev:
		default: // 慢消费者丢行不阻塞更新（终态事件必达——量级 ≤ 缓冲）
		}
	}
}

func (j *updateJob) finish(ev UpdateEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return
	}
	j.done = true
	j.final = ev
	for ch := range j.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	for ch := range j.subs {
		close(ch)
	}
	j.subs = map[chan UpdateEvent]struct{}{}
}

// StartUpdate 异步启动更新任务（过程经 SubscribeUpdateEvents 逐行可视化）。
// 同实例互斥：任务在途返回 started=false（幂等，不报错）；任务历史保留至下次
// 启动（重放用）。任务上下文独立于请求生命周期（background + 5 分钟上限）。
func (r *Registry) StartUpdate(_ context.Context, id string, runner Runner) (bool, error) {
	exe, err := r.executableOf(id)
	if err != nil {
		return false, err
	}
	if runner == nil {
		return false, errors.New("harness: 无探测执行面（ProbeRunner 未配置）")
	}
	argv := buildUpdateCmd(exe)
	if argv == nil {
		return false, fmt.Errorf("%w：%s", ErrNoUpdateChannel, updateChannels[exe.Adapter].manualNote)
	}
	r.mu.Lock()
	if job, ok := r.updateJobs[id]; ok && !job.done {
		r.mu.Unlock()
		return false, nil // 在途幂等
	}
	job := &updateJob{subs: map[chan UpdateEvent]struct{}{}}
	if r.updateJobs == nil {
		r.updateJobs = map[string]*updateJob{}
	}
	r.updateJobs[id] = job
	r.mu.Unlock()
	go r.runUpdateJob(exe, argv, job, runner)
	return true, nil
}

// buildUpdateCmd 按通道构造更新命令 argv（登录 shell 包装，与 RunUpdate 同源）。
func buildUpdateCmd(exe Executable) []string {
	spec := updateChannels[exe.Adapter]
	rt := Runtime(exe.Runtime)
	switch {
	case len(spec.selfUpdateArgs) > 0:
		return loginShellArgs(rt, shellQuote(exe.Path)+" "+strings.Join(spec.selfUpdateArgs, " "))
	case spec.npmPackage != "":
		return npmArgs(rt, "install -g "+spec.npmPackage+"@latest")
	}
	return nil
}

// displayCmd 过程日志里的命令呈现（(argv 还原可读形态——bash -lc '...' 取 -c 串）。
func displayCmd(argv []string) string {
	if len(argv) >= 3 && argv[0] == "bash" && argv[1] == "-lc" {
		return "$ " + argv[2]
	}
	if len(argv) >= 3 && argv[0] == "cmd" && argv[1] == "/c" {
		return "> " + argv[2]
	}
	return "$ " + strings.Join(argv, " ")
}

// runUpdateJob 任务主体：命令行呈现 → 流式执行（逐行转发）→ 版本回探 → 终态。
func (r *Registry) runUpdateJob(exe Executable, argv []string, job *updateJob, runner Runner) {
	ctx, cancel := context.WithTimeout(context.Background(), updateJobTimeout)
	defer cancel()
	job.emit(displayCmd(argv))
	out, code, err := runner.RunStream(ctx, Runtime(exe.Runtime), exe.Distro, argv, job.emit)
	if err != nil {
		job.finish(UpdateEvent{Kind: "failed", Error: fmt.Sprintf("更新命令执行失败：%v", err)})
		return
	}
	if code != 0 {
		job.finish(UpdateEvent{Kind: "failed", Error: fmt.Sprintf("更新命令退出码 %d：%s", code, lastLines(out, 3))})
		return
	}
	job.emit("$ " + exe.Path + " --version   （版本回探）")
	vctx, vcancel := context.WithTimeout(ctx, 30*time.Second)
	defer vcancel()
	vout, vcode, verr := runner.Run(vctx, Runtime(exe.Runtime), exe.Distro, []string{exe.Path, "--version"})
	if verr != nil || vcode != 0 {
		job.finish(UpdateEvent{Kind: "failed", Error: fmt.Sprintf("更新后版本探测失败（退出码 %d）：%s", vcode, firstLine(vout))})
		return
	}
	version := strings.TrimSpace(firstLine(vout))
	if err := r.RefreshVersion(exe.ID, version); err != nil {
		job.finish(UpdateEvent{Kind: "failed", Error: err.Error()})
		return
	}
	job.emit("已更新到 " + version)
	job.finish(UpdateEvent{Kind: "done", NewVersion: version})
}

// SubscribeUpdateEvents 订阅任务事件：先重放已积累行（快照一致——与 emit
// 同锁下注入通道），再实时；终态事件后通道关闭。无任务（从未启动）返回
// ok=false。cancel 必须调用（回收订阅）。
func (r *Registry) SubscribeUpdateEvents(id string) (<-chan UpdateEvent, func(), bool) {
	r.mu.Lock()
	job, ok := r.updateJobs[id]
	r.mu.Unlock()
	if !ok {
		return nil, nil, false
	}
	ch := make(chan UpdateEvent, updateSubBuffer)
	job.mu.Lock()
	// 重放与注册同临界区：快照后新行必然经通道，无丢失窗口。
	for _, line := range job.lines {
		ch <- UpdateEvent{Kind: "line", Line: line}
	}
	if job.done {
		ch <- job.final
		close(ch)
		job.mu.Unlock()
		return ch, func() {}, true
	}
	job.subs[ch] = struct{}{}
	job.mu.Unlock()
	cancel := func() {
		job.mu.Lock()
		if _, ok := job.subs[ch]; ok {
			delete(job.subs, ch)
			close(ch)
		}
		job.mu.Unlock()
	}
	return ch, cancel, true
}
