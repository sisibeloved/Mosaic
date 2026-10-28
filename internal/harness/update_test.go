// 0.4.1 版本检测与更新 UT：通道分类、semver 抽取/比较、npm view 实查与 30 分钟
// 缓存（失败不缓存）、claude 官方自更新 / npm i -g 通道 argv 形态、manual 家族
// 拒收、版本回填持久化、Windows native 的 cmd /c 形态。
package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func updateTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	reg, path := tempRegistry(t)
	return reg, path
}

// TestUpdateChannelSpecs：五家族通道分类（实证 2026-10-27 分发面；kimi 验收
// 反馈轮改道官方自更新；zcode 负责人问询轮改道源码 tag 检测——官方无 Linux
// CLI 产物、本机为自建打包，检测对齐 git tag、更新保持 manual）。
func TestUpdateChannelSpecs(t *testing.T) {
	cases := map[string]struct {
		channel   string
		canCheck  bool
		canUpdate bool
	}{
		"claude":  {"self-update", true, true},
		"codex":   {"npm", true, true},
		"minimax": {"npm", true, true},
		"kimi":    {"self-update", false, true},
		"zcode":   {"releases", true, false},
	}
	for adapter, want := range cases {
		spec := updateChannels[adapter]
		if spec.channel() != want.channel {
			t.Fatalf("%s channel = %s，期望 %s", adapter, spec.channel(), want.channel)
		}
		if got := len(spec.selfUpdateArgs) > 0 || spec.npmPackage != ""; got != want.canUpdate {
			t.Fatalf("%s canUpdate = %v，期望 %v", adapter, got, want.canUpdate)
		}
		if got := spec.npmPackage != "" || spec.gitRepo != ""; got != want.canCheck {
			t.Fatalf("%s canCheck = %v，期望 %v", adapter, got, want.canCheck)
		}
	}
	if updateChannels["claude"].npmPackage != "@anthropic-ai/claude-code" ||
		updateChannels["codex"].npmPackage != "@openai/codex" ||
		updateChannels["minimax"].npmPackage != "@minimax-ai/code" {
		t.Fatal("npm 分发面包名与实证不符")
	}
	if updateChannels["zcode"].gitRepo != "zai-org/ZCode" {
		t.Fatal("zcode 源码仓应为 zai-org/ZCode")
	}
	if !reflect.DeepEqual(updateChannels["kimi"].selfUpdateArgs, []string{"upgrade", "--yes"}) {
		t.Fatal("kimi 自更新子命令应为 upgrade --yes")
	}
}

// TestExtractAndCompareSemver：异构探测原文抽取 + 语义化比较（无当前版本按
// 最小处理——保守提示有更新）。
func TestExtractAndCompareSemver(t *testing.T) {
	cases := map[string]string{
		"2.1.172 (Claude Code)": "2.1.172",
		"codex-cli 0.153.4":     "0.153.4",
		"0.16.9":                "0.16.9",
		"v1.2.3":                "1.2.3",
		"无版本串":                  "",
	}
	for raw, want := range cases {
		if got := extractSemver(raw); got != want {
			t.Fatalf("extractSemver(%q) = %q，期望 %q", raw, got, want)
		}
	}
	if compareSemver("2.1.172", "2.1.311") >= 0 {
		t.Fatal("2.1.172 < 2.1.311 应判有更新")
	}
	if compareSemver("2.1.311", "2.1.172") <= 0 {
		t.Fatal("2.1.311 > 2.1.172")
	}
	if compareSemver("2.1", "2.1.1") >= 0 {
		t.Fatal("缺位按 0 补——2.1 < 2.1.1")
	}
	if compareSemver("", "1.0.0") >= 0 {
		t.Fatal("无当前版本按最小——判有更新")
	}
	if compareSemver("3.0.0", "3.0.0") != 0 {
		t.Fatal("相等")
	}
	if !semverLike("2.1.311") || semverLike("") || semverLike("2.1.x") {
		t.Fatal("semverLike 形状校验不符")
	}
}

