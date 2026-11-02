# ADR-0030：Run 崩溃恢复——unknown、迟到结果与核验处置（禁止盲目重跑）（0.6.7）

- 状态：Accepted（2026-11-01，随 0.6.7 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.2 Run 状态机——`queued → starting → running → succeeded / failed / canceled / unknown`，"unknown 先核验副作用，不能直接当失败重试"；§3.2 Mission——`running ↔ waiting`；§11.3 崩溃、副作用与恢复——"已启动但没有终态回执：检查 Runtime、进程与动作日志；无法确认时 Run=unknown，WorkItem 等待 reconciliation"、"取消或迁移后旧结果到达：generation / 终态检查；保留 late 回执，不覆盖新任务、不发布重复成果"、"其余沿用现有 Run 的保守 unknown 语义"；§11.2 错误语义 `action_outcome_unknown`——"UI 不以通用'重试'掩盖结果未知"；§11.5 可观测——"记录……重试与 unknown 数"；AT-12——"在副作用后、回执前崩溃，旧代次结果迟到：unknown 可核验；无盲重试；旧结果只审计；重复投递不产生重复交付"）、[ADR-0028](ADR-0028-mission-execution.md)（恢复语义占位："0.6.7 增 unknown 语义的 Mission 侧等待原因"）、[ADR-0029](ADR-0029-mission-pause-resume-cancel.md)（停止意图分派——canceling/paused 恢复路径本件不改；waiting"随 0.6.7+ 增列"）、交付计划 v2.00（0.6.7：Run 崩溃恢复：unknown、迟到结果及禁止盲目重跑）

## 背景与问题

0.6.5 的启动恢复把"进程重启时在执行"的未终态 Run 记为 **failed**（文本注明"结果未知"），
并让 Mission **永久滞留 running** 以阻止自动重跑——这是占位实现：
(1) 把"结果不可判定"伪装成"已知失败"，违反 RFC §3.2 Run 机的 `unknown` 语义；
(2) Mission 停在 running 而实际无任何在途执行，任务中心无法解释"为什么不动"；
(3) **用户无出路**——Mission 非 ready 则手动触发被拒（SetRunning changed=false），
除取消外没有任何命令能让它继续；(4) 停止后迟到的执行器结果被静默丢弃（缓冲
通道无人读），AT-12 要求"旧结果只审计"。本件兑现 RFC 的保守 unknown 语义：
**结果不可判定即 unknown、Mission 明示等待核验、用户显式处置后方可再执行、迟到
结果只审计不发布**。

## 决策

### 1. Run `unknown` 终态（migration v20：runs 增 resolution 三列）

```
重启恢复（RecoverUnfinished）分派——按 Mission 当前状态：
  canceling / paused（停止意图已持久）→ Run canceled（0.6.6 不变）
  running（无停止意图，结果不可判定）  → Run unknown（本件；此前 failed）
       + 租约释放（进程已死——持有不可能，释放安全）
       + Mission running → waiting（reason=reconciliation）
```

- **unknown 是终态**（出未终态口径——部分唯一索引/ListUnfinished 不变），但
  **可核验处置**（reconciliation）：`resolution ∈ '' | verified_completed |
  verified_failed | discarded`、`resolution_note`、`resolved_at`。
- **Run 状态不被处置改写**：系统从未观测到完成——用户 attests 的是副作用核验
  结论，不冒充系统事实（RFC §3.3"模型说'完成'不替代后两者"同一精神；
  §7.3"文字摘要不能将 pending 偷换成 completed"）。状态保持 unknown，处置
  事实在 resolution 列与 `resolved` 事件。
- 历史不回溯：0.6.5/0.6.6 已落的 failed（"结果未知"文本）行不改写。

### 2. Mission `waiting` 执行控制态 + `waiting_reason`（migration v20：missions 增列）

```
running → waiting(reconciliation)   恢复分派（有未处置 unknown Run）
waiting → ready | draft              处置后（完备性重判，清 waiting_reason）
waiting → paused                     暂停（reason 保留——RFC：running/waiting → paused）
paused  → waiting                    恢复：waiting_reason 非空即回 waiting（非 ready）
waiting → canceling → canceled       取消两段（非终态皆可）
```

- **出调度列**：ListReady 只取 ready——waiting 天然不被周期调度器拾取。
- **等待原因是一等事实**（RFC §2"任务中心持续展示……等待原因"）：
  `waiting_reason` TEXT 列，本版唯一取值 `reconciliation`；后续等待原因
  （runtime_unavailable / remote_delivery_unavailable / grant_required）同列承载。
- **暂停不清等待原因**：paused 期间 reason 留存，Resume 据此回 waiting——
  "禁止盲目重跑"不因一次暂停/恢复被绕过。
- **修订保持投影**：执行控制态集合 {running, paused, canceling} += waiting
  （ADR-0029 决策 5 延伸——存储事务内以现行状态为准，0.6.6 审校 P1 口径）。

### 3. 核验处置面（reconciliation——禁止盲目重跑的兑现）

```
POST /v1/mission-runs/{run_id}/resolve
  { outcome: verified_completed | verified_failed | discarded, note?: ≤500 字 }
  → 条件更新 runs（state='unknown' AND resolution=''）+ resolved 事件
  → Mission waiting → ready | draft（完备性重判；清 waiting_reason）
  → 200 MissionRunView（携 resolution）
```

