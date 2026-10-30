# ADR-0024：房间交办与 Dispatch 首切片（0.6.1）

- 状态：Accepted（2026-10-30，随 0.6.1 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§4.4–4.8 网关职责/寻址优先级/就绪与投递、§6.1 source_refs、§10.1 决策交办、§11.2 create_mission 与幂等约定、§11.2.1 RouteRequest/RouteDecision/DispatchReceipt 与 `(caller_ref, scope, idempotency_key)` 唯一约束；2026-10-30 v0.6 网关增补）、[ADR-0023](ADR-0023-mission-contract.md)（决策 6 Dispatch 排布裁定——本 ADR 兑现"首切片随 0.6.1"；Mission 契约与完备性单源——本 ADR 复用不重定义）、交付计划 v2.00（0.6.1：房间交办：来源引用及 `(room_id, task_id)` 唯一映射）

## 背景与问题

0.6.0 交付了契约事实源，但 Mission 与房间仍无关联：来源引用是自由字符串，
无法回答"这个房间任务卡是否已经交办过"。RFC 裁定：

1. **来源是事实**（§6.1）：`source_refs` 固定房间事件/文档修订/决策版本；
   房间交办须建立 `(room_id, task_id)` → Mission 的**唯一映射**——同一任务
   卡不得重复交办（重复即冲突可见，不是再建一份）。
2. **网关首切片**（§4.4/ADR-0023 决策 6）：入口后的确定性 Dispatch——
   解析（只预检不创建）→ 投递（复核后执行领域命令）→ 回执；幂等以
   `(caller_ref, scope, idempotency_key)` 唯一约束兜底：相同请求重放原
   回执、同键异载荷拒绝、重试不得变成新派单。

## 决策

### 1. 房间来源与唯一映射（migration v14 `mission_sources`）

```
mission_sources（(room_id, task_id) 主键 → mission_id，创建即映射）：
  同一房间任务卡至多交办一次——撞键 409 source_already_mapped（消息携带
  已映射 mission_id，可恢复）；映射为创建事实，修订不可改（换来源 = 新交办）
```

- `MissionCreate` 增可选结构化 `room_source {room_id, task_id}`（Revise
  **不接受**该字段——来源不可变）；`MissionView` 回显（无来源为 null）。
- 校验（SourceLookup 端口，app 装配注入房间域适配：EventReader 分页读全
  量事件 → `room.TasksOf` 纯函数重建 → 任务存在）：房间与任务卡均须存在；
  `source_refs` 保持自由引用串（引用校验本版只做结构化 room_source）。
- 直接 `POST /v1/missions` 与网关 dispatch 两条路径共用同一映射约束。

### 2. Dispatch 首切片（internal/dispatch，确定性最小实现）

**只做确定性路径**（RFC §4.5 前两级中 0.6.1 相关项）：结构化请求 →
解析（显式/房间缺省，无语义模型、无续接、无 IM 入口——后续小版本按
§13 阶段表接入）。`caller_ref` 固定 `owner`、`scope` 固定 `workspace`
（服务端从写门推导，请求体不得自称——§11.2.1 caller_context 规则）。

| 步骤 | 命令 | 语义 |
|---|---|---|
| 解析 | `POST /v1/dispatch/resolve` | 只解析与预检，**不创建任务**：房间/任务卡存在、映射未被占、owner（若指定）存在、契约内容合法 → `resolved`；任一不满足 → `blocked`（reason_code：room_not_found / task_not_found / source_already_mapped / agent_not_found / invalid_mission）。决策落库（`dispatch_decisions`：route_id `rd_`、decision_version、request_digest、请求全文 JSON、expires_at = 解析 +10 分钟）——决策是投递的输入事实，投递执行**决策存档的请求**而非客户端重发内容 |
| 投递 | `POST /v1/dispatch/dispatch` | 复核决策（未过期 + 当前状态仍满足预检——目标变化 409 `route_stale` 重新解析）→ 幂等检查 → 执行 `mission.Create`（含映射，同 0.6.0 单源）→ 回执落库（`dispatch_receipts`：receipt_id `dpr_`、(caller_ref, scope, idempotency_key) **UNIQUE**、request_digest、route_id、domain_command_id = mission_id、committed_at） |
| 查回执 | `GET /v1/dispatch/receipts?idempotency_key=` | 重放原投递结果；404 receipt_not_found 不等于领域动作未发生（描述如实标注） |

- **幂等语义**（§11.2.1）：同键同摘要 → 200 重放原回执（Binding 换人、
  契约演进都不改变"已提交"事实）；同键异摘要 → 409 `idempotency_conflict`；
  回执查询先于一切复核——已提交回执直接重放。
