# Mosaic v0.6.7 — Run 崩溃恢复（0.6 Mission 与本地执行领域·第八件）

0.6.6 给了执行"暂停/取消"的控制面；本件补上**崩溃诚实面**：进程死在执行中怎么办——"结果未知"是独立事实（不是失败），副作用可能已发生所以不许自动重跑，核验后由人来处置。

## 新增

- **Run unknown 终态**（RFC-0018 §3.2 Run 机补全 / ADR-0030）：重启时仍在执行、无终态回执的 Run → unknown——failed 从此只表示**已观测到的失败**，崩溃残留（结果不可判定）不再冒充失败。unknown 终态但**可核验处置**（resolution），处置不改写状态（用户 attest 非系统事实）。
- **Mission waiting 等待态**（migration v20 加列）：恢复分派结果不可判定 → `running → waiting(reconciliation)` 出调度列，直至用户处置；带 waiting_reason 一等事实——暂停保留原因、恢复回 waiting（不绕过禁止盲目重跑）、取消两段照常。
- **核验处置面 `POST /v1/mission-runs/{id}/resolve`**：`{outcome: verified_completed | verified_failed | discarded, note?}`——处置事实落 resolution 三列 + resolved 事件（**条件更新恰一胜者**）；处置后 `waiting → ready | draft`（完备性重判）。**再执行只发生在显式处置之后**。幂等：同结论 200；异结论 409；非 unknown 409。
- **迟到结果只审计**（AT-12"旧结果只审计"）：停止后执行器结果到达 → `late_result` 审计事件（宽限内即刻、宽限后后台等待）——Run 行不变、结果不落、不发布。
- **诊断面**：调度器状态新增 `unresolved_unknown_runs`（未处置残留计数）。
- **SPA**：waiting 状态说明与徽章、unknown Run 行内处置控件（三结论 + 备注）。

## 修复（本版随附）

- **停止超窗不再释放租约**（0.6.6 遗留 P1）：停止等待超窗时执行器可能仍在写工作区——改为持有租约、后台等执行器退出（先补迟到审计再释放；跨进程由租约 TTL 兜底）：工作区独占性优先。
- **旧库升级收敛**：v0.6.5/0.6.6 的"failed+结果未知"终态行升级后自动认领——转 unknown（事件追加不抹历史）；Mission 卡 running 形态进 waiting，ready 形态由执行面围栏（409 `residue_unresolved`）挡自动重跑；处置后照常放行。
- **处置投影失败上抛**：处置已落库而 waiting 清除失败不再静默——同结论幂等重试会补投影。
- **中文正文按 rune 截断**：不再产出无效 UTF-8、不再提前丢内容。
- **mcode 更新修复**：`mcode update` 无终端时打印指引即退 0（呈"已更新"假象）——改 npm 钉参直装（参数按 mcode 自荐命令），真机 0.6.1 → 0.7.4。
- **zcode 检测/更新面**：检测基线改取 App 版本（Info.plist）；无自动通道家族呈现禁用态按钮+指引；feed 定位双布局（macOS 修正）。

## 升级说明

- 数据库 migration v20（missions 加 waiting/waiting_reason 列）；v0.6.6 数据可直接升级。
- 旧库中 0.6.5/0.6.6 写下的"failed+结果未知"行会在升级后首次启动被自动收敛为 unknown 并等待处置——不会自动重跑；在 Run 区处置后 Mission 回调度列。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（Approved §3.2/§11.3/AT-12）与 [ADR-0030](docs/design/adr/ADR-0030-run-crash-recovery.md)。
