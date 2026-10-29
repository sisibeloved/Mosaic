# 特性设计说明书（RFC 提案）：RFC-0018 长期 Agent、Slot 与圆桌到 Swarm 的协作闭环

**状态 (Status):** Approved（2026-10-28 负责人指令："按新的项目计划，设计和测试先行，开发0.5.0版本"——批准本设计为基线；实施范围与顺序按交付计划 v2.00 版本映射分阶段推进，各阶段验收以本文 §13 为准）

**创建日期 (Created):** 2026-10-28

**更新日期 (Updated):** 2026-10-28（v0.4 批准定稿）

**作者 (Authors):** Mosaic 项目组 / Codex

**相关 Issue/PR:** 尚未建立

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| 上游输入 | [讨论圆桌与 Swarm 衔接](chatgpt-conversation://6ae16fe6-7a50-83e8-a508-df53e8373021)（ChatGPT 内部引用，普通浏览器或仓库文档查看器可能无法打开）；尤其是用户关于自管 Agent、独立生命周期、可迁移记忆与上下文、Runtime 与 Slot 的四点要求 |
| 输入边界 | 本次可读取的是聊天引用附带的截断预览，未取得完整聊天；本文将可见的明确要求转成设计，对新增机制给出提案，不将未读部分推定为已决事项 |
| 开放问题来源 | 无对应架构 §11.5 清单项；本提案来自上述产品讨论，既有技术类 OQ 已由先前 RFC 承接 |
| 代码基线 | v0.4.1，`f9346c3aee790d05b2e09985111dcd740f0828c4`；现状以本地代码核查为准 |
| 范围 | 个人本地版的架构演进、领域契约、迁移策略与分阶段验收；未来远程运行保留接口 |
| 与既有文档关系 | 扩展 RFC-0002、0007、0008、0009、0010、0012，承接 ADR-0013 稳定身份及 ADR-0014 独立资产事件流；具体修订见 §2.2 |
| 生效条件 | 已获批准（见状态行）。各领域版本（0.5–0.9）落地时按 §2.2 同步修订对应基线，并按 §13 分阶段验收；批准不等于已实现，现行实现以各小版本交付为准 |

## 0.1 核心决定

Mosaic 自己管理长期 Agent 的身份、记忆、承诺和运行生命周期。Agent 通过 Slot 承接职责，在 Room 中讨论并形成决策，由 Mission 保存执行契约，负责人按需组织 Swarm，结果回到原讨论及任务中心验收。外部封闭 Personal Agent 的互联不作为前置依赖；已有 CLI、模型和 Harness 继续提供执行能力。

“即嵌即走”定义为：一个 Agent 可在能力与授权满足时接入版本化 Slot；更换 Agent、Harness 或环境后，可从持久任务状态、授权记忆与交接包恢复工作。它不承诺跨模型保留相同隐式推理、无损搬运 CLI 内部会话，也不承诺零准备时间。

# 1. 目标、边界与用户体验

## 1.1 需求追踪

| 编号 | 明确要求 | 本文落实 | 验收入口 |
|---|---|---|---|
| R1 | 现阶段走 Mosaic 自管长期 Agent 路线 | 身份与协作控制归 Mosaic；模型调用仍可交给 Harness | AT-01、AT-11 |
| R2 | Agent 生命周期跳出 Room | 独立 Agent、Goal、Mission、Binding、Session、Runtime；Room 只保有成员及讨论事实 | AT-01、AT-02、AT-03 |
| R3 | 记忆与上下文可迁移，参考最新实践 | 分域记忆、来源与时效、Context Receipt、Checkpoint、可校验迁移包 | AT-04、AT-05、AT-06 |
| R4 | Runtime 管理、可选环境管理员、职责与工具提炼为 Slot | SlotSpec / Binding / Environment / Runtime 分离；受限环境维护职责 | AT-07、AT-08、AT-09 |
| R5 | 圆桌自然衔接负责人带领的 Swarm | 交办卡、Mission 契约、长期协作者与临时 Worker 两种委派、产物验收 | AT-10、AT-11、AT-12 |

## 1.2 用户可见的闭环

1. 房间里形成一个决定或明确工作请求，选择“交给 Agent”，指定负责人、目标和验收条件。
2. 系统生成交办卡，沿用原讨论、来源与具名异议；缺少关键范围时保留草稿。
3. 负责人独立工作，必要时邀请另一位长期 Agent 或创建临时 Worker；用户仍可在原房间继续讨论。
4. 任务中心持续展示负责人、下一步、等待原因和产物。用户可补充要求、暂停或取消。
5. 负责人提交交付包，授权验收人接受或退回。新证据触及原决定的重开条件时，返回讨论。

长期存在不等于模型持续运行。没有工作时保存身份、队列、记忆和唤醒条件；执行资源按需启动。桌面应用关闭或机器休眠期间不会继续本地执行；重启后恢复调度，离线错过的唤醒按规则合并处理。

## 1.3 非目标

- 等待 Grok Bot、Dots、Duet、Muse 或灵光开放跨产品委派；本文不依赖对这些产品封闭内部机制的推测。
- 重建各家 Harness、代理所有模型调用、统一不同模型的内部上下文窗口。
- 首版引入多租户、分布式共识、跨组织 Agent 市场或强制云端常驻。
- 自动把每次讨论收束变成执行任务，或把 Agent 的口头承诺当作已启动的工作。
- 将全部历史、私有记忆、登录凭据和机器镜像作为一个不可分割对象迁移。

# 2. 现有基础与需要改变的边界

## 2.1 已核查的实现

以下是代码基线的实际能力，不等于本 RFC 的目标能力已具备。

| 当前基础 | 代码 / 规范依据 | 可复用部分与缺口 |
|---|---|---|
| 稳定 BotID 与路径重绑 | [ADR-0013](../adr/ADR-0013-stable-bot-identity.md)、[registry.go](../../../internal/harness/registry.go) | 已分离身份与路径；身份登记仍附着 Executable，需提升为独立 Agent |
| 提出方、负责人分离的任务清单 | [tasklist.go](../../../internal/room/tasklist.go) 的 `TaskItem`、`TasksOf` | 是房间消息的确定性投影；`mosaic-todo` 申报及 delivered 不能代替执行调度和成果验收 |
| 单 Agent 独立 Run | [runs.go](../../../internal/room/runs.go) 的 `runTask`、`executeRun`、`RecoverRuns` | 脱离反应波，但仍按 Room 存储并依赖房间座位；重启转 unknown，不自动重跑副作用 |
| 执行上下文 | 同文件 `runContext` | 只携任务指令、任务 ID 和最近 10 条消息；提交仍为 `agent.KindGenerate` |
| 会话隔离 | [supervisor.go](../../../internal/agent/supervisor.go) 的 `sessionKey` | 当前按 ProfileID × RoomID 隔离；尚无 Mission / Binding 级语义会话 |
| 收束与双平面记忆 | [closure.go](../../../internal/room/closure.go)、[context.go](../../../internal/contextx/context.go)、[RFC-0007](2026-08-25-rfc-0007-context-memory.md) | 已有胶囊、召回、上下文回执与记忆编辑；当前设计将共享记忆归房间，CLI 会话归 Harness |
| 监控唤醒 | [monitor.go](../../../internal/monitor/monitor.go)、[monitorwire.go](../../../internal/app/monitorwire.go) | 可复用变化检测；唤醒入口与 Run 状态仍使用 room_id |
| 外部命令身份门 | [service.go](../../../internal/room/service.go) 的 `ExecuteCommand` | 只允许具名 human/system；新增 Agent 委派须走专用授权入口，不能直接放开 actor 字符串 |
| 独立资产事件流 | [ADR-0014](../adr/ADR-0014-document-event-stream.md) | 文档已脱离 Room 生命周期；可沿用按对象归属权威事实的办法 |

另外，`internal/contextx/context.go` 的 `TasklistProtocol` 仍提示“没有后台执行”。引入 Mission 后须按执行能力与任务类型提供提示词，避免新旧行为互相矛盾。

## 2.2 规范兼容与修订清单

| 原约定 | 本提案的变化 | 保留项 / 生效方式 |
|---|---|---|
| Room 为讨论中心，文档另有权威日志 | Agent、Mission、Binding、Runtime、长期记忆也有独立权威状态 | 讨论事实仍归 Room；按对象分流，不把多份镜像当共同权威 |
| RFC-0007 §7.4：Memory 归房间 | 增加 Agent、Workspace、Goal/Mission 和职责实例的记忆域 | 原 Room 记忆不自动变成 Agent 私有记忆；跨域复制须记录授权与来源 |
| RFC-0007 / OQ-16：Session 归 Harness | Mosaic 新增语义 Session 与可移植 Checkpoint；原生线程仅作可选加速 | Harness 继续负责自己的 token 窗口与内部压缩；Mosaic 不读取隐藏推理 |
| RFC-0002：Generate 等讨论任务，Run 仍在 Room | 增加能力协商后的执行任务类型、委派回调和 runtime/session 寻址 | 旧适配器继续讨论；不支持执行时明确报能力缺失 |
| 架构 AR-003：Agent 不能直接调用 Agent | 允许通过 Mission 的授权入口请求委派，系统持久化并调度 | Agent 仍不能直接控制另一 Agent 的进程、身份或私有会话；圆桌发言仍由 Room 协议仲裁 |
| ADR-0013：BotID 附着可执行项 | 独立 AgentID，保留 BotID / ParticipantID / ProfileID 的映射 | 不重写旧事件，不以显示名猜测合并身份 |
| RFC-0012：tasklist 为房间承诺投影 | 显式提升为 Mission 后，由 Mission 管理执行状态 | 旧 tasklist 格式及旧终态保持原义，不自动批量启动 |
| RFC-0008/0009/0010：授权、工具、数据生命周期 | 增加 Binding 权限交集、迁移、派生记忆删除、运行回执 | 沿用 owner 写门、最小权限和删除留痕原则；不导出密钥 |

批准本 RFC 时应同步修订架构说明书 §3（含 AR-003）/§6.3/§6.4/§6.5 与上述 RFC 的相应边界。现阶段仅登记提案链接，不将 Draft 伪装为已生效基线。

# 3. 概念、所有权与生命周期

## 3.1 对象边界

| 对象 | 持有的状态 | 生命周期 / 关系 |
|---|---|---|
| Workspace | 用户控制域、政策、资源、共享知识 | 个人版对应本地用户的数据域，先不新增多租户机制 |
| Agent | 稳定 ID、公开人格配置、私有记忆入口、收件箱、责任索引 | Workspace 所有；跨房间、模型、环境存在；责任索引从 Mission / Binding 派生 |
| SlotSpec | 职责、技能引用、工具与环境需求、上下文要求、交付规范 | 不可变版本；可复用模板，不含在办任务与私有凭据 |
| SlotInstance | 某作用域内的一份具体职责、职责知识入口 | Workspace / Goal / Mission 持有；同一模板可实例化多份；换人后实例保留 |
| Binding | Agent 任职 SlotInstance 的版本、实际授权、并发约束、交接状态 | Agent 与职责的任职记录；独立于环境分配；首版每实例最多一位 active 任职者 |
| Goal | 长期目标、衡量方式、负责人、周期、等待策略 | 可含多次 Mission；可无结束日期，但必须有复查或停止条件 |
| Mission | 可验收的一次交办、负责人、范围、预算、工作图、交付与验收 | 可不隶属 Goal；可关联多个 Room；归 Workspace，不能由房间删除级联销毁 |
| WorkItem | Mission 内的子任务、依赖、承接者、进度与结果 | 执行图节点；依赖边必须无环；与旧 Room TaskItem 分开命名 |
| Room / Membership | 共同讨论、来源、决定、成员关系 | Room 只拥有参与关系；退出/关闭房间不删除 Agent，不取消独立 Mission |
| Session | 某 Agent 在某作用域与任职下的语义工作上下文 | 按 Agent × scope × Binding 隔离；可压缩、分支、关闭；原生句柄不作主键 |
| Run / Attempt | 一次具体尝试、输入版本、Runtime、动作与结果回执 | 有限生命周期；每次重试用新 RunID，保留 WorkItemID；本文以 Run 作为 Attempt |
| Environment | 工作目录、依赖、数据卷、工具配置、资源与连接要求 | 可共享或专用；独立持久化；删除前处理所有租用者和需保留文件 |
| Runtime | 一份可执行实例的 Harness、主机、能力与健康状态 | 绑定 Environment，可启动/休眠/排空/重建；一个 Agent 可使用多个实例 |
| RuntimeLease | Run 对 Runtime 的限时占用、租约代次、资源限额 | 续租或到期；围栏令牌用于拒绝旧实例提交，不代表能撤销外部已发生动作 |
| Worker | 为 Mission 临时创建的执行身份与最小上下文 | 不默认拥有长期人格/私有记忆；完成回收；晋升长期 Agent 必须显式创建及转移 |

这里的 Slot 是“职责槽位”，不同于既有架构中讨论发言顺序的 slot。新接口统一使用 `slot_spec_id` / `slot_instance_id`，旧发言槽位不改义。

“主 Agent”是 Mission 的 accountable owner；“子 Agent”可能是长期协作者，也可能是临时 Worker，不设固定身份等级。一个 Agent 可以同时负责 A、协助 B、参与 C 的讨论。跨 Mission 可并行，首版同一 Session 串行，同一可写工作区须独占租约或分出独立工作区。

## 3.2 状态与转移约束

| 对象 | 状态与主要转移 | 关键约束 |
|---|---|---|
| Agent | active ↔ suspended；active/suspended → retiring → retired | active 不代表正在运行；retiring 停止接新任务，处置所有承诺后才能 retired |
| Binding | preparing → ready → active → draining → released；准备失败 → blocked → preparing | 每次换 Agent 创建新 Binding；旧 Binding 留作审计；排空后才能切换职责写入者 |
| Goal | active ↔ paused；active → achieved / canceled | 子 Mission 完成不自动证明长期目标达成；暂停停止新调度并显式处理在途任务 |
| Mission | draft → ready → running；running ↔ waiting；running/waiting → paused → ready；running → reviewing → accepted | reviewing 可退回 ready；非终态可进入 canceling → canceled 或失败处置后 failed；accepted/canceled/failed 不被迟到回调翻转 |
| WorkItem | offered → accepted → queued → running → submitted → accepted_result | offered 可 declined；进行中可 waiting/blocked、取消或失败；submitted 退回后重新排队 |
| Run | queued → starting → running → succeeded / failed / canceled / unknown | 旧房间 requested/completed 通过兼容映射展示，不重写；unknown 先核验副作用，不能直接当失败重试 |
| Runtime | discovered → provisioning → ready ↔ busy；ready/busy → draining → stopped；故障 → unhealthy → provisioning | busy 可表示仍有容量；draining 不接新租约；同一 Runtime 可多 Run，但资源上限与工作区隔离必须满足 |
| Session | open → checkpointed → open；open/checkpointed → closed | checkpointed 表示可恢复边界；是否支持原生恢复另行报告 |

取消和暂停都不能抹去既有动作。暂停以持久状态先阻止新调度，再请求在途 Run 在安全边界停止；无法安全终止或结果不可确认时显示待核验。Mission 只有所有子项停止、交付或已明确处置，才进入 canceled。新要求进入带版本的契约修订；在途 Run 不静默换输入，越过安全边界的修订应停止并重排。

## 3.3 必须保持的不变量

- 每个 Mission 恰有一个 accountable owner 或显式 `unassigned`；退役交接必须记录新负责人及承接回执。
- Room 暂停只暂停讨论及房间发布；Mission 的执行状态独立。要同时暂停任务，使用任务操作。
- 取消发给长期 Agent B 的工作项，只撤销该工作项授权与租约，不关闭 B 的其他任务或身份。
- 同一 WorkItem 的有效执行代次唯一；可存在旧进程，但旧代次结果不更新当前状态。
- 执行成功、成果提交、验收通过是三个事实；模型说“完成”不替代后两者。
- 共享知识、职责知识与 Mission 产物不随 Agent 退役消失；私有记忆不因职责换人自动转交。

# 4. 总体架构与部署

## 4.1 逻辑结构

延续模块化单体、SQLite、本地进程与逐家 Harness 适配。下图是目标结构，节点既包括现有组件，也包括拟新增模块；不要求拆成微服务。

```mermaid
graph TB
  subgraph Mosaic["Mosaic 本地应用"]
    subgraph Presentation["交互组件"]
      UI["Web / 桌面界面"]
      Gateway["HTTP / Agent 命令与授权入口"]
    end
    subgraph Collaboration["协作组件"]
      Room["Room 服务"]
      Mission["Goal / Mission 服务"]
      Scheduler["持久调度与收件箱"]
    end
    subgraph Identity["身份与职责组件"]
      AgentRegistry["Agent 注册与 Slot / Binding"]
    end
    subgraph Knowledge["知识组件"]
      Context["Context / Checkpoint 服务"]
      Memory["分域记忆服务"]
      Assets["文档与产物服务"]
    end
    subgraph Execution["执行组件"]
      Runtime["环境 / Runtime / Lease 管理"]
      Adapter["Harness 适配与能力协商"]
    end
    subgraph Persistence["持久化组件"]
      Store["SQLite 事件、投影、outbox 与内容存储"]
    end
  end
  Harness["外部 Harness 与工具进程"]
  Provider["模型供应方 / 外部业务系统"]
  UI --> Gateway
  Gateway --> Room
  Gateway --> Mission
  Gateway --> AgentRegistry
  Gateway --> Memory
  Gateway --> Runtime
  Room --> Mission
  Room --> Context
  Mission --> Scheduler
  Mission --> AgentRegistry
  Mission --> Assets
  Mission --> Store
  Scheduler --> Context
  Scheduler --> Runtime
  Scheduler --> Store
  AgentRegistry --> Store
  Context --> Memory
  Context --> Assets
  Context --> Store
  Memory --> Store
  Assets --> Store
  Runtime --> Adapter
  Runtime --> Store
  Adapter --> Harness
  Harness --> Provider
```

## 4.2 上下文与聚合接口

```mermaid
graph TB
  UI["Web / 桌面界面"] -->|IF-USER：管理、交办、验收、观察| Gateway["HTTP / Agent 命令与授权入口"]
  Harness["外部 Harness 与工具进程"] -->|IF-AGENT：承接、委派、查询、交付| Gateway
  Gateway -->|IF-ROOM：讨论与结果引用| Room["Room 服务"]
  Gateway -->|IF-MISSION：契约、工作图、验收| Mission["Goal / Mission 服务"]
  Gateway -->|IF-IDENTITY：身份、任职、授权| AgentRegistry["Agent 注册与 Slot / Binding"]
  Gateway -->|IF-MEMORY：查询、策展、纠错| Memory["分域记忆服务"]
  Gateway -->|IF-RUNTIME：检查、维护、排空| Runtime["环境 / Runtime / Lease 管理"]
  Room -->|IF-MISSION：从决定交办| Mission
  Mission -->|IF-SCHEDULE：入队、唤醒、取消| Scheduler["持久调度与收件箱"]
  Scheduler -->|IF-CONTEXT：组装、检查点| Context["Context / Checkpoint 服务"]
  Scheduler -->|IF-RUNTIME：分配、提交、停止| Runtime
  Runtime -->|IF-ADAPTER：探测、执行、取消| Adapter["Harness 适配与能力协商"]
  Adapter -->|IF-HARNESS：原生协议| Harness
```

具体命令见 §11。`IF-ADAPTER` 是 Mosaic 内部统一适配端口，`IF-HARNESS` 是适配器调用外部 Harness 的原生协议边界。Harness 的回调始终由 Gateway 验证身份与作用域；Runtime 管理器提供资源与执行事实，Mission 决定工作是否完成。环境管理员也通过同一入口调用受限维护接口。

## 4.3 逻辑元素到代码、构建与部署

| 逻辑元素 | 现有或拟新增代码落点 | 构建、交付与部署 |
|---|---|---|
| UI | `apps/web/src`，扩展 `components/chat/TasksTab.tsx` | 既有 Vite 构建，嵌入服务端与桌面壳 |
| Gateway / Room | `internal/transport/httpapi`、`internal/room` | 既有 Go 服务；Room 保留讨论引擎 |
| AgentRegistry | `internal/harness` 保留可执行发现；拟新增 `internal/personalagent`、`internal/slot` | 同一 Go 进程；注册表迁移为独立身份与执行位置映射 |
| Mission / Scheduler | 拟新增 `internal/mission`、`internal/scheduler`；复用 `internal/monitor` | 同一 Go 进程；持久队列恢复依赖 Store |
| Context / Memory / Assets | `internal/contextx`、拟新增 `internal/memory`，既有 `internal/doc` | 同一 Go 进程；不增加默认云端记忆服务 |
| Runtime / Adapter | 拟新增 `internal/runtime`；复用 `internal/agent`、`internal/harness` | Go 管理器与现有外部 CLI 子进程；先支持本机 / 已有 WSL 路径 |
| Store | `internal/storage/sqlite` 及本地内容文件 | 沿用现有数据库与备份介质，新增版本化迁移 |
| Harness / Provider | 用户安装的 Harness 及其模型连接、业务工具 | 外部依赖，不打包模型服务或复制用户认证信息 |

装配继续由 `internal/app` 完成，交付仍是 `cmd/mosaic-server` 与 `apps/desktop`。远端连接、容器隔离可作为后续 RuntimeProvider 插件；能力和凭据就绪前不调度到这些环境。

# 5. Slot：职责模板、任职与装配

## 5.1 SlotSpec 的内容

Slot 描述“承担什么职责、需要什么能力、如何交付”，不绑定某个模型或绝对安装路径。工具写为能力要求与版本约束，实际连接由 Binding 解析。

```yaml
schema_version: 1
slot_spec_id: slot.research-lead
version: 1
name: 研究负责人
responsibility: 汇总证据、组织协作，并对研究交付负责
skills:
  - ref: workspace/skills/evidence-review
    version: 1
tools:
  required: [documents.read, documents.write, memory.search]
  optional: [web.search]
context:
  required: [mission.contract, decision.capsule, slot.knowledge]
  optional: [agent.preferences, room.recent]
deliverables:
  - 结论、来源、具名异议、未知项与可验收产物
delegation:
  allowed: [existing_agent, ephemeral_worker]
  max_depth: 2
runtime:
  required: [execute, cancel, isolated_workspace]
  workspace_access: read_write
handoff:
  required: [checkpoint, artifact_refs, pending_actions]
```

上述是候选语义示例，正式 Schema 在实现阶段加入必填、枚举和大小限制。此模板要求决策胶囊，适用于圆桌交办；无需讨论的任务可选用不要求该项的模板。`skills` 是可审查的版本化工作方法，安装或导入不自动执行其中脚本。

## 5.2 Binding 与准入

Binding 记录 `binding_id、agent_id、slot_instance_id、slot_spec_version、scope、effective_grants、tool_bindings、context_policy_version、concurrency_limit、status`。SlotInstance 单独持有 `knowledge_namespace`，不能把职责知识挂到任职者的私有库。

装配依次完成：解析模板 → 核对工具/运行能力 → 绑定允许的数据域与凭据引用 → 生成上下文清单 → 健康探测 → ready → 激活。输出 readiness report，列出满足项、缺失项、可选降级及不能开展的动作。

有效权限取用户授权、Workspace 政策、职责范围、Mission 范围、Runtime 强制能力的交集。Slot 写了工具名称不等于获得权限；Agent 自己修改职责、扩张资源或要求更高权限均不能自授权。可选工具缺失时按已声明降级执行；必需项缺失则 blocked，不静默换负责人、模型或工具。

同一 SlotSpec 新版本不自动改变在途工作。Binding 固定版本；升级先生成差异与就绪报告，完成排空再切换。换人时保留 SlotInstance 的知识、交付和在办事项，新 Binding 读取交接包并承接，旧 Binding 释放租约与写权限。

# 6. Mission 与 Swarm 的协作契约

## 6.1 MissionContract

交办至少包含：`mission_id、contract_version、requester、owner_agent_id、reviewer、objective、scope、constraints、acceptance_criteria、deliverable_specs、budget、source_refs、decision_ref、context_manifest_ref、return_targets、delegation_policy`。

- `source_refs` 固定房间事件、文档修订与决策版本；没有 Closure Capsule 的直接交办须保存明确请求，不能捏造“已达成共识”。
- `decision_ref` 对应的假设、具名异议、未决问题、反证和重开条件随交接包保留；负责人可提出改计划，不能改写原决定。
- `budget` 至少含并发、总 Run 数、墙钟时长、重试与委派深度；token 只有适配器可测时才作硬门，不依赖费用核算。
- `reviewer` 默认请求人；自动验收必须由事先约定的确定性检查或授权验收角色执行，负责人自述不算验收。

## 6.2 两种委派

| 委派方式 | 接受方式 | 上下文与生命周期 | 取消的范围 |
|---|---|---|---|
| 长期 Agent 协作 | 投递 inbox offer，按已授权任职策略自动承接或明确 accepted/declined | 独立身份、队列和私有记忆；仅授予本工作项上下文 | 当前 offer / WorkItem / 对应授权 |
| 临时 Worker | 调度器在负责人配额内创建 | Mission 专用身份、子 Session 与临时工作区；不默认继承负责人全部记忆 | 当前 Worker 的所有在途工作和租约 |

Swarm 是 Mission 的执行配置与 WorkItem 图，不新建一层万能“主脑”状态。负责人通过结构化动作 `offer_work`、`spawn_worker`、`submit_result`、`request_review` 调度；“@B 帮我做”只有在转成合法命令、通过授权并落库后才形成委派。WorkItem 交付给负责人审查，Mission 交付给契约中的验收人。

首次实现限制工作图为 DAG；新增依赖时检测环，拒绝 A 等 B、B 又等 A。限制扇出、深度、总运行次数与重试次数，额度在调度事务中预留，终止后释放。嵌套委派从父项扣预算；等待子项时释放可复用计算配额，避免父任务占满并发造成死锁。

## 6.3 持续事务与唤醒

Goal 汇总长期责任，Mission 推进一次阶段性交付。等待原因结构化为 `dependency / user_input / approval / external_event / schedule / runtime_unavailable / reconciliation`，同时记录 `wake_condition、next_check_at、deadline、last_observed_version`。

定时与事件触发写入持久 inbox，按触发来源与版本去重；规则需有时区、冷却期、合并策略、到期时间与最大尝试数。离线错过多个 tick 默认合并一次“检查当前状态”，不补跑每个错过时刻的外部写动作。Agent suspended、Mission paused 或 Binding draining 时可收件但不启动执行。默认只通知交付、失败、需要用户行动或有意义的变化，观察结果不变时保持安静。

# 7. 记忆、上下文与可恢复工作状态

## 7.1 数据分层和归属

记忆是可纠错的派生知识，任务状态是权威业务事实，上下文是某次执行实际获得的输入，三者不能互相替代。

| 数据层 | 保存内容 | 所有者与迁移策略 |
|---|---|---|
| 事实与证据 | 事件、明确用户输入、动作回执、文档修订 | 原始领域对象持有；可用版本化引用，删除规则独立 |
| Agent Memory | 长期偏好、经验、经确认的公开人格配置 | Agent 私有域；跨任务使用仍需用途与读取权限过滤 |
| Workspace / Room Memory | 项目知识、房间共识与异议 | 共享域；导出 Agent 不默认复制整个共享库 |
| Goal / Mission Memory | 阶段经验、假设、证据整理 | 随事务保存，换负责人可继续使用 |
| Slot Knowledge | 某职责实例的工作规范、环境说明与积累 | 随 SlotInstance 保存，区别于模板与任职者私有记忆 |
| Working Context | 目标、短期进度、当前证据与待执行动作 | Session 的显式 Checkpoint；随任务恢复，按授权迁移 |
| Native Session | CLI 会话 ID、供应方内部状态 | Harness / Runtime 私有；可选恢复提示，不是便携事实源 |
| 检索索引 | FTS、向量、图索引、摘要缓存 | 可重建派生数据；不作为迁移的唯一载荷 |

记忆类型与权限域正交。`semantic / episodic / procedural` 分别表达知识、经历和方法；方法改进先成为建议，影响工具或执行规则时发布新 SlotSpec / Skill 版本，不允许通过“记忆”绕过授权。episodic 只保存可观察动作、结果与公开理由，不保存隐藏推理链。

## 7.2 MemoryItem 与策展

拟定字段：`memory_id、version、namespace、kind、content_ref、source_refs、derived_from、observed_at、valid_from、valid_to、confidence、status、visibility、sensitivity、retention_policy、extractor_version、supersedes、edit_history`。

每条记忆须能追到已提交事件、文档版本或显式人工记忆操作；不能从未经授权的上下文提炼后转入更宽可见域。抽取 → 候选 proposed → 验证来源、去重、冲突检查 → accepted；高可信明确指令可按预设政策自动接受，其他项保留候选或让用户纠错。

时效同时记录“何时知道”与“何时有效”。新旧矛盾不只按写入时间覆盖：区分纠错、事实随时间变化与并存异议；人工纠错具有明确版本记录；过期、撤回和 superseded 项不默认参与当前召回，历史查询仍能解释当时依据。

策展可在任务完成、明确记忆指令或有界后台批次运行；不能每条消息启动一轮反思。抽取所需模型能力经既有 Harness 或已配置能力提供；首版不新增强制模型账户。观察、归纳与推断须标类型，模型推断不能升级成来源事实。

## 7.3 Context Manifest、Receipt 与 Checkpoint

`ContextManifest` 固定本次输入引用与版本：Agent profile、SlotSpec / Binding、MissionContract、决策胶囊、授权记忆、文档与证据、Checkpoint、工具清单、policy version、各领域水位。跨流用水位向量，不虚构一个全局房间序号。

组装顺序：约束与有效权限 → 当前职责 → 任务契约/验收 → 决策与未决风险 → Checkpoint → 相关证据/记忆 → 近期消息 → 工具接口。必需层必须保留；可选层按独立配额截断并标明遗漏。授权及有效期过滤先于排序、摘要与计数，避免泄漏其他域的内容和存在性。

组装是确定性过程，先提供有限正文及按需查询入口；初始沿用 RFC-0002 的 256 KiB 输入上限，再取适配器声明上限的较小者。必需内容放不下或必需证据已被删除时，进入 blocked 并说明原因，不能静默缺失。这里管理“交付什么”，Harness 仍管理实际 token 窗口、压缩和内部会话。

`ContextReceipt` 记录实际交付和后续查询的 ID、版本、摘要、裁剪与脱敏规则、能力版本及 RunID，不复制完整 Prompt、不采集隐藏推理。它证明 Mosaic 交付了什么，不能证明模型保留或理解了全部内容。

`Checkpoint` 独立于 Receipt，保存可恢复业务状态：`session_id、checkpoint_version、mission_version、work_item_id、completed_steps、pending_steps、artifact_refs、action_receipts、uncertain_actions、waiting_conditions、source_watermarks`，以及简短公开进度摘要。它必须能重新读取权威任务状态；文字摘要不能将 pending 偷换成 completed。正常安全边界、等待与交接时落盘；最后一次动作之后尚未落盘则恢复时核验回执。

## 7.4 检索与技术取舍

首版保留 SQLite + FTS5、结构化 scope / 时间 / 来源过滤及按需查询，先把可迁移性、来源和纠错做实。用真实任务记录漏召回与错误召回，再评估向量或时序图；不得把现有“FTS5 起步、向量推迟”的决定默默改成必须安装新数据库。

MemoryBackend 提供 `Propose / Accept / Revise / Search / Export / Import / Forget` 端口，外部系统可作为候选后端或索引。Mosaic 的 ID、授权、删除语义和便携格式保持自身契约；替换检索引擎不能改变记忆归属。§14 的近期资料只提供机制参考，不构成供应商选型结论。

# 8. 可迁移 Agent 与交接协议

> 个人全量备份扩展：[RFC-0019](2026-10-28-rfc-0019-encrypted-backup-merge.md)（Draft）覆盖整个受管实例的加密导出与导入合并，包含受管文件及可导出第三方凭据；本节的选择性 Agent Bundle 继续用于职责/身份交接，保留不携秘密的边界。两者都不因导入自动获得执行权；RFC-0019 的全量范围及合并规则不应误用本节的范围选择流程。

## 8.1 三种迁移，分别承诺

| 操作 | 必须携带或重新绑定 | 不保证的内容 |
|---|---|---|
| 同 Agent 换 Harness / Runtime | 稳定身份、任务版本、授权记忆、Checkpoint、工作文件、工具能力映射 | 原生会话 ID、隐式推理、模型行为完全相同 |
| 同 Slot 换 Agent | 职责实例知识、Mission / WorkItem 状态、产物与未决动作、承接回执 | 上任 Agent 全部私有记忆；不得复制整个人格冒充原负责人 |
| 跨 Mosaic 实例导出 / 导入 | 版本化便携包、来源与权限元数据、脱敏报告、ID 映射、目标环境校验 | 原账号登录态、原路径可用、原工具授权自动成立 |

恢复成功定义为：新执行者知道目标与验收、已完成和未完成事项、哪些动作结果未知，并能访问继续所需证据。恢复应优先使用结构化任务状态与回执，原生会话可用时仅作辅助。

## 8.2 Portable Agent Bundle v1（候选格式）

包内包含清单与内容摘要，文件路径只允许包内相对路径：

```text
manifest.json                 格式版本、来源实例、身份、导出模式、水位、文件摘要
agent/profile.json            显式人格与偏好配置，不含认证凭据
slots/specs.json              所需 SlotSpec 版本与职责实例说明
memory/items.ndjson           经授权的记忆、来源、时效、纠错及删除标记
work/checkpoints.ndjson       指定任务的检查点、等待条件与动作回执
work/commitments.json          承诺及原对象映射；导入默认暂停
context/manifests.ndjson       上下文引用与可用证据清单
artifacts/                    明确选择携带的工作文件和产物
dependencies.json             工具、技能、环境需求及版本约束
redaction-report.json         省略项、断链项、敏感内容处理及恢复限制
```

包不含访问令牌、环境变量中的秘密、原生会话认证材料；embedding 可省略并重建。内容摘要用于完整性检查，不等同于可信签名。导入先检查格式、大小、压缩展开上限、路径穿越、符号链接与引用闭包；不执行包内代码，不因导入启用 MCP server、工具或自动唤醒。

大资产可保留引用，但报告必须说明目标是否可访问。必要证据缺失就阻止激活；可选证据缺失可降级并展示。新版本消费者不理解的必需字段应拒绝，不能静默丢失约束。首版支持本格式同版本与已定义的前向迁移；不承诺直接加载其他产品的导出包。

## 8.3 导出、移动、复制与恢复

普通备份/导出可在一致水位快照上进行，不停止源 Agent，目标导入默认为 inactive。**移动**必须停接新任务、排空或暂停在途工作、核验未知动作，再取得最终快照。目标校验通过并确认源端已撤销执行权后才激活。

跨实例不引入分布式锁：源不可达且无法证明已停止时，不能自动以同一身份接手对外写入。可导入只读待核验副本，或由用户明确按新身份复制；复制生成新 AgentID、记录 `origin_agent_id`，默认不继承活跃承诺、授权与唤醒规则。不能靠包内一个“已停机”字段证明外部执行者已经停止。

迁移事务记录 `preparing → exported → imported_inactive → activated` 或 `aborted`。目标激活前失败可继续使用源；目标已执行外部动作后回切必须先排空目标、核验并回传新状态，不能直接恢复旧快照制造第二位写入者。绑定的全局任务若不在同一库，需显式承接和对象映射，不能仅复制任务摘要当作责任转移。

```mermaid
sequenceDiagram
  participant UI as Web / 桌面界面
  participant Gateway as HTTP / Agent 命令与授权入口
  participant AgentRegistry as Agent 注册与 Slot / Binding
  participant Scheduler as 持久调度与收件箱
  participant Runtime as 环境 / Runtime / Lease 管理
  participant Context as Context / Checkpoint 服务
  participant Store as SQLite 事件、投影、outbox 与内容存储
  UI->>Gateway: 请求切换运行环境(agent_id, target, expected_version)
  Gateway->>AgentRegistry: 开始迁移并将任职设为 draining
  AgentRegistry->>Store: 保存迁移意图与禁止新调度状态
  Gateway->>Scheduler: 排空相关工作项
  Scheduler->>Runtime: 请求在安全边界停止并核验动作
  Runtime-->>Scheduler: 运行回执与未决动作
  Scheduler->>Context: 保存 Checkpoint 与最终水位
  Context->>Store: 提交检查点和引用清单
  Gateway->>Runtime: 检查目标能力、工作文件与凭据引用
  Runtime-->>Gateway: readiness report
  alt 存在未知副作用或必需能力缺失
    Gateway->>Store: 记录 blocked 与待处理项
  else 源执行权已撤销且目标就绪
    Gateway->>AgentRegistry: 切换运行映射并递增执行代次
    AgentRegistry->>Store: 原子提交映射与激活事件
    Gateway->>Scheduler: 恢复已暂停工作项
  end
```

# 9. Runtime、环境管理员与运行约束

## 9.1 管理职责

Environment 保存可声明、可重建的环境规格；Runtime 保存具体运行实例；Lease 保存占用。Agent 身份不绑定 PID，Harness 更新也不新建 Agent。

RuntimeProvider 至少提供 `Discover、Probe、Prepare、Acquire、Execute、Cancel、Drain、Release、Inspect`，并声明版本化能力矩阵：执行任务、取消/进程树终止、可检查恢复、原生会话恢复、工具挂载、文件隔离、网络限制、检查点与资源限制。`RestoreNativeSession` 是可选能力，不能把能力缺失解释成成功恢复。

调度前检查 OS/架构、Harness 版本与登录态、工作区、必需工具、剩余配额及强制隔离条件。原生 CLI 的登录、订阅和模型访问继续由原工具管理，Mosaic 只保存连接/凭据引用及可用性，不导出登录目录。执行期固定 Harness 与工具版本；更新先排空对应 Runtime，再探测、切换、允许新工作进入。

租约含 `lease_id、runtime_id、run_id、generation、expires_at、workspace_ref、resource_limits`。心跳失败触发 unhealthy，禁止新调度；有权限强制控制的进程树请求停止，旧代次的回调只记审计。围栏令牌只保护 Mosaic 接收的状态与受控工具调用；CLI 已经发出的邮件、提交或其他远端动作无法由围栏撤销。

## 9.2 环境管理员是可选的任职角色

用户可为 Agent 的专用 Environment，或一组共享环境，指定一个具备“环境管理员”Slot 的 Agent。该管理员负责依赖检查、健康诊断、声明式准备、升级建议、排空与重建，不成为 Runtime 的实际授权根。

- 默认由确定性探测器和管理器维持环境，没有管理员 Agent 也能执行任务。
- 管理员先产出 MaintenancePlan：目标环境、变更项、影响的租用者、回滚点、验证步骤和所需权限。
- 已授权的常规修复可直接执行；超出既有授权、修改共享系统组件或销毁未保存数据时进入相应权限流程。管理员不能批准自己的越权请求。
- 只读取维护所需的状态和已脱敏日志；不默认获得业务记忆、聊天正文和密钥明文。
- 不允许在仍被其他任务使用的共享环境上重装依赖；先排空或创建替代实例。使用环境本身的故障不能阻断其管理员：维护执行在独立的最小可用 Runtime 上进行。
- 每次维护设重试与时间上限，失败后回退或要求处理，避免“维护 Agent 无限修自己”的循环。

## 9.3 本地进程的真实能力边界

只有当能力可被执行环境强制落实时，才能把文件、网络或工具范围声明为隔离保证。普通本地 CLI 可能拥有用户级权限；提示词里的“只允许访问某目录”不是安全边界。需要强隔离的 Slot 必须匹配可强制执行的 Runtime，否则准入失败。首版可以支持用户信任的本地环境，但 UI 必须如实说明其运行范围。

执行资源按独立工作区分配，同一仓库写入可使用分支/工作树，产物汇总时再冲突处理；禁止多个 Worker 无约束同时编辑同一路径。共享只读数据可复用；共享可写文件须有资源级租约，不能只靠“同一 Agent”判断互斥。

# 10. 圆桌到执行再回传的行为协议

## 10.1 决策交办与独立执行

```mermaid
sequenceDiagram
  participant UI as Web / 桌面界面
  participant Gateway as HTTP / Agent 命令与授权入口
  participant Room as Room 服务
  participant Mission as Goal / Mission 服务
  participant Scheduler as 持久调度与收件箱
  participant Context as Context / Checkpoint 服务
  participant Runtime as 环境 / Runtime / Lease 管理
  participant Adapter as Harness 适配与能力协商
  participant Harness as 外部 Harness 与工具进程
  participant Store as SQLite 事件、投影、outbox 与内容存储
  UI->>Gateway: 交办(source_refs, owner, acceptance, idempotency_key)
  Gateway->>Room: 读取并校验决策版本与可见性
  Room->>Mission: 创建带来源的 MissionContract
  Mission->>Store: 同事务写 Mission、交办引用与调度 outbox
  Mission-->>UI: Mission 卡片和当前状态
  Scheduler->>Store: 领取待执行工作项并预留配额
  Scheduler->>Context: BuildExecutionInput(contract, binding, checkpoint)
  Context-->>Scheduler: ContextManifest、输入与 Receipt
  Scheduler->>Runtime: Acquire(work_item, requirements)
  Runtime-->>Scheduler: Lease 与执行能力
  Scheduler->>Runtime: Execute(run_id, lease, manifest)
  Runtime->>Adapter: SubmitExecutionTask
  Adapter->>Harness: 通过原生接口执行
  Harness->>Gateway: SubmitResult(run_id, generation, artifact_refs, receipts)
  Gateway->>Mission: 校验交付与当前执行代次
  Mission->>Store: 保存工作项结果与交付回执
  Harness->>Gateway: RequestReview(mission_id, delivery_package_ref, expected_version)
  Gateway->>Mission: 校验负责人权限、交付包与任务版本
  Mission->>Store: 保存 reviewing 状态及通知 outbox
  UI->>Gateway: 验收或退回(mission_id, expected_version)
  Gateway->>Mission: RecordReview
  Mission->>Store: 保存验收事实与返回目标引用
  Room->>Store: 消费结果通知并按发布条件写引用卡片
```

图中 Harness 回调代表结构化结果通道；不支持主动回调的 CLI 由 Adapter 解析结果后通过相同的受校验入口提交，仍不得从自由文本猜测控制动作。执行任务与讨论任务分开提示词和输出 Schema；房间 Floor 只约束讨论发言，不限制已经授权的 Mission 执行。

## 10.2 变更、验收与返回讨论

DeliveryPackage 至少包含成果引用、验收逐项对应、验证证据、未解决项、风险、动作回执和建议记忆变更。负责人不能仅拼接 Worker 回复代替验证；必要时指派独立验收角色。

`submit_result` 保存一次 Run 的工作项结果，不自动将 Mission 送验或结案。负责人汇总交付包后调用 `request_review`，校验通过才使 Mission 从 running 进入 reviewing 并通知契约中的验收人；验收人通过 `review_mission` 接受或退回。执行回执、提请验收与验收裁定各自留痕。

Mission 的结果先持久化，再通知 Room。Room 暂停/归档时保留任务中心结果及待投递通知，不为回传自动恢复讨论；恢复后按来源事件去重发布。Room 已删除时取消该返回目标，保留 Mission 内的来源墓碑与结果，允许用户另选去处。

触及决策的 falsifier / reopen trigger 时，负责人提交 `reopen_request`，标出受影响决定、证据和需停止的工作项；系统暂停依赖该假设的执行，把问题交回 Room。已发生的外部动作保留，不能用新决定覆盖旧回执。不存在可用 Room 时进入任务中心待决，不能借“需要讨论”无限挂起而不展示原因。

# 11. 数据、接口与一致性

## 11.1 权威存储

在现有 SQLite 库新增版本化的领域流与可重建投影。候选流为 `agent_events、slot_events、binding_events、goal_events、mission_events、memory_events、runtime_events`；WorkItem / Run 的任务事实放在所属 Mission 流，旧 Room Run 留在原流。`sessions/checkpoints、context_manifests/receipts、inbox、outbox、leases、action_receipts` 作为有明确所有者的持久记录；索引和列表缓存可重建。

每条事件携 `event_id、aggregate_type/id、aggregate_version、actor_ref、causation_id、correlation_id、schema_version、occurred_at`。一个对象的状态只有一个权威写入点；关联 Room 只持有引用与公开摘要。新事件格式独立版本化，不能硬塞到必需 room_id 的旧 Envelope 并伪造房间。

同库的“创建 Mission + 源任务映射 + outbox”原子提交；未关联 Room 的任务不写房间事件。跨对象修改在一个事务中校验所有相关版本与权限。读取固定版本的数据、生成 Manifest、保存 Run 请求与 Receipt 后才能开始执行；领取租约时再次验证 Binding、Mission 未暂停及授权未撤销。

外部执行与通知采用至少一次投递、幂等消费；不承诺端到端 exactly-once。SSE 新增按领域订阅及授权后的 opaque cursor，不向客户端暴露其他对象的全局序号或对象存在性；断线后先重取权威状态再续传。

## 11.2 候选命令和回执

下表是拟议契约，尚未加入当前 HTTP / Agent API。正式实现须同步 JSON Schema、OpenAPI、Go / TypeScript 类型、正反夹具与能力协商，不先发未声明的协议字段。

| 命令 / 查询 | 关键输入 | 返回或拒绝条件 |
|---|---|---|
| `create_agent / suspend_agent / retire_agent` | profile、预期版本、承诺处置计划 | AgentID、状态；未处置承诺禁止完成退役 |
| `prepare_binding / activate_binding / release_binding` | Agent、SlotInstance、模板版本、scope、授权 | BindingID、readiness report；必需能力缺失或排空未完成则拒绝激活 |
| `create_mission` | 契约、来源、返回目标、幂等键 | MissionID、版本；来源或验收缺失则保留 draft 或拒绝 |
| `revise_mission / pause_mission / cancel_mission` | expected_version、变更或原因、在途处理策略 | 新版本与受影响工作项；版本冲突返回当前版本 |
| `offer_work / accept_work / decline_work` | parent、依赖、接收 Agent、期限、上下文子集 | WorkItemID、承接回执；环、越权、超配额拒绝 |
| `spawn_worker` | parent、SlotSpec、受限预算、上下文 | WorkerID、WorkItemID；不默认复制父私有库 |
| `submit_result` | RunID、generation、产物与证据、工作项版本 | 工作项结果与交付回执；迟到只审计，不自动送验或结案 |
| `request_review` | MissionID、delivery_package_ref、expected_version | 校验负责人权限与交付包后进入 reviewing 并通知验收人；状态不允许、版本冲突或必需交付项缺失则拒绝 |
| `review_mission` | MissionID、expected_version、验收结论、逐项依据与退回原因 | 契约指定验收人作出接受/退回裁定；非 reviewing、版本冲突或验收权限不足拒绝 |
| `reopen_request` | MissionID、expected_version、decision_ref、证据引用、受影响工作项 | 校验负责人权限与来源后记录重开请求，暂停受影响执行并投递 Room；Room 不可用时在任务中心待决；不自动重开 Room 或翻转 Mission 终态 |
| `search_memory / revise_memory / forget_memory` | namespace、查询或版本化变更、来源 | 授权结果或删除影响清单；越权不得暴露命中数量 |
| `checkpoint / inspect_runtime / apply_maintenance` | 当前工作版本、RuntimeID 或维护计划 | 检查点、健康/就绪报告或有界维护任务 |
| `export_agent / import_agent / activate_import` | 导出范围、包、映射、迁移模式 | 完整性/兼容性报告；导入本身不激活 |

写命令共用 `command_id、idempotency_key、expected_version`；服务端从认证会话或有效 Run/Binding 推导 actor，拒绝客户端自称 system。相同主体、作用域、幂等键与相同请求重放原回执；同键不同载荷冲突。受限 Agent token 只允许指定动作、对象、代次与有效期，不把本地 owner token 交给 Worker。

建议错误语义：`version_conflict、capability_missing、grant_required、dependency_cycle、quota_exceeded、source_unavailable、stale_generation、action_outcome_unknown`，每项携可理解说明与可恢复动作。UI 不以通用“重试”掩盖结果未知。

## 11.3 崩溃、副作用与恢复

| 故障点 | 恢复动作 |
|---|---|
| 任务落库后未投递 | outbox 重投，调度领取及 Run 创建幂等 |
| 已启动但没有终态回执 | 检查 Runtime、进程与动作日志；无法确认时 Run=unknown，WorkItem 等待 reconciliation |
| 外部写动作成功但本地未确认 | 使用稳定 action_id 查询远端或回读；支持幂等键则复用原键，否则人工/授权核验后决定 |
| 取消或迁移后旧结果到达 | generation / 终态检查；保留 late 回执，不覆盖新任务、不发布重复成果 |
| 产物成功但 Room 通知失败 | 交付保持有效，outbox 有界重试通知；Room 去重，不重跑执行 |
| 环境重建失败 | 任务进入 runtime_unavailable；保留身份、承诺、Checkpoint，显示处理入口 |

受控写动作先登记 ActionIntent，再执行并保存回执。部分 CLI 工具执行不向 Mosaic 暴露动作细节，必须标记 `opaque_execution`；这种运行崩溃后不能自动证明安全重试。读操作与已验证可幂等动作可有界重试，其余沿用现有 Run 的保守 unknown 语义。

## 11.4 删除、授权撤销与备份

删除 Room 不级联独立 Mission 或文档，但撤销对其原文的读取。迁移到任务域的最小决策摘要须在交办时有明确的持有与保留依据；没有该依据就只保留引用，来源删除后标 unavailable。隐私删除可沿 `derived_from/source_refs` 追踪清除摘要、记忆正文、索引、检查点副本及托管导出包；审计仅保留不含正文的墓碑。

撤销授权立即阻止新的上下文交付、查询和工具执行；若原生会话已经包含失效数据，关闭该 Session 并从清理后的输入新建。无法从外部 Harness 保证清除的副本与用户手工下载的包须在删除报告中如实列出，不能宣称全域擦除。

备份覆盖所有权威流、检查点、任务文件与映射，恢复后先加载墓碑和撤销记录，再重建索引；备份恢复与导入均默认禁止自动重放外部动作。删除过的数据不能因旧包导入静默复活，导入须核对已知来源的删除代次，无法核实时作为待确认数据处理。

# 12. 产品界面与可观测性

产品主导航保持“Agent、职责、目标与任务、房间、环境”五个入口。Session、Lease、generation 等细节放执行记录或诊断视图，不让用户为了交办先理解调度实现。

| 入口 | 主要展示与操作 |
|---|---|
| Agent | 当前职责、承担事务、收件箱、记忆、可用环境；暂停接单、迁移、退役 |
| 职责 | 模板版本、任职者、职责知识、工具要求和就绪状态；任职、升级、换人交接 |
| 房间任务卡 | 来源决定、负责人、执行状态、等待原因、成果跳转；交办与返回讨论 |
| 目标与任务 | 跨房间工作、子任务图、负责人/协作者、预算、下一步、验收与历史 |
| 环境 | 健康、运行范围、占用任务、工具能力、管理员与维护历史；排空、修复、替换 |
| 记忆面板 | 分域浏览、来源/有效期、纠错、删除及迁移预览；避免将私有域混进房间公共列表 |

Room 归档/删除页显示仍在继续的独立 Mission，并提供单独处理入口；默认不替用户取消。停止 Agent 时区分“停止接单”“暂停相关任务”“退役并转交责任”，避免一个按钮意外销毁未交付工作。

记录调度延迟、等待原因、工具/环境缺失、重试与 unknown 数、记忆命中/遗漏、输入裁剪、迁移就绪和恢复失败原因。Trace 通过 MissionID / WorkItemID / RunID 关联；公开日志脱敏，诊断不能默认输出完整 Prompt、凭据或跨域正文。

# 13. 落地阶段、迁移与验收

## 13.1 渐进交付

本 RFC 使用 P0–P5，避免与既有交付计划的 M0–M4 混淆。发行映射由[交付与进度规划](../../plan/2026-08-25-delivery-plan.md#02-当前-roadmap)统一承载：P0 是 0.5 的开工门槛，不单独占用特性版本；P1 拆为 0.5「长期 Agent 与职责」和 0.6「Mission 与本地执行」；P2、P3、P4 分别对应 0.7、0.8、0.9；0.10「对话优化」是 1.0.0 前最后一个领域中版本；P5 留作 1.0 后的条件领域。每个领域版本线内的第三段每次只交付一个特性，不另发只用于汇总的中版本。

| 阶段 | 实现范围与依赖 | 退出条件 |
|---|---|---|
| P0 契约与基线 | 批准边界修订；领域 Schema、能力矩阵、现有数据快照与恢复演练 | 完成 §2.2 对齐；迁移 fixture 可重复且旧行为不变；确定评测机器与数据集 |
| P1 独立身份与单人闭环 | AgentRegistry、最小 Slot/Binding、独立 Mission、执行任务类型、上下文来源与本地 Runtime/Lease；能力仅开放单 Agent | Room 关闭后可交付与验收；暂停、取消、unknown 可解释；Slot 必需能力与强制隔离准入、可选降级通过；AT-01/02/03/07/10/13 |
| P2 记忆与可移植恢复 | 分域 Memory、Checkpoint、Bundle、导入预检、Harness 切换 | 换 Harness 续办；权限不扩张、证据不丢失、未知动作不重做；AT-04/05/06 |
| P3 有界 Swarm | 长期协作 offer/accept、临时 Worker、DAG、配额、结果汇总 | 两种委派及取消隔离通过；至少两种真实 Harness 协作；AT-11/12 |
| P4 持续事务与环境维护 | Goal、持久唤醒、离线恢复、可选管理员 Slot、排空升级 | 重启恢复、重复唤醒去重、共享环境维护隔离；AT-08/09，并在环境变更后回归 P1 的 AT-07 |
| P5 按证据扩展 | 根据召回评测选语义/图后端；远端 RuntimeProvider、更多格式适配 | 有可复现收益和平台/安全验收才引入；不阻塞 P1–P4 |

P1 已须有基本执行隔离、租约与身份授权，AT-07 全部准入检查在 P1 验收；强制隔离能力不满足时拒绝激活，不能把安全边界推迟到环境管理员阶段。P4 对维护或替换后的环境重新检查这些条件。P2 之前不可宣传跨 Harness 便携恢复；P4 之前不可宣传长期无人值守管理。

## 13.2 存量升级与回退

1. 备份数据库、Harness 注册表与任务文件；迁移前后保留版本和计数报告。
2. 为每个唯一 durable BotID 建立 Agent 映射，保留旧 ParticipantID / ProfileID / BotID 作为别名。可执行路径只作部署位置；冲突和歧义进入待处理，不按同名或同 adapter 自动合并。旧 API 输出保持旧 ID。
3. 新独立 Agent 注册表成为身份权威，可执行发现表只引用 AgentID；旧注册表作为单向兼容导出，禁止双向同步造成两位权威。
4. 原 tasklist、Room Run、Closure Capsule 和 Room memory 留在原日志。用户选择“交办”为其创建唯一 `(room_id, task_id) → mission_id` 映射；幂等重复交办返回原 Mission。已映射清单项显示 Mission 状态，后续 `mosaic-todo` 的消失/打勾不取消或验收 Mission。
5. legacy Run 的启动与回传继续原路径；新 Mission 走独立执行器，同一工作不可同时走两条路径。非终态旧 Run 先保守恢复为原有 unknown / 已确认结果，不自动转换成可重试新 Run。
6. 旧原生会话仅在匹配原 Harness/环境时复用；新 Session 采用作用域键，保留原键映射但不跨房间串用。迁移默认不复制 Room 记忆到私有域。
7. 新能力由功能开关逐阶段启用，关闭后停止新任务创建并排空新执行器；新数据保留只读。执行过新模式后不支持直接让旧二进制写新库。恢复备份只能用于停机回退，并核验备份之后的外部动作，防止重复执行。

## 13.3 验收矩阵

以下是实现必须通过的验收设计，本次文档交付未运行或宣称通过这些功能测试。

| 编号 | 场景 | 可观察通过条件 |
|---|---|---|
| AT-01 | 无 Room 创建 Agent，并在两个房间参与讨论 | 身份相同，原生/语义 Session 隔离；退出任一房间不删除 Agent |
| AT-02 | 从房间交办后暂停、归档、删除原房间 | Mission 独立继续或按用户操作暂停；结果任务中心可取，删除规则和来源缺失如实展示 |
| AT-03 | Agent 退役且有未交付事务 | 任务逐一转交/取消/显式无人承接；共享知识保留；未处置时退役不能完成 |
| AT-04 | 执行中换 Harness，原生会话完全不可用 | 结构化目标/进度/来源/未决动作可恢复；能力缺失阻止激活，不捏造成功 |
| AT-05 | A 私有记忆、两个不同可见域及过期冲突事实 | 作用域过滤正确、历史时效可解释；共享结果不泄露私有内容或命中数量 |
| AT-06 | 记忆纠错/删除后导出、恢复或导入旧包 | 最新修订与墓碑生效；派生内容可追踪；受管理副本不复活已删正文 |
| AT-07 | Slot 缺必需工具、选配工具或强制隔离能力 | 必需缺失 blocked；可选降级有报告；不得偷偷换执行者或扩大权限 |
| AT-08 | 管理员修复共享环境或申请越权操作 | 活跃租用者先排空/迁移；权限校验生效；失败保留回滚点，不读无关业务记忆 |
| AT-09 | 应用关闭后错过多次唤醒，重启收到重复事件 | 合并/去重符合规则；不重复外部写动作；暂停任务不启动；不变状态不反复通知 |
| AT-10 | 执行“成功”但缺验收证据，之后补证交付 | submit_result 不自动送验；request_review 缺必需项时拒绝，补齐后进入 reviewing；仅授权 review_mission 可裁定接受/退回；任务卡、产物、决定可互相追踪 |
| AT-11 | A 委派长期 B，并创建临时 C；取消 B 的当前子项 | B 的其他任务继续；C 回收；预算/环检测生效，父等待不饿死子任务 |
| AT-12 | 在副作用后、回执前崩溃，旧代次结果迟到 | unknown 可核验；无盲重试；旧结果只审计；重复投递不产生重复交付 |
| AT-13 | 两个并发修订、重复幂等键和导入同身份 | CAS 冲突明确；同键异载荷拒绝；不出现两个 active 任职者/执行代次 |

测试层次：状态机与权限/版本单测，SQLite/outbox/恢复集成测试，UI 的交办—协作—验收端到端测试，以及真实 Harness 的跨环境续办和取消验证。模拟器通过不能替代后两者。

性能目标沿用 RFC-0007 的检索 P95 < 300ms、上下文组装 P95 < 500ms（不含模型），适用基准暂定 1 万条记忆、10 万条历史事件、20 个活跃 Mission、4 路并发，控制输入在声明上限内。P0 固定硬件、冷/热缓存与中英文查询集并记录基线；目标不是已测成绩。长事务、索引重建和后台策展不得持续阻塞聊天写入，比较启用前后的聊天延迟与队列积压。

记忆质量至少评估有来源支持的召回准确性、必需事实覆盖、时间冲突处理、删除有效性及跨域泄漏（必须为零）。新增向量/图方案需在同一任务集与输入预算下证明收益，并报告延迟、资源和部署代价。

## 13.4 风险与放弃方案

| 方案 / 风险 | 取舍与应对 |
|---|---|
| 只给 Run 加 parent_id | 仍受 Room 生命周期、单一上下文和原生会话约束；选择独立 Mission，但分阶段实现 |
| 把长期 Agent 等同常驻进程 | 重启、休眠会丢失语义连续性；选择持久身份与按需执行，增加调度恢复成本 |
| 一个 Slot 打包身份、记忆、密钥和环境 | 换人就复制权限与隐私；选择 Spec / Instance / Binding / Runtime 分离 |
| 全部聊天复制进 Agent | 体积、误记忆和权限泄漏；采用来源引用、分域记忆与显式交接 |
| 默认采用重型向量/图服务 | 破坏本地交付并增加运维；先 FTS5 与端口，按测量决定扩展 |
| 任意 Agent 都能随意生子/派工 | 无界扇出、循环等待、成本与资源失控；使用有界 DAG、准入和预算预留 |
| 包已导入就直接运行 | 来源不可信、能力不匹配、双活副作用；先 inactive、校验、重新授权与单执行权接管 |
| 多权威流增加复杂度 | 需要跨对象版本、删除追踪与恢复演练；延续同库事务和 outbox，暂不分布式化 |
| 原生 CLI 绕过工具控制 | 无法保证动作审计和强隔离；能力矩阵诚实声明，严格 Slot 拒绝不合格 Runtime |
| 老客户端/旧二进制不认识新对象 | API 能力协商、只读兼容投影、显式迁移版本；不进行危险的无条件降级 |

## 13.5 评审待定项与默认提案

以下不阻止设计成文；在对应阶段实现前形成裁定并回填，不通过臆造版本排期解决。

| 待定项 | 默认提案 | 决策时点 |
|---|---|---|
| 跨房间 Agent Memory 的默认采集 | 默认只收显式保存与任务交接；推断型长期偏好需选择启用 | P0 |
| SlotInstance 是否支持多位同时任职 | 首版单 active，协作通过不同实例；未来有真实共享职责需求再扩展 | P0 |
| Goal 是否首版展示 | P1 数据关系预留且可为空，完整用户入口与唤醒 P4 交付 | P0 |
| 便携包大小、保留与加密 | 本地导出可审查、禁止秘密；上限、默认保留和加密方案需结合文件基准确定 | P2 前 |
| 各 Harness 的强制隔离与恢复分级 | 每家逐项实测并公开；原生会话恢复不是最低准入要求 | P1/P2 前 |
| 外部记忆后端与 .af 等格式互通 | 暂不默认引入，仅做显式转换器，输出损失清单 | P5 |
| 远端常驻执行及离线接管 | 本地优先；另立认证、租约、回执协议设计后开放 | P5 |

# 14. 近期记忆系统参考与设计依据

下列官方资料在 **2026-10-28** 查阅。仅记录查到的机制，不用产品宣传或榜单分数证明 Mosaic 的实现效果；“借鉴”栏是本 RFC 的设计判断。

| 官方来源 | 可核查机制与限制 | 本提案的借鉴 / 适用边界 |
|---|---|---|
| [Letta AgentFile](https://docs.letta.com/v1-sdk/concepts/agent-file) | 描述 Agent 配置、记忆块、工具及消息的序列化与导入导出；页面位于 V1 SDK legacy 文档，且将 archival passages 标为 planned | 借鉴显式、版本化便携包；不把 .af 视为全行业无损标准，也不声称其导出了全部长期记忆 |
| [LangMem Core Concepts](https://langchain-ai.github.io/langmem/concepts/conceptual_guide/) | 区分 semantic / episodic / procedural，profile / collection，以及运行中与后台形成记忆 | 记忆内容类型与存储/采集时机分开；Mosaic 另行坚持权限域、来源与不采集隐藏推理 |
| [Graphiti 官方仓库](https://github.com/getzep/graphiti) | 时序事实、来源 episode、有效窗口和混合检索；自建方仍需提供存储、模型、接入和权限治理 | 先引入时效及 provenance 字段；时序图作为候选索引，不把图库当作权限或事实权威 |
| [Hindsight 官方仓库](https://github.com/vectorize-io/hindsight) | retain / recall / reflect；bank 内组织事实、经历及归纳，并使用语义、关键词、关系和时间检索 | 将提取、召回、反思分成有界流程；反思结果带证据与类型，不自动覆盖事实或触发执行 |

上述机制支持“记忆管理应覆盖生命周期、来源、时间、权限、纠错与迁移”的设计判断；并不要求采用其中任何整套运行时。封闭 Personal Agent 产品仍是体验参照，Mosaic 的可交付能力以自己掌握的契约和实测结果为准。

## 14.1 仓库内规范入口

- [架构设计说明书](../2026-08-13-mosaic-architecture-design.md)、[设计文档索引](../README.md)。
- [RFC-0002 Agent Protocol](2026-08-25-rfc-0002-agent-protocol.md)、[RFC-0005 收束协议](2026-08-25-rfc-0005-closure-capsule.md)、[RFC-0007 Context 与 Memory](2026-08-25-rfc-0007-context-memory.md)。
- [RFC-0008 身份与授权](2026-08-25-rfc-0008-identity-authz.md)、[RFC-0009 工具与 Artifact](2026-08-25-rfc-0009-tool-artifact.md)、[RFC-0010 数据生命周期](2026-08-25-rfc-0010-data-lifecycle.md)。
- [RFC-0012 群聊模型](2026-09-01-rfc-0012-chat-model.md)、[ADR-0013 稳定身份](../adr/ADR-0013-stable-bot-identity.md)、[ADR-0014 文档事件流](../adr/ADR-0014-document-event-stream.md)、[ADR-0016 Agent 更新通道](../adr/ADR-0016-agent-update-channels.md)。

## 14.2 修订记录

| 版本 | 日期 | 说明 |
|---|---|---|
| v0.1 | 2026-10-28 | 从可见讨论要求及 v0.4.1 代码基线形成设计草案：生命周期、Slot、Mission/Swarm、可迁移记忆与上下文、Runtime 与可选环境管理员、协议一致性、迁移和验收 |
| v0.2 | 2026-10-28 | 吸收 GLM 审校意见：AT-07 归入 P1 并在 P4 回归；补全 request_review / reopen_request 命令及送验时序；标题层级对齐 RFC 系列；区分 IF-ADAPTER / IF-HARNESS，标注内部引用和 OQ 来源，补登交付计划；保持 Draft |
| v0.3 | 2026-10-28 | 按负责人裁定对齐 Roadmap：P0 作为 0.5 开工门槛，P1 拆为 0.5 / 0.6，P2–P4 映射 0.7–0.9，0.10 对话优化为 1.0.0 前最后一个领域；领域内第三段每次只交付一个特性，P5 留作 1.0 后条件扩展；版本规划不代表设计批准或功能实现，保持 Draft |
| v0.4 | 2026-10-28 | 负责人批准（指令见状态行），状态 Draft → Approved；P0 门槛随 0.5.0 开工执行（RFC 批准、契约冻结、基线 fixture 与 §2.2 对齐），实施细节按各领域小版本另立 ADR 登记 |
