# ADR-0046：0.7.9 Slot 换 Agent 交接——职责知识与在办事项转移，不复制私有记忆

- 状态：Accepted（2026-11-12，随 0.7.9 实施；v0.2 一审 / v0.3 复审 / v0.4 三审 / v0.5 四审修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§8.1 三种迁移之"**同 Slot 换 Agent**"：必须携带或重新绑定 = **职责实例知识、Mission / WorkItem 状态、产物与未决动作、承接回执**；不保证 = **上任 Agent 全部私有记忆；不得复制整个人格冒充原负责人**；§3.1"实例跨任职保留"；§8.3"移动必须停接新任务、排空或暂停在途工作、核验未知动作"）；[ADR-0020](ADR-0020-slotinstance-knowledge-namespace.md)（knowledge_namespace——职责知识以**实例**为 scope，天然跨任职）；[ADR-0021](ADR-0021-binding-lifecycle.md)（Binding 状态机 + 单 active 部分唯一索引 + handoff_note）；[ADR-0022](ADR-0022-readiness-admission.md)（准入两时点复核——激活时点授权再核）；[ADR-0029](ADR-0029-mission-pause-resume-cancel.md)（paused——交接安全态）；[ADR-0035](ADR-0035-mission-handover.md)（单任务两段式交接 + 归属锚 + 承接回执事件形制——本件复用其事件语汇，不动其锚语义）；[ADR-0037](ADR-0037-domained-memory-items.md)（NSSlot 命名空间）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.9 收官件：Slot 换 Agent 交接）。

## 背景与问题

0.7 线已交付九件，剩最后一件收口"换将"。现状拼图齐而缺口清晰：

- **职责知识已在位**：0.5.3 起实例携 `knowledge_namespace`，0.7.0 起记忆五命名空间中 `slot` 域以**实例 ID** 为 scope——职责知识生来就挂在实例上而非 Agent 上，换 Agent 天然不丢。**缺的是交接时点的确认与清单**（新任知道自己继承了什么、用户可审计转移了什么）。
- **在办事项（Mission）挂在 Agent 名下**：`owner_agent_id` 是责任归属事实。换 Agent 后若只走 Binding 释放/新建，在办任务仍属上任——0.6.13 的执行核对（CreateRun 事务内核对任职授权与任务归属）会拒绝执行，任务**悬空**：旧任无绑定不可执行、新任非 owner 不可执行。
- **0.6.12 有单任务交接，但不是换将**：offer → accept 两段式逐任务操作，承接人须逐个"点头"。换将场景下新任由**用户任命**（Binding 装机即承接），逐任务 offer 既冗余又语义错位（N 次手工操作 + 承接确认主体错位）；且 RFC §8.1 明确换将必须携带"Mission / WorkItem 状态 + 承接回执"——需要的是**一次编排、逐任务留痕**。
- **不复制私有记忆**是边界承诺（RFC §8.1"不得复制整个人格"）：交接必须**显式排除** agent 命名空间记忆并如实报告排除面，而非"恰好没做"。

## 决策

### 1. 交接语义：一次编排、逐任务承接回执、知识不搬动

`slot.HandoverService.Handover(ctx, slotInstanceID, newAgentID, notes)` 单入口编排，返回**交接回执**（HandoverReceipt）。三类对象三种处置：

| 对象 | 处置 | 机制 |
|---|---|---|
| **职责知识**（slot 命名空间，scope=实例） | **不搬动**——以实例为 scope 天然跨任职（ADR-0020 设计本意）；交接 = 确认 + 清单（按 status 计数入回执） | 既有 memory.List 只读 |
| **在办事项**（Mission：owner=上任 ∧ slot_instance_id=本实例 ∧ 非终态） | **转移**：owner 翻转至新任 + ready/running/waiting → paused（reason=slot_handover）+ 逐任务 `handover_accepted` 事件（cause=slot_handover——承接回执）；reviewing/canceling **不转移**（见下） | mission 域新单事务方法（§3） |
| **上任私有记忆**（agent 命名空间，scope=上任） | **显式排除、如实报告**（计数不入正文——"不得复制整个人格"的可审计呈现） | 既有 memory.List 只读计数 |

