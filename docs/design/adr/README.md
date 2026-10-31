# Mosaic 架构决策记录（ADR）

ADR 记录**实现与工具选型**级决策。三层文档分工：架构级约束归[架构设计说明书](../2026-08-13-mosaic-architecture-design.md)，协议与行为契约归 [RFC](../rfc/)，实现选型归本目录。

**状态权威顺序**：架构基线 > RFC > ADR。RFC 尚未 Approved 时，直接依赖该 RFC 结论的 ADR 保持 Proposed，待 RFC Approved 后同步 Accepted；被取代的 ADR 保留不删除。术语统一：RFC 用 Approved / Rejected / Superseded，ADR 用 Proposed / Accepted / Superseded。

格式：状态 / 日期 / 关联 / 背景 / 决策 / 后果与放弃方案。决策变化时更新对应 ADR 并在修订处注明取代关系。

| 编号 | 决策 | 状态 |
|---|---|---|
| [ADR-0001](ADR-0001-realtime-transport.md) | 客户端实时传输：SSE 下行 + HTTP 命令 | Accepted |
| [ADR-0002](ADR-0002-web-frontend.md) | Web 前端：Vite 静态 SPA（React + TypeScript） | Accepted |
| [ADR-0003](ADR-0003-postgresql-pgvector.md) | 主存储：PostgreSQL 17+（参考栈 18.x）+ pgvector ≥ 0.8 | Accepted |
| [ADR-0004](ADR-0004-data-access-migration.md) | 数据访问与迁移：pgx/v5 + sqlc + goose | Accepted |
| [ADR-0005](ADR-0005-observability-supplychain.md) | 可观测与供应链：OTel Trace/Metric + slog；syft / osv-scanner | Accepted |
| [ADR-0006](ADR-0006-agent-integration.md) | Agent 集成：本地进程为主 + 适配器抽象（ACP 可选） | Accepted |
| [ADR-0007](ADR-0007-protocol-toolchain.md) | 协议工具链：JSON Schema 权威 + oapi-codegen + 运行时校验 | Accepted |
| [ADR-0008](ADR-0008-personal-storage-sqlite.md) | 个人版存储：SQLite（WAL）+ sqlite-vec | Accepted |
| [ADR-0009](ADR-0009-personal-local-identity.md) | 个人版身份：本地单用户免登录 | Accepted |
| [ADR-0010](ADR-0010-personal-app-shell-wails.md) | 个人版应用壳：Wails（WebView2 / WKWebView） | Accepted |
| [ADR-0011](ADR-0011-snapshot-inline-deviation.md) | 快照四元组内联正文：对 RFC-0001 快照形态的 M1 偏离登记（M2 对齐） | Accepted |
| [ADR-0012](ADR-0012-multi-instance-discovery-priority.md) | Harness 多实例发现：全枚举 + 渠道标签 + 家族优先级（Codex App 优先、Kimi Code 优先） | Accepted |
| [ADR-0013](ADR-0013-stable-bot-identity.md) | 稳定 Bot 身份与会话寻址（M4-4） | Accepted |
| [ADR-0014](ADR-0014-document-event-stream.md) | 文档作为第二权威事实源：per-doc 事件流（同一 SQLite 库） | Accepted |
| [ADR-0015](ADR-0015-spreadsheet-editor-and-formula.md) | 协作表格：自绘网格编辑器 + Go 服务端权威公式求值器（封闭函数集） | Accepted |
| [ADR-0016](ADR-0016-agent-update-channels.md) | Agent 更新通道三分类：registry（第三方仓库）/ self-update（官方自更新）/ mosaic（Mosaic 自带脚本通道） | Accepted |
| [ADR-0017](ADR-0017-agent-identity-registry.md) | 独立 Agent 身份注册表（0.5.0）：SQLite 权威 agents/agent_aliases、bot_id 等价键迁移、旧 ID 别名解析、注册表单向同步 | Accepted |
| [ADR-0018](ADR-0018-agent-lifecycle.md) | Agent 生命周期状态机与事件流（0.5.1）：active/suspended/retiring/retired、agent_events 流与 v7 存量回填、身份写面（create/lifecycle/events） | Accepted |
| [ADR-0019](ADR-0019-slotspec-versioned-template.md) | SlotSpec 版本化职责模板（0.5.2）：不可变版本行、内容 Schema 定形（职责/技能/工具 required-optional/上下文/交付/委派/运行时/交接）、slot_spec_id 用户选定谱系 | Accepted |
| [ADR-0020](ADR-0020-slotinstance-knowledge-namespace.md) | SlotInstance 职责实例与独立知识命名空间（0.5.3）：si_ 生成式实例、slotns_ 全局唯一稳定命名空间、模板谱系引用不钉版、默认标题存档 + 模板现势连接 | Accepted |
| [ADR-0021](ADR-0021-binding-lifecycle.md) | Binding 任职状态机与交接（0.5.4）：preparing/ready/active/draining/released/blocked、单 active 部分唯一索引、固定模板版本、Readiness/Drain 双端口预埋（0.5.5/0.6 挂靠）、交接位 handoff_note | Accepted |
| [ADR-0022](ADR-0022-readiness-admission.md) | 职责准入（0.5.5）：AgentGrant 授权档案、EvaluateReadiness 交集单源（必需缺失 blocked / 选配降级 / workspace 格栅）、ReadinessChecker 实装、试算与 blocked 重试端点（AT-07） | Accepted |
| [ADR-0023](ADR-0023-mission-contract.md) | MissionContract 独立事实源（0.6.0）：不可变契约版本行 + CAS 修订（version_conflict）、契约状态子集 draft/ready/canceled（完备性判定单源）、budget/delegation 安全缺省、/missions 任务中心首屏；Dispatch 排布裁定（首切片随 0.6.1） | Accepted |
| [ADR-0024](ADR-0024-room-dispatch-first-slice.md) | 房间交办与 Dispatch 首切片（0.6.1）：room_source 结构化来源 + (room_id, task_id) 唯一映射（409 冲突可见）；resolve/dispatch/回执两段式 + (caller_ref, scope, idempotency_key) 幂等约束；决策存档请求为投递输入；确定性路径 only | Accepted |
| [ADR-0025](ADR-0025-execution-context-manifest.md) | 执行上下文清单（0.6.2）：ContextManifest/ContextReceipt 存档装配（必需层不截断超预算 blocked、可选层独立配额、授权过滤先于排序计数）、digest 即身份确定性重建去重、水位向量、context_manifest_ref 查询时联查兑现 | Accepted |
| [ADR-0026](ADR-0026-local-environment-runtime.md) | 本地 Environment / Runtime（0.6.3）：命名工作区环境（默认恰一）+ rtx_ 运行时记录（发现幂等键 (adapter,host,distro,path)）+ discovered/ready/unhealthy 子集（探测失败可见、准备恢复）、runtime_events 流、harness Reprobe 只读复用 | Accepted |
| [ADR-0027](ADR-0027-runtime-lease.md) | RuntimeLease 工作区占用/互斥/围栏代次（0.6.4）：环境独占 + 单运行时容量 1（双部分唯一索引）、代次单源于租约行（MAX(generation)）、fnc_ 围栏令牌四关校验（0.6.5 结果提交消费）、续租即心跳、到期惰性判定、busy 派生化 | Accepted |
| [ADR-0028](ADR-0028-mission-execution.md) | 单 Agent Mission 执行（0.6.5）：run_ 域（attempt 唯一、每 Mission 至一未终态）+ 持久调度（两时点复核、租约+存档清单+围栏全消费）+ 执行器端口（agent.Supervisor 适配）+ Mission running 投影 + 围栏拒绝审计 | Accepted |
