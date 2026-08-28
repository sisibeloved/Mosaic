# 特性设计说明书（RFC 提案）：RFC-0006 Epistemic / Structure Projection 契约

**状态 (Status):** Draft

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-08-25

**更新日期 (Updated):** 2026-08-25

**相关 Issue/PR:** #TBD（RFC 入库时创建 tracking issue）

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| RFC 编号 | 0006 |
| 系列位置 | RFC 序列第 6 篇；Epistemic Plane 的全部契约（LE-18） |
| 上游输入 | [架构设计说明书](../2026-08-13-mosaic-architecture-design.md) v0.7 §6.9.2、§6.9.3、§6.9.4、§6.10、§8.1.3、§11.3；[RFC-0001](2026-08-25-rfc-0001-room-protocol.md)（envelope/可见性）；[RFC-0004](2026-08-25-rfc-0004-thread-graph.md)（relations 语义） |
| 吸收开放问题 | OQ-11（cluster 基图时间窗口/语义分段与 feedback 影响）、OQ-13（Claim 投影在线或离线）——本 RFC 提出裁决建议 |
| 下游 | RFC-0003（结构特征消费与漂移重聚焦）、RFC-0005（收束快照供给）、RFC-0007（Context 谱系回溯）、RFC-0011（指标定义）、`internal/epistemic`（LE-18）实现 |

## 0.1 修订记录

| 版本 | 日期 | 修订人 | 说明 |
|---|---|---|---|
| v0.1 | 2026-08-25 | Mosaic 项目组 / ZCode | 初稿：版本与重建纪律、聚类基图与结构特征、provisional/productive bridge、Claim/Evidence 认知投影、stance 台账、主体非干扰契约、feedback overlay、病理签名与收敛信号、降级矩阵、OQ-11/13 裁决建议 |

# 1. 概述

## 1.1 简介

投影是"从 Room Event 派生、可重建、带版本"的读模型，回答两类问题：结构上"谁在回应谁、观点如何汇聚"（结构投影：cluster/bridge/交替链）；认知上"哪些主张发生了什么变化"（认知投影：claim/evidence/stance/证据债务）。本 RFC 定义两者的对象模型、算法版本纪律、主体非干扰契约、人工纠正 overlay、病理签名与收敛信号的供给语义，以及各级降级路径。**推断不是事实**（原则 13）：投影不得回写 Room Event；**指标不得成为目的**（原则 14）：结构信号只作组合特征与诊断。

## 1.2 动机

架构 v0.7 §6.9–§6.10 给出了投影哲学与特征清单，但三件事未成契约：聚类基图的具体口径（OQ-11 未决）、Claim 投影在 v0.1 的在线/离线定位（OQ-13 未决）、主体非干扰契约的可测试形式（11.3 已列为适应度函数但无判定规范）。RFC-0003（结构特征消费）与 RFC-0005（收束快照）都在等待本 RFC 的供给接口定稿。

## 1.3 目标

### 1.3.1 目标

1. 定义版本三元组与重建纪律（旁路重建、不覆盖历史）；
2. 定义聚类基图口径与增量更新规则（OQ-11 裁决建议）；
3. 定义六类结构特征与 provisional/productive bridge、bridge_yield；
4. 定义认知投影对象（Claim/Assumption/Evidence/Open Question/Claim Relation）与提取语义（OQ-13 裁决建议）；
5. 定义 stance/commitment 台账与弱独立收敛标记；
6. 定义主体非干扰契约的可测试规范；
7. 定义 feedback overlay 与重放规则；
8. 定义病理签名与收敛信号四组的供给语义（消费方：0003/0005）；
9. 定义降级矩阵。

### 1.3.2 非目标

- 结构信号的消费策略与权重（RFC-0003）；
- 收束触发判定与 Capsule（RFC-0005）；
- 检索与上下文组装（RFC-0007）；
- 指标的在线奖励（永久禁止；执行面归 RFC-0011）；
- 提取所用自用模型的选择（OQ-03 后半，Model Gateway ADR）。

# 2. 用例分析

