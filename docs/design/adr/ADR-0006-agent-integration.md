# ADR-0006：Agent 集成——本地进程为主 + 适配器抽象（ACP 为可选适配器）

| 状态 | Accepted（RFC-0002 已于 2026-08-25 Approved，按治理规则同步） |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | RFC-0002；架构 v0.6 §6.4、§8.2.5、§9.1.5 |

## 决策

- 集成分两层：Mosaic 自有 **Harness 端口**（任务、结构化结果、取消、grant epoch、usage 的域级契约）+ **适配器**实现；
- 主要适配方向是**本地进程原生适配**：按 Agent Profile 以子进程（`os/exec`）启动既有 agent（首批：Codex、ZCode、Kimi Code CLI），逐 agent 对接其 headless/结构化输出模式；
- **ACP 适配器为可选项**：仅当 agent 已有现成 ACP 实现时采用；ACP 语义（session/prompt/cancel/request_permission）收敛在该适配器内部，不进入域层；
- 历史回查通道由适配器按 agent 能力选择（本地 MCP server 或结构化工具请求，端口语义一致）；
- provider 凭据与出网由 agent 进程自理（OQ-20）；Mosaic 不向进程注入任何自身凭据；不发布对外集成协议与对外 SDK。

## 后果与放弃方案

- 放弃"唯 ACP 客户端"方案（RFC-0002 v0.2）：把域语义耦合到 ACP 方法集，无 ACP 实现的 agent 反而多绕一层，ACP 规范演进会传导进域层；
- 放弃自有回调协议（任务 webhook + 入站结果 API + 长连接，RFC-0002 v0.1 初稿）：要求外部实现 Mosaic 专有端点与认证，与"Harness 是既有标准 agent 进程、被我们调用"的前提相悖；
- 放弃 A2A：其 agent 间直连模型与架构原则 3 冲突；
- 代价：逐 agent 原生适配有维护成本，以最小适配器接口 + 共享进程管理/解析工具库 + conformance 门禁控制；
- 远端/多机 Harness 为演进项：届时引入网络形态并启用 SSRF 六条约束与受控 egress proxy，组件归属随本 ADR 修订确定；
- ACP 无 usage 元数据：token 维度限额缺失时自动退化为轮次/时长（架构 §9.7 允许），显式记 unknown。
