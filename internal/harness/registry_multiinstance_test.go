// UT 层：同 Agent 多实例发现与家族优先级（C 轨，负责人裁定 2026-08-31）——
// 扫描枚举同一 adapter 的全部安装实例（PATH + 版本管理器目录 + 桌面应用内 bundled CLI），
// 实例按渠道（cli / app:*）分类并依家族优先级排序，启用态经 SetEnabled 在实例间切换。
// TDD：本文件先行于实现（红→绿）。Runner 用 fake。
package harness

import (
	"context"
	"testing"
)

// 多实例枚举：PATH 命中与多个已知目录命中全部登记（此前 first-hit 只留首个）。
func TestScanEnumeratesAllInstances(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	// PATH 一份 + nvm 一份 + volta 一份（同 adapter 三实例，配置/会话各自独立）
	runner.lookups["native||codex"] = "/usr/local/bin/codex"
	nvmDir := "/home/u/.nvm/versions/node/v24.14.1/bin"
	runner.globs["/home/u/.nvm/versions/node/*/bin"] = []string{nvmDir}
	runner.globs["/home/u/.volta/bin"] = []string{"/home/u/.volta/bin"}
	for _, p := range []string{"/usr/local/bin/codex", nvmDir + "/codex", "/home/u/.volta/bin/codex"} {
		runner.exists["native||"+p] = true
		runner.runs["native||"+p+" --version"] = "codex-cli 0.149.1\n"
		runner.runs["native||"+p+" login status"] = "Logged in using ChatGPT\n"
	}

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var codexes []Executable
	for _, e := range reg.List() {
		if e.Adapter == "codex" {
			codexes = append(codexes, e)
		}
	}
	if len(codexes) != 3 {
		t.Fatalf("三实例应全部登记，got %d: %+v", len(codexes), reg.List())
	}
	ids := map[string]bool{}
	for _, e := range codexes {
		if ids[e.ID] {
			t.Fatalf("实例 ID 冲突：%+v", codexes)
		}
		ids[e.ID] = true
		if e.Channel != ChannelCLI {
			t.Fatalf("PATH/glob 发现应为 cli 渠道：%+v", e)
		}
	}
}

// 同路径去重：PATH 解析与 glob 命中同一文件时只登记一次。
func TestScanDedupesSamePath(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	nvmDir := "/home/u/.nvm/versions/node/v24.14.1/bin"
	runner.lookups["native||codex"] = nvmDir + "/codex" // PATH 解析到 nvm 内同一文件
	runner.globs["/home/u/.nvm/versions/node/*/bin"] = []string{nvmDir}
	runner.exists["native||"+nvmDir+"/codex"] = true
	runner.runs["native||"+nvmDir+"/codex --version"] = "codex-cli 0.149.1\n"
	runner.runs["native||"+nvmDir+"/codex login status"] = "Logged in using ChatGPT\n"

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n := len(reg.List()); n != 1 {
		t.Fatalf("同路径应去重为 1 项，got %d: %+v", n, reg.List())
	}
}

