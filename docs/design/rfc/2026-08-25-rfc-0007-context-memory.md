# 特性设计说明书（RFC 提案）：RFC-0007 Context 组装与 Memory

**状态 (Status):** Draft

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-08-25

**更新日期 (Updated):** 2026-09-03（v0.2 裁定）；2026-09-03（v0.3 实现落地面）

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
| v0.2 | 2026-09-03 | 陆尘裁定 / ZCode | **记忆双平面与责任边界裁定（§7.4）**：双参考调研（Hermes Agent 跨会话记忆 / MaiBot 群聊记忆）后负责人五点裁定——session 归 harness / memory 归房间（共享、可审计、可编辑）；按需平面 SQLite FTS5 起步（§3.1.8 修订，pgvector 无限期推迟）；恒常平面容量纪律（字符上限+水位可见+超限倒逼合并）；承诺/待办追踪改口为**带责任人的 tasklist**（非记忆系统，落 RFC-0012 OQ-A）；记忆查看/编辑/纠错 UI 为 M3-3 验收项 |
| v0.3 | 2026-09-03 | Mosaic 项目组 / ZCode | **M3-3 实现落地面（v0.2 裁定的执行）**：FTS5 前置验证定案——modernc v1.57.0（SQLite 3.53.3）FTS5 可用，unicode61 对中文整串成单 token 不可用，**trigram 对 CJK 子串 ≥3 字与英文正确命中**，<3 字查询回退 LIKE 子串（语义与线性基准一致，无需自研 fts5_cjk 类扩展）；恒常平面 CapsuleBudgetRunes=3000（room 包常量，水位透出 debug/memory 与公开 memory 端点，dropped_count>0 即超预算信号）；**v1.36 声明失实修复**——capsule 注入（第八层）此前是死代码（capsuleMemoriesOf 无调用点），本版接进 assembleChat 并升级为编辑后视图；组装层从 8 层扩至 10 层（+retrieved_memory 按需召回 +tasklist）；编辑闭环 = memory.edited 事件 + MemoryCapsulesOf 投影 + edit_memory 命令 + 公开 GET /v1/rooms/{id}/memory + SPA 记忆面板（查看/编辑/edit_history/容量水位）；按需平面另有组装时召回（刺激关键词 → CJK bigram 重叠/ASCII 子串 → 近窗外 top5 带 provenance）与房内搜索端点 GET /v1/rooms/{id}/search（FTS5 trigram + LIKE 回退 + 自愈重建） |

# 1. 概述

## 1.1 简介

本 RFC 定义 Mosaic 向 Agent 交付什么（统一讨论输入与权威历史查询）、记住什么（四层 Memory）以及如何留痕（Context Receipt）。OQ-16 已决的分工是边界：Mosaic 不按 Model Binding 差异化组装、不做超窗压缩——那是 Harness 的职责；Mosaic 的保证是**统一输入 + 不存在授权可见却查不到的历史 + 每次运行可审计"实际看到了什么"**。Memory 不是真相：必须带 provenance、scope、版本与人工编辑历史。**责任边界裁定（2026-09-03，§7.4）**：Harness 的 CLI 会话是 Agent 的私有工作记忆（OQ-16 边界内，不透明、不可审计、不可编辑——可接受）；Mosaic Memory 是**房间的共享记忆**（有 provenance、可审计、可编辑、人类可纠错）——两者所有者与审计要求不同，互不替代，Mosaic 只建后者。

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
| X-05 混合检索 | pgvector + 关键词/结构化；scope/visibility 过滤（个人版 FTS5 起步，§7.4） | 检索 P95 < 300ms（有界集） | 命中集按主体过滤 |
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

### 3.1.8 混合检索口径（v0.2 修订：FTS5 起步，pgvector 无限期推迟）

- **个人版按需平面以 SQLite FTS5 全文检索起步**（v0.2 裁定，§7.4）：事件日志本就在库内全量，对 message 正文建 FTS5 索引 + 关键词/结构化过滤组合召回；pgvector/embedding **无限期推迟**，以 FTS5 召回不足的狗粮实录为重启条件；前置验证项：modernc.org/sqlite 的 FTS5 编译可用性与 CJK tokenizer 方案（参考实现 Hermes 为中文专门做 fts5_cjk 扩展）；
- 过滤链：tenant → scope（四层）→ visibility → 主体非干扰；
- （向量形态保留为未来服务端部署选型）embeddings 表记录 `model/version/dimensions`；模型切换并行重建新索引不覆盖旧向量（架构 §8.3.2）；
- 命中集上限（默认 20 条）进 Policy；相似度阈值随向量推迟挂起。

### 3.1.9 最小必要与不可信标记

- 工具结果、网页、文件内容在注入前统一打 `untrusted: true` 标记（架构 §6.7）；
- 组装只包含本次 Observe/Intent/Generate 所需（原则 11）；redaction 规则集（如 PII 基础规则）随 Receipt 记录。

## 3.2 技术选型

