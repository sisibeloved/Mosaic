# ADR-0032：Mission 验收——`request_review` 与 `review_mission`（0.6.9）

- 状态：Accepted（2026-11-02，随 0.6.9 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.2 Mission 状态机——`running → reviewing → accepted`、"reviewing 可退回 ready"、"accepted/canceled/failed 不被迟到回调翻转"；§3.3——"执行成功、成果提交、验收通过是三个事实"；§10.2——"负责人汇总交付包后调用 `request_review`，校验通过才使 Mission 进入 reviewing 并通知契约中的验收人；验收人通过 `review_mission` 接受或退回。执行回执、提请验收与验收裁定各自留痕"；§11.2 命令表——`request_review`（MissionID、delivery_package_ref、expected_version → 状态不允许、版本冲突或必需交付项缺失则拒绝）、`review_mission`（MissionID、expected_version、验收结论、逐项依据与退回原因 → 非 reviewing、版本冲突或验收权限不足拒绝）；AT-10——"request_review 缺必需项时拒绝，补齐后进入 reviewing；仅授权 review_mission 可裁定接受/退回；任务卡、产物、决定可互相追踪"）、[ADR-0031](ADR-0031-delivery-package.md)（交付包 append-only、`package_id` 寻址、完备性归送验面）、[ADR-0029](ADR-0029-mission-pause-resume-cancel.md)（两段取消——非终态皆可取消）、交付计划 v2.00（0.6.9：Mission 验收：`request_review` 与 `review_mission`）

## 背景与问题

0.6.8 交付了第二个事实"成果提交"，并把完备性检查明确推给送验面。此时 Mission 的
生命周期仍止于 ready ↔ running：**没有"验收"这一第三事实**——交付包提交后既不能
送验，也没有人能作出接受/退回裁定，Mission 永远不会"完成"。RFC §3.2 为此裁定
`reviewing → accepted` 与"reviewing 可退回 ready"；§11.2 把送验与裁定拆成两个显式
命令，各自留痕；AT-10 要求送验面拒绝缺必需项的交付。本件交付第三个事实：**提请
验收**（含必需交付项检查）与**验收裁定**（接受 = 首个"完成"终态；退回 = 回 ready
再执行/再交付/再送验）。

## 决策

### 1. Mission 状态机扩展（migration v22：missions 增 `review_package_id`）

```
ready → reviewing（request_review：CAS + 交付包必需项检查）
reviewing → accepted（review_mission accepted——终态：不修订/不取消/不执行/不再交付）
reviewing → ready（review_mission returned——退回原因留痕；可再执行、再交付、再送验）
reviewing → canceling → canceled（两段取消照常——非终态皆可取消，ADR-0029）
```

- **送验入口为 ready**：RFC §3.2 表为 `running → reviewing`（WorkItem 连续执行
  模型）；本版执行投影在 Run 终态后回 ready（ADR-0028），故 ready 即"执行完毕且
  不再在途"——语义等价。waiting（unknown 待处置）/paused/draft/running 不可送验：
  先处置/恢复/补齐/等执行终态。
- **验收期间契约冻结**：reviewing 下 `revise` 拒绝（验收基线不得变动）；`pause`
  拒绝（验收门不是执行，无可暂停之物）；`submit_result` 拒绝（受审包已固定——
  退回后再补证）。取消照常。
- **accepted 为终态**：与 canceled 同列——修订/取消/暂停/送验/再交付/处置均拒绝；
  调度器只取 ready，天然不执行。"不被迟到回调翻转"由条件更新语义保证。
- **`review_package_id` 一等列**（AT-10"任务卡、产物、决定可互相追踪"）：
  request_review 写入受审包 id；accepted 保留（追溯"按哪个包验收"）；returned 清
  空（退回后重新送验另指）。Mission → package → Run → 事件流可互相追踪，无需翻
  事件流重建。

### 2. `request_review`（送验面）

```
POST /v1/missions/{id}/request-review  { expected_version, package_id }
  → 包存在、属该 Mission、contract_version == Mission 当前版本（否则包陈旧）
  → 必需交付项检查（AT-10）：
       artifact_refs 非空（有成果引用）
       且 Mission 当前 acceptance_criteria 每一条在包的 acceptance_mapping 中
       有对应条目（criterion 文本相等）且 evidence 非空
     缺项 → 409 missing_required_items（携缺项清单——可补证后重试）
  → mission CAS：ready → reviewing + review_package_id（状态不允许 409
     invalid_transition；版本不符 409 version_conflict）
  → status_changed{from: ready, to: reviewing, package_id} 事件
```

