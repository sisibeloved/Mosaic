// M4-4（ADR-0013）：稳定 Bot 身份——消失重绑规则回归。覆盖：单候选重绑
// （身份/启用/模型覆盖迁移）、多实例并存不绑、歧义不绑、登录硬门、手动项
// 免疫、WSL 暂时不可达不判死、存量文件祖父化零迁移。
package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// scanCodexAt 以给定 PATH 位置执行一次扫描（fake 探测面）。
func scanCodexAt(t *testing.T, reg *Registry, runner *fakeRunner, path, version, loginOut string) {
	t.Helper()
	runner.lookups["native||codex"] = path
	runner.runs["native||"+path+" --version"] = version + "\n"
	runner.runs["native||"+path+" login status"] = loginOut
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

func TestRebindSingleCandidateKeepsIdentity(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	scanCodexAt(t, reg, runner, "/old/codex", "codex-cli 1.0", "Logged in using ChatGPT\n")
	first := reg.List()[0]
	if err := reg.SetEnabled(first.ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := reg.UpdateRuntime(first.ID, "gpt-5-codex", "medium"); err != nil {
		t.Fatalf("runtime: %v", err)
	}
	oldBot := first.BotIDOf()

	// nvm 升级语义：路径变化，旧路径不复存在（fake exists 未登记即不存在）
	scanCodexAt(t, reg, runner, "/new/codex", "codex-cli 1.1", "Logged in using ChatGPT\n")

	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("旧项应被承接移除，剩 1 项，got %d: %+v", len(list), list)
	}
	n := list[0]
	if n.Path != "/new/codex" {
		t.Fatalf("应保留新路径: %+v", n)
	}
	if n.BotIDOf() != oldBot {
		t.Fatalf("稳定身份应延续: old=%s new=%s", oldBot, n.BotIDOf())
	}
	if !n.Enabled || n.Model != "gpt-5-codex" || n.ReasoningEffort != "medium" {
		t.Fatalf("用户意图（启用/模型覆盖）应迁移: %+v", n)
	}
	if notes := reg.DrainNotes(); len(notes) == 0 {
		t.Fatal("重绑应有可观测注记")
	}
}

func TestRebindMultiInstanceNoAdopt(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	scanCodexAt(t, reg, runner, "/a/codex", "codex-cli 1.0", "Logged in\n")
	botA := reg.List()[0].BotIDOf()

	// 新实例出现且旧实例仍在（多实例并存——ADR-0012 C 轨）
	runner.globs["/home/u/.local/bin"] = []string{"/home/u/.local/bin"}
	runner.exists["native||/home/u/.local/bin/codex"] = true
	runner.runs["native||/home/u/.local/bin/codex --version"] = "codex-cli 1.1\n"
	runner.runs["native||/home/u/.local/bin/codex login status"] = "Logged in\n"
	scanCodexAt(t, reg, runner, "/a/codex", "codex-cli 1.0", "Logged in\n")

	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("多实例并存应为 2 项: %+v", list)
	}
	for _, e := range list {
		if e.BotIDOf() == "" {
			t.Fatalf("新实例应有自身身份: %+v", e)
		}
	}
	if list[0].BotIDOf() == list[1].BotIDOf() {
		t.Fatalf("并存实例身份不得合并: %+v", list)
	}
	if reg.List()[0].Adapter != "codex" || botA == "" {
		t.Fatalf("原实例身份在席: %+v", list)
	}
	if notes := reg.DrainNotes(); len(notes) != 0 {
		t.Fatalf("并存不绑不应有迁移注记: %v", notes)
	}
}

func TestRebindAmbiguousNoAdopt(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	scanCodexAt(t, reg, runner, "/old/codex", "codex-cli 1.0", "Logged in\n")
	oldBot := reg.List()[0].BotIDOf()

	// 旧路径消失，同时出现两个新实例（PATH + glob 各一）——歧义不自动绑
	runner.globs["/home/u/.local/bin"] = []string{"/home/u/.local/bin"}
	runner.exists["native||/home/u/.local/bin/codex"] = true
	runner.runs["native||/home/u/.local/bin/codex --version"] = "codex-cli 1.1\n"
	runner.runs["native||/home/u/.local/bin/codex login status"] = "Logged in\n"
	scanCodexAt(t, reg, runner, "/n1/codex", "codex-cli 1.2", "Logged in\n")

	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("消亡项移除 + 两个新实例 = 2 项: %+v", list)
	}
	for _, e := range list {
		if e.BotIDOf() == oldBot {
			t.Fatalf("歧义场景不得自动承接旧身份: %+v", e)
		}
	}
}

