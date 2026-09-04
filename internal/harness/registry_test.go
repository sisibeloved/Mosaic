// UT 层：宿主层可执行程序注册表（RFC-0002 双层管理的宿主面）——
// 自动扫描（native + WSL）、手动配置、登录态门控、持久化、合并规则。
// TDD：本文件先行于实现（红→绿）。Runner 用 fake，真实探测见 IT。
package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner 模拟目标运行时（native 与 wsl 发行版）的探测面。
type fakeRunner struct {
	// 路径 → 版本输出
	lookups map[string]string   // key: runtime|distro|binary → path（空串=未找到）
	runs    map[string]string   // key: runtime|distro|argsJoined → stdout（"\x00EXIT:n" 后缀控制退出码）
	exists  map[string]bool     // key: runtime|distro|path
	homes   map[string]string   // key: runtime|distro
	digests map[string]string   // key: path
	globs   map[string][]string // key: pattern → 展开结果
	files   map[string]string   // key: path → 文件内容（ReadFile 面）
	distros []string
}

func (f *fakeRunner) key(runtime Runtime, distro string) string {
	return string(runtime) + "|" + distro
}

func (f *fakeRunner) LookPath(ctx context.Context, runtime Runtime, distro, binary string) (string, bool) {
	path, ok := f.lookups[f.key(runtime, distro)+"|"+binary]
	return path, ok && path != ""
}

func (f *fakeRunner) Run(ctx context.Context, runtime Runtime, distro string, args []string) (string, int, error) {
	out := f.runs[f.key(runtime, distro)+"|"+strings.Join(args, " ")]
	if code := 0; strings.Contains(out, "\x00") {
		parts := strings.SplitN(out, "\x00", 2)
		out = parts[0]
		if parts[1] == "EXIT:1" {
			code = 1
		}
		return out, code, nil
	}
	return out, 0, nil
}

func (f *fakeRunner) Home(ctx context.Context, runtime Runtime, distro string) string {
	return f.homes[f.key(runtime, distro)]
}

func (f *fakeRunner) Exists(ctx context.Context, runtime Runtime, distro, path string) bool {
	return f.exists[f.key(runtime, distro)+"|"+path]
}

func (f *fakeRunner) Digest(ctx context.Context, runtime Runtime, distro, path string) (string, error) {
	if d, ok := f.digests[path]; ok {
		return d, nil
	}
	return "sha256:fake", nil
}

func (f *fakeRunner) WSLDistros(ctx context.Context) []string { return f.distros }

func (f *fakeRunner) Glob(ctx context.Context, runtime Runtime, distro, pattern string) []string {
	return f.globs[pattern]
}

func (f *fakeRunner) ReadFile(ctx context.Context, runtime Runtime, distro, path string) (string, bool) {
	content, ok := f.files[path]
	return content, ok
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		lookups: map[string]string{},
		runs:    map[string]string{},
		exists:  map[string]bool{},
		homes:   map[string]string{},
		digests: map[string]string{},
		globs:   map[string][]string{},
		files:   map[string]string{},
	}
}

func tempRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness-registry.json")
	reg, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return reg, path
}

func TestRegistryListEmptyMarshalsAsJSONArray(t *testing.T) {
	reg, _ := tempRegistry(t)

	raw, err := json.Marshal(map[string]any{"executables": reg.List()})
	if err != nil {
		t.Fatalf("marshal empty registry: %v", err)
	}
	if got, want := string(raw), `{"executables":[]}`; got != want {
		t.Fatalf("empty registry JSON = %s, want %s", got, want)
	}
}

func TestScanNativeDiscoversProbesAndGates(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.lookups["native||codex"] = "/usr/local/bin/codex"
	runner.runs["native||/usr/local/bin/codex --version"] = "codex-cli 0.149.1\n"
	runner.runs["native||/usr/local/bin/codex login status"] = "Logged in using ChatGPT\n"
	runner.digests["/usr/local/bin/codex"] = "sha256:abc"
	runner.homes["native|"] = "/home/u"

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("应发现 1 个可执行程序，got %d: %+v", len(list), list)
	}
	codex := list[0]
	if codex.Adapter != "codex" || codex.Runtime != "native" || codex.Path != "/usr/local/bin/codex" {
		t.Fatalf("发现项不符：%+v", codex)
	}
	if codex.Version != "codex-cli 0.149.1" {
		t.Fatalf("version = %q", codex.Version)
	}
	if codex.Login != LoginLoggedIn {
		t.Fatalf("login = %q（期望 logged_in）", codex.Login)
	}
	if codex.Digest != "sha256:abc" || codex.Source != SourceAuto {
		t.Fatalf("digest/source 不符：%+v", codex)
	}
	// 已登录 → 可启用
	if err := reg.SetEnabled(codex.ID, true); err != nil {
		t.Fatalf("已登录者应可启用: %v", err)
	}
}

