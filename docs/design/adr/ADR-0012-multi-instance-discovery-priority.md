# ADR-0012：Harness 多实例发现——全枚举 + 渠道标签 + 家族优先级

- 状态：Accepted
- 日期：2026-08-31
- 背景：M2 C 轨（多实例发现 + Kimi 适配器晋级），负责人裁定（2026-08-31）

## 语境

harness 自动发现在 M1 是 first-hit（每家族只取第一个命中的可执行体）。实际环境会出现**同一 Agent 的多个独立实例并存**：例如 Windows 装了 ChatGPT/Codex 桌面 App（MSIX 包内 bundled codex.exe），WSL 里又装了 Codex CLI——两者的配置目录、登录态、会话各自独立，不是同一安装的两个入口。M2 设置页需要把实例全部扫出来，并支持在配置中切换。

负责人的两条家族裁定：

1. **Codex**：桌面 App 内 bundled CLI 的优先级高于独立 CLI 安装；
2. **Kimi**：Kimi Code CLI 的优先级高于 Kimi Work 桌面形态——两者独立计费，Code 是 Mosaic 的实际驱动面。

## 决策

1. **发现改全枚举**：PATH + KnownDirGlobs + AppGlobs 命中的可执行体全部入库，按绝对路径去重（含 `.exe` 兜底）；废弃 first-hit。
2. **渠道（Channel）标签**：实例携带渠道——`cli`（独立安装）/ `app:codex-desktop` / `app:kimi-work`；AppGlobs 仅在 native 面展开（WSL 发行版内无 Windows 应用概念，实证见 platform-notes W-7/W-8）。
3. **家族优先级表**：`PriorityFor(adapter, channel)` 数值小者优先——codex：app 10 < cli 20；kimi：cli 10 < app 20；未裁定家族一律 50；已裁定家族的未知渠道 90。优先级每次入库重算，裁定调整即刻生效，不做持久固化。
4. **排序即契约**：`List()` / `EnabledList()` 按 (adapter, priority, path) 稳定排序——设置页展示顺序与参与者座位顺序共用同一序，不加第二层"默认实例"状态。
5. **切换经 enable 门控**：实例间切换不改排序——用户 enable 想用的实例、disable 其余，参与者座位从 `EnabledList()` 取（门控语义与 M1 一致）。手动登记（`AddManual`）可携带 channel 覆盖（形态校验：`cli` 或 `app:<小写>`），且不被后续扫描冲掉。

## 后果

- 同家族多实例同时 enable 时按 priority 排座位，高优先级实例先入座；想用低优先级实例就 disable 高优先级项——语义直白，切换是显式动作；
- Codex App 的 bundled CLI 路径形状据上游 issue 归档（platform-notes W-7），本机未装该应用、**未真机实证**——首次实证若形状不符，改 glob 即可，不动机制；
- Kimi Work 暂无可驱动 CLI 面（platform-notes W-8），`app:kimi-work` 渠道目前只经手动登记进入，作为排序与计费语义的占位；
- 放弃方案：按"最近修改时间/版本号"猜默认实例（不可靠且不可解释）；引入 profile 级 `preferred_executable` 字段（与 enable 门控职能重叠，徒增状态面）。
