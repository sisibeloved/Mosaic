# ADR-0031：DeliveryPackage 成果提交——`submit_result` 不自动送验（0.6.8）

- 状态：Accepted（2026-11-02，随 0.6.8 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.3 不变量——"执行成功、成果提交、验收通过是三个事实；模型说'完成'不替代后两者"；§10.2——"DeliveryPackage 至少包含成果引用、验收逐项对应、验证证据、未解决项、风险、动作回执和建议记忆变更。负责人不能仅拼接 Worker 回复代替验证"、"`submit_result` 保存一次 Run 的工作项结果，不自动将 Mission 送验或结案。负责人汇总交付包后调用 `request_review`……执行回执、提请验收与验收裁定各自留痕"；§11.2 命令表 `submit_result`——"RunID、generation、产物与证据、工作项版本 → 工作项结果与交付回执；迟到只审计，不自动送验或结案"；AT-10——"submit_result 不自动送验；request_review 缺必需项时拒绝，补齐后进入 reviewing"）、[ADR-0023](ADR-0023-mission-contract.md)（契约版本 CAS——交付包对应的验收标准以契约版本为准）、[ADR-0028](ADR-0028-mission-execution.md)（Run 终态与 result_block 截断存档——"完整交付物随 0.6.8"）、[ADR-0030](ADR-0030-run-crash-recovery.md)（unknown + verified_completed 处置——用户 attest 的完成亦可交付）、交付计划 v2.00（0.6.8：DeliveryPackage 成果提交：`submit_result` 不自动送验）

## 背景与问题

0.6.5 起 Run 完成即把结果正文截断存档于 `result_block`（≤8000 字），Mission 回
ready "可再执行"——**执行成功就是全部**：没有"成果提交"这一独立事实，没有结构
化的交付物（成果引用、验收逐项对应、证据、未解决项、风险），也就没有验收
（0.6.9）可以审查的对象。RFC §3.3 把"执行成功 / 成果提交 / 验收通过"裁为三个
互不替代的事实；§10.2 要求交付包有最小结构、且提交**不自动送验或结案**（AT-10
——缺验收证据的"成功"可以先交付再补证，由 request_review 的必需项检查把关）。
本件交付第二个事实：**成果提交**——结构化 DeliveryPackage 的提交、留痕与读面，
且对 Mission 状态**零副作用**。

## 决策

### 1. DeliveryPackage 域（internal/delivery，migration v21 一表）

```
delivery_packages：package_id（dlv_ 前缀）主键；mission_id + 来源 Run 溯源
  （run_id / attempt / generation）+ contract_version（交付对应的契约版本）
  + RFC §10.2 最小结构（JSON 列）：
      artifact_refs         成果引用（工作区路径/文档引用/URL——引用非正文）
      acceptance_mapping    验收逐项对应 [{criterion, evidence}]（criterion 为
                            契约版本内验收条文快照，evidence 为对应证据/说明）
      verification_evidence 验证证据（负责人验证过程——"不能仅拼接 Worker 回复"）
      open_items            未解决项
      risks                 风险
      action_receipts       动作回执（本版为引用串——ActionIntent 面随工具域）
      suggested_memory      建议记忆变更（随 0.7 记忆域消费；本版只留痕）
      notes                 备注
  + submitted_at。append-only：每次提交新建一行（历史不改写）；Mission 的
  "当前交付包"= 最新提交（读面按 submitted_at 倒序）。
```

- **三个事实分离**：Run `completed`（执行成功，系统观测）≠ DeliveryPackage
  （成果提交，负责人汇总）≠ 验收通过（0.6.9 review_mission）。交付包行与
  Run 行、Mission 行互不改写；`result_block` 仍是执行正文截断存档，交付包
  以**引用**承载成果（完整产物在工作区/文档域，不复制正文入库）。

### 2. `submit_result`（提交面）语义

```
POST /v1/missions/{mission_id}/delivery-packages
  { run_id, generation, expected_version, artifact_refs[], acceptance_mapping[],
    verification_evidence?, open_items[]?, risks[]?, action_receipts[]?,
    suggested_memory[]?, notes? }
  → 201 DeliveryPackageView + Run 流事件 delivery_submitted{package_id}
```

- **来源 Run 可交付**：`completed`，或 `unknown` 且处置 `verified_completed`
  （ADR-0030——用户 attest 的完成）；failed / canceled / 未处置 unknown /
  在途 → 409 `run_not_deliverable`（无产物可交付）。
- **代次核对**（§11.2"迟到只审计"的提交面兑现）：请求携 `generation`，与
  Run 实际代次不符 → 409 `stale_generation`（调用方视图陈旧——拒绝落库，不
  静默以当前代次改写）。Run 必须属该 Mission（否则 409 `run_mission_mismatch`）。
- **契约版本 CAS**：`expected_version` 须等于 Mission 当前 contract_version
  （验收逐项对应锚定契约版本——契约已修订则交付映射陈旧，409
  `version_conflict` 携当前版本）。
- **守卫在写包事务内权威复核**（v0.2 审校 #1）：服务层对 Mission 的预读只用
  于早拒与清晰错误；存储层 `CreatePackage` 在同一事务内重读 missions 行——
  契约版本不符 / 已取消 / 不存在分别以 version_conflict / mission_canceled /
  mission_not_found 拒绝，整体不落。读-写之间的并发修订或取消不再放行。
- **包与回执同事务**（v0.2 审校 #2）：`delivery_submitted` 回执随包在同一事务
  写入来源 Run 流（存储层组合——同库单事务）；不存在"包已落、回执永久缺失"
  的部分成功，请求中断即整体不落、重试恰一包一回执。Runs 端口退回只读。
