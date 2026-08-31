# ADR-0011：快照四元组内联正文——对 RFC-0001 快照形态的 M1 偏离登记

- 状态：Accepted（偏离登记，带对齐计划）
- 日期：2026-08-28
- 背景：M1 收口审校（2026-08-28）发现实现偏离 RFC-0001 快照契约且无登记

## 语境

RFC-0001 §快照规定快照为"重建位点 + 投影状态引用"：`room_version` + opaque `event_watermark` + `projection_state_ref` + `visibility_filtered`，**不含事件正文**（正文按 watermark 经历史通道补拉）。

M1 实现（`internal/room/projection.go` + `GET /v1/rooms/{id}/snapshot`）返回：`room_version` + `watermark` + `projection_version`/`algorithm_version` + **内联 Timeline 正文**（message 族事件体），无 `projection_state_ref`、无 `visibility_filtered`。

## 决策

登记为**已批准的 M1 偏离**，理由：

1. 个人版单投影（Timeline v1）阶段，投影状态就是即时重算结果，`projection_state_ref` 指向的持久投影存储尚不存在（引入即假引用）；
2. 房间规模小（M1 出口判据 ≤ 千级事件），内联正文的快照体积可接受，省一次历史通道往返——这也是内嵌最小 UI 校准 `room_version` 的唯一通道（UI 发命令前以快照对版本）；
3. `visibility_filtered` 的前提是多视图可见性过滤（RFC-0001 完全体），M1 只有 public 单视图。

## 对齐计划（M2）

- React SPA 引入持久化投影时补 `projection_state_ref`（投影存储 + 失效策略）；
- 参与者/系统视图分离时补 `visibility_filtered`；
- 快照正文内联改为可选（`include_bodies` 参数），默认走 watermark + 历史通道；
- 届时 `projection_version` 递增，旧快照可识别过期（机制已就位）。

## 后果

- 对外契约与 RFC-0001 全量形态不一致——本 ADR 是唯一的偏离登记点，交付计划 v1.5 同步引用；
- `algorithm_version`/`projection_version` 双版本位已就位，对齐时无需破坏 envelope 层。
- 2026-08-31 注记（UI 重设计切片 1）：快照新增 `display_name`（room.created/room.renamed 投影产物）与 `participants`（装配层注入——本地 owner + 引擎座位快照，**非投影产物**；不进 room_version/水位语义，回放一致性不受影响，`projection_version` 不递增）。
