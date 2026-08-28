# 特性设计说明书（RFC 提案）：RFC-0005 收束协议与 Closure/Pause Capsule

**状态 (Status):** Draft

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-08-25

**更新日期 (Updated):** 2026-08-25

**相关 Issue/PR:** #TBD（RFC 入库时创建 tracking issue）

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| RFC 编号 | 0005 |
| 系列位置 | RFC 序列第 5 篇；"优雅收束"产品承诺的协议契约 |
| 上游输入 | [架构设计说明书](../2026-08-13-mosaic-architecture-design.md) v0.7 §6.9.4、§6.9.5、§6.5、§6.10.3、§2.6；[RFC-0003](2026-08-25-rfc-0003-attention-floor.md)（round/Floor 机制复用）；[RFC-0004](2026-08-25-rfc-0004-thread-graph.md)（closed/reopen/epoch）；[RFC-0006](2026-08-25-rfc-0006-epistemic-projection.md)（认知快照供给，本 RFC 定义消费语义） |
| 吸收开放问题 | OQ-12（各 closure_type 的接受权、纯 Agent 房间 quorum 与人类确认默认值）——本 RFC 提出裁决建议 |
| 下游 | RFC-0007（Capsule 作为一等 Memory）、RFC-0011（收束质量指标）、`internal/room` 收束路径实现 |

## 0.1 修订记录

| 版本 | 日期 | 修订人 | 说明 |
|---|---|---|---|
| v0.1 | 2026-08-25 | Mosaic 项目组 / ZCode | 初稿：触发条件、closure round 语义、三态 Intent payload 与合格性判定、六种 closure_type、Closure/Pause Capsule Schema、接受权与 quorum 建议（OQ-12）、Evidence Request 生命周期、事件族 payload |

# 1. 概述

## 1.1 简介

收束是"可逆的认知快照，不是强制共识"。本 RFC 定义：收束轮（closure round，复用 RFC-0003 的 Floor 机制、不新增权威角色）、候选 Agent 的三态评估（conclude / object / abstain）、合格异议的确定性判定（合格则中止收束、无增量反对进具名 dissent）、六种类型化 Closure Capsule 的 Schema 与接受权、预算/轮次硬顶下的 Pause Capsule（不伪装成结论）、Evidence Request 的生命周期与重开联动。"结构收敛"永远不等于"事实共识"，`silent`/`timeout`/`unavailable` 永远不计为同意。

## 1.2 动机

架构 v0.7 §6.9.5 给出了收束协议骨架，但 OQ-12（接受权与 quorum 默认值）未决；Capsule 必含字段、合格异议的判定规则、Pause Capsule 与 Closure Capsule 的写入边界都需要可测试的契约形式。收束质量是 MVP 切片 8 的验收对象，且 dissent_survival / closure_stability 指标（RFC-0011）依赖本 RFC 定义的数据结构。

## 1.3 目标

### 1.3.1 目标

1. 定义收束触发条件（三源）与 closure round 语义；
2. 定义三态 Intent（conclude/object/abstain）payload 与合格性确定性判定；
3. 定义六种 closure_type 的语义与适用条件；
4. 定义 Closure Capsule 与 Pause Capsule 的完整 Schema；
5. 定义接受权与纯 Agent 房间 quorum（OQ-12 裁决建议）；
6. 定义 Evidence Request 生命周期与重开联动；
7. 定义剩余轮次注入与 closure_horizon_visibility；
8. 定义 Closure 事件族 payload（closure.proposed/accepted/rejected、pause_capsule.created）。

### 1.3.2 非目标

- 收敛信号与病理签名的计算（RFC-0006 供给）；
- Floor/round 机制本身（RFC-0003）；
- Thread closed/reopen 的图语义（RFC-0004）；
- Capsule 写入 Memory 后的检索与组装（RFC-0007）；
- 收束质量指标的定义与标定（RFC-0011）。

# 2. 用例分析

