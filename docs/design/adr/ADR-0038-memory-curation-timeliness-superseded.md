# ADR-0038：0.7.1 记忆策展与纠错——接受、修订、否决与 superseded（0.7.1）

- 状态：Accepted（2026-11-05，随 0.7.1 实施；v0.2 一轮审校修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.2 MemoryItem 与策展——"抽取 → 候选 proposed → 验证来源、去重、冲突检查 → accepted；高可信明确指令可按预设政策自动接受"、"新旧矛盾不只按写入时间覆盖：区分纠错、事实随时间变化与并存异议；人工纠错具有明确版本记录；过期、撤回和 superseded 项不默认参与当前召回，历史查询仍能解释当时依据"）；[ADR-0037](ADR-0037-domained-memory-items.md)（0.7.0 基石——status/supersedes/edit_history 预留列本件直接启用，**零迁移**）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.1 唯一交付特性：记忆策展与纠错：来源、时效、接受、修订与 superseded）。

## 背景与问题

0.7.0 交付了分域 MemoryItem 的显式写入与隔离列表，但策展语义未启用：status 恒 active（无候选/终结态流转）、supersedes 恒空（无纠错链）、edit_history 恒空（修订无版本记录）、列表默认口径含已过期条目（valid_to 形同虚设）。RFC §7.2 要求"可纠错的派生知识"——候选须经策展接受、纠错留痕、被替代/否决/撤回的条目不再参与当前召回但历史可查。

## 决策

### 1. 状态机与终结态

```
            ┌── accept ──> active ── reject/withdraw（cause）──┐
 Create ──> proposed ── reject ────────────────────────────────┤──> superseded（终结，不可复活）
   │                                                          │
   └──（默认）active ── supersede（事务：旧终结 + 新条目）──────┘
```

- **superseded 是唯一终结态**，统一表达三种归宿（edit_history 的 cause 区分）：`reject`（候选否决）、`withdraw`（active 撤回）、`supersede`（被显式替代）。不再新增枚举值（0.7.0 契约 [active, proposed, superseded] 冻结不破）。
- **终结不可复活**：superseded 条目不接受 accept/revise/supersede/reject（409 `memory_terminal`——与 0.7.8 "旧包防复活"同一基调）。
- Create 写入面 += 可选 `status`（`active` 默认 | `proposed`）——"高可信明确指令"直接 active，不确定候选留 proposed（抽取器接线随后续小版本；0.7.1 的 proposed 由显式写入产生）。

### 2. 四个策展动作（版本与留痕纪律）

| 动作 | 端点 | 语义 | 版本 |
|---|---|---|---|
| 接受 | `POST /v1/memory/items/{id}/accept` | proposed → active；已 active **幂等返回**当前视图（策展重复确认无害） | 不递增（内容未变） |
| 否决/撤回 | `POST /v1/memory/items/{id}/reject` | proposed → superseded（cause=reject）/ active → superseded（cause=withdraw）；note 记原因 | 递增（状态是可追溯事实） |
| 原位修订 | `POST /v1/memory/items/{id}/revise` | title/content/source_refs/observed_at/valid_from/valid_to/sensitivity/confidence 可改；status 保持；**expected_version CAS** | 递增 + edit_history |
| 替代 | `POST /v1/memory/items/{id}/supersede` | **单事务**：旧 → superseded（cause=supersede，edit_history 留痕）+ 新条目创建（`supersedes=[旧 id]`；replacement 为完整创建命令，含自身 status 默认 active） | 旧递增；新从 v1 起 |

- **edit_history**（0.7.0 预留列启用）：`[{version, cause: revise|reject|withdraw|supersede|accept, note?, edited_by: "owner", occurred_at}]`——append-only，"人工纠错具有明确版本记录"；每次状态/内容变化恰追加一条。
- **supersede 事务性**：旧终结与新条目建行同事务（SQLite `SupersedeItem`；内存件分步 + 失败回滚补偿）——不存在"旧的死了新的没建"或反向窗口。
- **事实随时间变化 ≠ 纠错**：不删旧条目，revise 调整 valid_from/to 或新建有效期条目（0.7.0 字段天然表达）；并存异议 = 两条 active 并存（supersede 只用于显式替代）。

### 3. 时效口径：过期不默认参与召回