- **request_digest**：解析请求规范化 JSON 的 SHA-256（Go struct 序列化
  字段序确定）；投递与回执都携带，防"决策后改请求"。
- **两事务窗口**（权衡，如实登记）：Mission 创建（事务一）与回执落库
  （事务二）分属 mission/dispatch 两存储方法——崩溃于其间会留下"已建
  Mission、无回执"的孤儿：重试走幂等检查（无回执）→ 复核发现映射已占
  → `source_already_mapped` 回显已建 mission_id（可见恢复，不产生重复
  派单）。回执与领域提交同事务的紧耦合留待 0.6.x 统一写路径重构。
- **不做**（本版非目标）：语义选择（AT-17 另定）、续接/mission_update
  （§10.3，随执行域）、IM/事件入口、deadline/预算传递（budget 随契约
  事实，不由网关再加码）、受限 Agent token 作用域。

### 3. HTTP 契约（3 新端点 + mission 创建扩展；写面过生产写门）

| 端点 | 语义 | 主要失败 |
|---|---|---|
| `POST /v1/dispatch/resolve` | 预检（**恒 200**——blocked 也是决策不是错误） | 400 invalid_request（结构/枚举/必填） |
| `POST /v1/dispatch/dispatch` | 复核 + 幂等 + 建契约 + 回执 | 400 / 404 route_not_found / 409 route_stale、idempotency_conflict、source_already_mapped / 404 agent_not_found |
| `GET /v1/dispatch/receipts?idempotency_key=` | 回执查询 | 404 receipt_not_found |

### 4. UI

- 任务 Tab（房间内）：pending 任务卡新增「交办为 Mission」——跳转
  `/missions?room=<id>&task=<id>` 预填来源。
- MissionsPage 创建表单新增「房间来源」区（room_id + task_id，可空）：
  冲突 409 如实展示已映射 mission_id。

## 非目标（0.6.1 明确不做）

- 语义选择与候选评分（AT-17）；事项续接（mission_update §10.3）；IM/
  事件入口与多 caller 作用域；deadline/预算链（§11.2.1 budget_ref）；
  WorkItem/offer（0.6.8+）；执行启动（0.6.5——回执不表示执行）。
- decision_ref / context_manifest_ref 契约字段（仍随 0.6.2）。

## 测试策略（先行红→绿）

- **UT·mission**：room_source 校验（房间/任务不存在、映射冲突
  ErrSourceAlreadyMapped）、视图回显、修订不接受来源。
- **UT·dispatch**：resolve resolved/blocked 族（含 reason_code）；
  dispatch 幂等三态（重放/异载荷冲突/首投）；决策过期 route_stale；
  复核时状态变化（映射被占）route_stale；决策存档请求被执行（客户端
  重发内容不参与）。
- **IT·SQLite**：v14 新库 + v0.6.0 旧库升级；映射 UNIQUE 实测；回执
  (caller, scope, key) UNIQUE 并发恰一胜者。
- **API**：三端点 + mission 创建扩展全链（含 blocked 决策 200、幂等
  重放、冲突 409、写门）。
- 真机冒烟：房间建房→任务申报→resolve→dispatch→重放→同键异载→冲突
  映射→重启持久。

## 后果

- 房间任务卡与 Mission 建立可审计一对一关联；重复交办从"数据重复"变为
  "冲突可见"。
- dispatch 成为后续所有入口（IM、事件唤醒、Agent 委派）的统一前厅——
  0.6.1 只开 owner/结构化一条道，但幂等与决策-投递两段式骨架就此定型。
- 回执两事务窗口如实登记（恢复路径明确），不宣称跨存储原子。

## 修订记录

- v0.1 2026-10-30 初稿（0.6.1 开工——设计先行）。
- v0.2 2026-10-30 审校修复登记（独立审校"1 P1 + 4 P2，建议修复后定版"）：**(P1)** 幂等键改为**创建前原子认领两段式**（ClaimKey 绑定摘要 → 领域创建 → FinalizeReceipt 完结）——异摘要写入前退出零副作用、并发孪生恰一认领；两事务恢复窗口保留（pending 认领重驱动 + 映射撞键可见恢复），补入决策 2。**(P2#2)** 来源半填拦截（两项同时空/非空）。**(P2#3)** SourceLookup 适配跨页累积全量历史后统一重建（TasksOf 因果序单遍依赖前页上下文）。**(P2#4)** 幂等身份从路由改为**请求摘要**：同摘要异路由重放原回执；已提交回执查验先于决策时效判定（§11.2.1 次序——投递成功改变预检状态是常态）。**(P2#5)** resolve 入口复用直接创建的必填存在性 + 显式预算严格校验（两入口一致）。
