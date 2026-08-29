// 内建探测规格（Harness 调研 2026-08-25 实证；命令面以本机验证为准）。
package harness

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
	},
	{
		// 实证：kimi --version → "0.38.0"；无 login status 命令，
		// 登录态以凭证文件存在性判定（~/.kimi-code/credentials/kimi-code.json）；
		// 官方安装位置 ~/.kimi-code/bin（本机实证：PATH 无此目录仍能经 glob 发现）
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
