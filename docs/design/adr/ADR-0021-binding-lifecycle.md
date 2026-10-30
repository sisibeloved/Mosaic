# ADR-0021：Binding 任职状态机与交接（0.5.4）

- 状态：Accepted（2026-10-29，随 0.5.4 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.1 Binding 对象边界、§3.2 状态转移、§5.2 装配与准入/单 active/固定版本/交接、§11 prepare/activate/release 命令、§13.5"首版单 active"裁定、AT-13）、[ADR-0018](ADR-0018-agent-lifecycle.md)（状态机 + 事件流 + BEGIN IMMEDIATE 模式）、[ADR-0019](ADR-0019-slotspec-versioned-template.md)/[ADR-0020](ADR-0020-slotinstance-knowledge-namespace.md)（模板/实例——Binding 的引用对象）、交付计划 v2.00（0.5.4 唯一交付特性：Binding 任职管理：准备、激活、排空、释放与交接状态机）

## 背景与问题

0.5.2/0.5.3 交付了职责的定义与存在；0.5.4 交付**任职**：谁在承担某个职责
实例。RFC-0018 的裁定：

1. **状态机**（§3.2）：`preparing → ready → active → draining → released`；
   准备失败 `→ blocked → preparing`。每次换 Agent 创建新 Binding，旧
   Binding 留作审计；**排空后才能切换职责写入者**。
2. **单 active**（§3.1/§13.5）：首版每实例最多一位 active 任职者
   （AT-13 验收：并发下不出现两个 active）。
3. **固定版本**（§5.2）：Binding 创建时固定 `slot_spec_version`——模板
   后续演进不自动改变在途任职；升级 = 旧 Binding 排空释放 + 新 Binding。
4. **准入是前置装配步**（§5.2）：装配链"解析模板 → 核对工具/运行能力 →
   绑定授权 → 上下文清单 → 健康探测 → ready → 激活"产出 readiness
   report，必需项缺失 blocked——这是 **0.5.5** 的交付；0.5.4 只需为它
   留出挂靠点。

## 决策

### 1. 状态机与门（沿用 ADR-0018 承诺门模式）

| 动作 | 转移 | 门（0.5.4 生产缺省） |
|---|---|---|
| `prepare`（建任职） | 新行 preparing →（readiness 门）→ ready 或 blocked | `ReadinessChecker` 端口 nil = 恒通过 → 一次调用至 ready（preparing/blocked 仅在注入检查器时可见，0.5.5 起真实） |
| `activate` | ready → active | **单 active 不变量**：同实例已有 active Binding → 409 `active_binding_exists`（拒绝，不静默换人——§5.2） |
| `release`（在任） | active → draining →（排空门）→ released | `DrainChecker` 端口 nil = 在途工作恒空 → 两段事件与终态**同一事务一次提交**（ADR-0018 v0.2 #4 教训：中断不留 draining）；门非空停留 draining 并返回未处置清单，重试 release 只补终段 |
| `release`（未激活） | ready → released | 单段（从未在任，无物可排空） |
| （0.5.5 预留） | blocked → preparing → ready | 重试 prepared 任职；0.5.4 生产不可达，服务层转移与注入测试先行 |

- released 为**唯一终态**；一切转移经 BEGIN IMMEDIATE 临界区（读在写锁内，
  败者 409 可见拒绝——ADR-0018 v0.2 #1 全库约定）。
- **前置校验**：prepare/activate 时 Agent 必须存在且 `active`（suspended/
  retired 不接新任职——409 `agent_not_active`；§3.1"Agent 任职"语义的
  防御性收紧）；实例必须存在（404）；`slot_spec_version` 缺省取 prepare
  时该谱系最新版，显式指定须 ∈ [1, latest]（409 `invalid_spec_version`）。

### 2. 记录与投影（migration v11，§5.2 字段的 0.5.4 子集）

```
slot_bindings（投影，同 agent_events 模式）：
  binding_id（bd_ 生成式）、agent_id、slot_instance_id、
  slot_spec_version（固定）、scope（随实例）、status、
  handoff_note（释放时交接备注，≤4KB）、created_at、updated_at
binding_events（append-only 流）：
  seq 全表单调 / binding_id / event_type{created,status_changed} /
  payload{...} / occurred_at——建行与 created 事件同事务
idx_slot_bindings_single_active：slot_bindings(slot_instance_id)
  WHERE status='active' 的**部分唯一索引**——单 active 由库级约束兜底
  （并发激活恰一胜者，AT-13 的存储层保险）
```

§5.2 的 `effective_grants、tool_bindings、context_policy_version、
concurrency_limit` 是 0.5.5 权限交集/工具绑定的产物——**本版不落列**
（0.5.5 迁移增补），避免空壳字段伪装能力。

### 3. 交接（0.5.4 交付交接位，不交付交接包本体）

