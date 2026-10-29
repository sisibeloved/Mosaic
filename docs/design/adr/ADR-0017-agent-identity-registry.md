# ADR-0017：独立 Agent 身份注册表（0.5.0）

- 状态：Accepted（2026-10-28，随 0.5.0 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.1 对象边界、§13.2 存量升级、§11.1 权威存储）、[ADR-0013](ADR-0013-stable-bot-identity.md)（稳定 BotID）、[ADR-0008](ADR-0008-personal-storage-sqlite.md)（SQLite 同库）、交付计划 v2.00（0.5.0 唯一交付特性：独立 Agent 身份：AgentID、旧 ID 别名与注册表迁移）

## 背景与问题

ADR-0013 给了 durable `BotID`，但它仍附着在 `harness-registry.json` 的
`Executable` 上；profileID / participantID 在装配层每次扫描时从 BotID 派生
（`prof_<adapter>_<sanitize(BotID)>` / `par_<adapter>_<sanitize(BotID)>`）。
由此，"一个 Agent"在系统里没有自己的存在：

1. **身份寄生在部署位置上**。可执行项被移除（清理/禁用/路径永久消失）后，
   该 BotID 的任何痕迹只剩历史事件里的 participantID 字符串；无法回答
   "这台机器上有哪些长期 Agent、各自挂着哪些可执行与座位"。
2. **身份权威与发现数据同库同表**。harness-registry.json 既是扫描产物又是
   身份载体；RFC-0018 §13.2.3 要求身份权威独立，可执行发现表只引用 AgentID。
3. **无别名解析面**。旧事件、座位、注册表三处各用一种 ID（participantID /
   profileID / BotID），没有一处能互相翻译。

0.5.0 只交付一件事：把"Agent 是谁"从注册表里拆出来，成为 SQLite 里的独立
权威对象，并把旧 ID 收敛为它的别名。Slot / Binding / 生命周期状态机分别是
0.5.2–0.5.5 与 0.5.1，不在本 ADR 范围。

## 决策

### 1. 新域 `internal/personalagent`（RFC-0018 §4.3 落点）

- 聚合根 `Agent`：`agent_id、display_name、adapter、status、source、created_at`。
  0.5.0 里 `status` 恒为 `active`（状态机随 0.5.1）；`source` 取
  `migration`（自注册表迁移）/ `manual`（未来手工建）。
- `AgentID` 形态：`ag_` + 时间有序毫秒 + 随机后缀（与 `newID` 前缀族同构），
  生成即持久，永不回收重派。
- 别名表独立于 Agent 行：`alias_kind ∈ {bot_id, profile_id, participant_id}`，
  `(kind, value)` 唯一指向 `agent_id`。别名是**只增事实**：旧 ID 一旦登记
  不改写、不删除（房间历史事件里的 participantID 永远可解析）。

### 2. 存储：SQLite 同库 migration v7（快照权威表，事件流后置）

```
CREATE TABLE agents (          -- Agent 聚合快照（0.5.0 无状态转移，事件流随 0.5.1）
  agent_id TEXT PRIMARY KEY, display_name TEXT NOT NULL, adapter TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active', source TEXT NOT NULL,
  created_at TEXT NOT NULL);
CREATE TABLE agent_aliases (   -- 旧 ID → AgentID 解析面（只增）
  alias_kind TEXT NOT NULL, alias_value TEXT NOT NULL, agent_id TEXT NOT NULL,
  created_at TEXT NOT NULL, PRIMARY KEY (alias_kind, alias_value));
```

选快照表而非 `agent_events` 流的理由：0.5.0 只有 `created` 一个事实、无状态
转移与回放需求；0.5.1 引入生命周期时建流并回填投影，届时本表降为投影重建。
同库（ADR-0008）使身份随既有备份/恢复介质覆盖，无需新增备份面。

### 3. 身份等价与迁移算法（`SyncFromHarness`，幂等）

等价键 = **bot_id 别名**（ADR-0013 重绑语义的自然延伸：路径变化不改 BotID，
故 BotID 相等即同一 Agent）。算法：

1. 读注册表全量 `List()`（含未启用项——身份不取决于是否可执行）；
2. 对每个 exe：按 `bot_id` 别名查 Agent——
   - 未命中 → 建 Agent（`ag_*`，display_name 取 adapter 家族名，source=
     migration）+ 登记三别名：bot_id、`prof_<adapter>_<sanitize(bot_id)>`、
     `par_<adapter>_<sanitize(bot_id)>`；
   - 命中 → 只补缺失别名（防御旧库半迁移），不更新既有字段；
3. 回写 `Executable.AgentID`（新字段，批量收集后一次 `ApplyAgentIDs` 原子落盘，
   与注册表写锁同临界区）；
