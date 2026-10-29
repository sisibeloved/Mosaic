# ADR-0020：SlotInstance 职责实例与独立知识命名空间（0.5.3）

- 状态：Accepted（2026-10-29，随 0.5.3 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.1 对象边界、§5.2 实例与模板/任职的关系、§7.1 Slot Knowledge 记忆分域、§7.2 MemoryItem.namespace、§13.5"首版单 active 任职"待定项）、[ADR-0019](ADR-0019-slotspec-versioned-template.md)（0.5.2 SlotSpec 模板——实例的引用对象）、交付计划 v2.00（0.5.3 唯一交付特性：SlotInstance：职责实例及独立知识命名空间）

## 背景与问题

0.5.2 交付了职责的**定义**（SlotSpec 版本化模板）；0.5.3 交付职责的**存在**：
某个作用域里实际承担着的一份具体职责（SlotInstance）。RFC-0018 对它的裁定：

1. **同一模板可实例化多份**（§3.1）——实例与模板是一对多；
2. **换人后实例保留**（§3.1）——实例属于作用域（Workspace / Goal / Mission），
   不属于任何任职者；任职是 0.5.4 Binding 的事，Binding 才固定模板版本；
3. **实例单独持有 `knowledge_namespace`**（§5.2）——职责知识（工作规范、
   环境说明与积累，§7.1）挂在实例的命名空间下，**不能挂到任职者的私有库**；
   该命名空间即未来 MemoryItem 的 `namespace` 值（§7.2）。

## 决策

### 1. 域内扩展（internal/slot，不另立包）

SlotSpec 与 SlotInstance 同属 Slot 域（RFC-0018 §4.3 的 `internal/slot`
落点覆盖整段 §5）；实例类型与服务并入既有包。

```
SlotInstance = {
  slot_instance_id     si_ 前缀生成式（时间有序 + 随机，ag_ 同族）——
                       实例一对多于模板、按需创建，不用户命名
  slot_spec_id         模板谱系引用（不固定版本——Binding 才固定，
                       §5.2"Binding 固定版本；升级先排空再切换"）
  created_from_version 实例化时模板的最新版本号（审计事实："从哪版来"，
                       非钉住——模板后续演进不自动改变实例）
  scope                作用域：枚举 {"workspace"}（0.5.3 仅 Workspace；
                       goal/mission 随 0.6 域扩展——契约先带字段，非破坏）
  title                展示标题 1–64 字；缺省 = 实例化时模板最新版名称的
                       存档副本（模板后续改名不回写实例标题）
  knowledge_namespace  slotns_ 前缀生成式，全局唯一、终身稳定、永不重派
  created_at
}
```

- **无状态机**：RFC §3.2 未给实例定义状态（active/archived 之类皆无依据）；
  实例的存在性即其全部状态。不做删除/归档（无消费者；与 0.5.2 模板同纪律）。
- **命名空间是登记事实，不是存储后端**：0.5.3 交付的是唯一性、稳定性与
  解析面（实例 → namespace）；Slot Knowledge 的本体存储随 0.7 分域记忆
  落地，届时 MemoryItem.namespace 直接使用本值。命名空间与实例同生，
  UNIQUE 约束兜底生成器碰撞。

### 2. 存储：SQLite migration v10（同库）

```
CREATE TABLE slot_instances (
  slot_instance_id    TEXT PRIMARY KEY,
  slot_spec_id        TEXT NOT NULL,
  scope               TEXT NOT NULL DEFAULT 'workspace',
  title               TEXT NOT NULL,
  knowledge_namespace TEXT NOT NULL UNIQUE,
  created_from_version INTEGER NOT NULL,
  created_at          TEXT NOT NULL
);
```

插入走既有 BEGIN IMMEDIATE 临界区（ADR-0018 v0.2 #1 全库约定）；模板
存在性校验在服务层（建实例先查谱系——跨表引用不依赖 FK，与身份域同口径）。

### 3. API 面（契约冻结，openapi.yaml 权威）

- `POST /v1/slots/instances`：`{slot_spec_id, title?}` → 201
  `SlotInstanceView`；谱系未知 404 `spec_not_found`；标题校验失败 400
  `invalid_instance`；写门同全库（Origin/CT/OwnerToken）。