| 用例 | 功能点 | 关键指标 | 安全隐私与 DFX 约束 |
|---|---|---|---|
| E-01 聚类与 bridge 判定 | 相对冻结基图判定 long-range edge；历史 bridge 保留当时版本 | 10 万事件 Thread 增量投影 < 30s（9.6） | 跨 cluster 边不参与本版本 cluster 形成（防自指） |
| E-02 productive bridge 延迟确认 | 被引用/改 claim 状态/进 Capsule/形成有效 Evidence Request 的延迟归因 | bridge_yield 归因窗口（默认 3 纪元） | 只诊断不奖励 |
| E-03 Claim 提取 | 自用模型通道 + 确定性规则混合抽取；置信度与来源 | 单事件提取延迟不阻塞投影水位 | 提取失败降级可见；失败不阻塞讨论（6.10.1） |
| E-04 非干扰 fixture | 隐藏来源对任意主体响应零影响（计数/标签/水位/错误/timing） | CI 全绿（11.3） | 缓存键含主体与策略版本 |
| E-05 feedback 纠正重放 | 人工纠正 overlay；下一版本重建必须重放 | 纠正生效 ≤ 下次重建 | 不改原始事件 |
| E-06 算法升级旁路对比 | 新版本旁路重建 + 固定 fixture 对比后切换（8.1.3） | 全量重建 < 10min（9.6） | 不覆盖旧结果 |
| E-07 降级 | claim 投影不可用 → 显式关系 + 结构信号 | 切换自动、可见 | 核心讨论不阻塞（MVP 切片 9） |
| E-08 同构组弱独立收敛 | 共享模型/共享上下文/首发锚定的收敛标记弱独立 | 随收束元数据（联动 0005） | 不与异构独立收敛等价 |
| E-09 漂移签名 | 话题漂移签名 → 触发 0003 重聚焦窗口（Decision/Review） | 签名判定确定性 | 只触发探测不证明收敛 |
| E-10 病理签名 | 乒乓/礼貌螺旋/翻旧账/漂移/无人喊停五类 | 结构化告警 | 无人喊停只触发 quiescent/收束探测 |

# 3. 方案设计

## 3.1 总体方案

### 3.1.1 版本与重建纪律

- 每次投影产出以三元组标识：`projection_version + algorithm_version + event_watermark`；
- 算法升级走**旁路重建**：新版本并行构建，经固定 Event fixture 对比与人工样本抽检后切换；历史结果保留当时版本，不原地覆盖（架构 §3.3.1/§8.3.2）；
- 一致性门禁：Projection 可从空库对固定 fixture 重建为一致结果（11.3）；`projection_offsets` 以 `(projection_name, room_id, last_seq)` 幂等推进。

### 3.1.2 聚类基图（OQ-11 裁决建议）

- 输入三要素：**直接回复骨架**（reply_to 树）+ **时间窗口** + **语义主题**（topic_tags + embedding 质心）；
- **裁决建议**：时间窗口默认"滑动 48 小时或 200 事件，取先到"；语义分段降级路径 = topic_tags 聚合 + embedding 质心，不依赖在线学习聚类；
- 分层语义（架构 §6.9.2）：显式/推断的跨 cluster 关系**不参与**本版本 cluster 的形成；关系判定相对"前一投影水位或冻结的当前基图"进行；新事件可改变下一版本 cluster，但历史 bridge 保留当时 projection version；fan-in/fan-out、中心性、交替链按 cluster 大小、时间衰减（半衰期 20 轮）与活跃度归一。

### 3.1.3 结构特征定义

| 特征 | 定义 | 主要消费方 |
|---|---|---|
| 高 fan-in | 指向某 cluster/事件的长程边入度（归一） | 收束覆盖校验（0005）、谱系回溯（0007） |
| 高 fan-out | 发散源出度 | 收敛检测、分支建议 |
| A↔B 交替链 | 双边连续往返且 claim/evidence 状态无变化 → 乒乓签名 | Deep Dive 指纹、dyad 检测（0003） |
| provisional bridge | 相对冻结基图连接此前独立论点群的长程边 | 探索提示、novelty 组合特征（0003） |
| productive bridge | 后续被引用、改变 claim 状态、进入 Capsule 或形成有效 Evidence Request 的 provisional bridge（归因窗口默认 3 纪元） | bridge_yield 分子 |
| same-cluster revisit | 指向已讨论 cluster 的长程边；无状态变化 → 结构化重复告警，有新 evidence/assumption → 有效 reopen | repetition_risk（0003）、翻旧账签名 |

`bridge_rate` 只作结构诊断；首选延迟指标 `bridge_yield = productive_bridges / provisional_bridges`。