4. 别名碰撞（同 kind+value 已指向另一 Agent）：**不合并、不覆盖**，记冲突
   note（启动日志可见），进入待处理——RFC-0018 §13.2.2"不按同名或同 adapter
   自动合并"的落地。同名/同 adapter 从不构成合并依据。

派生函数（sanitize + 前缀拼接）从 `internal/app/app.go` **收敛到
personalagent 单源**（`ProfileKeyOf / ProfileIDOf / ParticipantIDOf`），app.go
改为调用；输出必须与旧实现逐字节相等（有专门测试钉住，保证存量
participantID 解析不漂移）。

### 4. 同步时机（单向：registry → identity）

- 启动装载注册表后（覆盖磁盘存量，先于扫描）；
- 启动扫描完成后（覆盖新发现/重绑）；
- 身份 API 读前惰性同步（手动登记/删除可执行项后无需挂钩子即最终一致）。

同步只增不改：可执行项从注册表消失**不删 Agent**（Agent 跨部署位置存在是
本特性的意义；退役是 0.5.1 的显式动作）。身份表永不回写注册表身份字段
（bot_id 等），杜绝双向同步双权威。

### 5. 兼容规则（RFC-0018 §13.2.2）

- `Executable` JSON 增 `agent_id,omitempty`——增量字段，旧字段（id / bot_id /
  …）原样保留；旧 API 输出保持旧 ID；
- 座位面 `GET /v1/agents` 完全不动（participant_id / adapter / display_name）；
- 装配层座位派生逻辑不变（改经 personalagent 单源函数，输出不变）。

### 6. API 面（契约冻结，openapi.yaml 权威）

- `GET /v1/identity/agents` → `{agents: [IdentityAgent]}`；
- `GET /v1/identity/agents/{agent_id}` → `IdentityAgent | 404`；
- `IdentityAgent`：`agent_id、display_name、adapter、status、source、created_at、
  aliases{bot_id, profile_ids[], participant_ids[]}、executables[]
  {executable_id, runtime, channel, enabled}`（可执行引用从注册表现算，
  只读）；
- `GET /v1/harness/executables` 的 `Executable` schema 增 `agent_id`。

两个新端点均为只读观察面，无写门（与房间/文档 SSE 读面同口径）；身份写面
（create/suspend/retire）随 0.5.1。

### 7. UI 最小面

设置页宿主扫描区每个可执行项行展示 `ag_*` 身份徽标（别名明细进
title/tooltip）。完整 Agent 导航入口（RFC-0018 §12）随 0.5.1+ 交付。

## 测试策略（先行红→绿）

| 层 | 用例 |
|---|---|
| UT personalagent | 迁移幂等（连跑两次结果一致）；新 BotID 建 Agent + 三别名；同 BotID 多可执行项归一 Agent；别名三向解析；派生函数与 app.go 旧式逐字节等价；别名碰撞记 note 不合并 |
| IT sqlite | migration v7 新库/旧库双路径建表；Agent/别名 CRUD 往返；(kind,value) 唯一约束 |
| API httpapi | 两端点形状与 404；`/v1/harness/executables` 含 agent_id 且旧字段原样；`/v1/agents` 座位面不变 |
| 迁移 fixture（P0 门槛） | `testdata/registry-migration.json`（多 adapter / 多实例 / 缺 bot_id 祖父化 / 同 BotID 双 exe）→ 同步两次确定性 golden；既有 harness/room 测试全绿 = 旧行为不变 |

## 后果

- 正面：身份权威入 SQLite（事务 + 备份面）；旧三 ID 有了统一解析点；
  0.5.1+ 的生命周期 / Slot / Binding / Mission 获得挂靠主体；旧面零破坏。
- 代价：注册表与身份表存在单向同步窗口（读前惰性同步收敛）；启动多一次
  幂等同步（毫秒级，条目数 = 可执行项数）。
- 放弃方案：JSON 文件身份库（无事务/备份面，重复注册表的老路）；
  `agent_events` 事件流先行（无状态转移可记，空转复杂度）；
  按显示名 / adapter 合并（违反 RFC-0018 不自动合并裁定）；
  座位 participantID 直接换成 agent_id（重写存量事件引用，违反不重写旧事件）。

## 实现清单

- `internal/personalagent/`：`agent.go`（类型 + 派生单源）、`service.go`
  （SyncFromHarness / List / Get / Resolve）、`store.go`（Store 端口）、
  `memstore.go`（测试件）；
- `internal/storage/sqlite/store.go`：migration v7 + `personalagent.Store` 实现；
- `internal/harness/registry.go`：`Executable.AgentID` + `ApplyAgentIDs`；
- `internal/app/app.go`：装配（装载后/扫描后同步）+ 座位派生改单源调用；
- `api/http-api/openapi.yaml` + `internal/transport/httpapi/server.go`：两端点；
- `apps/web/src/`：设置页身份徽标；
- 测试四层 + `testdata/registry-migration.json` golden。