func TestRebindLoginGate(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	scanCodexAt(t, reg, runner, "/old/codex", "codex-cli 1.0", "Logged in\n")
	first := reg.List()[0]
	_ = reg.SetEnabled(first.ID, true)
	oldBot := first.BotIDOf()

	// 新位置未登录：身份与模型迁移，但启用不迁移（登录硬门）
	scanCodexAt(t, reg, runner, "/new/codex", "codex-cli 1.1", "Error: not logged in\x00EXIT:1")

	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("单候选重绑应合并为 1 项: %+v", list)
	}
	n := list[0]
	if n.BotIDOf() != oldBot {
		t.Fatalf("身份应延续: %+v", n)
	}
	if n.Enabled {
		t.Fatalf("未登录的新实例不得继承启用态: %+v", n)
	}
}

func TestManualEntryImmuneToRebindAndPrune(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	// 手动登记项（路径 m）
	runner.runs["native||/m/codex --version"] = "codex-cli 0.9\n"
	runner.runs["native||/m/codex login status"] = "Logged in\n"
	if err := reg.AddManual(context.Background(), runner, Executable{
		Adapter: "codex", Runtime: "native", Path: "/m/codex"}); err != nil {
		t.Fatalf("manual: %v", err)
	}
	manualBot := reg.List()[0].BotIDOf()

	// 扫描只发现另一个位置：手动项不被自动重绑、不被清理
	scanCodexAt(t, reg, runner, "/n/codex", "codex-cli 1.0", "Logged in\n")

	var manual, auto *Executable
	for i, e := range reg.List() {
		if e.Source == SourceManual {
			manual = &reg.List()[i]
		} else {
			auto = &reg.List()[i]
		}
	}
	if manual == nil || auto == nil {
		t.Fatalf("手动项与自动项应并存: %+v", reg.List())
	}
	if manual.Path != "/m/codex" || manual.BotIDOf() != manualBot {
		t.Fatalf("手动项不受重绑影响: %+v", manual)
	}
	if auto.BotIDOf() == manualBot {
		t.Fatalf("自动发现项不得承接手动项身份: %+v", auto)
	}
}

func TestWSLUnreachableNotPrunedOrRebound(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	runner.homes["wsl|openEuler-24.03"] = "/home/w"
	runner.distros = []string{"openEuler-24.03"}
	// WSL 内发现 kimi（凭证文件判定登录）
	runner.exists["wsl|openEuler-24.03|/home/w/.kimi-code/credentials/kimi-code.json"] = true
	runner.globs["/home/w/.kimi-code/bin"] = []string{"/home/w/.kimi-code/bin"}
	runner.exists["wsl|openEuler-24.03|/home/w/.kimi-code/bin/kimi"] = true
	runner.runs["wsl|openEuler-24.03||/home/w/.kimi-code/bin/kimi --version"] = "0.39.1\n"
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{IncludeWSL: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var wslEntry *Executable
	for i, e := range reg.List() {
		if e.Runtime == "wsl" {
			wslEntry = &reg.List()[i]
		}
	}
	if wslEntry == nil {
		t.Fatalf("应发现 WSL kimi: %+v", reg.List())
	}
	wslBot := wslEntry.BotIDOf()

	// 发行版整体下线（暂时查不到）：不判死、不清理、不重绑
	runner.distros = nil
	// native 面出现同族 kimi（假想 native 安装）
	runner.lookups["native||kimi"] = "/native/kimi"
	runner.runs["native||/native/kimi --version"] = "0.40.0\n"
	runner.exists["native||/home/u/.kimi-code/credentials/kimi-code.json"] = true
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{IncludeWSL: true}); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	var stillWSL, nativeKimi *Executable
	for i, e := range reg.List() {
		if e.Runtime == "wsl" {
			stillWSL = &reg.List()[i]
		}
		if e.Path == "/native/kimi" {
			nativeKimi = &reg.List()[i]
		}
	}
	if stillWSL == nil {
		t.Fatalf("WSL 暂时不可达的项应保留: %+v", reg.List())
	}
	if stillWSL.BotIDOf() != wslBot {
		t.Fatalf("WSL 项身份不应变化: %+v", stillWSL)
	}
	if nativeKimi != nil && nativeKimi.BotIDOf() == wslBot {
		t.Fatalf("native 项不得承接暂时不可达项的身份: %+v", nativeKimi)
	}
}

func TestGrandfatherBotIDZeroMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness-registry.json")
	// 旧版本格式：无 bot_id 字段
	old := map[string]any{
		"executables": []map[string]any{{
			"id": "codex@native:/old/codex", "adapter": "codex", "runtime": "native",
			"path": "/old/codex", "login_state": "logged_in", "source": "auto_scan",
			"channel": "cli", "enabled": true,
		}},
	}
	raw, _ := json.Marshal(old)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	reg, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	list := reg.List()
	if len(list) != 1 || list[0].BotIDOf() != "codex@native:/old/codex" {
		t.Fatalf("祖父化应补 BotID=ID: %+v", list)
	}
	// 落盘持久（重启后仍在）
	reg2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reg2.List()[0].BotIDOf() != "codex@native:/old/codex" {
		t.Fatalf("祖父化应已持久化: %+v", reg2.List())
	}
}