- **再执行只能发生在显式处置之后**：unknown 期间 Mission 处于 waiting，调度器
  不拾取、手动触发 SetRunning(ready→running) 条件不符拒启动；处置把 Mission 放
  回 ready 即用户的显式决定（三种结论都允许再执行——0.6.7 尚无 Mission 完成
  态，ready = "可再执行"；交付/验收随 0.6.8/0.6.9）。
- 幂等与拒绝：同 outcome 重复 → 200 当前；不同 outcome → 409
  `run_already_resolved`；非 unknown → 409 `run_not_unknown`；未知 → 404。
- Mission 侧收尾为条件转移（waiting → target）：Mission 已被取消/暂停时无操作
  （处置事实仍落 Run——审计完整）。

### 4. 迟到结果只审计（AT-12——"旧结果只审计；不发布重复成果"）

- **停止路径**（0.6.6 finalizeStopped）：执行器结果若在停止宽限内到达，或宽限后
  经后台等待到达——均记 `late_result` 事件（payload：outcome ok|error、
  preview ≤400 字、generation、published=false、phase=after_stop）；**Run 行不变
  （state canceled）、结果不落 result_block、不发布**。此前经缓冲通道静默丢弃。
- 存储面新增 `AppendRunEvents`（事件追加，不重写行）——审计写入不与行状态争
  写（终态行只增事件）。
- **重启后无进程内迟到源**（执行 goroutine 随进程消亡）——unknown Run 的"旧结
  果"只能以**工作区副作用**形态存在，由用户经 resolve 核验兑现（§11.3"外部写动
  作成功但本地未确认……人工/授权核验后决定"）。围栏拒绝路径（finalizeRun
  fence_rejected 携 detail）保持为另一形态的迟到审计。

### 5. 可观测（RFC §11.5"记录 unknown 数"）

- 调度器诊断面 `GET /v1/scheduler/status` += `unresolved_unknown_runs`（存储
  `ListUnresolvedUnknown`）。

### 6. 契约面（OpenAPI）

- `POST /v1/mission-runs/{run_id}/resolve`（RunResolve 请求体）。
- MissionView / MissionSummary `status` 枚举 += `waiting`；MissionView +=
  `waiting_reason`。
- MissionRunView `state` 枚举 += `unknown`；+= `resolution`（枚举）、
  `resolution_note`、`resolved_at`（nullable）。
- MissionRunEvent 事件类型文档 += `unknown` / `late_result` / `resolved`。
- SchedulerStatus += `unresolved_unknown_runs`。

### 7. 迁移 v20

```sql
ALTER TABLE runs ADD COLUMN resolution      TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN resolution_note TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN resolved_at     TEXT NOT NULL DEFAULT '';
ALTER TABLE missions ADD COLUMN waiting_reason TEXT NOT NULL DEFAULT '';
```

逐语句执行、重复加列容忍（v18 同款——新库经 v19 建 runs 后再加列；旧库直接
加列；再次打开全部"duplicate column"容忍）；migrations 登记 20。

## 非目标

- **ActionIntent / `opaque_execution` 标记**（RFC §11.3 受控写动作登记与可幂等
  重试判定）：随工具/动作面接入——本版一律保守 unknown，不自动判定安全重试。
- **远端回读核验**（稳定 action_id 查询外部系统）：无外部动作面可查。
- **WorkItem 级 reconciliation 与多子项处置**（0.6.11+）。
- **Mission 终态 failed 处置**、交付/验收（0.6.8/0.6.9）。
- **原生会话恢复**（RestoreNativeSession 可选能力）。

## 验收要点

1. 重启恢复：running Mission 的未终态 Run → unknown（非 failed）+ 租约释放 +
   Mission → waiting(reconciliation)；canceling/paused 路径不变（canceled）。
2. waiting 出调度列：多轮扫描零新 Run；手动触发拒启动。
3. 处置：三种 outcome 均落 resolution + resolved 事件，Run 状态仍 unknown；
   Mission waiting → ready（完备）| draft（不完备），waiting_reason 清空；此后
   调度器可拾取（attempt+1）。
4. 幂等/拒绝：重复同 outcome 200；异 outcome 409 already_resolved；非 unknown
   409 run_not_unknown；未知 404。
5. 暂停/恢复不绕过：waiting → paused（reason 留存）→ resume → waiting；取消
   两段照常。
6. 迟到审计：停止后执行器结果到达 → late_result 事件、Run 仍 canceled、
   result_block 空；宽限后到达亦审计。
7. 诊断面 unresolved_unknown_runs 随处置归零；v20 新库/旧库升级均通过。

## 后果

- Run 机出现第五态 unknown；终态集合 = completed | failed | canceled | unknown。
  "failed" 从此只表示**已观测到的失败**。
- Mission 多一个可长期停留的 waiting 态——任务中心必须展示等待原因与处置入
  口，不得以通用"重试"掩盖（RFC §11.2 UI 裁定）。
- 处置是用户 attest，不是系统事实：审计读者须区分 `state` 与 `resolution`。
- 停止宽限后仍可能有后台等待 goroutine（每个被放弃的执行器一个）——有界于被
  放弃执行数，进程退出即消亡。

## 修订记录

- v0.1（2026-11-01）：首版（随 0.6.7 实施）。
