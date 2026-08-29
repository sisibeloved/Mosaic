# ADR-0004：数据访问与迁移采用 pgx/v5 + sqlc + goose

| 状态 | Accepted |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | 架构 v0.6 §8.2.5（IF-PERSIST）、§8.5（BE-04） |

## 背景

Event Log / outbox 大量依赖 advisory lock、`FOR UPDATE SKIP LOCKED`、RLS、JSONB 细粒度控制与 expected version 乐观并发，需要显式、可审查的 SQL。

## 决策

- pgx/v5 作 PostgreSQL 驱动；sqlc 从 SQL 生成类型安全的访问代码；
- 迁移工具用 goose：纯 SQL 迁移，可直接打包进 BE-04 迁移镜像；
- RLS 的租户上下文用事务级 `SET LOCAL` 绑定（连接池安全）。

## 后果与放弃方案

- 放弃 GORM / ent：重 ORM 的抽象与锁语义、JSONB 与 RLS 的细粒度控制相互对抗；
- tern、golang-migrate 亦为可行替代，选 goose 取其生态成熟度与容器化便利；
- 迁移交付以 forward-fix 为主，不承诺回滚脚本（架构 §8.6.2）。
- **个人版形态（2026-08-25 追加）**：v1.0 桌面形态随 [ADR-0008](ADR-0008-personal-storage-sqlite.md) 走 SQLite 方言（goose 双方言支持，迁移文件按方言分目录）；pgx/v5 + sqlc 组合保留于服务端形态。

## 修订（2026-08-28，M1）

- SQLite 形态下的迁移基线已落地：`migrations` 表记录已应用版本（v1 = 当前幂等 DDL：room_events/outbox/command_receipts/context_receipts）；
- goose 评估结论：**首个破坏性 schema 变更出现前不引入 goose 依赖**——当前 DDL 全幂等（CREATE IF NOT EXISTS），新表即新增语句；需要 ALTER/回填时再评估 goose SQLite 方言 vs 内嵌版本化迁移（届时以新增 ADR 修订记录）。
