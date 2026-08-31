// 内建探测规格（Harness 调研 2026-08-25 实证；命令面以本机验证为准）。
package harness

// 实例渠道：同一 Agent 家族的不同安装形态（配置/会话各自独立——负责人裁定 2026-08-31）。
const (
	// ChannelCLI 独立 CLI 安装（PATH / 版本管理器目录）。
	ChannelCLI = "cli"
	// ChannelAppCodexDesktop Codex/ChatGPT 桌面应用内 bundled CLI
	// （MSIX 包 OpenAI.Codex_* / OpenAI.ChatGPT_*，路径形状实证自 openai/codex
	// issue #40700/#41059；WindowsApps ACL/加密可能导致直接执行失败——探测失败
	// 即 login unknown，登录门控天然挡启用，Windows 真机验证登记 platform-notes）。
	ChannelAppCodexDesktop = "app:codex-desktop"
	// ChannelAppKimiWork Kimi Work 桌面形态（实证 2026-08-31 本机 Windows 侧
	// kimi-desktop 安装内未见可驱动 CLI 面——暂无自动扫描位置，经手动登记携带
	// channel 进入优先级体系；与 Kimi Code 独立计费，故 Code 优先，见 PriorityFor）。
	ChannelAppKimiWork = "app:kimi-work"
)

// 家族优先级（负责人裁定 2026-08-31；数值小者优先）：
//   - codex：App 实例 > 独立 CLI（"Codex以App的优先级更高"）；
//   - kimi：Kimi Code CLI > Kimi Work 桌面（两者独立计费，Code 优先）；
//   - 其余家族未裁定：一律默认位次。
var familyChannelOrder = map[string][]string{
	"codex": {"app", "cli"},
	"kimi":  {"cli", "app"},
}

// 优先级常量（数值小者优先；位次留间隔便于未来插入）。
const (
	priorityStep           = 10
	PriorityDefault        = 50 // 家族未裁定
	PriorityUnknownChannel = 90 // 家族已裁定但渠道不在裁定表
)

// PriorityFor 计算 (adapter, channel) 的优先级：按渠道类（channel 冒号前缀）在家族
// 裁定表中的位次取值；未知渠道/未裁定家族落兜底位次。
func PriorityFor(adapter, channel string) int {
	class := channel
	for i := 0; i < len(channel); i++ {
		if channel[i] == ':' {
			class = channel[:i]
			break
		}
	}
	order, ok := familyChannelOrder[adapter]
	if !ok {
		return PriorityDefault
	}
	for i, c := range order {
		if c == class {
			return (i + 1) * priorityStep
		}
	}
	return PriorityUnknownChannel
}

// AppGlob 桌面应用内 bundled CLI 的发现位置：绝对 glob 模式（支持 {LOCALAPPDATA} /
// {PROGRAMFILES} 占位，native 面按宿主家目录展开；WSL 发行版内无 Windows 应用概念，不展开）。
type AppGlob struct {
	Channel  string   // 命中实例的渠道标签
	Patterns []string // 目录 glob；命中后拼二进制名（含 .exe 兜底）探测
}

// BuiltinProbes 首批适配家族的探测规格。
var BuiltinProbes = []ProbeSpec{
	{
		// 实证：codex --version → "codex-cli 0.149.1"；
		// codex login status → "Logged in using ChatGPT"（exit 0），未登录非零。
		// nvm 装在版本化目录且仅登录 shell 注入 PATH——glob 是可靠发现面（本机实证）
		Adapter:        "codex",
		Binary:         "codex",
		VersionArgs:    []string{"--version"},
		LoginCmd:       []string{"login", "status"},
		LoginOKPattern: "Logged in",
		KnownDirGlobs: []string{
			".nvm/versions/node/*/bin",
			".fnm/node-versions/*/installation/bin",
			".local/share/fnm/aliases/default/bin",
			".volta/bin",
			".npm-global/bin",
			".local/bin",
		},
		AppGlobs: []AppGlob{
			{
				Channel: ChannelAppCodexDesktop,
				Patterns: []string{
					// MSIX 商店包（winget 9PLM9XGG6VKS）：发行者哈希稳定，版本/架构走通配
					`{PROGRAMFILES}/WindowsApps/OpenAI.Codex_*/app/resources`,
					`{PROGRAMFILES}/WindowsApps/OpenAI.ChatGPT_*/app/resources`,
				},
			},
		},
	},
	{
		// 实证：kimi --version → "0.38.0"；无 login status 命令，
		// 登录态以凭证文件存在性判定（~/.kimi-code/credentials/kimi-code.json）；
		// 官方安装位置 ~/.kimi-code/bin（本机实证：PATH 无此目录仍能经 glob 发现）。
		// Kimi Work 桌面形态暂无可驱动 CLI 面（实证 2026-08-31），不挂 AppGlobs——
		// 实例经 AddManual 携带 channel 进入优先级体系。
		Adapter:     "kimi",
		Binary:      "kimi",
		VersionArgs: []string{"--version"},
		CredFile:    ".kimi-code/credentials/kimi-code.json",
		KnownDirGlobs: []string{
			".kimi-code/bin",
			".nvm/versions/node/*/bin",
			".local/bin",
		},
	},
	{
		// headless 缺口（调研 issue #29）：仅登记存在性与版本，登录探测待 headless 落地后补
		Adapter:     "zcode",
		Binary:      "zcode",
		VersionArgs: []string{"--version"},
	},
}
