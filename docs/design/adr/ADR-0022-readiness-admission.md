# ADR-0022：职责准入——能力核对、权限交集与 readiness report（0.5.5）

- 状态：Accepted（2026-10-30，随 0.5.5 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§5.2 装配与准入/权限交集/blocked 语义、§13.1"模板只描述能力要求"、AT-07、AT-13）、[ADR-0021](ADR-0021-binding-lifecycle.md)（ReadinessChecker 端口预埋——本 ADR 实装；blocked → preparing 重试路径服务层已先行）、[ADR-0019](ADR-0019-slotspec-versioned-template.md)（tools/context required-optional 分层——本 ADR 消费）、[ADR-0017](ADR-0017-personal-agent-identity.md)（Agent 身份——授权对象）、交付计划 v2.00（0.5.5 唯一交付特性：职责准入：能力核对、权限交集与 readiness report）

## 背景与问题

0.5.4 交付了任职状态机：`preparing →（readiness 门）→ ready/blocked`，但
生产装配的门是 `nil = 恒通过`——任何 Agent 都能 ready 任何职责。RFC-0018
§5.2 裁定准入要核对三件事：

1. **能力核对**：模板声明的必需能力（skills / tools.required /
   context.required / runtime.required）缺失 → **blocked**，缺失清单可
   见；选配缺失 → **降级有报告**，不阻塞（AT-07：必需缺失 blocked、可选
   降级有报告、不得偷偷换执行者或扩大权限）。
2. **权限交集**：有效权限 = 用户授权 ∩ 职责范围（本版两个输入；Workspace
   政策 / Mission 范围 / Runtime 强制能力随 0.6+ 并入同一交集函数）。
   Slot 写了工具名不等于获得权限；Agent 自己不能自授权（RFC §5.2）。
3. **readiness report**：装配产出报告——满足项、缺失项、可选降级、有效
   权限；支持**试算**（未建任职先看报告）与 **blocked 重试**（补授权后
   blocked → preparing → ready，ADR-0021 预留路径暴露为 HTTP）。

缺失的输入是**用户授权侧的数据**：Agent 有什么技能、可用什么工具、可访问
哪些上下文域、具备什么运行能力、工作区权限到哪级。本 ADR 定义它。

## 决策

### 1. AgentGrant 授权档案（用户授权侧，migration v12）

```
slot_agent_grants（agent_id 主键，一 Agent 一档；upsert 全量替换）：
  skills               JSON [{ref, version}]   版本化工作方法（同 SkillRef）
  tools                JSON [string]           工具授权
  context              JSON [string]           上下文/知识域授权
  runtime_capabilities JSON [string]           运行能力（execute/cancel/…）
  workspace_access     none | read | read_write
  updated_at
```

- **无档案 = 空授权**（安全缺省）：一切必需项缺失 → blocked；模板未声明
  任何要求（skills/tools/context/runtime 均缺省）的职责仍 ready——准入
  只核对模板声明的要求，不制造隐式门槛。
- PUT 全量替换（同 RuntimeUpdateRequest 语义：显式空数组 = 清空，无三态
  歧义）；Agent 须存在（404 `agent_not_found`），任意生命周期状态可存档
  （suspended Agent 的授权是数据，不是行为许可——行为许可是 prepare/
  activate 两时点的 `agent_not_active` 校验，0.5.4 已在）。
- **不建 grant 事件流**（权衡）：单用户 owner-token 本地应用，写操作即
  owner 本人；grant 是声明式授权数据而非状态机。0.9/0.10 若引入环境
  管理员或多设备同步需要授权审计时再补——本版不落空壳。

### 2. EvaluateReadiness 纯函数（交集单源）

报告、门、试算共用一个纯函数 `EvaluateReadiness(spec SpecContent, grant
AgentGrant) ReadinessReport`——无 IO、无时钟，表驱动可测：

