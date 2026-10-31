# ADR-0027：RuntimeLease——工作区占用、互斥与围栏代次（0.6.4）

- 状态：Accepted（2026-10-30，随 0.6.4 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3 对象表——RuntimeLease = Run 对 Runtime 的限时占用、租约代次、资源限额，续租或到期，围栏令牌拒绝旧实例提交；§5.2"同一可写工作区须独占租约或分出独立工作区"；§10.3.1 租约字段 `lease_id、runtime_id、run_id、generation、expires_at、workspace_ref、resource_limits`、心跳失败触发 unhealthy、围栏令牌只保护 Mosaic 接收的状态与受控工具调用；§10.1 调度领取时校验并领取 Lease；§13"Session、Lease、generation 等细节放执行记录或诊断视图"）、[ADR-0026](ADR-0026-local-environment-runtime.md)（Runtime ready 须绑环境——本域租约的工作区口径；busy/draining/stopped 裁定续接）、交付计划 v2.00（0.6.4：RuntimeLease：工作区占用、互斥与围栏代次）

## 背景与问题

0.6.3 让 Runtime 可发现、可准备、可查健康，但"谁在用、用到何时、旧实例
提交如何拒绝"仍无事实：没有占用记录，两个执行方可以同时写同一工作区
（§5.2 明令禁止）；没有代次与围栏，0.6.5 的 Run 无法区分现行实例与
迟到旧实例的提交（§6.1"同一 WorkItem 的有效执行代次唯一"）。本件
交付租约底座——Scheduler（0.6.5）领取、续租、释放与围栏校验的对象。

## 决策

### 1. 域与存储（internal/runtime 扩展，migration v17 一表）

```
runtime_leases：lease_id（lse_ 前缀）主键；runtime_id + environment_id
  + mission_id（主体）+ generation + fence_token + state（active |
  released | expired）+ resource_limits（JSON 声明）+ acquired_at /
  expires_at / released_at / updated_at。
  部分唯一索引 ×2：UNIQUE(runtime_id) WHERE state='active'（单运行时
  并发 1）；UNIQUE(environment_id) WHERE state='active'（同一可写
  工作区独占——§5.2）。
```

- **无事件流、不改 runtimes 表**：租约行即事实（全部时间戳在行上，
  审计经租约历史）；generation 单源于租约行——**运行时当代代次 =
  MAX(generation)**（无租约即 0），不落运行时列（单事实源，免列迁移）。
- `run_id` **不落**：Run 对象随 0.6.5 存在（ADR-0023 空壳裁定同型）；
  主体以 `mission_id` 计（经端口验存在且非 canceled——任务即占用理由）。

### 2. 领取（acquire）：前置与互斥

- 前置：runtime **ready 且已绑环境**（租约工作区 = 准备绑定的环境目录，
  租约不改绑——隔离以准备事实为准）；mission 存在且非 canceled。
- 互斥（事务内惰性清障后判定）：**环境独占**——同一环境至多一个活跃
  租约（跨运行时也互斥：可写工作区是共享排他资源，不是运行时私产）；
  **运行时容量 1**——首版单 Agent 串行（RFC"资源上限与工作区隔离必须
  满足"；容量 >1 随 0.6.5 资源上限扩展）。
- 惰性到期：任一领取/续租/围栏校验/读取触碰时，先把已过期的活跃租约
  转入 expired（代次随之递增——旧代次自此失效）；不设后台计时器
  （确定性，无定时器漂移）。
- 领取成功：generation = 当代 + 1，签发围栏令牌 `fnc_`（随机 32 hex，
  随租约行存取）；`resource_limits` 为声明（默认 `{"concurrency":1}`），
  强制随 0.6.5 Scheduler（ADR-0023"budget 存契约不存执行态"同型裁定）。
- TTL：缺省 3600s，界限 [60, 86400]；`expires_at = now + ttl`。

### 3. 生命周期与围栏代次

```
（acquire）→ active ──release──▶ released（终态，代次 +1）
                 └──到期（惰性判定）──▶ expired（终态，代次 +1）
active ──renew（心跳）──▶ active（expires_at = now + ttl，从现在起延长）
```

