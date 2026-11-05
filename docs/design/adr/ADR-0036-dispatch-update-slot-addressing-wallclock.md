# ADR-0036：0.6 域收口——网关续接、Slot 职责寻址与墙钟强制（0.6.13）

- 状态：Accepted（2026-11-03，随 0.6.13 实施；v0.2–v1.0 九轮审校修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§4.5——"任务型职责寻址使用 当前 scope → SlotInstance → active Binding → AgentID → 候选 Runtime。SlotSpec 只作版本化模板，同名模板在不同项目下可有多个实例；禁止跨项目猜选任职者。没有 active Binding 时返回待任职，不能自动激活一个 ready Binding"；§4.6——投递复核与"已提交后从 A 改为 B 走领域交接，不能只改路由缓存"；§10.3——`mission_update` 续接序列与 `route_stale`；§6.1——budget 五项"契约声明事实，运行时强制随 Scheduler 落"；AT-14/15/18）；[ADR-0024](ADR-0024-dispatch-first-slice.md)（网关首切片——create 路径）；[ADR-0028](ADR-0028-single-agent-execution.md)（Scheduler）；[ADR-0029](ADR-0029-mission-pause-resume-cancel.md)（停止面复用）；[ADR-0030](ADR-0030-run-crash-recovery.md)（迟到结果审计复用）；[ADR-0035](ADR-0035-mission-handover.md)（换人走交接——本件寻址不改在途归属）；[交付计划](../../plan/2026-08-25-delivery-plan.md) v2.69 盘点三项落差、负责人裁定"开 0.6.13"。

## 背景与问题

v2.69 阶段性验收盘点确认：0.6 出口的既定范围（AT-02/03/10/13）全部兑现，但网关分步（AT-14/15/18）存在三项**出口承诺与 0.6.1 首切片范围之间的落差**：

1. **网关续接缺失**：dispatch 只有 create 路径（房间来源 + 契约 + owner）；任务卡追加"再加 Windows 和绑核结果"（§10.3 `mission_update`）无受控入口——直接调 revise 命令面没有幂等键（断线重试双改）也没有网关复核留痕。
2. **Slot 职责寻址未接**：dispatch 目标面无 slot 形态——§4.5 的确定性职责链（scope→实例→active 任职→Agent）与"无任职待任、多实例歧义、陈旧拒绝"规则无处落地。
3. **墙钟无运行时强制**：budget 五项是契约声明并随执行输入传递，但 Scheduler 无 deadline——AT-18"执行超时"没有触发面；0.6.0 注释声明"运行时强制随 0.6.5 落"属欠账。

本件一次收口三项，0.6 域出口闭环。

## 决策

### 1. 网关续接：`dispatch.Update`（= mission_update，§10.3）

**命令面 `POST /v1/dispatch/update`**：`{mission_id, expected_version, idempotency_key, request_id?, contract（全量替换内容）, reason?}`。

- **幂等**（§10.3 序列）：复用 dispatch 既有 ClaimKey 机制（caller/scope/key 库级唯一）——同键同载荷重放原回执（不重改）、同键异载荷 `idempotency_conflict`；载荷摘要 = 命令全文 digest（同 create 路径）。
- **复核**（服务端先读）：Get mission → 状态/版本/归属；owner 非 active 拒绝（`agent_not_active`——续接指令不应发往已停身份）；版本冲突 / 归属锚 / 终态 / reviewing / 在途交接冻结窗等**透传** mission 域错误（400/409 语义不变）——网关不吞并领域拒绝。
- **执行**：`mission.Revise(missionID, expectedVersion, cur.Owner, cur.Owner, contract)`——**owner 不变**（任务范围变更不含换人：换人走 0.6.12 交接或修订命令面显式改；归属锚由网关以服务端读值填充，规避客户端陈旧锚）。成功 → FinalizeReceipt（`domain_command_id = mission:<id>:revise:v<新版本>`，target_refs 携新版本）。
- **不做 route 决策行**：§10.3 两段（解析→投递）防的是"寻址与投递之间目标漂移"；Update 无寻址面（mission_id 已定），竞态防护即 CAS + 归属锚——决策行与 `route_stale` 在此路径无对象。ADR 明确此简化。
- **失败不落回执**（与 create 路径同语义）：可修正后重试；领域事实（修订或其拒绝）以 mission 事件流为准。

