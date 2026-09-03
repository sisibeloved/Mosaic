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
package harness

import (
	"context"
	"encoding/json"
	"sort"
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
}

// codexEffortLevels codex model_reasoning_effort 档位（v1.48 实证：负责人
// config 在用 xhigh；CLI help 的 -c 通道接受 config.toml 全部键）。
var codexEffortLevels = []string{"minimal", "low", "medium", "high", "xhigh"}

var codexEffortSet = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

// RuntimeOptionsOf 某实例的模型候选与强度档位：kimi 经 Runner 实查
// `provider list --json`（失败回退空候选+dynamic=true 保留——UI 提示查询失败
// 可手输）；codex 返回五档强度与空候选；其余空面。
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
	switch exe.Adapter {
	case "kimi":
		opts := RuntimeOptions{Models: []ModelOption{}, Dynamic: true, EffortLevels: []string{}}
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
			Models:       []ModelOption{}, // 无官方列表命令：自由输入（placeholder 示例如 gpt-5.6-sol）
			Dynamic:      false,
			EffortLevels: append([]string(nil), codexEffortLevels...),
		}, nil
	default: // minimax 及后续适配器
		return RuntimeOptions{Models: []ModelOption{}, Dynamic: false, EffortLevels: []string{}}, nil
	}
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
