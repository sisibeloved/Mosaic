# 特性设计说明书（RFC 提案）：RFC-0007 Context 组装与 Memory

**状态 (Status):** Draft

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-08-25

**更新日期 (Updated):** 2026-08-25

**相关 Issue/PR:** #TBD（RFC 入库时创建 tracking issue）

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| RFC 编号 | 0007 |
| 系列位置 | RFC 序列第 7 篇；"最小必要上下文"原则（原则 11）的落地契约 |
| 上游输入 | [架构设计说明书](../2026-08-13-mosaic-architecture-design.md) v0.7 §6.3、§6.5、§6.10.4；[RFC-0002](2026-08-25-rfc-0002-agent-protocol.md)（任务通道、历史查询通道、usage）；[RFC-0006](2026-08-25-rfc-0006-epistemic-projection.md)（投影版本引用） |
| 已决背景 | OQ-16（上下文窗口管理归 Harness；Mosaic 统一交付讨论输入 + 权威历史查询） |
| 下游 | RFC-0005（Capsule 一等 Memory）、RFC-0010（级联删除）、RFC-0011（Receipt 评测）、`internal/context`、`internal/memory`（LE-08）实现 |

## 0.1 修订记录

| 版本 | 日期 | 修订人 | 说明 |
|---|---|---|---|
| v0.1 | 2026-08-25 | Mosaic 项目组 / ZCode | 初稿：统一讨论输入契约（七层组装+独立配额）、历史查询内容面、四层 Memory 与 Memory Item Schema、Capsule 一等 Memory、GroundedSummary、Context Receipt Schema、混合检索口径、最小必要与脱敏 |

# 1. 概述

## 1.1 简介

本 RFC 定义 Mosaic 向 Agent 交付什么（统一讨论输入与权威历史查询）、记住什么（四层 Memory）以及如何留痕（Context Receipt）。OQ-16 已决的分工是边界：Mosaic 不按 Model Binding 差异化组装、不做超窗压缩——那是 Harness 的职责；Mosaic 的保证是**统一输入 + 不存在授权可见却查不到的历史 + 每次运行可审计"实际看到了什么"**。Memory 不是真相：必须带 provenance、scope、版本与人工编辑历史。

## 1.2 动机

架构 v0.7 §6.3/§6.5 给出了组装顺序与记忆分层，但未成契约：七层组装的配额、输入体积上限、Memory Item 字段、Context Receipt 字段、GroundedSummary 的引用约束都需要精确化。RFC-0002 的 `context` 字段（≤256 KiB + receipt_ref）需要本 RFC 定义其内容；"同桌不同视角若出现源于 Harness 能力差异"的产品承诺需要统一输入契约兜底。

## 1.3 目标

### 1.3.1 目标

1. 定义统一讨论输入契约：七层组装顺序、独立配额、体积上限、水位；
2. 定义权威历史查询的内容面语义（通道归 RFC-0002）；
3. 定义四层 Memory 模型与 Memory Item Schema、生命周期事件；
4. 定义 Closure Capsule 作为一等 Memory 的写入规则；
5. 定义 GroundedSummary 契约（summarize 任务产物）；
6. 定义 Context Receipt Schema 与生成时机；
7. 定义混合检索口径（pgvector + 关键词/结构化 + scope 过滤）；
8. 定义最小必要与不可信内容标记的注入规则。

### 1.3.2 非目标

- 任务传输与通道（RFC-0002）；
- 投影算法与版本（RFC-0006，本 RFC 只引用版本）；
- 记忆的删除/保留策略（RFC-0010）；
- Agent 内部上下文窗口管理与压缩（Harness，原则 15）；
- 检索所用 embedding 模型选择（Model Gateway ADR）。

# 2. 用例分析

| 用例 | 功能点 | 关键指标 | 安全隐私与 DFX 约束 |
|---|---|---|---|
| X-01 常规组装与 Receipt | evaluate_intent/generate 前组装统一输入；生成 Receipt | 组装 P95 < 500ms（不含模型） | 最小必要（原则 11）；不可信内容带标记 |
| X-02 历史查询补全 | Agent 经通道（MCP/结构化请求）查询；查询计入 Receipt | 有界查询往返 P95 < 1s（0002 口径） | 非干扰过滤 + 审批审计（0002/0006） |
| X-03 Capsule 入 Memory | 被接受 Capsule 按 closure_type 写入 Room/Thread Memory | 随接受原子写入 | dissent/falsifiers/Requests 保留 provenance |
| X-04 记忆编辑与冲突 | 人工编辑留历史；冲突时展示来源由策略定序 | 编辑生效 ≤ 下次组装 | Memory 不是真相（UI 明示） |
| X-05 混合检索 | pgvector + 关键词/结构化；scope/visibility 过滤 | 检索 P95 < 300ms（有界集） | 命中集按主体过滤 |
| X-06 摘要生成 | merge（0004）/收束（0005）摘要走 summarize 任务 | 引用必须可解析 | 摘要是派生数据可重建（原则 9） |
| X-07 迟到与水位 | 迟到 Intent 滚入（0003）时按新水位重交付 | 水位校验强一致 | Receipt 记录实际交付水位 |
| X-08 全局偏好 | Global Profile Memory 默认关闭，opt-in | 用户明确保存才启用 | 可查看/编辑/删除（9.4） |