### 3.1.4 认知投影（OQ-13 裁决建议）

| 对象 | 关键字段 |
|---|---|
| Claim | claim_id、statement、status（open/strengthened/weakened/superseded）、置信度、source_event_ids、priority |
| Assumption | 归属 claim、statement、被挑战记录 |
| Evidence | 引用类型（artifact/tool/traceable utterance）、引用 ID、支持/反对方向 |
| Open Question | 尚不能回答的问题、关联 claim |
| Claim Relation | kind ∈ {supports, challenges, refines, supersedes, depends_on} |

- 提取器 = Mosaic 自用模型通道（Model Gateway，embedding/utility）+ 确定性规则（显式 relations 直通、无模型参与）；
- 每对象带 `projection_version / source_event_ids / 提取器版本 / 置信度 / visibility`；混合来源对象 visibility = 全部来源权限交集；
- **裁决建议（OQ-13）**：v0.1 Claim/Evidence 投影默认 **feature-flag、离线评测 + Context 谱系只读**；不进入 Closure 在线判定路径（收束快照在 claim 投影不可用时退化为结构信号 + 显式关系）；Claim 投影失败不得阻塞核心讨论（MVP 切片 9）。

### 3.1.5 Stance / Commitment 台账

- 从公开发言派生：当前立场、目标 Claim、依据事件、置信度、修订历史（`stance.revised` 为投影变化及解释，非事件事实）；
- Participant 可因新 evidence 公开改变立场；Floor 不得写入或修改立场（架构 §6.10.2）；
- 弱独立收敛标记：同构组（同 provider+model，0005 quorum 折算同源）、共享上下文（simultaneous 冻结水位）、首发锚定（sequential 首位）三种来源的一致标为 weak，不与异构独立生成后的一致等价。

### 3.1.6 主体非干扰契约（可测试规范）

对任一主体 P，历史与投影 API 返回的每个字段，必须**等价于只使用 P 有权读取的 source_event_ids 重新构建的结果**：

1. 混合来源对象 visibility 不得宽于来源权限交集；
2. 隐藏来源的 **ID、数量、cluster 大小、标签、质心、水位、错误类型、timing 差异**均不得泄露；
3. 投影缓存键必须包含 tenant、participant/role、visibility policy version 与 projection version；
4. 错误响应与 correction overlay 同样按主体过滤；
5. CI 维护跨可见性 fixture：对每个 API，"存在隐藏来源"与"不存在隐藏来源"两个世界对 P 的响应完全一致（11.3）。

### 3.1.7 Feedback Overlay

- 人类纠正经 `projection.feedback.recorded` 事件：`{对象类型, 对象 id, 纠正动作（删除/改类/合并/改可见性）, 理由, actor}`；
- 事件不修改原始 Room Event；下一次重建**必须重放**全部未过期 feedback；
- UI 区分显式关系与推断关系（9.8），纠正入口面向有权限人类。

### 3.1.8 病理签名与收敛信号（供给语义）

| 病理签名 | 判定（确定性） | 消费方 |
|---|---|---|
| 乒乓辩论 | A↔B 交替链不衰减且 claim/evidence 状态无变化 | 0003（dyad）、告警 |
| 礼貌螺旋 | Intent 类型塌缩为 support/synthesize + 对总结的再总结 | 收敛检测 |
| 翻旧账循环 | 同 claim/cluster 重复长程边且无新 evidence/assumption | 0003（repetition_risk） |
| 话题漂移 | 持续新 cluster、新颖性不衰减、目标覆盖不增 | 0003 重聚焦窗口（Decision/Review） |
| 无人喊停 | 纯 Agent 自动续聊且人类刺激停止 | 仅触发 quiescent/收束探测，不证明收敛 |

收敛信号四组（Intent 侧/内容认知侧/结构侧/人类侧，架构 §6.9.4）由本 RFC 计算、Policy 组合消费（0005 触发）；语义相似只代表重复不代表共识。

### 3.1.9 降级矩阵

| 不可用组件 | 降级后 | 消费方表现 |
|---|---|---|
| Claim 投影 | 显式 relations + 结构信号 | 0005 快照退化；0007 谱系回溯仅沿显式关系 |
| 结构投影 | 0003 已定（embedding+tags+最近窗口） | novelty/repetition_risk 降级；漂移重聚焦关闭 |
| 自用模型（embedding） | 关键词 + 结构化过滤 | 检索退化（架构 §7.2.3） |
| 全部 | 显式关系 only | 核心讨论完整可用 |

