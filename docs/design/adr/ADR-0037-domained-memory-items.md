# ADR-0037：0.7.0 分域 MemoryItem——五命名空间记忆基石（0.7.0）

- 状态：Accepted（2026-11-04，随 0.7.0 实施；v0.2–v0.3 两轮审校修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.1 数据分层与归属——"增加 Agent、Workspace、Goal/Mission 和职责实例的记忆域；原 Room 记忆不自动变成 Agent 私有记忆；跨域复制须记录授权与来源"；§7.2 MemoryItem 拟定字段；§7.4 MemoryBackend 端口；AT-04）；[RFC-0007](2026-08-25-rfc-0007-context-memory.md)（§7.4 原 Memory 归房间——胶囊/记忆编辑保持房间事件流事实）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.0 唯一交付特性：分域 MemoryItem——Agent、Room、Workspace、Mission、Slot 命名空间）。

## 背景与问题

0.6 收官后记忆仍是**单域**：房间收束胶囊（`internal/room` closure/memoryedit，事件流事实）是唯一长期记忆形态；Agent 私有经验、Workspace 项目知识、Mission 阶段积累、Slot 职责规范没有归属容器。RFC-0018 §7.1 明确数据分层：五类记忆域与检索索引分离、类型（semantic/episodic/procedural）与权限域正交；§7.2 拟定 MemoryItem 字段；0.7.1 策展、0.7.2 召回、0.7.5+ 迁移包都叠在这块基石上。

本件（0.7.0）只交付**基石**：MemoryItem 域模型 + 五命名空间 + 锚校验 + 存储与读写面。不做策展状态机（0.7.1）、不做 FTS/授权过滤召回（0.7.2）、不做迁移（0.7.5+）。

## 决策

### 1. 五命名空间与锚

| Namespace | 锚（scope_id） | 存在性校验（写入时） | 语义（RFC §7.1） |
|---|---|---|---|
| `agent` | agent_id | identity 在册（personalagent.AgentByID） | 长期偏好、经验、经确认的公开人格配置——Agent 私有域 |
| `room` | room_id | 房间事件流在册（room.RoomExists 口径——见过 room.created） | 项目知识、房间共识与异议（共享域） |
| `workspace` | 固定 `"default"` | 恒真（单工作区事实） | 工作区级知识（个人版单工作区；多设备/Bundle 0.7.5+ 不破坏） |
| `mission` | mission_id | Mission 在册（sqlite.MissionByID） | 阶段经验、假设、证据整理——随事务保存 |
| `slot` | slot_instance_id | SlotInstance 在册（slot.InstanceByID） | 职责实例的工作规范与环境说明——区别于模板与任职者私有记忆 |

- 锚不存在 → 409 `memory_scope_not_found`（域事实性校验，区别于字段格式 400）；workspace 命名空间 scope_id ≠ "default" → 400（单工作区事实）。
- **每条记忆归属恰一命名空间**（无全局域）；列表按命名空间必填隔离——跨域列表不存在（§7.3 "授权及有效期过滤先于排序、摘要与计数"的基调在 0.7.0 就位：越域内容与**存在性**不泄漏）。
- 与 RFC-0007 房间记忆的关系：房间胶囊与 memory.edited **不动**（房间事件流的权威事实）；`room` 命名空间 MemoryItem 是显式人工/系统写入的房间派生记忆，0.7.0 **不自动**从胶囊生成（抽取归 0.7.1 策展——"不能从未经授权的上下文提炼后转入更宽可见域"）。

### 2. MemoryItem 字段（0.7.0 稳定子集 + 预留列）

表结构与域结构一次带全 §7.2 拟定字段（后续小版本免连续迁移）；**0.7.0 写入面**只开放稳定子集，策展/召回字段由域固定默认：

| 字段 | 0.7.0 写入面 | 默认/说明 |
|---|---|---|
| memory_id / version | 系统生成 | ULID 风格 `mem_` 前缀；version 恒 1（修订 0.7.1 起递增） |
| namespace / scope_id / kind | ✅ 必填 | kind ∈ `semantic \| episodic \| procedural`（类型与权限域正交——§7.1） |
| title / content | ✅ 必填 | title ≤200 rune；content ≤20000 rune（显式校验 400） |
| source_refs | ✅ 可选 ≤16 条 | 每条 ≤500 rune——"每条记忆须能追到已提交事件、文档版本或显式人工记忆操作" |
| observed_at | ✅ 必填 RFC3339 | "何时知道" |
| valid_from / valid_to | ✅ 可选 RFC3339 | "何时有效"；valid_to > valid_from（否则 400） |
| sensitivity | ✅ 可选 | `normal \| sensitive`（默认 normal——0.7.5 Bundle 脱敏消费） |
| confidence | ✅ 可选 0–1 | 默认 1.0（人工显式写入满置信；模型抽取候选 0.7.1 自带置信） |
| status | 固定 `active` | proposed/superseded 状态机与时效判定归 0.7.1（列预留） |
| visibility | 固定 `namespace` | 域内可见；授权过滤细化归 0.7.2（列预留） |
| supersedes / derived_from / edit_history / extractor_version | 空 / `"manual"` | 策展链（0.7.1）/Bundle 血缘（0.7.5）预留列 |

