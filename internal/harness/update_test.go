// 0.4.1 版本检测与更新 UT：通道分类、semver 抽取/比较、npm view 实查与 30 分钟
// 缓存（失败不缓存）、claude 官方自更新 / npm i -g 通道 argv 形态、manual 家族
// 拒收、版本回填持久化、Windows native 的 cmd /c 形态。
package harness

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func updateTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	reg, path := tempRegistry(t)
	return reg, path
}

// TestUpdateChannelSpecs：五家族通道分类（实证 2026-10-27 分发面）。
func TestUpdateChannelSpecs(t *testing.T) {
	cases := map[string]struct {
		channel   string
		canCheck  bool
		canUpdate bool
	}{
		"claude":  {"self-update", true, true},
		"codex":   {"npm", true, true},
		"minimax": {"npm", true, true},
		"kimi":    {"manual", false, false},
		"zcode":   {"manual", false, false},
	}
	for adapter, want := range cases {
		spec := updateChannels[adapter]
		if spec.channel() != want.channel {
			t.Fatalf("%s channel = %s，期望 %s", adapter, spec.channel(), want.channel)
		}
	}
	if updateChannels["claude"].npmPackage != "@anthropic-ai/claude-code" ||
		updateChannels["codex"].npmPackage != "@openai/codex" ||
		updateChannels["minimax"].npmPackage != "@minimax-ai/code" {
		t.Fatal("npm 分发面包名与实证不符")
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

// TestCheckUpdateManualChannel：kimi 非 npm 分发——无检测面不标红、note 携指引、
// 不发起任何 runner 调用。
func TestCheckUpdateManualChannel(t *testing.T) {
	reg, _ := updateTestRegistry(t)
	reg.exes = []Executable{{
		ID: "k_1", Adapter: "kimi", Runtime: "native",
		Path: "/home/u/.kimi-code/bin/kimi", Version: "0.39.1", Enabled: true,
	}}
	runner := newFakeRunner()
	check, err := reg.CheckUpdate(context.Background(), "k_1", runner)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if check.CanCheck || check.CanUpdate || check.Channel != "manual" {
		t.Fatalf("manual 通道字段不符：%+v", check)
	}
	if !check.UpToDate || check.Note == "" {
		t.Fatalf("manual 不标红且 note 携指引：%+v", check)
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
