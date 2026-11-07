# ADR-0041：0.7.4 跨 Harness 续办——Checkpoint 注入执行上下文（0.7.4）

- 状态：Accepted（2026-11-06，随 0.7.4 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§1"即嵌即走"——更换 Harness/环境后可从持久任务状态恢复工作，不承诺无损搬运 CLI 内部会话；§7.1 Native Session 是 Harness 私有、"可选恢复提示，不是便携事实源"；§7.3 Checkpoint 层位与"最后一次动作之后尚未落盘则恢复时核验回执"；§7.4 续办保留面：稳定身份/任务版本/授权记忆/Checkpoint/工作文件）；[ADR-0025](ADR-0025-exec-context-manifest.md)（execctx 层结构——checkpoint 层自始预留）；[ADR-0040](ADR-0040-run-checkpoints.md)（0.7.3 Checkpoint 存储——"执行面自动挂点随 0.7.4 续办接线"）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.4 唯一交付特性：跨 Harness 续办——原生会话不可用时仍能恢复工作）。

## 背景与问题

会话按 Run 隔离（`mrun:<run_id>`——0.6.13 八审裁定：撤权后不复用旧线程），任务连续性由 **Inline 每次全量重递**承担；0.7.3 交付了 Checkpoint 存储，但清单的 checkpoint 层仍恒 omitted（`ReasonCheckpointUnavailable`）——执行输入不含可恢复业务状态，新 Run（换 Harness、旧会话失效、重启续跑）只能从契约裸启动，丢掉上一段工作的进度事实。

## 决策

### 1. execctx checkpoint 层接线（架构预留位启用）

`execctx.Ports` += `Checkpoints CheckpointFactsFn`（闭包端口——execctx 不 import checkpoint 域，依赖方向同既有端口）：

```go
// CheckpointFacts 续办事实（§7.3 字段的清单投影——进入 digest/存档/回执）。
type CheckpointFacts struct {
    CheckpointID, RunID string
    CheckpointVersion, MissionVersion int
    ContractDrift bool // 落盘版本 != 清单契约版本——续办输入带漂移标记
    CompletedSteps, PendingSteps, ArtifactRefs,
    ActionReceipts, UncertainActions, WaitingConditions []string
    ProgressSummary string
}
```

- assemble 在 contract 层后装入 checkpoint 层（role `resume`——§7.3 组装序中 Checkpoint 位于任务契约/验收之后）；无检查点（首 Run）仍 omitted（`ReasonCheckpointUnavailable`——首跑无续办状态是常态而非缺陷）。
- **漂移不阻断**：`checkpoint.mission_version != contract_version` → 层内容带 `contract_drift: true`（进度是业务状态，契约修订不必然作废；消费方据此重读权威任务状态——"必须能重新读取权威任务状态"）。
- 进 256KiB 预算与 digest（同检查点状态同 digest——幂等重建语义与既有层一致）。

### 2. 按 Mission 取最新检查点（跨 Run 续办）

`checkpoint.Store/Service` += `LatestOfMission(ctx, missionID)`（sqlite `ORDER BY rowid DESC LIMIT 1 WHERE mission_id = ?`；memstore 插入序兜底）——新 Run ≠ 旧 Run，续办取的是**任务级**最新。

### 3. Run 终态自动落盘（安全边界挂点——0.7.3 预留兑现）

`run.Service` += `Checkpoints` 端口（`SaveTerminal(ctx, TerminalCheckpoint) error`——run 不 import checkpoint）；`finalizeRun` 在 `UpdateRunTerminal` 成功后写系统检查点：`progress_summary = "Run {id} 终态 {state}" + 结果/停止摘要`（≤500 字），`action_receipts` 空（回执核验由恢复面按 §7.3 处置），steps 不伪造（系统不知业务步骤——显式落盘归执行面）。**best-effort**：落盘失败不阻断终态（终态是权威事实；检查点是派生可恢复状态）。

### 4. Inline 透传（消费端零改动）

`manifestInline` 为 checkpoint 层抽 `resume_checkpoint` 键 → `supervisorExecutor` 既有 `Context{Inline}` 全量重递（app.go:1225 不动）——新会话 Boot 即收到续办状态，原生会话不可用不再丢工作上下文。

### 5. 测试与验证

execctx UT（层装入/漂移标记/无检查点 omitted/预算截断/digest 稳定）、checkpoint UT（LatestOfMission 跨 Run 取最新）、run UT（终态自动落盘 best-effort——正常写入/写失败不阻断终态）、IT（执行→终态→新 Run 清单含 checkpoint 层端到端）、真机冒烟（落盘业务 checkpoint → 手动新 Run → 清单/回执含 resume 层）。

## 后果

- "原生会话不可用时仍能恢复工作"闭环：Checkpoint（0.7.3）→ 清单层（本件）→ Inline 重递（既有）——换 Harness/重启后新 Run 的执行输入携带上一段工作的全部可恢复事实。
- 清单 digest 现随检查点演进——每次落盘后首次装配产生新清单（符合预期：上下文确实变了）。
- Bundle（0.7.5+）导出面自然复用 CheckpointFacts 投影；Slots 换 Agent 交接（0.7.9）不复制私有记忆、Checkpoint 属任务状态随任务走。
