# ADR-0029：Mission 暂停、恢复与取消——在途 Run 的运行时控制面（0.6.6）

- 状态：Accepted（2026-10-31，随 0.6.6 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.2 Mission 状态机——`running ↔ waiting；running/waiting → paused → ready`、`非终态可进入 canceling → canceled`；§3.2 裁定——"取消和暂停都不能抹去既有动作。暂停以持久状态先阻止新调度，再请求在途 Run 在安全边界停止；无法安全终止或结果不可确认时显示待核验"；§9.4 变更时序——"停止与取消不要求 Runtime 就绪才受理：先持久化停止意图"；§11.2 `pause_mission / cancel_mission`——expected_version CAS + 在途处理策略；§11.3 迟到结果——generation/终态检查不覆盖）、[ADR-0023](ADR-0023-mission-contract.md)（契约状态机与 CAS 编排单源）、[ADR-0028](ADR-0028-mission-execution.md)（Run 域/持久调度/围栏两时点复核——本件的执行底座）、[ADR-0027](ADR-0027-runtime-lease.md)（租约释放使围栏失效——停止后迟到结果的拒收依据）、交付计划 v2.00（0.6.6：Mission 暂停、恢复与取消）

## 背景与问题

0.6.5 交付了执行闭环，但 Mission 只有 `draft → ready → running ↔ ready` 与
`canceled`（0.6.0 无在途版）：**执行一旦开始就无法干预**——持久调度器每
5s 自动派发 ready Mission，用户既不能叫停某件任务的自动执行（暂停），也不
能中途终止一次进行中的执行（取消在途）。RFC §3.2 裁定暂停/取消必须是
**先持久化意图、再在安全边界停止**的两段动作，且都不抹去既有动作、不被
迟到回调翻转。本件交付控制面：暂停/恢复/取消三命令 + 在途 Run 停止 +
重启恢复分派。

## 决策

### 1. Mission 状态机扩展（无 migration——状态值扩展）

```
现行：draft → ready → running ↔ ready；draft/ready/running → canceled
0.6.6：draft → ready → running ↔ ready
       ready | running → paused → ready          （暂停/恢复）
       draft | ready | running | paused → canceling → canceled   （取消两段）
```

- **paused**（执行控制态，非契约生命周期）：阻止新调度——`ListReady` 只取
  ready，paused 天然出列；恢复（paused → ready）后调度器下一轮自动拾取，
  不重启已停止的 Run（attempt 自然递增——被停止的尝试已以 canceled 终态
  留档）。
- **canceling**（停止中间态）：取消的"已持久化停止意图"事实——在途 Run
  停止期间对外可见（待停止）；全部在途 Run 终止后进 canceled。
- **对 RFC §3.2 的一处增列**：RFC 表为 `running/waiting → paused`；本版
  允许 **ready → paused**——0.6.5 持久调度器周期自动派发 ready Mission，
  "暂停先阻止新调度"（RFC §3.2 裁定）要求 ready 可暂停，否则暂停只能在
 执行的 5s 窗口内抢落。waiting 状态随 0.6.7+ 增列。
- **CAS 语义**：三命令均携 `expected_version`（§11.2 共用约定），匹配且
  状态合法才转移；控制命令**不递增 contract_version**（执行投影，非契约
  修订——与 0.6.0 Cancel 同口径）。
- **幂等与拒绝**：paused 再暂停（版本匹配）返回当前（重试安全）；ready
  再恢复（版本匹配）返回当前；draft 不可暂停（从未被调度）；canceled 拒
  绝一切（终态幂等拒绝——0.6.0 语义不变）；canceling 再取消 = 续驱停止
  （重复信号 + 有界等待，不报错）。版本不匹配 → `version_conflict`；状
  态不合法 → `invalid_transition`（败因分辨复用 casOrTransition 重读）。

### 2. 停止面（run 域：每 Run 独立取消上下文 + 注册表）

- **执行脱离请求上下文**：执行任务的上下文改为服务内
  `context.WithCancel(context.Background())` 派生（按 Run 注册）。行为变
  更（对 0.6.5）：HTTP 请求断开**不再中止执行**——执行是持久事实，中止
  只经控制面（暂停/取消）或进程退出（恢复面接管）。扫描触发的执行本就
  等价脱离。
- **停止注册表**（内存，mutex）：`runID → {cancelFunc, cause}`；终理化时
  注销。`SignalStopByMission(missionID, cause)`：对该 Mission 全部未终态
  Run 记录 cause 并 cancel。
- **执行侧终理化**：提交包一层 `select { 完成, runCtx.Done() }`——
  - 正常完成：现行 finalizeRun 路径不变（围栏核对 + 终态同序）。
  - 被停止：Run → **canceled**（新 Run 终态，RFC §3.2 Run 机
    `succeeded/failed/canceled/unknown` 的 0.6.6 子集），单次 UpdateRun
    同事务写 `stop_requested{cause: pause|cancel}` + `canceled` 两事件；
    围栏核对保留（租约仍在手——正常通过后释放）；**Mission 投影不动**
    （此时 Mission 必为 paused/canceling，非 running——`SetReadyFromRunning`
    条件转移幂等无操作，由 ADR-0028 #6/#7 的条件更新语义自然保证）。
  - **迟到结果不落**：被放弃的执行 goroutine 结果经缓冲通道丢弃（围栏
    已随租约释放失效——即使经任何路径再到达也无门可入，ADR-0027）。
- **进程树终止不在本版**：停止 = 结果拒收 + 围栏失效 + 域内取消传播
  （executor ctx cancel——echo/慢执行器面即返）。宿主进程树终止
  （RFC §10.4 RuntimeProvider `Cancel`）随 adapter 能力协商接入；此前
  真实 CLI 进程可能继续跑完但结果必被拒收（安全——不产生第二写入者）。

