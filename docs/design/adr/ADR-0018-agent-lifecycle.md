# ADR-0018：Agent 生命周期状态机与事件流（0.5.1）

- 状态：Accepted（2026-10-29，随 0.5.1 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.2 状态转移约束、§12 停止 Agent 的三分、§13.2 存量升级）、[ADR-0017](ADR-0017-agent-identity-registry.md)（v7 快照表——"事件流随 0.5.1 引入时建流并回填投影，届时本表降为投影重建"、§6 身份写面预留）、交付计划 v2.00（0.5.1 唯一交付特性：Agent 生命周期：active、suspended、retiring、retired 状态机）
- 上游需求变更注记：交付计划 v2.04 新增 0.10 个人数据备份与多设备同步领域（RFC-0019 S1–S6 → 0.10.0–0.10.5；自动同步另立 RFC 预排 0.10.6–0.10.13），**0.5–0.9 排期不变**——本 ADR 范围不受影响。

## 背景与问题

ADR-0017 交付的 Agent 是"恒 active"的静态身份：`agents.status` 落库后无人改写，
也没有任何历史可查。RFC-0018 §3.2 要求 Agent 有独立生命周期——

```
active ↔ suspended；active/suspended → retiring → retired
```

且"active 不代表正在运行；retiring 停止接新任务，处置所有承诺后才能
retired"。ADR-0017 落库时预埋了两个 0.5.1 义务：引入生命周期时**建
`agent_events` 流并回填投影**（v7 快照表降为投影重建基线），以及补**身份
写面（create/suspend/retire）**。

0.5.1 只交付状态机本身：状态的权威存储、显式转移、事件史与写面 API。
不交付任何"消费状态做行为管制"的 enforcement（那是 0.5.2–0.5.5 Slot /
Binding / 准入与 0.6 Mission 的职责）。

## 决策

### 1. 状态机（Go 单源校验表）

| 转移动作 | from → to | 语义 |
|---|---|---|
| `suspend` | active → suspended | 暂停：身份保留、状态可见；不触发任何运行行为（0.5.1 无执行域） |
| `resume` | suspended → active | 恢复 |
| `retire` | active \| suspended → retiring | 停止接新任务的声明态；经承诺门后自动完成 |
| （承诺门通过） | retiring → retired | 终态；行永不删除（身份只增，RFC-0018 §13.2.2） |

- `retired` 为**唯一终态**：此后一切转移拒绝（invalid_transition）。
- `active` 不代表正在运行——它是身份状态，不是进程状态（§3.2 原文）。
- **承诺门**：RFC-0018 要求"处置所有承诺后才能 retired"。0.5.1 中
  Mission / Binding / Goal 域尚未存在，承诺集合**恒为空集**，故 `retire`
  在同一调用内完成 retiring → retired（两个事件、一个事务边界内先后落）。
  为不把"恒空"写死：`Service` 持 `CommitmentChecker` 端口
  （`Outstanding(agentID) ([]string, error)`），生产装配缺省 nil = 空集；
  返回非空时 Agent 停留在 retiring，API 以 `outstanding_commitments`
  返回未处置清单（0.5.2+/0.6 各域接入后自然生效）。对已处 retiring 的
  Agent 再次 `retire` 不重复记 retiring 事件，只重过承诺门。
- **转移拒绝可见而非静默幂等**：对 active 再 suspend 返回 409
  invalid_transition——转移重放必须让客户端重取终态，不吞掉状态漂移。

### 2. `agent_events` 事件流（SQLite migration v8）

```
CREATE TABLE agent_events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,  -- 全表单调，流序
  agent_id    TEXT NOT NULL,
  event_type  TEXT NOT NULL,   -- created | status_changed
  payload     TEXT NOT NULL,   -- JSON 事件体
  occurred_at TEXT NOT NULL,
);
CREATE INDEX idx_agent_events_agent ON agent_events(agent_id, seq);
```

- 事件体：`created` = `{source, adapter, display_name}`；
  `status_changed` = `{from, to, reason}`（reason 可空串）。