- 列表（status=active 口径）默认**排除已过期条目**（`valid_to != "" && valid_to < now`——RFC3339 **时刻比较**，非字典序，0.6.13 审校 #15 同教训）；`include_expired=true` 显式请求可见（历史查询仍能解释当时依据）。
- proposed/superseded 显式口径不过滤有效期（策展审视需要看全量候选与历史）。
- now 由 Service 时钟单源传入 Store（`ListFilter.Now`）——不引入全局时钟。
- **行为变更说明**：0.7.0 列表默认含过期 active 条目；0.7.1 起默认排除（RFC §7.2 明确"过期…不默认参与当前召回"）——CHANGELOG/发布说明标注。

### 4. 契约（apigen 冻结增量）

- `MemoryItemCreateRequest` += `status?: "active" | "proposed"`（默认 active）。
- `MemoryItemView` += `edit_history: MemoryEditRecordView[]`（0.7.0 起恒数组——null 列兼容同 0.7.0 一审 #1）。
- 四端点（均 owner 写门 + 严格拒收；错误族：400 `invalid_request` / 404 `memory_not_found` / 409 `memory_version_conflict`（CAS）| `memory_terminal`（终结态动作）| `memory_scope_not_found`（supersede replacement 锚））。
- `GET /v1/memory/items` += `include_expired?: boolean`（默认 false）。

### 5. 端口切片（Store 增量）

`CreateItem` 保持；+= `UpdateItem(ctx, expectedVersion, next Item) (Item, bool, error)`（CAS 单写事务：version 核对 + 全列替换 + edit_history 由域构造传入）与 `SupersedeItem(ctx, oldID string, expectedVersion int, oldNext Item, replacement Item) (Item, Item, bool, error)`（双动作同事务）。memstore/sqlite 双实现同语义。

### 6. SPA

`/memory` 页扩展：状态过滤（active/proposed/superseded，默认 active；proposed 候选区显式可见）+ 条目操作（接受/否决与撤回/修订表单/替代表单——替代预填旧条目内容）+ 详情展开显示版本链（edit_history 倒序）与 supersedes 链接 + 过期条目标记与 `include_expired` 开关。

### 7. 测试与验证

域 UT（状态机全矩阵 × 初始态、CAS、edit_history 恰一条/次、过期口径含边界 valid_to==now）、sqlite IT（CAS 并发、supersede 事务原子性——中间态不可见）、httpapi UT（四端点语义 + 写门 + 错误族）、SPA 构建、真机冒烟（proposed→accept/reject、revise 版本链、supersede 双动作、过期口径切换）、全量 vet/-race + it/st + 生成物幂等。

## 审校修订记录

- v0.2（2026-11-05）：一审 5 项（2 P1 + 3 P2）修复——
  1. **快照绑定 + 状态基准 CAS（#1 P1）**：Revise/Supersede 读快照后先核对请求版本与快照一致（不等 CAS——"读到旧值却以新版本号过检"的窗口关闭）；CAS 从纯版本升级为**版本 + 状态双基准**（`WHERE version=? AND status=?`）——并发撤回产生的同名版本不再让旧修订复活终结条目。
  2. **接受不被并发修订撤销（#2 P1）**：accept 维持不递增版本约定，双基准 CAS 使"proposed v1 接受成 active v1 后，基于 proposed 快照的陈旧修订"被拒（同名版本状态已变）；Accept 的 CAS 失败重读一次——已被并发接受则幂等收敛。
  3. **include_expired 编码（#3 P2，前端）**：API 客户端补 `include_expired` → URL 查询参数（此前勾选无效）。
  4. **时间语义一致（#4 P2）**：写入/修订/替代三时间字段**统一 UTC 规范化**（Parse→UTC Z 后缀——与 julianday 过滤同语义）；旧数据（如 `+15:00` 偏移使 julianday 返回 NULL）**保守包含**不误隐（宁多显不漏显）。
  5. **替代保留敏感度（#5 P2，前端）**：替代表单预填原 sensitivity 并**恒携带**（不默认降级 normal）；修订表单同开放敏感度修改。

## 后果

- 记忆从"只写不改"升级为**可策展、可纠错、可追溯**的派生知识：候选有归宿、修订有版本、替代有链、过期有口径、历史可解释。
- 0.7.2 召回（授权过滤 + FTS5）建立在 active + 未过期口径上（本件的默认过滤即召回前置纪律——"授权及有效期过滤先于排序、摘要与计数"）。
- superseded 不可复活为 0.7.8 删除传播与旧包防复活预设基调；抽取器（delivery.suggested_memory / 房间胶囊 → proposed 候选）在后续小版本按策展纪律接线，本件动作面已备。
