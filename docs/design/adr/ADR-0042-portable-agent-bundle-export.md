# ADR-0042：0.7.5 Portable Agent Bundle 导出——范围选择、摘要与脱敏报告

- 状态：Accepted（2026-11-09，随 0.7.5 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§8 可迁移 Agent 与交接协议——§8.1 三种迁移分别承诺、§8.2 Portable Agent Bundle v1 候选格式、§8.3 导出语义"普通备份/导出可在一致水位快照上进行，不停止源 Agent"）；[ADR-0037](ADR-0037-domained-memory-items.md)（0.7.0 基石——sensitivity 列"0.7.5 Bundle 脱敏消费"、supersedes/derived_from 血缘预留、"导出范围按 namespace×scope 组合"）；[ADR-0038](ADR-0038-memory-curation-timeliness-superseded.md)（0.7.1 时效口径——active 排除 valid_to < now）；[ADR-0039](ADR-0039-memory-recall-fts-authz.md)（0.7.2 授权过滤先行——"他域命中不入结果与计数"）；[ADR-0040](ADR-0040-run-checkpoints.md)/[ADR-0041](ADR-0041-cross-harness-resume.md)（Checkpoint 事实源与 LatestForResume 选取口径）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.5 唯一交付特性：Portable Agent Bundle 导出——范围选择、摘要与脱敏报告）。

## 背景与问题

0.7 线已交付记忆基石/策展/召回/Checkpoint/续办五件：单实例内的可恢复事实闭环完成。RFC §8 的下一步是**跨实例可移植**——Agent 连同其授权记忆、任务承诺与检查点打成版本化便携包，供 0.7.6 导入预检、0.7.7 激活接管、0.7.8 删除传播消费。本件只交付**导出面**三件事：

1. **范围选择**——用户显式圈定包内容（记忆命名空间、任务集合、敏感记忆是否随行）；
2. **摘要**——manifest.json 逐文件 sha256 内容摘要 + 水位记录（完整性检查与导入侧陈旧判定的依据）；
3. **脱敏报告**——redaction-report.json 逐项说明省略、排除与恢复限制（RFC §8.2："省略项、断链项、敏感内容处理及恢复限制"）。

导入预检（0.7.6）、激活（0.7.7）、墓碑与删除传播（0.7.8）不在本件。

## 决策

### 1. 包格式 v1（§8.2 候选格式落定）

产物 = 单个 zip 文件（`mosaic-agent-bundle.zip`），包内路径**只允许包内相对路径**（生成侧保证；`/` 分隔；无 `..`/绝对路径——测试钉住）。布局：

```text
manifest.json                 格式版本、来源、身份、导出模式、水位、文件摘要
agent/profile.json            身份事实：Agent + 三向别名 + 可执行项引用（无认证凭据）
slots/specs.json              现任职责：active Binding + 所需 SlotSpec 版本与内容
memory/items.ndjson           经授权记忆（active 且时效有效），一行一条
work/checkpoints.ndjson       每选中任务一条续办检查点（LatestForResume 投影）
work/commitments.json         任务承诺（契约快照 + 原对象映射；导入默认暂停——0.7.6 消费）
context/manifests.ndjson      上下文引用与证据清单（任务→检查点/工件引用/房间来源）
dependencies.json             运行依赖（harness 可执行项标识 + SlotSpec 运行要求；无凭据无路径）
redaction-report.json         脱敏报告（§4）
```

**v1 边界**（候选格式的裁剪，均进脱敏报告 notes，导入侧不得视为缺陷）：

- **`artifacts/` 不携实体文件**：checkpoint/delivery 的 artifact_refs 是自由字符串（无统一解析面），v1 原文保留在 checkpoints/commitments 内、不搬运文件；报告 `artifact_policy: "refs_only"` 声明"目标侧可访问性未判定"（RFC："大资产可保留引用，但报告必须说明目标是否可访问"）。文件搬运待引用解析面统一后递进。
- **`room`/`slot` 命名空间暂不开放导出**：这两域承载他参与方的会话派生知识，导出授权口径（房间名册/任职范围核对）尚未建；v1 命名空间枚举 = `agent | workspace | mission`，传 room/slot → 400。
- **embedding 不存在**（Mosaic 记忆无向量索引），报告 notes 说明"无 embedding 随包"。

### 2. 范围选择（ExportScope）

```
POST /v1/agents/{agent_id}/bundle-exports
{ "namespaces": ["agent","workspace"],      // ⊆ {agent, workspace, mission}，缺省 ["agent","workspace"]，重复/未知/room/slot → 400
  "mission_ids": ["msn_..."],               // 缺省 null = 自动：该 Agent 全部非终态在册任务；显式数组 = 恰好所选（终态也可选）
  "include_sensitive": false }              // 缺省 false：sensitive 记忆省略并报告
```

