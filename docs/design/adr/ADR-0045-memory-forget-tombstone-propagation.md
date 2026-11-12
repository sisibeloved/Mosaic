# ADR-0045：0.7.8 派生删除与旧包防复活——墓碑、删除传播与影响报告

- 状态：Accepted（2026-11-12，随 0.7.8 实施；v0.2–v0.3 两轮审校修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§11.4"隐私删除可沿 derived_from/source_refs 追踪清除摘要、记忆正文、索引、检查点副本及托管导出包；审计仅保留不含正文的墓碑"、"无法保证清除的副本与用户手工下载的包须在删除报告中如实列出，不能宣称全域擦除"、**"删除过的数据不能因旧包导入静默复活，导入须核对已知来源的删除代次，无法核实时作为待确认数据处理"**；§8.2 memory/items.ndjson"删除标记"位；AT-06"记忆纠错/删除后导出、恢复或导入旧包——最新修订与墓碑生效；派生内容可追踪；受管理副本不复活已删正文"）；[ADR-0038](ADR-0038-memory-curation-timeliness-superseded.md)（EditRecord 留痕机制——delete 留痕复用）；[ADR-0039](ADR-0039-memory-recall-fts-authz.md)（FTS5 派生索引 v33——迁移注释预留"Forget 归 0.7.8"；AU 触发器随行同步）；[ADR-0042](ADR-0042-portable-agent-bundle-export.md)/[ADR-0043](ADR-0043-bundle-import-precheck.md)（删除端点欠账两处——"0.7.8 统一治理包生命周期"）；[ADR-0044](ADR-0044-bundle-activation.md)（激活 origin 标记——防复活的第二把键）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.8 唯一交付特性：派生删除与旧包防复活——墓碑、删除传播与影响报告）。

## 背景与问题

0.7 线已交付八件：记忆可策展、可召回、可打包、可导入、可激活——但**删不掉**。0.7.1 的策展只有替代/否决（内容仍在库）；0.7.5/0.7.6 的包生命周期两处删除端点显式欠账；而 AT-06 的验收靶（"删除后导出/导入旧包不复活"）没有删除面就无从谈起。RFC §11.4 给出三原则：**隐私删除沿血缘追踪清除**（正文/索引/托管副本）、**审计只留不含正文的墓碑**、**删除报告如实列出无法清除的副本**（不宣称全域擦除）。

本件交付：**记忆 Forget（隐私删除）** 为入口——墓碑化 + 删除传播 + 影响报告；**旧包防复活**（预检报告 + 激活阻断）；**包生命周期删除端点**（两处欠账兑现）。

## 决策

### 1. Forget：墓碑化而非删行（v33 预留的落定方式）

`memory.Service.Forget(ctx, memoryID, note)`：

- **墓碑行**（同表原地，零 migration）：`status="deleted"`、**Title/Content 置空**（"审计仅保留不含正文的墓碑"）、Version+1、EditRecord 追加 `cause:"delete"`（0.7.1 留痕机制复用——**delete 留痕即删除代次事实源**，Version 单调）；保留：ID/namespace/scope/kind/source_refs（派生追踪依据）/sensitivity/时间戳/历史留痕。
- **索引同步零新增**：不删行 → v33 的 `memory_fts_au` AFTER UPDATE 触发器随正文置空同步清 FTS 内容（trigram 无 token 可命中）；召回另有 status=active 过滤（0.7.2 口径）——**双保险结构性不可达**。v33 注释预留的 AD 触发器不需要（那是删行路线的预留；墓碑行路线更符合 RFC"保留审计"）。
- **幂等**：已 deleted 再 Forget → 返回当前墓碑（no-op，Version 不再递增）。任意非墓碑态（active/proposed/superseded）皆可 Forget——superseded 条目同样承载隐私内容。
- **不跨条目级联**：删除传播的对象是**该条目的托管副本**（本库行/FTS/导出包），不是"同 source_refs 的其他条目"——跨条目自动级联在没有显式 `derived_from` 字段（0.7.0 预留未启用）支撑下误删风险不可控，显式不做（边界登记，血缘字段启用后递进）。

### 2. 删除传播与影响报告（DeletionReport——不宣称全域擦除）

Forget 的编排层（app）组合三方事实返回报告：

```json
{ "memory_id": "mem_…", "namespace": "agent", "scope_id": "ag_…", "deleted_at": "…",
  "cleared": { "content": true, "index": true },                    // 本库正文 + FTS（结构性不可达）
  "contained_in_bundles": ["bdl_…"],                                 // 托管导出包内仍有正文——zip 是定版快照，不重写（破坏 manifest 摘要完整性）；如实列出，可经包删除端点处置
  "out_of_reach": ["已在他实例激活的副本无法召回（包已离开本实例）"] }   // RFC 原文口径
```