### 2. Slot 职责寻址：dispatch.Resolve 目标面扩展（§4.5）

**ResolveRequest += 可选 `slot_instance_id` 或 `slot_spec_id`**（二选一；与 `owner_agent_id` 互斥——同时给出 400 `invalid_request`：显式 Agent 与职责引用并存须先明确语义，§4.5 上文"显式指定 B ≠ 已把责任交给 B"）。

寻址链（端口 `SlotLookups`，app 装配适配 slot 域——依赖方向同 Agents/Sources）：

| 输入 | 解析 | 结果 |
|---|---|---|
| `slot_instance_id` | 实例存在？→ 该实例 **active** Binding → AgentID | 不存在 → blocked `slot_instance_not_found`；无 active 任职 → blocked `slot_unassigned`（**不自动激活 ready Binding**——§4.5 明文）；有 → resolved，`selection_source=slot_binding` |
| `slot_spec_id` | scope 内该模板的实例集 | 0 → blocked `slot_no_instance`；>1 → blocked `slot_ambiguous`（跨项目/同名多实例**禁止猜选**，须显式指实例）；恰 1 → 同上行 |

- **决策可复核**：Decision 存 selection_source 与 target（agent）；`Dispatch`（复核投递）对 `slot_binding` 决策**重查** active Binding 的 Agent——换人后与决策时不符 → `route_stale`（重新解析；§4.6"不能只改路由缓存"）。owner 显式路径复核不变。
- **投递语义**：slot 寻址解析出的 Agent 作为 create_mission 的 owner（责任随任职成立）；Binding/执行代次固定发生在 Run 领取时（0.6.5 既有——本件不提前）。
- **不自动转移在途**（§4.5 尾句）：寻址只作用于**新交办**；已分配 Mission 换人走 0.6.12 交接——本件不改。

### 3. 墙钟强制：run deadline + 扫描停止（§6.1 / AT-18）

- **快照**：Run 进入 running 时按**执行开始时契约**的 `budget.max_wall_clock_seconds` 计算 `deadline = started_at + 墙钟`，落 runs 行（migration v30：`deadline TEXT NOT NULL DEFAULT ''`——空 = 无墙钟，历史行兼容）。修订不追改在途 Run 的 deadline（在途 Run 不静默换输入的同族原则）。
- **扫描**：ScanOnce 每轮追加超时扫描——`state = running AND deadline != '' AND deadline < now` → 逐 Mission 走**既有停止面** `SignalStopByMission(cause="wall_clock_exceeded")`：run → canceled（stop_requested/canceled 事件携 cause）+ 围栏核对 + 租约释放 + Mission 回 ready（`run_canceled`）——迟到结果按 0.6.7 只审计。停止面零新增机制。
- **再执行**：超时停止后 Mission 回 ready 可再执行（attempt 递增）；总次数由 **max_runs 派发上限**兜底（本件一并落：派发前查该 Mission 已创建 Run 数 ≥ `budget.max_runs` → 不派发，`budget_exhausted` 计入 Scheduler 诊断面——任务停留 ready 不被静默吞）。
- 并发/幂等：停止信号经 stops registry 幂等（0.6.6 既有）；同轮重复扫描无副作用。

## 非目标（0.6.13 明确不做）

