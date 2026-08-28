# 特性设计说明书（RFC 提案）：RFC-0009 工具与 Artifact 治理

**状态 (Status):** Draft

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-08-25

**更新日期 (Updated):** 2026-08-25

**相关 Issue/PR:** #TBD（RFC 入库时创建 tracking issue）

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| RFC 编号 | 0009 |
| 系列位置 | RFC 序列第 9 篇；工具不是 v0.1 核心验证点，本 RFC 定义"最小而完整"的治理契约 |
| 上游输入 | [架构设计说明书](../2026-08-13-mosaic-architecture-design.md) v0.7 §6.7、§7.3.2、§8.3.2、§9.9；[RFC-0002](2026-08-25-rfc-0002-agent-protocol.md)（历史查询通道与权限映射）；[RFC-0008](2026-08-25-rfc-0008-identity-authz.md)（审批人权限） |
| 吸收开放问题 | OQ-08（v0.1 是否接入 MCP）——本 RFC 与 RFC-0002 联合提出裁决建议 |
| 下游 | `internal/tool`（LE-12）、`internal/artifact`（LE-15）实现、审批 UI、MCP server 组件 |

## 0.1 修订记录

| 版本 | 日期 | 修订人 | 说明 |
|---|---|---|---|
| v0.1 | 2026-08-25 | Mosaic 项目组 / ZCode | 初稿：工具端口与 capability Schema、调用规约与审批流、内置历史查询工具、OQ-08 裁决建议（MCP server 自用通道）、Artifact quarantine 流水线、不可信内容标记、沙箱参数 |

# 1. 概述

## 1.1 简介

工具调用不是 v0.1 的核心验证点，但治理路径必须一次成型：**每次调用绑定上下文、过 capability allowlist 与参数 Schema、按风险级别决定自动/审批/拒绝、结果与 Artifact 走不可信内容通道**。本 RFC 定义 Tool Gateway 端口、capability 声明 Schema、审批流（含超时即拒绝）、内置只读历史查询工具（与 RFC-0002 的双通道衔接），以及 Artifact 的隔离发布流水线（quarantine → hash/MIME/扫描 → 引用 + 短 TTL signed URL）。

## 1.2 动机

架构 §6.7 的五条规则只有原则表述；OQ-08（MCP 是否进 v0.1）因 RFC-0002 的历史回查通道建议而需要正式裁决；Artifact 的 quarantine 流水线（§8.3.2）是供应链与内容安全的第一道闸。工具越权是 STRIDE/LLM 专项威胁（Tool Confused Deputy）的主要落点，契约必须先于第一个外部工具存在。

## 1.3 目标

### 1.3.1 目标

1. 定义 Tool Gateway 端口与 capability 声明 Schema；
2. 定义调用规约（绑定、校验、风险分级、审批、结果规整）；
3. 定义内置只读历史查询工具的 capability 与审批默认；
4. 对 OQ-08 提出裁决建议（与 RFC-0002 一致）；
5. 定义 Artifact 上传/隔离/发布流水线与 signed URL 契约；
6. 定义不可信内容标记与沙箱参数的传递。

### 1.3.2 非目标

- 外部 MCP 工具生态接入（延后，见 OQ-08 建议）；
- 带副作用工具（v0.1 只读低风险）；
- 远程执行集群/沙箱服务（演进项，架构 §8.2.5）；
- 扫描引擎实现（部署方挂钩，本 RFC 只定接口）。

# 2. 用例分析

