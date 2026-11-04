# ADR-0036：0.6 域收口——网关续接、Slot 职责寻址与墙钟强制（0.6.13）

- 状态：Accepted（2026-11-03，随 0.6.13 实施）
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