// TestCheckUpdateLatestAndCache：npm view 实查（登录 shell 形态 argv）+ 30 分钟
// 缓存（命中不重查——runs 撤键后仍能拿到最新版）。
func TestCheckUpdateLatestAndCache(t *testing.T) {
	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "cl_1", Adapter: "claude", Runtime: "native",
		Path: "/home/u/.local/bin/claude", Version: "2.1.172 (Claude Code)", Enabled: true,
	}}
	runner := newFakeRunner()
	runner.runs["native||bash -lc npm view @anthropic-ai/claude-code version"] = "2.1.311\n"

	check, err := reg.CheckUpdate(context.Background(), "cl_1", runner)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if check.Latest != "2.1.311" || check.UpToDate {
		t.Fatalf("应检出 2.1.311 有更新：%+v", check)
	}
	if check.CurrentSemver != "2.1.172" || check.Channel != "self-update" || !check.CanUpdate {
		t.Fatalf("检测字段不符：%+v", check)
	}
	// 缓存命中：撤掉 runs 供给，二次查询仍取缓存值。
	delete(runner.runs, "native||bash -lc npm view @anthropic-ai/claude-code version")
	check2, err := reg.CheckUpdate(context.Background(), "cl_1", runner)
	if err != nil {
		t.Fatalf("check2: %v", err)
	}
	if check2.Latest != "2.1.311" {
		t.Fatalf("缓存命中应返回既有最新版：%+v", check2)
	}
}

// TestCheckUpdateGitTagsChannel：zcode 源码 tag 检测（负责人问询轮：CLI 为自建
// 打包，官方仅桌面安装包——检测对齐 git ls-remote 最新 semver tag，更新保持
// manual）。三形态：(1) 版本方案不可比（安装 0.16.9 桌面内核版方案 vs 上游
// 3.x）——不标红、note 说明方案差异；(2) 同 major 系可比——正常判有更新；
// (3) 更新执行 → ErrNoUpdateChannel（自建产物 Mosaic 不做替换）。
func TestCheckUpdateGitTagsChannel(t *testing.T) {
	tags := "7e7bd17  refs/tags/v3.14.3\n" +
		"9ab8a64  refs/tags/v3.15.1\n" +
		"abcdef0  refs/tags/nightly\n" +
		"1234567  refs/tags/v3.9.0\n"
	newRunner := func() *fakeRunner {
		runner := newFakeRunner()
		runner.runs["native||bash -lc git ls-remote --tags --refs https://github.com/zai-org/ZCode.git"] = tags
		return runner
	}

	// (1) 不可比：0.16.9（0 系）vs 3.15.1（3 系）
	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "z_2", Adapter: "zcode", Runtime: "native",
		Path: "/home/u/.zcode/bin/zcode", Version: "0.16.9", Enabled: true,
	}}
	check, err := reg.CheckUpdate(context.Background(), "z_2", newRunner())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !check.CanCheck || check.CanUpdate || check.Channel != "releases" {
		t.Fatalf("releases 通道字段不符：%+v", check)
	}
	if check.Latest != "3.15.1" {
		t.Fatalf("应取最大 semver tag（非字典序、非 semver tag 忽略）：%+v", check)
	}
	if check.Comparable || !check.UpToDate {
		t.Fatalf("跨版本方案不可比——不判更新不标红：%+v", check)
	}
	if !strings.Contains(check.Note, "不可比") || !strings.Contains(check.Note, "自建") {
		t.Fatalf("note 应说明方案差异与自建指引：%+v", check)
	}

	// (2) 可比：3.14.3 vs 3.15.1 → 有更新
	reg2, _ := updateTestRegistry(t)
	reg2.exes = []Executable{{
		ID: "z_3", Adapter: "zcode", Runtime: "native",
		Path: "/home/u/.zcode/bin/zcode", Version: "3.14.3", Enabled: true,
	}}
	check2, _ := reg2.CheckUpdate(context.Background(), "z_3", newRunner())
	if !check2.Comparable || check2.UpToDate {
		t.Fatalf("同 major 系应可比且判有更新：%+v", check2)
	}

	// (3) 更新执行：自建产物无自动通道
	if _, err := reg2.RunUpdate(context.Background(), "z_3", newRunner()); !errors.Is(err, ErrNoUpdateChannel) {
		t.Fatalf("自建打包应 ErrNoUpdateChannel：%v", err)
	}
}