| 用例 | 功能点 | 关键指标 | 安全隐私与 DFX 约束 |
|---|---|---|---|
| C-01 正常收束（架构 UC-006） | 收束轮收集三态；快照构建；Capsule 接受 | 快照构建（1 万事件 Thread）< 2s | 不把多数意见提升为事实 |
| C-02 合格异议中止收束 | object 改变结论边界 → closure.rejected + phase 回 active | 中止判定确定性 | 异议必须具名且指向具体 claim |
| C-03 无增量反对 | object 不合格 → 具名 dissent / parked issue | 不形成无限否决 | 无增量反对不得阻塞收束 |
| C-04 evidence_blocked | 证据缺口转 Evidence Request；以 evidence_blocked 收束 | Request 创建随 Capsule 原子 | 不伪造答案（架构 §6.10.3） |
| C-05 预算耗尽（Pause Capsule） | 硬顶只产生 pause_capsule.created + thread.paused + 未收敛标记 | 100% 硬停即时 | 不得写 closure.accepted / thread.closed（11.3） |
| C-06 重开 | 新证据满足 Request → 提议新 epoch 重开 | reopen 引用旧 Capsule | 重开代价低但必须留痕 |
| C-07 纯 Agent 房间 quorum | 无人类时按 Policy 接受 | 同构组折算（弱独立） | 防同模型伪共识（架构 §6.4） |
| C-08 剩余轮次注入 | 收束期向 Agent 注入剩余轮次；horizon 可见性记录 | 注入随上下文交付 | closure_horizon_visibility 入评测元数据（防末轮冗长） |

# 3. 方案设计

## 3.1 总体方案

### 3.1.1 触发条件（三源，任一满足）

1. **Policy 检测**：RFC-0006 收敛信号四组中多组组合达标（组合规则归 Policy 版本化参数，不由模型判定）；
2. **人类显式请求**：`propose_closure` 命令（RFC-0008 权限）；
3. **剩余轮次末段**：Thread 预算进入末段（默认最后 20% 轮次）。

预算熔断只能触发暂停与 Pause Capsule，永不触发收束（架构 §6.9.5）。

### 3.1.2 Closure Round

- 一种特殊 round 类型：复用 RFC-0003 的 Intent→Floor→发布机制，`kind = evaluate_closure`，默认 reveal = sequential；
- 候选 = 当前 Thread 参与者（人类可直接表态或弃权；硬资格、预算、cooldown 照旧）；
- 提交期写入 `thread.phase.changed(converging)`；
- `timeout` / `unavailable` 是系统状态（系统判定，非 Intent），**永远不能计为同意**（11.3）。

### 3.1.3 三态 Intent payload（RFC-0002 `closure_intent` 结构化块内容规范）

```json
{"action": "object", "claim_id": "claim_01H8...",
 "new_evidence": [{"artifact_ref": "art_..."}],
 "new_assumptions": ["成本假设在并发场景不成立"],
 "expected_impact": "推翻主结论的成本边界", "public_rationale": "..."}
```

| action | 必填字段 | 语义 |
|---|---|---|
| conclude | public_rationale | 支持生成/接受当前收束 |
| object | claim_id +（new_evidence 或 new_assumptions 至少一项）+ expected_impact | 必须指向具体 Claim，携带新证据/假设并声明预期影响 |
| abstain | reason（可选） | 主动不表态，不等于同意 |

### 3.1.4 合格性判定（确定性规则）

`object` 合格当且仅当：claim_id 指向当前快照中存在的 Claim，且满足以下之一：

1. new_evidence 引用有效（Artifact/工具结果/可追溯发言，可解析）；
2. new_assumptions 非空且非重复既有 assumption；
3. expected_impact 声明改变高优先级 Claim 状态（优先级取 RFC-0006 置信度+引用数组合，阈值 Policy 化）。

- **合格** → 中止本次收束：`closure.rejected` + `thread.phase.changed(active)`，快照中的 claim/evidence 状态更新；
- **不合格** → 记入具名 dissent 或 parked issue，不阻塞收束（防无增量否决，架构 §6.9.5）。

### 3.1.5 Closure Type 六种

| closure_type | 含义 | 典型接受权（见 3.1.6） |
|---|---|---|
| consensus | 具名共识，不把多数提升为事实 | 人类确认默认 |
| bounded_disagreement | 分歧边界、依据与反证条件已明确 | 可 Policy 自动 |
| decision | 可执行选择，可保留反对意见 | 人类确认默认 |
| option_map | 方案空间与权衡整理，未作决定 | 可 Policy 自动 |
| evidence_blocked | 继续讨论不能解决，需外部证据（必附 Evidence Request） | 可 Policy 自动 |
| abandoned | 人类或 Policy 显式放弃 | 人类显式 |

### 3.1.6 接受权与 quorum（OQ-12 裁决建议）