| 模板要求 | 判定 | 缺失呈现 |
|---|---|---|
| skills（模板列出即必需） | grant 含同 `ref` 且 `version ≥` 模板引用版 | `skill:{ref}@v{N}` |
| tools.required / context.required | grant 对应清单含该名 | `tool:{name}` / `context:{name}` |
| tools.optional / context.optional | 同上 | 缺失 → **degraded**（`tool:{name}（选配缺失，降级）`），不阻塞 |
| runtime.required | grant.runtime_capabilities 含该名 | `runtime:{name}` |
| runtime.workspace_access | 格栅 none < read < read_write；grant 级别 ≥ 模板要求即过 | `workspace_access:{要求}（授权 {实际}）` |

- `ready = 缺失清单为空`；**有效权限 = min(授权级, 模板要求)**——授权
  read_write 而职责只要 read 时生效 read（交集语义，不是授权直通）。
- skill 版本取 `≥`（工作方法向后兼容），不钉死精确版本。
- 报告字段：`ready / satisfied / missing / degraded / effective_workspace_
  access`——对应 RFC §5.2"满足项、缺失项、可选降级"（"不能开展的动作"
  由 missing+degraded 聚合表达）。
- §5.2 装配链中的"绑定数据域与凭据引用 / 上下文清单 / 健康探测"是
  0.6+（Mission/Runtime 域）步骤——本版交集输入为用户授权 ∩ 职责范围
  两项，函数留扩展位。

### 3. ReadinessChecker 端口升级 + GrantReadinessChecker 实装

- 端口签名升级：`Check(ctx, candidate Binding) (ReadinessReport, error)`
  （原 `(bool, []string, error)` 只有缺失维度，装不下降级与有效权限）。
  nil 仍 = 恒通过（既有测试与"无要求职责"路径不变）。
- `slot.NewGrantReadinessChecker(store)`：按 candidate 的实例 → 谱系 →
  钉住版本内容 → grant（无档 = 空）→ EvaluateReadiness。app 装配注入，
  生产从此有真实 blocked。
- prepare/retry 的 blocked 事件 payload 记 `missing` 与 `degraded` 两键
  （空清单省键）——审计面可见为什么被拒、降了什么级。
- **不自动换人不自动扩权**：blocked 就是 blocked（AT-07）——不回落到
  其他 Agent、不隐式放大授权；修复路径只有一条：owner 改授权档案 →
  重试。
- **激活时点复核**（v0.2，审校 P1#2）：ready 只代表 prepare 时点就绪，
  授权此后可被撤销——activate 按钉住版本重算交集，必需项缺失 409
  `readiness_not_met` 可见拒绝（与 Agent active 两时点校验同口径；
  RFC §11"必需能力缺失则拒绝激活"的落实）。

### 4. HTTP 契约（4 端点）

| 端点 | 语义 | 失败 |
|---|---|---|
| `GET /v1/slots/grants/{agent_id}` | 授权档案；未设置 404 `grant_not_found` | 404 / 503 |
| `PUT /v1/slots/grants/{agent_id}` | 全量替换（写门）；非法清单/枚举 400 `invalid_grant` | 400 / 404 / 503 |
| `GET /v1/slots/instances/{id}/readiness?agent_id=&slot_spec_version=` | **试算**（只读无副作用；not-ready 也是 200——报告不是错误）；version 缺省 = 谱系最新，显式须 ∈ [1, latest] | 404 / 409 `invalid_spec_version` |
| `POST /v1/slots/bindings/{id}/retry` | blocked → preparing →（门）→ ready/blocked（单事务，ADR-0021 v0.2 #3 路径暴露）；非 blocked 409 `invalid_transition` | 404 / 409 / 503 |

### 5. 前端（设置页职责区）

- **Agent 授权档案**块：Agent 下拉 → 档案表单（skills `ref@version` 行、
  tools/context/运行能力行、workspace_access 下拉）→ 保存；未设置态与
  读取失败态区分（404 grant_not_found = 可编辑空表单，非错误）。