| 用例 | 功能点 | 关键指标 | 安全隐私与 DFX 约束 |
|---|---|---|---|
| G-01 只读工具自动放行 | read_only 工具按 allowlist 自动执行 | 审批零等待 | 全审计；结果带不可信标记 |
| G-02 审批请求 | 中风险工具挂起；人类批准/拒绝 | 审批通知送达 < 1s；超时（默认 5 分钟）即拒绝 | 审批人按 0008 矩阵；Repudiation 由审计覆盖 |
| G-03 参数校验拒绝 | 越界参数/Schema 不符同步拒绝 | < 100ms | 拒绝不产生副作用 |
| G-04 注入防护 | 工具结果作为不可信内容进上下文（0007 标记） | — | 防 indirect prompt injection 提权（9.1.3.1） |
| G-05 Artifact 隔离发布 | quarantine → hash/MIME/扫描 → 引用 | 流水线异步；引用可见即已通过扫描 | 未通过隔离区保留（策略清理） |
| G-06 历史查询工具 | 内置 read_only；经 MCP server 或结构化请求（0002） | P95 < 1s | 非干扰过滤 + 审计 + 计入 Receipt |
| G-07 恶意内容拦截 | 扫描命中 → 隔离并告警 | 命中即阻断发布 | 误报可人工复核放行（审计） |

# 3. 方案设计

## 3.1 总体方案

### 3.1.1 Capability 声明 Schema

```json
{
  "tool_id": "tl_history_query",
  "risk": "read_only",
  "param_schema": {"$ref": "history_query_input"},
  "output_schema": {"$ref": "envelope_list"},
  "untrusted_output": true,
  "timeout_ms": 5000,
  "allowed_scopes": ["room", "thread"]
}
```

- `risk ∈ {read_only, side_effect}`（v0.1 仅注册 read_only；side_effect 登记即拒绝执行）；
- allowlist 按 `(tenant, room?, profile capability)` 三级声明；未注册工具不可调用。

### 3.1.2 调用规约（架构 §6.7 五条的契约化）

1. 每次调用绑定 `run_id / participant_id / room_id / thread_id / causation_event_id`（缺一拒绝）；
2. 参数经 param_schema 严格校验（越界拒绝非修正）；
3. 风险分级处置：`read_only → auto_allow`（allowlist 内）、`side_effect → request_approval`、未注册/越 allowlist → `deny`；
4. 结果规整为摘要 + 引用；大二进制只写 Artifact，不进 Event payload（RFC-0001 上限）；
5. 输出统一 `untrusted` 标记注入上下文（RFC-0007 3.1.9）。

### 3.1.3 审批流

- 事件：`tool.approval_requested {tool_call_id, tool_id, params_digest, requester}` → 人类 `approve_tool_call` / `deny_tool_call` 命令（0008：member+ 审只读、moderator+ 审副作用）→ `tool.completed {status, result_digest}`；
- 超时（默认 5 分钟）= 拒绝（不悬挂）；在途审批随 pause/cancel 撤销；
- 审批不可委托给 Agent（判断权归人，同 0003 保送原则）。

### 3.1.4 内置历史查询工具与 OQ-08 裁决建议

- 内置 `tl_history_query`（read_only、auto_allow、全审计、计入 Receipt），通道 = RFC-0002 双轨（本地 MCP server 或结构化请求）；
- **裁决建议（与 RFC-0002 一致）**：v0.1 引入 MCP **仅 server 侧**（自用历史查询通道，官方 Go SDK，MIT）；外部 MCP 工具（client 侧生态）延后，引入前须过本 RFC 的 capability 注册 + 安全评审；
- 该建议若被接受，OQ-08 关闭口径为"MCP 以自用工具通道身份进入 v0.1"。

### 3.1.5 Artifact 流水线

```text
上传 → quarantine key（不可读区）→ hash(SHA-256) + MIME 嗅探 + 恶意内容扫描
     → 通过：artifacts 表登记（hash/MIME/scan_status=clean）+ 引用发布
     → 失败：隔离区保留（策略清理）+ 告警
```

- signed URL：短 TTL（默认 10 分钟）、绑定对象与操作、生成前校验 Room 成员权限（架构 §9.2.1.3：signed URL 不授予成员权限）；
- 大小上限默认 100 MiB/文件；引用进 Event 时只带 `{artifact_id, hash, mime, size}`；
- 扫描引擎为部署方挂钩接口（`scan(artifact) -> verdict`），不内置引擎。