### 3. 端口切片（MemoryBackend 的 0.7.0 子集）

`internal/memory`：域（校验单源）+ `Service`（Anchors 组合）+ `Store` 端口（Create/Get/List）——§7.4 的 `Propose/Accept/Revise/Search/Export/Import/Forget` 分别归 0.7.1/0.7.2/0.7.5–0.7.8，本件不预建空方法。

- **Anchors**（app 装配组合闭包——同 dispatchSlotLookups 依赖方向模式，memory 不 import 各域）：
  `AgentExists / RoomExists / MissionExists / SlotInstanceExists func(ctx, id) (bool, error)`。
- **List(filter)**：namespace 必填；scope_id/kind/status 可选过滤；`limit ≤100`（默认 50）+ `before`（memory_id 游标，创建序降序——新记忆在前）。
- 幂等：显式写入不带幂等键（与房间命令面不同——记忆创建无副作用竞态面，重复提交产生两条独立记忆由 0.7.1 去重策展收敛）。

### 4. 存储与迁移

SQLite migration **v32**：`memory_items` 表（全字段列 + `(namespace, scope_id)` 与 `(namespace, created_at)` 索引 + memory_id 主键）；memstore 双实现（UT/内存件装配）。

### 5. HTTP 契约（apigen 冻结）

- `POST /v1/memory/items`——guardWrite 三件套 + MaxBytesReader 1MiB + DisallowUnknownFields；201 返回 MemoryItemView。
- `GET /v1/memory/items?namespace=&scope_id=&kind=&status=&limit=&before=`——列表（namespace 缺失 400）+ next_cursor。
- `GET /v1/memory/items/{memory_id}`——单查（未知 404）。
- 错误：`invalid_request`（格式/枚举/长度）400、`memory_scope_not_found` 409。

### 6. SPA

`/memory` 路由 MemoryPage（AppShell 导航 +1）：五命名空间 tab、scope 过滤输入、记忆卡列表（kind 徽标/时效/来源数/敏感标）、详情展开、创建表单（命名空间→锚下拉或输入、kind、title、content、source_refs 多行、时效区间、sensitivity）——"记忆可查看、可显式写入"的 0.7.0 用户面。

### 7. 测试与验证

域 UT（锚矩阵：五命名空间 × 存在/不存在/ workspace 锚约束；字段校验矩阵）、memstore UT（CRUD/过滤/游标）、sqlite IT（v32 迁移 + 新旧库升级 + CRUD + 锚集成 + 并发唯一）、httpapi UT（三端点 + 写门 + 400/404/409 语义）、SPA 构建、真机冒烟（建锚→写五域→列表过滤→409 面）。

## 审校修订记录

- v0.3（2026-11-05）：二审 1 项 P2（一审 #2 修法的闭包残留）——**创建等待期间切换筛选被旧列表覆盖**：写入成功回调直接调 `reload()` 携带提交时闭包的旧 ns/scopeFilter。改为只发 `refreshTick` 信号，由依赖当前 `ns`/`scopeFilter`/`refreshTick` 的列表 effect 以**当次渲染**条件重查（无独立 reload 函数）。
- v0.2（2026-11-05）：一审 4 项（1 P1 + 3 P2）修复——
  1. **空数组规范化（#1 P1）**：`source_refs` 省略/空数组三层统一——域 Create 规范化非 nil（JSON `[]` 不出 `null`）；sqlite 读回 null 列兼容（v32 首版已落 null 行统一空数组）；契约视图 nil 兜底；SPA 显示 `?? []` 兜底。
  2. **创建后刷新口径（#2 P2，前端）**：刷新统一走**当前浏览条件**（`reload` 单源——tab + 过滤输入）——写入表单的 namespace/scope 不改写列表口径。
  3. **续页竞态（#3 P2，前端）**：`loadMore` 加代次检查（切域后迟到响应不追加——越域混入关闭）+ 在途限制（同游标不重复发）。
  4. **游标域内定位（#4 P2）**：sqlite 子查询与 memstore 匹配均限定 `namespace`——外域同 ID 作 before 一律空页（与未知 ID 同语义，越域存在性不泄漏）。

## 后果

- 五域记忆有了统一归属容器与最小读写面；0.7.1（策展状态机/时效/superseded）、0.7.2（FTS5 + 授权过滤）、0.7.3+（Checkpoint 引用、Bundle 载荷）在本件表结构与端口上递进，不再动基础迁移。
- 房间胶囊记忆保持事件流权威（不迁移、不复制）——两套形态并存由"共享域显式派生写入"衔接，自动抽取留 0.7.1 按策展纪律接入。
- workspace 锚定 "default" 是单工作区事实快照；0.10 多设备/0.7.5 Bundle 不受影响（导出范围按 namespace×scope 组合）。
