# ADR-0044：0.7.7 导入激活与执行权接管——防止同身份双写

- 状态：Accepted（2026-11-11，随 0.7.7 实施；v0.2–v0.4 三轮审校修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§8.1 跨实例迁移承诺"ID 映射、目标环境校验"；§8.3"目标校验通过并确认源端已撤销执行权后才激活"、"可导入只读待核验副本，或由用户明确按新身份复制；复制生成新 AgentID、记录 `origin_agent_id`，默认不继承活跃承诺、授权与唤醒规则"、"不能靠包内一个'已停机'字段证明外部执行者已经停止"、迁移事务 `preparing → exported → imported_inactive → activated | aborted`、"绑定的全局任务若不在同一库，需显式承接和对象映射"）；[ADR-0043](ADR-0043-bundle-import-precheck.md)（0.7.6 导入预检——blockers[]/verdict 是本件输入契约；§5 激活语义欠账在此兑现）；[ADR-0042](ADR-0042-portable-agent-bundle-export.md)（commitments `default_on_import: "paused"` 的导入侧消费；可移植别名投影——激活按 agent_id 主键重映射）；[ADR-0040](ADR-0040-run-checkpoints.md)/[ADR-0041](ADR-0041-cross-harness-resume.md)（检查点事实与 LatestForResume 选取口径——导入检查点按业务快照参与续办）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.7 唯一交付特性：导入激活与执行权接管——防止同身份双写）。

## 背景与问题

0.7.6 交付了导入预检：包经过安全门与兼容性报告后落为 **inactive 只读副本**（不进库、不建域状态）。迁移链的下一环是把这份副本变成**活的域状态**——Agent 身份、授权记忆、任务承诺与续办检查点真正落到目标实例，并由该 Agent 接管执行权。核心风险即本件标题：**同一 agent_id 在源与目标两实例同时对外写入**（RFC §8.3"源不可达且无法证明已停止时，不能自动以同一身份接手对外写入"）。

激活与删除传播（0.7.8：旧包防复活/墓碑）不在本件。

## 决策

### 1. 两种激活模式（RFC §8.3 两分支的结构化落定）

| 模式 | 前提 | 身份 | 前置确认 | 承诺承接 |
|---|---|---|---|---|
| **takeover（同身份承接）** | 本实例**已存在**同 `agent_id` Agent | 沿用既有 Agent——不建身份行 | **必须** `confirm_source_stopped=true`（人工声明源端已撤销执行权，声明时间入档） | 任务（新 ID）+ 检查点 + 记忆挂到既有 Agent |
| **copy（新身份复制）** | 本实例**无**同 ID Agent | **新 AgentID**（域生成），`source` 记 `bundle:<bundle_id>:<origin_agent_id>`（RFC"记录 origin_agent_id"） | 无须（新身份无双写面） | 同左；承诺一律 **paused**（"默认不继承活跃承诺"——非活跃承接，恢复是用户显式动作） |

- **模式与实例事实必须匹配**：takeover 遇目标无此 Agent → 409（"应使用 copy"）；copy 遇已存在 → 409（"应使用 takeover 并确认源端已停"）。不自动推断——显式模式是用户对"这是承接还是复制"的意图声明，也是防双写的第一道门。
- **`confirm_source_stopped` 是人工声明而非验证**：服务端不验证源端状态（RFC："不能靠包内一个'已停机'字段证明外部执行者已经停止"）——记录声明事实（确认人恒 owner、时间），责任在声明人；这是 takeover 模式的唯一执行权门。
- 复制模式的授权与唤醒不继承：包内本就无凭据/唤醒规则（0.7.5 设计边界），激活产物无任何自动唤醒。

### 2. 激活时实时复核（blockers 是预检快照，不是激活时刻的事实）

激活不信任登记时的 verdict/blocks——按**激活时刻**重跑关键对照（环境可能已变，ADR-0043 一审 #6"激活前须重跑预检"的兑现）：

- `identity_conflict` → 由模式匹配规则消费（见 §1），不再是 block 本身；
- `mission_conflict`（同 ID 任务且版本漂移）/ `harness_availability`（包内可执行引用本实例缺失）→ 任一存在即 **422 `activation_blocked`**（明细透出）；
- 包完整性不重跑（zip + 边车在登记时已过安全门；导入记录若被篡改属文件系统边界外）。