- **必需项口径**（本版）：成果引用非空 + 验收逐项有证据。deliverable_specs 与
  artifact_refs 的逐项对应无结构化字段，随结构化交付增强；本版不以 spec 条数
  校验（避免伪精确）。
- **幂等**：已 reviewing 且 review_package_id 相同（版本匹配）→ 200 当前；不同
  包 → 409 invalid_transition（"reviewing 中不可换包——先退回"）。
- **通知验收人**：持久通知/回房随 0.6.10（outbox）；本版任务中心以 reviewing
  徽章 + "等待验收"提示承载。

### 3. `review_mission`（裁定面）

```
POST /v1/missions/{id}/review
  { expected_version, reviewer?, decision: accepted | returned,
    reason?（returned 必填）, item_notes?[{criterion, note}] }
  → 非 reviewing → 409 invalid_transition；版本不符 → 409 version_conflict
  → 验收权限：契约 reviewer 非空时 body.reviewer 须相等（否则 403
     review_not_authorized）；契约 reviewer 为空 = 请求人自验（owner）
  → accepted：reviewing → accepted（review_package_id 保留）
     returned：reviewing → ready（review_package_id 清空）
  → status_changed{from: reviewing, to, decision, reviewer, reason,
     item_notes, package_id} 事件——"逐项依据与退回原因"留痕
```

- **权限模型**（本地单用户）：写门仍是 owner token；`reviewer` 字段是**契约一致
  性校验**（裁定人须是契约指定的验收人），不是独立身份认证——多主体授权随网关/
  委派。`item_notes` 为逐项依据（可选、≤16 项、每项 ≤500 字）；`reason` 在
  returned 时必填（≤500 字）。
- **退回后的路径**：Mission 回 ready——可再执行（attempt+1）、可补证再交付（新
  包）、可再送验（指向新包）。被退回的包不改写（append-only）。

### 4. 对既有面的收口

- **delivery 提交守卫**（ADR-0031 决策 2 延伸）：写包事务内守卫从"非 canceled"扩
  为"非 canceled / 非 reviewing / 非 accepted"（409 mission_canceled /
  mission_reviewing / mission_accepted），服务层预读同口径。
- **mission 域守卫**：Revise 拒 reviewing/accepted；Pause 入口集合不含 reviewing/
  accepted；MarkCanceling 拒 accepted（终态集合 = canceled | accepted）；Resume/
  MarkWaiting/ClearWaiting 条件转移天然不涉及。
- **调度**：ListReady 只取 ready——reviewing/accepted 不被执行。

### 5. 契约面（OpenAPI）

- `POST /v1/missions/{id}/request-review`（MissionReviewRequest）、
  `POST /v1/missions/{id}/review`（MissionReviewDecision）→ MissionView。
- MissionView / MissionSummary `status` 枚举 += `reviewing, accepted`；
  MissionView += `review_package_id`。
- 错误语义：`invalid_review`（400）、`package_not_found`（404）、
  `missing_required_items` / `stale_package` / `package_mission_mismatch`（409）、
  `review_not_authorized`（403）；delivery 提交 += `mission_reviewing` /
  `mission_accepted`（409）。

### 6. 域放置

新建 `internal/review`（送验/裁定编排：Missions 端口——事实 + 两条 CAS；
Packages 端口——只读包事实）。mission 域提供 `RequestReview` / `RecordReview`
（CAS 存储方法 + 服务层幂等/拒绝分辨）；delivery 域不变（只读消费）。app 装配
注入；httpapi 经 `Deps.Review`。

### 7. SPA

任务详情：ready 且有交付包 → 「提请验收」（选包，缺项由服务端 409 回显清单）；
reviewing → 徽章 + 受审包引用 + 验收面板（接受 / 退回 + 退回原因 + 逐项备注，
reviewer 预填契约验收人）；accepted → 终态徽章，控制区与交付表单只读；退回 →
回 ready 并显示最近退回原因（事件流）。

## 非目标

- **通知验收人 / 结果回房**（0.6.10 outbox）：本版仅任务中心可见。
- **Mission `failed` 终态**（"失败处置后 failed"）：随后续失败处置面。
- **多主体身份认证的验收权限**：reviewer 为契约一致性校验，非独立 token。
- **deliverable_specs ↔ artifact_refs 逐项校验**：随结构化交付增强。
- **reopen_request**（0.6.11）。

## 验收要点

1. 完整包（引用非空 + 全部验收条有证据）→ request_review 201/200：reviewing +
   review_package_id + 事件；Mission 从此不被调度器执行。
