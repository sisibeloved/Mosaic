# Mosaic v0.6.13 — 0.6 域收口：网关续接、Slot 职责寻址与墙钟强制（0.6 Mission 与本地执行领域·第十四件·域收口收官）

v2.69 阶段性验收盘点确认 0.6 出口既定范围（AT-02/03/10/13）全部兑现，但网关分步（AT-14/15/18）存在三项出口承诺与 0.6.1 首切片之间的落差：任务卡追加要求没有受控入口（§10.3 `mission_update`）；dispatch 目标面没有 slot 职责寻址（§4.5）；budget 五项只是契约声明、无运行时强制（§6.1 / AT-18）。本件一次收口三项，**0.6 域出口闭环**。十二轮独立审校全部关闭后定版。

## 新增

- **网关续接 `POST /v1/dispatch/update`**（= mission_update，§10.3）：幂等键先行（同键同载荷重放原回执 / 同键异载荷 409 `idempotency_conflict` / pending 恢复重驱动采纳原行）→ 服务端复核（任务在册 + owner active——续接不发往已停身份）→ `revise`（CAS + 归属锚由网关以服务端读值填充，**owner 不变**——换人走 0.6.12 交接）→ 回执（`domain_command_id = mission:<id>:revise:v<新版本>`，`reason` 持久化）；领域拒绝原样透传（网关不吞并）。SQLite 上**认领 + 复核 + 修订 + 回执单写事务**（`UpdateMissionAtomic`——修订已生效而回执失败不留"同键重试 409"裂口；拒绝整体回滚不占幂等键）。
- **Slot 职责寻址**（§4.5）：resolve 请求 += `slot_instance_id`（显式实例）或 `slot_spec_id`（模板——scope 内无实例 blocked `slot_no_instance` / 多实例歧义 blocked `slot_ambiguous` 禁止猜选 / 恰一实例继续）；寻址链 scope → SlotInstance → **active Binding** → AgentID（无 active 任职 blocked `slot_unassigned`——**不自动激活 ready Binding**）；解析结果写入决策存档 owner（`selection_source=slot_binding`）+ `resolved_slot_instance_id` / `resolved_agent_id`；**投递复核**：决策后任职换人 → 409 `route_stale` 重新解析（§4.6"不能只改路由缓存"）；与显式 `owner_agent_id` 互斥（并存 400）。
- **建单事务门 `CreationGate`**：显式 owner / slot 交办在建单**同一写事务**内核对五项——身份（owner 在册且 active）、任职（active Binding == 解析预期）、授权准入（workspace_access 完整快照）、归属、模板唯一性锚（SpecID——长出兄弟实例即歧义拒建）；漂移即 409 `route_stale`，不落入新承诺。
- **墙钟强制**（§6.1 / AT-18）：Run 建行事务内按执行清单钉住版本的 `max_wall_clock_seconds` 快照 `deadline`（migration v30：runs += deadline；修订不追改在途）；扫描过期走既有停止面（run canceled + `cause=wall_clock_exceeded` + Mission 回 ready，迟到结果只审计 ≤400 字 + 世代）；墙钟监控独立 ticker 按 RunID 直达——不被同步执行器阻塞。
- **max_runs 派发上限**：建行事务内计数核对——达上限 409 `budget_exhausted`（手动入口同步报错），任务停留 ready 不被静默吞（`budget_exhausted_hits` 诊断计数）。
- **执行全门收口**：`CreateRun` 建行事务内核对执行者身份门 + 任职门 + 归属 + 授权指纹（`GrantFingerprint`——执行清单重放以本次实际授权回填）+ 契约版本漂移（409 `contract_drift`）+ 现行预算；**交付时点授权复核**（goroutine 首步 + supervisor Boot 后 `PreRun` 双点——Boot 期间/建行后撤权即阻止交付，复核闭包用独立 runCtx 不受客户端断开影响）；**会话按 Run 隔离**（`mrun:<run_id>`——撤权重装配不复用旧线程）。
- **停止体系收口**：暂停/取消**同一事务**为在途 Run 写持久停止标记（NOT EXISTS 去重恰一条 `stop_requested`）；终态写锁后三段裁决（deadline → Run 持久停止标记 → Mission 状态动作码——人工暂停并发完成不再发布正文）；恢复面兑现（paused/canceling/标记分支统一动作码 + recovery 标记、读写错误如实上抛不误判 unknown、写失败保留 running 可重试）。
- **认领查询与释放**：`GET /v1/dispatch/claims/{key}`（owner token 校验——鉴权后可见 pending 只读面）+ `POST /v1/dispatch/claims/{key}/release`（五态：released / finalized / not_found / digest_mismatch / not_releasable）；v30 遗留 pending 续接认领可处置（`legacy_pending_unresolved` 409 + 指引：GET claims 取摘要 → 确认未执行 → release → 更正载荷重提）。
- **SPA 任务卡续接**：已交办任务卡在 MissionsPage 切"追加要求"模式（`room_source` 命中映射即续接，不再撞 createMission 409）——目标/版本/草稿原子生效、代次作废在途响应、版本冲突显式重载；409 冻结合并（字段级任务卡归属 + 提交时快照 + 按卡保留待合并记录——跨卡污染与丢草稿双向关闭）。
- **契约**（OpenAPI + 两端生成物）：`DispatchUpdateRequest` / `DispatchClaimView` / `DispatchClaimReleaseRequest` / `DispatchReceiptView.reason` / `RouteDecisionView.selection_source` 枚举与 `resolved_*` / `MissionSummary.room_source`（含墓碑）/ runs 409 说明补 `budget_exhausted` / `grant_drift` / `contract_drift` / `owner_not_active`。

