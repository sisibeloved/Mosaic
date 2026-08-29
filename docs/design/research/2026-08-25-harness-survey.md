# Harness 调研报告：主流 Coding Agent CLI 的可集成性

## 0. 文档控制

| 项目 | 内容 |
|---|---|
| 目的 | 支撑 [RFC-0002](../rfc/2026-08-25-rfc-0002-agent-protocol.md) v0.4（首批适配器、OS 适配、session/mode/provider/model 配置、历史回查通道）与 OQ-03 裁决 |
| 方法 | 公开官方文档 / 仓库 / issue 检索 + 本地使用实证；检索时间 2026-08-25 |
| 修订 | 2026-08-25 复核（随 RFC 首轮审校）：app-server 已有官方文档但标注 experimental、不支持生产负载；ACP 官方列表含 Kimi CLI 与 Codex CLI（经适配器）；补 Kimi 非交互默认自动权限注意项 |
| 可信度标注 | 【官方】官方文档或仓库；【社区】社区文章 / issue / 论坛；【实证】本项目开发环境直接观察；【待验证】单一来源或需上手确认 |
| 范围 | 首批适配目标：Codex、ZCode、Kimi Code CLI；生态参照：Claude Code、Gemini CLI |

## 1. 结论速览

1. **端口 + 适配器抽象被调研证实是必要的**：五家 Harness 在 headless 形态、会话恢复机制、模式/权限配置、provider/model 传递方式上几乎无一相同，任何"唯某协议"的方案都会撞上至少两家的缺口；
2. **首批三家可行性分级**：Codex 与 Kimi Code 具备官方 headless + 结构化输出 + 会话恢复，可直接做原生适配；**ZCode 暂缺公开的 headless/JSON 输出模式**（官方反馈渠道已有该需求，issue #29），是唯一已知硬缺口，需内部推动或走过渡方案；
3. **历史回查双轨通道成立**：五家均有 MCP client 能力（或经配置获得），MCP server 通道可用；结构化工具请求通道不依赖 agent 侧配置，二者取舍标准就是"对该 agent 哪个更好用"；
4. **OS 适配是真实复杂度**：Codex 在 Windows 原生（受限令牌沙箱）与 WSL/Linux（Landlock/seccomp）下沙箱机制不同；Windows 与 POSIX 的信号语义差异影响取消实现；supervisor 与适配器需要 per-OS 策略；
5. **所有家的 JSON 输出 schema 均无稳定性承诺**：适配器必须钉版本 + fixture 测试，印证 conformance 门禁设计。

## 2. 能力矩阵

