# ADR-0025：执行上下文清单（ContextManifest 与 ContextReceipt，0.6.2）

- 状态：Accepted（2026-10-30，随 0.6.2 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.3 Context Manifest、Receipt 与 Checkpoint——组装顺序/必需层与可选层/授权过滤先于排序计数/256 KiB 上限/Receipt 不复制完整 Prompt；§6 存储模型——`context_manifests/receipts` 为有明确所有者的持久记录；§10.2 流程——Scheduler→Context: BuildExecutionInput；§13 阶段依赖表）、[ADR-0023](ADR-0023-mission-contract.md)（决策 2——`context_manifest_ref` 随 0.6.2 出现的裁定，本 ADR 兑现）、[ADR-0022](ADR-0022-readiness-admission.md)（授权交集/格栅单源——本 ADR 消费不重定义）、[ADR-0024](ADR-0024-room-dispatch-first-slice.md)（存档执行纪律——决策 5 同型沿用）、交付计划 v2.00（0.6.2：执行上下文清单：ContextManifest 与 ContextReceipt）

## 背景与问题

0.6.1 之后，交办已是独立事实并可经网关派发，但"执行时到底交给 Agent
什么"仍无答案：RFC §10.2 中 `BuildExecutionInput(contract, binding,
checkpoint)` 的 Context 服务不存在，Mission 契约预留的
`context_manifest_ref` 无对象可指（ADR-0023 决策 2 裁定字段先于对象、
随 0.6.2 增列）。

Scheduler 要到 0.6.5 才有。0.6.2 交付的是**装配与存证本身**：把"本次
执行输入引用了什么、装配了什么、裁剪了什么"变成可查询、可验证、可
重放的持久事实——Runtime 到位后从存档清单执行，不采信现场重装。

与既有 `internal/contextx`（RFC-0007 房间对话七层组装，讨论房间用）
**不是同一对象**：那是房间消息流的对话上下文；本件是 Mission 执行
输入的事实存档。新域独立建包 `internal/execctx`，不混同、不合并。

## 决策

### 1. 域与对象（internal/execctx，migration v15 两表）

```
exec_context_manifests：manifest_id（cmf_ 前缀）主键；mission_id +
contract_version（钉住的契约版本行）+ binding_id/agent_id（装配对象）+
state（assembled|blocked）+ reason_code + digest + 层账本（含内容块）+
水位向量 + total_bytes + created_at；UNIQUE(mission_id, digest)
exec_context_receipts：receipt_id（cxr_ 前缀）主键；manifest_id 唯一
外键 + 交付账目（层状态/字节/截断/遗漏及原因）+ policy_version +
created_at（表名冠 exec_——房间域 RFC-0007 context_receipts 已占名）
```

- **无事件流**：RFC §6 把 manifests/receipts 列为"有明确所有者的持久
  记录"（同 sessions/checkpoints 类），不进候选流清单；mission_events
  是契约生命周期专属（created/revised/status_changed），不混入装配
  事实——与 `slot_agent_grants`（ADR-0022）同一裁定。
- **Receipt 不复制内容**：完整层内容块在 Manifest（Runtime 执行时消费）；
  Receipt 只记账目与摘要（RFC"不复制完整 Prompt"）。`run_id` **不落**
  ——Run 对象随 0.6.5 存在，空壳字段不落库（ADR-0023 决策 2 同型）。

### 2. 装配：确定性、层模型 0.6.2 子集（RFC §7.3 裁剪）

`Build(mission_id, binding_id)`：读 Mission 契约**当前最新版本行** +
任职 Binding（钉其 SlotSpecVersion 对应模板版本行）+ 授权档案（经
ADR-0022 交集单源）+ 来源房间事件流，按下表装配层账本。同状态必同
结果（digest 可复算——与 contextx 同纪律）。

| 层 | 角色 | 0.6.2 内容 | 未提供时 |
|---|---|---|---|
| `constraints` | 必需 | 约束与有效权限：readiness 报告（满足/缺失/降级）、有效 workspace 格栅（min(授权,模板)）、工具有效集（模板∩授权）、委派策略（模板∩契约，缺省不允许）、budget 声明 | — |
| `responsibility` | 必需 | 当前职责：Binding（agent/实例/模板版本/状态）+ 模板 responsibility/deliverables 文本 | — |
| `contract` | 必需 | 任务契约/验收：全量 Content + contract_version + status + room_source | — |
| `tools` | 必需 | 工具接口：有效工具清单（required/optional 分层，名称即能力名） | — |
| `sources` | 可选 | 证据引用：`source_refs` 原文列表（内容解析与按需查询随 0.6.5+） | 契约必有 ≥1，仅无配额概念 |
| `recent_messages` | 可选 | 近期消息：来源房间 **public** `message.posted` 事件，配额 64 KiB | 无 room_source → 省略 `no_source_room` |
| checkpoint / memory / 决策胶囊 | — | 未建模 | 账本如实记 omitted：`checkpoint_unavailable`（0.7）、`memory_unavailable`（0.7）、`decision_capsule_unavailable`（随 0.6.5/0.6.11） |

- **必需层不得截断**：必需层合计超出总预算 → state=blocked
  `required_over_budget`（§7.3"必需内容放不下进入 blocked"）。
- **可选层独立配额截断并标明遗漏**：recent_messages 从新往旧装满即停，
  账本披露 `dropped`（更旧被裁条数）与单条超限截断数；per-message
  上限 8 KiB（超限取头部并标记该条截断）。
- **总输入上限 256 KiB**（RFC-0002 沿用）；"取适配器声明上限较小者"
  随 0.6.3 Runtime 落（当前无适配器声明）。`policy_version` 固定
  `context-assembly/1`——层表/配额/渲染规则任何变更须升版本。

### 3. 授权过滤先于排序、摘要与计数（§7.3 硬约束）

recent_messages 只装 `public` 可见事件；`participants/moderators/
system` 事件**不进入、不计数、不列名**（不泄漏被过滤内容的存在性——
账本只披露配额裁剪，不披露权限过滤）。房间水位 = 该层**实际交付的**
最新一条事件 seq（交付前沿，非读取前沿）——水位只反映已交付内容。
participants 域消息随 0.6.5 Runtime 的 Agent 房间身份定义后扩展。

### 4. 状态与 blocked（probe 纪律：blocked 是数据不是错误）

装配恒产出完整层账本（钉住一切引用与版本），随后按**固定优先级**判
state（先命中先记录，可测）：

```
owner_mismatch（binding.agent ≠ 契约 owner——执行者必须是负责人）
  > binding_not_active（binding.status ∉ {ready, active}）
  > readiness_not_met（授权交集缺必需项，层内携 missing 明细）
  > required_over_budget