- 建任职行加**准入试算**：选 Agent（+钉版本）→ 报告展示就绪/缺失/降级/
  有效权限——任职仍是显式动作，试算不建记录。
- blocked 任职行展示缺失清单（事件 payload）+ **重试准备**按钮。

## 非目标（0.5.5 明确不做）

- 凭据引用 / tool_bindings 实连接、健康探测、context_policy_version、
  concurrency_limit（§5.2 字段，0.6+ Runtime/Mission 域随真连接落列）。
- Workspace 政策 / Mission 范围 / Runtime 强制能力并入交集（同一
  EvaluateReadiness 函数扩展，本版输入两项）。
- grant 事件流与授权审计（见决策 1 权衡）。
- 强隔离 Runtime 匹配与强制执行（RFC §13.1——**P1 出口要求，随 0.6 Runtime/Mission 域交付**：能力不可被强制落实时准入失败，不得仅凭提示词声明隔离；P4/0.9 仅在环境维护或替换后复验这些条件，见 RFC §15 P1 行与 P4 行）。0.5.5 的交集以用户声明授权为输入，不构成隔离保证。

## 测试策略（先行红→绿）

- **UT·纯函数表驱动**：四类核对 × 满足/缺失/降级；workspace 格栅六组合；
  无档案空授权；无要求模板；skill 版本 ≥ 边界。
- **UT·域服务**：PutGrant 校验族（模式/上限/枚举/重复 ref/未知 Agent）；
  GrantReadinessChecker 经 prepare 落 blocked（missing+degraded 入事件）；
  补授权后 retry → ready → activate 全链；试算版本三态。
- **IT·SQLite**：v12 新库建表；v11 旧库升级（marker 切分）；grant upsert
  幂等与并发；blocked→retry 单事务（中断零残留同口径）。
- **API**：四端点全链 + AT-07 场景（必需工具缺失 blocked → 授权 → retry
  ready → 激活；选配降级出现在报告与事件；显式越界版本 409）。
- 真机冒烟：AT-07 全链 + 单 active 回归 + 重启持久。

## 后果

- 生产 prepare 从"恒 ready"变为真实准入：**既有无要求职责不受影响**
  （模板未声明要求即无门）；有要求的职责在补授权前 blocked——这是
  AT-07 要的行为，发布说明显著标注。
- 授权数据成为任职前置：UI 需引导"先授权、再任职"；报告试算降低来回
  成本。
- EvaluateReadiness 成为全系统权限交集的单源——0.6+ 并入更多输入时，
  blocked 语义、报告形状与审计 payload 不变，只加判定行。

## 修订记录

- v0.1 2026-10-30 初稿（设计先行——0.5.5 开发指令）。
- v0.2 2026-10-30 审校修复登记（独立审校"2 P1 + 4 P2，建议修复后定版"）：**(P1#1)** 授权表单归属锁——表单记录所属 Agent、加载期间禁编辑禁保存、保存前核对归属（防切档残留内容覆盖他人授权）。**(P1#2)** 激活两时点准入复核——ready 只代表 prepare 时点就绪，激活按钉住版本重算交集，必需项缺失 409 `readiness_not_met` 可见拒绝（与 Agent active 两时点校验同口径）。**(P2#3)** 试算请求序号守卫（迟到响应丢弃）。**(P2#4)** 钉版本解析前后端同源：空 = 最新，非空必须正整数（试算/建任职共用）。**(P2#5)** 强隔离准入阶段归属更正——RFC-0018 §13.1/§15 P1 出口即验收 AT-07 强制隔离（能力不可强制落实 → 拒绝激活），由 0.6 Runtime/Mission 域交付；P4（0.9）只做环境变更后复验（v0.1 误归 P4）。**(P2#6)** 授权 PUT 必填字段存在性检查——缺键或 null 一律 400，半体 PUT 不清空既有授权（显式空数组才是清空语义）。
