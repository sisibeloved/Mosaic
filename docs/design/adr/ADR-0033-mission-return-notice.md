# ADR-0033：执行结果回房——返回通知的持久、去重与 Room 删除后的保留（0.6.10）

- 状态：Accepted（2026-11-02，随 0.6.10 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§10.1 时序——"保存 reviewing 状态及通知 outbox""保存验收事实与返回目标引用""Room 消费结果通知并按发布条件写引用卡片"；§10.2——"Mission 的结果先持久化，再通知 Room。Room 暂停/归档时保留任务中心结果及待投递通知，不为回传自动恢复讨论；恢复后按来源事件去重发布。Room 已删除时取消该返回目标，保留 Mission 内的来源墓碑与结果"；§11.3——"外部执行与通知采用至少一次投递、幂等消费"；§12 故障表——"产物成功但 Room 通知失败：交付保持有效，outbox 有界重试通知；Room 去重，不重跑执行"；AT-09——"不变状态不反复通知"；AT-20——"返回位置失效不重跑任务"）；[ADR-0024](ADR-0024-room-dispatch-first-slice.md)（`mission_sources` 房间来源唯一映射）；[ADR-0008](ADR-0008-personal-storage-sqlite.md)（outbox 提交后分发机制）；[ADR-0032](ADR-0032-mission-review.md)（reviewing / accepted / returned 事实）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.6.10。

## 背景与问题

0.6.1 把房间任务卡与 Mission 建立了 `(room_id, task_id)` 唯一映射，0.6.5–0.6.9
依次交付了执行、控制、崩溃诚实、成果提交与验收——至此 Mission 已能完整走到
accepted 终态。但**结果只存在于任务中心**：交办它的房间对"送验了没有、验收
过了没有、被退回了还是取消了"一无所知，参与者必须离开讨论去任务中心翻看。
RFC §10.1/§10.2 为此裁定结果以"引用卡片"回房，并提出三条硬约束：

1. **持久**：通知是事实的派生物，进程重启不得丢；
2. **去重**：至少一次投递 + 幂等消费——崩溃后重投不得在房间里出现两张同一事实的卡；
3. **Room 失效鲁棒**：Room 暂停时保留待投递、恢复后发布且不借回传自动恢复讨论；
   Room 删除时取消该返回目标但保留任务结果与来源墓碑，不重跑任务。

本件只交付这一件事：**把四类 Mission 事实按上述约束投递回来源房间**。

## 决策

### 1. 通知源：四类 Mission 事实（按 mission_events 派生，不另造事实）

| 来源事件（`status_changed`） | 卡片 kind | 含义 |
|---|---|---|
| `to = reviewing` | `reviewing` | 已提请验收（携交付包、轮次） |
| `to = accepted` | `accepted` | 验收通过（终态；携验收人、轮次） |
| `to = ready` 且 `decision = returned` | `returned` | 验收退回（携原因、轮次） |
| `to = canceled` | `canceled` | 终局取消 |

- 依据 RFC §10.1 时序：通知 outbox 写在**送验**与**裁定**两处；§6.2 默认"只通知交付、
  失败、需要用户行动"——送验即交付已提出，裁定即结果，取消即终局。
- Run 级事实（attempt 失败、unknown → waiting）**不回房**：它们是执行过程而非结果，
  任务中心可见；回房只讲"任务怎么样了"。
- 没有房间来源的 Mission（`mission_sources` 无映射）不产生任何通知——RFC §11.3
  "未关联 Room 的任务不写房间事件"。返回目标即 0.6.1 的结构化 `RoomSource`；契约字段
  `return_targets`（自由串）语义不变、本版不消费。

### 2. 投影：mission_events 全局游标 → 通知行（migration v25）

通知不在各写路径里逐一塞入，而是由**一个游标投影器**从 `mission_events`（`seq` 为
全局 AUTOINCREMENT，BEGIN IMMEDIATE 串行写入 ⇒ seq 序 = 提交序）派生：

