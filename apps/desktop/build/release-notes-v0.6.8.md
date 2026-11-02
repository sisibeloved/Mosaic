# Mosaic v0.6.8 — DeliveryPackage 成果提交（0.6 Mission 与本地执行领域·第九件）

0.6.5–0.6.7 把"执行"做实（闭环、控制、崩溃诚实）；本件把 RFC-0018 §3.3 的裁定落成事实：**执行成功 ≠ 成果提交 ≠ 验收通过**——三个互不替代的事实。本版交付第二个：结构化交付包的提交、留痕与读面，且提交**不自动送验**（AT-10）。

## 新增

- **DeliveryPackage 域**（`internal/delivery`，migration v21 一表 / ADR-0031）：append-only 交付包行——溯源来源 Run（run_id / attempt / generation）与契约版本；RFC §10.2 最小结构：成果引用（引用非正文）、验收逐项对应 `[{criterion, evidence}]`（criterion 为契约版本内条文快照，evidence **可空**——"先交付再补证"）、验证证据（负责人验证过程，不能仅拼接执行回复）、未解决项、风险、动作回执（本版引用串）、建议记忆变更（随 0.7 消费）、备注。补证 = 新包，旧包不改写；"当前交付包" = 最新提交。
- **提交面 `POST /v1/missions/{id}/delivery-packages`**（submit_result）：来源 Run 须**可交付**——`completed`，或 `unknown` 且处置 `verified_completed`（0.6.7 用户 attest 的完成）；`generation` 须等于 Run 实际代次（409 stale_generation——调用方视图陈旧，拒绝而非静默改写）；`expected_version` 须等于当前契约版本（409 version_conflict——验收对应锚定契约版本）；Run 须属该 Mission；canceled Mission 拒绝（读面照常）。**完备性不在提交面检查**——缺项由 0.6.9 `request_review` 拒绝。
- **不自动送验 = 对 Mission 零副作用**：不改状态/版本/updated_at、不写 mission_events、不产生通知、不触发验收；ready / running / waiting / paused 各态均可提交（交付是对已完成 Run 的事实陈述，与控制态正交）。回执落来源 Run 事件流 `delivery_submitted{package_id}`。
- **读面**：`GET /v1/missions/{id}/delivery-packages`（按真实提交时间倒序——首项即当前）、`GET /v1/delivery-packages/{package_id}`（供 0.6.9 `delivery_package_ref` 寻址）。
- **SPA**：任务详情「交付」区——当前交付包摘要（来源 Run、验收对应有证据 n/m、未解决/风险计数）、历史提交、提交表单（选可交付 Run、逐条验收证据、成果引用/验证证据/未解决项/风险/备注）；明示"已提交成果——尚未送验（验收随 0.6.9）"。

## 修复（独立审校 1 项 P1、3 项 P2）

- **并发修订或取消后不再能提交**（P1）：权威守卫下沉到存储层——`CreatePackage` 在写包的**同一事务**内重读 missions 行，契约版本不符 / 已取消 / 不存在分别拒绝、整体不落；服务层预读只作早拒。
- **包与回执同事务**（P2）：`delivery_submitted` 回执随包同事务写入 Run 流，不存在"包落了回执丢"的部分成功；请求中断即整体不落、重试恰一包一回执。
- **列表按真实时间排序**（P2）：RFC3339Nano 小数位可变使字符串序错位（`.1Z` 排在更晚的 `.11Z` 前）——两存储改按解析时间倒序，补小数位回归。
- **交付卡资格列表随执行进度刷新**（P2）：10s 轮询 + 详情变化即刻重载，表单草稿不受影响；首次执行完成后无需重开页面即可提交。

## 升级说明

- v0.6.7 数据库首次启动自动建 `delivery_packages`（migration v21，幂等 DDL），无需手工步骤；旧版本二进制可直接升级本库。
- 提交交付包不改变任务状态，也不会送验——送验（`request_review` → reviewing）与验收裁定（`review_mission`）随 0.6.9 接入，以本版的 `package_id` 为锚。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.3/§10.2/§11.2、AT-10）与 [ADR-0031](docs/design/adr/ADR-0031-delivery-package.md)（v0.2）。