## 3.2 技术选型

| 决策点 | 选择 | 放弃方案与原因 |
|---|---|---|
| 聚类算法 | 确定性增量（reply 骨架优先 + 质心吸附） | 在线 ML 聚类：参数敏感、不可重建风险 |
| 提取器 | 小模型 + 确定性规则混合 | 大模型全量抽取：成本与延迟；纯规则：召回不足 |
| 投影存储 | PG 版本化投影表 | 外部图/向量库：无规模证据（ADR-0003 同口径） |
| 立场派生 | 只读派生 + 显式修订解释 | 事件化写回：违背"推断不是事实" |

## 3.3 功能与性能设计

- **内部端口原型**：`ProjectionPort: QueryStructure(subject, thread, watermark) / QueryClaims(subject, ...) / BuildClosureSnapshot(thread, watermark)（供 0005）/ DriftSignature(thread) / RecordFeedback(cmd) / Rebuild(room, algorithm_version)`；
- **性能目标**（对齐 9.6）：10 万事件 Thread 增量投影 P95 < 30s；全量重建 < 10min；非干扰过滤查询增加延迟 < 20%；
- **影响范围**：`internal/epistemic`（LE-18）、投影表与 `projection_offsets`、CI 非干扰 fixture 与重建一致性门禁。

## 3.4 安全隐私与 DFX 设计

- **非干扰**：3.1.6 全条款 fixture 化（含缓存键与错误路径）；
- **不可回写**：投影写路径物理隔离（无 Event Store 写凭据）；
- **Goodhart 防线**：结构指标仅诊断与组合特征；在线 Floor 奖励禁用清单由 RFC-0011 执行；
- **观测**：投影 lag、重建时长、feedback 待重放数、提取失败率入指标；
- **可靠性**：投影可丢弃可重建（原则 9）；单 Room 重建不影响他 Room。

## 3.5 编程与调用设计

### 3.5.1 编程模型基本设计

- **开发环境**：投影 fixture 仓（固定 Event 集 + 期望产出 + 非干扰双世界对照）；算法版本注册表；
- **开发约束**：新算法版本必须先旁路对比后切换；提取器版本必须登记；
- **可验收设计**：空库重建一致性、feedback 重放、降级切换三类门禁。

### 3.5.2 接口定义与设计

#### QueryStructure / QueryClaims（内部查询端口）

接口描述：按主体过滤的结构/认知特征查询（0003/0005/0007 的唯一供给面）。

接口原型：`ProjectionPort.Query*(subject, thread_id, watermark?)`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| subject | 输入 | object | 主体（participant/role/visibility policy version） | — |
| thread_id / watermark | 输入 | — | 定界与水位（缺省取最新） | — |
| features / objects | 输出 | array | 3.1.3/3.1.4 定义的对象（已过滤） | — |

- 异常处理：claim 投影未启用返回 `unsupported` 并附降级标记。
- 约束说明：响应满足 3.1.6 全部条款。
- 变更说明：首版。
- 调用参考代码：`const st = await projection.queryStructure(subject, threadId, wm)`。

#### BuildClosureSnapshot（供 RFC-0005）

接口描述：构建收束快照（claims、dissent、evidence debt、投影版本）。

接口原型：`ProjectionPort.BuildClosureSnapshot(thread_id, watermark) -> SnapshotRef`

- 异常处理：claim 不可用 → 结构信号退化快照（显式标记）。
- 约束说明：只读引用，不重算。
- 变更说明：首版。
- 调用参考代码：（0005 内部调用。）

#### RecordProjectionFeedback（命令）

接口描述：有权限人类纠正推断对象。

接口原型：`command_kind = "record_projection_feedback"`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| target | 输入 | object | 对象类型 + id | 投影注册类型 |
| action | 输入 | string | delete / retype / merge / revisibility | 枚举 |

- 异常处理：目标不存在或已过期拒绝。
- 约束说明：不改原始事件；入审计；下次重建重放。
- 变更说明：首版。
- 调用参考代码：（命令通道。）

### 3.5.3 编程手册设计

