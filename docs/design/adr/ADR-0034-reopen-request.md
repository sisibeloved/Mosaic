# ADR-0034：决策重开请求——`reopen_request`（0.6.11）

- 状态：Accepted（2026-11-03，随 0.6.11 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§6.1——`decision_ref` 对应的假设、具名异议、未决问题、反证和**重开条件**随交接包保留；负责人可提出改计划，**不能改写原决定**；§10.2——"触及决策的 falsifier / reopen trigger 时，负责人提交 `reopen_request`，标出受影响决定、证据和需停止的工作项；系统暂停依赖该假设的执行，把问题交回 Room。已发生的外部动作保留，不能用新决定覆盖旧回执。不存在可用 Room 时进入任务中心待决，不能借'需要讨论'无限挂起而不展示原因"；§11.2 命令表——"`reopen_request`：MissionID、expected_version、decision_ref、证据引用、受影响工作项——校验负责人权限与来源后记录重开请求，暂停受影响执行并投递 Room；Room 不可用时在任务中心待决；**不自动重开 Room 或翻转 Mission 终态**"）；[ADR-0029](ADR-0029-mission-pause-resume-cancel.md)（暂停面复用）；[ADR-0033](ADR-0033-mission-return-notice.md)（回房通知基建复用）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.6.11。

## 背景与问题

0.6.0–0.6.10 交付了从交办到验收、回房的完整闭环，但**决策默认不可逆**：验收通过、
收束落定之后，若出现 falsifier（反证），系统没有一条受控通道把"这个决定需要重新
讨论"表达出来——要么人工去房间发一条普通消息（无结构、无留痕、不暂停任何执行），
要么任由依赖该假设的执行继续跑。RFC §10.2 为此裁定重开是**请求**而非翻转：负责人
提交请求、标出受影响范围，系统暂停依赖执行并把问题交回 Room；已发生的外部动作与
旧回执原样保留。本件只交付这一件事。

## 决策

### 1. 命令面：`POST /v1/missions/{id}/reopen-request`

| 字段 | 约束 | 语义 |
|---|---|---|
| `expected_version` | int ≥1，CAS | 与当前 `contract_version` 不符 409（防预读表单与修订交错） |
| `decision_ref` | 1..256 字 | 受影响决定的引用（收束胶囊 / 文档修订 / 决策版本——自由引用串） |
| `evidence_refs` | 1..8 条、每条 ≤256 | 反证/触发证据的引用 |
| `affected_mission_ids` | 0..16 | 需停止的工作项（可空 = 仅请求不暂停；含自身亦允许） |
| `reason` | ≤280 字 | 说明（进卡片与事件） |

- **事件留痕**：`mission_events` 追加 `reopen_requested` 事件（payload 携
  `request_id` / `decision_ref` / `evidence_refs` / `affected_mission_ids` / `reason` /
  `contract_version`——CAS 后的实际版本随事实写入，回房投影不读当前行）。append-only：
  已发生的外部动作与旧回执**原样保留**（RFC"不能用新决定覆盖旧回执"由事件账本
  天性满足）。`request_id` = `rop_` 前缀生成。
- **不翻转状态**：请求不改发起 Mission 的状态——任何状态（含 accepted 终态——新
  证据推翻已验收的决定正是重开的典型场景）均可发起，发起后 Mission 照旧。
- **不自动重开 Room**：不写 closure 事件、不翻收束状态、不开新轮（交回 Room 的
  卡是系统消息，不触发讨论波——引擎刺激只认 human，ADR-0033 已定）。
- **权限**：个人版 owner 写门（Origin/Token 三件套）；请求人为负责人的身份校验随
  0.6.12 责任交接一并考虑（多人语义未到）。无显式幂等键——CAS 防乱序，重复提交
  即多条请求如实记录（每条一张卡）；暂停面幂等（下节）。
- **响应**：201 `ReopenRequestView`（请求事实 + 逐项暂停结果）；形状不符 400
  `invalid_reopen`；任务不存在 404；域未装配 503。

### 2. 受影响工作项暂停：显式列表 + 逐项如实报告

- 请求落地后**逐项**调用 0.6.6 暂停面（`reason = "reopen:<request_id>"` 留痕可回链），
  每项结果如实返回：

  | result | 语义 |
  ---|---
  | `paused` | 本次由 ready/running → paused（在途 Run 按 ADR-0029 停止面收尾 canceled） |
  | `already_paused` | 已是 paused（重复提交幂等） |
  | `terminal` | accepted/canceled 终态——无执行可停，跳过（不翻转） |
  | `not_found` | 不存在的任务 |
  | `failed` | 暂停面错误（detail 携原因；请求事实不回滚——已持久即已记录） |