// Codex 桌面应用实例：WindowsApps 内 bundled codex.exe 经 AppGlobs 发现，渠道 app:codex-desktop。
// （路径形状实证自 openai/codex issue #40700/#41059；Windows 真机验证登记 platform-notes。）
func TestScanCodexDesktopAppInstance(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = `C:/Users/u`
	appDir := `C:/Program Files/WindowsApps/OpenAI.Codex_26.820.9563.0_x64__2p2nqsd0c76g0/app/resources`
	runner.globs[`C:/Program Files/WindowsApps/OpenAI.Codex_*/app/resources`] = []string{appDir}
	runner.exists["native||"+appDir+"/codex.exe"] = true
	runner.runs["native||"+appDir+"/codex.exe --version"] = "codex-cli 0.149.1\n"
	runner.runs["native||"+appDir+"/codex.exe login status"] = "Logged in using ChatGPT\n"
	// 同时存在独立 CLI（次优先级）
	runner.lookups["native||codex"] = `C:/Users/u/.nvm/versions/node/v24.14.1/bin/codex`
	runner.exists["native||C:/Users/u/.nvm/versions/node/v24.14.1/bin/codex"] = true
	runner.runs["native||C:/Users/u/.nvm/versions/node/v24.14.1/bin/codex --version"] = "codex-cli 0.149.1\n"
	runner.runs["native||C:/Users/u/.nvm/versions/node/v24.14.1/bin/codex login status"] = "Logged in using ChatGPT\n"

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("应发现 App+CLI 两实例，got %+v", list)
	}
	// 负责人裁定 2：Codex App 实例优先级更高 → 排序在前
	if list[0].Channel != ChannelAppCodexDesktop {
		t.Fatalf("App 实例应排首位：%+v", list)
	}
	if list[0].Priority >= list[1].Priority {
		t.Fatalf("App 优先级数值应更小（更优先）：%+v", list)
	}
	if list[0].Path != appDir+"/codex.exe" {
		t.Fatalf("App 实例路径不符：%+v", list[0])
	}
	// 两实例均可启用、独立成座（配置/会话独立）
	for _, e := range list {
		if err := reg.SetEnabled(e.ID, true); err != nil {
			t.Fatalf("实例应可启用: %v", err)
		}
	}
	if n := len(reg.EnabledList()); n != 2 {
		t.Fatalf("启用切换后应 2 座，got %d", n)
	}
}

