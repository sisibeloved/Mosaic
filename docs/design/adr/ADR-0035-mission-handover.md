# ADR-0035：Mission 责任交接——`handover` 与退役承诺处置（0.6.12）

- 状态：Accepted（2026-11-03，随 0.6.12 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.3 不变量——"每个 Mission 恰有一个 accountable owner 或显式 `unassigned`；**退役交接必须记录新负责人及承接回执**"、"共享知识、职责知识与 Mission 产物不随 Agent 退役消失"；§4.5——"已分配 Mission 固定其负责人、Binding 与执行代次，职责后来换人不自动转移在途工作；**交接沿用 §8、§10.2**"；§8.1——同 Slot 换 Agent 必须携带"职责实例知识、Mission / WorkItem 状态、产物与未决动作、**承接回执**"；§11.2 命令表——"`retire_agent`：未处置承诺禁止完成退役"；AT-03——"Agent 退役且有未交付事务：任务逐一转交/取消/显式无人承接；共享知识保留；**未处置时退役不能完成**"）；[ADR-0018](ADR-0018-agent-identity-lifecycle.md)（身份生命周期 retiring 停留语义）；[ADR-0023](ADR-0023-mission-contract.md)（owner 契约事实——任意生命周期可指派）；[ADR-0032](ADR-0032-mission-review.md)（验收冻结先例）；[ADR-0034](ADR-0034-reopen-request.md)（"负责人身份的多人校验随 0.6.12"遗留承接）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.6.12。

## 背景与问题

0.6.0–0.6.11 交付了交办→执行→验收→回房→重开的完整闭环，但责任归属自创建/修订指派后**没有受控的转移通道**：

1. **换负责人**只能靠契约修订整包替换 owner（版本递增、无承接确认——RFC §4.5"换人不自动转移在途工作；交接沿用 §8、§10.2"要求的是显式交接）。
2. **承接回执不存在**——新负责人是否知情接手，系统无事实可查（§3.3 不变量要求退役交接记录回执）。
3. **退役承诺门是空转的**——身份域 0.5.1 预留 `CommitmentChecker` 端口（"retiring 停止接新任务，处置所有承诺后才能 retired"），生产装配恒空集：Agent 带着 owned Mission 也能一步 retired，责任悬空（AT-03 明确"未处置时退役不能完成"）。

本件交付这三件事，0.6 领域线（0.6.0–0.6.12 十三件）就此收官。

## 决策

### 1. 两段式交接：发起 → 承接 | 婉拒 | 撤回

Mission 投影 += `pending_owner_agent_id`（migration v29，空 = 无在途交接；**同一时刻至多一条在途交接**——发起新交接前须先了结旧交接）。owner 在承接落地前**不变**（发起人仍是负责人——责任不因单方意愿转移）。

**发起 `POST /v1/missions/{id}/handover`**：

| 字段 | 约束 | 语义 |
|---|---|---|
| `expected_version` | int ≥1，CAS | 与当前 `contract_version` 不符 409 |
| `new_owner_agent_id` | 非空、≠当前 owner | 新负责人（承接人）；不存在 404、非 active 409 |
| `notes` | ≤2000 字 | 交接说明——在途事项 / 未决动作 / 证据指引（§8.1"职责实例知识、Mission / WorkItem 状态、产物与未决动作"的轻量承载体；完整交接包随 0.7+/RFC-0019） |

- 前置：Mission 状态 ∈ draft / ready / running / waiting / paused；无在途交接。拒绝：**accepted / canceled**（终态——责任已随裁定/取消了结，无物可交）、**reviewing**（验收基线冻结——先退回或裁定，ADR-0032 冻结先例）、**canceling**（停止瞬态——先收尾）。
- 承接人校验：存在且 **active**（suspended / retiring / retired 不接新承诺——"retiring 停止接新任务"；offer 后状态可能变化，accept 时**复核** active）。与 Create/Revise 的 owner 存在性校验（ADR-0023"任意生命周期可指派"——契约事实记录，行为能力由执行时点校验）区分：交接是**责任转移的受控通道**，承接方必须能履职。
- 效果（单事务）：`pending_owner_agent_id = B` + `handover_offered` 事件（payload：`from_owner` / `to_owner` / `notes` / `contract_version`——CAS 后实际版本随事实留痕）。**`contract_version` 不递增**（归属是控制面事实，pause/resume 先例）。