func TestScanLoggedOutDetected(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.lookups["native||codex"] = "/usr/bin/codex"
	runner.runs["native||--version"] = "codex-cli 0.149.1\n"
	runner.runs["native||/usr/bin/codex login status"] = "Error: not logged in\x00EXIT:1"

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := reg.List()[0].Login; got != LoginLoggedOut {
		t.Fatalf("login = %q（期望 logged_out）", got)
	}
	// 未登录 → 启用被门控拒绝（负责人要求 3）
	if err := reg.SetEnabled(reg.List()[0].ID, true); err == nil {
		t.Fatal("未登录不得启用")
	}
}

func TestScanKimiLoginViaCredFile(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.lookups["native||kimi"] = "/home/u/.kimi-code/bin/kimi"
	runner.runs["native||--version"] = "0.38.0\n"
	runner.homes["native|"] = "/home/u"
	runner.exists["native||/home/u/.kimi-code/credentials/kimi-code.json"] = true

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	kimi := reg.List()[0]
	if kimi.Adapter != "kimi" || kimi.Login != LoginLoggedIn {
		t.Fatalf("kimi 发现/登录态不符：%+v", kimi)
	}
}

// 负责人要求 1：Windows 宿主要扫描 WSL 内安装的 CLI。
func TestScanCoversWSLDistros(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.distros = []string{"Ubuntu", "openEuler-24.03"}
	runner.lookups["wsl|openEuler-24.03|codex"] = "/home/wslu/.nvm/versions/node/v24.14.1/bin/codex"
	runner.runs["wsl|openEuler-24.03|--version"] = "codex-cli 0.149.1\n"
	runner.runs["wsl|openEuler-24.03|/home/wslu/.nvm/versions/node/v24.14.1/bin/codex login status"] = "Logged in using ChatGPT\n"
	runner.homes["wsl|openEuler-24.03"] = "/home/wslu"

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{IncludeWSL: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("应发现 WSL 内的 codex，got %+v", list)
	}
	exe := list[0]
	if exe.Runtime != "wsl" || exe.Distro != "openEuler-24.03" {
		t.Fatalf("runtime/distro 不符：%+v", exe)
	}
	// Ubuntu 里没装 → 不出现
	for _, e := range list {
		if e.Distro == "Ubuntu" {
			t.Fatalf("未安装的发行版不应出现：%+v", e)
		}
	}
}

// 负责人要求 2：手动配置——PATH 外/未扫描到的可执行程序可手工登记。
func TestManualAddAndLoginProbe(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.lookups["native||codex"] = "/opt/tools/codex"
	runner.runs["native||/opt/tools/codex --version"] = "codex-cli 0.149.1\n"
	runner.runs["native||/opt/tools/codex login status"] = "Logged in using ChatGPT\n"

	if err := reg.AddManual(context.Background(), runner, Executable{
		Adapter: "codex", Runtime: "native", Path: "/opt/tools/codex",
	}); err != nil {
		t.Fatalf("add manual: %v", err)
	}
	manual := reg.List()[0]
	if manual.Source != SourceManual || manual.Login != LoginLoggedIn || manual.Version != "codex-cli 0.149.1" {
		t.Fatalf("手动项探测不符：%+v", manual)
	}
	if err := reg.SetEnabled(manual.ID, true); err != nil {
		t.Fatalf("已登录手动项应可启用: %v", err)
	}
}

