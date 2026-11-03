# Mosaic v0.6.11 — 决策重开请求：`reopen_request`（0.6 Mission 与本地执行领域·第十二件）

0.6.10 把结果送回了房间，但**决策默认不可逆**——验收通过、收束落定之后，若出现 falsifier（反证），系统没有一条受控通道把"这个决定需要重新讨论"表达出来。本件按 RFC-0018 §10.2/§11.2 交付重开请求：负责人标出受影响决定与证据，系统暂停依赖执行并把问题交回 Room。两轮独立审校（7 + 5 = 12 项）全部关闭后定版。

## 新增

- **命令面 `POST /v1/missions/{id}/reopen-request`**（ADR-0034）：CAS 版本（任意状态可发起——含 accepted 终态：新证据推翻已验收的决定正是重开典型场景）+ `reopen_requested` 事件（append-only 不改写旧回执）；**不自动重开 Room、不翻转 Mission 终态**。写门三件套 + MaxBytesReader（1 MiB）+ DisallowUnknownFields 严格解码。
- **逐项暂停受影响工作项**（≤16 显式列表）：`paused` / `already_paused`（幂等重发停止信号）/ `terminal`（accepted/canceled）/ `not_pausable`（draft/reviewing/canceling）/ `not_found` / `failed`——如实报告，失败不回滚请求；暂停步骤用独立上下文（请求断线不丢暂停）。
- **交回 Room 复用回房通知栈**（第五类事实 `kind=reopen`）：migration v28（`mission_return_notices` += `decision_ref` / `affected_count` 快照列）；投影器扩扫 `reopen_requested` 事件——暂停房保留 / 删房待决 / 重启重放 / `source_seq` 去重 / `GET return-notices` 台账**全栈继承**。
- **读面 `GET /v1/missions/{id}/reopen-requests`**：事件流组装（seq 升序），任务中心待决可见性。
- **契约面**：`message.posted` 载荷 `mission_ref` += `kind: reopen` 与可选 `decision_ref`（系统 actor 专用，`post_message` 命令面拒收）；`ReopenSubmit` / `ReopenRequestView` / `ReopenAffectedResult` schema。
- **SPA**：任务详情「决策重开」区（提交表单——决定引用 / 证据 / 受影响项 / 原因 + 历史列表 + 错误可重试）；房间时间线 reopen 徽标 + 决定引用显示。

## 修复（两轮独立审校 12 项）

- **P1 写门绕过**：SubmitReopenRequest 补 guardWrite（Origin / Content-Type / Owner-Token 三件套）。
- **版本冲突误报已暂停**：already_paused 分支的版本冲突不吞——重读重试一次，仍冲突如实返回 failed。
- **事务提交后断线丢暂停**：Service 层在 store 事务成功后零值返回（暂停步骤不依赖读取回值）。
- **请求校验**：MaxBytesReader（1 MiB）+ DisallowUnknownFields（拼错字段 400）。
- **非终态状态**：draft/reviewing/canceling 明确报 `not_pausable`（terminal 保留给 accepted/canceled）。
- **SPA 时序**：重开历史乱序守卫 / 失败保留末次成功 / 暂停失败原因 detail 展示 / 首次失败可见可重试。
- **v28 迁移登记**：补 INSERT OR IGNORE。

## 升级说明

- v0.6.10 数据库首次启动自动执行 migration v28（幂等加列），无需手工步骤。
- 无新增请求必填字段（重开端点自带 `expected_version` CAS）；老客户端对 `kind=reopen` 卡按可选忽略。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§10.2/§11.2）与 [ADR-0034](docs/design/adr/ADR-0034-reopen-request.md)（v0.3）。