- 语义候选寻址（AT-17——确定性路径稳定后独立启用）；跨设备待交接（AT-19）；Session/Checkpoint 恢复（AT-20 恢复子项——0.7）；WorkItem 委派（offer/accept——0.8）；Goal 域；`dispatch.Update` 的 route 决策行与 route_stale（无寻址对象，见 §1）；slot 寻址的版本钉选（spec 版本随实例创建存档，解析不绑版本）；墙钟外的 budget 细项（并发上限由单 Agent 执行面天然满足；重试/委派深度随 0.8）；性能基线（全局工作）。

## 验收要点（先红后绿）

- **UT·dispatch（续接）**：幂等重放原回执（不二次修订）/ 同键异载荷 409 / 版本冲突与冻结窗透传 / owner 非 active 拒 / owner 不变（载荷 owner 与原一致）/ 回执 domain_command_id 与新版本。
- **UT·dispatch（寻址）**：实例→active 任职→Agent；无实例 / 无任职（ready Binding 不自动激活）/ 模板多实例歧义 / 模板无实例各 blocked；显式 owner 与 slot 引用并存 400；**投递复核**：决策后任职换人 → `route_stale`。
- **UT·run（墙钟）**：running 且 deadline 过期 → 扫描触发停止（run canceled + cause 事件 + Mission 回 ready）；deadline 空 / 未到期不动；max_runs 派发上限挡新执行（诊断计数）。
- **IT·SQLite**：v30 新库 + v29 旧库升级；deadline 落行与过期查询；slot 寻址经真实 binding 表。
- **API**：三新面（dispatch/update；resolve 扩 slot 字段；诊断面 budget_exhausted）+ 写门 + 严格解码。
- **真机冒烟**：交办 → 短墙钟契约（60s 下限——冒烟用 `max_wall_clock_seconds` 最小合法值）执行超时 → run canceled（cause=wall_clock_exceeded）→ Mission 回 ready；任务卡追加经 dispatch/update 幂等（同键重放同版本）；slot 实例交办 → owner = 任职 Agent；换绑后旧决策投递 409 route_stale；无任职实例 resolve blocked。

## 后果

- 正面：0.6 域出口三项落差闭环（AT-14 续接 / AT-15 寻址 / AT-18 超时全兑现）；墙钟让 budget 从声明变强制；职责寻址使"按职责交办"成为一等入口。
- 负面：墙钟到期即停止——长任务被截断可再执行（attempt 递增、max_runs 兜底），但用户须显式感知（run 事件与诊断面承载）；slot 寻址歧义时须人工指实例（宁拒不猜）。
- 风险：ScanOnce 增两条查询（超时扫描 + 派发计数）——扫描周期 5s、量级单库数十 run，可忽略；dispatch.Update 的幂等窗口（pending 回执崩溃残留）由既有 pending 重驱动语义覆盖（0.6.1 ClaimKey 先例）。

## 修订记录

- v0.1（2026-11-03）：首版（随 0.6.13 实施）。
- v1.0（2026-11-05）：九审 2 项（1 P1 + 1 P2）修复——两项设计修订：
  1. **按卡草稿快照（前端，九审 #1）**：草稿与编辑记录**绑定具体任务卡**（`cardDraftsRef`——内容/预算每次变化记入当前 lastSrc 卡）；409 合并只用**当前卡的编辑快照**（未在本卡编辑过的字段回落详情现行值）——未映射 A 卡的 200 次上限/验收/交付不再写进 B 卡。显示层维持已确认卡清理（八审），数据层由快照保证。
  2. **恢复面错误语义补齐（§3，九审 #2）**：paused/canceling 分支与标记分支同规——停止标记**读取失败**上抛跳过（不当不存在重复写 stop_requested）；canceled 写失败保留 running 可重试（恢复返回错误，不再静默成功）。