- **mission_ids 语义**：null = 自动圈定（非终态 owned——`ready/running/waiting/paused/canceling`；draft 属未派发草稿不带走）；显式数组 = 精确圈定，**逐条校验存在且当前 owner == agent_id**，不符 → 400（透明拒绝，不静默缩圈）。`mission` 命名空间记忆的 scope 集合 = 选中任务集（未选任务的结构性不查询——授权口径见 §3）。
- **导出是只读快照**：不停源 Agent、不加锁、不改任何域状态（RFC §8.3"普通备份/导出"行）。Agent 任意生命周期状态可导出（退役 Agent 的知识归档是正当场景）。
- **Agent 不存在 → 404**（personalagent Get 单源判定）。

### 3. 授权与脱敏口径（0.7.0 标记 + 0.7.2 过滤先前的导出侧消费）

**过滤先前的结构化应用：未授权 scope 根本不进入查询**（比"查了再滤"更强）——

| 命名空间 | scope 解析 | 授权依据 |
|---|---|---|
| `agent` | 恒 `agent_id` | 私有记忆自持（他人 agent 域结构性不可达） |
| `workspace` | 恒 `default` | 单工作区共享知识 |
| `mission` | 选中任务集（owned） | 责任归属 = 授权（同 0.7.2 mission 域列表口径） |

入包条件（与 0.7.1/0.7.2 同一 WHERE 语义）：`status = active` **且** 时效有效（`valid_to` 空/NULL 或 ≥ now——`include_expired` 导出侧不开放，过期知识不移植）；`sensitivity = sensitive` 且未显式 include_sensitive → **省略**（默认安全缺省）。`proposed`（策展未决）与 `superseded`（已终结，替代品为现行 active 项）不入包——纠错史不移植是口径而非缺陷，替代链在包内由 active 项的 supersedes 字段保留血缘。

**一切排除都计数进脱敏报告**（§4）；包内计数（manifest 水位、响应 counts）只含入包项——"他域/被排除命中不入计数"的 0.7.2 纪律原样平移。

### 4. 脱敏报告（redaction-report.json，同时内联在创建/详情响应）

```json
{ "bundle_id": "bdl_...", "generated_at": "...",
  "omitted_sensitive": { "count": 2, "item_ids": ["mem_...", "mem_..."] },   // 只 ID 不内容/标题——审计可用且不泄敏感面
  "excluded_by_rule": { "expired": 1, "proposed": 3, "superseded": 5 },      // 口径排除计数（范围外不记——那是选择不是省略）
  "recovery_limits": ["msn_x 无任何检查点——导入后从契约裸启动"],            // 逐任务恢复限制
  "artifact_policy": "refs_only",
  "notes": ["凭据/令牌/原生会话材料不随包（设计边界）", "artifact_refs 保留标识不携文件——目标侧可访问性未判定", "无 embedding 随包"] }
```

### 5. 摘要与水位（manifest.json）

- **逐文件 sha256 + 字节数**（`files[]`）——完整性检查用（"不等同于可信签名"，RFC 原文）；manifest 自身不在 files 内（自摘要在数学上不闭合）。
- **水位**：`exported_at`（RFC3339Nano UTC）+ 每任务 `{mission_id, contract_version, checkpoint_version, checkpoint_source}` + 每命名空间入包计数——0.7.6 导入预检据此判定陈旧与漂移。
- **一致水位说明（v1 诚实边界）**：各源（身份/任职/任务/检查点/记忆）顺序独立读取，非单事务快照——导出期间并发的契约修订/策展可能产生跨源轻微斜切；水位记录各源读取合并时刻，导入侧以任务级 version 比对治理（0.7.6）。ADR 明记此边界，不假装原子。
- **来源实例**：`product_version` + `bundle_format: "mosaic-agent-bundle/1"`——不记主机名/用户名（包要旅行，最小事实面）。

### 6. 存储、生命周期与 HTTP 面

- 产物落数据目录 `bundles/<bundle_id>/`：`mosaic-agent-bundle.zip`（便携件）+ `export.json`（边车 = 创建响应全文，供列表/详情端点零重算读取）。`bundle_id = bdl_<ts36>_<hex>`（backup 同族）；`GET /v1/bundle-exports/{id}` 的 id 先过 `^bdl_[0-9a-z_]+$` 白名单再拼路径（防穿越，backup 纪律）。
- **零 migration**：导出记录在库外文件系统（backup 同例）；0.7.0 预留的 supersedes 血缘列随 items 原文携带，不动表。
- 三端点（写门三层照旧）：