```
mission_return_notices（通知行——持久、可审计）
  notice_id, source_seq UNIQUE（= 去重键）, mission_id, room_id, task_id,
  kind, contract_version, review_round, package_id, reviewer, reason,
  objective（≤80 字摘要快照）, status ∈ pending|held|delivered|canceled|failed,
  attempts, next_attempt_at, last_error, room_event_id, created_at, updated_at
mission_return_state（游标：id=1, last_seq）
mission_sources += deleted_at（来源墓碑，NULL = 房间仍在）
```

- 投影一轮 = 一个事务：`SELECT … WHERE seq > last_seq AND event_type='status_changed'
  ORDER BY seq LIMIT n` → 过滤出表 1 的四类且有房间来源 → `INSERT`（`source_seq`
  UNIQUE，重放即幂等）→ 推进 `last_seq` → COMMIT。崩溃后从游标重放：**不漏不重**。
- 投影时快照卡片所需字段（objective 摘要、轮次、验收人、原因、包 ID）——投递不再
  回读 Mission，事实在事件落地那一刻定格（AT-09：不变状态不反复通知——每条通知对应
  恰一条来源事件）。

### 3. 投递：核对 + 追加 `message.posted` + 标记 delivered 在同一 SQLite 事务

卡片 = 房间事件 `message.posted`，actor `par_system`（system），载荷新增封闭对象
**`mission_ref`**（`mission_id / kind / contract_version / review_round / package_id /
task_id / source_seq`）+ Markdown `body`（事实一句话 + objective 摘要）。

```
DeliverReturnNotice(notice_id, envelope)：
  BEGIN IMMEDIATE
    通知行须为 pending|held（否则 ErrNoticeNotPending——并发/重放保护）
    房间墓碑或不存在            → ErrRoomDeleted（不落卡）
    最新 room.paused|room.started 为 paused → ErrRoomPaused（不落卡）
    appendTx 共享体：room_events + outbox + FTS（与引擎系统事件同一条路）
    UPDATE 通知行 status=delivered, room_event_id
  COMMIT
```

- **去重由构造保证**：卡片与标记同事务落地，不存在"卡落了标记没落"的重复窗口；
  进程内崩溃重启后游标与通知行都是持久态，重投只会命中 delivered 行而跳过。
- 复用既有 outbox 扇出：SSE hub 与引擎照常消费这条 `message.posted`——SPA 实时可见；
  **引擎刺激过滤器只认 human 的 `message.posted`**（engine.go 波门控），系统卡片不触发
  讨论波，"不为回传自动恢复讨论"由构造满足；卡片会进入后续 agent 语境（它们看得到
  "任务已验收通过"），这是期望行为。
- 命令面 `post_message` 不收 `mission_ref`（`DisallowUnknownFields`）——人类不能伪造结果卡。
- 不走命令面（而走 appendTx）的理由：命令面要求 `ExpectedRoomVersion` 严格相等，
  后台投递得读版本 + 冲突重试；而原子性需要的是"同事务标记"，命令面的回执幂等只覆盖
  房间侧、覆盖不到通知行。引擎的 round.opened 等系统事件已是直追加先例。

### 4. 暂停保留 / 删除取消 / 有界重试

- **暂停**：`ErrRoomPaused` → `held`（不计入 attempts、不会 failed），`next_attempt_at =
  now + 5s` 周期复查；房间 `room.started` 后下一轮即投递。同房间通知按 `source_seq`
  保序：房头未投出则该房本轮余下通知一律跳过（reviewing 不会落在 accepted 之后）。
- **删除**：两处兜底——(a) `DeleteRoom` 级联同事务 `UPDATE mission_sources SET
  deleted_at` + 该房 pending|held 通知置 `canceled(room_deleted)`；(b) 投递侧再核墓碑
  （覆盖"先删房后升级到本版"与"删除与投递竞态"）。Mission 结果与来源映射保留——视图
  `room_source.deleted_at` 即"来源墓碑"；AT-20：返回位置失效不重跑任务。"另选去处"
  非本版目标（见非目标）。