- v0.9（2026-11-04）：八审 6 项（3 P1 + 3 P2）修复——三项设计修订：
  1. **已确认卡语义（前端）**：预算/草稿绑定**已确认卡**（详情命中预填过的来源，`confirmedSrcRef`）——逐字输入中的来源是草稿不触发清理（第二字符不再误判换卡清空正文/放宽预算）；仅离开一张已确认卡时清理一次，命中新卡时确认。
  2. **交付时点收口两段（§3）**：授权复核经 `agent.Task.PreRun` 在 supervisor 内 **Boot 完成后、Session.Run 之前**调用（Boot 期间撤权窗口关闭）；执行 goroutine 首步核对该 Run 的**持久停止事实**（建行后、句柄注册前暂停再恢复——标记在册注册表无信号；读取失败同样阻止交付），命中即不交付，错误走结果通道由终态事务按标记改判 canceled。复核闭包用 **runCtx**（客户端断开不使授权读取失败记 failed）。
  3. **恢复面审计与错误语义（§3）**：标记分支 canceled 携**原停止动作码**与 recovery 标记（无重复 stop_requested）；paused/canceling 分支映射动作码（pause/cancel）并按去重规则写事件；标记**读取失败**上抛跳过（不当不存在误判 unknown）；canceled 写失败保留 running 可重试（不落 unknown/waiting、恢复返回错误）。
- v0.8（2026-11-04）：七审 11 项（4 P1 + 6 P2 + 1 P3）+ 既有 P1 修复——五项设计修订：
  1. **来源草稿边界（前端）**：仅**换卡**（上一来源非空）时丢弃上一卡的预算与契约草稿——首填来源不重置（先设预算再填卡不放宽）；换卡清契约草稿（A 卡预填值不得经 409 合并写进 B 卡）；映射列表未载（null）期间锁定全部契约字段（formLocked——列表到达触发的详情预填不覆盖加载期输入）。
  2. **停止标记入控制事务（§3）**：暂停/取消的 Run 停止标记随 `PauseMission`/`MarkCanceling` **同一事务**写入（`stop_requested{cause=pause|cancel}`，NOT EXISTS 去重恰一条）；信号路径回归**零持久写**（监控只取消进程内上下文——两 Run 同时超时不被首条事件写拖住第二个的取消）；终态裁决组与 finalizeStopped 按存在性去重（恰一条停止审计）。
  3. **交付时点复核（§3）**：授权快照复核挂 `ExecutionInput.PreDeliver` 钩子——goroutine 入口先核（执行器未实现钩子的底线），真实适配器在会话启动后、内容送出前**实际交付时点**再核；失败经结果通道上报（停止意图在场 → 停止终理化 canceled；否则 failed → 终态事务再仲裁）。
  4. **恢复面核对 Run 停止标记（§3）**：RecoverUnfinished 对无 Mission 停止意图的在途 Run 先查自身持久标记——有则按停止兑现收尾（canceled + Mission 回 ready），不再误判 unknown/waiting（暂停→恢复→进程重启序列）。
  5. **终写失败上抛（§3）**：finalizeFailed/finalizeFenceRejected 的 UpdateRun 错误上抛 `ErrTerminalWrite`——手动入口不再伪装 201（Run 滞留 running 由恢复面收敛）。执行会话按 **Run** 隔离（撤权重装配后不复用旧线程——任务连续性由 Inline 每次全量重递）。手动执行 409 说明补 budget_exhausted。
- v0.7（2026-11-04）：六审 4 项（2 P1 + 1 P2 + 1 P3）+ 2 项基线修复——三项设计修订：
  1. **预算草稿绑定任务卡（前端）**：预算值与编辑集**归属 room/task**——切换来源即丢弃上一卡的编辑（共用集合会把 A 卡的 200 次上限写进 B 卡的续接合并）；新建请求在用户编辑过任何预算字段后即使收起面板也随请求发送（收起不得丢弃显式设置的限制）。
  2. **停止事实绑定 Run（§3）**：停止信号**首次送达**即把 stop_requested{cause, signalled} 持久写入该 Run 的事件流（resume 不清除——停止是 Run 级事实，恢复只允许新执行）；UpdateRunTerminal 事务内核对序：行上期限 → **Run 自身持久停止事实**（取其 cause）→ Mission 现行状态（映射为公开动作码 paused→pause / canceling→cancel——ADR-0029 约定，与 SignalStopByMission/finalizeStopped 同组常量）。"暂停→立即恢复→旧 Run 取得写锁"的窗口由此关闭。
  3. **交付前授权复核（§3，基线 P1）**：ExecuteMission 在提交执行器**之前**复核授权快照指纹（`VerifyGrantFingerprint`——Run 建行后、输入交付前撤权的窗口）：漂移则不交付（Run failed 留档 + 释放 + 回 ready，重新装配按剩余授权降级）。手动执行 API 的 409 说明补 grant_drift/contract_drift/owner_not_active 语义。