- **不自动送验**（AT-10 核心）：提交对 Mission.status **零变更**、不产生通知
  （回房随 0.6.10）、不触发任何验收流程——Mission 仍停在其当前状态（ready /
  running / waiting / paused 均可提交——交付是对已完成 Run 的事实陈述，与
  Mission 控制态正交）；canceled 拒绝（终态——409 `mission_canceled`）。
- **完备性不在提交面检查**：验收逐项对应可以不覆盖全部验收标准、证据可以
  为空——"先交付再补证"是 AT-10 的明示路径；必需项检查归 0.6.9
  `request_review`（缺必需交付项则拒绝送验）。提交面只校验形状与上限。
- **可重复提交**：同一 Run 可多次提交（补证 = 新包）；不同 Run 亦可——每次
  一行、Run 流各记一条 delivery_submitted 审计。

### 3. 读面

- `GET /v1/missions/{mission_id}/delivery-packages` → 列表（**按真实提交时间**
  倒序——首项即当前交付包；v0.2 审校 #3：submitted_at 为 RFC3339Nano 小数位
  可变，字符串序会把 `.1Z` 排在更晚的 `.11Z` 之前——两存储均按解析时间比较，
  同刻按插入序倒序）。
- `GET /v1/delivery-packages/{package_id}` → 单包（供 0.6.9
  `delivery_package_ref` 寻址）。
- Mission canceled 后读面照常（历史可查）。

### 4. 校验上限（形状单源 `delivery.Validate`）

artifact_refs ≤16 项 × ≤256 字；acceptance_mapping ≤16 项（criterion 1–256、
evidence ≤1000）；verification_evidence ≤4000；open_items ≤16 × ≤256；
risks ≤8 × ≤256；action_receipts ≤16 × ≤128；suggested_memory ≤8 × ≤256；
notes ≤1000。空切片补齐 `[]`（JSON 往返稳定）。超限 → 400
`invalid_delivery`。

### 5. 契约面（OpenAPI）

- 三端点（提交 + 两读）；DeliveryPackageSubmit / DeliveryPackageView /
  AcceptanceMappingEntry schemas。
- MissionRunEvent 事件枚举 += `delivery_submitted`（payload {package_id}）。
- 错误语义：`invalid_delivery`（400）、`mission_not_found` /
  `run_not_found`（404）、`version_conflict` / `stale_generation` /
  `run_mission_mismatch` / `run_not_deliverable` / `mission_canceled`
  （409）、`delivery_unavailable`（503）。

### 6. SPA

任务详情增「交付」区：当前交付包摘要（来源 Run #attempt、成果引用、验收
对应覆盖 n/m、未解决项/风险计数）+ 历史包折叠 + 提交表单（选可交付 Run、
逐条验收证据、成果引用/证据/未解决项/风险）。提交后 Mission 状态区不变——
UI 明示"已提交成果——尚未送验（验收随 0.6.9）"。可交付 Run 资格列表随执行
进度刷新（v0.2 审校 #4：10s 轮询 + 详情变化即刻重载；只刷新包/Run 列表，
表单草稿独立不受影响）。

## 非目标

- **request_review / reviewing 状态 / review_mission**（0.6.9）：本版不进入
  reviewing，不通知验收人，不做必需交付项完备性检查。
- **结果回房通知**（0.6.10）：提交不产生 outbox/房间通知。
- **产物正文入库 / 附件上传**：交付包只承载引用；完整产物在工作区/文档域。
- **ActionIntent / 动作回执结构化**（随工具域）：本版 action_receipts 为引用串。
- **记忆变更消费**（0.7）：suggested_memory 只留痕。
- **WorkItem 级交付**（0.6.11+）：单 Agent 直接 Mission 交付。

## 验收要点

1. completed Run 提交 → 201 包行 + Run 流 `delivery_submitted`；Mission 状态
   与版本**不变**（ready/running/waiting/paused 各态验证）；无 mission_events
   新增。
2. unknown + verified_completed 可交付；failed / canceled / 未处置 unknown /
   running → 409 run_not_deliverable。
3. generation 不符 → 409 stale_generation；run 不属 mission → 409
   run_mission_mismatch；expected_version 不符 → 409 version_conflict；canceled
   Mission → 409 mission_canceled。
4. 形状上限各 400；空切片往返 `[]`。
5. 同 Run 重复提交 → 两行、两条审计；列表倒序首项为最新；单包 GET/404。
6. 迁移 v21 新库/旧库（v20 形状）升级；JSON 列往返稳定。
7. 真机：echo 执行完成 → 提交交付包 → Mission 仍 ready、调度器照常可再执行、
   列表/单包可查、stale generation 409。

## 后果

- 任务中心出现"已执行但未交付 / 已交付但未验收"的可见中间态——这正是 §3.3
  三事实分离的产品含义；0.6.9 的验收以 package_id 为锚。
- 交付包 append-only：补证产生新行而非改写——审计完整，"当前"由时间序定义。
- 提交面对 Mission 零副作用的不变量须被 0.6.9/0.6.10 尊重：送验与回房各自是
  显式命令，不由提交触发。

## 修订记录

- v0.1（2026-11-02）：首版（随 0.6.8 实施）。
- v0.2（2026-11-02）：审校修复（独立审校 1 P1 + 3 P2）——#1 P1 写包事务内
  权威复核 Mission 版本/取消（并发修订或取消后不再放行）；#2 P2 包与回执同
  事务（Runs 端口退回只读；无部分成功）；#3 P2 列表按真实提交时间倒序（两
  存储 + 小数位回归）；#4 P2 交付卡资格列表随执行进度刷新且保留草稿。