### 3. 控制编排（app 屏障层 missionControl，组合两域服务）

```
Pause(missionID, expectedVersion, reason):
  CAS ready|running → paused（与调度器 SetRunning 竞态由条件更新串行化，
    服务层 ≤3 次有界重读重试）→ 若来自 running：SignalStop(pause)，
  不等待（paused 已持久即已阻止新调度；在途 Run 异步终理化，Run 区可见
  canceled）

Resume(missionID, expectedVersion):
  CAS paused → ready（此后调度器自动拾取；无在途处置）

Cancel(missionID, expectedVersion, reason):
  CAS 非终态 → canceling → SignalStop(cancel) → 有界等待未终态清零
  （缺省 10s，100ms 轮询）→ FinishCancel（canceling → canceled）；
  超时 → 返回 canceling（"待停止"可见——RFC §3.2 待核验形态；可重复
  cancel 续驱；重启恢复兜底终理化）。无在途 Run 时 canceling → canceled
  同步直达。
```

- 取消语义对 0.6.0 的兼容：draft/ready 且无在途 Run → 单步直达 canceled
  （响应与事件序列含 canceling 中间事件，语义等价）；已取消 → 409
  invalid_transition（不变）。
- **有界等待不可无限**：执行器不响应取消（真实 CLI 阻塞面）时不得挂起
  HTTP 请求——超时返回 canceling 是"无法安全终止……显示待核验"的兑现。

### 4. 重启恢复分派（RecoverUnfinished 细化）

未终态 Run 在启动恢复时按 Mission 当前状态分派（停止意图已持久化者兑现）：

| Mission 状态 | Run 处置 | Mission 处置 |
|---|---|---|
| canceling | → canceled（"停止请求在途时重启——结果未知"） | → canceled |
| paused | → canceled（同上） | 停 paused |
| 其余（running 等） | → failed（结果未知；不自动重跑——0.6.5 语义不变） | 停 running |

### 5. 修订保持执行投影（Revise 投影保持）

running/paused/canceling 期间的契约修订：**执行控制态保持**（修订只落
新版本行与内容，不回写 draft/ready——在途 Run 不静默换输入，被停止/完成
后的下一 attempt 自然用新版本）。draft/ready 期间修订照旧完备性重判。此
举顺带修复 0.6.5 遗留投影缺陷：running 中修订会把状态覆写为 ready。

### 6. 契约面（OpenAPI 增两端点 + 一端点扩义；无 migration）

- `POST /v1/missions/{id}/pause`：`{expected_version, reason?}` →
  MissionView（404/409 version_conflict|invalid_transition/503 同族）。
- `POST /v1/missions/{id}/resume`：`{expected_version}` → MissionView。
- `POST /v1/missions/{id}/cancel`（0.6.0 端点扩义）：body 不变；响应可为
  canceled（已停止/无在途）或 **canceling**（停止进行中——客户端轮询）。
- MissionView / MissionSummary `status` 枚举 += `paused, canceling`；
  MissionRunView `state` 枚举 += `canceled`。
- 调度器诊断面与 ListRuns 不变（canceled 属终态，未终态口径
  `pending|running` 不变）。

## 非目标

- **waiting 状态与 unknown 深度处理**（0.6.7）：Run unknown 终态、结果
  核验、副作用审计不在本版。
- **进程树终止 / Draining 能力协商**（RFC §10.4 RuntimeProvider
  Cancel/Drain）：随真实 adapter 能力矩阵接入。
- **交付物撤回**：取消不抹去既有动作——已落库的 completed Run 与
  result_block 保留（§3.2 裁定）。
- **多 Run/WorkItem 并行处置**：单 Agent 单未终态 Run（ADR-0028 部分唯
  一索引）；多子项取消编排随 0.6.11+ WorkItem 体系。

## 验收要点

1. ready 暂停后调度器不再自动派发（多轮扫描零新 Run）；恢复后自动执行。
2. 在途 Run 暂停：Mission → paused 持久先行，Run 终态 canceled +
   stop_requested{pause} 事件，租约释放，迟到执行器结果不落。
3. 在途 Run 取消：canceling 可见 → Run canceled → Mission canceled
   （事件链 ready/running→canceling→canceled 完整留痕）。
4. 取消有界等待超时：返回 canceling 不挂起；重复取消续驱；重启后恢复
   分派兑现（canceling→canceled / paused 停留 / running→failed 未知）。
5. CAS：版本冲突 409 携当前版本；终态拒绝不重复记事件；幂等重试安全。
6. 修订保持 paused/canceling/running 投影；draft/ready 照旧重判。
7. canceled 不被迟到回调翻转（围栏 + 终态条件更新双保险——ADR-0027/0028
   语义复用）。

## 后果

- 执行任务脱离请求上下文：手动触发的 HTTP 请求断开后执行继续、终理化照
  常（控制面可显式停止）——0.6.5 的"请求断开清理面独立"演进为"执行面
  本身独立"。
- canceling 是可长期停留的中间态（执行器不可停止时）——任务中心必须展示
  "待停止"并允许续驱，不得以通用"重试"掩盖（RFC §11.2 UI 裁定）。
- 停止注册表是进程内事实：重启后由恢复分派兑现（意图已在库）。
- Run 机出现第四态 canceled——未终态口径（部分唯一索引、ListUnfinished）
  不变，终态集合 = completed | failed | canceled（unknown 随 0.6.7）。

## 修订记录

- v0.1（2026-10-31）：首版（随 0.6.6 实施）。