| 维度 | Codex | ZCode | Kimi Code | Claude Code | Gemini CLI |
|---|---|---|---|---|---|
| headless 模式 | `codex exec`【官方】 | 未见公开文档；需求追踪中【社区】 | 单轮无 TUI + `--output-format`【官方/社区】 | `-p/--print` 成熟【官方】 | `-p/--prompt`【官方】 |
| 流式结构化输出 | `--json` JSONL 事件流（实验性）【官方】 | —（hooks 机制为 stdin JSON 行 → stdout JSON）【官方】 | stream-json/JSONL，含 session resume hint 结构化元消息【官方 changelog】 | `--output-format stream-json` + `--include-partial-messages`【官方】 | `--output-format json`（stats 含 token，含 cached）【官方/社区】 |
| 会话恢复 | `codex exec resume --last` / 按 id；本地 rollout JSONL【官方/社区】 | ADE 任务内会话概念；CLI 侧待查【待验证】 | `--resume` / `--continue`；JSON 会话映射（**异常退出会破坏映射**）【社区】 | `--resume <id>` / `--continue` / `--fork-session`【官方】 | `--resume [id\|last]`，自动保存会话【官方】 |
| 模式（审批×沙箱） | `approval_policy` × `sandbox_mode` 组合（含细粒度表）【官方】 | 任务内可切换模型/执行模式（交互面）【官方】 | 跳过审批类标志【社区】 | `--permission-mode`：default / acceptEdits / plan / bypassPermissions【官方】 | 审批模式、`--checkpointing` 检查点【官方/社区】 |
| provider / model 配置 | `~/.codex/config.toml`：`model`、`model_providers`（可自定义 provider）【官方】 | GLM Coding Plan 账号计费；GLM 系列模型；CLI 配置文件 `~/.zcode/cli/config.json`【官方/社区】 | Kimi 系列模型，账号/API 登录【官方】 | Anthropic 及兼容网关（环境变量/配置）【官方】 | Gemini 系列 + API key【官方】 |
| MCP client | `config.toml` 的 `mcp_servers`【官方】 | 支持（CLI 生态含 MCP/插件/skills/hooks）【实证】 | 支持（`/mcp-config`）【社区】 | `--mcp-config` + `--allowedTools` 预授权【官方】 | `mcp.json`（项目/全局、scoped）【官方】 |
| ACP | ACP 官方列表含 Codex CLI（经适配器）【官方列表】 | 未见【待验证】 | ACP 官方列表含 Kimi CLI【官方列表】 | 已支持（经适配）【社区】 | ACP 参考实现【官方】 |
| usage 上报 | exec 输出含 token 用量【社区】 | 待查【待验证】 | 输出统计【社区】 | json 输出含 cost/usage【官方】 | json stats 含 tokens【社区】 |
| Windows | 原生（PowerShell + 受限令牌沙箱）+ WSL2（Landlock/seccomp），官方现推荐原生【官方】 | Windows 原生【实证：本仓库开发环境即 win32 + Git Bash 下 ZCode CLI 驱动】 | 跨平台单二进制（TS 打包）【官方/社区】 | 原生支持【官方】 | Node 跨平台【官方】 |
| macOS / Linux | 支持【官方】 | 支持（macOS；Linux/WSL 经由 CLI 运行）【实证/待验证】 | 支持【官方】 | 支持【官方】 | 支持【官方】 |
| 已知编排陷阱 | `--json` 事件 schema 无稳定性承诺【社区】；`--output-schema` 在 ChatGPT 账号 provider 组合下上游 400（text.format.schema 序列化 bug，2026-08-29 本机实证 codex 0.149.1——适配器改走提示词约束 + 本地校验）；超时击杀须整进程组（孙进程握管道致调用方永挂，实证） | headless 缺失（#29）；Native Agent 不触发 config.json hooks（#32）【社区】 | 异常退出破坏 session 映射【社区】 | 子进程调用需关闭 stdin，否则可能挂起【社区】 | json 输出在个别非交互场景有 bug 报告【社区】 |

## 3. 各家详述与适配器要点

### 3.1 Codex（OpenAI）

- **headless**：`codex exec` 官方非交互模式，配 `--json` 输出实验性 JSONL 事件流；社区持续请求更稳定的编排事件面（issue #4219）；另有 `app-server` 模式——**官方文档已有，但 WebSocket 等能力标注 experimental、不支持生产负载**【官方】——候选集成面，生产路径不依赖，适配器详细设计时验证；
- **session**：`codex exec resume --last` 或按会话 id 恢复；会话以本地 rollout JSONL 持久化；
- **配置**：`~/.codex/config.toml` 集中管理 `model`、`model_providers`（自定义 provider/网关）、`approval_policy`（untrusted / on-failure / on-request / never，新版本含细粒度表）、`sandbox_mode`（read-only / workspace-write / danger-full-access）、`mcp_servers`；
- **OS**：Windows 原生已成官方推荐（PowerShell + 受限令牌沙箱），WSL2 仍适合要 Landlock/seccomp 的场景——同一 Profile 在两种部署下应产生不同沙箱参数；
- **适配器要点**：mode 映射 = approval_policy × sandbox_mode 的组合翻译；session 句柄取自 exec 输出；usage 从事件流提取。

### 3.2 ZCode（Z.ai / 智谱）

