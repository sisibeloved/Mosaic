# ADR-0039：0.7.2 记忆召回——FTS5 与授权过滤先行（0.7.2）

- 状态：Accepted（2026-11-05，随 0.7.2 实施；v0.2–v0.3 两轮审校修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§7.3——"授权及有效期过滤**先于**排序、摘要与计数，避免泄漏其他域的内容和存在性"；§7.4——"首版保留 SQLite + FTS5、结构化 scope / 时间 / 来源过滤及按需查询"）；[ADR-0037](ADR-0037-domained-memory-items.md)（0.7.0 基石）；[ADR-0038](ADR-0038-memory-curation-timeliness-superseded.md)（0.7.1 策展——active + 未过期口径即召回前置纪律）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.2 唯一交付特性：授权过滤与 FTS5 召回——先过滤权限，再排序和计数）。

## 背景与问题

0.7.0/0.7.1 交付了分域记忆与策展，但**没有召回**：找记忆只能按 namespace/scope 翻列表。RFC §7.4 明确首版技术形态是 SQLite + FTS5；§7.3 给出召回的铁律——授权与有效期过滤必须先于排序与计数（否则 FTS 全库搜索会泄漏其他域的**内容与存在性**——查询词在别域命中与否本身是信息）。

## 决策

### 1. FTS5 trigram 索引（migration v33）

沿用 room_fts/doc_fts 先例（v1.46 spike 实证）：**trigram tokenizer**（unicode61 对中文整串成单 token 不可用；trigram 对 CJK 子串 ≥3 字与英文均正确命中）。

- 表：`memory_fts(memory_id UNINDEXED, title, content, tokenize='trigram')`。
- **触发器同步**（AI/AU 两触发器，同事务自动维护——比应用层双写可靠，策展修订/替代路径零改动）：`memory_items` INSERT/UPDATE 后同步写 FTS 行。本件无删除面（Forget 归 0.7.8）。
- v33 存量回填：`DELETE FROM memory_fts` 全清后 `INSERT … SELECT`（幂等重放安全）。
- 查询注入防护：MATCH 词以 phrase 引号包裹（`"` 转义 `""`——room 先例同法）。

### 2. 召回入口与"过滤先行"语义

`GET /v1/memory/items` += 可选 `q`（trim 非空 = 召回模式）：

- **授权过滤**：namespace 必填（既有纪律——**召回永不跨域**）；scope/kind/status 可选过滤与 FTS `MATCH` 在**同一 WHERE**（SQL 执行上过滤先于排序——其他域即使有命中词的条目，既不出现在结果也不计入 total）。
- **时效口径**：0.7.1 的默认排除过期在召回同样生效（同一 WHERE）；`include_expired=true` 显式可见。
- **短查询回退**：<3 rune 走 `LIKE`（title/content，COLLATE NOCASE——与 memstore 子串基准同语义；trigram 不可用区间）。
- **排序**：召回模式 `bm25(memory_fts)` 升序（越相关越前）+ `rowid DESC` 兜底；非召回保持创建序降序。
- **分页**：召回模式**不支持游标**（rank 序下 rowid 游标语义不稳）——limit 截断（默认 50 上限 100），契约注明；非召回模式游标不变。
- **计数**：响应 += `total_hits`（过滤+匹配后的全量计数，不含 limit 截断——"先过滤再计数"）。非召回模式 total_hits 同样给出（该口径下总数）。

### 3. 端口与双实现

- `ListFilter` += `Query string`；`Store.ListItems` 返回 `(items, total, error)`（total = 过滤+匹配全量计数）；`Service.List` 返回 `ListResult{Items, NextCursor, TotalHits}`。
- memstore：Query 用子串匹配（`strings.Contains` + 大小写不敏感——与 LIKE 基准同语义；room SearchMessages 线性基准同法）；排序保持创建序降序（无 rank——语义子集，测试口径以"命中集与 total"为准，排序差异由 sqlite IT 独占断言）。

### 4. 契约（apigen 冻结增量）

