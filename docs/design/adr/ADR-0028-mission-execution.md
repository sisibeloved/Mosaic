# ADR-0028：单 Agent Mission 执行——持久调度与独立执行任务类型（0.6.5）

- 状态：Accepted（2026-10-31，随 0.6.5 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§6 存储模型——"WorkItem / Run 的任务事实放在所属 Mission 流"；§10.2 执行时序——Scheduler → Context（BuildExecutionInput）→ Runtime（Acquire → Lease）→ Adapter（SubmitExecutionTask）→ 结果经围栏回传；§10.3.1 租约心跳与围栏；§10.3.2 Run 创建与围栏令牌；§9.2 三种判断分层——调度时实时复核）、[ADR-0023](ADR-0023-mission-contract.md)（Mission ready 可受领——执行随 0.6.5 起）、[ADR-0025](ADR-0025-execution-context-manifest.md)（存档清单为执行输入——执行存档输入不采信现场重装）、[ADR-0026](ADR-0026-local-environment-runtime.md)（Runtime ready/环境绑定）、[ADR-0027](ADR-0027-runtime-lease.md)（租约领取/续租/释放/围栏四关）、交付计划 v2.00（0.6.5：单 Agent Mission 执行：持久调度及独立执行任务类型）

## 背景与问题

0.6.2–0.6.4 交付了执行的三件底座——"执行什么"（契约 + 存档清单）、"在哪
执行"（环境 + 运行时）、"占用与旧实例拒绝"（租约 + 围栏）——但没有东西
把它们连成一次执行：Mission ready 后无调度，无 Run 记录，无向适配器提交
执行任务的通道。本件交付**第一次真实执行**：从 ready Mission 到运行中
的 CLI 进程再到完成/失败事实落库。

## 决策

### 1. Run 域（internal/run，migration v19 两表）

```
runs：run_id（run_ 前缀）主键；mission_id + attempt（第几次尝试，1 起）
  + lease_id + manifest_id + runtime_id + binding_id + agent_id（执行者）
  + state（pending → running → completed | failed）
  + result_block（完成正文截断存档——完整交付物随 0.6.8 DeliveryPackage）
  + error_text（失败原因截断）
  + fence_token（本次执行的围栏令牌——结果提交经它核对代次）
  + requested_at / started_at / finished_at / updated_at。
  UNIQUE(mission_id, attempt)；每 Mission 至多一个未终态行（部分唯一索引）。
run_events：run_id 流（attempt_requested / started / completed / failed /
  fence_rejected——迟到结果被围栏拒绝的审计事实）。
```

- **Mission 侧投影**：`missions.state` 扩展 `running`（ready → 领取租约
  创建 Run 后进入；completed/failed 回 **ready**——可再执行；0.6.6 增
  paused，0.6.7 增 unknown 语义的 Mission 侧等待原因）。Run 事实在
  run_events 流（Mission 流不混入执行细节——ADR-0023 的
  mission_events 是契约生命周期专属；RFC §6"任务事实放 Mission 流"的
  具体形态 = Mission 状态投影 + 独立 Run 流，两者经 mission_id 关联）。
- **attempt 语义**：同 Mission 可多次执行（每次 attempt+1）；同一时刻
  至多一个未终态 Run（部分唯一索引 + 服务层复核）。

### 2. 持久调度（Scheduler）

```
Scan（周期轮询，缺省 5s，可关）：
  对每个 ready Mission（且无未终态 Run）：
    1. 候选解析：owner 的 active Binding → 绑定 SlotSpec 声明的 runtime
       必需能力 → ready 且能力满足的 Runtime（单 Agent：取发现序首个）
    2. 领取租约（ADR-0027：mission 为占用主体；失败=资源忙，静默下轮再试）
    3. 装配存档清单（ADR-0025：Build(mission, binding)——执行存档输入）
       清单 blocked → 记 fence_rejected 审计 + 释放租约 + Mission 回 ready
    4. 创建 Run（pending → running）+ Mission → running（同事务）
    5. 提交执行任务（见决策 3）——结果回填 Run 终态 + 释放租约
```

- **两时点复核**（RFC §9.2）：领取前 Mission 仍 ready、Runtime 仍 ready
  （健康事实在租约领取面原子核对）；提交前围栏仍有效。
- **恢复语义**：启动时未终态 Run **不自动重跑**（0.6.7 的 unknown 处理；
  本版标记 failed 携"进程重启时在执行——结果未知"原因，不静默重试——
  RFC"禁止盲目重跑"裁定）。调度器自身无状态——全部事实在库（持久调度）。
