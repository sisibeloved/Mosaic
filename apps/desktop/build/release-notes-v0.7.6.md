# Release v0.7.6 — Bundle 导入预检：安全校验、兼容性报告及默认 inactive（0.7 记忆与可移植恢复领域·第七件）

迁移包（0.7.5 导出）在目标实例上传接收：先过结构化安全门，再出逐项兼容性报告，登记为 inactive 只读副本——激活与执行权接管归后续版本（0.7.7）。

## 新增

- **安全校验**（`POST /v1/bundle-imports`，multipart 上传；RFC"导入先检查"六项，任一违规 422 整体拒绝、不留记录）：格式（manifest 严格解码拒收未知字段、仅同版本 `mosaic-agent-bundle/1`）、大小与压缩展开上限（压缩体 64MiB / 展开总量 256MiB——声明量与实际读取双向计量）、路径穿越、符号链接条目、九文件精确布局（未知/缺少/重复拒绝）、引用闭包（检查点/上下文/水位/依赖清单/任务域记忆跨文件核验——断链包拒绝）。
- **兼容性报告**（分级 findings，非阻断）：安全门通过摘要 + 对照本实例事实——身份冲突（同 ID Agent 已存在 → block，同身份双写防线起点）、任务冲突（契约版本漂移 block / 同版重复 warn）、依赖可用性（包内可执行引用本实例缺失 → block；凭据永不随包）、途中时效过期 / 敏感携带 / 恢复限制 warn；verdict ∈ eligible / eligible_with_warnings / blocked，`blockers[]` 为 0.7.7 激活的输入契约。
- **默认 inactive**（RFC §8.3）：导入产物 = 待核验只读副本（数据目录 `bundle-imports/<bdi>/`——zip + 报告边车，零 DB migration）；不进库、不建 Agent/任务/记忆行、不执行包内代码、不启用 MCP/工具/自动唤醒；同源 bundle_id 重复导入 409。
- **查面与 UI**：`GET /v1/bundle-imports`（列表）/ `GET /v1/bundle-imports/{import_id}`（详情）；设置页"迁移包"新增"导入与预检"面板——verdict 徽标、检查表（check·result·detail）、blockers、脱敏报告透传、导入历史与"查看报告"重开。

## 安全边界（两轮独立审校钉住）

- NDJSON 行计数/解析扫描式零分配（高压缩空行包不得放大内存绕行数门，AllocsPerRun=0 钉住）；严格 JSON 解码以二次 Decode==io.EOF 判尾随（`]{...}` 类非法注入拒绝）。
- 依赖清单与身份可执行项一致性核验（清空 dependencies.harness 不得绕过依赖对照）；敏感判定以包内实际载荷为准（与 scope 声明不一致时明示）。
- 并发同源导入互斥（恰一成功一 409）；必要对照（身份/依赖）查询失败 = 必要证据缺失 → block 并注明重跑预检。

## 修复（随本版）

- 一审 1 P1 + 6 P2、二审 3 P2 全关闭（含 0.7.5 存量：受保护 GET 不带令牌四处）——见 [CHANGELOG](https://github.com/sisibeloved/Mosaic/blob/main/CHANGELOG.md#076---2026-11-11)。

---

**产物**：mosaic.exe（Windows/amd64）。数据格式迁移链无新增（v35 保持）；导入记录在库外文件系统。