### 3. 写入编排（跨域顺序写入 + origin 标记幂等，无跨域事务）

个人实例无分布式事务——顺序写入，**每项写入以 origin 标记幂等**（重试/部分失败后重激活不产生重复）：

```
1) 身份：takeover → 校验存在即得 agent_id；copy → CreateImported（EnsureAgent 幂等，
   agent_id 记入激活记录供重试复用）
2) 任务承诺（逐条）：origin 标记 = "bundle:<bundle_id>:<origin_mission_id>" 进契约
   SourceRefs（§7.2 可追溯纪律——来源引用语义本就是它）；幂等查重 = 按新 owner 列任务
   后逐 Get 比对 SourceRefs 含标记。新 mission_id（域生成——RFC"显式承接和对象映射"：
   跨库 ID 空间独立，映射记入激活结果）；创建 → 完备契约 Pause("bundle-import:<import_id>")
   （draft 不强制暂停——本就未入调度）；房间来源不映射（目标无原房间——0.7.6
   room_source_unbound 已声明，回传需显式重绑定）
3) 检查点（逐任务至多一条）：checkpoint 域新增导入写入面 CreateImported——保留源
   checkpoint_id（生成式全局唯一，目标无撞）、RunID 保留源 run_id（溯源事实）、
   Source="import"（业务快照类——LatestForResume 优先选取口径不变，续办可达）；
   跳过 run 双锚（目标无源 run——锚定语义由激活编排保证任务先行存在）；
   幂等 = ListOfRun(源 run_id) 已含同 checkpoint_id 即跳过
4) 记忆（逐条）：namespace 原样；agent 域 scope = 目标 agent_id（copy 模式重锚新身份），
   workspace 域 scope=default（共享域合入），mission 域 scope = 映射后的新 mission_id
   （未承接任务——同 ID 冲突跳过——的 mission 记忆跳过并计数）；ID 域生成新 mem_*
   （跨库 ID 空间独立），origin 标记 = "bundle:<bundle_id>:<origin_memory_id>" 进
   SourceRefs；幂等 = 目标 scope List 后比对标记。策展/时效/血缘字段按包内容原样
   （status=active、时效有效性是安全门保证的包内事实）
```

任一步失败 → **中止并保持 inactive**：已写部分不回滚（跨域无事务），但幂等标记使重激活安全；失败明细进错误信息（导入记录不写半激活态——`activated` 只在全程成功后落）。

### 4. 迁移事务状态与记录（RFC `imported_inactive → activated`）

- `ImportView.status`：`inactive | activated`（`aborted` 归 0.7.8 删除传播的生命周期治理）。
- 激活成功后 import.json 边车重写：status=activated + `activation` 块 = 激活结果全文（模式/agent 映射/逐任务 ID 映射/逐类计数 carried·skipped/确认声明时间）——**映射是一等公民**（RFC §8.1"ID 映射"），续办与 0.7.9 交接以它为事实源。
- 幂等：已 activated 的记录再次激活 → **200 返回既有结果**（不重复写入）。

### 5. HTTP 面（单端点，写门三层照旧）

| 端点 | 语义 |
|---|---|
| `POST /v1/bundle-imports/{import_id}/activation` | 激活 → 200 ActivationView（已激活幂等返回）；400 模式不合法/缺确认；404 导入不存在；409 模式与实例事实不匹配（`mode_mismatch`，message 指出可用模式）；422 `activation_blocked`（实时复核 block 项明细）；503 未装配 |

`ActivationView`：import_id/bundle_id、mode、status=activated、activated_at、目标 agent_id + origin、逐任务映射 `{origin_mission_id, local_mission_id, status}`[]、计数 `{memories, missions, checkpoints} × {created, skipped}`、`confirm_source_stopped_at`（takeover）。域视图 JSON 形状即契约形状。

### 6. 域结构（bundle 增激活面 + 两域小改）

- `internal/bundle/activate.go`：`ActivationService`（复用 ImportService 的边车/记录面）+ `ActivationPorts` 全闭包投影（同 0.7.5/0.7.6 依赖方向纪律——bundle 不 import 各域）：