- **形态**：Agentic Development Environment（ADE）+ CLI 组件；GLM-5.x 深度适配，GLM Coding Plan 计费；
- **headless**：**当前主要缺口**。官方文档未见 headless/`--print`/JSON 输出页；zai-org/feedback #29 正是"非交互单轮执行模式"的需求请求。CLI 侧存在配置文件（`~/.zcode/cli/config.json`）与 hooks 机制（stdin 一行 JSON → 退出码 + stdout JSON）【官方】；
- **本地实证**：本项目即在 Windows 原生（win32 + Git Bash）环境由 ZCode CLI 驱动开发，CLI 具备插件、skills、hooks、MCP、子代理等扩展机制——OS 支持与扩展性无忧，缺的只是程序化驱动面；
- **适配器要点**：缺口解除前该 Participant 能力降级可见（unavailable/受限模式）；过渡方案优先级：官方 headless 落地（内部推动 #29）> ACP 适配（若 ZCode 提供 ACP agent）> hooks/单轮包装的受限集成。

### 3.3 Kimi Code（Moonshot）

- **形态**：MIT 开源、TypeScript 单二进制（无需 Node 运行时），仓库 MoonshotAI/kimi-code；支持子代理；
- **headless**：单轮无 TUI 执行 + `--output-format`（stream-json/JSONL）；changelog 明确"以结构化元消息输出 session resume hint"——对适配器获取会话句柄非常友好；
- **session**：`--resume` / `--continue`；会话映射存 JSON 文件，**非正常退出（非 /exit、非 Ctrl+D）会破坏映射**——supervisor 的终止路径必须考虑优雅退出，或适配器不依赖其自身映射而由 Mosaic 侧持有会话句柄；
- **MCP/ACP**：均支持（`/mcp-config`；ACP 支持见社区日报）；
- **适配器要点**：流式解析 + 会话句柄提取路径清晰；注意异常退出语义。

### 3.4 Claude Code（Anthropic，生态参照）

- headless 最成熟：`-p` + `--output-format json/stream-json`，json 含 session_id 与 cost/usage；`--resume`/`--fork-session` 多轮编排模式官方文档化；权限四档 + `--allowedTools` 预授权；`--mcp-config` 在 headless 下可用（首轮会等待 pending 的 MCP server）；
- **陷阱（对 supervisor 的直接输入）**：作为子进程调用时**必须关闭 stdin** 否则可能挂起（社区多个报告）；stream-json 的 NDJSON 类型曾有不一致 issue；
- **适配器要点**：若列入后续批次，几乎是"教科书对接对象"；其子进程陷阱应吸收进 supervisor 的通用进程管理规约。

### 3.5 Gemini CLI（Google，生态参照）

- `-p` + `--output-format json`；`--resume [id|last]` + 自动会话保存；`--checkpointing` 检查点与 `/restore`；`mcp.json` 配置 MCP；ACP 参考实现（ACP 适配器首选验证对象）；
- **适配器要点**：json stats 含 token/cached，usage 提取容易；个别非交互 json 输出 bug 有报告，fixture 化即可覆盖。

## 4. OS 适配专节（Windows 原生 / WSL / macOS）

1. **沙箱机制分裂**：Codex 类 agent 的沙箱在 Linux/WSL 是 Landlock/seccomp，Windows 原生是受限进程令牌——同一安全意图需要 per-OS 的参数映射；Profile 的 `adapter_options` 必须允许按 OS 覆盖；
2. **信号与取消语义**：POSIX SIGINT/SIGTERM 与 Windows 控制台事件/强制终止不等价；supervisor 的取消升级策略（中断 → 等待窗口 → 终止）在 Windows 下需单独实现并测试；终止前尽量走 agent 的优雅退出路径（见 Kimi 会话映射教训）；
3. **shell 与路径**：Windows 原生（PowerShell/cmd）与 WSL/macOS（bash/zsh）的引号、路径分隔、环境变量传递不同；Profile 的 command/args/env 需按 OS 描述或由适配器归一化；
4. **WSL 是第三种形态而非 Windows 子集**：WSL 下 agent 访问的文件系统边界（\\wsl.localhost vs drvfs）、网络命名空间与 Windows 原生不同，部署文档与 Profile 校验要把它当独立目标对待；
5. **验证矩阵**：conformance 套件必须在三形态上跑（Windows 原生、WSL、macOS/Linux），CI 至少覆盖两形态。

## 5. 对 RFC-0002 v0.4 的修订输入

