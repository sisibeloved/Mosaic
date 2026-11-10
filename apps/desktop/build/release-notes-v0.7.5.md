# Release v0.7.5 — Portable Agent Bundle 导出：范围选择、摘要与脱敏报告（0.7 记忆与可移植恢复领域·第六件）

Agent 连同其授权记忆、任务承诺与续办检查点打成版本化便携 zip（RFC-0018 §8）——跨实例迁移链首环落地，导入预检（0.7.6）随后续版本交付。

## 新增

- **范围选择**：`POST /v1/agents/{agent_id}/bundle-exports`——记忆命名空间 ⊆ `agent|workspace|mission`（缺省 agent+workspace；room/slot 授权口径未建暂不开放）；任务口径缺省自动圈定全部在办任务，可显式逐条选择（非所属整体拒绝）或显式不携带；敏感记忆默认不随包（`include_sensitive` 显式携带）。
- **摘要与水位**：包内 `manifest.json` 逐文件 sha256 + 任务级契约/检查点版本水位（导入预检判陈旧的依据）；来源实例只记产品版本（最小事实面——不记主机/用户）。
- **脱敏报告**（包内 `redaction-report.json` 同构内联响应）：敏感省略（只 ID 不内容/标题）、口径排除计数（时效过期/策展未决/已终结）、逐任务恢复限制、`refs_only` 工件策略与设计边界说明（凭据/令牌/原生会话材料永不随包）。
- **包布局 v1**（§8.2，zip 内相对路径）：agent/profile、slots/specs、memory/items.ndjson、work/checkpoints.ndjson、work/commitments.json（导入默认暂停）、context/manifests.ndjson、dependencies.json、redaction-report.json、manifest.json。产物落数据目录 `bundles/`（零 DB migration）。
- **设置页"迁移包"面板**：Agent 选择、命名空间勾选、任务口径、敏感开关、脱敏报告内联展示与导出历史。

## 安全边界（两轮独立审校钉住）

- 未授权 scope 结构性不查询（0.7.2 过滤先前纪律的导出侧应用）；任务交接后的归属竞态双层防线（自动圈定跳过/显式拒绝）。
- 身份标识全链脱敏：可执行项与身份别名均投影为不含安装路径的可移植标识（`adapter@runtime[:distro]`——本机用户名/目录不随包）；整包断言钉住。

## 修复（随本版）

- 一审 5 项 P2 + 二审 2 项 P2 全关闭（面板切档重置/代次校验/任务口径解耦/路径脱敏/交接归属复核）——见 [CHANGELOG](https://github.com/sisibeloved/Mosaic/blob/main/CHANGELOG.md#075---2026-11-09)。

---

**产物**：mosaic.exe（Windows/amd64）。数据格式迁移链无新增（v35 保持）；导出记录在库外文件系统。