## 修复（十二轮独立审校）

- **建单与寻址**：执行装配按实例固定 Binding（同人双任职不串用）；换绑后重解析同键重放原回执（原请求摘要稳定 + resolved 事实另存）；create 拒绝不释放认领（并发孪生共享）；回执 upsert 自愈换绑拒绝后同键重投。
- **执行与预算**：预算取清单钉住版本（RefOf 权威快照——修订收紧/放宽即时生效）；手动执行达限同步 409；期限比较改时刻（RFC3339Nano 字典序漏过期行）；停止信号零持久写并发不拖尾；终写失败如实报错（`ErrTerminalWrite`——Run 滞留 running 由恢复面收敛）。
- **前端草稿**：字段级任务卡归属（每契约/预算字段记最后编辑所在卡——换卡隔离）；未绑定草稿提交时按最终来源接纳（空键/部分键/逐字中间键=提交键输入前缀态）；创建请求发送前冻结提交时快照（等待期间切卡编辑不改写合并）；待合并冻结按任务卡 Map 保留（其它卡详情加载不清——回到原卡才消费）。

## 升级说明

- v0.6.12 数据库首次启动自动执行 migration v30/v31（runs += `deadline`；missions += `slot_instance_id`、dispatch_decisions += `resolved_agent_id`/`resolved_instance_id`、dispatch_receipts += `reason`——幂等加列），无需手工步骤。
- 运行过候选版（b59a395–ce80848）的库升级后，遗留 pending 续接认领按 `legacy_pending_unresolved` 指引处置（GET claims → 确认未执行 → release → 重提）；正式版首装无此面。
- 无新增必填请求字段（新端点自带幂等键/CAS）；`dispatch/resolve` 的 slot 引用与 `owner_agent_id` 互斥。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§4.5/§4.6/§6.1/§10.3，AT-14/15/18）与 [ADR-0036](docs/design/adr/ADR-0036-dispatch-update-slot-addressing-wallclock.md)（v1.3，含十二轮审校修订记录）。
- 下一领域：0.7 记忆与可移植恢复（0.7.0 起十件）。