- v0.6（2026-11-04）：五审 8 项（3 P1 + 4 P2 + 1 P3）+ 既有 P1 修复——四项设计修订：
  1. **预算字段级编辑跟踪（前端）**：按预算字段记录用户编辑过的值——冲突合并以现行预算为底仅覆盖编辑字段（只展开查看或只改一个数值不再放宽其余上限）；面板展开状态移出详情加载依赖（展开/收起只改显示，不再触发重载覆盖草稿）。
  2. **重放清单的授权回填（§3）**：同 digest 命中存档重放时以**本次实际授权**重算填写 GrantFingerprint 交运行层（存档行无此列——先预览后运行、旧库存档清单的领取事务核对由此生效；digest/ManifestID/回执历史语义不变）。
  3. **终态事务的停止意图核对（§3）**：UpdateRunTerminal 事务内裁决条件扩展——行上期限已过 **或 Mission 持久停止意图（paused/canceling，存储层可读）**；命中即**替换**（非追加）原拟提交终态事件——唯一一组 stop_requested + canceled + late_result（preview ≤400 rune、携 generation、published=false）；失败终态同样并入（快速路径与锁后裁决一致）；Mission 投影原因取实际落库终态（Run 行、事件流、状态原因三者重建同一结果）。
  4. **认领读面鉴权与指引（§1）**：GET claims 执行 owner token 校验（401 契约——完整摘要是处置凭据，与 release 同源保护）；ErrGrantDrift 映射 409 `grant_drift`（手动执行与其它冲突一致——不再 500）；遗留待核验文案统一单一处置流程（删除 SQLite/内存分支残留的"携当前版本重试"旧指引——照做必得摘要冲突）。
- v0.5（2026-11-04）：四审 8 项（4 P1 + 4 P2）修复——四项设计修订：
  1. **冲突草稿合并与归属绑定（前端）**：create 撞 409 `source_already_mapped` 后保留的用户草稿**绑定引发 409 的 room/task**（页面级标志会被其他来源的详情消费串卡）；命中同一来源时以详情现行契约为**底稿合并**用户显式输入的字段（非空者），未编辑字段与预算留现行值——不清空验收/交付、不放宽 1 次/60 秒等预算；合并结果回填表单供核对。预算开关与输入在详情加载期间禁编辑（与契约字段同门）。
  2. **清单授权快照指纹（§3 / RFC §11.4）**：execctx 装配时计算 `GrantFingerprint`（授权档案规范化哈希——集合序归一）随清单传递；`CreateRun` 建行事务内与**当前授权**比对——可选上下文/工具撤销（必需判定不变）即 `ErrGrantDrift` 拒领，释放租约重新装配按剩余授权降级。
  3. **终态写入的写锁后裁决（§3）**：`UpdateRunTerminal`（双存储）在**取得写锁后**以 nowFn 现行时刻核对行上 deadline——等待写锁期间期限越过/监控已发停的窗口收口：completed/failed 改按 canceled（cause=wall_clock_exceeded）落终态，拟发布结果仅 late_result 审计；失败终态同样并入停止归档。
  4. **认领处置资格与精准结果（§1）**：`ReleaseClaim` 单一写事务内判定+删除，仅 RowsAffected=1 为释放；零行按事务内真值分辨（finalized/not_found/digest_mismatch）。资格限定 **update 类认领（route_id 空）**——create 在途认领（route 非空）公开处置不可及（409 `claim_not_releasable`，防同键建两单串回执）。新增 `GET /v1/dispatch/claims/{key}` 只读面（四审 #7：提供 release 所需完整 request_digest 与状态）；`legacy_pending_unresolved` 文案改为正确指引（取摘要 → 确认未执行 → release → 更正载荷重提——不再指引必然摘要冲突的"改版本同键重试"）。
