# ADR-0008：个人版存储采用 SQLite（WAL）+ sqlite-vec

| 状态 | Accepted |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | [交付规划 D-1](../../plan/2026-08-25-delivery-plan.md)；[ADR-0003](ADR-0003-postgresql-pgvector.md)、[ADR-0004](ADR-0004-data-access-migration.md)；RFC-0001 |

## 背景

交付目标重定为个人完全可用的双平台桌面 App（Windows/macOS）：零依赖安装、单文件备份、单进程运行是第一原则；PostgreSQL 的运维面（进程管理、initdb 本地化、自升级迁移）对个人桌面是持续负担。

## 决策

- 个人版（v1.0 桌面形态）存储用 **SQLite（WAL 模式）**，向量检索用 **sqlite-vec**；
- 驱动采用 **modernc.org/sqlite**（纯 Go、免 CGO）：Windows/macOS 交叉编译与三平台 CI 无工具链负担；spike 实测 per-connection pragma 必须走 DSN（连接池下 `db.Exec` 只命中单连接，并发写会立刻 SQLITE_BUSY）；
- 机制映射（设计不变，实现换形）：advisory lock / 房间串行 → `BEGIN IMMEDIATE` 互斥写事务；outbox `FOR UPDATE SKIP LOCKED` → 进程内提交后分发（outbox 表仍持久化，崩溃后按水位重放）；RLS/租户隔离 → 本地单用户天然满足（tenant 保留为常量列）；uuidv7 应用生成不变；迁移用 goose 的 SQLite 方言；
- 检索起步用精确/遍历检索，sqlite-vec 索引按数据量证据启用（与 ADR-0003 的渐进口径一致）。

## 后果与放弃方案

- 安装零依赖、备份即拷贝单文件、升级即迁移文件——直接支撑交付规划 DoD 第 7 条；
- **放弃内嵌 PostgreSQL**（fergusstrange/embedded-postgres 类 + 自打包 pgvector）：保留为 D-1 回退预案，仅当 SQLite 出现并发/迁移硬伤时启用；
- ADR-0003/0004 的 PostgreSQL 结论**限定于服务端部署形态**，不视为被推翻；RFC-0001 契约存储无关，不受影响。