转移集**复用域单源** `HandoverOfferable`（draft/ready/running/waiting/paused）——不另立口径：reviewing（验收基线冻结，ADR-0032 先例）与 canceling（停止瞬态——先收尾）不在转移集，**不阻断交接**、回执 `missions_not_transferred[]` 如实列出（含原因——它们是收束中的流转，owner 不变，裁定/收尾仍可经 owner 令牌记录）。转移态口径（RFC §8.3"暂停在途工作"）：`ready/running/waiting → paused`（新任核验检查点后续办——0.7.4 跨 Harness 续办机制是入口）；`draft/paused` 保持原状（draft 契约未完备不催熟）。终态（accepted/canceled）不在办、不收集。

### 2. 编排步骤与单写者不变量（v0.2 一审修订）

**服务级互斥（审校 #2/P1）**：整个编排持 HandoverService 互斥锁——并发换将串行化，不存在"两请求同读旧任 A 各转不同目标"的批次分裂（单 active 索引只护绑定激活，护不住任务批次）。个人实例单进程（SQLite 单写者同界），跨实例亦串行（频度极低无争用之虞）。

```
① 前置核对（零变更）      实例存在 → 绑定现势定位（active / draining / released
                          曾任 active）→ 新任 active → readiness 预核（latest 版）
                          → 预收集全属校验（在途 0.6.12 交接锚 + 遗留转移——见 §3）
② 新任就位               复用/新建 ready 绑定（不占 active 位——单 active 索引不涉）
③ 排空边界（审校 #4/P1） active → draining 单段——**停接新任务**（0.6.13 建单门
                          在写事务内要求 active 任职，此后拒新单）；权威收集后置到
                          边界之后，"边收边进"结构性消除（RFC §8.3"停接新任务"）
④ 在办转移               全属收集（§3）→ 逐任务 ReassignMission（单事务；app 闭包
                          信号先行停止在途 Run——见 §3a）
⑤ 旧任释放               ReleaseBinding（draining → released；handoff_note = 交接备注）
⑥ 新任激活 + 承接回执     **同一事务一次提交**（审校 #6/P2：TransitionBindingWithEvent
                          ——激活成功则回执必在，回执落流失败激活一并未遂）
⑦ 报告                   HandoverReceipt（转移清单/知识清单/排除面/绑定变迁）
```

**单写者不变量**（全程至多一人可对实例职责写入）：

- ③ 边界先于 ④ 收集：停接单后收集清单即封闭快照（边界提交后建单门拒——sqlite IT 钉住）。
- ④ 先于 ⑤：任务 owner 翻转即剥夺上任执行权（0.6.13 CreateRun 核对 owner + 在任绑定）；在途 Run 停止信号先于翻转发出（§3a——不存在"翻转成功而停止信号永不发出"）。
- ⑥ 最后且原子：新任在激活前对任务无执行权；激活与承接回执同一事务（不存在"激活成功回执缺失"态——审校 #6 反例结构性消除）。
- ① 把所有可预期拒绝前置（readiness 不满足/在途单任务交接/遗留转移/新任非 active——**零变更失败**）；②–⑥ 的中断窗口见 §5 幂等收敛。

**旧任识别（审校 #5/P2）**：from 不以创建序/最新 released 猜测——按**曾任 active 证据**（binding_events 含 to=active 转移）取最近释放者（ready→released 直释候选从未在任，被结构性排除）；Resume 分支优先取最近 draining（上轮边界中断的旧任）。

**绕行封闭与自愈（复审 P1 + 三审 P1）**：普通激活不得绕过未完成交接批次——draining 守卫在**激活写事务内**（`ActivateBindingTx`：ready 锚 + 同实例无 draining + 单 active 索引 + 可选同事务回执事件，一次提交）：服务层预查与写入之间的并发绕行窗口结构性消除（三审反例：预查通过后并发置 draining 仍激活——交错注入 UT 钉住）；守卫使"一实例至多一个 draining"成为不变量。守卫上线前的绕行数据态（新任已在任 + 旧任仍 draining、在办遗留旧任）由重放分支**回退自愈**：from 取 draining 旧任，补转其名下在办并释放之。UI 在 draining/ready 态以"继续换将交接"（原目标续跑）替代普通激活入口。

### 3. 在办转移：mission 域单事务方法

