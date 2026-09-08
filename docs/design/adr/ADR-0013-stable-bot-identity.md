# ADR-0013：稳定 Bot 身份与会话寻址（M4-4）

- 状态：Accepted（2026-09-07，随 M4-4 实施）
- 关联：RFC-0002（Agent 协议，附录"路由与稳定身份"）、ADR-0009（个人本地身份）、ADR-0012（多实例发现与家族优先级）

## 背景与问题

M4-4 之前，agent 座位的 ParticipantID 从可执行文件路径派生：
`par_<adapter>_<sanitize(exe.ID)>`，其中 `exe.ID = adapter@runtime[:distro]:path`。这带来四类真实故障：

1. **身份随路径消亡**。nvm 升级 node、CLI 换安装位置、native↔WSL 切换都会改路径
   → exe.ID 变 → 新 ParticipantID。旧房间名册（participant.admitted 事件里的 PID）、
   tasklist 的 owner、run 记录的 assignee 全部悬空；kimi/mcode 的会话续接句柄
   （按 ProfileID 键）断线。用户视角是"Agent 失忆/换人"，实质是身份从未存在。
2. **注册表幽灵项**。Scan 只 upsert 不清理：路径消失的旧项永久留驻，若仍 enabled
   则座位继续挂死路径（执行期才失败）。
3. **会话被周期性驱逐**。装配层每 10s resync 调 `RegisterFor`，而 RegisterFor 无条件
   关闭既有会话——kimi/mcode 的会话对象（含续接 session_id）每 10 秒被清一次，
   跨任务连续性名存实亡（M4-4 实施中实证）。
4. **会话跨房间串线**。Supervisor 会话按 ProfileID 单键——同一 kimi 座位在多个房间
   共用一条 CLI 线程，群聊与私聊上下文互相泄漏。

## 决策

### 1. 稳定 Bot 身份（注册表层）

- `Executable` 增 `BotID`（持久字段）。**首次发现时赋值 `BotID = exe.ID`**（确定性），
  此后 BotID 是 durable 键，不随 ID（路径）变化。
- **祖父化零迁移**：既有 `harness-registry.json` 条目缺 BotID 的，装载时补
  `BotID = ID`。由于 PID 派生式 `par_<adapter>_<sanitize(BotID)>` 与旧式
  `par_<adapter>_<sanitize(exe.ID)>` 在无重绑发生时逐字相等，存量房间/任务/run
  引用无需任何数据迁移。
- 座位派生改用 BotID：`profileID = prof_<adapter>_<sanitize(BotID)>`、
  `participantID = par_<adapter>_<sanitize(BotID)>`。

### 2. 消失重绑（rebind）规则

Scan 后对"本次未发现"的 auto 项执行：

- **确认消亡才重绑**：`runner.Exists(旧路径) == false` 且（旧项 native，或旧 distro
  当前可达）——WSL 发行版整体下线时无法区分"暂时查不到"与"确实没了"，保守不绑。
- **单候选**：同 adapter 的本次新发现项恰一个时，把旧项的 BotID、Model/EvalModel/
  ReasoningEffort 迁给新项；Enabled 仅在新项 logged_in 时迁移（登录硬门不因重绑
  破例）；旧项移除。多候选（歧义）不绑——用户手动选择，各得新身份。
- **手动登记项永不自动重绑**（用户显式管理的登记面）。
- 新旧路径并存（多实例）不绑——ADR-0012 的 C 轨多实例语义不变。

重绑 = "同一 Bot 换了运行实例"：身份延续（房间/任务/run 归属不断），但**会话不
复用**（见 §3）——CLI 线程句柄属于具体实例的运行历史。

### 3. 会话键与注册纪律（Supervisor/装配层）

- **会话按（ProfileID, RoomID）独立映射**：`sessions[profileKey+"\x00"+roomID]`。
  群聊与私聊、不同房间各走各的 CLI 线程——上下文不串。代价是会话数 = 活跃
  (profile × room)，本地单用户规模可忽略。
- **签名变更才重注册**：装配层为每个 exe 维护注册签名（路径+模型覆盖+思考强度+
  评估降档）；resync 时签名不变则不调 `RegisterFor`——修复"每 10s 驱逐会话"。
  签名变化（升级换路径/改模型）→ RegisterFor → 既有会话关闭（实例替换不复用
  旧会话，语义由 Supervisor 既有驱逐行为承担）。
- 进程重启后会话映射为空 → 按需 Boot 新会话（不误续旧线程）；重连/重开页面不
  触碰会话（会话生命周期只挂在任务提交上）。

### 4. 显式路由契约

- 调用（run_task）可携 `connection` 字段（缺省 `local`）；连接注册表现在只有
  `local`。未知连接 → 显式 `invalid_command`（"连接不可达"），**不回落**到任何
  当前可用/选中的连接。远端连接是 RFC-0002 扩展的演进项，寻址字段先行定形。

## 后果

- 存量数据零迁移；重装 CLI（同 adapter 单实例）身份延续；换机/多实例按 ADR-0012。
- "删除房间后同名重建"得到新 room_id、新事件流——旧内容与记忆不复活（房间
  记忆本就 room-scoped）；"重绑"不适用于此场景。
- 会话数增长与 per-room CLI 线程成本：kimi/mcode 每房间一线程；上下文组装仍以
  Mosaic 事件流为准（CLI 线程只是各房间自身的近因缓存）。
- 双连接（远端）契约验证以受控 fake 完成（UT），产品面在 RFC-0002 扩展定稿前
  不展示远端能力（能力展示与实际可用性一致）。