# 3. 方案设计

## 3.1 总体方案

### 3.1.1 统一讨论输入契约

七层组装**顺序固定**（架构 §6.3）：

| 层 | 内容 | 上限占比（默认） |
|---|---|---|
| 1 | Room 目的、Policy、Agent Profile 版本 | 5% |
| 2 | 当前 Thread 目标、状态、参与者、来源事件 | 5% |
| 3 | Thread 最近窗口 | 20% |
| 4 | 主线/父 Thread 结构化摘要与关键事件引用 | 15% |
| 5 | 被检索的 Room/Participant Memory（带 provenance） | 15% |
| 6 | 明确点名、当前 Intent 与 FloorGrant | 5% |
| 7 | 工具能力与输出 Schema 说明 | 5% |

- 剩余 ~30% 为保留余量（历史查询的空间）与结构化协议开销；
- 总量 ≤ 256 KiB（与 RFC-0002 task.context 对齐）；带 `context_watermark` 与 `receipt_ref`；
- **统一交付**：所有 Agent 得到同一组装规则与同一水位的输入（OQ-16）；不按 Model Binding 差异化。

### 3.1.2 谱系配额（防热点吞 token）

Context Builder 沿 `reply_to`、显式 `relations` 与版本化 claim 谱系回溯，四类独立配额：因果祖先 25% / 当前 cluster 30% / 对立与未解决 claim 25% / 最近窗口 20%（占第 3–5 层合并预算）；单类打满即止，不挤占其他类（架构 §6.3）。

### 3.1.3 权威历史查询（内容面）

查询维度与语义见 RFC-0002（thread/epoch、水位窗、事件 ID、关系链、claim 谱系）；本 RFC 补充：查询结果进入该 run 的 Context Receipt（`queries[]`）；返回内容带不可信标记注入（防上下文内注入）；claim 谱系在 RFC-0006 降级时自动收敛为显式关系链。

### 3.1.4 四层 Memory

| 层 | 内容 | 默认可见性 | 生命周期 |
|---|---|---|---|
| Room Memory | 目标、共识、争议、术语、未解决问题 | Room | Room 级 |
| Thread Memory | 局部结论、假设、证据与待办 | Thread participants | Thread 级 |
| Participant Memory | 该 Agent 在该 Room 的公开立场、偏好与关系 | Owner + policy | 可跨 Thread；默认不跨 Room |
| Global Profile Memory | 用户明确保存的长期偏好 | User/tenant policy | 可编辑可删除，默认 opt-in |

**Memory Item Schema**：`memory_id、version、scope、visibility、content、source_event_ids[]（非空，provenance 强制）、extractor/model 版本、置信度、created_at、expires_at、edit_history[]（人工编辑留痕）`。

生命周期事件：`memory.proposed → memory.accepted → memory.edited / memory.expired`；冲突不裁决真相：展示来源，上下文策略决定优先级（架构 §6.5）。

### 3.1.5 Capsule 一等 Memory

被接受的 Closure Capsule 写入 Room/Thread Memory，按 `closure_type` 区分（共识/边界分歧/决定/方案图/证据阻塞），不统一写成"共识"；具名 dissent、assumptions、falsifiers、Evidence Requests、reopen triggers 全部保留 provenance（架构 §6.5）。

### 3.1.6 GroundedSummary

`summarize` 任务（RFC-0002）产物结构化块：`{summary: string, cited_event_ids: [非空]}`；引用必须可解析为已提交事件，否则校验失败；用于 merge 摘要（0004）与 Thread 结构化摘要（第 4 层来源）。

### 3.1.7 Context Receipt Schema

每次 EvaluateIntent / Generate / Summarize 都生成（AR-016）：