| 决策点 | 选择 | 放弃方案与原因 |
|---|---|---|
| 摘要生成 | RFC-0002 summarize 任务（Harness 模型） | Mosaic 直调模型：违背 OQ-20（发言与摘要流量归 Harness） |
| 检索 | PG 内混合检索（个人版：SQLite FTS5 起步，§7.4 裁定） | 外挂 rerank 服务：无规模证据 |
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
| Hermes Agent（Nous Research，2026-09-03 调研） | 恒常/按需双平面（硬上限策展文件冻结注入 + FTS5 原文检索零 LLM）；容量压力倒逼策展（不自动压缩，超限报错由 agent 当轮合并）；write_approval 门控 + pending 队列；外部记忆 provider 并行增强、永不替代内置存储 | Mosaic 恒常平面=胶囊/MemoryItem 注入层、按需平面=权威历史查询；提案-接受生命周期已事件溯源化（memory.* 事件族），agent 提案面留待工具面成熟 |
| MaiBot A_Memorix（群聊记忆，2026-09-03 调研） | 画像=证据驱动+人工覆盖优先（纠错触发自动刷新）；Episode 异步流水线（pending/processed/failed，可按来源重建）；来源管理支撑批量运维（影响预览/回收站恢复）；显式生命周期操作集（强化/弱化/冻结/遗忘）；编辑 UI 一等公民（173 端点） | Mosaic 的记忆单元是房间/线程级结论、承诺与任务而非人物画像；source_event_ids 强制 provenance 与其来源管理同构 |
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

## 7.4 记忆双平面与责任边界裁定（v0.2，2026-09-03 负责人裁定）

**背景**：负责人裁定"session 和 memory 是两码事"，并指定两个参考实现先行调研——跨会话记忆看 **Hermes Agent**（Nous Research），群聊记忆看 **MaiBot**（A_Memorix，docs.mai-mai.org/manual/features/memory-system）。调研以两家官方文档为据，要点：

- **Hermes（跨会话记忆）**：刻意分两个平面——**恒常平面** = 两个硬上限策展文件（`MEMORY.md` 2,200 字符 / `USER.md` 1,375 字符），会话启动时作冻结快照注入 system prompt（保 prefix cache）、注入头带容量水位（`[67% — 1,474/2,200 chars]`）；**不做自动压缩**——超限写入报错并附当前条目清单，由 agent 当轮自行合并删除后重试（容量压力倒逼策展）；写入经 memory tool 自助（add/replace/remove，子串匹配定位），每个 turn 结束跑后台自我改进审查，`write_approval` 开启时非交互场景的写入进 pending 队列人工批准；写入前扫注入/外泄模式与不可见 Unicode、完全重复自动拒绝。**按需平面** = Session Search：全部历史会话入 SQLite FTS5，查询 ~20ms、返回消息原文、**无 LLM 摘要、零模型成本**（并为中文专门做 fts5_cjk 扩展）。外部记忆 provider（Honcho/Mem0 等 8 家）只并行增强、**永不替代**内置存储。
- **MaiBot（群聊记忆）**：多模块协作（长期/短期/按消息窗口滚动摘要/Episode 片段/知识图谱/人物画像）；画像 = **证据驱动 + 手动覆盖优先**的双层模型（纠正证据触发画像自动刷新）；每条记忆带**来源管理**字段（来自哪个聊天流/导入任务），支撑按来源批量重建、删除前影响范围预览、回收站恢复；生命周期显式操作集（强化/弱化/冻结/保护/永久记住/遗忘）；173 个 HTTP 端点把查看/纠错/编辑/删除全部暴露为 UI 能力。

**五点裁定**（负责人 2026-09-03 确认，第 4 点经其纠正改口）：

1. **责任边界**：Harness CLI 会话 = Agent 私有工作记忆（OQ-16 边界内，不透明/不可审计/不可编辑——可接受）；Mosaic Memory = **房间共享记忆**（有 provenance、可审计、可编辑、人类可纠错）。两者所有者与审计要求不同，互不替代；Mosaic 只建后者。与 Hermes "外部 provider 并行不替代内置"同构。
2. **按需平面 FTS5 起步**（§3.1.8 随之修订）：事件日志本就在 SQLite 全量，FTS5 全文检索 + 结构化过滤即覆盖"我们之前是不是聊过 X"类召回；查询走权威历史查询通道并入 Context Receipt（§3.1.3/§3.1.7 既定）。pgvector/embedding 无限期推迟，以 FTS5 召回不足的狗粮实录为重启条件。前置验证项：modernc.org/sqlite 的 FTS5 编译可用性 + CJK tokenizer 方案。
3. **恒常平面正规化**：现有"已接受胶囊注入第八层"（v1.36，最新 5 条）升级为容量纪律——字符容量上限（替代条数上限）、容量水位在调试面可见、超限倒逼合并（不静默截断丢旧）。§3.1.1 七层配额（记忆层 15%）不变，容量纪律是该配额的执行面。
4. **承诺/待办追踪是 tasklist，不是记忆系统**（负责人纠正）：形态取常见 Harness 的 tasklist/todolist，但在多 Agent 群聊形态上**必须加责任人字段**——每项任务归属到具体承诺者，否则"谁该交付"无从判定。确定性派生（v1.49 起为显式申报协议——mosaic-todo 围栏块，正文自然语言永不派生；v1.46 宣言模式匹配因误报严重废弃）+ 人工门控；清单入评估语境供主动开口消费（RFC-0012 OQ-A 触发源的承载物）。与记忆层强关联（provenance 纪律同源）但独立成物——设计落 RFC-0012 OQ-A，不属本 RFC 范围。
5. **编辑面是 M3-3 验收项而非可选**：查看来源 / 人工编辑留 edit_history / 纠错生效于下次组装的最小闭环随 M3-3 交付（两家参考一致：无编辑面的记忆系统静默劣化）。

**对既有条款的影响**：§3.1.8 已按裁定 2 修订；X-05 与 §3.2 检索行加个人版注记；§6 未解决问题 5（相似度阈值初值）随向量推迟挂起；§3.1.4 四层 Memory 与 Memory Item Schema 不变（恒常平面的分期接入面）；§5 现有技术增补两个参考实现对照行。
