# Release v0.7.9 — Slot 换 Agent 交接：职责知识与在办事项转移，不复制私有记忆（0.7 记忆与可移植恢复领域·第十件·收官）

0.7 线收官件：RFC-0018 §8.1"同 Slot 换 Agent"的迁移承诺落地——必须携带职责实例知识、Mission/WorkItem 状态与承接回执；**不得复制整个人格**。换将从逐任务手工 offer/accept 变为一次编排，且全程可审计、可中断收敛。

## 新增

- **一次编排换将**：`POST /v1/slots/instances/{id}/handover`——七步幂等编排（前置核对零变更 → 新任就位 → **排空边界**（active→draining 停接新单——0.6.13 建单门此后拒）→ **全属收集**（不论归属——前轮遗留转移检测）→ 逐任务转移 → 旧任释放 → 新任激活与承接回执**同一事务** → 报告）。**单写者不变量**（边界先于收集、转移先于释放、激活最后且与回执原子）；服务级互斥串行化并发换将；中断重跑按现势收敛（无 attempt 持久化）。
- **在办转移**：mission 域单事务 `ReassignMission`——归属锚 / HandoverOfferable 域单源 / 无在途 0.6.12 交接 / 承接人 active 四项事务内核对 → owner 翻转（版本不递增）+ ready/running/waiting→paused（新任核验检查点后续办）+ `handover_accepted` 承接回执（cause=slot_handover——MissionEvent 枚举零新增）；reviewing/canceling 不转移（回执如实列出）；**在途单任务交接整体 409**（不静默吞 0.6.12 承诺）；**遗留转移改目标 409**（循原目标重跑收敛）；在途 Run **停止信号先行**（app 闭包——无"翻转成功而停止信号永不发出"的空洞）。
- **职责知识不搬动 + 私有记忆显式排除**：slot 命名空间以实例为 scope 天然跨任职（交接 = 确认 + 按 status 计数清单入回执）；上任 agent 命名空间计数入排除面（仅计数不入正文——"不得复制整个人格"的可审计呈现）。
- **承接回执体系**：`SlotHandoverReceipt`（转移/不转移明细含原因、知识清单、排除面、绑定变迁）+ 新任绑定流 `slot_handover` 事件（BindingEvent 枚举扩展）；旧任识别按**曾任 active 证据**（不以创建序猜测）；**回执补偿按 (任务, 当时转出人) 配对对账**——mission_events 持久事件源（终态/改派后的历史转移仍可补齐；跨任期补写不顶替转出人）；漂移回流任务重放补转。
- **激活守卫**：`ActivateBindingTx` 写事务内核对"同实例无 draining"（未完成交接批次不可绕行激活——检查与写入原子）+ 激活与回执同一事务（不存在"激活成功回执缺失"态）；守卫前绕行数据态由重放分支回退自愈。
- **UI**：设置页实例任职块"换 Agent 交接"（确认警示 + 回执卡：转移明细/知识清单/排除面）；未完成批次态"继续换将交接"续跑入口（原目标收敛）。零 migration。

## 修复（随本版）

- 四轮独立审校 13 项全关闭（一审 4 P1 + 2 P2：在途 Run 停止/并发批次分裂/遗留悬空/接单边界；复审 2 项：激活绕行/补转回执丢失；三审 2 项：守卫原子性/终态补偿；四审 1 项：跨任期归档）——见 [CHANGELOG](https://github.com/sisibeloved/Mosaic/blob/main/CHANGELOG.md#079---2026-11-14)。

---

**产物**：mosaic.exe（Windows/amd64）。数据格式迁移链无新增（v35 保持）；换将全部状态经既有表（slot_bindings/binding_events/missions/mission_events）承载。
