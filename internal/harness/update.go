// 0.4.1 Agent 版本检测与更新（负责人指令 2026-10-27："本机使用的 CLI 通常不是
// 最新的"——原 0.7.0 生态项前移）。更新通道三分类（ADR-0016，负责人裁定
// 2026-10-27："现在是三种更新通道：第三方分发仓库 / 自更新 / Mosaic 自带更新
// 通道"——代码与文档同构，便于后续拓展到其它 agent）：
//  1. registry    第三方分发仓库：检测（最新版）与更新（安装）都走仓库客户端。
//     现役 npm（codex）；扩展 = 新增 packageRepo 实现（brew/apt/pip…），家族
//     配置换绑即可。
//  2. self-update 官方自更新：CLI 自带升级子命令——各家参数/输出有差异
//     （claude update / kimi upgrade --yes / mcode update，机制同一：执行即自
//     完成）。检测面可借其仓库分发查最新版（claude/mcode 有 npm 面），无仓库
//     面的如实呈现"无独立查询面"（kimi）。
//  3. mosaic     Mosaic 自带更新通道：自建分发 CLI（zcode——基于官方源码自有
//     打包脚本），更新由 Mosaic 驱动用户提供的脚本（约定
//     $HOME/.mosaic/update/<adapter>.sh，须可执行；过程日志与其他通道同一可
//     视化管线），检测面对齐自建 releases 信号（产品更新源 feed 优先、源码
//     tag 回退）。
//
// npm 调用形态（实证钉死）：绝对路径直跑 npm 失败（shebang `/usr/bin/env node`
// 需登录 shell 的 nvm PATH）；`sh -lc` 会解析到 Windows 互操作 npm
// （/mnt/d/.../nodejs/npm——dash 登录不读 bash 的 nvm 注入）。一律 `bash -lc`
// （登录环境与人类用户终端一致：nvm 前置、npmrc 镜像生效）；Windows native 面
// 走 `cmd /c`（npm.cmd）。
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

// packageRepo 第三方分发仓库客户端抽象（registry 通道的执行面；现役 npm，
// 扩展 = 新仓库实现三方法即可被家族配置换绑——brew/apt/pip 等，命令形态封装
// 在实现内，通道类型与其余面不变）。
type packageRepo interface {
	latestCmd(pkg string) string   // 最新版查询命令（登录 shell 内执行）
	installCmd(pkg string) string  // 安装/更新命令（登录 shell 内执行）
	parseLatest(out string) string // 查询输出 → 版本串（不可解析返回空）
}

// npmRepo npm 仓库（镜像源/代理随登录 shell 环境——包注释实证）。
type npmRepo struct{}

func (npmRepo) latestCmd(pkg string) string  { return "npm view " + pkg + " version" }
func (npmRepo) installCmd(pkg string) string { return "npm install -g " + pkg + "@latest" }
func (npmRepo) parseLatest(out string) string {
	v := strings.TrimSpace(lastLine(out))
	if semverLike(v) {
		return v
	}
	return ""
}

// familySpec 家族通道配置（ADR-0016 三选一：registry | self-update | mosaic）。
type familySpec struct {
	channel  string      // 更新机制类型
	repo     packageRepo // 仓库面：registry 通道的检测+安装；self-update 通道可兼作检测面
	pkg      string      // 仓库包名
	selfArgs []string    // self-update 通道：升级子命令（跟在可执行后的 argv 后缀）
	gitRepo  string      // mosaic 通道：自建 releases 信号仓（owner/repo）
	note     string      // 无检测面/无自动面时的指引（check note 呈现）
}

// updateFamilies 家族通道分派（实证 2026-10-27；mcode 归 self-update——
// 负责人裁定口径 + `mcode update` 子命令实证 "Check for and install a
// Minimax Code update"，npm 面仅作检测）。
var updateFamilies = map[string]familySpec{
	"codex": {channel: "registry", repo: npmRepo{}, pkg: "@openai/codex"},
	"claude": {
		channel: "self-update", repo: npmRepo{}, pkg: "@anthropic-ai/claude-code",
		selfArgs: []string{"update"},
		note:     "原生安装位（~/.local/bin）不受 npm 管辖——更新走官方自更新（claude update）；检测借 npm 分发面查最新版",
	},
	"minimax": {
		channel: "self-update", repo: npmRepo{}, pkg: "@minimax-ai/code",
		selfArgs: []string{"update"},
		note:     "更新走官方自更新子命令（mcode update）；检测借 npm 分发面查最新版",
	},
	"kimi": {
		channel:  "self-update",
		selfArgs: []string{"upgrade", "--yes"},
		note:     "Kimi Code 官方安装器分发（无 npm 包）——无独立最新版查询面；「更新到最新」经 kimi upgrade 幂等执行（已是最新则无动作），登录态与配置保留",
	},
	"zcode": {
		channel: "mosaic",
		gitRepo: "zai-org/ZCode",
		note:    "Mosaic 自带更新通道（自建打包分发）：放置可执行更新脚本 ~/.mosaic/update/zcode.sh（拉上游源码 + 自有打包管线 + 安装）即可被「更新到最新」驱动，过程日志与其他通道同一可视化管线；脚本缺位时请手动执行自有管线，桌面版在应用内自更新",
	},
}