// TestCheckUpdateFailureNotCached：查询失败（npm view 非零退出）返回部分结果携
// note，且不缓存——恢复后二次查询即拿到最新版。
func TestCheckUpdateFailureNotCached(t *testing.T) {
	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "cx_1", Adapter: "codex", Runtime: "native",
		Path: "/home/u/bin/codex", Version: "codex-cli 0.153.4", Enabled: true,
	}}
	runner := newFakeRunner()
	runner.runs["native||bash -lc npm view @openai/codex version"] = "network error\x00EXIT:1"
	check, err := reg.CheckUpdate(context.Background(), "cx_1", runner)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if check.Latest != "" || check.UpToDate || !strings.Contains(check.Note, "失败") {
		t.Fatalf("失败面应携 note 不带最新版：%+v", check)
	}
	runner.runs["native||bash -lc npm view @openai/codex version"] = "0.160.0\n"
	check2, _ := reg.CheckUpdate(context.Background(), "cx_1", runner)
	if check2.Latest != "0.160.0" || check2.UpToDate {
		t.Fatalf("失败不缓存——恢复后即取最新：%+v", check2)
	}
}

// TestRunUpdateSelfUpdateClaude：claude 走官方自更新（bash -lc 包装 exe update），
// 完成后重探版本并持久化（重载注册表验证）。
func TestRunUpdateSelfUpdateClaude(t *testing.T) {
	reg, path := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "cl_2", Adapter: "claude", Runtime: "native",
		Path: "/home/u/.local/bin/claude", Version: "2.1.172 (Claude Code)", Enabled: true,
	}}
	runner := newFakeRunner()
	runner.runs["native||bash -lc "+shellQuote("/home/u/.local/bin/claude")+" update"] = "Updated to 2.1.311\n"
	runner.runs["native||/home/u/.local/bin/claude --version"] = "2.1.311 (Claude Code)\n"

	res, err := reg.RunUpdate(context.Background(), "cl_2", runner)
	if err != nil {
		t.Fatalf("run update: %v", err)
	}
	if res.Status != "ok" || res.NewVersion != "2.1.311 (Claude Code)" {
		t.Fatalf("更新结果不符：%+v", res)
	}
	// 持久化：重载注册表版本已回填。
	reloaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.List()[0].Version != "2.1.311 (Claude Code)" {
		t.Fatalf("版本回填未持久化：%+v", reloaded.List()[0])
	}
}

// TestRunUpdateNPMChannel：codex 走 npm i -g（登录 shell 形态——argv 由 runs 键
// 字面钉住）；版本回探同自更新通道。
func TestRunUpdateNPMChannel(t *testing.T) {
	reg, path := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "cx_2", Adapter: "codex", Runtime: "native",
		Path: "/home/u/bin/codex", Version: "codex-cli 0.153.4", Enabled: true,
	}}
	runner := newFakeRunner()
	runner.runs["native||bash -lc npm install -g @openai/codex@latest"] = "added 1 package\n"
	runner.runs["native||/home/u/bin/codex --version"] = "codex-cli 0.160.0\n"

	res, err := reg.RunUpdate(context.Background(), "cx_2", runner)
	if err != nil {
		t.Fatalf("run update: %v", err)
	}
	if res.NewVersion != "codex-cli 0.160.0" {
		t.Fatalf("更新结果不符：%+v", res)
	}
	reloaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.List()[0].Version != "codex-cli 0.160.0" {
		t.Fatalf("版本回填未持久化：%+v", reloaded.List()[0])
	}
}

