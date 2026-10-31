# Mosaic v0.6.2 — 执行上下文清单（0.6 Mission 与本地执行领域·第三件）

0.6.1 让交办可派发；本版交付**装配与存证本身**——"执行时到底交给 Agent 什么"从此成为可查询、可验证、可重放的持久事实。0.6.5 Runtime 到位后从存档清单执行，不采信现场重装。

## 新增

- **ContextManifest 存档装配**（RFC-0018 §7.3/§6 / ADR-0025，migration v15）：钉契约当前版本行 + 任职钉版本模板行 + 授权交集（复用 0.5.5 准入单源）+ 来源房间水位向量，装配内容**整份存档**。层模型 0.6.2 子集（RFC 组装顺序）：必需层（约束与有效权限 / 职责 / 契约 / 工具）不得截断——合计超 256 KiB 总预算 blocked `required_over_budget`；可选层独立配额（近期消息 64 KiB、单条 8 KiB 头部截断如实披露）且同时受剩余总预算约束；checkpoint / memory / 决策胶囊未建模层账本如实 omitted 携原因。
- **授权过滤先于排序、摘要与计数**：来源房非 public 事件不进入、不计数、不列名（存在性零泄漏）；水位 = 实际交付的最新 seq（交付前沿，非读取前沿）；模板声明 room.recent 域而授权未含时读取前省略（`context_not_authorized`）。
- **digest 即身份**：`UNIQUE(mission_id, digest)`——同状态重建重放原清单（`replayed=true`，授权撤销再恢复也重放原件），状态变化（房间新事件 / 授权变更 / 契约修订）追加新行：每个 Mission 保留"各时点会交付什么"的完整装配历史。摘要经规范化编码（键序字典序、数字保真），存储往返后可按原算法复算——审计与回放可验证。
- **ContextReceipt** 交付回执：只记账目不复制完整 Prompt（层状态/字节/截断/遗漏及原因 + 政策版本 `context-assembly/1`）；`run_id` 随 0.6.5 Run 对象存在时增列（不落空壳）。
- **blocked 恒 200**（probe 纪律），固定优先级 `owner_mismatch > binding_not_active > readiness_not_met > required_over_budget`，blocked 也存完整账本；`context_manifest_ref` 兑现为查询时联查（ADR-0023 决策 2——不污染契约不可变版本行）。
- **API** 三端点（`POST/GET /v1/missions/{id}/context-manifests`、`GET /v1/context-manifests/{id}`）；任务中心详情新增"执行上下文"区——选负责人任职装配、层账本与内容块查看、装配历史。

## 修复（独立审校 1 项 P1、7 项 P2）

- **无 tools 模板装配不再崩溃**：tools 是模板可选声明，改空值安全访问——无声明即空清单（不虚构能力）。
- **换负责人后旧任职不可提交**：选择随负责人变化清空，提交前核对归属，失效就地报错。
- **迟到响应不再覆盖最新查看结果**：装配与历史查看共用结果请求序号。
- **准入报告与工具清单共用同一授权快照**：单次装配只读一份授权，"必需工具满足而有效工具为空"的矛盾清单不可再产生。
- **异常大输入不再突破总预算**：可选层配额同时受剩余总预算约束（首条消息也不例外）。
- **存档读取后摘要可按原算法复算**：规范化编码 + 模板版本入水位向量——digest 输入在清单行内自包含。
- **取消任务后历史仍可查看**：保留只读上下文卡，仅禁新装配。
- **声明未授权的 room.recent 不再被存档**：读取、计数前省略；未声明该域的模板不扩大要求。

## 升级说明

- v0.6.1 数据库首次启动自动建 `exec_context_manifests` / `exec_context_receipts` 两表（migration v15，表名冠 `exec_` 与房间域对话上下文回执区分），无需手工步骤；旧版本二进制可直接升级本库。
- 本版交付装配与存证：Runtime 执行消费、Checkpoint/记忆/决策胶囊层分别随 0.6.5 / 0.7 / 0.6.11 增列。效果可在任务中心详情"执行上下文"区与 `POST /v1/missions/{id}/context-manifests` 核对。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（Approved §7.3/§6）与 [ADR-0025](docs/design/adr/ADR-0025-execution-context-manifest.md)（v0.2）。
