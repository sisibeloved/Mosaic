// 模型与思考强度的候选面（v1.48）：设置页下拉的数据源。
// 实证（2026-09-03，负责人本机）：
//   - kimi 0.39.1：`kimi provider list --json` 输出全量模型别名/DisplayName/
//     capabilities（含 thinking 能力标记）——唯一支持 CLI 实查的一家（动态）；
//   - codex：-m 任意模型名 + -c model_reasoning_effort 五档，但无列表子命令
//     （顶层仅 agents/exec/review/login/mcp/plugin/mcp-server/app-server）；
//   - mcode 0.2.7：exec --model provider/model，无列表子命令（provider list
//     只列凭据源）。
//
// 静态候选不瞎编（过时/错名比空列表更误导）——codex/mcode 返回空候选+自由输入；
// 官方列表命令出现后接动态查询（登记 M4 backlog）。
//
// v1.49 确定量默认值：不覆盖时的 CLI 默认模型/思考强度必须是确定量——优先读
// CLI 配置文件，缺失回退官方文档/出厂默认（负责人裁定）。
package harness

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// ModelOption 模型候选项。
type ModelOption struct {
	ID          string `json:"id"` // 传给 CLI 的模型名（kimi 为 alias，如 kimi-code/k3-256k）
	DisplayName string `json:"display_name,omitempty"`
}

// RuntimeOptions 某实例的运行参数候选面。
type RuntimeOptions struct {
	Models []ModelOption `json:"models"`
	// Dynamic 候选是否 CLI 实查（kimi）：false = 无官方列表命令（空候选+自由输入）。
	Dynamic bool `json:"dynamic"`
	// EffortLevels 思考强度档位（仅 codex 五档；空 = 该 adapter 无强度面）。
	EffortLevels []string `json:"effort_levels"`
	// DefaultModel/DefaultEffort 不覆盖时的 CLI 默认（v1.49 确定量）：配置文件值
	// 优先（source=config），缺失回退官方文档/出厂默认（source=builtin）。两者
	// 皆空 = 无任何确定量（codex 未配置时模型为 CLI 内置预设，官方未公布常量）。
	DefaultModel  string `json:"default_model,omitempty"`
	DefaultEffort string `json:"default_effort,omitempty"`
	DefaultSource string `json:"default_source,omitempty"` // config | builtin | ""
}

// codexEffortLevels codex model_reasoning_effort 档位（v1.48 实证：负责人
// config 在用 xhigh；CLI help 的 -c 通道接受 config.toml 全部键）。
var codexEffortLevels = []string{"minimal", "low", "medium", "high", "xhigh"}

var codexEffortSet = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

// adapterDefaultSpec CLI 默认值的确定量来源（实证 2026-09-01，负责人本机配置 +
// 官方文档）：
//   - codex 0.151.0：~/.codex/config.toml 顶层 model / model_reasoning_effort；
//     官方 config-reference 未给 model 文档化默认（内置预设随版本/登录方式轮换，
//     例值已到 gpt-5.5），model_reasoning_effort 内置默认 medium（官方确认）。
//   - kimi 0.39.1：~/.kimi-code/config.toml default_model（官方文档：首启自动
//     创建；-m 帮助文本"Defaults to default_model in config.toml"）；出厂值
//     取本机首启生成 default_model。无用户档位（思考内建于模型能力）。
//   - mcode 0.2.7：~/.minimax/config.yaml 顶层 defaultModel。
type adapterDefaultSpec struct {
	configPath     string // 家目录下配置文件相对路径
	tomlModelKey   string // TOML 顶层模型键
	tomlEffortKey  string // TOML 顶层强度键（仅 codex）
	yamlModelKey   string // YAML 顶层模型键（mcode）
	fallbackModel  string // 配置缺失回退（空 = CLI 内置预设未公布，不虚构）
	fallbackEffort string
}

var adapterDefaultSpecs = map[string]adapterDefaultSpec{
	"codex":   {configPath: ".codex/config.toml", tomlModelKey: "model", tomlEffortKey: "model_reasoning_effort", fallbackEffort: "medium"},
	"kimi":    {configPath: ".kimi-code/config.toml", tomlModelKey: "default_model", fallbackModel: "kimi-code/k3-256k"},
	"minimax": {configPath: ".minimax/config.yaml", yamlModelKey: "defaultModel", fallbackModel: "minimax/MiniMax-M3"},
}