- **其他投递错误**：有界重试——`attempts++`，退避 `min(1s·2^attempts, 60s)`，
  `attempts ≥ 12` 置 `failed(last_error)`；交付/验收事实不受影响（RFC §12："交付
  保持有效……不重跑执行"）。failed 行在任务中心可见，不静默。
- 已 delivered / canceled / failed 的行是终态；投影器不会为同一 `source_seq` 再造行。

### 5. 读面与视图

- `GET /v1/missions/{id}/return-notices` → `{ notices: [ReturnNoticeView] }`（按
  `source_seq` 升序；404 `mission_not_found`）——持久与去重对用户**可审计**：每条来源
  事件对应的投递状态、房间事件 ID、尝试次数与最后错误。
- `MissionView.room_source` += `deleted_at`（nullable）——来源墓碑。
- `message.posted` 载荷 += `mission_ref`（可选、封闭对象；旧事件无该字段投影为零值）。
- SPA：房间时间线把带 `mission_ref` 的系统消息渲染为**引用卡片**（kind 徽章 + 跳转任务
  中心 `/missions?id=` 选中该任务）；任务中心详情新增「回房」区（通知状态徽章 / 房间
  事件 ID / 失败原因），来源房间已删除时在来源区明示。

### 6. 装配

- 新包 `internal/missionreturn`：`Worker`（投影 + 投递，1s 周期，同 server 生命周期）
  依赖窄端口 `Store`（`ProjectNotices / DueNotices / DeliverNotice / UpdateNotice`），
  SQLite 实现；UT 用包内假件验证排序、持有、取消、退避与幂等。
- `app.Start` 在房间分发器之后启动 worker；关闭即停，未投出的行留待下次启动重放。

## 非目标（0.6.10 明确不做）

- Run 级失败 / waiting 回房；交付包提交（`delivery_submitted`）回房——RFC 时序把
  outbox 放在送验与裁定。
- 自动裁定任务卡（`task.resolved` 为人工门控——delivered/dismissed 由人类裁定，本版
  不破）；卡片只引用，不改任务卡状态。
- failed 通知的手动重投端点；返回目标改选（"另选去处"）——留待 0.6.12 责任交接一并
  考虑；跨设备通知（0.10）。
- 房间"归档"态（当前只有暂停）。

## 验收要点（先红后绿）

- **UT·missionreturn**：投影只对四类 + 有房间来源的事件产行，快照字段正确；同房保序
  （房头 held 则后继本轮跳过、异房不受影响）；paused → held 不计 attempts，恢复后投递；
  deleted → canceled；投递错误退避序列与 12 次封顶 → failed；delivered 行重放不再投递。
- **IT·SQLite**：v25 新库 + v0.6.9（v24）旧库升级；投影恰一次（同一事件重放不产第二行，
  游标推进持久）；`DeliverReturnNotice` 原子（卡 + 标记同落；paused / deleted 不落卡且行
  状态不变）；`DeleteRoom` 级联（通知取消 + 来源墓碑）；卡片随 outbox / FTS 落行。
- **API**：`GET return-notices`（空 / 列表 / 404）；`MissionView.room_source.deleted_at`
  回显；`post_message` 携 `mission_ref` 被 400 拒。
- **真机冒烟**：建房 → 申报任务 → 交办 → 执行 → 交付 → 送验（房间出现 reviewing 卡）
  → 退回（returned 卡）→ 再送验 → 接受（accepted 卡）；暂停房间后取消另一任务 →
  held → 恢复 → 投递；删房 → pending 取消 + `deleted_at`；重启 → 零重复卡。

## 后果

- 正面：房间与任务中心闭环——讨论里交办的事在讨论里看到结果；通知是事件流的
  派生投影，可重建、可审计，无第二份事实源。
- 负面：多一个后台轮询器（1s）与一张通知表；系统卡片进入 agent 语境会略增上下文。
- 风险：投影只看 `status_changed`——未来新增回房事实须同时扩表 1 与 kind 枚举（契约
  冻结点）；若 mission_events 迁移改变 seq 语义，游标须随迁。