// WSL 发行版内不展开 App 位置（Windows 桌面应用是宿主原生面的概念）。
func TestScanSkipsAppGlobsForWSL(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.distros = []string{"Ubuntu"}
	runner.homes["wsl|Ubuntu"] = "/home/wslu"
	runner.globs[`C:/Program Files/WindowsApps/OpenAI.Codex_*/app/resources`] = []string{
		`C:/Program Files/WindowsApps/OpenAI.Codex_26.820.9563.0_x64__2p2nqsd0c76g0/app/resources`,
	}
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{IncludeWSL: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, e := range reg.List() {
		if e.Channel != "" && e.Channel != ChannelCLI {
			t.Fatalf("WSL 面不得出现 App 渠道实例：%+v", e)
		}
	}
}

// 家族优先级裁定（负责人 2026-08-31）：Codex App > Codex CLI；Kimi Code > Kimi Work（独立计费）。
func TestPriorityForRulings(t *testing.T) {
	if got := PriorityFor("codex", ChannelAppCodexDesktop); got >= PriorityFor("codex", ChannelCLI) {
		t.Fatalf("codex app(%d) 应优先于 cli(%d)", got, PriorityFor("codex", ChannelCLI))
	}
	if got := PriorityFor("kimi", ChannelCLI); got >= PriorityFor("kimi", ChannelAppKimiWork) {
		t.Fatalf("kimi code(%d) 应优先于 work(%d)", got, PriorityFor("kimi", ChannelAppKimiWork))
	}
	if got := PriorityFor("kimi", "app:anything"); got != PriorityFor("kimi", ChannelAppKimiWork) {
		t.Fatalf("kimi app 渠道应按类归并：%d vs %d", got, PriorityFor("kimi", ChannelAppKimiWork))
	}
	if got := PriorityFor("codex", "mystery"); got != PriorityUnknownChannel {
		t.Fatalf("未知渠道优先级应为 %d，got %d", PriorityUnknownChannel, got)
	}
	if got := PriorityFor("zcode", ChannelCLI); got >= PriorityFor("zcode", ChannelAppZcodeDesktop) {
		t.Fatalf("zcode cli(%d) 应优先于桌面垫片(%d)——官方无头形态优先（裁定 2026-09-23）",
			got, PriorityFor("zcode", ChannelAppZcodeDesktop))
	}
	if got := PriorityFor("minimax", ChannelCLI); got != PriorityDefault {
		t.Fatalf("无裁定家族应为默认优先级 %d，got %d", PriorityDefault, got)
	}
}

// 手动登记携带渠道覆盖（App 实例面未实证前的进入路径）：kimi 手动项标 app:kimi-work，
// 排序落在家族优先级对应位次；非法渠道串拒绝。
func TestAddManualChannelOverride(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	runner.lookups["native||kimi"] = "/home/u/.kimi-code/bin/kimi"
	runner.runs["native||/home/u/.kimi-code/bin/kimi --version"] = "0.39.1\n"
	runner.exists["native||/home/u/.kimi-code/credentials/kimi-code.json"] = true

	if err := reg.AddManual(context.Background(), runner, Executable{
		Adapter: "kimi", Runtime: "native", Path: "/home/u/.kimi-code/bin/kimi", Channel: ChannelAppKimiWork,
	}); err != nil {
		t.Fatalf("manual: %v", err)
	}
	got := reg.List()[0]
	if got.Channel != ChannelAppKimiWork || got.Priority != PriorityFor("kimi", ChannelAppKimiWork) {
		t.Fatalf("手动渠道覆盖未生效：%+v", got)
	}
	if err := reg.AddManual(context.Background(), runner, Executable{
		Adapter: "kimi", Runtime: "native", Path: "/x/kimi", Channel: "weird channel!",
	}); err == nil {
		t.Fatal("非法渠道串必须拒绝")
	}
}

// 排序规范：List/EnabledList 按 (adapter, priority, path) 稳定排序——设置页展示与
// 座位顺序共用同一事实源（Track A 设置菜单直接消费）。
func TestListSortedByAdapterPriorityPath(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	// codex cli 两份 + codex app 一份 + kimi 一份
	runner.lookups["native||codex"] = "/usr/local/bin/codex"
	runner.globs["/home/u/.volta/bin"] = []string{"/home/u/.volta/bin"}
	runner.lookups["native||kimi"] = "/home/u/.kimi-code/bin/kimi"
	appDir := `C:/Program Files/WindowsApps/OpenAI.Codex_26.820.9563.0_x64__2p2nqsd0c76g0/app/resources`
	runner.globs[`C:/Program Files/WindowsApps/OpenAI.Codex_*/app/resources`] = []string{appDir}
	for _, p := range []string{"/usr/local/bin/codex", "/home/u/.volta/bin/codex", appDir + "/codex.exe"} {
		runner.exists["native||"+p] = true
		runner.runs["native||"+p+" --version"] = "codex-cli 0.149.1\n"
		runner.runs["native||"+p+" login status"] = "Logged in using ChatGPT\n"
	}
	runner.runs["native||/home/u/.kimi-code/bin/kimi --version"] = "0.39.1\n"
	runner.exists["native||/home/u/.kimi-code/credentials/kimi-code.json"] = true

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) != 4 {
		t.Fatalf("应 4 项，got %+v", list)
	}
	// codex app → codex cli（path 序）→ kimi cli
	wantChannels := []string{ChannelAppCodexDesktop, ChannelCLI, ChannelCLI, ChannelCLI}
	wantAdapters := []string{"codex", "codex", "codex", "kimi"}
	for i := range list {
		if list[i].Adapter != wantAdapters[i] || list[i].Channel != wantChannels[i] {
			t.Fatalf("第 %d 项应为 %s/%s，got %+v", i, wantAdapters[i], wantChannels[i], list[i])
		}
	}
	if list[1].Path > list[2].Path {
		t.Fatalf("同优先级内应按 path 排序：%+v", list)
	}
	// 全部启用后 EnabledList 同序
	for _, e := range list {
		if err := reg.SetEnabled(e.ID, true); err != nil {
			t.Fatalf("enable %s: %v", e.ID, err)
		}
	}
	enabled := reg.EnabledList()
	if enabled[0].Channel != ChannelAppCodexDesktop || enabled[3].Adapter != "kimi" {
		t.Fatalf("EnabledList 排序不符：%+v", enabled)
	}
}