### 3.1.6 沙箱参数传递

Agent 侧工具（进程内，RFC-0002 权限模式）与 Mosaic 侧工具（本 RFC）各自治理：Profile 的 permission_allowlist 与沙箱参数由适配器下发到 agent 进程；Mosaic 侧工具的执行受 capability 与审批约束；两侧审计汇合到 run 维度。

## 3.2 技术选型

| 决策点 | 选择 | 放弃方案与原因 |
|---|---|---|
| MCP server SDK | 官方 Go SDK（MIT） | 自研 JSON-RPC：维护成本，生态零收益 |
| 扫描 | 部署方挂钩接口 | 内置引擎（ClamAV 等）：加重镜像、误报策略难统一 |
| signed URL | 短 TTL 自签 | 永久链接 / 公共桶：违背最小暴露 |
| 审批通道 | 房间事件 + 通知 | 私聊/邮件旁路：不可审计、不可回放 |

## 3.3 功能与性能设计

- **内部端口原型**：`ToolPort: Invoke(binding, tool_id, params) -> ToolResult | ApprovalPending / Approve(cmd) / Deny(cmd)`；`ArtifactPort: Put(stream) -> artifact_id / GetSigned(artifact_id, ttl) / QuarantineStats()`；
- **性能目标**：read_only 调用端到端 P95 < 1s；审批通知 < 1s；上传吞吐满足 100 MiB/文件异步流水线；
- **影响范围**：`internal/tool`、`internal/artifact`、MCP server 组件、`artifacts` 表、审批 UI、CI 注入与越权 fixture。

## 3.4 安全隐私与 DFX 设计

- **Tool Confused Deputy 防护**：capability + 参数校验 + 不可信标记三连；工具结果永不被当作指令执行（仅作为上下文）；
- **审计全覆盖**：request/approve/deny/complete/quarantine 五类事件；
- **隐私**：Artifact 内容不进日志；扫描在隔离区进行；
- **可靠性**：工具失败不阻塞讨论（可见失败状态）；流水线崩溃可从 quarantine 重放；
- **DFX**：审批积压指标（防审批疲劳导致误放行）。

## 3.5 编程与调用设计

### 3.5.1 编程模型基本设计

- **开发环境**：echo 工具（回显参数）+ mock 扫描挂钩本地联调；
- **开发约束**：新工具必须先注册 capability（缺注册 CI 拒绝，与 0008 矩阵同机制）；
- **可验收设计**：越权/越参/注入 fixture；流水线往返（含扫描失败路径）；审批超时拒绝断言。

### 3.5.2 接口定义与设计

#### InvokeTool（内部端口）

接口描述：统一的工具调用入口（经 RFC-0002 通道或内部触发）。

接口原型：`ToolPort.Invoke(binding, tool_id, params)`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| binding | 输入 | object | run/participant/room/thread/causation | 五要素齐备 |
| tool_id / params | 输入 | — | 注册工具与参数 | allowlist 内、过 Schema |
| ToolResult / ApprovalPending | 输出 | — | 结果（untrusted 标记）或审批句柄 | — |

- 异常处理：deny 同步返回原因；审批超时转拒绝事件。
- 约束说明：结果摘要 + 引用，不含大二进制。
- 变更说明：首版。
- 调用参考代码：`res := tools.Invoke(binding, "tl_history_query", q)`。

#### ApproveToolCall / DenyToolCall（命令）

接口描述：人类审批决定（命令通道，RFC-0001）。

- 异常处理：非待审状态/权限不足拒绝。
- 约束说明：member+（只读）/ moderator+（副作用）；审计。
- 变更说明：首版。
- 调用参考代码：`await mosaic.rooms.command(roomId, {command_kind:"approve_tool_call", payload:{tool_call_id}})`。

#### PutArtifact / GetSignedURL

接口描述：上传走隔离流水线；下载走短 TTL 签名。