// 扫描不得清除手动项；重复扫描按 ID 刷新 auto 项（版本/登录态更新）。
func TestScanMergePreservesManualAndRefreshes(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.lookups["native||codex"] = "/usr/bin/codex"
	runner.runs["native||/usr/bin/codex --version"] = "codex-cli 0.149.1\n"
	runner.runs["native||/usr/bin/codex login status"] = "Logged in using ChatGPT\n"
	runner.homes["native|"] = "/home/u"
	runner.lookups["native||kimi"] = "/home/u/.kimi-code/bin/kimi"
	runner.exists["native||/home/u/.kimi-code/credentials/kimi-code.json"] = false

	// 手动登记 kimi（未登录）
	if err := reg.AddManual(context.Background(), runner, Executable{
		Adapter: "kimi", Runtime: "native", Path: "/home/u/.kimi-code/bin/kimi",
	}); err != nil {
		t.Fatalf("manual kimi: %v", err)
	}

	// 扫描：发现 codex（auto）；kimi 手动项保留且登录态被刷新为已登录
	runner.exists["native||/home/u/.kimi-code/credentials/kimi-code.json"] = true
	runner.runs["native||/usr/bin/codex --version"] = "codex-cli 0.150.0\n" // 升级
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	byAdapter := map[string]Executable{}
	for _, e := range reg.List() {
		byAdapter[e.Adapter] = e
	}
	if len(byAdapter) != 2 {
		t.Fatalf("应共存 2 项：%+v", reg.List())
	}
	if byAdapter["codex"].Version != "codex-cli 0.150.0" {
		t.Fatalf("auto 项版本未刷新：%+v", byAdapter["codex"])
	}
	if byAdapter["kimi"].Source != SourceManual {
		t.Fatalf("手动项被扫描清除：%+v", byAdapter["kimi"])
	}
	if byAdapter["kimi"].Login != LoginLoggedIn {
		t.Fatalf("手动项登录态未刷新：%+v", byAdapter["kimi"])
	}
}

func TestPersistRoundTrip(t *testing.T) {
	reg, path := tempRegistry(t)
	runner := newFakeRunner()
	runner.lookups["native||codex"] = "/usr/bin/codex"
	runner.runs["native||--version"] = "codex-cli 0.149.1\n"
	runner.runs["native||/usr/bin/codex login status"] = "Logged in using ChatGPT\n"
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := reg.SetEnabled(reg.List()[0].ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	reloaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := reloaded.List()
	if len(got) != 1 || !got[0].Enabled || got[0].Login != LoginLoggedIn {
		t.Fatalf("持久化往返丢失状态：%+v", got)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("registry 文件应为合法 JSON: %v", err)
	}
	// 原子写（M1 收口补课）：不留 tmp 残件
	if entries, err := os.ReadDir(filepath.Dir(path)); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("原子写不得残留 tmp 文件：%s", e.Name())
			}
		}
	}
}

func TestManualAddValidation(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	if err := reg.AddManual(context.Background(), runner, Executable{Adapter: "codex"}); err == nil {
		t.Fatal("缺 runtime/path 必须拒绝")
	}
	if err := reg.AddManual(context.Background(), runner, Executable{Adapter: "unknown-tool", Runtime: "native", Path: "/x"}); err == nil {
		t.Fatal("未知 adapter 必须拒绝（登记面限定已知家族）")
	}
}

func TestSetEnabledUnknownID(t *testing.T) {
	reg, _ := tempRegistry(t)
	if err := reg.SetEnabled("nope", true); err == nil {
		t.Fatal("未知 ID 必须报错")
	}
}

// 版本管理器形态（nvm 等）：PATH 看不见，靠已知目录 glob 发现（文件系统事实驱动）。
func TestScanFindsViaKnownDirGlobs(t *testing.T) {
	reg, _ := tempRegistry(t)
	runner := newFakeRunner()
	runner.homes["native|"] = "/home/u"
	// codex 不在 PATH
	// nvm 版本目录经 glob 展开
	nvmDir := "/home/u/.nvm/versions/node/v24.14.1/bin"
	runner.globs["/home/u/.nvm/versions/node/*/bin"] = []string{nvmDir}
	runner.exists["native||"+nvmDir+"/codex"] = true
	runner.runs["native||"+nvmDir+"/codex --version"] = "codex-cli 0.149.1\n"
	runner.runs["native||/home/u/.nvm/versions/node/v24.14.1/bin/codex login status"] = "Logged in using ChatGPT\n"

	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) != 1 || list[0].Adapter != "codex" || list[0].Path != nvmDir+"/codex" {
		t.Fatalf("应经 nvm glob 发现 codex：%+v", list)
	}
	if list[0].Login != LoginLoggedIn {
		t.Fatalf("login = %s", list[0].Login)
	}
}

func (f *fakeRunner) RunWithDir(ctx context.Context, runtime Runtime, distro, binDir string, args []string) (string, int, error) {
	return f.Run(ctx, runtime, distro, args)
}