// hasCheckFace 检测面在位（与更新面正交）：registry/self-update 看仓库绑定；
// mosaic 的自建 releases 信号面（feed→tag）恒在。
func (s familySpec) hasCheckFace() bool {
	switch s.channel {
	case "registry", "self-update":
		return s.repo != nil && s.pkg != ""
	default:
		return true
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
	Channel    string `json:"channel"`    // registry | self-update | mosaic（ADR-0016 三分类）
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
	spec := updateFamilies[exe.Adapter]
	now := time.Now()
	check := UpdateCheck{
		Adapter:        exe.Adapter,
		CurrentVersion: exe.Version,
		Channel:        spec.channel,
		CanCheck:       spec.hasCheckFace(),
		CanUpdate:      spec.channel == "registry" || spec.channel == "self-update",
		CheckedAt:      now.UTC().Format(time.RFC3339),
	}
	check.CurrentSemver = extractSemver(exe.Version)
	if spec.note != "" {
		check.Note = spec.note
	}
	if spec.channel == "mosaic" && runner != nil {
		// mosaic 更新面 = 约定脚本在位（检测面独立于脚本）。
		if _, ok := mosaicUpdateScript(ctx, runner, exe); ok {
			check.CanUpdate = true
		}
	}
	if !check.CanCheck {
		check.UpToDate = true // 无检测面不标红（manual 通道由 Note 指引）
		return check, nil
	}
	if runner == nil {
		check.Note = "无探测执行面（ProbeRunner 未配置）"
		return check, nil
	}

	// 检测面按通道分派：registry / self-update（有仓库绑定时）→ 仓库客户端；
	// mosaic → 自建 releases 信号（桌面产品更新源 feed 优先，不可达回退
	// git ls-remote --tags）。
	latest, srcNote := "", ""
	var cacheKey string
	if spec.repo != nil && spec.pkg != "" {
		cacheKey = latestCacheKey(Runtime(exe.Runtime), exe.Distro, spec.pkg)
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
			loginShellArgs(Runtime(exe.Runtime), spec.repo.latestCmd(spec.pkg)))
		if err != nil || code != 0 {
			check.Note = fmt.Sprintf("最新版查询失败（退出码 %d）：%s", code, firstLine(out))
			return check, nil // 环境态：返回可呈现的部分结果，不 5xx
		}
		latest = spec.repo.parseLatest(out)
		if latest == "" {
			check.Note = "最新版查询输出不可解析：" + firstLine(out)
			return check, nil
		}
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

// gitRepoLatest mosaic 通道（自建分发家族，zcode）的版本信号：两层取优。
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
func (r *Registry) gitRepoLatest(ctx context.Context, runner Runner, exe Executable, spec familySpec, now time.Time) (latest, cacheKey, srcNote string, err error) {
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
	argv := buildUpdateCmd(ctx, runner, exe)
	if argv == nil {
		return UpdateRun{}, fmt.Errorf("%w：%s", ErrNoUpdateChannel, updateFamilies[exe.Adapter].note)
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
func (r *Registry) StartUpdate(ctx context.Context, id string, runner Runner) (bool, error) {
	exe, err := r.executableOf(id)
	if err != nil {
		return false, err
	}
	if runner == nil {
		return false, errors.New("harness: 无探测执行面（ProbeRunner 未配置）")
	}
	argv := buildUpdateCmd(ctx, runner, exe)
	if argv == nil {
		return false, fmt.Errorf("%w：%s", ErrNoUpdateChannel, updateFamilies[exe.Adapter].note)
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

// buildUpdateCmd 按通道（ADR-0016）构造更新命令 argv（登录 shell 包装）：
// registry → 仓库安装命令；self-update → 可执行 + 家族子命令；mosaic →
// 约定脚本 $HOME/.mosaic/update/<adapter>.sh（缺位返回 nil = 无自动更新面）。
func buildUpdateCmd(ctx context.Context, runner Runner, exe Executable) []string {
	spec := updateFamilies[exe.Adapter]
	rt := Runtime(exe.Runtime)
	switch spec.channel {
	case "self-update":
		return loginShellArgs(rt, shellQuote(exe.Path)+" "+strings.Join(spec.selfArgs, " "))
	case "registry":
		return loginShellArgs(rt, spec.repo.installCmd(spec.pkg))
	case "mosaic":
		if script, ok := mosaicUpdateScript(ctx, runner, exe); ok {
			return loginShellArgs(rt, shellQuote(script))
		}
	}
	return nil
}

// mosaicUpdateScript mosaic 通道更新脚本约定位：$HOME/.mosaic/update/<adapter>.sh
// （存在性探测；可执行性由执行面裁决——不可执行则任务失败面如实呈现）。
func mosaicUpdateScript(ctx context.Context, runner Runner, exe Executable) (string, bool) {
	home := runner.Home(ctx, Runtime(exe.Runtime), exe.Distro)
	if home == "" {
		return "", false
	}
	script := strings.TrimSuffix(home, "/") + "/.mosaic/update/" + exe.Adapter + ".sh"
	return script, runner.Exists(ctx, Runtime(exe.Runtime), exe.Distro, script)
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
