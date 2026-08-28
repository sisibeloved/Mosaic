# Mosaic 设计文档

| 日期 | 文档 | 状态 |
|---|---|---|
| 2026-08-13 | [Mosaic 架构设计说明书](2026-08-13-mosaic-architecture-design.md) | Draft |
| 2026-08-25 | [RFC-0001 Room Protocol——事件信封、命令语义与协议演进](rfc/2026-08-25-rfc-0001-room-protocol.md) | Approved |
| 2026-08-25 | [RFC-0002 Agent Protocol——外部 Harness 接入](rfc/2026-08-25-rfc-0002-agent-protocol.md) | Approved |
| 2026-08-25 | [RFC-0003 Attention 与 Floor 仲裁](rfc/2026-08-25-rfc-0003-attention-floor.md) | Draft |
| 2026-08-25 | [RFC-0004 Thread 生命周期与 Conversation Graph](rfc/2026-08-25-rfc-0004-thread-graph.md) | Draft |
| 2026-08-25 | [RFC-0005 收束协议与 Closure/Pause Capsule](rfc/2026-08-25-rfc-0005-closure-capsule.md) | Draft |
| 2026-08-25 | [RFC-0006 Epistemic / Structure Projection 契约](rfc/2026-08-25-rfc-0006-epistemic-projection.md) | Draft |
| 2026-08-25 | [RFC-0007 Context 组装与 Memory](rfc/2026-08-25-rfc-0007-context-memory.md) | Draft |
| 2026-08-25 | [RFC-0008 身份、多租户与授权](rfc/2026-08-25-rfc-0008-identity-authz.md) | Draft |
| 2026-08-25 | [RFC-0009 工具与 Artifact 治理](rfc/2026-08-25-rfc-0009-tool-artifact.md) | Draft |
| 2026-08-25 | [RFC-0010 数据生命周期](rfc/2026-08-25-rfc-0010-data-lifecycle.md) | Draft |
| 2026-08-25 | [RFC-0011 评测框架与反 Goodhart 治理](rfc/2026-08-25-rfc-0011-evaluation.md) | Draft |
| 2026-08-25 | [Harness 调研报告](research/2026-08-25-harness-survey.md) | Done |

RFC 系列存放于 `rfc/` 目录，状态取 Draft / Reviewing / Approved / Rejected / Superseded；每份 RFC 吸收架构说明书 11.5 中的对应开放问题，Accepted 后回填架构文档与 `api/room-protocol` 工程。

实现与工具选型决策记录（ADR）存放于 [adr/](adr/) 目录，索引见 [adr/README.md](adr/README.md)；架构级约束归架构设计说明书，协议契约归 RFC，实现选型归 ADR。

后续文档按 `YYYY-MM-DD-<topic>-<type>.md` 命名，并在本索引中登记。架构决策变化时，同步更新文档修订记录。
