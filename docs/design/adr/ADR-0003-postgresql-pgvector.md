# ADR-0003：主存储 PostgreSQL（下限 17，参考栈 18.x）+ pgvector ≥ 0.8

| 状态 | Accepted |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | 架构 v0.6 §8.2.6、§8.3、§9.6 |

## 背景

事件存储、outbox、投影、记忆与向量检索同库，是模块化单体的一致性承诺；开源自托管要求数据库由部署方提供。

## 决策

- 兼容下限 PostgreSQL 17，参考实现、CI 与托管配置固定 18.x；
- uuidv7 由应用生成，不依赖 18 的原生函数，保持对 17 的兼容；
- pgvector 钉 ≥ 0.8：检索起步用精确检索 / iterative scan，HNSW 索引按数据量与延迟证据再启用。

## 后果与放弃方案

- 硬钉 18 会限制自托管适配面（托管 PG 普遍滞后一个大版本），故采用"下限 + 参考栈"双轨写法；
- iterative scan 缓解 tenant/visibility 强过滤下的召回与结果不足问题；压测口径必须覆盖过滤场景，不只测平均延迟（架构 §9.6）；
- 放弃独立向量数据库：无规模证据，不引入；embedding 维度由首个自用模型钉死（OQ-03，Model Gateway ADR）。
- **个人版形态（2026-08-25 追加）**：v1.0 桌面形态改用 SQLite + sqlite-vec（[ADR-0008](ADR-0008-personal-storage-sqlite.md)）；本 ADR 的 PostgreSQL 结论限定于服务端部署形态。