- `GET /v1/memory/items` += `q?: string`（≤200 rune；召回模式——过滤先行 + bm25 排序 + 无游标）。
- `MemoryItemListView` += `total_hits: integer`（required——过滤+匹配后总数，不含分页截断）。

### 5. SPA

`/memory` 页 += 搜索框（受控输入进列表 effect 依赖——与 tab/过滤/口径同源刷新）+ 结果计数显示（"共 N 条"）。

### 6. 测试与验证

域 UT（q 命中集/大小写/total 计数排除 status 与时效过滤项）；sqlite IT（trigram 中文 ≥3 字与英文命中、<3 字 LIKE 回退、**过滤先行断言**——异域同词条目不出现在结果与 total、修订后 FTS 同步旧词不中新词中、superseded 不入召回、触发器同步、v33 存量回填）；httpapi UT（q + total_hits）；真机冒烟（中文召回/过滤先行/total 口径）；全量 vet/-race + it/st + 生成物幂等 + SPA。

## 审校修订记录

- v0.3（2026-11-05）：二审 2 项 P2 修复——
  1. **MemStore 游标漏续页（#1 P2）**：游标定位提前至 **namespace 隔离后、其余过滤前**——游标条自身被撤回（status）或过期（时效）不影响续页收集门；计数与收集门解耦（passes 判据）——每页 total 恒定保持。补撤回/过期两种续页回归。
  2. **全库 BM25 统计污染排名（#2 P2）**：FTS5 `bm25()` 是**全索引统计**（avgdl 含过滤外条目）——六组过滤外的长文增补均翻转本域排名。召回改 **MATCH+过滤驱动取全量候选行 → Go 语料内 BM25 精排**（k1=1.2/b=0.75，dl/avgdl/tf 限定过滤后语料；输入 rowid 降序 + 稳定排序兜底）。补六组过滤面排名不变回归；性能代表性提升至 **2000 命中 <1s**（实测 0.15s）。
- v0.2（2026-11-05）：一审 4 项（1 P1 + 3 P2）修复——
  1. **MATCH 驱动 JOIN（#1 P1）**：召回改 `FROM memory_fts f JOIN memory_items m ON m.memory_id = f.memory_id WHERE f.memory_fts MATCH ? AND <公共过滤>`——FTS 索引一次 MATCH 驱动、连接过滤、bm25 在 MATCH 行上评分。原 `memory_id IN (子查询)` + 逐候选 `bm25()` 子查询对过滤后每行全扫 FTS 表（1000/9000 命中约 1.56/14s）——受控 300 命中实测 **8ms**。
  2. **LIKE 字面转义（#2 P2）**：短词路径 pattern 转义 `%`/`_`/`\` 并显式 `ESCAPE '\\'`（SQL 字面单反斜杠）——字面 `100%`/`_a` 恰命中含该字符条目（原通配命中全部）；与 memstore `strings.Contains` 双实现同语义（对照测试固化）。
  3. **total 与分页解耦（#3 P2）**：计数只含公共过滤（namespace/scope/kind/status/时效），**不含 before**——七条 limit=2 首页与每续页 total 恒 7（sqlite 原续页 5、memstore 原首页 3）；memstore 去提前 break、before 只作用收集门。
  4. **bm25 真实评分（#4 P2）**：MATCH 与评分共享游标——旧强命中不再被新弱命中挤出（limit=1 用例固化）；与 #1 同行同修但独立断言（评分与扫描分别验收）。

## 后果

- 记忆可按内容召回，且召回纪律（过滤先行、计数不泄漏）在 SQL 层固化——0.7.3 Checkpoint 引用与 0.7.5 Bundle 导出复用同一过滤口径。
- rank 排序仅 sqlite 实现（memstore 语义子集）——双实现差异显式记录，命中集一致性由测试保证。
- Forget（删除面，0.7.8）落地时触发器补 AD（删除同步）；向量/图检索推迟决策维持 §7.4 原裁定（用真实任务记录漏召回后再评估）。