1. 首批适配器定为 Codex / ZCode / Kimi Code CLI（OQ-03 组成部分）；
2. Profile 增加 `adapter_options`：收敛各家 session/mode/provider/model/沙箱参数的翻译，允许 per-OS 覆盖；
3. 历史回查通道定调双轨：本地 MCP server 与结构化工具请求皆可，适配器按 agent 能力选择并在能力声明登记；
4. ZCode headless 缺口登记为风险项与依赖项（内部推动 + 过渡方案）；
5. supervisor 通用规约吸收调研陷阱：优雅退出优先、stdin 关闭、per-OS 取消升级；
6. ACP 适配器的首个候选验证对象：Gemini CLI（参考实现）或 Kimi Code（已支持）。

## 6. 缺口与待验证清单

| 项 | 现状 | 动作 |
|---|---|---|
| ZCode headless/JSON | 无公开文档，#29 需求追踪中 | 内部推动；适配器详细设计前复访 |
| Codex `app-server` 模式 | 官方文档已有，experimental、不支持生产负载（2026-08-25 复核） | 持续观察；生产路径不依赖 |
| Kimi 非交互默认自动权限 | 官方命令文档确认 | 适配器显式权限参数映射 + 实测门禁 |
| Codex `--json` schema 稳定性 | 实验性、无承诺 | 钉版本 + fixture |
| Kimi 异常退出会话映射破坏 | 论坛确认 | supervisor 优雅退出 + Mosaic 侧持有句柄 |
| Claude 子进程 stdin 挂起 | 多来源确认 | supervisor 规约吸收 |
| ZCode CLI 会话/mode 细节 | 未见公开文档 | 本地实证补充（团队即用户） |
| 各家 usage 字段口径 | 不一致 | 结构化自报 + unknown 兜底 |

## 7. 参考资料

- Codex：[Non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode)、[Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference)、[Windows sandbox](https://learn.chatgpt.com/docs/windows/windows-sandbox)、[Issue #4219 headless orchestration](https://github.com/openai/codex/issues/4219)、[Issue #5207 非交互恢复](https://github.com/openai/codex/issues/5207)、[Agent Safehouse 沙箱分析](https://agent-safehouse.dev/docs/agent-investigations/codex)
- ZCode：[官方文档](https://zcode.z.ai/cn/docs/welcome)、[Hooks](https://zcode.z.ai/en/docs/hooks)、[feedback #29 非交互模式请求](https://github.com/zai-org/feedback/issues/29)、[feedback #32 CLI hooks](https://github.com/zai-org/feedback/issues/32)、[官网](https://zcode.z.ai/cn)
- Kimi Code：[GitHub MoonshotAI/kimi-code](https://github.com/MoonshotAI/kimi-code)、[kimi 命令参考](https://www.kimi.com/code/docs/en/kimi-code-cli/reference/kimi-command.html)、[Changelog（session resume hint）](https://www.kimi.com/code/docs/en/kimi-code-cli/release-notes/changelog.html)、[非交互工作流教程（MarkTechPost）](https://www.marktechpost.com/2026/07/28/building-non-interactive-agentic-coding-workflows-with-moonshot-ais-kimi-cli-jsonl-streaming-testing-and-session-memory/)、[Kimi --continue 论坛](https://forum.moonshot.ai/t/kimi-continue-not-work/284)
- Claude Code：[Headless](https://code.claude.com/docs/en/headless)、[CLI Reference](https://code.claude.com/docs/en/cli-reference)、[Sessions](https://code.claude.com/docs/en/sessions)、[Permissions](https://code.claude.com/docs/en/permissions)、[子进程 stdin 问题](https://stackoverflow.com/questions/79826420/calling-claude-cli-as-a-child-process-yields-no-output)
- Gemini CLI：[Headless](https://geminicli.com/docs/cli/headless/)、[Session management](https://geminicli.com/docs/cli/session-management/)、[Configuration](https://geminicli.com/docs/reference/configuration/)、[GitHub](https://github.com/google-gemini/gemini-cli)
- 生态：[ACP 官网](https://agentclientprotocol.com/get-started/introduction)、[ACP 可用 Agent 清单](https://agentclientprotocol.com/get-started/agents)、[ACP Protocol](https://agentclientprotocol.com/protocol/)、[社区日报（各家 ACP 支持动态）](https://github.com/borq168/radar-forge/issues/79)