| 场景 | 建议默认 |
|---|---|
| 有人类房间：consensus / decision | 人类确认（moderator+，RFC-0008）；候选中 0 合格 object |
| 有人类房间：bounded_disagreement / option_map / evidence_blocked | Policy 可配自动接受（默认人工确认，纯 Agent 房间除外） |
| 有人类房间：abandoned | 显式人类动作 |
| 纯 Agent 房间 | quorum ≥ 2/3 候选 conclude 且 0 合格 object；**同构组（同 provider+model）内的一致按弱独立折算**——同构组整体计 1 票（架构 §6.4） |
| 兜底 | 任何模式下人类可随时显式接受或否决（覆盖 quorum） |

### 3.1.7 Closure Capsule Schema

```json
{
  "closure_id": "cap_01H8...", "closure_type": "bounded_disagreement",
  "thread_id": "thr_...", "discussion_epoch_id": "epoch_...",
  "conclusions": ["..."],
  "named_dissent": [{"participant_id": "par_b", "claim_id": "claim_...", "basis": "..."}],
  "assumptions": ["..."],
  "evidence": {"support": ["art_...", "evt_..."], "oppose": ["evt_..."]},
  "open_questions": ["..."],
  "falsifiers": ["若并发 A/B 结果相反则重开"],
  "conditions_of_reversal": ["..."],
  "evidence_requests": ["ereq_..."],
  "participation": {"concluded": ["par_a"], "objected": [], "abstained": ["par_c"],
                     "timeout": ["par_d"], "unavailable": []},
  "projection_version": "proj_12/alg_3/wm_1480",
  "reopen_triggers": [{"kind": "evidence_resolved", "ref": "ereq_..."}],
  "accepted_by": "par_human", "accepted_at": "..."
}
```

- Capsule 不可变；修订产生新版本（版本化引用，架构 §7.3.2 closure_capsules）；
- 体积超 RFC-0001 payload 上限时引用外置（Artifact 化的 Capsule 附录）。

### 3.1.8 Pause Capsule

- 触发：`pause_reason ∈ {budget, round_limit, human_pause}`；
- 事件：`pause_capsule.created + thread.paused`；**不得写 `closure.accepted` 或 `thread.closed`**（11.3 适应度函数）；
- 内容：尽可能复用 3.1.7 字段，另含 `pause_reason`、已覆盖范围（watermark 与已处理 claim）、未解决问题清单、恢复水位——定位是"未收敛快照"，不作为认知终态（架构 §7.3.4）。

### 3.1.9 Evidence Request

```json
{
  "request_id": "ereq_01H8...", "claim_id": "claim_...",
  "question": "需要什么事实才能判断该主张？",
  "required_evidence": ["benchmark", "user_interview"],
  "acceptance_criteria": "相同环境至少三轮 A/B 结果",
  "owners": [], "status": "open",
  "reopen_thread_on_resolution": true
}
```

- 生命周期：`open → resolved / dismissed`；resolution 附 Artifact/工具结果引用；
- 满足后系统**只能提议**按新 epoch 重开（RFC-0004）；是否自动重开由 Room Policy 与权限决定；
- owners 默认空（人类认领），提醒进 Room 侧栏。

### 3.1.10 剩余轮次注入与 horizon 可见性

进入收束后，Context Builder（RFC-0007）向所有 Agent 注入剩余轮次；Policy 选择 `exact`（精确倒计时）或 `qualitative`（"即将收束"）；所选模式记入评测元数据 `closure_horizon_visibility`，用于监测末轮提示是否诱发策略性冗长（RFC-0011）。

### 3.1.11 Closure 事件族 payload 示例

```json
// closure.proposed
{"thread_id": "thr_...", "snapshot_ref": "proj_12/alg_3/wm_1480", "trigger": "policy | human | budget_tail"}

// closure.accepted
{"closure_id": "cap_...", "closure_type": "bounded_disagreement", "accepted_by": "par_human"}

// closure.rejected
{"qualified_objection": "int_...", "reason": "new_evidence", "phase_to": "active"}

// pause_capsule.created
{"pause_id": "pse_...", "pause_reason": "budget", "watermark": 1480,
 "coverage": "...", "open_issues": ["ereq_..."]}
```

## 3.2 技术选型