```json
{
  "receipt_id": "rcpt_01H8...", "run_id": "run_...",
  "room_id": "...", "thread_id": "...", "discussion_epoch_id": "...",
  "context_watermark": 148,
  "delivered": {"event_ids": ["evt_..."], "memory_ids": ["mem_..."],
                 "summary_versions": {"parent": "sum_3"}, "projection_version": "proj_12/alg_3"},
  "queries": [{"kind": "history_query", "thread_id": "...", "from_seq": 100}],
  "profile_version": 3, "policy_version": "pol_7",
  "provider": "self-hosted", "model": "...",
  "redaction": {"applied": true, "rules": ["pii_basic"]}
}
```

- **不复制正文、不存 Prompt、不存隐藏推理**；只记 ID/版本清单；
- `unique(run_id)`；用途：检测引用未提供内容、区分模型/摘要/投影错误、删除影响范围追踪（6.10.4）；
- "Mosaic 交付了什么 + Agent 查询了什么"两侧都在 Receipt；Harness 内部保留/压缩不属于审计范围（OQ-16）。

### 3.1.8 混合检索口径

- pgvector（ADR-0003：起步精确/iterative scan）+ 关键词/结构化过滤组合召回；
- 过滤链：tenant → scope（四层）→ visibility → 主体非干扰；
- embeddings 表记录 `model/version/dimensions`；模型切换并行重建新索引不覆盖旧向量（架构 §8.3.2）；
- 命中集上限（默认 20 条）与相似度阈值进 Policy。

### 3.1.9 最小必要与不可信标记

- 工具结果、网页、文件内容在注入前统一打 `untrusted: true` 标记（架构 §6.7）；
- 组装只包含本次 Observe/Intent/Generate 所需（原则 11）；redaction 规则集（如 PII 基础规则）随 Receipt 记录。

## 3.2 技术选型

| 决策点 | 选择 | 放弃方案与原因 |
|---|---|---|
| 摘要生成 | RFC-0002 summarize 任务（Harness 模型） | Mosaic 直调模型：违背 OQ-20（发言与摘要流量归 Harness） |
| 检索 | PG 内混合检索 | 外挂 rerank 服务：无规模证据 |
| Memory 存储 | PG（结构化 + JSONB） | 独立记忆库：运维负担（架构 §6.5 已定） |
| Receipt 存储 | PG 表（ID 清单） | 全文快照：体积与隐私（明确禁止） |

## 3.3 功能与性能设计

- **内部端口原型**：`ContextPort: BuildInput(task, subject) -> {inline, watermark, receipt_ref} / RecordQuery(run, query) / Memory: Propose / Accept / Edit / Expire / Query(scope, subject)`；
- **性能目标**：组装 P95 < 500ms（不含模型）；检索 P95 < 300ms（有界集）；Receipt 写入随任务提交原子；
- **影响范围**：`internal/context`、`internal/memory`（LE-08）、`context_receipts`/`memory_items`/`embeddings` 表、CI 组装确定性 fixture（同输入同输出）。

## 3.4 安全隐私与 DFX 设计

- **可审计**：每次运行的 Receipt 可回答"当时看到了什么"（AR-016）；不复制正文（隐私）；
- **防注入**：不可信标记贯穿组装与查询结果注入；
- **provenance 强制**：Memory 无来源事件不可写入；
- **隐私**：Global Profile Memory 默认关闭；行为细粒度数据不长期保留（9.4）；
- **级联**：来源删除后 Memory/Receipt 的处理挂 RFC-0010；
- **确定性**：组装是纯函数（输入事件集+配置 → 输出），fixture 回归。

## 3.5 编程与调用设计

### 3.5.1 编程模型基本设计

- **开发环境**：组装 fixture（七层配额、水位、不可信标记用例）；记忆冲突样例集；
- **开发约束**：组装不得调用模型；Memory 写入必须带 source_event_ids；
- **可验收设计**：组装确定性回归 + Receipt 完整性断言（每次 run 恰好一条）+ Capsule 写入断言（0005 联动）。

### 3.5.2 接口定义与设计

#### BuildDiscussionInput（内部端口）

接口描述：为任务组装统一讨论输入（Attention/Adapter 调用）。

接口原型：`ContextPort.BuildInput(task, subject) -> ContextBundle`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| task | 输入 | Task（0002） | 任务与 grant/水位 | — |
| subject | 输入 | object | 主体与可见性策略 | — |
| inline / watermark / receipt_ref | 输出 | — | 七层组装产物（≤256 KiB） | 3.1.1 |

- 异常处理：某层来源缺失跳过并记录，不阻塞。
- 约束说明：纯函数；不感知 Model Binding。
- 变更说明：首版。
- 调用参考代码：`const ctx = await context.buildInput(task, subject)`。

#### MemoryItem CRUD（命令）

接口描述：记忆提案/接受/编辑/过期（事件族 memory.*）。

