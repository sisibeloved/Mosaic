# ADR-0023：MissionContract 独立事实源——版本化契约与契约状态（0.6.0）

- 状态：Accepted（2026-10-30，随 0.6.0 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.1 Mission 对象边界、§3.2 Mission 状态与转移、§3.3 不变量"恰有一个 accountable owner 或显式 unassigned"、§6.1 MissionContract、§6.2 委派、§11.2 create_mission / revise_mission / cancel_mission 候选命令、§11.2.1 网关回执与幂等、AT-13；2026-10-30 v0.6 网关增补）、[ADR-0019](ADR-0019-slotspec-versioned-template.md)（不可变版本行模式——本 ADR 沿用）、[ADR-0018](ADR-0018-agent-lifecycle.md)（事件流 + BEGIN IMMEDIATE 模式）、[ADR-0021](ADR-0021-binding-lifecycle.md)/[ADR-0022](ADR-0022-readiness-admission.md)（owner 来自身份域；任职/准入先行）、交付计划 v2.00（0.6.0 唯一交付特性：独立 MissionContract：独立事实源、版本与契约状态）与 v2.20（Dispatch 排布留待本 ADR 裁定）

## 背景与问题

0.5 领域线（身份/职责/任职/准入）已收官。0.6 交付"从交办到执行、交付、验收
的单 Agent 闭环"，第一件是**契约**：Mission 作为独立事实源——可验收的一次
交办（§3.1），归 Workspace、不能由房间删除级联销毁。没有契约对象，后续的
执行（0.6.5）、验收（0.6.9）、回房（0.6.10）都没有挂靠点。

RFC 裁定：契约**带版本**（"新要求进入带版本的契约修订"）、修订经
`expected_version` CAS（§11.2：版本冲突返回当前版本；AT-13：两个并发修订
冲突明确）；创建时"来源或验收缺失则保留 draft"（不能捏造已达成共识）。

## 决策

### 1. 独立事实源形态（沿用 SlotSpec/Binding 已验证模式）

```
missions（投影行）：
  mission_id（mi_ 生成式）、contract_version、status、
  content（契约 JSON 单列）、owner_agent_id、created_at、updated_at
mission_contract_versions（不可变版本行，(mission_id, version) 主键）：
  契约 JSON 快照——修订即追加，历史永不清除（ADR-0019 同款）
mission_events（append-only 流）：
  created / contract_revised / status_changed——写路径单事务
  [校验→投影→版本行→事件]（BEGIN IMMEDIATE，ADR-0018 全库约定）
```

- `mi_` 前缀生成式 ID（ag_/bd_/si_ 同族）。
- **归 Workspace，不挂 Room**：0.6.0 无房间关联字段——来源引用与
  `(room_id, task_id)` 唯一映射随 0.6.1 房间交办落（`source_refs` 先为
  自由引用串，不做引用校验）。

### 2. 契约内容：§6.1 字段的 0.6.0 子集（不落空壳字段）

落库：`objective、scope、constraints、acceptance_criteria、
deliverable_specs、budget、source_refs、return_targets、
delegation_policy、owner_agent_id、reviewer`（requester 固定本地
owner，视图可见——多入口请求方随网关/委派扩展才有语义）。

**不落**：`decision_ref`（引用对象随 0.6.1 房间交办/决策胶囊出现）、
`context_manifest_ref`（随 0.6.2 ContextManifest 出现）——字段先于对象
落库即空壳伪装能力，两处随各自小版本增列。

- **budget 安全缺省**：`{并发 1、总 Run 数 32、墙钟 86400s、重试 0、
  委派深度 0}`——单 Agent、不重试、不委派起步；数值上限校验入库。
  运行时强制（超配额拒绝）随 Scheduler（0.6.5）落，0.6.0 只做契约事实。
- `delegation_policy` 缺省 = 不允许委派（与 SlotSpec 同一安全缺省）。
- `owner_agent_id`：须为存在的 Agent（AgentLookup 端口本地定义、app
  装配注入身份服务——与 slot 同口径）；**任意生命周期状态可指派**——
  owner 是责任归属事实，行为能力由执行/交接时点校验（0.6.5/0.6.12）。
  未指派 = 显式 unassigned（空串）。

### 3. 契约状态 0.6.0 子集（执行态随后续小版本扩展）

| 状态 | 语义 | 进入 |
|---|---|---|
| `draft` | 契约不完备（验收/来源/交付物缺失或 owner 未指派）——保存明确请求，不捏造共识 | 创建时完备性判定未过 |
| `ready` | 契约完备、可受领（执行随 0.6.5 起） | 创建即完备，或修订补齐 |
| `canceled` | 唯一终态；不可修订、不被迟到回调翻转 | draft/ready → cancel |

- **完备性判定**（create 与 revise 同一单源）：`objective ∧ scope ∧
  acceptance_criteria ≥1 ∧ deliverable_specs ≥1 ∧ source_refs ≥1 ∧
  owner 已指派`。RFC 原文"来源或验收缺失则保留 draft"；本 ADR 增裁定：
  owner 未指派（unassigned）同停 draft。
- §3.2 的 `running/waiting/paused/reviewing/accepted/failed` 不在
  0.6.0——执行（0.6.5/0.6.6）与验收（0.6.9）落地时扩展；cancel 在
  0.6.0 无在途工作，单步至终态（"所有子项停止才 canceled"在无子项时
  平凡成立；在途处置版随 0.6.6）。

### 4. 版本与 CAS（AT-13）

- `contract_version` 自 1；revise = **全量替换 + 追加不可变版本行**，
  version+1；事件 `contract_revised{from_version, to_version}`。
- revise/cancel 必带 `expected_version`：不匹配 → 409 `version_conflict`
  （并发修订恰一胜者，败者拿回当前版本可见重试——不静默合并）。