- **renew 即心跳**：续租从现在起延长（不从旧到期点叠加）；非 active
  （含已惰性过期）409 `lease_inactive`。心跳失败触发 runtime unhealthy
  随 0.6.5 心跳调度（本件提供原语）。
- **release**：显式释放（终态）；释放即代次递增——该租约的围栏令牌
  自此无效。
- **check-fence（围栏校验，probe 纪律恒 200）**：校验四关——租约在且
  令牌匹配、state=active、未过期（惰性转 expired）、**generation 为该
  运行时当代**（无更新租约取代）。失败携 `fence_token_mismatch |
  lease_not_active | lease_expired | generation_stale`。0.6.5 结果提交
  经此拒绝旧实例提交；**围栏只保护 Mosaic 接收的状态与受控工具调用，
  不代表能撤销外部已发生动作**（RFC 原文裁定——CLI 已发出的邮件/提交
  无法由围栏回滚）。

### 4. busy 派生化；draining/stopped 续延

活跃租约存在即忙——**不落 runtime 状态名**（租约表是唯一占用事实，
避免双事实源；ADR-0026 的 busy 裁定在此收敛为派生）。draining/stopped
随 0.6.5 执行面（那时才有在途 Run 需要排空与停机语义）。

### 5. HTTP 契约（六端点；写门禁照旧；租约细节归诊断视图）

| 端点 | 语义 |
|---|---|
| `POST /v1/runtime-leases` `{runtime_id, mission_id, ttl_seconds?, resource_limits?}` | 领取（201；409 runtime_not_ready / lease_conflict_runtime / lease_conflict_workspace / mission_canceled） |
| `GET /v1/runtime-leases?state=&runtime_id=` | 租约历史（诊断视图；state 缺省 all） |
| `GET /v1/runtime-leases/{lease_id}` | 详情 |
| `POST /v1/runtime-leases/{lease_id}/renew` `{ttl_seconds?}` | 续租（心跳） |
| `POST /v1/runtime-leases/{lease_id}/release` | 释放（代次 +1） |
| `POST /v1/runtime-leases/{lease_id}/check-fence` `{fence_token}` | 围栏校验（恒 200 携 valid/reason） |

运行环境页加租约诊断区（活跃租约徽标/历史/释放）——领取是 0.6.5
调度行为，不做人工领取入口（§13：不让用户为交办理解调度实现）。

### 6. 端口与装配

runtime 域新增 `Missions` 小端口（主体存在性与状态——app 适配
missionSvc，域不 import mission 包）；租约写路径沿用 per-runtime
串行锁 + `identityWriteTx`（并发领取恰一胜者，库级部分唯一索引兜底）。

## 非目标

- Run 对象与 run_id 关联、Scheduler 领取与心跳调度、结果提交围栏消费
  （0.6.5）；draining/stopped 排空与停机。
- resource_limits 强制、容量 >1、共享只读复用与分支/工作树分仓
  （§5.2 后半——按需随 0.6.5+）。
- 受限 Agent token（§10.4——0.6.5 Worker 面）；心跳定时器。

## 验收要点

- §5.2 互斥门控：同环境跨运行时互斥、单运行时容量互斥（并发恰一）。
- 围栏代次：释放/到期后旧令牌失效（generation_stale / lease_not_active
  可见）；再领取代次严格递增。
- 续租语义：从现在延长；过期后续租 409 可见。
- 回归 ADR-0026：租约不覆盖运行时健康/绑定事实。

## 后果

- 可写工作区从"君子协定"变为库级独占事实；0.6.5 调度有了领取/续租/
  释放/围栏的完整原语。
- 代次单源于租约行：任何代次问题只需查一张表；无运行时列漂移。
- 到期惰性判定意味着"过期后无人触碰则行仍标 active"——读取口径统一
  经服务层（触碰即转），API 消费者不会看到过期的活跃租约。

## 修订记录

- v0.1 2026-10-30 初稿（0.6.4 开工——设计先行）。
