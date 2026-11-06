# ADR-0040：0.7.3 Checkpoint——可恢复业务状态（0.7.3）

- 状态：Accepted（2026-11-06，随 0.7.3 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.3——"`Checkpoint` 独立于 Receipt，保存可恢复业务状态：session_id、checkpoint_version、mission_version、work_item_id、completed_steps、pending_steps、artifact_refs、action_receipts、uncertain_actions、waiting_conditions、source_watermarks，以及简短公开进度摘要。它必须能重新读取权威任务状态；文字摘要不能将 pending 偷换成 completed。正常安全边界、等待与交接时落盘；最后一次动作之后尚未落盘则恢复时核验回执"）；[ADR-0030](ADR-0030-run-crash-recovery.md)（Run 崩溃恢复——权威执行状态）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.3 唯一交付特性：Checkpoint——进度、待办、产物、动作回执与等待条件）。

## 背景与问题

Run 的权威状态（state/result）只记录执行终态事实，恢复续办所需的**业务中间态**——已完成/待办步骤、产物引用、不确定动作、等待条件、各领域水位——没有持久载体。0.7.4 跨 Harness 续办（"原生会话不可用时仍能恢复工作"）依赖这些结构化事实重建工作上下文；没有 Checkpoint，续办只能靠文字猜测。

## 决策

### 1. 域模型（RFC §7.3 字段全量）

`internal/checkpoint` 包：

| 字段 | 说明 |
|---|---|
| checkpoint_id | `ckp_` 前缀生成式 |
| run_id / mission_id | **双锚**：Run 在册且 `Run.MissionID == mission_id`（一致性门——checkpoint 属于该 Run 的任务）；mission_version 记录落盘时契约版本（"必须能重新读取权威任务状态"——恢复时以此判漂移） |
| checkpoint_version | **存储原子分配**（同 run 事务内 `MAX+1`——执行面无需记住版本，并发落盘不撞号） |
| work_item_id | 可选工作项锚 |
| completed_steps / pending_steps | 结构化进度事实；域校验**交集为空**（"文字摘要不能将 pending 偷换成 completed"的机器可判子集——同一步骤不得同时出现在两列） |
| artifact_refs / action_receipts / uncertain_actions / waiting_conditions | 产物引用 / 已核验动作回执 / 未定动作 / 等待条件（各 ≤64 条、每条 ≤300 rune） |
| source_watermarks | 各领域水位 `map[string]string`（跨流对齐——不虚构全局序号） |
| progress_summary | 简短公开摘要（≤500 rune） |
| created_at | 时钟单源 |

### 2. 读写面（apigen 冻结）

- `POST /v1/mission-runs/{run_id}/checkpoints`——落盘一条（owner 写门 + 严格拒收；锚门 404 `run_not_found` / 409 `checkpoint_scope_mismatch`）。
- `GET /v1/mission-runs/{run_id}/checkpoints`——列表（checkpoint_version **降序**——最新在前；run 未知 404）。
- `GET /v1/mission-runs/{run_id}/checkpoints/latest`——最新一条（无落盘 404 `checkpoint_not_found`——恢复面区分"无 checkpoint"与"有"）。

0.7.3 为**显式写入面**（执行面/人工按需落盘）；Run 生命周期的自动落盘挂点（启动/等待/停止边界）随 0.7.4 续办面接线——本件动作面已备。

### 3. 存储（migration v34）

`checkpoints` 表（checkpoint_id 主键 + `(run_id, checkpoint_version)` 唯一索引 + run_id 索引）；JSON 列存步骤/回执/水位。memstore 双实现（分配/排序同语义）。

### 4. 测试与验证

域 UT（校验矩阵：交集空/长度/双锚一致性；版本分配并发）、sqlite IT（v34、版本递增与并发不撞号、列表降序）、httpapi UT（三端点 + 写门 + 404/409）、真机冒烟（Run 锚链路：建任务 → 手动 Run → 落盘三级 checkpoint → latest/列表）、全量 vet/-race + it/st + 生成物幂等。SPA 本件不做（Checkpoint 是系统事实——0.7.4 续办面可视化时一并交付）。

## 后果

- 可恢复业务状态有了结构化持久载体；0.7.4 跨 Harness 续办以 `latest` + mission_version 漂移核对重建上下文，不再依赖文字猜测。
- checkpoint_version 存储原子分配——执行面（未来自动挂点）无状态落盘。
- 交集空校验是"pending 不得偷换 completed"的弱机器判据（摘要自由文本不可判）——完整纪律由执行面写入规范承担。