// TestRunUpdateManualRejected：kimi/zcode 无自动通道 → ErrNoUpdateChannel（HTTP
// 409 面），不发起任何执行。
func TestRunUpdateManualRejected(t *testing.T) {
	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "z_1", Adapter: "zcode", Runtime: "native",
		Path: "/home/u/.zcode/bin/zcode", Version: "0.16.9", Enabled: true,
	}}
	_, err := reg.RunUpdate(context.Background(), "z_1", newFakeRunner())
	if !errors.Is(err, ErrNoUpdateChannel) {
		t.Fatalf("manual 家族应拒收：%v", err)
	}
}

// TestRunUpdateCommandFailure：更新命令非零退出 → 显式错误携输出尾（不静默）。
func TestRunUpdateCommandFailure(t *testing.T) {
	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "cx_3", Adapter: "codex", Runtime: "native",
		Path: "/home/u/bin/codex", Version: "codex-cli 0.153.4", Enabled: true,
	}}
	runner := newFakeRunner()
	runner.runs["native||bash -lc npm install -g @openai/codex@latest"] = "EACCES permission denied\x00EXIT:1"
	_, err := reg.RunUpdate(context.Background(), "cx_3", runner)
	if err == nil || !strings.Contains(err.Error(), "EACCES") {
		t.Fatalf("更新失败应携输出尾：%v", err)
	}
}

// TestLoginShellArgsForms：WSL/Linux native 走 bash -lc；Windows native 走
// cmd /c（npm.cmd）——宿主 OS 分支经 goruntimeOS 替换面钉住。
func TestLoginShellArgsForms(t *testing.T) {
	if got := loginShellArgs(RuntimeWSL, "npm view x version"); !reflect.DeepEqual(got, []string{"bash", "-lc", "npm view x version"}) {
		t.Fatalf("WSL 形态：%v", got)
	}
	orig := goruntimeOS
	defer func() { goruntimeOS = orig }()
	goruntimeOS = "windows"
	got := npmArgs(RuntimeNative, "view x version")
	if !reflect.DeepEqual(got, []string{"cmd", "/c", "npm view x version"}) {
		t.Fatalf("Windows native 形态：%v", got)
	}
}

// TestRefreshVersionNotFound：不存在实例 → ErrNotFound。
func TestRefreshVersionNotFound(t *testing.T) {
	reg, _ := updateTestRegistry(t)
	if err := reg.RefreshVersion("nope", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound：%v", err)
	}
}

// TestRunUpdateSelfUpdateKimi：kimi 官方自更新子命令（upgrade --yes——验收反馈③
// 实证：无 npm 包、upgrade|update 子命令在席、-y 免确认）；检测面缺席如实呈现
// （can_check=false + note 说明幂等升级语义），更新按钮语义由 UI 承接。
func TestRunUpdateSelfUpdateKimi(t *testing.T) {
	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "k_2", Adapter: "kimi", Runtime: "native",
		Path: "/home/u/.kimi-code/bin/kimi", Version: "2.1.1", Enabled: true,
	}}
	runner := newFakeRunner()
	runner.runs["native||bash -lc "+shellQuote("/home/u/.kimi-code/bin/kimi")+" upgrade --yes"] = "已升级到 2.2.0\n"
	runner.runs["native||/home/u/.kimi-code/bin/kimi --version"] = "2.2.0\n"

	check, err := reg.CheckUpdate(context.Background(), "k_2", runner)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if check.CanCheck || !check.CanUpdate || check.Channel != "self-update" || check.Note == "" {
		t.Fatalf("kimi 检测面缺席应如实呈现：%+v", check)
	}
	res, err := reg.RunUpdate(context.Background(), "k_2", runner)
	if err != nil {
		t.Fatalf("run update: %v", err)
	}
	if res.NewVersion != "2.2.0" {
		t.Fatalf("更新结果不符：%+v", res)
	}
}