2. 缺证据包 → 409 missing_required_items 携缺项；补证新包再送验 → reviewing
   （AT-10 主路径）。
3. 包陈旧（契约已修订）→ 409 stale_package；包不属 Mission → 409；非 ready →
   409 invalid_transition；版本冲突 → 409。
4. review accepted → accepted 终态：修订/取消/暂停/送验/再交付/执行均拒绝；
   review_package_id 保留；事件携 decision/reviewer/item_notes。
5. review returned → ready + review_package_id 清空 + reason 留痕；可再执行/再
   交付/再送验。
6. reviewer 与契约不符 → 403；契约 reviewer 空 → 任意（自验）通过。
7. reviewing 期间 revise/pause/submit 拒绝；cancel 两段照常。
8. 迁移 v22 新库/旧库（v21 形状）升级；真机：执行 → 提交 → 送验（缺项 409 →
   补证 → reviewing）→ 退回 → 再送验 → 接受 → 终态拒绝族 → 重启持久。

## 后果

- Mission 首次拥有"完成"终态（accepted）——任务中心出现已完成任务；0.6.10 的回房
  通知以 accepted/returned 事件为源。
- 验收期间契约冻结是对 0.6.0 "非终态可修订"的收紧——修订须先退回。
- review_package_id 是追踪锚：审计读者可从 Mission 直达受审包与来源 Run。

## 修订记录

- v0.1（2026-11-02）：首版（随 0.6.9 实施）。
- v0.4（2026-11-02）：三审收口（4 项 P2）——#1 契约版本切换/列表重载即清送验
  选择（加载慢或失败窗口不持旧包；发送前再核所选包版本）；#2 受审包加载失败
  原地重试（不必退出任务详情）；#4 同包同版本在审的匹配重试**先行走幂等路径**
  返回当前 reviewing（必需项检查只对新一轮生效——v22/v23 升级库的在审包不因
  证据口径收紧而 409）；#5 终态处置测试补正（先在 ready 期成功建 Run 再转
  accepted，断言 ErrMissionTerminal——Run 不存在的"通过"不算数）。
- v0.3（2026-11-02）：复审修复（2 P1 + 8 P2 + 2 P3）——R1 P1 未决 Run 双向堵
  （CreateRun 写事务内核对任务仍是 ready|running——送验后过期调度拒绝并释放；
  RecordReview 接受事务内复查未决 Run）；R2 P1 migration v24（v23 升级残留补正：
  reviewing/accepted 轮次 0 → 1 恢复可裁定；canceled 行清受审包锚——R9）；R10 裁
  定事务核对包锚（预填未来轮次 + 换包交错时按实际锚拒绝，事件不可能记作旧包）；
  R5/R7/R8 终态写事务内核对（execctx 新建清单、ResolveRun 处置、AcquireLease 租
  约——迟到交错写入不再落库）；R6 判空统一（Unicode White_Space ∪ U+FEFF——
  blankTrimmed/blankTrim 前后端同集）；R12 轮次仅 reviewing/accepted 公开（库内
  单调计数不受影响）；R3/R4/R11 SPA（包列表随提交刷新 + 当前版本过滤收紧、受审
  包加载成功且一致才可裁定、accepted 装配卡只读）。
- v0.2（2026-11-02）：审校修复（独立审校 3 P1 + 12 P2）——#1 P1 送验轮次守卫
  （migration v23 missions.review_round 单调递增；裁定请求携 review_round，
  SQLite/MemStore 裁定事务内核对轮次+包+契约版本+状态——旧标签页/迟到/重放的
  旧轮次裁定 409；同包重审轮次也递增，单比包 ID 不足以防串轮）；#2 P1 纯空白
  证据按 TrimSpace 判非空（缺项 409 携标准清单）；#3 P1 未决 Run 拦送验（在途/
  未处置 unknown——送验前置 + SQLite 事务内权威复核 ErrUnsettledRuns；ready 残留
  崩溃形态同样进 waiting；accepted/canceled 终态封处置入口 ErrMissionTerminal）；
  P2：#4 裁定草稿按轮次隔离（SPA）、#5 accepted/reviewing 按钮门控、#6 送验选包
  （不再固定最新）、#7 验收人预填 + 逐项备注、#8 并发送验同包幂等（CAS 未胜重读）、
  #9 验收人 ≤64 字、#10 退回事件 to 为真实状态 ready、#12 expected_version 预读
  校验（防与修订交错跳标准）、#13 两段取消清受审包锚、#14 受审包详情可核对、
  #15 accepted 拒新装配（execctx）、#16 accepted 拒租约（runtime）。
