# ADR-0019：SlotSpec 版本化职责模板（0.5.2）

- 状态：Accepted（2026-10-29，随 0.5.2 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.1 SlotSpec 对象边界、§5.1 内容语义与候选示例、§4.3 `internal/slot` 落点、§13.1 P1）、[ADR-0018](ADR-0018-agent-lifecycle.md)（0.5.1 生命周期——Slot 任职者将挂靠该身份）、交付计划 v2.00（0.5.2 唯一交付特性：SlotSpec 版本化模板：职责、工具、上下文与交付要求）
- 上游需求注记：0.5 领域出口含 AT-07（必需缺失 blocked、可选降级）——0.5.2 只交付**模板本身**（Schema 定形与版本化存取）；能力核对、权限交集与 readiness report 是 0.5.5 的准入交付，消费本版定形的 `tools.required/optional` 分层。

## 背景与问题

RFC-0018 把"职责"从 Agent 与 Room 中拆出：一个 Agent 通过 Slot 承担职责，
而职责的定义本身是**可复用、可版本化、不可变**的模板。§5.1 给出了候选语义
示例并明确"正式 Schema 在实现阶段加入必填、枚举和大小限制"——本 ADR 完成
该定形，并交付模板的版本化存取面。SlotInstance（0.5.3）与 Binding（0.5.4）
随后在此模板上实例化与任职；本版不含任何实例、任职、准入行为。

## 决策

### 1. 新域 `internal/slot`（RFC-0018 §4.3 指定落点）

- 聚合根 `SlotSpec`：**不可变版本行**——`(slot_spec_id, version)` 复合主键；
  `slot_spec_id` 为模板谱系标识（用户选定、跨版本稳定），`version` 谱系内
  自增（1 起）。无 UPDATE / DELETE 路径：模板只能追加新版本，错误定义以
  新版本取代，历史版本永不清除（Binding 未来固定版本引用的审计前提）。
- **版本行本身即不可变历史**——不再另设 slot 事件流（放弃方案：模板是
  append-only 快照，行为状态机（Binding）在别的聚合上；与 ADR-0018 中
  "有状态转移才建流"的裁定同口径）。
- `slot_spec_id` 形态：`slot.` 前缀 + 小写 slug（`^slot\.[a-z0-9][a-z0-9._-]{0,62}$`，
  对齐 RFC 示例 `slot.research-lead`；前缀命名空间防跨域 ID 碰撞）。
  与 `ag_`（生成式）不同，模板 ID 由用户选定——它要进配置、导出与引用
  （§9 `slots/specs.json`），人类可读性是功能而非装饰。

### 2. 内容 Schema（§5.1 候选语义的正式定形）

```
SlotSpecContent = {
  name            string  必填 1–64
  responsibility  string  必填 1–512（职责陈述）
  deliverables    []string 必填 1–16 项，每项 1–256（交付规范——Slot 的本质字段）
  skills          []{ref string（^[/\w.-]{1,128}$）, version int ≥1}  可选 ≤16
                  （可审查的版本化工作方法引用；仅引用，不含可执行体——
                   "安装或导入不自动执行其中脚本"）
  tools           {required []string, optional []string}  可选
                  能力名（^[a-z0-9][a-z0-9._-]{0,63}$）各 ≤32；required 与
                  optional 不得相交（AT-07 的 blocked/降级分层预埋）
  context         {required []string, optional []string}  可选（上下文域引用，同上约束）
  delegation      {allowed []enum{existing_agent, ephemeral_worker}, max_depth 0–8}
                  可选；缺省语义 = 不允许委派（安全缺省，非"无限制"）
  runtime         {required []string ≤16, workspace_access enum{none,read,read_write}}
                  可选（能力要求，非安装路径——实际连接由 0.5.4 Binding 解析）
  handoff         {required []string ≤16}  可选（交接必备件）
}
schema_version 恒为 1（演化 = 新 schema_version + 迁移，不在 v1 上加可变字段）
```

- 校验单源在 `internal/slot` 服务层（必填/上限/模式/枚举/required-optional
  不相交）；OpenAPI 定义传输形状并经 apigen 生成类型，语义校验不做两份。
- 请求体 1MiB 上限 + `DisallowUnknownFields`（与 harness 登记同口径）。

### 3. 存储：SQLite migration v9（同库，ADR-0008）

