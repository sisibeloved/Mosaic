# Mosaic v0.6.6 — Mission 暂停、恢复与取消（0.6 Mission 与本地执行领域·第七件）

0.6.5 交付了执行闭环（ready Mission → CLI 进程 → 终态落库）；本件补上**运行时控制面**：执行一旦开始即可干预——暂停阻止新调度、恢复自动拾取、取消两段化，且执行脱离 HTTP 请求上下文（关掉页面执行照跑，停止只经控制面）。

## 新增

- **Mission 暂停 / 恢复**（`POST /v1/missions/{id}/pause`、`/resume`）：`ready | running → paused → ready`——暂停状态持久化先行即**阻止新调度**（调度器只取 ready；对 RFC §3.2 的一处增列：ready 可暂停——周期调度器派发 ready，"暂停先阻止新调度"要求 ready 可停）；来自 running 时在途 Run 在安全边界停止（结果拒收 + 围栏失效——Run 终态 `canceled` + `stop_requested{pause}` 审计 + 租约释放），响应不等停止完成。
- **取消两段化**（`cancel` 端点扩义）：非终态 → **canceling**（停止意图持久化先行）→ 停止在途 Run（有界等待缺省 10s，独立上下文——请求断开不丢终态翻转）→ **canceled**；无在途同步直达。超时返回 canceling（"待停止"可见——可重复 cancel 续驱；重启恢复兜底）。
- **执行脱离请求上下文**（行为变更）：每 Run 独立取消上下文——HTTP 请求断开不再中止执行（执行是持久事实，中止只经控制面或进程退出）。
- **重启恢复按停止意图分派**：canceling → Run canceled + 收尾兑现；paused → Run canceled + 停 paused；其余照旧 failed（结果未知，禁自动重跑）。
- **迟到结果不落**：被放弃的执行 goroutine 结果经缓冲通道丢弃（围栏已随租约释放失效）。
- **修订保持执行控制投影**：running/paused/canceling 中修订契约不回写 draft/ready（在途 Run 不静默换输入）；顺带修复 0.6.5 遗留投影缺陷。
- **SPA**：任务详情控制区（暂停/恢复/取消/续驱停止按钮按状态呈现）+ paused/canceling 状态徽章与说明 + Run canceled 徽章。

## 修复（独立审校 4 项 P1、4 项 P2）

- **长任务收尾不再失效**：清理窗口（30s）改自**收尾时刻**起算——此前自执行开始起表，31 秒任务完成时窗口已耗尽，终态写入/租约释放随过期上下文静默失败（Run 滞留 running、租约滞留 active）。
- **暂停窗口内信号不再丢失**：停止句柄先于 Mission 投影注册——此前注册过晚，CreateRun 后暂停落地的窗口内信号扫描可见 Run 而注册表无句柄，执行照启、结果照落。
- **并发修订不再覆盖暂停/取消**：执行控制投影的保持移入存储事务内以现行状态为准——此前服务层旧读快照（ready）经版本 CAS 放行，把 paused/canceling 盖回 ready，Mission 重新入调度列、停止意图丢失。
- **停止时工作区独占不失守**：租约释放后于执行器退出（有界等待缺省 10s）——此前终态写入后立即释放，执行器可能仍在收尾写工作区，新执行随即领取同一工作区。
- **停止终态写库失败不再吞错**：上抛明确错误（Run 滞留 running + 租约滞留 active 事实对齐，恢复面兜底）。
- **恢复按完备性重判**：暂停期间修订删除验收条件后，Resume 落 **draft**（不入调度列），补齐后修订回 ready——此前直通 ready。
- **重启不再漏收尾**："Run 已终态、Mission 滞 canceling"按状态枚举兜底兑现（此前恢复面只随未终态 Run 走，此类滞留无人认领）。
- **Web 飞书授权不再误判弹窗失败**：`window.open` 携 `noreferrer` 按 MDN 语义成功也返回 null——null 不再当"被拦"（此前 Web 形态提示错误且轮询中断，而授权页已正常弹出）；桌面壳系统浏览器兜底照旧。

## 升级说明

- 无数据库 migration（状态值扩展）；v0.6.5 数据可直接升级本版。
- 控制面端点需 MissionControl 装配（桌面/服务端形态默认装配；纯 Web 形态返回 503）。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（Approved §3.2/§9.4/§11.2）与 [ADR-0029](docs/design/adr/ADR-0029-mission-pause-resume-cancel.md)。