| 决策点 | 选择 | 放弃方案与原因 |
|---|---|---|
| 快照构造 | 引用 RFC-0006 投影版本（只读，不重算） | 收束时实时重算：水位不一致、延迟不可控 |
| 合格性判定 | 确定性规则 | 模型判定 objection 合格性：违背原则 5，且不可回放 |
| quorum 计数 | 确定性 + 同构组折算 | 简单多数：同模型多实例伪共识 |
| 摘要者 | 按正常 Floor 产生候选，人类选择 | 固定总结者：变相 Conductor |

## 3.3 功能与性能设计

- **内部端口原型**：`ClosurePort: Propose(thread, trigger) / CollectClosureRound() / ValidateObjection(intent) -> qualified \| parked / BuildCapsule() / BuildPauseCapsule(reason) / Accept(actor) / Reject(intent)`；
- **性能目标**：快照构建（1 万事件 Thread）P95 < 2s；合格性判定 < 100ms；Capsule 接受提交 < 500ms；
- **影响范围**：`internal/room`（收束路径）、`internal/epistemic`（快照供给）、Closure 事件族 fixture、CI 断言门禁（Pause 不得写 closed、timeout 不计同意、Capsule 必含 dissent/assumptions/falsifiers）。

## 3.4 安全隐私与 DFX 设计

- **不伪造共识**：`abstain/timeout/unavailable` 与人类无输入不得聚合为同意（fixture 断言）；
- **dissent 保留**：Capsule 必须覆盖主要 claim/cluster，合格 dissent 不得在摘要中丢失（dissent_survival 指标，RFC-0011）；
- **可逆性**：reopen_triggers 强制非空（evidence_blocked 至少一条 Evidence Request）；
- **审计**：收束触发、接受/否决、quorum 计算过程入审计；
- **可靠性**：收束轮崩溃恢复后从已提交事件重建（round 状态以事件为准）。

## 3.5 编程与调用设计

### 3.5.1 编程模型基本设计

- **开发环境**：收束 fixture 仓（三态 Intent 正反例、合格/不合格 objection、quorum 场景含同构折算）；
- **开发约束**：收束路径不得调用模型；quorum 与合格性规则全部确定性；
- **可验收设计**：CI 门禁四断言（见 3.3 影响范围）+ 快照引用一致性（capsule.projection_version 可重建）。

### 3.5.2 接口定义与设计

#### ProposeClosure（命令）

接口描述：人类请求收束（或 Policy 自动提议走同一内部路径）。

接口原型：`command_kind = "propose_closure"`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| thread_id / expected_version | 输入 | — | 目标与乐观并发 | — |
| trigger | 输出 | string | human（命令）/ policy / budget_tail（系统） | 枚举 |

- 异常处理：生命周期/权限/预算校验失败拒绝。
- 约束说明：预算熔断不可走本命令。
- 变更说明：首版。
- 调用参考代码：`await mosaic.rooms.command(roomId, {command_kind:"propose_closure", payload:{thread_id}})`。

#### EvaluateClosure Intent（结构化块，RFC-0002 承载）

接口描述：候选 Agent 的三态表态；内容规范见 3.1.3。

接口原型：见 3.1.3 JSON。

- 异常处理：Schema 失败按 RFC-0002 有限重试；object 缺新证据/假设直接判不合格（不重试）。
- 约束说明：timeout/unavailable 由系统判定。
- 变更说明：首版。
- 调用参考代码：（适配器产出，域层消费。）

#### AcceptClosure / ResolveEvidenceRequest（命令）

接口描述：人类接受收束；证据满足登记。

接口原型：`command_kind = "accept_closure" | "resolve_evidence_request"`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| closure_id / request_id | 输入 | string | 目标对象 | — |
| evidence_refs | 输入（resolve） | array | 满足证据的 Artifact/事件引用 | 可解析 |
| thread.reopened 提议 | 输出（resolve） | — | 满足 reopen_thread_on_resolution 时提议新 epoch | — |

- 异常处理：接受权校验（3.1.6）；quorum 不足时拒绝并返回计数。
- 约束说明：接受人类动作全审计。
- 变更说明：首版。
- 调用参考代码：（命令通道。）

#### GetCapsule（查询）

接口描述：读取 Capsule/Pause Capsule 及其引用链（结论、异议、反证、Evidence Requests）。

接口原型：`GET /v1/rooms/{room_id}/capsules/{closure_id}`