```
CREATE TABLE slot_specs (
  slot_spec_id    TEXT NOT NULL,
  version         INTEGER NOT NULL,
  schema_version  INTEGER NOT NULL,
  name            TEXT NOT NULL,
  responsibility  TEXT NOT NULL,
  content_json    TEXT NOT NULL,   -- 完整内容（权威事实；name 冗余列便于列表直查）
  created_at      TEXT NOT NULL,
  PRIMARY KEY (slot_spec_id, version)
);
```

- **追加版本走 BEGIN IMMEDIATE 临界区**（ADR-0018 v0.2 #1 的全库约定）：
  读 latest → version+1 → 插入，一次事务；并发追加串行化为连续版本号，
  无空洞、无重复。
- 旧库（≤ v0.5.1）开库即建表（schemaDDL 幂等段），无数据回填需求
  （新域首表）；迁移登记 version 9。

### 4. API 面（契约冻结，openapi.yaml 权威）

写面过生产写门（guardWrite：Origin + Content-Type + OwnerToken）：

- `POST /v1/slots/specs`：建谱系（首版 version=1）；`slot_spec_id` 已存在 →
  409 `spec_exists`；校验失败 400 `invalid_spec`。
- `POST /v1/slots/specs/{slot_spec_id}/versions`：追加下一不可变版本
  （version=latest+1）；谱系未知 404 `spec_not_found`。
- `GET /v1/slots/specs`：谱系列表（id / name / latest_version / 版本数 /
  created_at / updated_at），谱系创建序。
- `GET /v1/slots/specs/{slot_spec_id}`：最新版本全量内容；404。
- `GET /v1/slots/specs/{slot_spec_id}/versions`：全版本升序（含内容）；
  404。

### 5. Store 端口

`CreateSpec(ctx, content)`（含 slot_spec_id；建行即首版）、
`AppendVersion(ctx, slotSpecID, content)`、`ListSpecs()`（谱系摘要）、
`LatestSpec(id)`、`VersionsOf(id)`。错误：`ErrSpecExists` / `ErrNotFound` /
`ErrInvalidSpec`。MemStore 同语义（含并发追加的互斥）。

## 非目标（0.5.2 明确不做）

- 不做 SlotInstance / 知识命名空间（0.5.3）；不做 Binding / 交接 / 排空
  （0.5.4）；不做能力核对、权限交集与 readiness report（0.5.5）。
- 不做模板删除 / 归档 / 弃用标记：版本不可变，取代即追加；生命周期语义
  随实例域（0.5.3+）有真实引用者后再议。
- 不做模板导入导出（随 0.7 便携 / 0.10 备份域）；不做 skills 的实际装载
  （`skills` 是引用清单，本版不解析、不执行）。
- 不做 UI 模板编辑器的高级形态：设置页"职责"区提供列表 + 新建（核心字段）
  + 版本明细查看；完整职责页（RFC-0018 §12 五入口）随 0.5.3 实例面立项。

## 测试策略（先行红→绿）

| 层 | 用例 |
|---|---|
| UT slot | 校验族（缺必填/超限/模式/枚举/required-optional 相交全拒）；建谱系 version=1；追加 version 递增且旧版本内容不变（不可变钉住）；同内容追加仍新版本（快照语义）；谱系碰撞 ErrSpecExists；未知谱系 ErrNotFound；并发追加 8 路 → 版本连续唯一 |
| IT sqlite | v9 新库建表 + 迁移登记；v0.5.1 旧库升级（截 v9 段建库 → Open 后表可用、迁移登记恰一条 v9、重开幂等）；PK (spec_id, version) 唯一约束；并发追加 8 路恰得 2–9 连续版本、无重复无空洞 |
| API httpapi | 建 201 形状；重复建 409 spec_exists；追加 201 版本递增；列表/最新/版本序列端点形状与 404；校验 400 invalid_spec；写门（无 Origin 跨源 403 / 非 JSON 415）；读面无门 |

## 后果

- 正面：职责定义有了不可变权威（0.5.3/0.5.4/0.5.5 的挂靠基座）；工具
  required/optional 分层为 AT-07 准入预埋；模板版本行天然可审计。
- 代价：模板错误只能以新版本取代（无删除——个人版规模可忽略）；内容
  JSON 冗余存 name 一列（列表免解包）。
- 放弃方案：slot 事件流（版本行即历史，无状态转移可记）；可变模板 + 
  changelog 字段（弱审计，Binding 固定版本引用失去依据）；生成式 ID
  （模板要进配置与导出，人类可读是功能）；删除/弃用端点（无引用者前
  语义未定，先不给）。

## 修订记录

| 版本 | 日期 | 说明 |
|---|---|---|
| v0.1 | 2026-10-29 | 初稿（随 0.5.2 实施） |