"换人时保留 SlotInstance 的知识、交付和在办事项，新 Binding 读取交接包
并承接"（§5.2）——知识本体（0.7）与在办清单（0.6）尚不存在。0.5.4 交付：
release 接受可选 `handoff_note`（记入 status_changed 事件与行投影），
Binding 暴露该字段；接续方经实例的 Binding 列表（创建序）读取最近一次
released 的交接备注。自动汇编的交接包随 0.6/0.7 落地。

### 4. API 面（契约冻结；§11 命令对齐）

- `POST /v1/slots/bindings`（= prepare_binding）：`{slot_instance_id,
  agent_id, slot_spec_version?}` → 201 BindingView（ready）；404
  `instance_not_found`/`agent_not_found`；409 `agent_not_active`/
  `invalid_spec_version`。
- `POST /v1/slots/bindings/{id}/activate`：→ 200；409
  `invalid_transition` / `active_binding_exists`。
- `POST /v1/slots/bindings/{id}/release`：`{handoff_note?}` →
  `ReleaseResult{binding, outstanding}`；409 `invalid_transition`。
- `GET /v1/slots/bindings?slot_instance_id=`（过滤可选，创建序）、
  `GET /v1/slots/bindings/{id}`（404 `binding_not_found`）、
  `GET /v1/slots/bindings/{id}/events`（只读审计面）。
- 写面过生产写门；Agent 状态检查经 `AgentLookup` 端口注入身份域服务
  （slot 不 import personalagent——依赖方向与 harness 适配器同口径）。

## 非目标（0.5.4 明确不做）

- 不做能力核对、权限交集、tool_bindings、readiness report（0.5.5——
  ReadinessChecker 端口已就位）。
- 不做排空的实际在途工作处置（无 Mission/WorkItem 域——DrainChecker
  恒空；0.6 注入）。
- 不做交接包自动汇编（0.6/0.7）；不做 Binding 删除/归档（released 留审计，
  §3.2"旧 Binding 留作审计"）；不做多 active / 共享任职（§13.5 首版裁定）。
- 不做模板版本升级的自动切换（§5.2：升级 = 显式新建 Binding + 旧释放）。

## 测试策略（先行红→绿）

| 层 | 用例 |
|---|---|
| UT slot | prepare happy（默认钉最新版/显式钉旧版/超 latest 拒）；agent suspended/retired 拒（prepare 与 activate 两时点）；activate → active；同实例第二 activate → ErrActiveBindingExists；release ready 单段；release active 两段一事务 + 事件序；注入 Drain 非空 → 停留 draining + outstanding → 清空重试补终段（draining 事件不重复）；注入 Readiness 失败 → blocked（+服务层重试 → ready，0.5.5 预留）；released 终态全拒；事件流完整（created + 各转移）；并发激活恰一胜者 |
| IT sqlite | v11 新库（含部分唯一索引）；v0.5.3 旧库升级；**部分唯一索引并发实测**：两 Binding 8 路交替激活同实例 → active 恒 ≤1、恰一在任；release 取消上下文整体回滚；事件与投影一致 |
| API httpapi | prepare 201 形状与四类错误码；activate 冲突 409 active_binding_exists；release 携交接备注 → 视图回读；列表过滤/详情/事件/404；写门；身份联动（挂起 Agent 后 prepare 409） |

## 后果

- 正面：职责有了"谁在承担"的可审计事实——单 active 由库级约束兜底；
  0.5.5 准入与 0.6 排空/交接包的两个挂靠点（Readiness/Drain 端口）就位；
  任职史完整可回放。
- 代价：每次换人新建 Binding 行（审计友好，行数线性于任职次数）；
  activate 需查同实例 active（部分唯一索引使检查退化为索引插入竞争）。
- 放弃方案：实例上直接更新任职者字段（无审计、无法表达 preparing/
  draining 中间态、丢"旧 Binding 留作审计"）；多 active（违反 §13.5
  首版裁定）；本版落 grants 空列（伪装 0.5.5 能力）；release 两段分开
  提交（0.5.1 审校 P2#4 同类缺陷不复现）。

## 修订记录

| 版本 | 日期 | 说明 |
|---|---|---|
| v0.2 | 2026-10-29 | 审校修订（6 项 P2）：#3 prepare/retry 改单事务（建行直接落终态 + created + preparing→target 两事件一次提交；blocked 重试同口径——中断/失败零残留，不留无法推进的 preparing 死记录；取消回滚 IT 钉住）；#4 交接备注与状态/事件同事务入投影（ready 直释与 draining 补终段两路径——返回值/详情/事件三处一致，UT+冒烟钉住）；#5 版本三态（省略钉最新 / 显式 <1 → 400 不静默换最新 / 显式区间内钉住——端点与服务层双层拒）；#6 事件序列化改 BindingEvent 主体（binding_id 承载，不再借 AgentEvent.agent_id）；#1 前端最近交接按释放时间（updated_at）取最新（接口保持创建序）；#2 前端未加载/失败时摘要中性提示（不误报"空缺"） |
| v0.1 | 2026-10-29 | 初稿（随 0.5.4 实施） |
