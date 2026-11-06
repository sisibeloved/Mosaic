# Mosaic v0.7.3 — Checkpoint：可恢复业务状态（0.7 记忆与可移植恢复领域·第四件）

Run 的权威状态只记录执行终态——恢复续办所需的业务中间态（已完成/待办步骤、产物引用、不确定动作、等待条件、各领域水位）没有持久载体。本件按 RFC-0018 §7.3 交付 **Checkpoint**：独立于 ContextReceipt（那是"交付了什么"，这是"工作到哪了"）的可恢复业务状态存储。无独立审校轮（0.7.0–0.7.2 三件共七轮审校的模式延续，本件按需启动）。

## 新增

- **域模型（§7.3 字段全量）**：进度（`completed_steps`/`pending_steps`——**交集须为空**："文字摘要不能将 pending 偷换成 completed"的机器可判子集）、产物引用（`artifact_refs`）、动作回执（`action_receipts`）、不确定动作（`uncertain_actions`）、等待条件（`waiting_conditions`）、各领域水位（`source_watermarks`——跨流对齐，不虚构全局序号）、简短公开摘要（≤500 字）、work_item 锚。
- **双锚**：Run 在册且 `Run.MissionID == mission_id`（404 `run_not_found` / 409 `checkpoint_scope_mismatch`）；`mission_version` 记录落盘时契约版本——恢复时判漂移（"必须能重新读取权威任务状态"）。
- **checkpoint_version 存储原子分配**：INSERT 语句内子查询 `MAX+1`——单语句原子，`(run_id, checkpoint_version)` 唯一索引兜底，并发落盘不撞号；执行面无状态落盘。
- **读写面**：`POST /v1/mission-runs/{run_id}/checkpoints`（owner 写门 + 严格拒收）、`GET …/checkpoints`（checkpoint_version 降序——最新在前）、`GET …/checkpoints/latest`（**恢复面入口**——无落盘 404 `checkpoint_not_found`，区分"无检查点"与"有"）。
- SQLite migration **v34**（checkpoints 表 + 唯一/查询索引，幂等）；memstore 双实现同语义。
- 0.7.3 为**显式写入面**；Run 生命周期自动落盘挂点（启动/等待/停止边界）随 0.7.4 跨 Harness 续办接线——本件动作面已备。

## 验证

域 UT（校验矩阵/双锚/并发 20 落盘版本 1..23 连续不撞）+ sqlite IT（v34 结构/递增/降序/JSON 列读回 + 并发 10 race）+ httpapi UT（三端点 + 错误族）+ vet/-race + it/st 串行 + 生成物幂等 + SPA 构建；真机冒烟 10 项 ALL-PASS（Run 锚全链路：runtime→agent→grant→任职链→mission→run→三级落盘→latest/降序→双锚/交集校验/无落盘语义）。

## 升级说明

- v0.7.2 数据库首次启动自动执行 migration v34（checkpoints 表，幂等）。
- 无新增必填请求字段（新端点族 `/v1/mission-runs/{run_id}/checkpoints` 为本版新增，owner token 写门）。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.3）与 [ADR-0040](docs/design/adr/ADR-0040-run-checkpoints.md)（Accepted）。
- 下一件：0.7.4 跨 Harness 续办（原生会话不可用时仍能恢复工作——消费本件 `latest` + mission_version 漂移核对重建上下文）。