`mission.Store.ReassignMission(ctx, missionID, fromOwner, toOwner string, event) (Mission, bool, error)`——事务内核对四项，全过才写：

1. **归属锚**：现行 owner == fromOwner（0.6.12 审校 #2 同款——并发 0.6.12 accept / 重复交接按锚区分，不冒领）；
2. **状态族 ∈ HandoverOfferable**（域单源）且 **无在途交接**（pending_owner_agent_id 空——0.6.12 锚语义不被换将静默吞掉）；
3. **承接人 active**（事务内直查 agents 表——ADR-0035 审校 #3a 同款；retired/suspended 不接承诺）；
4. 写入：owner = toOwner（**不递增 contract_version**——归属翻转非契约修订，0.6.12 同口径）+ ready/running/waiting → paused（status_changed reason=slot_handover）+ `handover_accepted` 事件（payload 注入 from_owner/to_owner/contract_version/status_at_accept + **cause=slot_handover, slot_instance_id**——复用 0.6.12 承接回执事件形制，枚举零新增）。

`ok=false` = 锚不符/终态/在途/承接人非 active（服务层重读分类：已移动 → 计已转；在途 → 编排级冲突）。

**全属收集（审校 #3/P1）**：配套查询 `MissionsOfSlot(ctx, slotInstanceID)`——挂本实例的**全部非终态**任务不论归属（只收上任名下会漏"前轮未完成交接已转出"的遗留）。分类：owner=from → 转移；owner=目标 → 既成事实（非重跑批列入回执——批完整性；纯重放不入清单）；**可转移且归属第三方 → 整体 409"遗留转移"**（改选接任者会使遗留任务悬空——拒绝并指明以原目标重跑收敛的路径；reviewing/canceling 不转移、回执列明归属保留）。

**在途 0.6.12 交接预检**（①内预收集，零变更失败）：任一任务（不论归属）pending_owner_agent_id 非空 → **整体 409**（列任务与承接人，"先承接/婉拒/撤回"）——单任务交接是用户可见承诺，换将不静默代撤；修完重跑即可（ADR 登记：自动撤回是备选，被否——静默吞承诺违背 0.6.12 锚设计）。

### 3b. 补转轮回执补偿：逐次转移配对对账（复审 P2 + 三审 P2 + 四审 P2，app 装配层）

补转轮（重放漂移修复）无绑定态变化可作原子锚——其回执 append 可能失败，且重试因任务已属新任而零转移，append 不再重发（首轮激活的原子面管不到这里）。补偿机制（四审后终态）——对账按 **(任务, 当时转出人) 配对计数**，双方均为持久事实：
- **转移侧**：`HandoverTransferFactsOfSlot`——挂本实例、经换将转移至新任的**逐次转移事实**（每次发生一条：MissionID + from_owner；mission_events 事件扫描，**不依赖任务当前状态/归属**——终态或再改派后的历史转移仍在列）；
- **回执侧**：实例**全域**各绑定的 slot_handover 回执（to_agent == 新任者）中 (mission × 回执轮 from_agent) 配对计次（回执 payload 携 missions 清单 + 本轮 from_agent，逐次回执逐次计；新任历史任职的回执同属覆盖）。

**配对缺额 → 按配对补记**：from = 配对的**当时转出人**（四审 P2——跨任期补写不得把历史转移记到当前轮双方名下：A→B 第二次转移的回执缺失，B→C→B 时期补写记 from=A 而非当前轮 C）；多轮丢失一次补齐（missions 按缺额重复列）。漂移回流的再转移是**新事实**；0.6.12 点对点承接不含 cause，不误记。补偿自身失败上抛、重试再补——收敛且幂等（对账平后零动作）。

### 3a. 在途 Run 停止：信号先行（审校 #1/P1，app 装配层）

任务转移只翻 owner/暂停**不停止已启动的执行**（CreateRun 归属校验只拦新启动）——app 装配的 `ReassignMission` 闭包（`handoverPorts` 单源，生产/UT 共用）先核未终态 Run（`HasUnfinished`）→ 发停止信号（`SignalStopByMission`，cause=slot_handover——ADR-0029 PauseMission 同款面）→ 再翻转。**信号先于翻转的次序**保证重跑无空洞：未遂者重复发信号、已遂者首轮已收到；信号/查询失败即中止（不翻转——错误上抛重跑收敛）。