// RuntimeOptionsOf 某实例的模型候选与强度档位：kimi 经 Runner 实查
// `provider list --json`（失败回退空候选+dynamic=true 保留——UI 提示查询失败
// 可手输）；codex 返回五档强度与空候选；其余空面。三家均附确定量默认值
// （v1.49：配置文件优先，官方/出厂回退）。
func (r *Registry) RuntimeOptionsOf(ctx context.Context, id string, runner Runner) (RuntimeOptions, error) {
	exes := r.List()
	var exe *Executable
	for i := range exes {
		if exes[i].ID == id {
			exe = &exes[i]
			break
		}
	}
	if exe == nil {
		return RuntimeOptions{}, ErrNotFound
	}
	defModel, defEffort, defSource := resolveDefaults(ctx, runner, exe)
	switch exe.Adapter {
	case "kimi":
		opts := RuntimeOptions{
			Models: []ModelOption{}, Dynamic: true, EffortLevels: []string{},
			DefaultModel: defModel, DefaultEffort: defEffort, DefaultSource: defSource,
		}
		if runner == nil {
			return opts, nil
		}
		out, code, err := runner.Run(ctx, Runtime(exe.Runtime), exe.Distro,
			[]string{exe.Path, "provider", "list", "--json"})
		if err != nil || code != 0 {
			return opts, nil // 查询失败不阻塞设置页：空候选 + 自由输入
		}
		opts.Models = parseKimiModels(out)
		return opts, nil
	case "codex":
		return RuntimeOptions{
			Models:        []ModelOption{}, // 无官方列表命令：自由输入（placeholder 示例如 gpt-5.6-sol）
			Dynamic:       false,
			EffortLevels:  append([]string(nil), codexEffortLevels...),
			DefaultModel:  defModel,
			DefaultEffort: defEffort,
			DefaultSource: defSource,
		}, nil
	default: // minimax 及后续适配器
		return RuntimeOptions{
			Models: []ModelOption{}, Dynamic: false, EffortLevels: []string{},
			DefaultModel: defModel, DefaultEffort: defEffort, DefaultSource: defSource,
		}, nil
	}
}

// resolveDefaults 解析某实例的 CLI 默认模型/强度（v1.49 确定量要求）：
// CLI 配置文件键 > 官方文档/出厂常量。读文件走 Runner（native/wsl 两面一致）；
// 只提取目标键值，不回传/不记录文件其余内容（配置文件可能含密钥）。
func resolveDefaults(ctx context.Context, runner Runner, exe *Executable) (model, effort, source string) {
	spec, ok := adapterDefaultSpecs[exe.Adapter]
	if !ok || runner == nil {
		if ok {
			model, effort = spec.fallbackModel, spec.fallbackEffort
			if model != "" || effort != "" {
				return model, effort, "builtin"
			}
		}
		return "", "", ""
	}
	fromConfig := false
	if home := runner.Home(ctx, Runtime(exe.Runtime), exe.Distro); home != "" {
		if content, ok := runner.ReadFile(ctx, Runtime(exe.Runtime), exe.Distro, home+"/"+spec.configPath); ok {
			if spec.yamlModelKey != "" {
				model = yamlTopLevelString(content, spec.yamlModelKey)
			} else {
				model = tomlTopLevelString(content, spec.tomlModelKey)
				effort = tomlTopLevelString(content, spec.tomlEffortKey)
			}
			fromConfig = model != "" || effort != ""
		}
	}
	if model == "" {
		model = spec.fallbackModel
	}
	if effort == "" {
		effort = spec.fallbackEffort
	}
	if model == "" && effort == "" {
		return "", "", "" // codex 未配置：模型为 CLI 内置预设（官方未公布常量，不虚构）
	}
	if fromConfig {
		return model, effort, "config"
	}
	return model, effort, "builtin"
}

// tomlTopLevelString 取 TOML 顶层键（首个 [table] 头之前的裸键）。嵌套表内
// 同名键不参与——codex [profiles.*].model 与顶层 model 语义不同、kimi
// [models.*] 段的 model 是模型目录定义而非默认值。
func tomlTopLevelString(content, key string) string {
	if key == "" {
		return ""
	}
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "[") {
			return "" // 表头：顶层区结束
		}
		if !strings.HasPrefix(ln, key) {
			continue
		}
		rest := strings.TrimSpace(ln[len(key):])
		if !strings.HasPrefix(rest, "=") {
			continue // 是更长键（如 model_reasoning_effort 之于 model）
		}
		val := strings.TrimSpace(strings.TrimPrefix(rest, "="))
		if len(val) >= 2 && val[0] == '"' {
			if end := strings.Index(val[1:], `"`); end >= 0 {
				return val[1 : 1+end]
			}
		}
		return ""
	}
	return ""
}

// yamlTopLevelString 取 YAML 顶层标量键（列 0 的 key: value——嵌套键有缩进）。
func yamlTopLevelString(content, key string) string {
	for _, ln := range strings.Split(content, "\n") {
		if strings.HasPrefix(ln, key+":") {
			val := strings.TrimSpace(strings.TrimPrefix(ln, key+":"))
			return strings.Trim(val, `"'`)
		}
	}
	return ""
}

// parseKimiModels 解析 `kimi provider list --json` 的 models 段（alias → 选项，
// 按 alias 排序——CLI 输出的 map 序不定，候选顺序必须确定性）。
func parseKimiModels(stdout string) []ModelOption {
	var doc struct {
		Models map[string]struct {
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Models) == 0 {
		return []ModelOption{}
	}
	out := make([]ModelOption, 0, len(doc.Models))
	for alias, m := range doc.Models {
		out = append(out, ModelOption{ID: alias, DisplayName: m.DisplayName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