- 完备性在 revise 上重判：draft 补齐 → ready（事件 `status_changed`）；
  ready 修订保持 ready；canceled 一律拒修订（409 `invalid_transition`）。
- 幂等键体系**不在本版**：`(caller_ref, scope, idempotency_key)` 唯一
  约束与 DispatchReceipt 属网关命令面（§11.2.1），随 0.6.1 Dispatch
  首切片落；0.6.0 的 REST 创建非重试面，不带幂等键。

### 5. HTTP 契约（七端点；写面过生产写门）

| 端点 | 语义 | 主要失败 |
|---|---|---|
| `POST /v1/missions` | 创建（内容校验 400 `invalid_mission`；完备性判定落 draft/ready） | 400 / 404 `agent_not_found` / 503 |
| `GET /v1/missions` | 契约列表（创建序摘要） | 503 |
| `GET /v1/missions/{mission_id}` | 详情（当前契约 + 状态 + 版本） | 404 `mission_not_found` |
| `POST /v1/missions/{mission_id}/revise` | 全量替换修订（CAS） | 400 / 404 / 409 `version_conflict` / `invalid_transition` |
| `POST /v1/missions/{mission_id}/cancel` | 取消（CAS + 原因入事件；终态） | 404 / 409 |
| `GET /v1/missions/{mission_id}/versions` | 不可变版本列表（升序） | 404 |
| `GET /v1/missions/{mission_id}/events` | 事件流（seq 升序） | 404 |

### 6. Dispatch 排布裁定（兑现计划 v2.20 留白）

- `internal/dispatch` **首切片随 0.6.1 房间交办**落地：确定性最小实现
  （规范化请求 → 显式/续接/Room 默认目标解析 → 投递关联 Mission）+
  `resolve_request/dispatch_request` 候选命令的 REST 化 +
  DispatchReceipt 与 `(caller_ref, scope, idempotency_key)` 幂等约束。
- **不增列 0.6.13+ 小版本**：网关能力按 RFC §13 阶段依赖表并入既有
  0.6 小版本（确定性寻址与分发 AT-14/15/16/18 随 P1 各件逐步兑现）。
- 可选语义选择模块（AT-17）待确定性路径稳定后**另行裁定**小版本与
  模型选型（届时补计划行与 ADR）。**0.6.0 不含任何网关代码。**

### 7. UI：/missions 任务中心首屏

新页面（AppShell 导航入口）：创建表单（目标/范围/验收/交付物/来源/预算
高级项/负责人下拉）、契约列表（状态徽标 draft/ready/canceled + 版本）、
详情（当前契约 + 版本历史 + 事件流）、修订（expected_version 冲突 409
可见）、取消（原因输入）。请求序号守卫与错误态纪律沿用 0.5 系列教训。

## 非目标（0.6.0 明确不做）

- 执行、调度、Run、RuntimeLease（0.6.3–0.6.7）；验收与退回（0.6.9）；
  结果回房（0.6.10）；决策重开（0.6.11）；换负责人交接仪式与承接回执
  （0.6.12——0.6.0 的 owner 修订无仪式，事件留痕）。
- `decision_ref` / `context_manifest_ref` 字段（对象未存在）。
- 房间来源引用校验与 `(room_id, task_id)` 映射（0.6.1）。
- Goal 关联、WorkItem 工作图（0.8/0.9 线）；网关 Dispatch（0.6.1 起，
  见决策 6）；幂等键与回执体系（同前）。

## 测试策略（先行红→绿）

- **UT**：契约校验族（必填/上限/模式/预算界限/委派枚举）；完备性两态
  （创建与修订同源判定）；CAS 并发修订（8 路恰一胜者、败者
  version_conflict）；cancel 终态全拒；事件序列（created →
  contract_revised → status_changed）；owner 存在性（端口注入）；
  budget 缺省填充。
- **IT（SQLite）**：migration v13 新库 + v0.5.5 真旧库（v12）升级；
  版本行不可变（无 UPDATE/DELETE 路径）；并发 revise 串行化恰一胜者。
- **API**：七端点全链 + 反例（400 校验 / 404 / 409 CAS 与终态 / 写门）。
- 真机冒烟：创建 draft/ready 两态 → 修订补齐推进 → 并发修订冲突 →
  cancel 终态 → 版本/事件只读面 → 重启持久。

## 后果

- Mission 成为 0.6 域挂靠点：契约版本先行，后续执行事实按 Run/WorkItem
  独立对象挂靠，不改写契约历史。
- 契约内容全量替换 + 版本快照：简单可审计；字段级 diff 视图可后派生。
- budget 存契约不存执行态：0.6.5 Scheduler 消费时才强制——此前是声明
  事实，不是运行限制（发布说明如实标注）。
- /missions 页成为任务中心首屏：后续 0.6.x 的执行/验收视图在此生长。

## 修订记录

- v0.1 2026-10-30 初稿（0.6.0 开工——设计先行）。
- v0.2 2026-10-30 审校修复登记（独立审校"2 P1 + 1 P2，建议修复后定版"）：**(P1#1)** 详情/草稿按任务隔离（key 重挂载）+ 写入归属守卫（详情加载完成且 mission_id 一致才可修订/取消）——切换目标后旧状态不得写入新任务。**(P1#2)** 修订请求从完整契约快照构造——全量替换语义下未编辑字段（constraints/return_targets/budget/delegation/reviewer）必须显式回传，不得漏传清空。**(P2#3)** 请求体必填五键存在性检查（缺失/null → 400，显式空数组才是清空）+ budget 键在场严格界限校验（全零 400，缺省仅在键缺席时成立）——补入决策 5 的错误语义。