// ZCode 桌面应用实例（实证 2026-09-23）：NSIS 默认位经 AppGlobs 发现，命中目录
// 须同存内嵌 bundle（resources/glm/zcode.cjs）才算有效；版本探测走
// [exe, bundle, --version] + ELECTRON_RUN_AS_NODE=1 注入；家族裁定 CLI 优先于
// 桌面垫片（app:zcode-desktop 排后）。
func TestScanZcodeDesktopAppInstance(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = `C:/Users/u`
	appDir := `C:/Users/u/AppData/Local/Programs/ZCode`
	runner.globs[`C:/Users/u/AppData/Local/Programs/ZCode`] = []string{appDir}
	bundle := appDir + `/resources/glm/zcode.cjs`
	runner.exists["native||"+bundle] = true
	runner.exists["native||"+appDir+"/zcode.exe"] = true
	runner.runs["native||"+appDir+"/zcode.exe "+bundle+" --version"] = "0.16.9\n"
	runner.exists["native||C:/Users/u/.zcode/v2/credentials.json"] = true
	// 同机独立 CLI（优先级对照）
	cliPath := `C:/Users/u/.zcode/bin/zcode`
	runner.lookups["native||zcode"] = cliPath
	runner.exists["native||"+cliPath] = true
	runner.runs["native||"+cliPath+" --version"] = "0.16.9\n"

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("应发现 CLI+App 两实例，got %+v", list)
	}
	if list[0].Channel != ChannelCLI || list[1].Channel != ChannelAppZcodeDesktop {
		t.Fatalf("家族裁定 CLI 优先于桌面垫片：%+v", list)
	}
	if list[0].Priority >= list[1].Priority {
		t.Fatalf("CLI 优先级数值应更小：%+v", list)
	}
	app := list[1]
	if app.Bundle != bundle {
		t.Fatalf("App 实例应携带内嵌 bundle 路径：%+v", app)
	}
	if app.Version != "0.16.9" || app.Login != LoginLoggedIn {
		t.Fatalf("bundle 实例版本探测与登录态不符：%+v", app)
	}
	if len(runner.envCalls) == 0 {
		t.Fatal("bundle 实例版本探测应经 RunWithEnv 注入 ELECTRON_RUN_AS_NODE")
	}
	found := false
	for _, env := range runner.envCalls {
		for _, kv := range env {
			if kv == "ELECTRON_RUN_AS_NODE=1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("版本探测 env 应注入 ELECTRON_RUN_AS_NODE=1：%v", runner.envCalls)
	}
}

// 缺内嵌 bundle 的裸壳目录不算可驱动实例（版本探测必败——CLI 模式强制定位
// provider/zcode-builtin.json）：跳过不登记。
func TestScanZcodeDesktopSkipsWithoutBundle(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = `C:/Users/u`
	appDir := `C:/Users/u/AppData/Local/Programs/ZCode`
	runner.globs[`C:/Users/u/AppData/Local/Programs/ZCode`] = []string{appDir}
	runner.exists["native||"+appDir+"/zcode.exe"] = true // 有 exe、无 bundle

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, e := range reg.List() {
		if e.Adapter == "zcode" {
			t.Fatalf("缺 bundle 的目录不得登记 zcode 实例：%+v", e)
		}
	}
}

// 手动登记 zcode .exe（非标准安装位）：按约定布局自动填 Bundle
// （.exe 即桌面形态推断——避免裸 GUI 探测；同目录存在 bundle 时同构生效）。
func TestManualAddZcodeDesktopBundleInference(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = `C:/Users/u`
	appDir := `D:/Apps/ZCode`
	bundle := appDir + `/resources/glm/zcode.cjs`
	runner.exists["native||"+bundle] = true
	runner.exists["native||"+appDir+"/ZCode.exe"] = true
	runner.runs["native||"+appDir+"/ZCode.exe "+bundle+" --version"] = "0.16.9\n"
	runner.exists["native||C:/Users/u/.zcode/v2/provider_config.json"] = true

	if err := reg.AddManual(context.Background(), runner, Executable{
		Adapter: "zcode", Runtime: "native", Path: appDir + "/ZCode.exe",
		Channel: ChannelAppZcodeDesktop,
	}); err != nil {
		t.Fatalf("add manual: %v", err)
	}
	list := reg.List()
	if len(list) != 1 || list[0].Bundle != bundle {
		t.Fatalf("手动登记应按约定布局自动填 Bundle：%+v", list)
	}
}
