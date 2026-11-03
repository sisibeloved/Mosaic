# Mosaic v0.6.12 — Mission 责任交接：`handover` 与退役承诺处置（0.6 Mission 与本地执行领域·第十三件·收官）

0.6.11 之前，责任归属自创建/修订指派后**没有受控的转移通道**：换负责人只能靠契约修订整包替换（无承接确认）；承接回执不存在；退役承诺门自 0.5.1 预留端口以来恒空转——Agent 带着未交付任务也能一步 retired（AT-03"未处置时退役不能完成"）。本件按 RFC-0018 §3.3/§4.5/§8.1/§11.2 交付三件事：换负责人、承接回执、退役承诺处置。**0.6 领域线（0.6.0–0.6.12 十三件）就此收官。**三轮独立审校（7 + 5 + 3 = 15 项）全部关闭后定版。

## 新增

- **两段式责任交接 `POST /v1/missions/{id}/handover`**（ADR-0035）：发起只置 `pending_owner_agent_id`（**owner 不变——责任不因单方意愿转移**；同一时刻至多一条在途交接，`offer_id` 发起标识）；承接 `handover/accept` 单事务翻转 owner + **承接回执**（`handover_accepted` 事件：from / to / contract_version / status_at_accept / note——回执事实由**事务内核对后的实际值**生成）；婉拒 / 撤回了结在途交接并留痕（"显式无人承接"可审计）。承接 / 婉拒 / 撤回**必填 `offer_id` 锚**——撤回改派后（版本不递增）旧请求一律 409。四命令均不递增 `contract_version`（归属是控制面事实），事件 append-only。
- **承接人校验**：存在且 **active**（suspended / retiring / retired 不接新承诺——offer 与 accept 两点复核；accept 事务内直查身份表，承接×退役双向竞态在事务门拦截）。
- **在途交接冻结窗**：pending 期间 Revise / RequestReview / MarkCanceling 拒绝（服务层 + 存储事务内双守卫——防修订通道整包换 owner 与交接通道竞态）；Pause / Resume / ReopenRequest / 成果提交照常。
- **完备性重判**：unassigned draft 承接后按**事务内现行 content** + 新 owner 重判（`HandoverDraftTarget` 单源）→ 齐备即 ready 入调度列（交接同时是存量任务的指派通道）；running / waiting / paused 执行控制投影保持。
- **修订归属锚**（契约收紧）：revise 请求**必填 `expected_owner_agent_id`**（空串 = 预期 unassigned；null / 缺席 400）——承接翻转 owner 后携旧 owner 的修订被 409 `owner_mismatch` 拒（旧表单不得无感知撤销责任转移）。个人版单客户端生态，SPA 同步携带。
- **退役承诺门实装**：0.5.1 预留的 `personalagent.CommitmentChecker` 首次接生产装配——**非终态 owned Mission = 未处置承诺**（含在途交接：回执未落责任未转移）；retire 停留 retiring + 返回清单，逐一交接承接或取消后 → retired；retiring→retired 收尾统一过**未处置承诺事务门**（双向竞态零残留）。
- **读面与契约**：`GET /v1/missions/{id}/handovers` 交接史（seq 升序，含承接回执与 offer_id 回链）；`MissionView` += `pending_owner_agent_id` / `pending_offer_id`；`MissionEvent.event_type` 枚举补登 `reopen_requested`（0.6.11 漏登）与四类 `handover_*`；写端点 guardWrite 三件套 + MaxBytesReader（超限 413）+ DisallowUnknownFields。
- **SPA**：任务详情「责任交接」区（发起表单——承接人选择 + 交接说明 ≤2000 字；待承接徽标 + 承接 / 婉拒 / 撤回；交接历史按发起对齐、刷新中/失败窗口明确标识资料状态）；详情头 `⇄ 待承接 → B` 徽标；修订表单打開時固定基准（owner + 版本——旧草稿对不上当前基准时 409 可见）；设置页退役未处置承诺清单。

## 修复（三轮独立审校 15 项）

- **一审（3 P1 + 3 P2 + 1 P3）**：offer_id 锚（旧承接/婉拒/撤回处理新交接）；修订归属锚（旧修订撤销责任转移）；承接×退役双向事务门；回执状态事务内注入；历史重试按钮（交接 + 重开两处）；详情轮询刷交接元数据；超限 400 → 413。
- **复审（1 P1 + 3 P2 + 1 P3）**：修订草稿基准（打开时固定 owner + 版本）；offer 事件 from_owner 事务内注入 + draft 重判移入事务内（并发交错不进事实）；手动加载与轮询统一请求代次（迟到旧响应不覆盖新详情）；交接历史/备注按 offer_id 刷新隔离；归属锚必填键 presence 检查（缺席 400）。
- **三审（1 P2 + 2 P3）**：首轮轮询不再作废初次加载（取号源独立 + detail 应用层代次）；新 offer 资料未就绪明确标识状态（不显示旧说明）；归属锚 null 同拒 400。

## 升级说明

- v0.6.11 数据库首次启动自动执行 migration v29（`missions` += `pending_owner_agent_id` / `pending_offer_id`，幂等加列），无需手工步骤。
- **契约收紧（唯一破坏面）**：`POST /v1/missions/{id}/revise` 新增必填 `expected_owner_agent_id`（空串 = 预期 unassigned；null / 缺席 400）——旧客户端修订请求须补该字段；其余端点无新增必填字段（交接端点为本版新增，自带 `offer_id` / `expected_version` CAS）。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.3/§4.5/§8.1/§11.2，AT-03）与 [ADR-0035](docs/design/adr/ADR-0035-mission-handover.md)（v0.4）。
- 下一领域：0.7 记忆与可移植恢复（0.7.0 起十件）。