| 端点 | 语义 |
|---|---|
| `POST /v1/agents/{agent_id}/bundle-exports` | 创建导出 → 201 AgentBundleExportView（含脱敏报告内联）；400 范围不合法/任务非所属；404 agent_not_found；503 bundle_unavailable（未装配） |
| `GET /v1/agents/{agent_id}/bundle-exports` | 该 Agent 导出列表（新→旧，Summary 视图） |
| `GET /v1/bundle-exports/{bundle_id}` | 详情（View 全量，传输前核对脱敏报告用） |

- 删除端点不做（0.7.8 删除传播统一治理旧包生命周期；文件管理暂由数据目录承担）。

### 7. 域结构（internal/bundle 新包，端口闭包——依赖方向同 execctx）

`bundle.Service` 不 import personalagent/slot/mission/checkpoint/memory 任一域；`Ports` 全为闭包 + **域内投影类型**（AgentFacts/SlotFact/MissionFact/CheckpointFact/MemoryFact——字段同构、app 闭包做映射；execctx CheckpointFacts 先例），app.go 组合：

```go
type Ports struct {
    Agent      func(ctx, agentID) (AgentFacts, bool, error)       // 身份 + 别名 + 可执行项引用
    Slots      func(ctx, agentID) ([]SlotFact, error)             // active Bindings + SlotSpec 版本内容
    Missions   func(ctx, agentID) ([]MissionFact, error)          // owned 全量（含终态；bundle 域按选择圈定）
    Checkpoint func(ctx, missionID) (CheckpointFact, bool, error) // LatestForResume 投影（0.7.4 口径复用）
    Memories   func(ctx, namespace, scopeID string) ([]MemoryFact, error) // 该 scope 全量（任意状态——报告需见被排除项；域内按 §3 过滤+计数）
}
```

导出编排：校验 → 逐源读取 → 过滤/脱敏（单处口径）→ 确定性渲染（固定文件序、ndjson 行序 = 查询序）→ zip（临时文件写全后原子 rename 入 `bundles/<id>/`）→ manifest 收尾（摘要汇总）→ 边车落盘 → 返回。`View`/`Summary` 的 JSON 形状即 API 契约形状（backup Summary 先例——handler 零转换）。

### 8. 前端（SettingsPage 数据区新增"Agent 迁移包"面板）

Agent 选择（复用 GET /v1/agents）→ 命名空间勾选（默认 agent+workspace）→ 任务口径（自动/显式多选，显式时列 owned 任务）→ include_sensitive 开关（默认关，旁注"敏感记忆默认不随包"）→ 导出按钮 → 结果卡（bundle_id/大小/计数/zip 路径 + 脱敏报告全文展示——省略与恢复限制一目了然）+ 历史列表（Summary）。

### 9. 测试与验证

bundle UT（fake Ports + 临时目录）：范围校验族（未知/重复/room/slot 命名空间 400、任务非所属 400、agent 404）；默认范围与自动任务圈定；sensitive 默认省略（计数+ID 进报告、不入包不入计数）/显式携带（报告 notes 转记）；时效与状态口径排除计数；mission 记忆仅随选中任务；checkpoint 业务优先系统兜底 + 无检查点 recovery_limits；zip 布局断言（§8.2 文件集、路径无 `..`/绝对、`/` 分隔）；manifest 摘要与实际文件 sha256 逐一相符（manifest 自身缺席 files）；ndjson 每行可解析；List/Get 边车读取 + bundle_id 白名单拒绝。httpapi IT：三端点全语义（201/400/404/503/列表序/详情）。真机冒烟：造敏感+过期+多命名空间记忆与任务检查点 → 导出 → unzip -l 布局 + sha256 逐文件核对 + 报告内容人检。

## 后果

- 跨实例迁移链首环落地：导出（本件）→ 导入预检（0.7.6 消费 manifest 摘要/水位/报告）→ 激活接管（0.7.7 消费 commitments 默认暂停语义）→ 删除传播（0.7.8 消费血缘）。
- 0.7.0 的 sensitivity 标记、0.7.2 的过滤先前纪律首次在导出面被消费——记忆域"可策展可召回"之外补上"可选择带走"。
- 包内内容摘要为导入侧完整性校验提供依据，但**不是签名**——防篡改信任模型（如签名/封印）另行决策。
- artifacts 文件搬运与 room/slot 命名空间导出是显式欠账，登记于本 ADR 边界节，待引用解析面与授权口径成熟后递进。
