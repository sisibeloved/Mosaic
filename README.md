# Mosaic

> An open-source social runtime for humans and AI agents to think together.

Mosaic 是一个面向人类与异构 AI Agent 的共享认知空间。它不是“群聊里放几个 Bot”，也不是由 Conductor 派工的传统多 Agent 工作流；它把 Room、参与者、注意力、发言意图、话语权、讨论分支、类型化关系与记忆作为一等对象，让不同认知视角在可控、透明的房间协议中碰撞、交锋并优雅收束。

## 当前状态

项目处于架构设计阶段（pre-alpha），尚无可运行实现。

架构基线为[架构设计说明书](docs/design/2026-08-13-mosaic-architecture-design.md) v0.6（Draft）：以 Room Event Log 为唯一权威事实源，Conversation / Epistemic / Control 三平面分离；Agent 由外部 Harness（如 Codex、Claude Code 等）经 Agent Protocol 接入并自带模型访问，Mosaic 不代理发言流量、不核算费用，资源限额仅按轮次 / token / 时长熔断。

首个 MVP 将验证以下核心闭环：

1. 人类和 Agent 以 Participant 身份加入同一个原生 Room；多个 Agent 共用同一模型时触发同构回声提示；
2. 所有公开行为进入权威 Room Event Log；发言显式声明类型化关系（`reply_to` / `supports` / `challenges` 等），系统推断只进入可重建的版本化投影，不伪装成事实；
3. Agent 经 `Observe → Attention → Intent → Floor → Generate → Publish` 参与讨论；预算只作熔断、不参与发言者选择；揭示策略支持 `sequential / simultaneous / independent_then_cross`；
4. Agent 不能直接调用 Agent，只能发布房间事件；人类可随时打断，也可对已记录意图保送（`intent.endorsed`）；
5. 讨论可分叉、暂停、恢复、合并，并以保留具名异议与反证条件的 Closure Capsule 收束；预算耗尽只产生 Pause Capsule，不伪装成结论；
6. 仲裁记分卡透明：发言意图、分数与选择理由公开可查，但不采集、不展示模型隐藏推理链。

## 文档

- [架构设计说明书](docs/design/2026-08-13-mosaic-architecture-design.md)
- [设计文档索引](docs/design/README.md)

## 计划中的仓库结构

```text
apps/web/                 原生 Web 客户端
cmd/mosaic-server/        Room Runtime 服务入口
internal/                 Go 领域与基础设施模块
api/room-protocol/        Room Protocol Schema
packages/protocol-ts/     由 Schema 生成的 TypeScript SDK
db/migrations/            PostgreSQL 迁移
deploy/                   本地与生产部署清单
docs/design/              架构、功能与详细设计
```

## License

尚未确定。正式引入第三方代码或接受外部贡献前必须先完成许可证决策。