```

- `assembled` 之外的正常出口只有：Mission 不存在 404、Binding 不存在
  404、Mission 已 canceled 409（终态无执行语义）、binding_id 缺失 400。
- readiness 判定是**装配时点预演**（层 1 如实记录报告）；执行时点强制
  随 0.6.5 Scheduler——与 ADR-0023"budget 存契约不存执行态"同型。

### 5. digest 即身份；确定性重建去重（ADR-0024 决策 5 同型）

`digest = SHA-256(规范 JSON{层账本 + 水位 + 钉住版本})`——不含
created_at 与 ID。`UNIQUE(mission_id, digest)`：

- 同状态重复 Build → 返回既有 Manifest/Receipt，`replayed=true`
  （单行原子 INSERT，撞唯一键即转查——无两段式需求：无先创建后完结
  的副作用链，与 dispatch ClaimKey 场景不同，如实登记）。
- 状态变化（房间新事件/契约修订/授权变更）→ digest 变 → **追加新行**：
  每个 Mission 保留"各时点会交付什么"的完整历史，可审计。
- 0.6.5 Runtime 从**存档清单**执行（执行存档输入，不采信现场重装——
  ADR-0024"执行存档请求"同型纪律）。

### 6. `context_manifest_ref` 兑现（ADR-0023 决策 2）

裁定为**查询时联查**，不落契约内容字段：manifest 以 mission_id 寻址、
钉 contract_version；契约修订是全量替换的不可变版本行，塞入执行时点
引用会破坏版本行语义（manifest 是执行事实，不是契约内容）。消费方
（0.6.5 Scheduler）以 mission_id → manifests 查询定位。

### 7. HTTP 契约（三端点；写门禁照旧）

| 端点 | 语义 |
|---|---|
| `POST /v1/missions/{mission_id}/context-manifests` `{binding_id}` | 装配 + 存档（owner-token 写门禁）；200 携 manifest/receipt/replayed；blocked 恒 200 携 reason_code |
| `GET /v1/missions/{mission_id}/context-manifests` | 摘要列表（无内容块，创建序倒序） |
| `GET /v1/context-manifests/{manifest_id}` | 全量视图（含层内容块与 receipt） |

### 8. 端口与装配（app 组合根）

execctx 自定义小端口（不反向依赖 httpapi）：Missions（Get 单源）、
Bindings（binding+钉版本模板行一次解析）、Grants（授权档案——**单次
装配只读一份**：readiness 经 ADR-0022 交集单源纯函数 EvaluateReadiness
与工具清单/域授权共用同一快照，审校 P4）、Rooms（room.EventReader
分页——跨页累积全量后过滤，沿用 ADR-0024 P2#3 裁定）。SPA 两步选择器
（实例→任职）复用既有 slot 端点，不新增 slot 契约面。

## 非目标

- Scheduler/Runtime/Lease/Run（0.6.3–0.6.5）；`run_id` 关联与执行消费。
- Checkpoint、记忆召回、决策胶囊层（0.7 / 0.6.11）；适配器声明上限协商。
- participants 域消息、按需查询入口（on-demand query）、token 窗口
  管理（Harness 职责，§7.3）。
- RFC §7.3 组装顺序中"授权记忆/文档与证据"内容解析——0.6.2 只存引用。

## 验收要点

- AT-03（输入事实可审计）：Manifest/Receipt 可查、digest 可复算。
- 回归 AT-07：readiness 报告进层 1，交集单源不重定义。
- 门控反例：必需层超预算 blocked；授权过滤先于计数（非 public 事件
  不影响账目）；同状态重建 replayed；canceled 拒装配；owner/binding
  不匹配 blocked 可见。

## 后果

- "交付什么"从隐式（执行时现场拼）变为显式存档事实；0.6.5 Runtime
  的执行输入可审计、可重放。
- 每 Mission 保留装配历史（digest 追加），存储随装配次数线性增长——
  个人规模可接受，无清理策略（后续按需）。
- 房间对话上下文（contextx）与执行上下文（execctx）双轨并存：前者
  服务讨论闭环，后者服务 Mission 执行，互不耦合。

## 修订记录

- v0.1 2026-10-30 初稿（0.6.2 开工——设计先行）。
- v0.2 2026-10-30 审校修复登记（独立审校"1 P1 + 7 P2，建议修复后定版"）：**(P1)** 合法模板可不声明 tools（可选指针）——工具层改经空值安全访问（声明缺省助手），无声明即空清单不崩溃，补无 tools 模板装配回归。**(P2#2)** SPA 负责人变化清空任职选择 + 提交前核对任职仍属当前负责人。**(P2#3)** 历史查看与装配共用结果请求序号，迟到响应不覆盖最新结果。**(P2#4)** Readiness 端口移除——单次装配只读一份授权：准入报告经 ADR-0022 单源纯函数 EvaluateReadiness 与工具清单/域授权同快照推导（不可能出现"必需工具满足 Ready=true 而有效工具为空"的矛盾清单）。**(P2#5)** 可选层配额同时受剩余总预算约束（必需 + 已装可选先行扣减，首条消息也不例外）；总量超限防御性 blocked。**(P2#6)** 摘要改规范化编码（JSON 往返 map 键序字典序 + json.Number 保真）后哈希——typed（首次生成）与 map（存储读回）同字节序列；模板版本入水位向量（slot_spec:{id}），digest 输入在清单行内自包含、存储往返可复算（DigestOf 导出，UT/IT 双钉）。**(P2#7)** canceled 后保留只读上下文卡（历史/层内容可查，仅禁新装配）。**(P2#8)** 声明未授权的上下文域（room.recent）在读取、计数前省略（omitted context_not_authorized，房间读取不发生）；模板未声明该域不扩大要求照常装配。另补清单/回执同事务回滚 IT（回执插入失败清单行不留半成品）。