《投影与纠正指南》（面向用户）：推断与事实的 UI 区分、cluster/claim 含义、如何提交纠正、纠正何时生效。面向开发者的《投影算法注册手册》：算法版本登记、fixture 对比流程、切换审批。

# 4. 缺点和风险

| 风险 | 影响 | 应对 |
|---|---|---|
| 聚类对窗口/阈值敏感 | bridge 判定漂移、指标不可解释 | 参数随 algorithm_version 固化；旁路对比门禁 |
| 提取错误污染下游（0003/0005） | 选择与收束失真 | 置信度 + feedback + 降级路径 + 失败可见 |
| 非干扰实现遗漏（缓存/错误路径） | 隐私泄露 | 双世界 fixture 全 API 覆盖；缓存键规范 |
| 自用模型依赖 | 投影停摆 | 关键词/结构化降级（7.2.3） |
| 指标被 Goodhart 化 | 滥连 bridge、闲聊 | productive 延迟归因 + 组合指标 + 在线奖励禁用（0011） |
| 重建成本随事件增长 | 投影 lag | 增量优先；水位分桶；按 Room 隔离重建 |

# 5. 现有技术

| 参考 | 借鉴 | 差异 |
|---|---|---|
| Argument mining（论元挖掘） | claim/premise 自动抽取与置信度 | Mosaic 抽取结果为版本化投影且可人工纠正，不冒充事实 |
| RST/篇章解析 | 对话结构（回复骨架）先于语义分层的分层思想 | Mosaic 面向多 Agent 实时流而非静态文本 |
| 概率知识库（置信度传播） | evidence 聚合与 claim 状态迁移 | Mosaic 不做封闭世界推理，保留开放问题与证据债务 |
| 引文影响分析（延迟指标） | productive bridge 的延迟归因方法论 | bridge_yield 类比"引用才计入贡献" |

# 6. 未解决问题

1. **OQ-11 终裁**：时间窗口默认值（48h/200 事件）与语义分段降级路径确认；feedback 过期策略。
2. **OQ-13 终裁**：Claim 投影 v0.1 定位（建议离线+只读）与进入在线路径的切换条件。
3. 提取器自用模型选择与成本上限（OQ-03 后半 → Model Gateway ADR）。
4. "高优先级 Claim"的 priority 组合权重（联动 0005 合格性判定）。
5. bridge productive 归因窗口（默认 3 纪元）的校准。
6. stance 派生的最小信号集（显式声明 vs 推断阈值）。
7. 交替链"不衰减"的量化定义（衰减函数形式）。

# 7. 附录

## 7.1 参考资料

- 架构设计说明书 v0.7：§6.9.2–§6.9.4、§6.10、§8.1.3（LE-18 规格要求）、§11.3（重建与非干扰适应度）
- [RFC-0001](2026-08-25-rfc-0001-room-protocol.md)、[RFC-0003](2026-08-25-rfc-0003-attention-floor.md)、[RFC-0005](2026-08-25-rfc-0005-closure-capsule.md)、[RFC-0011](2026-08-25-rfc-0011-evaluation.md)

## 7.2 术语表

| 术语 | 英文 | 含义 |
|---|---|---|
| 聚类基图 | Cluster Base Graph | 由回复骨架+时间窗+语义主题形成的稳定 cluster（不含跨 cluster 边） |
| 冻结水位 | Frozen Watermark | 判定 long-range edge 时固定的基图版本 |
| 临时桥 | Provisional Bridge | 相对冻结基图连接独立论点群的长程边（候选） |
| 有效桥 | Productive Bridge | 后续产生认知价值的 provisional bridge（延迟归因） |
| 认知对象 | Epistemic Object | Claim/Assumption/Evidence/Open Question/Claim Relation |
| 主体非干扰契约 | Subject Non-interference | 响应等价于仅用调用者可见来源重建 |
| 纠正覆盖 | Correction Overlay | 人工纠正事件的重建重放机制 |
| 弱独立收敛 | Weak Independent Convergence | 同构/共享上下文/锚定来源的一致（不与独立一致等价） |

## 7.3 文档更新计划

- 评审期（Draft → Reviewing）：收敛未解决问题 1–2（OQ-11/13）；
- Accepted 后：fixture 仓与非干扰双世界门禁进 CI；`internal/epistemic` 实现排期；
- 后续：算法版本注册表随实现补充；0003/0005/0011 联动修订。