- v0.4（2026-11-04）：三审 6 项（4 P1 + 2 P2）+ 既有前端 2 项修复——四项设计修订：
  1. **create 路径拒绝不释放认领（§2）**：并发孪生共享同一条 pending（同键同摘要即同一请求），单方释放会让异摘要请求冒用键建第二单并串单（复审 #8 引入的回归）。语义：拒绝后 pending 留存，同键同载荷重试重驱动（孪生胜者完结后重放其回执）；一致性由采纳原行 + 回执 route_id 真值 + upsert 自愈保证。Update 路径拒绝释放保留（认领排他 + upsert 自愈）。
  2. **续接 CAS 败按认领来源分流（§1）**：**新插入**认领 → version_conflict 透传并回滚（连认领一并撤销——修正后重试；等值内容不冒领他人修订）；**既有** pending（并发孪生或旧候选版遗留）→ `legacy_pending_unresolved` 待核验（版本与正文等值不可证明归属——自动恢复分支删除）；处置入口：`POST /v1/dispatch/claims/{key}/release`（确认未执行后释放，digest 核对，已完结不可动）+ `ClaimByKey` 只读面；携当前版本同键重提在 release 后可用。
  3. **CreateRun 事务内任职授权与归属核对（§3）**：授权——清单所钉任职的钉版模板要求 vs 授权档案（复用建单门同源判定，撤权后旧清单不送达执行器，AT-16）；归属——owner_agent_id 与执行者核对（交接承接不递增版本，旧负责人清单不得领取新 Run）。
  4. **完成提交前停止仲裁下沉（§3）**：finalizeRun 在围栏通过后、终态写入前**重新**读取停止意图与行上期限（fresh now）——停止成立改按停止终理化（迟到正文只审计），残余窗口缩至判定与写入之间数行。
  前端两项：契约字段在详情加载期间禁编辑（防读取返回覆盖刚输入文字）；create 撞 409 source_already_mapped 后保留录入切续接模式（仅取现行版本，不预填覆盖）。
- v0.3（2026-11-03）：复审 13 项（5 P1 + 6 P2 + 2 P3）修复——五项设计修订：
  1. **建单事务门统一（§2）**：`CreationGate{Slot *SlotIntent, RequireOwnerActive bool}` 取代单一意图——显式 owner 网关交办携身份门（预检后退役/暂停不得落入新承诺，复审 #3）；slot 意图五核（+ 模板唯一性锚：SpecID 非空时 scope 实例集须仍恰为解析实例——长出兄弟实例即歧义拒建，复审 #7）；授权档案事务内快照**补 workspace_access**（漏读使合法 read_write 被当空值误拒，复审 #5）。
  2. **执行预算单快照（§3）**：预算（墙钟 + max_runs）取**清单钉住版本的契约层**（与执行输入同版本）；`CreateRun` 建行事务内核对——现行契约版本（漂移 `ErrContractDrift` 释放租约重新装配）与现行内容 max_runs（RefOf 后收紧即时生效，复审 #10）+ 执行者门（负责人 active + 所钉任职仍 active 未换人，`ErrOwnerNotActive`，复审 #4——暂停不自动解除任职不得启动）。
  3. **完成提交前终态判定（§3）**：结果分支在写完成前按**停止意图或行上期限**原子决定去向（复审 #9——独立监控信号必然伴随 deadline 已过，按期限兜底不依赖注册表读取时序；过期正文只审计不发布）。
  4. **投递认领一致性（§1/§2）**：Dispatch 采纳 pending 原行标识；复核/预检/建单拒绝**释放认领**（不占键）；FinalizeReceipt 写实际成功 route_id 并 upsert 自愈（复审 #8——换绑拒绝后换路由同键重投，首次响应与重放/按键查询同一份）。
  5. **v30 遗留恢复（§1）**：升级库的遗留 pending 续接认领——版本恰 expected+1 且现行内容与载荷等值（SameContent，摘要绑定同一命令）= 修订确已提交 → 完结回执；不等值不能证明 → 冲突透传（原子面回滚不占键；不无条件删除可能已提交的键，复审 #11）；reason 空列回落命令值（复审 #12）。前端续接模式重写：目标/版本/草稿（含 constraints/delegation 基准）同请求原子生效、代次作废、失败禁提重试、冲突显式重载（复审 #1/#2/#6）；摘要与详情同墓碑（#13）。