接口原型：`POST /v1/artifacts`（上传）；`GET /v1/artifacts/{id}/url?ttl=`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| stream | 输入 | binary | 文件流 | ≤ 100 MiB |
| artifact | 输出 | object | `{artifact_id, hash, mime, size, scan_status}` | clean 后可引用 |
| url | 输出 | string | 短 TTL 签名地址 | 默认 10 分钟 |

- 异常处理：扫描失败进入隔离区并告警。
- 约束说明：上传需 Room 成员权限；引用发布才进 Event。
- 变更说明：首版。
- 调用参考代码：（SDK 封装流式上传。）

### 3.5.3 编程手册设计

《工具接入与审批指南》（面向管理员/开发者）：capability 注册流程、审批操作、扫描挂钩配置、外部 MCP 工具引入的安全评审清单。

# 4. 缺点和风险

| 风险 | 影响 | 应对 |
|---|---|---|
| 审批疲劳 → 误放行 | 越权工具滥用 | v0.1 仅只读自动放行；副作用工具延后；审批积压指标 |
| 间接提示注入经工具结果 | Agent 被诱导越权 | untrusted 标记贯穿（0007）；工具结果不作指令 |
| 扫描误报阻断合法 Artifact | 协作受阻 | 人工复核放行（审计）+ 挂钩策略可调 |
| MCP server 暴露面 | agent 侧滥用查询 | 查询语义固定只读 + 非干扰 + 审计（0002/0006 契约） |
| 隔离区存储成本 | 磁盘膨胀 | 策略清理（保留期/上限） |

# 5. 现有技术

| 参考 | 借鉴 | 差异 |
|---|---|---|
| MCP 工具生态 | 工具发现/调用/审批的标准形态（server 侧采用） | Mosaic 只读起步、capability 先行 |
| OAuth scope 模型 | 最小能力声明的思想 | Mosaic capability 绑定讨论上下文五要素 |
| 邮件附件隔离管线 | quarantine → 扫描 → 发布模式 | Mosaic 加 hash 引用与事件绑定 |
| Slack App 权限模型 | 安装即授权的粒度参考 | Mosaic 逐调用审批而非一次性授权 |

# 6. 未解决问题

1. **OQ-08 终裁**：MCP server 自用通道建议的确认（与 RFC-0002 未决 1 联动）。
2. 副作用工具引入的时点与安全评审流程。
3. 审批超时默认值（5 分钟）与通知渠道。
4. 扫描挂钩的 verdict 协议（clean/infected/unknown 三态语义）。
5. Artifact 引用计数与孤儿清理（联动 RFC-0010）。
6. 外部 MCP 工具的白名单准入清单模板。

# 7. 附录

## 7.1 参考资料

- 架构设计说明书 v0.7：§6.7 工具与 Artifact、§7.3.2 artifacts 表、§8.3.2 quarantine、§9.9 安全基线
- [RFC-0002](2026-08-25-rfc-0002-agent-protocol.md)（历史查询双通道）、[RFC-0007](2026-08-25-rfc-0007-context-memory.md)（不可信标记）、[RFC-0008](2026-08-25-rfc-0008-identity-authz.md)（审批人权限）
- [Model Context Protocol](https://modelcontextprotocol.io/)、[MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)

## 7.2 术语表

| 术语 | 英文 | 含义 |
|---|---|---|
| 能力声明 | Capability | 工具的风险级、参数/输出 Schema 与作用域注册 |
| 审批 | Approval | 人类对中风险工具调用的显式决定（超时即拒绝） |
| 隔离区 | Quarantine | Artifact 发布前的不可读暂存区 |
| 签名地址 | Signed URL | 短 TTL 且绑定对象的受控下载链接 |
| 混沌代理 | Confused Deputy | 高权限执行者被低信任输入诱导的攻击形态 |

## 7.3 文档更新计划

- 评审期（Draft → Reviewing）：收敛 OQ-08 与未决 2–4；
- Accepted 后：MCP server 组件与流水线实现排期；越权/注入 fixture 进 CI；
- 后续：副作用工具与外部 MCP 准入时修订。
