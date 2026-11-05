# Mosaic v0.7.0 — 分域记忆基石：MemoryItem 五命名空间（0.7 记忆与可移植恢复领域·第一件）

0.6 收官后长期记忆仍是**单域**：房间收束胶囊是唯一长期记忆形态，Agent 私有经验、工作区项目知识、Mission 阶段积累、Slot 职责规范没有归属容器。本件按 RFC-0018 §7.1/§7.2 交付记忆域基石：**MemoryItem 五命名空间 + 锚定归属 + 读写面**——0.7 线的策展（0.7.1）、召回（0.7.2）、Checkpoint（0.7.3）、Bundle（0.7.5+）都叠在这块基石上。两轮独立审校（4 + 1 = 5 项）全部关闭后定版。

## 新增

- **五命名空间与锚**（RFC §7.1 数据分层）：每条记忆归属恰一命名空间并锚定归属对象——`agent`（agent_id，长期偏好/经验/公开人格配置）、`room`（room_id，项目知识/共识与异议）、`workspace`（恒 `default`——单工作区事实）、`mission`（mission_id，阶段经验/假设/证据整理）、`slot`（slot_instance_id，职责实例工作规范）。**锚对象不存在写入即拒**（409 `memory_scope_not_found`——app 装配组合 identity/room/mission/slot 四域存在性闭包）。
- **类型与权限域正交**：`semantic / episodic / procedural`（知识/经历/方法）；**来源引用**（≤16 条——每条记忆可追溯到事件/文档版本/显式操作）；**时效分记**（`observed_at` 何时知道 + `valid_from/to` 何时有效）；**敏感度**（normal/sensitive——0.7.5 Bundle 脱敏消费）与**置信度**（人工写入默认 1.0）。表结构一次带全 §7.2 拟定字段——策展状态机（0.7.1）与授权过滤/FTS 召回（0.7.2）直接启用预留列，不再动基础迁移。
- **读写面**：`POST /v1/memory/items`（owner 写门 + 严格拒收未知字段）、`GET /v1/memory/items?namespace=…`（**namespace 必填隔离**——跨域列表不存在，越域内容与存在性不泄漏；scope/kind/status 过滤，status 缺省 active；创建序降序 + `next_cursor` 游标——游标只在本域内定位，外域 ID 一律空页）、`GET /v1/memory/items/{id}`（未知 404）。SQLite migration **v32**（memory_items + 双索引，幂等重放）。
- **边界**：房间收束胶囊与 memory.edited（RFC-0007）保持房间事件流权威**不动**；`room` 命名空间 MemoryItem 是显式写入的房间派生记忆（自动抽取随 0.7.1 按策展纪律接入——"不能从未经授权的上下文提炼后转入更宽可见域"）。
- **SPA**：`/memory` 记忆页——五命名空间 tab、归属锚过滤、记忆卡列表（kind 徽标/时效/来源数/敏感标）、详情展开、显式写入表单（workspace 锚锁定 default、来源按行拆分、时效区间可选）；写入后按当前浏览条件刷新。

## 修复（两轮独立审校 5 项）

- **一审（1 P1 + 3 P2）**：`source_refs` 空数组三层规范化（省略/空数组不出 `null`，null 列读回兼容——记忆页崩溃面关闭）；创建后刷新走当前浏览条件；续页代次检查 + 在途限制（跨域混入与重复追加关闭）；游标域内定位（双存储——外域 ID 一律空页）。
- **二审（1 P2）**：一审刷新修法的闭包残留——写入成功只发 `refreshTick` 信号，列表 effect 以当次渲染条件重查（提交等待期间切换筛选不被旧闭包覆盖）。

## 升级说明

- v0.6.13 数据库首次启动自动执行 migration v32（memory_items 表 + 索引，幂等 DDL），无需手工步骤；v32 首版候选（9683c9f）写入的 `null` source_refs 列读回自动兼容为空数组。
- 无新增必填请求字段；新端点 `/v1/memory/items` 族为本版新增（写端点 owner token 门控）。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.1/§7.2/§7.4，AT-04 基石）与 [ADR-0037](docs/design/adr/ADR-0037-domained-memory-items.md)（v0.3，含两轮审校修订记录）。
- 下一件：0.7.1 记忆策展与纠错（来源、时效、接受、修订与 superseded——本件预留列直接启用）。