**承接 `POST /v1/missions/{id}/handover/accept`**：`{expected_version, note? ≤280}`——单事务：owner = B + pending 清空 + **`handover_accepted` 事件（承接回执）**：`from_owner` / `to_owner` / `contract_version` / `status_at_accept`（承接时的任务状态——B 接手的是什么）/ `note`。前置：pending 非空且 Mission 仍在可交接状态族（offer 后若进 reviewing / canceling / 终态则拒绝——先了结任务状态）；B 仍 active（复核）。

**婉拒 `POST /v1/missions/{id}/handover/decline`**：`{expected_version, reason? ≤280}`——B 显式婉拒：pending 清空 + `handover_declined` 事件。
**撤回 `POST /v1/missions/{id}/handover/revoke`**：`{expected_version, reason? ≤280}`——发起人撤回：pending 清空 + `handover_revoked` 事件。婉拒/撤回在任何 pending 状态了结在途交接（不应存在卡死路径）。

四命令均不改 `contract_version`；事件 append-only（交接史不可改写）。

### 2. 承接落地：归属翻转与完备性重判

accept 单事务内：

- `owner_agent_id = pending_owner`，pending 清空。
- **完备性重判**（statusOf 单源）：unassigned draft 承接后 owner 非空且契约齐备 → **ready**（入调度列——交接同时是存量任务的指派通道）+ `status_changed`（from/to/reason=handover）事件；draft 因其他原因不完备保持 draft；ready 保持；**执行控制投影保持**（running / waiting / paused 承接不翻——在途 Run 与结果事实不随归属变化："执行成功、成果提交、验收通过是三个事实"，Run 寻址按 Mission 不按 owner）。

### 3. 在途交接冻结窗

pending 期间**拒绝**：`Revise`（防修订通道整包换 owner 与交接通道竞态——双通道改归属是本设计明确排除的竞态）、`RequestReview`（验收基线含归属静定）、`MarkCanceling`（停止编排与归属转移不同时进行；要取消先撤回/婉拒交接）。**照常**：Pause / Resume（控制面——退役排空正需要先暂停再交接）、ReopenRequest（append-only 事实，与归属无竞态）、成果提交与 Run 生命周期（事实面与 owner 无关）。

### 4. 退役承诺处置：CommitmentChecker 实装

身份域 0.5.1 预留端口首次接生产装配：app 注入 `missionCommitments`（直查 mission Store，无服务环依赖）：

```
Outstanding(agentID) = { m : m.owner_agent_id == agentID ∧ m.status ∉ {accepted, canceled} }
```

- **非终态 owned Mission = 未处置承诺**（draft / ready / running / waiting / paused / reviewing / canceling 全计入）；**在途交接仍计入**（owner 未翻转、回执未落——责任未转移，§3.3 不变量）。
- 展示串 `{mission_id}（{status}）{objective 摘要}`——retire 响应 `outstanding_commitments` 携带（契约已有，SPA SettingsPage 已展示）。
- 退役编排：retire → retiring 停留 + 清单返回 → 逐一**交接（承接回执落）**或**取消（canceled）**→ 清单空 → 重试 retire → retired（identity 域既有两段语义，零改动）。
- unassigned 任务不入任何清单（无主承诺由任务中心可见性兜底）；契约 `reviewer` 身份不计入（较轻的契约事实——非目标）。
- **修订通道保留 owner 变更**（ADR-0023 既有语义：版本行留痕、CAS）：修订换 owner 同样清空原 owner 清单（归属事实已移动），但不产生回执——回执要求覆盖**交接通道**；两通道并存、均有审计，个人版 owner 是最终权威。冻结窗（§3）保证两者不同时进行。

### 5. 读面：`GET /v1/missions/{id}/handovers`

事件流组装（handover_offered / accepted / declined / revoked 按 seq 升序）→ `{ events: [HandoverEventView] }`（kind / from_owner / to_owner / notes / note / reason / contract_version / occurred_at）。`MissionView` += `pending_owner_agent_id`（空串 = 无在途交接）。

### 6. 契约面与端点

- 五端点（offer / accept / decline / revoke / list）；写四端点过 owner 写门三件套 + MaxBytesReader（1 MiB）+ DisallowUnknownFields（0.6.11 二审 #5 同例）。
- `HandoverSubmit` / `HandoverAccept` / `HandoverResolve` / `HandoverEventView` / `HandoverEventList` schema；`MissionView.pending_owner_agent_id`；OpenAPI → apigen / TS 再生成。
- 交接**不回房**（归属是任务内部事实，任务详情与事件史承载可见性；回房栈五类事实不动）。
- 承接 ADR-0034 遗留："负责人身份校验"——个人版 owner 写门仍是唯一身份面（身份域无 per-agent 凭证，OQ-20：凭据留 CLI 侧）；事件记 `from_owner` / `to_owner` 事实，多人 actor 推导随网关多入口语义另立。
- 错误映射：形状 400 `invalid_handover`；任务/承接人不存在 404；版本冲突 409 `version_conflict`；状态族 / 冻结窗 / 同 owner / 在途唯一 / 承接人非 active 409 `invalid_transition`（detail 携原因）；身份状态端口未装配 503。