### 4. 职责知识：不搬动、只确认；私有记忆：显式排除

- **知识清单**：`slot` 命名空间 scope=实例的记忆按 status 计数（active/proposed/superseded/deleted——墓碑也在位事实）入回执；正文不进回执（回执是审计面非内容面，读面走既有记忆端点）。
- **排除面**：回执 `excluded` 固定注记 + 上任 agent 命名空间计数——"上任私有记忆 N 条不随交接转移（RFC §8.1：不得复制整个人格）"。**不做**"职责相关但误存 agent 域"的迁移建议（那是策展判断，非交接事实——边界登记）。

### 5. 幂等与中断恢复：无 attempt 持久化，重跑按现势收敛

对照 0.7.7 激活的 attempt.json：激活是**内容写入**（建任务/写记忆，中断需知"写到哪"）；交接每步都是**可核对的状态推进**，重跑按现势核对自然收敛，无需持久化进行中映射：

| 重入时现势 | 收敛动作 |
|---|---|
| 在任 = 新任（已交接完成） | 幂等返回当前态回执（纯重放转移清单为空；若存在漂移任务——如 0.6.12 承接回流入上任名下——补转并独立记该轮回执事件） |
| 新任 ready + 旧任 draining（边界/转移中断） | 续跑（旧任即 draining 来源；任务按归属锚续转；释放补终段） |
| 新任 ready + 旧任 released（释放后激活失败） | 续跑（from 按曾任 active 证据；直接激活） |
| 新任 blocked | RetryPrepare 一次，仍 blocked → 409（报 missing——修授权后重跑） |
| 改选新接任者（存在遗留转移） | **409 拒**——以原目标重跑收敛或先处置遗留归属（审校 #3） |

④–⑥ 中断遗留态均为安全态：旧任 draining（停接单，无人可对新单）+ 部分任务已转（paused 挂新任名下）；或旧任已释+新任未激活（无人任职）——重跑收敛，无双写窗口。

### 6. 契约与域结构（零 migration）

- **端点**：`POST /v1/slots/instances/{slot_instance_id}/handover`，体 `{new_agent_id, notes?}`（notes ≤4096 同 handoff_note 上限）→ 200 `SlotHandoverReceipt`。错误族：404 实例未知；409（无在任且无可恢复态 / 新任=在任 / 新任非 active / readiness 不满足 / 在途单任务交接 / 新任绑定 blocked / 激活复核失败）；422 参数。
- **回执 schema**：`SlotHandoverReceipt`（slot 实例 id/title/knowledge_namespace、from/to agent、occurred_at、missions_transferred[]、missions_not_transferred[]（reviewing/canceling——含原因）、knowledge_inventory{}、excluded_private_count、excluded[]、released_binding_id、activated_binding_id、notes）。
- **BindingEvent.eventType 枚举 += `slot_handover`**（新任流上的承接回执事件——payload {from_agent, to_agent, transferred_count, slot_instance_id, notes}；事件类型是 additive 枚举扩展，oapi 再生成）。Mission 事件**零枚举变更**（复用 handover_accepted，payload 描述扩 cause/slot_instance_id）。
- **域结构**：slot 域 `handover.go`（HandoverService + HandoverPorts——Ports 闭包模式同 bundle.ActivationPorts，方法级注入避开构造环）；slot.Store += `AppendBindingEvent`；mission.Store += `MissionsOfOwnerSlot` / `ReassignMission`（memstore 同步实现——agent active 核对在 sqlite 事务内，memstore 以端口/查表等价实现供 UT）；httpapi Deps += SlotHandover；app 装配闭包连 missionSvc/memSvc。零新表零新列（binding_events/mission_events 的 event_type 均为自由 TEXT）。

### 7. 边界与非目标

- **不动 0.6.12**：单任务两段式交接保持原样（跨 Agent 点对点移交场景仍在）；换将编排是并行的第三通道（用户任命制），两者以归属锚/在途检查互斥。
- **不自动 resume**：转移后任务一律 paused/原状——新任须核验检查点（0.7.4 续办）再手动恢复，"停接新任务、核验未知动作"是 RFC §8.3 的明文要求，不做"无缝热切换"。
- **排空门（DrainChecker）不装配**：换将的"排空"语义就是**转移**（在办随交接走）；Drain 门面向"职责撤销/退役"场景，语义不同，仍留 nil。
- **产物（artifacts）与授权不转移**：产物属任务域（随 Mission 走）；授权档案（grants）按 Agent 各自持有——新任装机时自带（readiness 门核对的就是它）。
- **跨实例换将不在本件**：本件是同实例内换 Agent；跨实例走 Bundle 导出/导入/激活链（0.7.5–0.7.7 已交付）。