接口原型：`command_kind = "propose_memory" | "edit_memory" | "expire_memory"`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| content / scope / source_event_ids | 输入 | body | 内容与来源（非空） | — |
| memory_id / version | 输出 | — | 版本化标识 | — |

- 异常处理：来源缺失拒绝；越权编辑拒绝。
- 约束说明：编辑留痕；删除归 RFC-0010。
- 变更说明：首版。
- 调用参考代码：（命令通道。）

#### GetContextReceipt（审计查询）

接口描述：按 run 查询 Receipt（调试/评测/审计）。

接口原型：`GET /v1/admin/runs/{run_id}/receipt`

- 异常处理：仅审计角色（RFC-0008）。
- 约束说明：不含正文，天然脱敏。
- 变更说明：首版。
- 调用参考代码：`const r = await mosaic.admin.runReceipt(runId)`。

### 3.5.3 编程手册设计

《记忆管理指南》（面向用户）：四层记忆含义、查看来源、编辑与删除、全局偏好开关。内部文档《组装配置手册》：七层配额调优、检索参数、redaction 规则集。

# 4. 缺点和风险

| 风险 | 影响 | 应对 |
|---|---|---|
| 组装质量决定发言质量 | 配额不当致上下文失真 | 配额 Policy 化 + Receipt 驱动调优 + 0011 评测 |
| Receipt 清单增长 | 存储膨胀 | 保留期归 0010；ID 压缩 |
| 检索召回不足 | 记忆未命中 | iterative scan 起步 + 混合召回 + 阈值调优 |
| 记忆污染（注入经来源事件） | 长期错误行为 | provenance + TTL + 人工编辑 + 不可信标记（11.4） |
| "统一输入"与 Harness 差异化能力的落差 | 同桌不同视角 | 产品口径：差异属 Agent 个体属性（OQ-16 既定）；权威历史兜底 |
| Capsule 写入体积 | Memory 膨胀 | 附录引用外置（联动 0005） |

# 5. 现有技术

| 参考 | 借鉴 | 差异 |
|---|---|---|
| RAG 上下文工程 | 分层配额、引用可解析、混合召回 | Mosaic 以事件水位与谱系为准，不靠切块 |
| MemGPT 类记忆分层 | 分层记忆与编辑观 | Mosaic 记忆可审计、带 provenance、非真相 |
| W3C PROV（概念） | provenance 三元组思想 | Mosaic 以 source_event_ids 落地 |
| Zulip 引用回复上下文 | 显式锚点优于隐式拼接 | Mosaic 扩展为类型化关系谱系 |

# 6. 未解决问题

1. 七层配额默认比例的实测校准（Receipt 驱动）。
2. 摘要水位策略（第 4 层何时重摘要：事件增量阈值 or 纪元边界）。
3. Receipt 保留期与导出范围（OQ-14 → RFC-0010 联合）。
4. memory TTL 默认值（按层差异化？）。
5. 检索命中集上限与相似度阈值初值。
6. redaction 规则集首批范围（PII 基础规则之外）。

# 7. 附录

## 7.1 参考资料

- 架构设计说明书 v0.7：§6.3（上下文职责划分 OQ-16）、§6.5（记忆与检索）、§6.10.4（Context Receipt）、§8.3.2（embedding 重建）、§9.4（隐私）
- [RFC-0002](2026-08-25-rfc-0002-agent-protocol.md)、[RFC-0005](2026-08-25-rfc-0005-closure-capsule.md)、[RFC-0006](2026-08-25-rfc-0006-epistemic-projection.md)、[RFC-0010](2026-08-25-rfc-0010-data-lifecycle.md)

## 7.2 术语表

| 术语 | 英文 | 含义 |
|---|---|---|
| 统一讨论输入 | Unified Discussion Input | Mosaic 向所有 Agent 交付的同规则、同水位上下文 |
| 谱系配额 | Lineage Quota | 因果祖先/当前 cluster/对立 claim/最近窗口的独立预算 |
| 上下文回执 | Context Receipt | 一次运行实际交付与查询的 ID/版本清单 |
| 扎地摘要 | Grounded Summary | 引用必须可解析为已提交事件的摘要 |
| 溯源 | Provenance | 记忆到源事件的强制链接 |
| 不可信标记 | Untrusted Label | 外部内容进入上下文前的统一标注 |

## 7.3 文档更新计划

- 评审期（Draft → Reviewing）：收敛配额与 TTL 默认值；
- Accepted 后：`context_receipts`/`memory_items` Schema 进迁移；组装 fixture 进 CI；
- 后续：RFC-0010 落地时补级联细则；Receipt 评测口径随 RFC-0011。