### 7. 存储（migration v29）

`missions` += `pending_owner_agent_id TEXT NOT NULL DEFAULT ''`（幂等加列 + 登记）。四个 Store 方法（事务内核对模式——identityWriteTx 内重读状态再写）：`HandoverOffer`（版本 + 状态族 + pending 空 → 置 pending + 事件）；`HandoverAccept`（版本 + pending 匹配锚 + 状态族 → 翻 owner / 清 pending / 重判状态 + 回执事件 + 可选 status_changed）；`HandoverWithdraw`（decline/revoke 共用：版本 + pending 匹配锚 → 清 pending + 事件）。accept 的 pending 锚防"撤回后改派 C"与旧 accept 的竞态（锚不符即拒）。

## 非目标（0.6.12 明确不做）

- Portable Agent Bundle / 跨实例导出导入（§8.2–8.3——0.7 记忆与可移植恢复 / RFC-0019 承接）；WorkItem `offer_work / accept_work / decline_work`（WorkItem 域未建）；Binding 换 Agent 的交接包与排空（Slot 域既有 ReleaseBinding 语义不动）；自动暂停在途 Run（结果事实与 owner 无关）；自动撮合新负责人（显式指定——"显式无人承接"由 decline 留痕）；交接回房通知卡片；多人 per-agent 身份校验；Goal 域承诺（Goal 域未建）；共享知识 / 记忆随交接转交（§8.1 记忆分域——私有记忆不因职责换人自动转交）。

## 验收要点（先红后绿）

- **UT·mission（交接状态机）**：offer 五状态允许 / reviewing / canceling / 终态拒绝；同 owner 拒；在途唯一（二 offer 拒）；accept 翻 owner + 回执 payload（from/to/contract_version/status_at_accept/note）；完备性翻转（unassigned draft → ready + status_changed）；执行投影保持（running / paused / waiting 承接不翻）；decline / revoke 清 pending；冻结窗（pending 期间 Revise / RequestReview / MarkCanceling 拒，Pause 照常）；CAS 409；承接人 suspended / retiring 拒（offer 与 accept 两点）；版本不递增（控制面事实）。
- **UT·personalagent（承诺门）**：注入 mission 承诺门——owned 非终态任务 → retire 停留 retiring + 清单；交接承接后清单空 → 重试 retired；accepted / canceled 不计入；无 owned 任务直接 retired（回归：恒空集路径不回归）。
- **IT·SQLite**：v29 新库 + v28 旧库升级（幂等加列 + 登记 29 项）；offer / accept 单事务原子性（半途失败零残留）；Outstanding 查询过滤正确。
- **IT·httpapi**：五端点全链（发起 → pending 可见 → 承接 → owner 翻转 + 回执历史；decline / revoke 各留痕）；写门三件套 + 严格解码 + 超限；CAS 409 / 状态族 409 / 承接人 404·409 / 任务 404。
- **SPA**：任务详情交接区（发起表单——承接人选择 + 交接说明；待承接徽标 + 承接 / 婉拒 / 撤回；历史列表）；retire 未处置承诺展示（既有面承接新数据源）。
- **真机冒烟**：带 owner A 的任务 → 发起交接给 B（pending 徽标）→ B 承接 → owner=B + 回执历史；A retire → 承接后清单空 retired；另一任务发起后 B 婉拒 → pending 清 + 婉拒留痕 → A retire 停留 retiring + 清单展示；重启零重复。

## 后果

- 正面：责任归属有了受控转移通道（回执留痕）；退役承诺门从空转变实（AT-03 兑现——未处置时退役不能完成）；交接同时是存量 unassigned 任务的指派通道（完备性重判）。
- 负面：交接窗口内契约面短暂冻结（pending 期间不可修订/送验/取消——须先了结交接）；承诺门让 retire 从一步变两步（清单非空时）——如实展示原因，不静默。
- 风险：修订通道仍可换 owner（无回执）——两通道并存靠冻结窗防竞态、靠版本行/事件双审计；多人语义落地时须复核两通道的权限面。承接人 active 校验在 offer 与 accept 两点复核，窗口内状态变化由 accept 拦截。

## 修订记录

- v0.1（2026-11-03）：首版（随 0.6.12 实施）。