- 手动触发：`POST /v1/missions/{id}/runs`（免等扫描立即调度——同一条
  Scan 路径同步执行一次）。

### 3. 执行面（执行器接口，复用 agent 适配器注册表）

- **Executor 端口**（internal/run 定义，app 装配注入）：`Submit(ctx,
  ExecutionInput) (block string, err error)`——阻塞至完成。0.6.5 实现
  经 `agent.Supervisor.Submit`（Kind=generate，任务携带存档清单的
  contract 层内容块 + 近期消息窗——`agent.Context.Inline`），工作目录
  由 Runtime 绑定环境决定（适配器侧现状不改——0.6.3 已绑定）。
- **围栏消费**：结果返回时核对 `lease fence`（ADR-0027 check-fence 四关）
  ——租约已释放/到期/被取代 → 记 `fence_rejected` 审计事件，结果不落
  （**迟到旧实例提交被拒绝**——0.6.4 围栏的第一个消费者）；围栏有效
  → Run 终态 + 租约释放（成功 completed / 失败 failed——失败不自动重试，
  下轮调度按 attempt+1 再来，重试策略 0.6.6+）。
- **正文发布**：结果正文截断存 `result_block`（≤8000 字，room.Run 同
  刻度）；完整 DeliveryPackage / 验收流随 0.6.8/0.6.9。

### 4. HTTP 契约（写门禁照旧）

| 端点 | 语义 |
|---|---|
| `POST /v1/missions/{mission_id}/runs` | 手动调度一次执行（同步走 Scan 单 Mission 路径；201 携 Run） |
| `GET /v1/missions/{mission_id}/runs` | Run 历史（attempt 序） |
| `GET /v1/runs/{run_id}` | Run 详情（含围栏令牌核对所需代次事实） |
| `GET /v1/runs/{run_id}/events` | Run 事件流 |
| `GET /v1/scheduler/status` | 调度器面（扫描计数/在途/最近一轮错误——诊断） |

任务中心详情增「执行」区：手动执行按钮、Run 历史（attempt/状态/耗时/
结果摘要/错误）、围栏拒绝审计可见。

### 5. 端口与装配

run 域自定义端口（不反向依赖 httpapi）：Missions（状态投影 + ready 候选
单源）、Leases（领取/释放/围栏——ADR-0027 服务同源）、ExecCtx（存档清单
Build）、Runtimes（ready 候选）、Bindings（owner 的 active Binding 解析）、
Executor（执行面——app 注入 agent.Supervisor 适配）。Scheduler 归 run 域
（一个 ticker goroutine + Stop），app 装配启停。

## 非目标

- 暂停/恢复/取消在途 Run（0.6.6）；崩溃恢复 unknown 与迟到结果深度处理
  （0.6.7——本版启动标记 failed 携未知原因，不自动重跑）。
- DeliveryPackage 结构化提交、submit_result、验收（0.6.8/0.6.9）。
- 多 Agent / 委派 / 并行 Run（RFC 全量调度面）；预算强制（budget 声明
  随 0.6.6+ 消费）；检查点（0.7）。
- 执行进度流式可视（Handle.Updates 已有——本版只取 Result 终值）。

## 验收要点

- 闭环：ready Mission →（手动触发/扫描）→ 租约领取 + 清单存档 + Run
  running → 适配器执行 → 围栏有效 → completed + 租约释放 + Mission
  回 ready；attempt 递增可再执行。
- 围栏拒绝路径：执行期间释放租约 → 迟到结果 fence_rejected 审计、
  Run 不落结果。
- 失败路径：适配器错误 → failed + 错误截断 + 租约释放；下轮 attempt+1。
- 恢复：重启后未终态 Run → failed（结果未知）不重跑。
- 资源忙：租约冲突 → 本轮跳过（无 Run 行），下轮再试。

## 后果

- 第一次端到端执行闭环：0.6.0–0.6.4 的四件底座在本件被完整消费。
- 执行结果暂为正文截断存档（交付物形态 0.6.8 定形）；围栏拒绝只有审计
  （结果丢弃不补偿——0.6.7 补迟到结果处置）。
- 调度器周期扫描在个人规模足够（≤百级 Mission）；事件驱动调度随需要再议。

## 修订记录

- v0.1 2026-10-31 初稿（0.6.5 开工——设计先行）。
