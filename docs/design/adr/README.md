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