- 暂停**失败不阻断**其余项（逐项独立）；结果只出现在提交响应与各自任务的事件流
  （paused 事件携 reason），不回写请求事件。

### 3. 交回 Room：复用回房通知（第五类事实）

- 投影器（ADR-0033）扩展扫描 `event_type IN ('status_changed', 'reopen_requested')`；
  `reopen_requested` → 通知行 `kind = reopen`（migration v28：`mission_return_notices`
  += `decision_ref` 快照列）。卡片正文：`🔁 决策重开请求：「{decision_ref}」——发起
  任务「{objective}」{reason}（受影响 {n} 项）`；`mission_ref` += `kind: reopen` 与
  可选 `decision_ref`。
- 由此**整栈继承**：暂停房 held 恢复后发布、删除房 canceled + 来源墓碑、房首保序、
  崩溃重启游标重放、`source_seq` 去重、`GET return-notices` 台账——零新增机制。
- **Room 不可用**（无来源房间 / 已删除）→ 通知行按既有语义 canceled（或根本不产生
  ——未关联 Room 的任务不写房间事件），"任务中心待决"由 `GET reopen-requests` 读面
  与台账共同呈现——请求可见、原因可见，不无限静默挂起。

### 4. 读面：`GET /v1/missions/{id}/reopen-requests`

事件流组装（`reopen_requested` 按 seq 升序）→ `{ requests: [ReopenRequestView] }`
（request_id / decision_ref / evidence_refs / affected_mission_ids / reason /
contract_version / occurred_at）。逐项暂停结果不在此视图（见 §2）。

### 5. 契约面

- 两端点（POST / GET）；`MissionRef.kind` 枚举 += `reopen`、可选 `decision_ref`
  （message.posted schema / OpenAPI / room-protocol fixture 同步）；`ReturnNoticeView
  .kind` += `reopen`；`SPA`：任务详情「重开请求」区（历史 + 提交表单：决定引用 /
  证据 / 受影响项多选 / 原因；任何状态可发起）与房间 `MissionRefCard` 的 reopen 徽标。

## 非目标（0.6.11 明确不做）

- 自动重开 Room（新讨论轮 / 翻收束状态）；按 `decision_ref` 全库自动匹配受影响任务
  （负责人显式标出——自动匹配随决策版本树另立）；新决定对旧回执的改写（append-only
  天性排除）；请求的显式幂等键；负责人身份的多人校验（0.6.12）；跨任务全局待决队列
  （各任务读面 + 回房台账已覆盖可见性）。

## 验收要点（先红后绿）

- **UT·reopen**：形状校验族（缺决定引用 / 空证据 / 超限）；CAS 409；404；任何状态可
  发起且发起后状态不变（accepted 发起仍 accepted）；逐项结果五型（paused /
  already_paused / terminal / not_found / failed）；事件 payload 事实（含实际版本与
  request_id）；暂停 reason 回链。
- **UT·missionreturn**：reopen 卡正文模板与 mission_ref（kind=reopen + decision_ref）。
- **IT·SQLite**：v28 新库 + v27 旧库升级；reopen_requested 事件投影成行（快照
  decision_ref）；原子投递复用（暂停房 held / 删房 canceled）。
- **API**：POST 全链（含受影响项实测暂停 + already_paused 幂等 + terminal 跳过）、
  GET 列表、CAS 409、503；无房任务（零通知行，读面可见）。
- **真机冒烟**：房间交办任务 accepted → 发现反证 → reopen_request（决定引用 + 证据 +
  受影响另一 running 任务）→ 受影响项 paused（reason 回链）→ 房间出现重开卡 →
  台账 kind=reopen delivered；重启零重复；无房任务重开 → 读面待决、零房间事件。

## 后果

- 正面：决策有了受控的重开通道——留痕、暂停、交回讨论，且交回复用回房栈（一处
  新事实类型，整栈白得暂停/删除/去重语义）。
- 负面：受影响范围依赖负责人显式标出（漏标不自动补）；每条请求一张卡（重复提交
  会多次可见）。
- 风险：decision_ref 为自由引用串——错引不做存在性校验（同 source_refs 先例），
  靠负责人自证；决策版本树落地后可再收紧。

## 修订记录

- v0.1（2026-11-03）：首版（随 0.6.11 实施）。