```go
type ActivationPorts struct {
    AgentEnsureImported func(ctx, displayName, adapter, agentID, originRef string) error // copy：幂等建（agentID 可空=生成）
    AgentExists         func(ctx, agentID string) (bool, error)                          // takeover 校验（复用 0.7.6 口径）
    MissionCreate       func(ctx, ownerAgentID string, c ContractFacts, originRef string) (MissionRef, error)
    MissionPause        func(ctx, missionID string, version int, reason string) error
    MissionState        func(ctx, missionID string) (MissionState, bool, error)          // 同 ID 漂移复核（复用）
    MissionsOfOwner     func(ctx, ownerAgentID string) ([]MissionFact, error)            // 幂等查重（SourceRefs 比对）
    CheckpointImport    func(ctx, ckpt CheckpointFact) error                              // 幂等（同 ID 跳过）
    CheckpointsOfRun    func(ctx, runID string) ([]string, error)                        // 已入 checkpoint_id 集
    MemoryCreate        func(ctx, item MemoryFact, scopeID, originRef string) error      // scope 重锚 + 标记
    MemoryList          func(ctx, namespace, scopeID string) ([]MemoryFact, error)       // 幂等查重
    Harnesses           func(ctx) ([]string, error)                                      // 复核（复用 0.7.6 口径）
}
```

- `personalagent` += `CreateImported`（`EnsureAgent` 幂等路径，`source` 记 origin 引用——零 migration）；`checkpoint` += `CreateImported`（保留源 checkpoint_id/run_id、`Source="import"`、免 run 双锚——零 migration：store 表结构不变）。mission/memory 无域改动（现有接口 + SourceRefs 标记）。
- **Slot 任职不恢复**：包内 slots/specs.json 是源实例任职事实（SlotSpec 谱系 ID 跨实例不对应），激活不建 Binding——职责知识转移是 0.7.9（Slot 换 Agent 交接）的题；包内 slots 留在导入记录作参考事实。此为显式边界非欠账遗漏。

### 7. 前端（"导入与预检"面板激活区）

导入历史行（inactive 且 verdict 非 blocked 时）出"激活"操作 → 确认对话框：模式二选一（按实例事实预选——服务端 409 会纠错）、takeover 时须勾选"我确认源实例已撤销该 Agent 的执行权"（醒目警示：同身份双写风险）；激活结果卡：目标 agent_id、逐任务 ID 映射表、计数、（takeover）确认声明时间；已激活记录显示 activated 徽标 + 结果可重开。

### 8. 测试与验证

bundle UT（fake ActivationPorts + 导出→导入→激活全链）：takeover/copy 双模式全语义；模式不匹配 409；未确认 400；实时复核（漂移/依赖缺失 422；预检后环境变化被捕获）；幂等族（重复激活 200 既有结果；部分失败后重激活不重复写——origin 标记跳过计数）；记忆三命名空间重锚（copy 模式 agent 域 scope 新 ID；mission 域随映射；workspace 合入）；检查点 Source=import 落库与跳过；承诺 paused/draft 状态。personalagent/checkpoint 域 UT（CreateImported 幂等/免锚）。httpapi IT：激活端点全语义。真机冒烟：双实例对导——干净实例 copy 激活（新 AgentID/origin 记录/任务 paused/检查点续办可达/记忆重锚）→ 源实例数据目录再导入同包 takeover（确认门/挂既有身份）。

## 后果

- 迁移链第三环闭环：导出（0.7.5）→ 导入预检（0.7.6）→ **激活接管（本件）** → 删除传播（0.7.8 消费激活映射做旧包防复活）。
- 防同身份双写 = 模式显式声明 + takeover 确认门 + 复制模式新 AgentID 三层；服务端不验证源端停止（做不到）——记录人工声明，责任显式化。
- 跨库对象一律新 ID + origin 映射进 SourceRefs 与激活记录（"显式承接和对象映射"，RFC 原文）；任务承诺 paused 承接，恢复执行是用户显式动作（Resume 既有面）。
- 跨域写入无事务：顺序写入 + origin 标记幂等是本件的原子性替代；失败留痕（错误明细）+ 重激活安全。若未来要求强一致，需引入域间事务机制——另行决策。
- checkpoint `Source="import"` 扩展了 0.7.4 的 source 取值域（""/system → +import）——LatestForResume 选取口径不变（业务快照类优先）；ListOfRun 按源 run_id 查询在目标仅对导入条目有意义（溯源），无歧义。