## 修订记录

- v0.4（2026-11-02）：四审收口（"4 项产品 + 1 项测试缺口"+ 两项既有补正）——
  #1 来源成功响应单设已应用序号（较早的成功晚到不得抹掉已显示的删房标记）；
  #5 台账失败忽略早于最新已显示成功的响应（delivered 之上不再提示"暂不可用"）；
  #2 送验缺轮次按**本次送验区间**的显式裁定校准（v22→v24 混合历史——送验无
  轮次而裁定带迁移显式值，按全历史计数会 3/1、4/2 错位；同包重复送验无区间
  裁定时回退计数）；#3 取消恒第 0 轮（公开契约 mission_ref/卡片/台账三面一致
  ——不回查上一次送验）；#4 回归测试改**确定性同步屏障**（先登记停止——cause
  非空、cancel 为 no-op——再放行结果，select 必消费结果分支；旧实现 30/30
  失败、新实现 30/30 通过，休眠构造无鉴别力）。既有补正：classify 识别老式
  `to=returned` 退回事件（0.6.9 初版形态——实测真实 v22 历史两组共 3 条漏卡）；
  migration v27 首次重置投影游标使重扫幂等补投（INSERT OR IGNORE 按 source_seq
  ——已投行与状态不动；登记守卫避免每次启动全量重扫）。已知窗口（注记不修）：
  执行器终态提交前的停止确认窗口（TOCTOU）——完全关闭需停止意图入库参与
  终态事务，随后续版本评估；详情首次读取与来源轮询的跨回调交错由 #1 的
  已应用序号同一覆盖。
- v0.3（2026-11-02）：复审修复（独立复审报告"2 项 P1、3 项 P2、2 项 P3"）——
  #1 P1（基线即有）停止在场时执行器成功返回不得发布：done 分支统一先查停止
  意图（不分成败），迟到正文只进 late_result 审计（AT-12）；#2 P1（上轮引入）
  已取出的执行结果放回缓冲通道交 finalizeStopped——审计、释放恰一次，不空等
  stopGrace（租约不再滞留到 TTL）；#5 裁定轮次按同包（缺省最近）先行送验还原
  ——同包送验/裁定同轮，v22 旧式事件与升级补正计数不串轮；#6 版本行查到即
  保留空摘要（v1 目标仅 BOM 时不回退修订后目标）；#7 投递侧回填优先取房间
  实际删除时间（墓碑）；#3/#4 SPA 台账与来源两请求各自独立更新（互不阻塞），
  响应只丢弃比已显示更旧者（连续慢响应不再被发起序号全部丢弃）。
- v0.2（2026-11-02）：审校修复（独立审校报告"2 项 P1、6 项 P2、1 项 P3"）——
  #1 P1 事实归属当时契约：新事件的送验/裁定事务内写入实际 contract_version 与
  review_round（InjectReviewFacts），旧事件由投影按事件流回溯（created /
  contract_revised 序）+ 版本行 objective 快照 + 送验计数重建轮次——不读
  missions 当前行；#2 P1 + #6 P2 房首调度：Due 先取各房**队首**未完成行再判到期
  （NOT EXISTS 更早 pending/held）——队首暂停复查/退避中同房后继不越序，每房
  每轮一条、暂停房积压不占满批次饿死他房；#5 送验轮次随事实留痕（同 #1）；
  #8 migration v26 幂等回填升级前已删房的来源墓碑（按 room_tombstones 原删除
  时间）+ 投递侧发现删除时兜底回填（显式提交后返回）；#3 快照两路同形：
  room.TimelineItem += mission_ref（ProjectSnapshot 解码）+ SPA 快照映射；
  #7 lastMessage 反应锚点跳过 system 消息；#4/#9 SPA 台账轮询序号守卫 + 失败
  保留末次成功（空态/错误态分离）+ 顺带刷新来源元数据（他页删房可见墓碑）。
- v0.1（2026-11-02）：首版（随 0.6.10 实施）。