- v0.2（2026-11-03）：独立审校 17 项（5 P1 + 10 P2 + 2 P3，`v0.6.12..b59a395`）修复——四项设计修订：
  1. **续接原子化（§1 重写）**：认领 + 复核（owner 在册且 active 的**事务内身份门**——暂停/未指派/读取失败皆拒）+ 修订 + 回执**同一写事务**（SQLite `UpdateMissionAtomic`；内存件退化为分步 + 拒绝回滚补偿 `ReleaseKey` + pending 采纳原行）。领域拒绝整体回滚**不占键**（修正后同键重试成功）；修订已生效而回执失败不再出现；恢复重驱动与重放同 receipt_id；`reason` 随回执持久化读回；目标引用携修订后版本（`mission:<id>:v<n>`）；入口字段校验 400 先于认领（mission_id 形制 / expected_version≥1 / request_id≤128 / reason≤280）。公开回执查询只见已提交行（pending 是事务内部事实）。
  2. **派发意图落任务（§2 补强）**：解析所得实例 + 任职作为**决策事实另存**（`resolved_agent_id`/`resolved_instance_id`——原请求摘要稳定：换绑后重解析同键可重放，模板实例集漂移即 stale）；建单 `CreateMissionSlot` 在**同一写事务**四核（实例在册 / active 任职 == 预期 / 负责人 active / 授权准入 `EvaluateReadiness` 单源）——换绑/暂停/撤权不得插进复核与建单之间（`ErrSlotIntentViolated` → route_stale）；`missions.slot_instance_id` 为创建事实，执行装配按**实例**固定 Binding（同人双任职不串用；`Bindings.ActiveOfInstance`）。预检补 owner active 与授权准入（blocked `agent_not_active`/`slot_grant_missing`）。
  3. **墙钟监控独立（§3 重写）**：独立 ticker goroutine（不随调度循环的同步执行器阻塞）+ **按 RunID 直达**停止注册表（不经 Mission 再扫描——同 Mission 新 Run 未到期不误伤）；期限比较改**时刻**（RFC3339Nano 小数位数不定，字典序 .11Z < .1Z 会漏已过期行）。
  4. **预算权威面（§3 补强）**：`run.Missions.RefOf` 启动前重读现行契约（owner/意图/预算同一快照——手动与自动入口共用；枚举快照陈旧不再生效，预算查询失败中止当次派发）；`CreateRun` 建行事务内核对 max_runs（并发孪生恰一胜者；外层拒绝释放租约）；手动达限映射 409 `budget_exhausted`。任务卡续接入口：已映射来源在 MissionsPage 切"追加要求"模式走 `/v1/dispatch/update`（`MissionSummary.room_source` 识别）。migration v31（missions.slot_instance_id / dispatch_decisions 解析事实两列 / dispatch_receipts.reason）。