- 异常处理：非成员 forbidden。
- 约束说明：按调用者可见性过滤引用目标。
- 变更说明：首版。
- 调用参考代码：`const cap = await mosaic.rooms.capsule(roomId, closureId)`。

### 3.5.3 编程手册设计

《收束与异议指南》（面向用户）：六种收束类型的含义、如何提交合格异议、abstain 与沉默的区别、Evidence Request 认领、重开条件。随 Web Client 文档发布。

# 4. 缺点和风险

| 风险 | 影响 | 应对 |
|---|---|---|
| 合格性规则过严/过松 | 异议被误杀或无限否决 | 规则 Policy 化 + RFC-0011 标定（dissent_survival / closure_stability） |
| 低门槛早收束掩盖未记录异议 | 共识失真 | Capsule 覆盖校验（主要 claim/cluster）+ 具名 dissent 强制 |
| 同构组伪共识 | quorum 虚高 | 同构折算（组内计 1 票）+ 弱独立标记进评测 |
| Capsule 体积超限 | payload 超约束 | 引用外置附录（Artifact 化） |
| 收束轮增加延迟 | 讨论尾段体验 | sequential + 宽松期限（OQ-19 语义不变） |
| horizon 注入诱发策略性冗长 | 末轮灌水 | closure_horizon_visibility 分组对照评测（RFC-0011） |

# 5. 现有技术

| 参考 | 借鉴 | 差异 |
|---|---|---|
| IETF rough consensus | "大致共识 + 运行代码"的异议文化、不等同全体同意 | Mosaic 要求具名异议与反证条件结构化留存 |
| Robert's Rules（division/objection） | 异议记录与表决法定足数概念 | Mosaic 无主席，quorum 由确定性规则执行 |
| 学术 peer review | revise/reject 的增量意见门槛（类比合格 objection 负担） | Mosaic 在线、低摩擦、可逆 |
| Delphi 收敛 | 多轮独立判断趋稳作为收敛信号（供 RFC-0006） | Mosaic 保留分歧边界而非强行归一 |

# 6. 未解决问题

1. **OQ-12 终裁**：quorum 阈值（建议 2/3）与各 closure_type 接受权默认值表的确认。
2. "高优先级 Claim"的判定标准（RFC-0006 置信度 + 引用数组合的权重）。
3. Evidence Request 的 owner 认领与提醒流（UI/通知设计）。
4. horizon 默认 exact 还是 qualitative（建议 qualitative 起步）。
5. Capsule 附录（Artifact 化）的引用与导出格式（联动 RFC-0010）。
6. 纯 Agent 房间 abandoned 是否允许 Policy 自动（当前建议否）。

# 7. 附录

## 7.1 参考资料

- 架构设计说明书 v0.7：§2.6、§6.5（Capsule 一等 Memory）、§6.9.4（终止语义）、§6.9.5（收束协议）、§6.10.3（Evidence Debt）、§11.3（适应度函数相关断言）
- [RFC-0003](2026-08-25-rfc-0003-attention-floor.md)、[RFC-0004](2026-08-25-rfc-0004-thread-graph.md)、[RFC-0006](2026-08-25-rfc-0006-epistemic-projection.md)、[RFC-0011](2026-08-25-rfc-0011-evaluation.md)

## 7.2 术语表

| 术语 | 英文 | 含义 |
|---|---|---|
| 收束轮 | Closure Round | 复用 Floor 机制收集三态表态的特殊 round |
| 合格异议 | Qualified Objection | 携带新证据/假设且足以改变结论边界的 object |
| 具名异议 | Named Dissent | 记入 Capsule 的公开反对（含提出者与依据） |
| 收束胶囊 | Closure Capsule | 被接受收束的结构化不可变快照 |
| 暂停胶囊 | Pause Capsule | 预算/轮次硬顶或人工暂停的未收敛快照 |
| 证据请求 | Evidence Request | 争议依赖外部事实时创建的证据需求单 |
| 反证条件 | Falsifier | 推翻结论的可观察条件 |
| 重开触发 | Reopen Trigger | Capsule 中声明的重开条件（如证据满足） |

## 7.3 文档更新计划

- 评审期（Draft → Reviewing）：收敛未解决问题 1–4；
- Accepted 后：Closure 事件族 fixture 与 CI 四断言门禁；`internal/room` 收束路径实现排期；
- 后续：RFC-0006/0011 落地时同步修订快照引用与指标定义。