- **回填**（v7 存量库）：Open 时幂等 `INSERT … SELECT`——每个尚无
  `created` 事件的 agents 行按 rowid（创建序）补一条，`occurred_at` 取
  行内 `created_at`。新库（v8 起建）回填自然为空集；重开不重复。
- **投影降级**（兑现 ADR-0017 预留）：转移 = 单事务
  [读 status（Go 状态机表校验）→ UPDATE agents.status → INSERT
  agent_events]；`EnsureAgent` / `EnsureAgentWithAlias` 建行与 `created`
  事件同事务落库（要么两行、要么什么都不留）。由此不变量
  **"事件流 ⊇ 全部历史，agents 表可由流重建"**成立；不提供运行时重建
  入口（无消费场景；恢复演练随 0.10 备份域）。
- **写临界区（v0.2 审校修订 #1）**：身份域全部写事务（建行/认领/转移/
  退役）经 `BEGIN IMMEDIATE` 进入写锁后再读——WAL 下"deferred 事务先读
  后写"会在并发提交后撞 `SQLITE_BUSY_SNAPSHOT`，把本应按 409 语义返回
  的并发败者外泄成 500（100 次重复复现 5 次）。锁等待发生在 BEGIN 取锁
  处，由 busy_timeout 承担（与房间 appendTx 同机制）。
- **原子退役（v0.2 审校修订 #4）**：空承诺门的 retire 不再分
  retiring / retired 两次提交——`RetireAgent` 在同一事务内落两段事件与
  终态，中途取消/失败整体回滚，不滞留 retiring。承诺门前置判定（不依赖
  retiring 态）；已处 retiring 的中断重试走单步 TransitionAgent 只补终段，
  不重复记 retiring 事件。
- 并发：SQLite 写经 BEGIN IMMEDIATE 串行化；两路并发转移同一 Agent 恰一
  胜者，败者按其读到的 from 拒绝（409）。

### 3. 同步与生命周期解耦（单向只增的延续）

- `SyncFromHarness` **只建不改**（ADR-0017 §4 语义在生命周期下不变）：
  已存在 Agent（含 suspended / retiring / retired）的 bot_id 别名再现
  （重装、重扫、重绑）**不复活状态**——身份认领只影响归属，不触碰
  status；新 BotID 仍以 active 出生。
- 退役 ≠ 卸载可执行项：retired Agent 的可执行项仍出现在注册表与座位面
  （房间是可执行项驱动的）。状态的行为管制（停止接单 / 暂停相关任务 /
  换人交接，RFC-0018 §12 三分）由后续版本的消费域落地；0.5.1 的状态是
  **先行的权威事实**，供读面与后续 enforcement 挂靠。

### 4. 身份写面 API（契约冻结，openapi.yaml 权威）

写门与 harness 手动登记同口径（`guardWrite`：Origin 回环校验 +
Content-Type + X-Owner-Token）。

- `POST /v1/identity/agents`：手动建身份（`{display_name, adapter?}` →
  201 IdentityAgent，source=manual、status=active、created 事件同事务）。
  为 0.5.2+ Slot 任职与 Worker 晋升预留主体；display_name 必填（1–64），
  adapter 可空（空 = 无 harness 家族挂靠）。UI 入口随 0.5.2+（身份有可
  挂靠对象时）再给，0.5.1 仅 API。
- `POST /v1/identity/agents/{agent_id}/lifecycle`：
  `{action: suspend|resume|retire, reason?}` →
  `LifecycleResult {agent: IdentityAgent, outstanding_commitments: []}`；
  404 identity_not_found / 409 invalid_transition。
- `GET /v1/identity/agents/{agent_id}/events`：只读审计面，升序全量
  （`{events: [{seq, agent_id, event_type, payload, occurred_at}]}`）。
- `IdentityAgent.status` 收紧为 enum `{active, suspended, retiring,
  retired}`（读面形状不变，枚举扩义——0.5.0 客户端只见 active）。

### 5. Store 端口扩展

