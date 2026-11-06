# Mosaic v0.7.1 — 记忆策展与纠错：接受、修订、否决与 superseded（0.7 记忆与可移植恢复领域·第二件）

0.7.0 交付了分域 MemoryItem 的显式写入与隔离列表，但记忆还是"只写不改"：候选没有归宿、修订没有版本记录、被替代的条目一直占着现行口径、有效期形同虚设。本件按 RFC-0018 §7.2 启用策展语义——0.7.0 预留列（status/supersedes/edit_history）直接生效，**零迁移**。两轮独立审校（5 + 1 = 6 项）全部关闭后定版。

## 新增

- **状态机**：`proposed --accept--> active`（幂等——重复确认无害，不递增版本）；`reject`（候选否决）/ `withdraw`（active 撤回）→ `superseded` **唯一终结态，不可复活**（409 `memory_terminal`——与 0.7.8 旧包防复活同基调）。Create 写入面 += 可选 `status`（默认 active——高可信明确指令直接生效；不确定候选留 proposed 待策展；终结态不可直造）。
- **原位修订 `POST /v1/memory/items/{id}/revise`**：title/content/来源/时效/敏感度/置信度可改（归属与状态另有动作面）；`expected_version` CAS；版本递增 + **edit_history 恰追加一条**（append-only——"人工纠错具有明确版本记录"）。
- **显式替代 `POST /v1/memory/items/{id}/supersede`**：**单事务双动作**——旧条目终结（留痕）+ 新条目创建（`supersedes=[旧 id]`；replacement 可跨域——迁移纠错是合法替代）；CAS 失败整体不落。事实随时间变化不用替代（修订调整有效期或新建条目）。
- **时效口径**：默认 active 口径**排除已过期条目**（`valid_to < now` 时刻比较）；`include_expired=true` 显式可见（历史查询仍能解释当时依据）；proposed/superseded 显式口径不过滤（策展审视看全量）。
- **并发纪律**（一轮审校加固）：修订/替代**绑定读快照**提交（请求版本与快照不符即拒）；CAS 为**版本 + 状态双基准**——并发撤回的同名版本不能让旧修订复活终结条目、并发接受不被陈旧修订撤销（留痕不抹）；接受交错幂等收敛。
- **时间语义**：写入/修订/替代三时间字段统一 **UTC 规范化（RFC3339Nano 无损——小数秒保留）**；旧偏移格式行（sqlite `julianday` 返回 NULL）默认口径保守包含不误隐。
- **SPA**：`/memory` 页扩展——状态口径过滤（现行/候选/历史）+ 含已过期开关 + 条目操作（接受/否决与撤回/修订表单/替代表单——预填原敏感度不降级）+ 版本留痕链与替代链显示。

## 修复（两轮独立审校 6 项）

- **一审（2 P1 + 3 P2）**：快照绑定 + 双基准 CAS（并发撤回不复活终结/接受不被撤销——审计不抹）；`include_expired` 客户端 URL 编码；时间统一 UTC（旧数据保守包含）；替代表单保留敏感度。
- **二审（1 P2）**：UTC 规范化改 RFC3339Nano——小数秒区间不塌缩（仅改标题修订 400、截止 `.900Z` 提前隐藏、`observed_at` 失精度三面关闭）；状态 CAS 测试补强（同版本状态失配拒 + 数据/审计不变断言）。

## 升级说明

- 无迁移（0.7.0 的 migration v32 已带全策展列）。
- **行为变更**：列表默认口径自 0.7.1 起排除已过期 active 条目（RFC §7.2"过期不默认参与当前召回"）——`include_expired=true` 回到全量。
- 新端点四条（accept/reject/revise/supersede，owner 写门）；`MemoryItemCreateRequest` += 可选 `status`。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.2）与 [ADR-0038](docs/design/adr/ADR-0038-memory-curation-timeliness-superseded.md)（v0.3，含两轮审校修订记录）。
- 下一件：0.7.2 授权过滤与 FTS5 召回（"先过滤权限，再排序和计数"——本件的 active + 未过期口径即召回前置纪律）。