- **托管包扫描**：`bundle.Service.ExportsContaining(memoryID)`——解开 `bundles/*/` 各 zip 的 `memory/items.ndjson` 逐行比对（个人实例包量小；导出边车无逐条清单——不加契约字段）。
- **检查点副本**：被删记忆的正文不会进检查点（检查点存进度/工件引用非记忆正文）——本面无动作；影响报告不虚列。

### 3. 旧包防复活（两把键 × 两道门）

**两把墓碑键**：包内**原 memory_id**（同库导出→删除→旧包回导）与**激活 origin 标记** `bundle:<bdl>:<origin_mem_id>`（他实例激活产物在本实例删除→旧包再导入激活）——端口 `MemoryTombstoneHits(ctx, keys []string) ([]string, error)` 返回命中子集。

| 门 | 行为 |
|---|---|
| **预检（0.7.6）** | 新 finding `tombstone_hits`：包内记忆命中墓碑 → **warn**（列 ID——"激活时将拒绝复活"；RFC"无法核实时作为待确认数据处理"的可视化：可核实即拒绝，报告先亮出） |
| **激活（0.7.7）** | MemoryCreate 前查两把键：命中 → **跳过该条**（MemoriesSkipped 计数——"受管理副本不复活已删正文"）；未命中正常写入 |

同 ID 由 `mem_` 生成式唯一保证不会自然重建——墓碑存在即拒；多轮删除的代次事实在留痕链（Version/EditRecord）中可审计，v1 不做代次数值比对（无对端代次可比——包内不携删除标记；ADR 边界）。

### 4. 包生命周期删除端点（0.7.5/0.7.6 两处欠账兑现）

| 端点 | 语义 |
|---|---|
| `DELETE /v1/bundle-exports/{bundle_id}` | 删数据目录 `bundles/<id>/`（zip + 边车）→ 200 视图（deleted 文件清单 + 不可达注记："已传输/已在他实例激活的副本无法召回"）；不存在 404；id 白名单防穿越 |
| `DELETE /v1/bundle-imports/{import_id}` | **inactive** → 删记录（zip/边车/attempt）→ 200；**activated → 409**（激活产物是独立域状态——撤销激活是"反向传播"大题，不在本件；显式拒绝而非级联猜测） |

### 5. 域结构（零 migration；两域小增量 + bundle 编排）

- memory：`Service.Forget`（UpdateItem CAS 双基准复用——零 Store 新方法）；读取面墓碑可见（Get 返回墓碑行；List 默认口径 status=active 已排除——0.7.1 时效过滤同沿）。
- bundle：`Service.DeleteExport` / `ImportService.Delete` / `Service.ExportsContaining`；预检 ports += `MemoryTombstoneHits`（兼容性检查增 tombstone_hits finding）；激活 ports += 同名端口（MemoryCreate 前核对）。
- httpapi：`POST /v1/memory/items/{memory_id}/forget`（动作端点风格同 accept/reject，体 `{note}`）；两个 DELETE 包端点。
- 前端：MemoryPage 条目操作增"遗忘"（确认弹窗 + 影响报告展示——托管包清单醒目）；迁移包面板导出/导入历史行增"删除"（导入已激活行禁用并注明）。

### 6. 测试与验证（TDD——测试先行）

**红段先写**（本件流程约定：设计 → 测试红 → 实现绿）：

- memory UT：Forget 全语义（active/proposed/superseded 三态可删；正文清空 + 留痕 cause=delete + Version+1；幂等 no-op；title/content 空而 source_refs/sensitivity 保留）；已删条目 List 默认口径不可见、Get 可见墓碑。
- bundle UT：ExportsContaining（含/不含包扫描）；DeleteExport（目录删除 + 404 + 白名单）；ImportService.Delete（inactive 删除含 attempt 清理 / activated 409）；预检 tombstone_hits finding（包内命中墓碑 warn）；激活防复活（两把键命中跳过计数、未命中正常写）。
- sqlite it：Forget 后 FTS 召回不命中（正文空 + status 过滤双保险——AU 触发器同步实测）。
- httpapi IT：forget 端点全语义（200 报告/404/401/503）；DELETE 两端点（200/404/409/503）。
- 真机冒烟：造记忆 → 导出包 → Forget（报告列托管包）→ 旧包上传导入（预检 tombstone_hits warn）→ 激活（该条跳过不复活）→ 删除导出包（目录清）→ 删除未激活导入记录 → 已激活导入 409。

## 后果

- AT-06 落地：删除有墓碑、传播有报告、旧包不复活；0.7 领域出口（AT-04/05/06）随本件齐面。
- 墓碑行不删：存储不回收（隐私删除≠空间回收——审计保留是显式选择；空间回收另行决策）。
- 跨条目派生级联（同 source_refs 簇）与"删除代次数值比对"是显式欠账（derived_from 字段启用后递进）；他实例激活副本不可召回（物理边界，报告如实列出）。
- 撤销激活（反向传播：删激活产物）不在本件——activated 导入记录 409 显式拒绝，待后续版本裁定。
- 包删除是文件系统操作（zip/边车/attempt 原子目录删除）——不进 SQLite，零 migration。