// TestZcodeFeedPreferredOverTags：桌面产品更新源（app-update.yml → latest.yml）
// 优先于源码 tag——负责人问询轮实录：桌面已 3.16.3（自建 feed 分发）而 OSS
// tag 仅 v3.15.1，feed 在线时 latest 必须取产品通道值，且不发起 git 查询。
func TestZcodeFeedPreferredOverTags(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/latest.yml") {
			_, _ = w.Write([]byte("version: 3.16.3\npath: ZCode-3.16.3-win-x64.exe\nsha512: x\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer feed.Close()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "resources"), 0o700); err != nil {
		t.Fatal(err)
	}
	appUpdate := "provider: generic\nuseMultipleRangeRequest: false\nurl: " + feed.URL + "\n"
	if err := os.WriteFile(filepath.Join(dir, "resources", "app-update.yml"), []byte(appUpdate), 0o600); err != nil {
		t.Fatal(err)
	}

	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{
		{ID: "z_cli", Adapter: "zcode", Runtime: "native", Path: "/home/u/.local/bin/zcode",
			Version: "0.16.9", Source: SourceAuto, Enabled: true},
		{ID: "z_app", Adapter: "zcode", Runtime: "native", Path: filepath.Join(dir, "ZCode.exe"),
			Version: "0.16.9", Source: SourceAuto, Channel: "app:zcode-desktop"},
	}
	runner := newFakeRunner() // 无 git 供给：feed 命中则根本不应走到 git 查询
	check, err := reg.CheckUpdate(context.Background(), "z_cli", runner)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if check.Latest != "3.16.3" {
		t.Fatalf("feed 在线时 latest 应取产品通道值：%+v", check)
	}
	if !strings.Contains(check.Note, "产品更新源") {
		t.Fatalf("note 应标注信号来源为产品更新源：%+v", check)
	}
	if check.Comparable || !check.UpToDate {
		t.Fatalf("0.x vs 3.x 仍不可比不标红：%+v", check)
	}
}

// TestZcodeFeedDownFallsBackToTags：feed 不可达（自建 infra 离线常态）→ 回退
// 源码 tag，note 如实标注信号来源与产品通道可能领先的说明。
func TestZcodeFeedDownFallsBackToTags(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "resources"), 0o700); err != nil {
		t.Fatal(err)
	}
	// 端口几乎必然无人监听（1 号端口无服务）——feed 连接失败面
	appUpdate := "provider: generic\nurl: http://127.0.0.1:1/\n"
	if err := os.WriteFile(filepath.Join(dir, "resources", "app-update.yml"), []byte(appUpdate), 0o600); err != nil {
		t.Fatal(err)
	}

	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{
		{ID: "z_cli2", Adapter: "zcode", Runtime: "native", Path: "/home/u/.local/bin/zcode",
			Version: "0.16.9", Source: SourceAuto, Enabled: true},
		{ID: "z_app2", Adapter: "zcode", Runtime: "native", Path: filepath.Join(dir, "ZCode.exe"),
			Version: "0.16.9", Source: SourceAuto, Channel: "app:zcode-desktop"},
	}
	runner := newFakeRunner()
	runner.runs["native||bash -lc git ls-remote --tags --refs https://github.com/zai-org/ZCode.git"] =
		"abc1234  refs/tags/v3.15.1\n"
	check, err := reg.CheckUpdate(context.Background(), "z_cli2", runner)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if check.Latest != "3.15.1" {
		t.Fatalf("feed 离线应回退源码 tag：%+v", check)
	}
	if !strings.Contains(check.Note, "不可达") || !strings.Contains(check.Note, "开源仓库源码 tag") {
		t.Fatalf("note 应标注回退来源与产品通道说明：%+v", check)
	}
}