## 测试策略（TDD 先行）

红段（先写、先失败）：

- **slot UT**（handover_test.go，fake Ports + 真 Service/memstore）：七步全链；幂等重入四态（§5 表）；零变更失败族（无在任/同 Agent/新任非 active/readiness 拒/在途 offer 409 且零变更断言）；部分转移后重试收敛；锚错配计已转；回执字段（清单/计数/排除注记）；slot_handover 事件落流。
- **mission UT**：Reassign 状态族矩阵（ready/running/waiting→paused；draft/paused/reviewing/canceling 保持；终态拒）；归属锚/在途拒；MissionsOfOwnerSlot 过滤口径。
- **sqlite IT**：ReassignMission 事务原子性 + 承接人 active 内核对（SQL 直改 agents 状态打交错）；AppendBindingEvent 落流；MissionsOfOwnerSlot 查询。
- **httpapi IT**：端点全链（真装配：建模板/实例/双 Agent/绑定/任务/记忆 → 交接 → 断言任务 owner+paused、绑定变迁、事件、回执）+ 错误族 401/404/409/422。

绿段按本 ADR 实现；随后 openapi 契约 + 双链生成物、前端（设置页实例行"换 Agent 交接"操作 + 回执卡）、全量验证（vet 双标签/UT/-race/SPA）与真机冒烟。

## 修订记录

| 版本 | 日期 | 摘要 |
|---|---|---|
| v0.5 | 2026-11-14 | 四审修订（1 P2）：跨任期补写把历史回执记到错误交接双方（补偿以当前轮 from 归档）——对账改 (任务, 当时转出人) **配对计数**：转移侧逐次事实含原始 from_owner（SlotHandoverTransfers 返回 SlotTransferFact），回执侧 (mission × 回执轮 from_agent) 计次；缺额按配对补记（from=当时转出人）。跨任期反例 UT（两次 A→B → 取消 → B→C→B：补写 from=A 非 C、恰一条、幂等）钉住 |
| v0.4 | 2026-11-12 | 三审修订（1 P1 + 1 P2）：P1 draining 检查与激活写入不原子——守卫入写事务（ActivateBindingTx：ready 锚/无 draining/单 active/可选回执事件同事务；删服务层预查，交错注入 UT 钉住）；P2 终态任务补偿失效——对账改持久事件源（SlotHandoverTransfers 枚举不依赖在办/归属 + 实例全域回执覆盖），终态反例钉住。sqlite IT 两面（ActivateBindingTx 守卫 / 终态枚举） |
| v0.3 | 2026-11-12 | 复审修订（1 P1 + 1 P2）：P1 普通激活绕过未完成交接批次——ActivateBinding/ActivateBindingWithReceipt 增 draining 守卫（拒绕行 + "一实例至多一 draining"不变量）+ 重放分支回退 draining 自愈（守卫前数据态）+ UI 续跑入口；P2 补转轮回执永久丢失——回执携 missions 清单 + HandoverTransferCount 逐次转移计数对账补偿（mission_events 持久事实为单源，0.6.12 不误记）。反例钉住（绕行拒/自愈/补偿收敛幂等） |
| v0.2 | 2026-11-12 | 一审修订（4 P1 + 2 P2）：#1 在途 Run 停止——app 闭包信号先行（§3a）；#2 并发换将服务级互斥串行化（§2）；#3 全属收集 + 遗留转移改目标 409（§3）；#4 排空边界（active→draining 停接单）先于权威收集（§2/§3）；#5 旧任识别按曾任 active 证据（§2）；#6 激活与承接回执同一事务（TransitionBindingWithEvent，§2/§6）。反例各以 UT/IT 钉住（并发分裂/遗留悬空/边界后新单/直释候选误认/回执缺失态） |
| v0.1 | 2026-11-12 | 首版（随 0.7.9 实施） |
