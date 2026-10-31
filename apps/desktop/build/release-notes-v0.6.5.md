# Mosaic v0.6.5 — 单 Agent Mission 执行（0.6 Mission 与本地执行领域·第六件）

0.6.2–0.6.4 交付了执行的三件底座——"执行什么"（契约 + 存档清单）、"在哪执行"（环境 + 运行时）、"占用与旧实例拒绝"（租约 + 围栏）；本件把它们**连成第一次真实执行闭环**：从 ready Mission 到运行中的 CLI 进程再到完成/失败事实落库。

## 新增

- **Run 域**（RFC-0018 §6/§10.2 / ADR-0028，migration v19）：一次执行尝试（run_ 前缀）——`(mission_id, attempt)` 唯一 + 每 Mission 至多一个未终态行；绑定租约/清单/运行时/任职/Agent + 围栏令牌与代次；`pending → running → completed | failed`。`run_events` 流含 `fence_rejected` 审计（迟到旧实例结果被围栏拒绝——0.6.4 围栏的第一个消费者）。
- **持久调度器**（5s 周期 + 手动触发共用路径）：对每个 ready Mission——owner active Binding 解析 → ready Runtime（按工作区身份排除被占用者）→ 领取租约（ADR-0027）→ 装配存档清单（ADR-0025——层内容块传透：目标/验收/近期消息/工具清单入执行 Inline，非仅清单 ID）→ 创建 Run + Mission → running → **提交执行**（经 agent.Supervisor 按身份域 adapter 家族名接通真实 CLI）→ **围栏核对**（提交前复核 + 终态前紧邻核对——迟到结果不落）→ Run 终态 + 租约释放 + Mission 回 ready（attempt+1 可再执行）。
- **编排安全面**：canceled 后拒启动（SetRunning 返回 changed——条件更新校验实际修改行数）；请求断开后清理面独立 30s 超时上下文（终态/释放/投影不遗留 running + 活跃租约）；终态落库失败返回明确错误（非静默吞错）。
- **启动恢复**：未终态 Run → failed（"结果未知；不会自动重跑"）+ 租约释放——Mission **停在 running**（不回 ready），下次扫描不自动调度，等待用户手动再执行（RFC"禁止盲目重跑"强化）。
- **API 五端点**：手动调度 / Run 历史 / Run 详情 / Run 事件流 / 调度器诊断面。任务中心详情增「执行」区（手动执行按钮 + Run 历史 + 10s 轮询后台刷新）。
- **Mission running 投影**：`TransitionStatus` 条件状态更新（ready ↔ running，非契约生命周期状态——run 事件流承载执行细节）。

## 修复（独立审校 7 项 P1、5 项 P2）

- **执行输入不再丢失**：存档清单层内容块（目标/验收标准/近期消息/工具清单）传透到执行器 Inline——不再只传清单 ID。
- **真实执行器接通**：经 Agent 身份域解析 adapter 家族名（echo 仅测试装配兜底）——所选 Runtime 与工作区被实际使用。
- **租约释放后不再启动**：提交前围栏复核（失效/检查错误均拒绝启动并记 fence_rejected 审计）。
- **旧结果不再能发布**：围栏核对紧邻终态写入（finalizeRun 统一），检查错误保守拒绝；修复围栏取反逻辑缺陷。
- **请求取消不再使任务卡死**：清理面独立 30s 超时 context——请求断开后终态/释放/投影不遗留。
- **重启后不再自动重跑**：恢复后 Mission 停在 running（不回 ready），等待用户决定。
- **已取消任务不再启动**：SetRunning 返回 changed=false 时 Run 置 failed——条件更新校验实际修改行数。
- **空闲 Runtime 不再被冲突候选挡住**：按工作区身份排除被占用者（后续可用候选不被挡）。
- **Mission 状态枚举补 running**；**执行时间字段（started_at/finished_at）补入视图**；**SPA 后台刷新**；**终态落库失败返回明确错误**。

## 升级说明

- v0.6.4 数据库首次启动自动建 `runs` / `run_events` 两表（migration v19），无需手工步骤；旧版本二进制可直接升级本库。
- 本版交付执行闭环骨架：echo 测试桩与真实 CLI 均可执行（按 Agent 身份域自动解析 adapter）；暂停/恢复/取消在途 Run（0.6.6）、崩溃恢复 unknown 深度处理（0.6.7）、结构化交付（0.6.8/0.6.9）随后续小版本接入。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（Approved §6/§10.2）与 [ADR-0028](docs/design/adr/ADR-0028-mission-execution.md)（v0.2）。
