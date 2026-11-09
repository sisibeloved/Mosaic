# Mosaic v0.7.4 — 跨 Harness 续办：Checkpoint 注入执行上下文（0.7 记忆与可移植恢复领域·第五件）

会话按 Run 隔离的设计下（0.6.13 八审裁定），任务连续性由 Inline 全量重递承担——但执行输入不含可恢复业务状态，换 Harness、旧会话失效或重启续跑时只能从契约裸启动。本件按 RFC-0018 §7.3/§7.4 把 0.7.3 的 Checkpoint 接进执行链：**原生会话不可用时仍能恢复工作**。两轮独立审校（3 + 1 = 4 项）全部关闭后定版。

## 新增

- **清单 checkpoint 层接线**（execctx 架构预留位启用——`checkpoint_unavailable` 仅首跑保持）：`Ports.Checkpoints` 闭包按任务取续办事实，装入 `checkpoint` 层（role resume，进 digest/存档/回执）；**漂移不阻断**（`checkpoint.mission_version != contract_version` → 层内 `contract_drift` 标记）；读取失败报错（不静默 omitted——恢复面不能丢事实）。
- **续办事实选取**（`LatestForResume`）：**业务快照优先**——无业务条时回退最新系统终态摘要（停止/完成/失败事实仍可达新会话）；有业务条时系统条永不入选（不遮蔽业务字段）。
- **Run 终态自动落盘**（0.7.3 预留兑现）：全终态路径（完成/失败/暂停/取消/墙钟停止/围栏拒绝）在终态写入成功后、租约释放前写系统检查点（`source=system`，摘要 ≤500 字）；**best-effort**（失败不阻断终态）。执行面显式落盘（业务步骤）仍走 0.7.3 API。
- **Inline 透传**：`manifestInline` 抽 `resume_checkpoint` 键 → supervisorExecutor 既有 `Context{Inline}` 全量重递——新会话 Boot 即收到续办状态，**换 Harness/重启后工作上下文不丢**。
- **预算裁剪**：可选层配额扣减已装入的 checkpoint 层字节数——检查点与必需内容本可放入 256 KiB 时不误阻断续办。

## 修复（两轮独立审校 4 项）

- **一审（1 P1 + 2 P2）**：checkpoints += `source` 列（migration v35）——系统终态摘要与业务快照分流（`LatestOfMission` 排除 system，不遮蔽业务字段）；execctx 可选配额扣减 checkpoint 层（不误阻断）；停止路径统一落盘（`saveTerminalCkpt` helper——全终态路径，保持租约/执行器退出顺序）。
- **二审（1 P2）**：`LatestForResume` 回退面——无业务快照时交付系统终态摘要（续办层不丢）；`CheckpointFacts += source` 透传标记。

## 验证

execctx UT（层装入/漂移/首跑 omitted/读错阻断）+ checkpoint UT（LatestOfMission/LatestForResume 优先序与回退）+ run UT（终态自动落盘/best-effort/停止路径）+ sqlite IT（v34/v35/回退）+ vet/-race + it/st 串行 + 生成物幂等 + SPA；真机冒烟端到端（业务快照漂移场景、系统摘要回退场景、预算裁剪、v34→v35 升级）ALL-PASS。

## 升级说明

- v0.7.3 数据库首次启动自动执行 migration v35（checkpoints += `source` 列，幂等加列）。
- 零 HTTP 契约变更（内部接线——生成物幂等验证）。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§1/§7.3/§7.4）与 [ADR-0041](docs/design/adr/ADR-0041-cross-harness-resume.md)（v0.3，含两轮审校修订记录）。
- 下一件：0.7.5 Portable Agent Bundle 导出（范围选择、摘要与脱敏报告——0.7.0 敏感度标记与 0.7.2 授权过滤口径在本件消费）。
