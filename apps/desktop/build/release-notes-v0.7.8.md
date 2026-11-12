# Release v0.7.8 — 派生删除与旧包防复活：墓碑、删除传播与影响报告（0.7 记忆与可移植恢复领域·第九件）

0.7 线补上"删不掉"的最后一环：记忆隐私删除（墓碑）+ 删除传播与影响报告 + 旧包防复活——领域出口验收 AT-06（"记忆纠错/删除后导出、恢复或导入旧包——最新修订与墓碑生效；派生内容可追踪；受管理副本不复活已删正文"）落地。

## 新增

- **记忆 Forget（隐私删除）**：`POST /v1/memory/items/{id}/forget`——**墓碑化而非删行**（RFC §11.4"审计仅保留不含正文的墓碑"）：status=deleted、Title/Content 置空、归属/来源引用/敏感度/留痕保留（cause=delete 留痕即删除代次事实源）。FTS 经既有触发器随正文置空同步 + 召回 status 过滤——**双保险结构性不可达**（SQLite 实测）。幂等；active/proposed/superseded 皆可删；**策展四动作（接受/否决/修订/替代）拦截墓碑**——终态不可被策展接口消除。
- **删除传播与影响报告**：cleared（正文/索引）+ **contained_in_bundles**（托管导出包清单——zip 定版快照不重写，可经包删除端点处置）+ **contained_in_imports**（本机导入 zip 清单——activated 记录不可删，正文随记录保留直至治理）+ out_of_reach（"已在他实例激活的副本无法召回"）——**不宣称全域擦除**（RFC 原文口径）。多键扫描（本地 ID + 来源标记尾段——源 zip 内存源实例 ID）。
- **旧包防复活**：防复活键**归一到稳定记忆身份**（mem_ 裸 ID + bundle 标记尾段——跨导出/跨迁移跳同键；普通证据引用不参与）；两道门——预检 `tombstone_hits` finding（warn 列 ID）+ 激活 **CreateGuarded**（墓碑核对与创建在同一互斥边界——Forget 全程持同锁，无"查后插入仍写回"交错窗口，50 轮并发交错测试钉住）。
- **包生命周期删除端点（0.7.5/0.7.6 欠账兑现）**：`DELETE /v1/bundle-exports/{id}`（目录清除 + 不可达注记）；`DELETE /v1/bundle-imports/{id}`（inactive 删除含 attempt；**activated → 409**——激活产物是独立域状态，撤销激活另行交付）。
- **UI**：记忆页"遗忘"操作（确认警示 + 影响报告卡：双清单/不可达注记）；迁移包面板导出/导入历史行删除（activated 注明不可删）。零 migration。

## 修复（随本版）

- 一审 4 项 + 复审 3 项全关闭（策展拦截/跨包键归一/守卫边界/导入 zip 漏报/Forget 持锁/多键扫描/证据引用白名单）——见 [CHANGELOG](https://github.com/sisibeloved/Mosaic/blob/main/CHANGELOG.md#078---2026-11-12)。

---

**产物**：mosaic.exe（Windows/amd64）。数据格式迁移链无新增（v35 保持）；墓碑在 memory_items 原行（零新表）；删除的包记录是文件系统操作。
