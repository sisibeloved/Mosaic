// v1.48 运行参数 UT：模型覆盖与思考强度——注册表更新/校验/跨扫描保留 +
// 候选面（kimi 动态实查解析、codex 五档、mcode 空面）。
package harness

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func runtimeTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := LoadOrCreate(filepath.Join(t.TempDir(), "reg.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg.exes = []Executable{
		{ID: "codex_1", Adapter: "codex", Runtime: "native", Path: "/bin/codex", Enabled: true},
		{ID: "kimi_1", Adapter: "kimi", Runtime: "wsl", Distro: "d1", Path: "/home/u/.kimi-code/bin/kimi", Enabled: true},
		{ID: "mcode_1", Adapter: "minimax", Runtime: "wsl", Distro: "d1", Path: "/home/u/.nvm/versions/node/v24/bin/mcode", Enabled: true},
	}
	return reg
}

func TestUpdateRuntime(t *testing.T) {
	reg := runtimeTestRegistry(t)
	if err := reg.UpdateRuntime("codex_1", "gpt-5.6-sol", "xhigh"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	got := reg.List()
	if got[0].Model != "gpt-5.6-sol" || got[0].ReasoningEffort != "xhigh" {
		t.Fatalf("更新未生效: %+v", got[0])
	}
	// 非法档位
	if err := reg.UpdateRuntime("codex_1", "m", "ultra"); err == nil {
		t.Fatal("非法 effort 应拒绝")
	}
	// 清除覆盖（空串回 CLI 默认）
	if err := reg.UpdateRuntime("codex_1", "", ""); err != nil {
		t.Fatal(err)
	}
	got = reg.List()
	if got[0].Model != "" || got[0].ReasoningEffort != "" {
		t.Fatalf("空串应清除覆盖: %+v", got[0])
	}
	// 不存在
	if err := reg.UpdateRuntime("nope", "m", ""); err == nil {
		t.Fatal("不存在实例应报 ErrNotFound")
	}
	// kimi 拒绝强度档位语义（登记为空面——写入侧不拦 adapter，候选面不呈现；
	// 端到端由 RuntimeOptionsOf 的空 effort_levels 表达）
	_ = reg
}

func TestUpdateRuntimeSurvivesScan(t *testing.T) {
	reg := runtimeTestRegistry(t)
	if err := reg.UpdateRuntime("codex_1", "gpt-5.6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	// 再扫描同实例（探测字段刷新）：手编运行参数必须保留
	runner := &fakeRunner{
		lookups: map[string]string{"native||codex": "/bin/codex"},
		runs:    map[string]string{"native||/bin/codex --version": "codex 1.0"},
		exists:  map[string]bool{"native||/bin/codex": true},
	}
	spec := ProbeSpec{Adapter: "codex", Binary: "codex", VersionArgs: []string{"--version"}}
	if err := reg.Scan(context.Background(), runner, []ProbeSpec{spec}, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, e := range reg.List() {
		if e.ID == "codex_1" {
			if e.Model != "gpt-5.6-sol" || e.ReasoningEffort != "high" {
				t.Fatalf("运行参数被扫描冲掉: %+v", e)
			}
			return
		}
	}
	t.Fatal("扫描后实例丢失")
}

const kimiProviderJSON = `{
  "providers": {"managed:kimi-code": {"baseUrl": "https://api.kimi.com/coding/v1"}},
  "models": {
    "kimi-code/k3-256k": {"displayName": "K3 256K"},
    "kimi-code/kimi-for-coding": {"displayName": "K2.7 Coding"},
    "kimi-code/kimi-for-coding-highspeed": {"displayName": "K2.7 Coding Highspeed"}
  }
}`

func TestRuntimeOptionsKimiDynamic(t *testing.T) {
	reg := runtimeTestRegistry(t)
	runner := &fakeRunner{
		runs: map[string]string{
			"wsl|d1|/home/u/.kimi-code/bin/kimi provider list --json": kimiProviderJSON,
		},
	}
	opts, err := reg.RuntimeOptionsOf(context.Background(), "kimi_1", runner)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Dynamic || len(opts.EffortLevels) != 0 {
		t.Fatalf("kimi 应 dynamic=true 且无强度面: %+v", opts)
	}
	if len(opts.Models) != 3 {
		t.Fatalf("模型候选数 = %d: %+v", len(opts.Models), opts.Models)
	}
	// 确定性排序（alias 字典序）
	if opts.Models[0].ID != "kimi-code/k3-256k" || opts.Models[2].ID != "kimi-code/kimi-for-coding-highspeed" {
		t.Fatalf("候选排序不确定: %+v", opts.Models)
	}
	if opts.Models[0].DisplayName != "K3 256K" {
		t.Fatalf("displayName 丢失: %+v", opts.Models[0])
	}
}

func TestRuntimeOptionsKimiQueryFailFallback(t *testing.T) {
	reg := runtimeTestRegistry(t)
	runner := &fakeRunner{runs: map[string]string{
		"wsl|d1|/home/u/.kimi-code/bin/kimi provider list --json": "boom\x00EXIT:1",
	}}
	opts, err := reg.RuntimeOptionsOf(context.Background(), "kimi_1", runner)
	if err != nil {
		t.Fatalf("查询失败不应报错（回退空候选）: %v", err)
	}
	if !opts.Dynamic || len(opts.Models) != 0 {
		t.Fatalf("失败回退应空候选+dynamic=true: %+v", opts)
	}
}

func TestRuntimeOptionsCodexAndMinimax(t *testing.T) {
	reg := runtimeTestRegistry(t)
	opts, err := reg.RuntimeOptionsOf(context.Background(), "codex_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Dynamic || len(opts.Models) != 0 {
		t.Fatalf("codex 无官方列表命令: %+v", opts)
	}
	if strings.Join(opts.EffortLevels, ",") != "minimal,low,medium,high,xhigh" {
		t.Fatalf("codex 五档: %+v", opts.EffortLevels)
	}
	opts, err = reg.RuntimeOptionsOf(context.Background(), "mcode_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Models) != 0 || len(opts.EffortLevels) != 0 || opts.Dynamic {
		t.Fatalf("mcode 空面: %+v", opts)
	}
	if _, err := reg.RuntimeOptionsOf(context.Background(), "nope", nil); err == nil {
		t.Fatal("不存在实例应报错")
	}
}

// v1.49 确定量默认值：配置文件优先（顶层键——嵌套表内同名键不参与），
// 官方/出厂常量回退。
func TestRuntimeDefaultsFromConfig(t *testing.T) {
	reg := runtimeTestRegistry(t)

	// codex：顶层 model/model_reasoning_effort；[profiles.*] 内同名键必须被忽略
	runner := &fakeRunner{
		homes: map[string]string{"native|": "/home/u"},
		files: map[string]string{
			"/home/u/.codex/config.toml": "personality = \"pragmatic\"\n" +
				"model = \"gpt-5.6-sol\"\n" +
				"model_reasoning_effort = \"xhigh\"\n" +
				"service_tier = \"fast\"\n" +
				"[profiles.p]\nmodel = \"nested-ignored\"\n",
		},
	}
	opts, err := reg.RuntimeOptionsOf(context.Background(), "codex_1", runner)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultModel != "gpt-5.6-sol" || opts.DefaultEffort != "xhigh" || opts.DefaultSource != "config" {
		t.Fatalf("codex 配置默认值 = %s/%s/%s，期望 gpt-5.6-sol/xhigh/config", opts.DefaultModel, opts.DefaultEffort, opts.DefaultSource)
	}

	// kimi：default_model（[models.*] 段的 model 是目录定义，不参与）
	runner2 := &fakeRunner{
		homes: map[string]string{"wsl|d1": "/home/u"},
		files: map[string]string{
			"/home/u/.kimi-code/config.toml": "default_model = \"kimi-code/k3\"\n[models.\"kimi-code/k3\"]\nmodel = \"k3\"\nsupport_efforts = [\"low\"]\n",
		},
	}
	opts, err = reg.RuntimeOptionsOf(context.Background(), "kimi_1", runner2)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultModel != "kimi-code/k3" || opts.DefaultEffort != "" || opts.DefaultSource != "config" {
		t.Fatalf("kimi 配置默认值: %+v", opts)
	}

	// mcode：config.yaml 顶层 defaultModel（缩进的嵌套 models 不参与）
	runner3 := &fakeRunner{
		homes: map[string]string{"wsl|d1": "/home/u"},
		files: map[string]string{
			"/home/u/.minimax/config.yaml": "agent:\n  models:\n    m: 1\ndefaultModel: minimax/MiniMax-M3\n",
		},
	}
	opts, err = reg.RuntimeOptionsOf(context.Background(), "mcode_1", runner3)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultModel != "minimax/MiniMax-M3" || opts.DefaultSource != "config" {
		t.Fatalf("mcode 配置默认值: %+v", opts)
	}
}

func TestRuntimeDefaultsFallback(t *testing.T) {
	reg := runtimeTestRegistry(t)

	// codex 无配置文件：模型 = CLI 内置预设（官方未公布常量，不虚构），
	// 强度回退官方默认 medium
	runner := &fakeRunner{homes: map[string]string{"native|": "/home/u"}}
	opts, err := reg.RuntimeOptionsOf(context.Background(), "codex_1", runner)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultModel != "" || opts.DefaultEffort != "medium" || opts.DefaultSource != "builtin" {
		t.Fatalf("codex 回退默认值: %+v", opts)
	}

	// kimi 无配置：出厂默认（首启生成的 default_model 实证值）
	runner2 := &fakeRunner{homes: map[string]string{"wsl|d1": "/home/u"}}
	opts, err = reg.RuntimeOptionsOf(context.Background(), "kimi_1", runner2)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultModel != "kimi-code/k3-256k" || opts.DefaultSource != "builtin" {
		t.Fatalf("kimi 回退默认值: %+v", opts)
	}

	// nil runner（无探测面）同样回退——确定量不依赖探测可用性
	opts, err = reg.RuntimeOptionsOf(context.Background(), "kimi_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultModel != "kimi-code/k3-256k" {
		t.Fatalf("nil runner 回退: %+v", opts)
	}

	// mcode 回退
	opts, err = reg.RuntimeOptionsOf(context.Background(), "mcode_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DefaultModel != "minimax/MiniMax-M3" {
		t.Fatalf("mcode 回退: %+v", opts)
	}
}