- `GET /v1/slots/instances`：实例列表（创建序），视图携带模板**现势**
  信息（spec.name 取最新版名称、spec.latest_version 现算连接——与身份
  域可执行引用同口径；实例自身字段存档不受模板演进影响）。
- `GET /v1/slots/instances/{slot_instance_id}`：详情 / 404
  `instance_not_found`。

### 4. Store 端口扩展

`CreateInstance(ctx, inst)`、`ListInstances(ctx)`（创建序）、
`InstanceByID(ctx, id)`。错误复用 `ErrNotFound`（谱系/实例同码，HTTP 层
按端点区分 code）。MemStore 同语义（命名空间唯一互斥）。

## 非目标（0.5.3 明确不做）

- 不做 Binding / 任职 / 交接 / 排空（0.5.4）；"每实例最多一位 active
  任职者"是 Binding 不变量，实例侧无从谈起。
- 不做准入与 readiness report（0.5.5）。
- 不做 Slot Knowledge 的读写面（0.7 分域记忆）；命名空间仅登记。
- 不做实例改名 / 删除 / 归档 / scope 变更（无消费者；有真实引用者后
  另立）。
- 不做 goal / mission 作用域（0.6）。

## 测试策略（先行红→绿）

| 层 | 用例 |
|---|---|
| UT slot | 建实例默认标题 = 模板最新名存档（模板再追加版本不改实例标题）；显式标题校验（空/超长 400 语义）；未知谱系 ErrNotFound；同模板多实例 → 命名空间互异且稳定；列表创建序 + 模板现势连接（latest 名称/版本随追加演进）；单查/404；并发建 8 实例 → 8 个唯一命名空间、零重复 ID |
| IT sqlite | v10 新库建表 + 迁移登记；v0.5.2 旧库升级（截 v10 段建库 → 表可用、登记恰一条、重开幂等）；knowledge_namespace UNIQUE 约束实测（裸插重复 → 约束错）；并发建 8 实例全唯一 |
| API httpapi | 建 201 形状（si_/slotns_ 前缀、created_from_version、默认标题）；未知谱系 404 spec_not_found；空标题 400；写门（跨源 403 / 非 JSON 415）；列表现势连接（模板追加后实例视图 latest 演进但标题不变）；详情/404 |

## 后果

- 正面：职责有了可挂靠的存在实体与知识地址——0.5.4 Binding 的
  `slot_instance_id` 外键、0.7 记忆的 namespace 分域、§12 职责页的实例
  列表全部就位；模板演进与实例稳定性解耦（存档标题 + 现势连接各司其职）。
- 代价：实例只增（个人版规模可忽略）；模板现势连接使列表多一次谱系
  聚合查询（条目 = 实例数，微秒级）。
- 放弃方案：实例固定模板版本（违反 §5.2——固定版本是 Binding 的职责，
  实例钉版会让"升级先排空再切换"失去落点）；用户命名实例 ID（一对多
  按需创建，生成式防碰撞与隐私）；实例状态机（RFC 无依据，不发明）；
  knowledge_namespace 直接用 slot_spec_id（同模板多实例会共享命名空间，
  违反"每实例一份职责知识入口"）。

## 修订记录

| 版本 | 日期 | 说明 |
|---|---|---|
| v0.2 | 2026-10-29 | 审校修订（5 项 P2）：#1 列表组装改"先实例后模板"读取顺序（模板版本只追加 ⇒ 现势恒 latest ≥ created_from_version；反序会被两读间的并发创建/追加打出空名/v0 矛盾快照）+ 服务层防御钳制（UT 陈旧快照注入钉住）；#2 详情现势读取错误传播（故障注入 UT：不再吞错返回 200 空名，端点 500）；#3 标题三态区分——省略（nil）缺省取模板名、显式空串/纯空白 400（端点反例钉住，不与省略合流）；#4 前端实例列表读取失败单列错误态 + 保留最近成功列表 + 显式重试（不再永远"加载中"）；#5 模板提交成功后同步刷新实例现势连接（存档标题/created_from_version 服务端本就不变） |
| v0.1 | 2026-10-29 | 初稿（随 0.5.3 实施） |