`AgentByID`（转移前置读取）、`TransitionAgent(agentID, allowedFrom [],
to, reason, occurredAt) (from, ok, err)`（事务校验 + 投影 + 事件，ok=false
= 当前状态不在 allowedFrom）、`AgentEvents(agentID)`（升序）。MemStore
同语义（含事件追加），UT/端点测试共用。

## 非目标（0.5.1 明确不做）

- 不做状态的执行面 enforcement：座位派生、房间引擎、扫描启用一律不读
  status（消费域后置，见 §3）。
- 不做 Agent 改名 / 删除：身份只增；display_name 编辑有真实需要时另立。
- 不做身份面 SSE 推送：0.5.1 无实时消费场景，动作后 refetch 即得；
  0.5.2+ UI 有订阅需要时再加（hub 已在）。
- 不做运行时投影重建入口（见 §2）。

## 测试策略（先行红→绿）

| 层 | 用例 |
|---|---|
| UT personalagent | 状态机表全组合合法性（含 retired 终态全拒）；suspend↔resume 往返事件序列；retire 空 promise 门 → 一调用至 retired（事件恰 created/retiring/retired 各一）；注入非空门 → 停留 retiring + outstanding 返回；门清空后再 retire → 完成且 retiring 不重复记；同步不复活（retired 后 SyncFromHarness 状态不变、同 botID 重扫仍 retired、新 BotID 生 active）；manual 建（active/manual/created 事件/空名拒绝）；created 事件唯一性不变量（多轮同步不增） |
| IT sqlite | v8 新库建表 + 迁移登记；v7 存量库升级回填（行数/序/payload 对齐、重开不重复）；TransitionAgent 合法/非法/并发 8 路（恰一胜者、事件恰一条、投影与事件一致）；EnsureAgentWithAlias 建行与 created 事件原子；AgentEvents 升序 |
| API httpapi | 三个 action happy path + 409 + 404 + 写门拒（无 Origin / 无 token）；manual 建 201/400；events 端点形状；0.5.0 读面断言原样通过（兼容不破坏） |
| 迁移 fixture | 既有 registry-migration golden 不动（注册表面未变）；生命周期另以 UT/IT 覆盖 |

## 后果

- 正面：Agent 有了跨版本可审计的生命周期事实（事件流）；0.5.2+ Slot /
  Binding / Mission 获得状态依据与承诺门挂靠点；备份域（0.10）获得可
  回放的权威流。
- 代价：转移路径多一次事务内读（微秒级）；事件表随转移线性增长（个人版
  规模可忽略，无自动清理——退役后不再有新事件）。
- 放弃方案：status 直改不留史（不可审计、无法回放）；retire 一步到
  retired 不设 retiring（违反 §3.2 两段语义，承诺门无落点）；同步时
  顺手复活 retired（违反只增不改，重装即复活等于没有退役）；
  静默幂等转移（吞状态漂移，见 §1）。

## 修订记录

| 版本 | 日期 | 说明 |
|---|---|---|
| v0.1 | 2026-10-29 | 初稿（随 0.5.1 实施） |
| v0.2 | 2026-10-29 | 审校修订（1 P1 + 4 P2）：#1（P1）身份写事务族（Ensure/EnsureAgentWithAlias/Transition/Retire）改 BEGIN IMMEDIATE 临界区——读在写锁内，WAL 读后升级的 SQLITE_BUSY_SNAPSHOT 不再外泄 500，并发败者按 409 返回（HTTP 层 8 路并发实测恰一 200、其余 409、零 500；IT 100 次重复全绿）；#4 空承诺门 retire 改 RetireAgent 单事务一次提交（两段事件 + 终态，中断整体回滚不滞留 retiring；承诺门前置判定，retiring 重试只补终段）；#2 生命周期操作成功后按修订号使前端事件明细缓存失效并重读（已展开明细立即刷新，连续操作每次最新）；#3 手动登记/启停可执行项后前端同步刷新身份列表（新身份立即可暂停/退役）；#5 前端操作锁保持至列表刷新完成 + 请求序号拒绝过期响应（旧慢响应不得覆盖新状态） |
