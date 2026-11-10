# Changelog

本文件记录 Mosaic 的显著变更，面向使用者和维护者，而不是 git 日志的简单堆砌。格式基于 [Keep a Changelog 1.1.0](https://keepachangelog.com/zh-CN/1.1.0/)。版本号位置沿用 `X.Y.Z`；自 0.5 领域线起采用项目约定的“大版本 / 领域中版本 / 特性小版本”规则：第二段按领域聚合系统性交付，第三段每次只交付一个特性，不宣称遵循严格 [Semantic Versioning](https://semver.org/spec/v2.0.0.html) 的功能与修复升位规则；历史版本不回溯改写。

## [Unreleased]

### Added

- **0.7.5 Portable Agent Bundle 导出：范围选择、摘要与脱敏报告**（0.7 领域线第六件 / ADR-0042，RFC-0018 §8——跨实例可移植首环）：
  - **范围选择**：`POST /v1/agents/{agent_id}/bundle-exports`——命名空间 ⊆ `agent|workspace|mission`（缺省 agent+workspace；room/slot 授权口径未建 v1 不开放，显式拒绝）；`mission_ids` 缺省 = 自动圈定非终态在册任务（draft 不带走），显式数组逐条校验归属、不符整体拒绝（透明不静默缩圈）；`include_sensitive` 缺省 false。
  - **摘要与水位**：包内 `manifest.json` 逐文件 sha256 + 字节数（完整性检查用——不等同于可信签名；manifest 自身不在 files 内）+ 任务级 `{contract_version, checkpoint_version, checkpoint_source}` 水位与命名空间入包计数（导入预检 0.7.6 判陈旧的依据）；来源实例只记产品版本（最小事实面）。
  - **脱敏报告**（包内 `redaction-report.json` 同构内联在响应）：敏感省略（默认省略、只 ID 不内容/标题——审计可用且不泄敏感面）、口径排除计数（时效过期/策展未决/已终结——0.7.1/0.7.2 同一 WHERE 口径的导出侧应用，未授权 scope 结构性不查询）、逐任务恢复限制（无检查点——导入后从契约裸启动）、`artifact_policy: refs_only`（v1 引用保留不携文件，目标侧可访问性未判定）与设计边界 notes（凭据/令牌/原生会话材料不随包）。
  - **包布局 v1**（RFC §8.2，zip 内相对路径）：`agent/profile.json`（身份+三向别名+可执行项引用）、`slots/specs.json`（现任 active 任职 + 钉版模板内容）、`memory/items.ndjson`、`work/checkpoints.ndjson`（每选中任务一条——0.7.4 LatestForResume 口径复用：业务快照优先回退系统终态摘要）、`work/commitments.json`（契约快照 + 原对象映射 + 导入默认暂停标记）、`context/manifests.ndjson`（引用与证据清单）、`dependencies.json`、`redaction-report.json`、`manifest.json`。
  - **域与装配**：`internal/bundle` 新包（Ports 全闭包 + 域内投影类型——不 import personalagent/slot/mission/checkpoint/memory，依赖方向同 execctx）；产物落数据目录 `bundles/<bundle_id>/`（zip + export.json 边车，**零 migration**——backup 同例）；列表/详情端点（`GET /v1/agents/{agent_id}/bundle-exports`、`GET /v1/bundle-exports/{bundle_id}`——bundle_id 白名单防穿越）；设置页新增"迁移包"面板（Agent 选择/命名空间勾选/任务口径/敏感开关/脱敏报告内联展示/导出历史）。
  - 验证：bundle UT（范围校验族/默认范围与自动圈定/敏感省略与显式携带/时效与状态口径/mission 记忆随任务/检查点选取与恢复限制/zip 布局与摘要逐一相符/边车读取与防穿越）+ httpapi IT（三端点全语义）+ gofmt/vet 双标签 + UT/-race/it/st + 生成物幂等 + SPA；真机冒烟（默认范围导出→敏感/过期/未决排除计数→zip 九文件布局→manifest 摘要逐文件核对→水位→显式圈定含恢复限制→列表/详情→越权任务 400）ALL-PASS。

### Fixed

- **0.7.5 一审修复（5 P2，ADR-0042 v0.2）**：
  - **敏感选项跨 Agent 保留**：迁移包面板换 Agent 重置面补齐——include_sensitive 与命名空间/任务口径一并回缺省（为 A 开启的敏感开关不得替 B 做导出决策）。
  - **迟到响应覆盖当前 Agent**：面板所有异步响应按 Agent 代次校验（旧代次回包整体丢弃），切档清空旧任务清单——任务/历史/导出结果不串档。
  - **任务选择依赖记忆开关**：任务口径（自动/显式/不携带——显式空集 = 不带任务）与 mission 命名空间勾选解耦；承诺与检查点随任务口径独立入包。
  - **导出包含源安装路径**：注册表 ExeID 含安装路径（暴露本机用户名/目录）——wiring 投影为可移植标识 `adapter@runtime[:distro]`（portableExeID，单测钉住无路径分隔符）。
  - **交接后的任务仍归入原 Agent**：wiring Get 详情后复核 owner（窗口收窄）+ 域级权威复核（自动圈定跳过非所属、显式圈定整体拒绝；已交接任务的 mission 记忆不被查询——UT 钉住）。
- **0.7.5 二审修复（2 P2，ADR-0042 v0.3）**：
  - **路径脱敏遗漏身份别名**：一审只投影 ExecutableID——`BotIDOf` 回退 = ExeID（含路径）且派生 ID 嵌入用户名（profileKey 仅换 `/`），agent/profile.json 与 manifest.json 仍泄漏。`portableAliases` 三元组整体投影：BotID 切路径段、派生 ID 由脱敏后 BotID 单源重算（包内自洽；非路径型原样透传）；补整包脱敏断言（投影后身份进 zip 全文本无路径/用户名——`TestPortableIdentityIntoBundle`）。
  - **清空选择后迟到结果回填**：代次递增移到空值判断之前——A → "选择 Agent…" 同样作废在途响应（原空值分支提前返回未递增，A 的导出结果/成功消息会回填空选择页面）。

## [0.7.4] - 2026-11-08

### Added

- **0.7.4 跨 Harness 续办：Checkpoint 注入执行上下文**（0.7 领域线第五件 / ADR-0041，RFC-0018 §1/§7.3/§7.4——原生会话不可用时仍能恢复工作）：
  - **清单 checkpoint 层接线**（execctx 架构预留位启用——`ReasonCheckpointUnavailable` 仅首跑保持）：`Ports.Checkpoints` 闭包端口按任务取最新检查点，装入 `checkpoint` 层（role resume，进 digest/存档/回执）；**漂移不阻断**（`checkpoint.mission_version != contract_version` → 层内 `contract_drift: true`——进度是业务状态，契约修订不必然作废）；读取失败报错（不静默 omitted——恢复面不能丢事实）。
  - **任务级最新检查点**：`checkpoint.Service/Store += LatestOfMission`（跨 Run——新 Run ≠ 旧 Run，续办取任务级最新；sqlite rowid DESC / memstore 插入序同语义）。
  - **Run 终态自动落盘**（0.7.3 预留兑现）：`run.Service` += `CheckpointSaver` 端口（`WithCheckpointSaver`）；`finalizeRun` 在终态写入成功后落系统检查点（`progress_summary = "Run {id} 终态 {state}"`，≤500 字）；**best-effort**（失败不阻断终态——终态是权威事实）。执行面显式落盘（业务步骤）仍走 0.7.3 API。
  - **Inline 透传**：`manifestInline` 为 checkpoint 层抽 `resume_checkpoint` 键 → `supervisorExecutor` 既有 `Context{Inline}` 全量重递——新会话 Boot 即收到续办状态，换 Harness/重启后工作上下文不丢。
  - 验证：execctx UT（层装入/漂移/首跑 omitted/读错阻断）+ checkpoint UT（LatestOfMission 跨 Run 取最新）+ run UT（终态自动落盘/best-effort 不阻断/nil 兼容/Inline 透传）+ vet/-race + it/st + 生成物幂等 + SPA；真机冒烟（显式落盘含漂移 → 新 Run 清单含 checkpoint 层 + contract_drift → 终态自动落盘摘要入表）ALL-PASS。

### Fixed

- **0.7.4 二审修复（1 P2，ADR-0041 v0.3）**：
  - **无业务快照时续办层丢失（一审 #1 修法回归）**：`LatestForResume`——业务快照优先，无业务条时**回退最新系统终态摘要**——暂停/超时/完成/失败后没有业务快照的场景，新会话的 `resume_checkpoint` 仍可达（层 included 携终态摘要与 `source` 标记）；有业务条时系统条永不入选（一审"不遮蔽"语义保持）。

- **0.7.4 一审修复（1 P1 + 2 P2，ADR-0041 v0.2）**：
  - **终态摘要遮蔽业务快照（#1 P1）**：checkpoints += `source` 列（migration v35）；`LatestOfMission` 排除 system 终态摘要——Run 完成后下次续办仍收业务字段（采集/清洗步骤保留，不再被自动摘要清空）；系统条历史可查。
  - **预算计算遗漏检查点（#2 P2）**：execctx 可选配额扣减已装入的 checkpoint 层字节数——检查点与必需内容本可放入 256 KiB 时，房间消息按收缩后配额装窗，不再误阻断续办。
  - **运行中停止缺少续办摘要（#3 P2）**：`saveTerminalCkpt` 统一全终态路径（完成/失败/暂停/取消/墙钟停止/围栏拒绝）——停止后无更早检查点时续办仍可见停止事实；保持租约释放与执行器退出顺序。

## [0.7.3] - 2026-11-06

### Added

- **0.7.3 Checkpoint：可恢复业务状态**（0.7 领域线第四件 / ADR-0040，RFC-0018 §7.3——独立于 ContextReceipt：那是"交付了什么"，这是"工作到哪了"）：
  - **域模型（§7.3 字段全量）**：进度（completed/pending steps——**交集须为空**："文字摘要不能将 pending 偷换成 completed"的机器可判子集）、产物引用（artifact_refs）、动作回执（action_receipts）、不确定动作（uncertain_actions）、等待条件（waiting_conditions）、各领域水位（source_watermarks——跨流对齐不虚构全局序号）、简短公开摘要、work_item 锚。
  - **双锚**：Run 在册且 `Run.MissionID == mission_id`（404 `run_not_found` / 409 `checkpoint_scope_mismatch`——归属一致性门）；`mission_version` 记录落盘时契约版本（恢复时判漂移——"必须能重新读取权威任务状态"）。
  - **checkpoint_version 存储原子分配**（INSERT 语句内子查询 MAX+1——单语句原子，并发落盘经 `(run_id, checkpoint_version)` 唯一索引兜底不撞号；执行面无状态落盘）。
  - **读写面**：`POST /v1/mission-runs/{run_id}/checkpoints`（owner 写门 + 严格拒收）+ `GET …/checkpoints`（降序——最新在前）+ `GET …/checkpoints/latest`（恢复面入口；无落盘 404 `checkpoint_not_found`——区分"无检查点"与"有"）。0.7.3 为显式写入面；Run 生命周期自动落盘挂点随 0.7.4 续办接线。
  - SQLite migration **v34**（checkpoints + 唯一/查询索引，幂等）；memstore 双实现同语义。
  - 验证：域 UT（校验矩阵/双锚/并发 20 落盘版本不撞号）+ sqlite IT（v34/版本递增/降序/JSON 列读回/并发 10 race）+ httpapi UT（三端点 + 错误族）+ vet/-race + it/st + 生成物幂等；真机冒烟 10 项 ALL-PASS（Run 锚全链路——runtime→agent→grant→任职→mission→run→三级落盘→latest/降序→双锚/校验族）。

## [0.7.2] - 2026-11-05

### Added

- **0.7.2 记忆召回：FTS5 与授权过滤先行**（0.7 领域线第三件 / ADR-0039，RFC-0018 §7.3/§7.4）：`GET /v1/memory/items` += 可选 `q`（召回模式）——
  - **FTS5 trigram 索引**（migration v33：`memory_fts(title, content)`；AI/AU 触发器同事务同步——策展修订/替代路径零改动；存量回填幂等）。≥3 rune 走 MATCH（phrase 引号防注入——room_fts/doc_fts 同裁定：unicode61 中文整串单 token 不可用，trigram 对 CJK 子串与英文均正确命中）；<3 rune 回退 LIKE 子串（COLLATE NOCASE）。
  - **过滤先行**（RFC §7.3 铁律）：namespace（授权）+ scope/kind/status + 时效过滤与 FTS 匹配在**同一 WHERE**——他域命中不出现在结果也**不计入 total_hits**（存在性不泄漏）；0.7.1 的默认排除过期在召回同样生效。
  - **排序与计数**：召回模式 bm25 相关性升序（rowid DESC 兜底）、无游标（limit 截断）；响应 += `total_hits`（过滤+匹配后总数，不含分页截断——非召回模式同给）。memstore 子串基准同语义（命中集/total 一致——排序为 sqlite 独占）。
  - **SPA**：记忆页搜索框（召回词进列表 effect 依赖——与浏览条件同源刷新）+ 结果总数显示。
  - 验证：域 UT（命中集/大小写/total 排除 status 与时效项/召回无游标）+ sqlite IT（trigram 中文/英文/短词回退/过滤先行/修订 FTS 同步/终结不入召回/bm25 序）+ httpapi UT + vet/-race + it/st + SPA + 生成物幂等；真机冒烟 6 项 ALL-PASS。

### Fixed

- **0.7.2 二审修复（2 P2，ADR-0039 v0.3）**：
  - **MemStore 游标撤回/过期后漏续页（#1 P2）**：游标定位提前至 namespace 隔离后、状态/时效过滤**前**——首页末条被撤回或变过期后，续页仍取到更早条目（原返回空列表而 total=2）；计数与收集门解耦，每页总数恒定保持。生产 SQLite 不受影响（对照确认）。
  - **过滤外条目改变召回排名（#2 P2）**：FTS5 `bm25()` 使用全索引统计（avgdl 污染）——仅增一条被 namespace/scope/kind/状态/时效任一过滤的长文即可翻转本域排名。召回改 **MATCH+过滤驱动取全量候选 → Go 语料内 BM25 精排**——评分统计限定于过滤后语料，排名与 total 对语料外增删不敏感；性能代表性提升至受控 2000 命中 <1s（实测 0.15s）。

- **0.7.2 一审修复（1 P1 + 3 P2，ADR-0039 v0.2）**：
  - **召回随命中数线性变慢（#1 P1）**：MATCH 改为 FTS 索引驱动 JOIN（一次 MATCH → 连接公共过滤 → 评分排序）——原逐候选 `bm25()` 子查询对过滤后每行全扫 FTS 表（受控 1000/9000 命中约 1.56/14 秒）；修复后受控 300 命中 **8ms**。
  - **字面 %/_ 误命中（#2 P2）**：短词 LIKE 路径转义 `%`/`_`/`\` 并显式 `ESCAPE`——字面 `100%` 恰命中含该字符条目（原通配命中全部）；与内存件 `strings.Contains` 同语义（双实现对照测试）。
  - **列表总数被分页截断（#3 P2）**：`total_hits` 只按公共过滤计数（不含 `before`）——七条 limit=2 时首页与每续页恒 7（SQLite 原续页 5、内存件原首页 3）；内存件去除提前 break、游标只作用于收集起点。
  - **bm25 评分失效致旧强命中被挤出（#4 P2）**：评分与 MATCH 共享游标（原独立子查询脱离 MATCH 上下文恒零、退化为纯创建序）——旧强命中排在新弱命中之前（limit=1 用例固化）。

## [0.7.1] - 2026-11-05

### Added

- **0.7.1 记忆策展与纠错：接受、修订、否决与 superseded**（0.7 领域线第二件 / ADR-0038，RFC-0018 §7.2——0.7.0 预留列直接启用，**零迁移**）：
  - **状态机**：`proposed → accept → active`（幂等——重复确认无害）；`reject`（候选否决 cause=reject）/ `withdraw`（active 撤回）→ `superseded` **唯一终结态，不可复活**（409 `memory_terminal`——与 0.7.8 旧包防复活同基调）；Create 写入面 += 可选 `status`（默认 active——高可信明确指令；不确定候选留 proposed 待策展；终结态不可直造）。
  - **原位修订 `POST /v1/memory/items/{id}/revise`**：title/content/source_refs/时效/敏感度/置信度可改（归属与状态另有动作面）；`expected_version` CAS；版本递增 + **edit_history 恰追加一条**（append-only——"人工纠错具有明确版本记录"）。
  - **显式替代 `POST /v1/memory/items/{id}/supersede`**：**单事务双动作**——旧条目终结（留痕）+ 新条目创建（`supersedes=[旧 id]`；replacement 可跨域——迁移纠错是合法替代）；CAS 失败整体不落。事实随时间变化不用替代（revise 调整有效期或新建条目）。
  - **时效口径**：默认 active 口径**排除已过期条目**（`valid_to < now`——RFC3339 **时刻**比较经 `julianday()` 归一，0.6.13 审校 #15 同教训）；`include_expired=true` 显式可见（历史查询仍能解释当时依据）；proposed/superseded 显式口径不过滤（策展审视看全量）。**行为变更**：0.7.0 列表默认含过期条目，0.7.1 起默认排除。
  - **契约**：`MemoryEditRecordView`（version/cause/note/edited_by/occurred_at）+ `MemoryItemView.edit_history` + 三请求/结果 schema + 列表 `include_expired`；生成物再生成（幂等验证）。
  - **SPA**：状态口径过滤（现行/候选/历史）+ 含已过期开关 + 条目操作（接受/否决与撤回/修订表单/替代表单——替代预填旧内容）+ 版本留痕链与替代链显示。
  - 验证：域 UT（状态机全矩阵/CAS/留痕恰一条/时效边界含 valid_to==now）+ sqlite IT（策展链 + 事务原子 + julianday 口径）+ httpapi UT（四端点 + 409 族）+ vet/-race + it/st + SPA + 生成物幂等；真机冒烟 14 项 ALL-PASS（proposed 链路/幂等接受/撤回否决/修订 CAS/替代事务/时效开关）。

### Fixed

- **0.7.1 二审修复（1 P2，ADR-0038 v0.3）**：
  - **UTC 规范化丢失小数秒（一审 #4 修法回归）**：`normTime` 格式化改用 **RFC3339Nano**——合法区间 `12:00:00.100Z → 12:00:00.900Z` 经创建/替代后不再塌缩成整秒（此前区间两端同刻 → 仅改标题修订 400；截止 `.900Z` 在 `.500Z` 被默认口径提前隐藏；`observed_at` 同失精度）。回归覆盖创建/修订/替代与过期边界（.9Z 在 .5Z 未过期、.95Z 过期）。
  - **测试缺口补强**：状态 CAS 集成测试重写——同版本、状态失配拒且**数据/审计不变断言**（整行回读比对，移除状态条件必红）；补版本失配与基准相符分支。

- **0.7.1 一审修复（2 P1 + 3 P2，ADR-0038 v0.2）**：
  - **并发撤回后旧修订复活终结记忆（#1 P1）**：Revise/Supersede 读快照后先核对请求版本与快照一致（绑定快照提交——"读到 v1 却以 expected=2 过检"窗口关闭）；CAS 升级为**版本 + 状态双基准**——并发撤回产生的同名版本（v1→superseded v2）不再让旧修订写回 active 覆盖终结事实。
  - **并发修订撤销接受并抹掉审计（#2 P1）**：accept 不递增版本的约定保持，双基准 CAS 拦住"基于 proposed 快照的陈旧修订"（状态已 active）；接受留痕不再被抹；Accept 并发交错幂等收敛（CAS 失败重读——已被接受则返回现行）。
  - **"含已过期"开关未生效（#3 P2，前端）**：API 客户端将 `include_expired` 编码进 URL 查询参数——勾选后可见过期记忆。
  - **部分合法时区格式被误过滤（#4 P2）**：写入/修订/替代的三个时间字段统一 **UTC 规范化**（`+15:00` 等偏移读回为 Z 后缀——与 sqlite `julianday` 过滤同语义）；既有偏移格式行（julianday 返回 NULL）默认口径**保守包含**不误隐。
  - **替代丢失敏感标记（#5 P2，前端）**：替代表单预填原 sensitivity 并恒携带于 replacement（只改标题不再降级 normal）；修订表单同开放敏感度修改。

## [0.7.0] - 2026-11-05

### Added

- **0.7.0 分域记忆基石：MemoryItem 五命名空间**（0.7 领域线第一件 / ADR-0037，RFC-0018 §7.1/§7.2）：每条记忆归属恰一命名空间（`agent` / `room` / `workspace` / `mission` / `slot`）并锚定归属对象（agent_id / room_id / workspace 恒 `default`——单工作区事实 / mission_id / slot_instance_id）——**锚对象不存在写入即拒**（409 `memory_scope_not_found`，app 装配组合四域存在性闭包）。类型（`semantic` / `episodic` / `procedural`）与权限域正交；来源引用（≤16 条）、时效（observed_at + valid_from/to——"何时知道"与"何时有效"分记）、敏感度（normal/sensitive——0.7.5 Bundle 脱敏消费）与置信度落字段；**表结构一次带全 §7.2 拟定字段**（status/visibility/supersedes/derived_from/edit_history/extractor_version 列预留——策展状态机随 0.7.1、授权过滤与 FTS 召回随 0.7.2，写入面 0.7.0 固定 status=active、visibility=namespace）。
  - **读写面**：`POST /v1/memory/items`（owner 写门 + 严格拒收未知字段）、`GET /v1/memory/items?namespace=…`（**namespace 必填隔离**——跨域列表不存在，越域内容与存在性不泄漏；scope/kind/status 过滤，status 缺省 active；创建序降序 + `next_cursor` 游标）、`GET /v1/memory/items/{id}`（未知 404）。SQLite migration **v32**（memory_items + 双索引；幂等重放）；房间收束胶囊（RFC-0007）保持房间事件流权威，`room` 命名空间为显式写入的派生记忆（不经胶囊复制）。
  - **SPA**：`/memory` 记忆页（五命名空间 tab + 归属锚过滤 + 记忆卡列表/详情展开 + 显式写入表单——workspace 锚锁定 default、来源按行拆分、时效区间可选）。
  - 验证：域 UT（校验矩阵/锚矩阵/默认填充/列表隔离与游标）+ sqlite IT（v32 结构/CRUD/游标/幂等重开）+ httpapi UT（写门/域校验/锚门/隔离/单查/未装配 503）+ vet/-race + it/st + SPA + 生成物幂等；真机冒烟 19 项 ALL-PASS（五域写入/拒收面/隔离游标/写门）。

### Fixed

- **0.7.0 二审修复（1 P2，ADR-0037 v0.3）**：
  - **创建等待期间切换筛选被旧列表覆盖（一审 #2 修法回归）**：写入成功回调不再直接调用携带提交时闭包条件的刷新函数——只发 `refreshTick` 信号，由依赖当前 `ns`/`scopeFilter`/`refreshTick` 的列表 effect 以**当次渲染**条件重查。POST 等待期间切换 tab 或筛选，成功后显示仍为当前浏览条件（切到工作区不再显示 Agent 记忆、筛选 ag_b 不再显示 ag_a）。

- **0.7.0 一审修复（1 P1 + 3 P2，ADR-0037 v0.2）**：
  - **空数组规范化（#1 P1）**：`source_refs` 省略或传 `[]` 时三层统一输出空数组——域 Create 规范化非 nil、sqlite 读回兼容 v32 首版已落的 null 列、契约视图兜底；SPA 显示同步兜底。含该记录的列表不再使记忆页崩溃。
  - **创建后刷新口径（#2 P2，前端）**：写入成功后的刷新统一走**当前浏览条件**（tab + 过滤输入单源）——Agent 标签下不再显示工作区记忆、筛选 Agent A 不再显示 Agent B 列表。
  - **续页竞态（#3 P2，前端）**：续页请求加代次检查（切换命名空间后迟到响应丢弃——跨域混入关闭）与在途限制（同游标连续点击不重复追加）。
  - **游标域内定位（#4 P2）**：游标子查询限定 `namespace`（sqlite 与内存实现）——外域 ID 作 `before` 一律空页（与未知 ID 同语义），不再随外域条目存在与否漂移。

## [0.6.13] - 2026-11-04

### Added

- **0.6 域收口：网关续接、Slot 职责寻址与墙钟强制**（0.6 领域线第十四件 / ADR-0036，RFC-0018 §4.5/§6.1/§10.3，AT-14/15/18——v2.69 盘点三项落差收口）：**网关续接 `POST /v1/dispatch/update`**（= mission_update，§10.3）：幂等键先行（同键同载荷重放原回执 / 同键异载荷 409 / pending 恢复重驱动）→ 服务端复核（任务存在 + owner active——续接不发往已停身份）→ revise（CAS + 归属锚由网关以服务端读值填充，**owner 不变**——换人走交接）→ 回执（`domain_command_id = mission:<id>:revise:v<新版本>`）；领域拒绝原样透传（网关不吞并）；无 route 决策行（无寻址面，竞态防护即 CAS + 归属锚）。**Slot 职责寻址**（§4.5）：resolve 请求 += `slot_instance_id`（显式实例）或 `slot_spec_id`（模板——scope 内无实例 blocked `slot_no_instance` / 多实例歧义 blocked `slot_ambiguous` 禁止猜选 / 恰一实例继续）；寻址链 scope → SlotInstance → **active Binding** → AgentID（无 active 任职 blocked `slot_unassigned`——**不自动激活 ready Binding**）；解析结果写入决策存档的 owner（`selection_source=slot_binding`）；**投递复核**：决策后任职换人 → `route_stale` 重新解析（§4.6"不能只改路由缓存"）；与显式 `owner_agent_id` 互斥（并存 400——显式 Agent ≠ 责任已移交）。**墙钟强制**（§6.1 / AT-18）：Run 进入 running 时按执行开始契约的 `budget.max_wall_clock_seconds` 快照 `deadline`（migration v30：runs += deadline 列；修订不追改在途）；扫描过期 → 走既有停止面（run canceled + `cause=wall_clock_exceeded` 事件 + Mission 回 ready，迟到结果只审计）——停止面零新增机制；**max_runs 派发上限**一并落：已创建 Run 总数达上限不再派发（`budget_exhausted_hits` 诊断计数，任务停留 ready 不被静默吞）。
  - **契约**：`DispatchUpdateRequest` schema + `DispatchResolveRequest` += slot 引用 + `SchedulerStatus` += `budget_exhausted_hits`；apigen/TS 再生成。
  - **装配**：`dispatch.SlotLookups` 端口（app 直查 store——实例/active 任职存储事实）；`runMissionsAdapter.ListReady` 携 budget 快照（墙钟 + max_runs）。
  - 验证：UT 续接幂等/复核/透传族 + 寻址五分支 + 换绑 stale + 墙钟到期停止（可变时钟 + 阻塞执行器）+ max_runs 上限；IT v30 新库/旧库升级 + deadline 落行与过期查询；API 续接全链 + slot 寻址；全量 vet/-race + it/st + SPA 绿；真机冒烟 13 项 ALL-PASS（v30 结构 + max_runs 上限实测 + 续接幂等 + 寻址/换绑 stale/互斥/无任职）。

### Fixed

- **0.6.13 十二审修复（2 P1，ADR-0036 v1.3）**：
  - **逐字输入来源丢草稿（#1，前端）**：删除来源键迁移——task 首字符形成的伪完整中间键不再把未绑定草稿归属固定死；改为**提交时**按最终来源接纳（归属为空键/部分键/逐字中间键——提交键的输入前缀态——划归提交键，其它完整卡键隔离）。先填正文与 1 次/60 秒、再逐字输入来源补全，409 续接保留草稿与限制。
  - **切已映射卡清掉待合并记录（#2，前端）**：待合并冻结编辑按任务卡 Map 保留——其它任务卡的详情加载不清除记录；回到引发 409 的卡才消费；提交成功清空。A 等待 409 期间切到已映射 B 编辑，回到 A 续接仍合并 A 的提交快照。

- **0.6.13 十一审修复（2 P1，ADR-0036 v1.2）**：
  - **先草稿后来源仍丢输入（#1，前端）**：归属迁移改为只在**完整来源**（room 与 task 皆非空）建立时统一迁移——部分来源（只填 room）不再提前把未绑定草稿迁到部分键。先填正文与 1 次/60 秒预算、再依次填写 room、task，409 续接保留该草稿与限制（不再落库服务器原文与 32 次/86400 秒）。
  - **异步 409 丢提交时载荷（#2，前端回归）**：创建请求发送前冻结**提交时快照**（来源、字段归属、载荷一并拷贝）；409 返回时按快照构建续接目标与冻结编辑——等待响应期间切到其它任务卡编辑不再改写本次合并（A 的正文与 1 次/60 秒限制保留）。

- **0.6.13 十审修复（2 P1，ADR-0036 v1.1）**：
  - **跨卡污染残留（#1，前端）**：草稿归属改为**字段级**——每个契约/预算字段记录最后一次编辑所在卡；409 时按归属冻结本卡编辑快照（合并只消费冻结副本）。未映射 A 卡切到 B 后，只改 B 的目标/范围不再把 A 的 200 次上限与验收/交付/来源/返回带入 B 的合并（未在本卡编辑的字段回落 B 现行值）。
  - **先草稿后来源丢输入（#2，前端）**：**首绑来源**（空 → 卡）把绑定前的编辑归属迁移到新卡——先填正文与 1 次/60 秒预算、再首次填写任务卡，409 合并保留该草稿与预算（不再落库服务器原文与 32 次/86400 秒缺省）。

- **0.6.13 九审修复（1 P1 + 1 P2，ADR-0036 v1.0）**：
  - **跨卡预算/契约污染回归（#1，前端）**：草稿与编辑记录**绑定具体任务卡**（按卡快照 `cardDraftsRef`）——409 合并只用当前卡的编辑快照：未映射 A 卡编辑的 200 次上限与验收/交付/来源/返回不再写进 B 卡（未在本卡编辑的字段回落 B 的现行值）；预算同样只覆盖本卡快照中编辑过的字段。
  - **恢复错误处理遗漏（#2）**：paused/canceling 恢复分支补齐读写错误语义——停止标记读取失败上抛跳过（不再当不存在重复写 stop_requested）；canceled 写失败保留 running 可重试（恢复返回错误，不再静默报成功）。

- **0.6.13 八审修复（3 P1 + 3 P2，ADR-0036 v0.9）**：
  - **逐字输入来源误清草稿（#1，前端）**：预算/草稿绑定**已确认卡**（详情命中过的来源）——输入中的来源是草稿不触发清理；仅离开已确认卡时清理一次（正文不再丢、预算不再回默认）。
  - **Boot 期间撤权仍交付（#2）**：授权复核经 `agent.Task.PreRun` 移入 supervisor——**Boot 完成后、任务送入 Session.Run 之前**调用（既有会话路径同点复核）；Boot 期间撤权即阻止交付。
  - **已停止 pending Run 仍启动执行器（#3）**：执行 goroutine 首步核对该 Run 的**持久停止标记**（建行后、句柄注册前暂停再恢复——标记在册注册表无信号）——命中即不交付（零执行器调用/零副作用），读取失败同样阻止；错误走结果通道由终态仲裁改判 canceled。
  - **恢复审计不一致（#4）**：标记分支 canceled 携原停止动作码（pause/cancel）与 recovery 标记、无重复 stop_requested；paused/canceling 恢复分支同样映射动作码并按去重规则写事件。
  - **恢复失败误吞（#5）**：停止标记**读取失败**上抛跳过（不再当不存在误判 unknown）；canceled 写失败保留 running 可重试（不落 unknown/waiting），恢复返回错误。
  - **断开连接致 failed（#6）**：交付前复核闭包改用 **runCtx**（脱离请求上下文——客户端断开不使授权读取失败把 Run 记 failed）。

- **0.6.13 七审修复（4 P1 + 6 P2 + 1 P3 + 既有 P1，ADR-0036 v0.8）**：
  - **首填来源放宽预算（#1，前端）**：仅换卡（上一来源非空）时重置预算/草稿——先设 1 次/60 秒再首次填写房间任务卡不再被默认值覆盖。
  - **跨卡续接覆盖契约（#2，前端）**：换卡同时清契约草稿——A 卡预填值不再经 409 合并写进 B 卡的范围/验收/交付/来源/返回位置。
  - **交付时点撤权仍执行（#3）**：授权复核挂 `PreDeliver` 钩子——会话启动后、内容送出前（实际交付时点）再核；适配器启动期间撤权即放弃交付。
  - **暂停→恢复交错发布正文（#4）**：Run 停止标记随暂停/取消**同一事务**写入（两段提交间的交错恢复窗口闭合）；标记 NOT EXISTS 去重恰一条（#10：正常暂停不再写两条 stop_requested）。
  - **列表加载期可编辑（#5，前端）**：映射列表未载期间锁定全部契约字段（formLocked）——列表到达触发的详情预填不覆盖加载期输入。
  - **重启后误判 unknown（#6）**：恢复面对在途 Run 先查自身持久停止标记——暂停→恢复→重启序列按停止兑现收尾（canceled + 回 ready），不再 unknown/waiting。
  - **并发停止拖尾（#7）**：信号路径回归零持久写——两个 Run 同时超时，第二个的取消不再被首条事件写拖住。
  - **暂停后交付前撤权误 failed（#8）**：交付前复核失败经结果通道仲裁——停止意图在场按 canceled 终理化（不再直接 failed）。
  - **终写失败伪装 201（#9）**：finalizeFailed/finalizeFenceRejected 的落库错误上抛 `ErrTerminalWrite`——手动入口如实报错，Run 滞留 running 由恢复面收敛。
  - **budget_exhausted 文档（#11）**：手动执行 409 说明补齐（须上调契约预算后重试）。
  - **既有 P1（旧线程复用）**：执行会话按 **Run** 隔离（`mrun:<run_id>`）——撤权重装配后不再 resume 旧线程（旧线程携带已撤销上下文）；任务连续性由清单 Inline 每次全量重递。

- **0.6.13 六审修复（2 P1 + 1 P2 + 1 P3 + 2 基线，ADR-0036 v0.7）**：
  - **跨卡预算串写（#1，前端）**：预算值与编辑集绑定 room/task——切换来源即丢弃上一卡的编辑；A 卡的 200 次上限不再写进 B 卡的续接合并。
  - **暂停→恢复窗口发布正文（#2）**：停止信号**首次送达**即把 stop_requested 持久写入该 Run 的事件流（resume 不清除）；终态写事务核对 **Run 自身**的停止事实——暂停后立即恢复，旧 Run 取得写锁也改判 canceled、正文只审计。
  - **停止动作码（#3）**：锁内改判的 cause 由 Mission 状态码（paused/canceling）映射为公开动作码（pause/cancel——ADR-0029 约定，与控制路径/接口说明一致）。
  - **手动执行 409 说明（#4）**：OpenAPI 补 grant_drift（及 contract_drift/owner_not_active）与"按现行授权重新装配后重试可降级执行"的语义。
  - **基线 P1（交付前撤权）**：提交执行器**之前**复核授权快照指纹——Run 建行后、输入交付前撤权，旧清单不再送达执行器（Run failed 留档 + 重新装配降级）。
  - **基线 P2（收起预算丢设置，前端）**：新建时用户编辑过任何预算字段后，即使收起面板也随请求发送——显式设置的 1 次/60 秒不被缺省 32 次/1 天覆盖。

- **0.6.13 五审修复（3 P1 + 4 P2 + 1 P3 + 既有 P1，ADR-0036 v0.6）**：
  - **预算字段级编辑（#1/#2，前端）**：按字段记录用户编辑——冲突合并以现行预算为底仅覆盖编辑过的字段（只展开面板或只改并发数不再把 1 次/60 秒放宽成 32 次/1 天）；预算面板展开状态移出详情加载依赖（展开/收起只改显示——不再触发重载覆盖已合并或编辑中的草稿）。
  - **重放清单授权回填（#3）**：同 digest 命中存档重放时以本次实际授权重算 GrantFingerprint 随清单交运行层——先预览后运行、旧库存档清单的领取事务撤权核对由此生效（存档行无指纹列）。
  - **迟到审计规范（#4）**：写锁内改判的 late_result 与 finalizeStopped 同规范——preview ≤400 rune（rune 计）、携 `generation`、`published=false`；双存储一致。
  - **终态事件组唯一（#5）**：写锁内改判**替换**原拟提交的 completed/failed 事件（不再追加 late:false 的矛盾事实）；Mission 回 ready 的原因取实际落库终态（`run_canceled`）——Run 行、事件流、状态原因三者重建同一结果。
  - **既有 P1（人工停止）**：终态事务内核对 Mission 持久停止意图（paused/canceling）——人工暂停/取消已生效时未到期限的完成也改按 canceled、正文只审计；失败终态同样并入停止归档。
  - **认领读面鉴权（#6）**：GET `/v1/dispatch/claims/{key}` 执行 owner token 校验（空/错 token 401——完整摘要是处置凭据，与 release 同源保护）。
  - **授权冲突 409（#7）**：`ErrGrantDrift` 映射 409 `grant_drift`（手动执行与其他冲突一致——重新装配后可执行，不再 500）。
  - **遗留指引统一（#8）**：删除 SQLite/内存分支残留的"携当前版本重试"旧指引（照做必得摘要冲突）——仅保留 ErrLegacyPendingUnresolved 的单一处置流程（GET claims 取摘要 → 确认未执行 → release → 更正载荷重提）。

- **0.6.13 四审修复（4 P1 + 4 P2，ADR-0036 v0.5）**：
  - **冲突草稿合并（#1/#2/#5，前端）**：create 撞 409 后保留的用户草稿绑定引发冲突的 room/task（切换来源即丢弃——不再串入其他任务）；命中同源时以现行契约为底稿**合并**用户显式输入字段，未编辑字段与预算留现行值（不清空验收/交付、不放宽预算），合并结果回填供核对；预算开关与输入在详情加载期间禁编辑。
  - **可选授权撤销仍交付（#3）**：execctx 装配计算授权快照指纹（GrantFingerprint）随清单传递，`CreateRun` 建行事务内与当前授权比对——可选上下文/工具撤销（必需判定不变）即 `ErrGrantDrift`（409 `grant_drift`）拒领，重新装配按剩余授权降级（RFC §11.4 撤权立即阻止新交付）。
  - **release 可撤在途建单认领（#4）**：处置资格限定 update 类认领（route_id 空）——create 在途认领 409 `claim_not_releasable`，杜绝公开入口释放在途建单导致同键两单串回执。
  - **写锁等待期停止被完成覆盖（#6）**：`UpdateRunTerminal` 在取得写锁后以现行时刻核对行上 deadline——completed/failed 改按 canceled 落终态、结果仅 late_result 审计（失败终态同样并入停止归档）。
  - **遗留处置闭环（#7/#8）**：`GET /v1/dispatch/claims/{key}` 认领只读面（提供 release 所需完整摘要与状态）；`ReleaseClaim` 单一写事务判定+删除（仅 RowsAffected=1 为 released，零行按真值分辨 finalized/not_found/digest_mismatch——并发完结不再误报成功）；`legacy_pending_unresolved` 文案改为正确指引。

- **0.6.13 三审修复（4 P1 + 2 P2 + 既有前端 2 P2，ADR-0036 v0.4）**：
  - **同键并发串单（#1）**：create 路径拒绝不再释放认领——并发孪生共享同一条 pending，单方释放会让异摘要请求冒用键建第二单（A/B 均成功、重放互串）。拒绝后同键同载荷重试重驱动/重放胜者回执。
  - **新键冒领修订（#2）**：续接 CAS 败按认领来源分流——本请求新插入的认领 → version_conflict 回滚（内容恰与现行等值也不冒领他人修订，新库面关闭）；自动"等值恢复"分支删除。
  - **撤权后仍执行（#3）**：CreateRun 建行事务内核对**任职授权**（钉版模板要求 vs 授权档案，与建单门同源）——清单装配后撤回必需授权，旧清单不送达执行器。
  - **交接后旧负责人执行（#4）**：CreateRun 事务内核对**任务归属**（owner_agent_id vs 执行者——交接承接不递增版本，旧负责人清单不得领取新 Run）。
  - **停止信号输给完成写入（#5）**：finalizeRun 在围栏通过后、终态写入前重新仲裁（停止意图 + 行上期限，fresh now）——围栏检查期间监控发停不再漏网，过期正文只审计。
  - **遗留认领处置（#6）**：旧候选版（未发布）遗留 pending 的续接认领 → 明确 `409 legacy_pending_unresolved` 待核验（不冒领）；`POST /v1/dispatch/claims/{key}/release`（确认未执行后释放，digest 核对）；`ClaimByKey` 认领只读面；release 后携当前版本同键重提。
  - **前端（既有两项）**：详情加载期间契约字段禁编辑（防读取返回覆盖输入）；create 撞 `source_already_mapped` 后保留录入切续接模式（仅取现行版本与基准，不预填覆盖）。

- **0.6.13 复审修复（13 项：5 P1 + 6 P2 + 2 P3，ADR-0036 v0.3）**：
  - **建单事务门统一**（#3/#5/#7）：`CreationGate`（职责意图五核 + 模板唯一性锚 + `RequireOwnerActive` 身份门）——显式 owner 网关交办在建单事务内核对身份（预检后退役/暂停不得落入新承诺）；模板引用的建单核对该 scope 实例集仍恰为解析实例（长出兄弟实例即歧义拒建；显式实例路径不受限）；授权档案事务内快照补 `workspace_access`（合法 read_write 不再被当 none 误拒 409）。
  - **执行预算单快照**（#4/#10）：预算取**清单钉住版本的契约层**（与执行输入同版本——RefOf 后修订收紧即时生效，不混用两版本）；`CreateRun` 建行事务内核对现行契约版本（漂移 409 `contract_drift` 释放租约重新装配）与现行 max_runs；执行者门核负责人 active + 所钉任职未失效/换人（暂停身份启动 409 `owner_not_active`，任务停留 ready）。
  - **完成提交前终态判定**（#9）：结果分支写完成前按停止意图**或行上期限**原子决定去向——监控发停与围栏检查的交错不再漏网，过期正文只审计不发布。
  - **投递认领一致性**（#8）：Dispatch 采纳 pending 原行标识、复核/预检/建单拒绝释放认领（不占键）、回执落实际成功 route_id 并 upsert 自愈——换绑拒绝后换路由同键重投的首次响应与重放/按键查询读到同一份。
  - **v30 遗留恢复**（#11/#12）：运行过候选版 b59a395 的库升级后，遗留 pending 续接认领可恢复——版本恰 expected+1 且内容与载荷等值（SameContent）= 修订确已提交 → 完结回执；不等值冲突透传不宣告成功、不删除可能已提交的键；reason 空列回落命令值。
  - **前端续接模式重写**（#1/#2/#6）：目标/版本/完整草稿（含 constraints/delegation 基准——全量替换不清空不可编辑字段）在详情请求成功后**同请求原子生效**；切换来源作废旧请求（代次）；加载/失败禁用提交并提供重试；版本基准 = 预填同一详情响应（列表陈旧不再反复 409）；真并发冲突提供显式重新加载。
  - **摘要墓碑**（#13）：`ListMissions` 摘要与详情对房间来源给出同一 `deleted_at` 墓碑事实。

- **0.6.13 独立审校修复（17 项：5 P1 + 10 P2 + 2 P3，ADR-0036 v0.2）**：
  - **续接原子化**：`dispatch.Update` 在 SQLite 上以**单写事务**完成认领 + 复核（事务内身份门——owner 在册且 active）+ 修订 + 回执（`UpdateMissionAtomic`；内存件分步退化 + 拒绝回滚补偿）——修订已生效而回执失败不再留下"同键重试 409"的裂口（审校 #3）；领域拒绝/身份门失败整体回滚**不占幂等键**，修正后同键重试成功（#11）；owner 状态读取失败即错、未指派/未在册/非 active 明确拒绝（#12）；恢复重驱动命中 pending 采纳**原回执行**——与重放同 receipt_id（#13）；`reason` 随回执持久化并读回、目标引用携修订后版本 `mission:<id>:v<n>`（#16）；入口字段校验 400 先于认领（#17）；公开回执查询只见已提交行（pending 不可见）。
  - **Slot 派发链补强**：解析事实（实例 + 任职）作为决策事实另存、**原请求摘要**稳定——换绑后重解析同一请求同键可重放已提交回执（#10）；建单在**同一写事务**内核对实例/任职/身份/授权四项——换绑、暂停、撤权不得插进复核与建单之间（#2/#7，`CreateMissionSlot` + `mission.ErrSlotIntentViolated` → 409 route_stale）；解析所得实例落 `missions.slot_instance_id` 创建事实，执行装配按实例固定 Binding——**同人双任职不再串用**（#1，`Bindings.ActiveOfInstance`）；预检补 owner active 与授权准入（blocked `agent_not_active` / `slot_grant_missing`，#7）；契约对齐：`selection_source` 补 `slot_binding`/`explicit_owner`、reason_code 补 slot 族、决策回包携 `resolved_slot_instance_id`/`resolved_agent_id`（#9）。
  - **执行预算收口**：墙钟监控改**独立 ticker** + 按 **RunID** 直达停止——不被同步执行器阻塞（审校 #4：阻塞执行器跨多个扫描周期 Run 仍 running），同 Mission 换发的新 Run 未到期不误伤；启动前经 `RefOf` 重读**权威预算**（手动与自动入口共用——#5/#8：手动两次 POST 达限 409 `budget_exhausted` + deadline 落快照；排队期修订收紧/放宽即时生效；预算查询失败中止派发）；`max_runs` 核对移入 `CreateRun` 建行事务（并发孪生恰一胜者，#14）；期限比较改**时刻**——RFC3339Nano 小数位数不定，字典序 `.11Z < .1Z` 会漏已过期行/选中未来行（双存储同修，#15）。
  - **任务卡续接入口**（#6）：`MissionSummary` += `room_source`；已交办任务卡在 MissionsPage 表单切"追加要求"模式，经 `/v1/dispatch/update` 续接（稳定幂等键 + 现行契约预填），不再撞 createMission 的 409 `source_already_mapped`。
  - 换绑回归补强：`TestDispatchSlotStaleRereresolve` 重写为未映射任务卡起步（先断言 resolved 再换绑验 409——原测试第二任务卡预先被占用，删除复核分支仍侥幸通过）。

## [0.6.12] - 2026-11-03

### Fixed

- 修复 0.6.12 三审发现（独立三审报告"1 项 P2、2 项 P3"，ADR-0035 v0.4）：**(P2) 首轮轮询使初次加载失效**——轮询与载入共用取号导致初次挂载的详情/版本/事件三结果被作废（重复轮询挂起时详情停在"契约加载中"）。改为取号源各自独立 + detail 应用层共用单调代次（载入响应时取号——三结果原子落地且必覆盖先前轮询快照；轮询迟到旧响应仍被拦截）。**(P3) 新 offer 资料未就绪仍显示旧说明**——交接历史资料记对应发起（histFor），刷新中/失败窗口明确标识资料状态、不显示旧 offer 说明。**(P3) `expected_owner_agent_id` 传 null 返回 200**——presence 检查同拒 null（显式 `""` 才表达预期 unassigned）。

- 修复 0.6.12 复审发现（独立复审报告"1 项 P1、3 项 P2、1 项 P3"，ADR-0035 v0.3）：**(#1 P1) 旧草稿覆盖新归属与契约**——修订表单打開時固定基准（所见 owner + 版本），提交锚取基准值；轮询只保鲜显示，旧草稿对不上当前基准时 409 可见（不再以提交时点轮询值放行）。**(#2 P2) 事务前快照仍影响事实**——offer 事件 `from_owner` 由事务内注入；accept 的 draft 完备性重判移入事务内按现行 content + 新 owner 自算（并发把状态带到 draft 时完整任务不再被放回 paused）。**(#3 P2) 旧轮询响应覆盖新详情**——手动加载与轮询统一请求代次（同一计数取号、只应用更新者；删除清零路径）。**(#4 P2) 同人改派后说明与操作指向不同 offer**——交接历史按 `offer_id` 刷新、备注草稿按 offer_id 隔离。**(#5 P3) `expected_owner_agent_id` 缺席未拒**——presence 在场检查，缺席 400 `bad_request`（空串 = 预期 unassigned 不受影响）。

- 修复 0.6.12 审校发现（独立审校报告"3 项 P1、3 项 P2、1 项 P3"，ADR-0035 v0.2）：**(#1 P1) 旧交接操作会处理新的交接**——承接/婉拒/撤回只带 expected_version，撤回改派后（版本不递增）旧请求仍成功。发起生成 `offer_id`（`hdo_` 前缀；行存 `pending_offer_id` + 事件回链），三个了结动作**必填** `offer_id` 锚，服务层与存储事务内双校验。**(#2 P1) 旧修订能撤销已完成的责任转移**——承接翻转 owner 后契约仍 v1，旧表单（A/v1）保存成功把 owner 改回 A。revise 请求**必填** `expected_owner_agent_id`（归属锚，事务内核对）——不匹配 409 `owner_mismatch`（契约收紧：个人版单客户端生态，SPA 同步携带）。**(#3 P1) 承接与退役并发留下 retired 负责人的未完成任务**（双向竞态）——承接事务内直查 agents 表复核承接人 active；retiring→retired 收尾改走 RetireAgent 统一过**未处置承诺事务门**（原 TransitionAgent 旁路无门）；门拦下零写入、服务层重查清单停留 retiring。**(#4 P2) 承接回执用陈旧快照**——`status_at_accept` 改由事务内核对后的实际状态注入（并发暂停/恢复交错不进回执）。**(#5 P2) 交接/重开历史重试按钮不起作用**——改独立刷新计数触发重查（重开区为 0.6.11 遗留同病，一并修）。**(#6 P2) 详情页不同步外部交接变化**——5s 轮询刷新整个 detail（owner/pending/待承接入口可见），表单草稿不受影响。**(#7 P3) 超限请求返回 400 应为 413**——MaxBytesError 单独识别 → 413 `body_too_large`（交接 + 重开两处）。

### Added

- **Mission 责任交接：`handover` 与退役承诺处置**（0.6 领域第十三件·收官 / ADR-0035，RFC-0018 §3.3/§4.5/§8.1/§11.2，AT-03）：Mission 换负责人的受控通道——两段式 **发起 → 承接 | 婉拒 | 撤回**：发起只置 `pending_owner_agent_id`（owner 不变——责任不因单方意愿转移；同一时刻至多一条在途交接）；承接单事务翻转 owner + **承接回执**（`handover_accepted` 事件：from / to / contract_version / status_at_accept / note）；婉拒 / 撤回了结在途交接并留痕（"显式无人承接"可审计）。四命令均不递增 `contract_version`（归属是控制面事实），事件 append-only（交接史不可改写）。承接人须存在且 **active**（suspended / retiring / retired 不接新承诺——offer 与 accept 两点复核）。**在途交接冻结窗**：pending 期间 Revise / RequestReview / MarkCanceling 拒绝（先了结交接——防修订通道整包换 owner 与交接通道竞态；存储事务内同守卫），Pause / Resume / ReopenRequest / 成果提交照常。**完备性重判**：unassigned draft 承接后契约齐备 → ready 入调度列（交接同时是存量任务的指派通道）；running / waiting / paused 执行控制投影保持（结果事实与 owner 无关）。**退役承诺门实装**（0.5.1 预留的 `personalagent.CommitmentChecker` 首次接生产装配）：非终态 owned Mission = 未处置承诺（含在途交接——回执未落责任未转移）；retire 停留 retiring + 返回清单，逐一交接承接或取消后清空 → retired（AT-03"未处置时退役不能完成"兑现）。
  - **`POST /v1/missions/{id}/handover`**（发起）/ **`handover/accept`**（承接）/ **`handover/decline`**（婉拒）/ **`handover/revoke`**（撤回）：写门三件套 + MaxBytesReader（1 MiB）+ DisallowUnknownFields；**`GET /v1/missions/{id}/handovers`** 交接史读面（含承接回执）。`MissionView` += `pending_owner_agent_id`；`MissionEvent.event_type` 枚举补登 `reopen_requested` 与四类 `handover_*`。
  - **migration v29**：`missions` += `pending_owner_agent_id`（幂等加列 + 登记）；Store 三方法（`HandoverOffer` / `HandoverAccept`——pending 锚防撤回改派后的旧 accept / `HandoverWithdraw`）事务内核对；`mission.AgentLookup` += `AgentStatus`（身份域适配）。
  - **SPA**：任务详情「责任交接」区（发起表单——承接人选择 + 交接说明 ≤2000 字；待承接徽标 + 承接 / 婉拒 / 撤回；交接历史含回执）；详情头 `⇄ 待承接 → B` 徽标；设置页退役未处置承诺清单（既有面承接新数据源）。
  - 非目标（明确不做）：Portable Agent Bundle / 跨实例导出导入（0.7+ / RFC-0019）；WorkItem offer / accept / decline；Binding 换 Agent 交接包；自动暂停在途 Run；自动撮合承接人；交接回房卡片；多人 per-agent 身份校验（个人版 owner 代行，事件记 from / to 事实）。

## [0.6.11] - 2026-11-03

### Added

- **决策重开请求：`reopen_request`**（0.6 领域第十二件 / ADR-0034，RFC-0018 §10.2/§11.2）：触及决策的 falsifier 出现时，负责人提交重开请求——标出受影响决定（`decision_ref`）、证据与需停止的工作项；事实先落（`reopen_requested` 事件——append-only 不改写旧回执），逐项暂停受影响任务（`paused | already_paused | terminal | not_found | failed` 如实报告，失败不回滚请求），交回 Room 复用回房通知（`kind=reopen`——暂停房保留 / 删房待决 / 重启重放全继承）。不自动重开 Room、不翻转 Mission 终态（任何状态可发起，含 accepted）。
  - **`POST /v1/missions/{id}/reopen-request`**：CAS 版本（任意状态）+ `reopen_requested` 事件 + 逐项暂停 + 通知行；`GET /v1/missions/{id}/reopen-requests` 读面（任务中心待决可见性）。
  - **migration v28**：`mission_return_notices` += `decision_ref` / `affected_count` 快照列；投影器扩扫 `reopen_requested` 事件——卡片正文 `🔁 决策重开请求：「{决定引用}」——发起任务「{目标}」：{原因}（受影响 n 项）`，`mission_ref` += `kind: reopen` 与 `decision_ref`。
  - **SPA**：任务详情「决策重开」区（提交表单——决定引用 / 证据 / 受影响项 / 原因 + 历史列表）；房间时间线 reopen 徽标 + 决定引用。

### Fixed

- 修复 0.6.11 二审发现（独立复审报告"5 项 P2"，ADR-0034 v0.3）：**(#1) 版本冲突被误报为已暂停**——already_paused 分支的版本冲突重读重试一次，仍冲突如实返回 failed。**(#2) 事务提交后断线丢暂停**——Service 层在 store 事务成功后零值返回（暂停步骤不依赖读取回值）。**(#3) `not_pausable` 枚举缺失**——补 OpenAPI 并再生成。**(#4) 历史首次加载失败静默**——错误状态可见 + 原地重试。**(#5) 请求校验不完整**——MaxBytesReader（1 MiB）+ DisallowUnknownFields。

- 修复 0.6.11 审校发现（独立审校报告"1 项 P1、5 项 P2、1 项 P3"，ADR-0034 v0.2）：**(P1) 重开接口绕过写权限检查**——SubmitReopenRequest 补 guardWrite（Origin / Content-Type / X-Owner-Token 三件套 + 请求大小限制）。**(#2) 断线后暂停步骤丢失**——事实落地后暂停改用独立上下文（请求断线不丢暂停）。**(#3) 已暂停目标重试不重发停止信号**——already_paused 仍过控制面（幂等重发 SignalStopByMission）。**(#4) 非终态被报为 terminal**——draft/reviewing/canceling 明确报 `not_pausable`（terminal 保留给 accepted/canceled）。**(#5/#6) SPA**——重开历史乱序守卫 + 失败保留旧值 + 暂停失败原因 detail 展示。**(#7) v28 迁移登记遗漏**——补 INSERT OR IGNORE。

## [0.6.10] - 2026-11-02

### Added

- **执行结果回房：返回通知**（0.6 领域第十一件 / ADR-0033，RFC-0018 §10.1/§10.2、AT-09/AT-20）：Mission 结果先持久化、再通知来源房间——讨论里交办的事在讨论里看到结果。
  - **通知源（ADR 表 1）**：四类 `status_changed` 事实——`reviewing`（已提请验收）/ `accepted`（验收通过）/ `ready`+`returned`（验收退回）/ `canceled`（终局取消）；Run 级失败与 waiting 不回房（任务中心可见）；非房间交办的任务零通知（RFC §11.3）。
  - **投影（migration v25）**：`mission_events` 全局游标派生持久通知行（`mission_return_notices`——`source_seq` UNIQUE 即去重键；卡片所需字段在事实落地那一刻快照定格，AT-09 不变状态不反复通知）；崩溃重启从游标重放，不漏不重。
  - **原子投递**：同一 SQLite 事务内"墓碑/暂停核对 + 追加 `message.posted`（actor `par_system` + `mission_ref` 封闭对象）+ 标记 delivered"——卡与标记同落，**去重由构造保证**；复用房间 outbox/FTS 同事务路径，SSE 与引擎照常消费（系统卡不触发讨论波——引擎刺激只认 human，"不为回传自动恢复讨论"）。
  - **暂停保留 / 删除取消 / 有界重试**：房间暂停 → `held`（不计次、+5s 复查，恢复后发布；同房按 `source_seq` 保序——reviewing 不会落在 accepted 之后）；房间删除 → `DeleteRoom` 级联取消待投 + `mission_sources.deleted_at` **来源墓碑**（结果与映射保留、不重跑任务——AT-20；投递侧再核墓碑兜底旧库与竞态）；其余错误指数退避（≤60s）12 次封顶 → `failed`（last_error 留痕，交付/验收事实不受影响）。
  - **读面与契约**：`GET /v1/missions/{id}/return-notices`（每条来源事件恰一行——持久与去重可审计）；`MissionView.room_source` += `deleted_at`；`message.posted` 载荷 += `mission_ref`（系统 actor 专用，`post_message` 命令面拒收——人类不能伪造结果卡）。
  - **SPA**：房间时间线把系统消息渲染为**引用卡片**（kind 徽标 + 任务卡 + 轮次，点击直达 `/missions?id=` 选中该任务）；任务中心详情「回房」区（状态徽章 / 房间事件 ID / 失败原因，5s 轮询）与来源房间已删除提示。

### Fixed

- 修复控制停止与执行器取消同就绪的 select 竞态（CI macos-latest 实证，0.6.6 暂停/取消路径）：停止取消执行上下文后执行器即刻返回 context canceled，select 随机选中结果分支即把停止记成 failed——执行器错误路径先查停止意图（非空 cause），有意图按停止终理化（Run canceled + stop_requested 审计）。

- 修复 0.6.10 四审发现（独立复审报告"3 项 P2 产品回归、1 项历史配对、1 项测试缺口"及两项既有补正，ADR-0033 v0.4）：**(#1) 删房提示被旧响应抹掉**——来源成功响应单设已应用序号，乱序返回后"来源房间已删除"保持可见；**(#5) 旧失败覆盖新成功**——台账失败忽略早于最新已显示成功的响应。**(#2) 升级后同包轮次错位**（v22→v24 得 3/1、4/2）——送验缺轮次按本次送验区间的显式裁定校准，纯 v24 与 v22 纯旧式（计数）不变。**(#3) 取消沿用上次送验轮次**——取消恒第 0 轮（mission_ref / 卡片 / 台账三面一致）。**(#4) 回归测试缺口**——改确定性同步屏障（先登记停止再放行结果），旧实现 30/30 失败、新实现 30/30 通过。**（既有）老式退回事件漏卡**——classify 识别 `to=returned`（0.6.9 初版形态，实测 v22 历史 3 条漏卡）；migration v27 首次重置投影游标幂等补投（已投行不动）。

- 修复 0.6.10 复审发现（独立复审报告"2 项 P1、3 项 P2、2 项 P3"，ADR-0033 v0.3）：**(#1 P1) 停止后仍会发布成功结果**（基线即有）——停止意图在场时执行结果一律按停止终理化，迟到正文只进 `late_result` 审计、不写 `result_block`；**(#2 P1) 取消后租约滞留**（上轮竞态修复引入）——已取出的执行结果放回缓冲通道交收尾，审计、释放恰一次，不再空等 `stopGrace` 到租约 TTL。**(#5) 裁定轮次错位**——v22 两次送验退回升级后再送验时同包显示"送验第 3 轮、退回第 1 轮"：裁定轮次按同包（缺省最近）先行送验还原。**(#6) BOM 目标回退**——版本行查到即保留空摘要，不回退修订后目标。**(#7) 回填时间失真**——投递侧来源墓碑优先取房间实际删除时间。**(#3/#4) 台账轮询**——台账与来源两请求各自独立更新（互不阻塞）；响应只丢弃比已显示更旧者，连续慢响应不再被全部丢弃。

- 修复 0.6.10 审校发现（独立审校报告"2 项 P1、6 项 P2、1 项 P3"，ADR-0033 v0.2）：**(#1 P1) 旧事实被记到修订后的契约**——投影按来源事件时点还原当时生效的契约版本与目标（新事件的送验/裁定事务内写入实际 `contract_version` / `review_round`；旧事件按事件流回溯 `created` / `contract_revised` + 版本行 objective 快照 + 送验计数重建轮次），不再读 `missions` 当前行——v1 送验退回后修订 v2 的重放不会把旧包标成 v2、显示 v2 目标。**(#2 P1) 同房通知可越过暂停/退避中的队首**——Due 先取各房**队首**未完成行再判到期：队首复查/退避中同房后继不越序（恢复后按 reviewing → accepted 有序落卡），每房每轮一条，暂停房积压不再占满批次饿死他房（#6 同修）。**(#5) 送验通知丢失轮次**——轮次随送验事务写入事件（历史事件按送验计数重建），台账与房间卡显示真实轮次。**(#8) 升级遗漏已删房的来源墓碑**——migration v26 按 `room_tombstones` 原删除时间幂等回填 `mission_sources.deleted_at`；投递侧发现删除时兜底回填。**(#3) 刷新后回房卡失去任务链接**——快照与实时两路同形保留 `mission_ref`（room.TimelineItem + SPA）。**(#7) 系统卡抢占讨论锚点**——反应锚点跳过 system 消息。**(#4/#9) 台账轮询误报与删房提示滞后**——序号守卫丢弃过期响应、失败保留末次成功结果（空态/错误态分离），轮询顺带刷新来源元数据。

## [0.6.9] - 2026-11-02

### Added

- **Mission 验收：`request_review` 与 `review_mission`**（0.6 领域第十件 / ADR-0032，RFC-0018 §3.2/§10.2/§11.2、AT-10）：三事实中的第三件——**验收通过**。Mission 首次拥有"完成"终态。
  - **`reviewing` 验收门 + `accepted` 终态**（migration v22：missions 增 `review_package_id` 锚列）：`ready → reviewing → accepted | ready`（退回）。送验入口为 ready（RFC 表的 running 为 WorkItem 连续执行模型——本版执行投影在 Run 终态后回 ready，语义等价）；waiting/paused/draft/running 不可送验。
  - **`POST /v1/missions/{id}/request-review`**（submit_result 的对面）：以 `package_id` 为锚——包存在/属该 Mission/对应**当前**契约版本（陈旧 409 stale_package——须补证新包）；**必需交付项检查**（AT-10）：成果引用非空 + 当前每条验收标准在包的 acceptance_mapping 中有对应条目且证据非空，缺项 409 missing_required_items **携缺项清单**（补证后重试——"先交付再补证"闭环）；CAS ready → reviewing + review_package_id。幂等同包 200；reviewing 中换包 409（先退回）。
  - **`POST /v1/missions/{id}/review`**（裁定）：`decision ∈ accepted | returned`——accepted 为**终态**（修订/取消/暂停/送验/再交付/执行全拒绝，锚保留以追溯"按哪个包验收"）；returned → ready（锚清空——可再执行、补证再交付、再送验）。**验收权限 = 契约一致性校验**：契约 reviewer 非空时 body.reviewer 须相等（403 review_not_authorized；空 = 请求人自验）；returned 须携 reason；item_notes 逐项依据与退回原因留痕于 status_changed 事件（decision/reviewer/reason/package_id/item_notes）。
  - **验收期间契约冻结**：reviewing 下 `revise`（验收基线不得变动）、`pause`、交付提交（409 mission_reviewing——受审包已固定）均拒绝；**取消两段照常**（非终态皆可取消，ADR-0029）。delivery 写包守卫（存储事务内）扩 reviewing / accepted 两拒绝码。
  - **`review_package_id` 追踪锚**（AT-10"任务卡、产物、决定可互相追踪"）：Mission → 交付包 → 来源 Run → 事件流直达，无需翻事件流重建。
  - **SPA**：ready + 有包 → 「提请验收」（缺项 409 回显清单）；reviewing → 受审包徽章 + 验收面板（接受/退回 + 原因 + 验收人预填）；accepted → 终态徽章、控制区与交付表单只读。
  - 契约面：两端点 + MissionReviewRequest / MissionReviewDecision / ReviewItemNote；状态枚举 += reviewing, accepted；MissionView += review_package_id。

### Fixed

- 修复 MiniMax 原生 `<deliver-assets><media …/></deliver-assets>` 被当作消息正文或附件解析错误展示：适配器现分离正文与最多 4 个工作区产物，发布围栏通过后按 Profile 将文件安全导入既有受管附件库，消息显示可下载附件及 caption，不再泄漏内部 XML。Native 与 WSL 均只读取当前 Agent 工作区内的普通文件，拒绝路径穿越、符号链接逃逸和非普通文件；WSL 使用原始二进制流，保留既有 8 MiB / 房间配额 / MIME / SHA-256 校验与房间删除级联。单件失败保留正文并明确标记附件不可用，不伪装导入成功。

- 修复 0.6.9 三审发现（独立复审报告"4 项 P2"，ADR-0032 v0.4）：**(#1) 修订后加载窗口仍可发送旧包**——契约版本切换/送验列表重载开始即清选择与列表（加载失败同样清空），发送前再核所选包契约版本。**(#2) 受审包加载失败后无重试**——裁定面板原地「重试」入口（不必退出任务详情重进）。**(#4) 升级库在审包的匹配重试被证据口径拒**——同版本同包的在审重试**先行走幂等路径**返回当前 reviewing（200），必需项检查只对新一轮送验生效（v22/v23 已在审的纯空白证据包不再 409）。**(#5) 终态处置测试缺口**——先在 ready 期成功创建 Run 再转 accepted，断言 `ErrMissionTerminal`（Run 不存在的"通过"不算数）。

- 修复 0.6.9 复审发现（独立复审报告"2 项 P1、8 项 P2、2 项 P3"，ADR-0032 v0.3）：**(R1 P1) 送验后插入未决 Run、恢复后仍能接受**——CreateRun 写事务内核对任务仍是 ready|running（送验/暂停/取消/waiting/accepted 落地后的过期调度拒绝，调用方释放租约；SQLite 与 MemStore 同口径）；接受（RecordReview → accepted）事务内复查未决 Run（v22 升级残留形态同样拦截）。**(R2 P1) v22 已在验收任务升级后卡死**——migration v24 幂等补正：reviewing/accepted 轮次 0 → 1（恢复可裁定），canceled 行清受审包引用（R9 同修）。**(R10) 预填未来轮次能接受 B 却把事件记作 A**——裁定事务核对**包锚**（调用方预读锚与库内不符即拒绝），事件按核对后的实际锚写入。**(R5/R7/R8) 终态迟到写入**——execctx 新建清单/回执、Run 处置、租约领取分别在各自写事务内核对终态（预读 reviewing 后接受落地的交错写入不再落库；既有查询照常）。**(R6) BOM/NEL 判空前后端不一**——统一为 Unicode White_Space ∪ U+FEFF（Go blankTrimmed / SPA blankTrim 同集）。**(R12) 退回/取消后公开轮次**——视图仅 reviewing/accepted 暴露轮次（其余 0；库内单调计数不受影响）。**(R3/R4/R11) SPA**——送验包列表随提交即时刷新且只在当前契约版本内选择（修订后不再发出已隐藏旧包）；受审包加载失败/切换期间裁定禁用（证据与展示一致才可裁定）；accepted 任务装配卡只读。

- 修复 0.6.9 审校发现（独立审校报告"3 项 P1、12 项 P2"，ADR-0032 v0.2）：**(1) P1 旧裁定落到下一轮**——裁定请求未携送验轮次，读过 A 的接受请求可在 A 退回、B 送审后提交（任务锚为 B、事件指向 A）；无并发的重放也能用 A 的理由退回 B。新增 migration v23 `missions.review_round`（每次送验**单调递增**——同包重审也递增），裁定请求必填 `review_round`，SQLite/MemStore 裁定事务内核对**轮次 + 包 + 契约版本 + 状态**，旧轮次一律 409。**(2) P1 纯空白证据可通过 AT-10**——送验按 `TrimSpace` 判证据非空（空格/制表同罪），缺项 409 携标准清单。**(3) P1 未核验 Run 随任务被接受**——Run 落库与 running 投影间隙崩溃会残留 ready + 未处置 unknown；送验前置 + **送验事务内**复核在途/未处置 Run（409 runs_unsettled），ready 残留恢复同样进 waiting，accepted/canceled 终态封处置入口（409 mission_terminal）。**(4)–(16) P2**：并发送验同包幂等（不再误报 409）；expected_version 预读校验（与修订交错时旧包不能按新契约通过）；退回事件 to 为真实状态 ready；两段取消清受审包锚；验收人 >64 字拒绝；accepted 后修订/取消按钮隐藏、新装配与租约拒领（409 mission_accepted，历史读取照常）；SPA 送验改为选包（较早完整包可选）、裁定预填契约验收人并可填逐项备注、裁定草稿按轮次隔离（退回原因不再串入新一轮）、受审包逐项证据在裁定旁可核对（accepted 后保留入口）。

## [0.6.8] - 2026-11-02

### Added

- **DeliveryPackage 成果提交：`submit_result` 不自动送验**（0.6 领域第九件 / ADR-0031，RFC-0018 §3.3/§10.2/§11.2、AT-10）：兑现"执行成功 / 成果提交 / 验收通过是三个互不替代的事实"中的第二个——结构化交付包的提交、留痕与读面，对 Mission 状态**零副作用**。
  - **DeliveryPackage 域**（internal/delivery，migration v21 一表）：dlv_ 前缀、溯源来源 Run（run_id / attempt / generation）与契约版本；RFC §10.2 最小结构——成果引用（引用非正文）、验收逐项对应 [{criterion, evidence}]（criterion 为契约版本内条文快照，evidence 可空——补证路径）、验证证据、未解决项、风险、动作回执（本版引用串）、建议记忆变更（随 0.7 消费）、备注。append-only：每次提交新建一行（补证 = 新包，旧包不改写）；"当前交付包" = submitted_at 最新。
  - **提交面** `POST /v1/missions/{id}/delivery-packages`：来源 Run 须可交付（`completed`，或 `unknown` 且处置 `verified_completed`——ADR-0030 用户 attest 的完成；failed/canceled/在途/未处置 409 run_not_deliverable）；`generation` 须等于 Run 实际代次（409 stale_generation——调用方视图陈旧，拒绝而非静默改写）；`expected_version` 须等于 Mission 当前契约版本（409 version_conflict——验收对应锚定契约版本）；Run 须属该 Mission（409 run_mission_mismatch）；canceled Mission 拒绝（409 mission_canceled，读面照常）。**完备性不在提交面检查**（先交付再补证——缺必需项由 0.6.9 request_review 拒绝）。
  - **不自动送验**：提交不改 Mission 状态/版本/updated_at、不写 mission_events、不产生通知、不触发验收——ready/running/waiting/paused 各态均可提交（交付是对已完成 Run 的事实陈述，与控制态正交）；端口上无任何 Mission 变更方法（结构性保证）。提交回执落来源 Run 事件流 `delivery_submitted{package_id}`（事件追加不改行）。
  - **读面**：`GET /v1/missions/{id}/delivery-packages`（倒序——首项即当前）、`GET /v1/delivery-packages/{package_id}`（供 0.6.9 `delivery_package_ref` 寻址）。
  - **校验单源**：artifact_refs ≤16×256、acceptance_mapping ≤16（criterion 1–256 / evidence ≤1000）、verification_evidence ≤4000、open_items ≤16×256、risks ≤8×256、action_receipts ≤16×128、suggested_memory ≤8×256、notes ≤1000；空切片往返 `[]`（400 invalid_delivery）。
  - **契约面**：三端点 + DeliveryPackageSubmit / DeliveryPackageView / AcceptanceMappingEntry；MissionRunEvent 事件枚举 += delivery_submitted。
  - **SPA**：任务详情「交付」区——当前交付包摘要（来源 Run、验收对应有证据 n/m、未解决/风险计数）、历史提交折叠、提交表单（选可交付 Run、逐条验收证据、成果引用/验证证据/未解决项/风险/备注）；明示"已提交成果——尚未送验（验收随 0.6.9）"。

### Fixed

- 修复 0.6.8 审校发现（独立审校报告"1 项 P1、3 项 P2"，ADR-0031 v0.2）：**(1) P1 并发修订或取消后仍能提交**——服务层先读 Mission 再独立写包，两步之间契约被修订或 Mission 被取消时旧请求仍成功；改为存储层 `CreatePackage` 在写包的同一事务内重读 missions 行，契约版本不符 / 已取消 / 不存在分别以 version_conflict / mission_canceled / mission_not_found 拒绝、整体不落（服务层预读只作早拒）。**(2) P2 包已保存、审计回执却永久缺失**——回执用请求上下文另写，请求中断即"包落了回执丢"且响应不带包标识、重试只为新包留痕；改为回执随包在同一事务写入来源 Run 流（Runs 端口退回只读），无部分成功、重试恰一包一回执。**(3) P2 较旧交付包可能被显示为当前包**——列表按 submitted_at 字符串排序，RFC3339Nano 小数位可变使 `.1Z` 排在更晚的 `.11Z` 之前；两存储改按解析后的真实时间倒序（同刻按插入序），补不同小数位回归用例。**(4) P2 首次执行完成后仍无法交付**——交付卡只在挂载或成功提交后刷新 Run 列表，执行卡轮询完成/核验完成不更新资格列表；加 10s 轮询 + 详情变化即刻重载，只刷新包/Run 列表、表单草稿不受影响。

## [0.6.7] - 2026-11-02

### Fixed

- 0.6.7 发布前审校补齐（对 6107d83 基线四项缺口，语义在其 ADR-0030 之内）：**(1) 停止超窗租约（0.6.6 审校遗留 P1）**——finalizeStopped 宽限超窗后仍无条件释放租约，执行器未退出时新执行可同写工作区；改持有租约 + 后台等执行器退出（先补迟到审计再释放；跨进程由租约 TTL 兜底——独占性优先）。**(2) 处置投影吞错**——ResolveRun 的 ClearWaiting 失败被吞：Mission 卡 waiting 无感知，且同结论幂等重试短路不再补投影（双死角）；改上抛（事实说明）+ 幂等分支重试投影。**(3) 字节截断**——truncateS 按字节切中文产出无效 UTF-8 且提前丢内容（8000 字节 ≈ 2666 字）；改按 rune 计（ASCII 快路径不变）。**(4) 旧库升级残留**——v0.6.5/0.6.6 的"failed+结果未知"终态行升级后无人认领（Mission 卡 running 无出口；ready 形态更被调度器自动重跑——禁盲目重跑洞）；恢复面新增升级收敛：行转 unknown（`unknown{upgrade}` 事件，历史不抹）+ Mission running 形态进 waiting、ready 形态由**执行面围栏**（`residue_unresolved` 409）挡——处置后照常放行。定向先红后绿 + SQLite IT + 真机升级冒烟（双形态收敛/围栏/处置放行，事件链 `attempt_requested → unknown → resolved` 精确）。
- mcode 更新修复（负责人反馈实证 2026-11-01）：mcode 0.6.1 的 `update` 子命令需交互确认——Mosaic 无终端环境下它打印 npm 指引即退出 0，任务呈"已更新"而版本未动（静默假象）。minimax 家族由 self-update 改归 **registry npm 直装**，安装参数按 mcode 自荐命令钉死（`--include=optional` 平台二进制 + `--ignore-scripts=false --allow-scripts=@minimax-ai/code,better-sqlite3` 构建脚本——缺参即静默残缺安装，codex 同类 optional 缺失先例）；真机实证 0.6.1 → 0.7.4。并加**无变化防线**：更新退出 0 但版本回探与更新前相同时，过程日志如实提示"版本未变化——已是最新或更新未生效"（kimi 幂等无操作同面，文案中性）。
- zcode 检测与更新面修复（负责人反馈"没有更新按钮"，三处）：**(1)** 检测基线改取 **App bundle 版本**（Info.plist CFBundleShortVersionString，darwin）——登记的内核号 0.16.9 与 feed/tag 的应用版本方案（3.x）跨系不可比，恒"不可比不判更新"；基线改 App 版本后对照有意义（真机：3.17.1 vs 源码 tag 3.17.1 → 已是最新）。**(2)** UI 无自动通道的家族此前整行隐藏更新按钮——改为呈现**禁用态按钮 + 指引**（官方桌面版应用内自更新；自建打包可放置 `~/.mosaic/update/zcode.sh` 接入 Mosaic 驱动），note 随更新徽标同显。**(3)** `desktopFeedURL` 只按 Windows 安装根布局（exe 同级）找 `resources/app-update.yml`——macOS .app 内在 `Contents/` 下（上溯两级），mac 上恒落空回退源码 tag；双布局候选修正（真机实证 feed 已定位 http://localhost:8081，不可达如实回退并注明）。

### Added

- **Run 崩溃恢复：unknown、迟到结果及禁止盲目重跑**（0.6 领域第八件 / ADR-0030，RFC-0018 §3.2/§11.3/§11.5、AT-12）：兑现 RFC 的保守 unknown 语义——结果不可判定即 unknown、Mission 明示等待核验、用户显式处置后方可再执行、迟到结果只审计不发布。
  - **Run `unknown` 终态**：重启恢复时无停止意图（Mission running）的在途 Run → `unknown`（此前记为 failed 并附"结果未知"文本——把"不可判定"伪装成"已知失败"）；`unknown{recovery}` 事件；租约释放（进程已死）。`failed` 从此只表示已观测到的失败。canceling/paused 停止意图分派路径不变（0.6.6）。
  - **Mission `waiting` 态 + `waiting_reason`**（migration v20 加列）：running → waiting(reconciliation)——出调度列直至处置；等待原因是一等事实（任务中心展示"为什么不动"）；暂停保留等待原因、恢复据此回 waiting（暂停/恢复不绕过禁止盲目重跑）；取消两段照常；修订保持投影（存储事务内）。此前 Mission 永久滞留 running 且用户除取消外无出路。
  - **核验处置面** `POST /v1/mission-runs/{id}/resolve`（outcome ∈ verified_completed | verified_failed | discarded，note ≤500）：**Run 状态保持 unknown**（系统未观测到完成——处置是用户 attest，不冒充系统事实），处置事实落 resolution / resolution_note / resolved_at（migration v20 加列）+ `resolved` 事件（条件更新：state='unknown' AND resolution=''）；随后 Mission waiting → ready | draft（完备性重判，清等待原因）——再执行只能发生在显式处置之后。幂等同结论 200；异结论 409 run_already_resolved；非 unknown 409 run_not_unknown。
  - **迟到结果只审计**（AT-12）：停止后执行器结果在宽限内到达即记 `late_result` 事件（outcome/preview/generation/published=false/phase），宽限后由后台等待审计——Run 行不变、结果不落 result_block、不发布（此前经缓冲通道静默丢弃）。存储新增事件追加面 AppendRunEvents（终态行只增审计、不重写行）。
  - **可观测**：`GET /v1/scheduler/status` += `unresolved_unknown_runs`（RFC §11.5 记录 unknown 数，处置后归零）。
  - **契约面**：MissionView/MissionSummary `status` 枚举 += waiting；MissionView += waiting_reason；MissionRunView `state` 枚举 += unknown，+= resolution/resolution_note/resolved_at；RunResolve 请求体；MissionRunEvent 事件枚举补齐（含 0.6.6 漏登的 stop_requested/canceled）+ unknown/late_result/resolved。
  - **SPA**：任务详情 waiting 徽章与等待原因说明（不以通用"重试"掩盖）；执行区 unknown Run 行内处置控件（已达成 / 未达成 / 放弃 + 备注）与已处置徽章；暂停按钮含 waiting。
  - **迁移 v20**：runs 处置三列 + missions.waiting_reason——逐语句执行、重复加列容忍；v0.6.6 旧库自动升级，旧行处置列缺省空；历史 failed（结果未知）行不回溯改写。

## [0.6.6] - 2026-11-01

### Fixed

- 修复 0.6.6 审校发现（独立审校报告"4 项 P1、4 项 P2，暂不建议发布"，审校范围 `v0.6.5..5ed2b24`）：**(1) P1 收尾窗口起表过早**——清理上下文（30s）在执行开始时创建，任务时长超过窗口（31 秒实证）即完成时已耗尽：围栏检查报 context deadline exceeded（保守拒绝）、终态写入/租约释放全部随过期上下文静默失败——Run 滞留 running、租约滞留 active 至 TTL。窗口改自收尾时刻起算（`WithCleanupBudget` 可注入，前置失败路径统一走清理面）。**(2) P1 停止句柄注册过晚**——句柄在 SetRunning/围栏复核之后注册，其间落地的暂停（CAS 已翻转 paused）SignalStopByMission 扫描可见 Run 而注册表无句柄：信号丢失、执行照启、结果照落（暂停语义失守）。注册改先行于 Mission 投影（CreateRun 落库即注册），窗口内暂停要么信号命中取消执行上下文、要么 SetRunning changed=false 拒启动。**(3) P1 并发修订覆盖暂停/取消**——Revise 服务层先读后写，读到 ready 后暂停/取消落地（控制命令不递增 contract_version，CAS 版本面对该交错放行），next.Status=ready 把 paused/canceling 盖回 ready——Mission 重新入调度列、停止意图丢失。执行控制投影的保持改由存储事务内以现行状态为准（SQLite/MemStore 同口径，服务层投影降为先行非权威）。**(4) P1 停止路径租约早释放**——终态写入后立即释放租约，执行器 goroutine 可能仍在收尾写工作区，新执行随即领取同一工作区（独占失守）。释放改后于执行器退出（有界等待缺省 10s，`WithStopGrace` 可注入；超窗仍释放——租约滞留到 TTL 比短残余窗口更糟）。**(5) P2 停止终态写库失败被吞**——finalizeStopped 对 UpdateRun 失败静默返回，调用方拿"成功"而 Run 卡 running。改上抛 `ErrTerminalWrite`（Run 滞留 running + 租约滞留 active 事实对齐不释放，恢复面兜底收尾）。**(6) P2 暂停期间删除验收后恢复直通 ready**——Resume 无条件 paused → ready，缺验收契约直接入调度列。恢复改按当前契约完备性重判目标态（ready | draft；`ResumeMission` 存储签名携 target，draft 待补齐、修订补齐完备即回 ready）。**(7) P2 重启漏"Run 已终态、Mission 滞 canceling"收尾**——恢复面只随未终态 Run 走，"停止写库成功、进程在控制面 FinishCancel 前退出"的 Mission 无未终态 Run 从此无人兑现。恢复面补状态枚举兜底（`ListCanceling` 端口 + `ListMissionsByStatus` 存储，无未终态即 FinishCanceling 幂等收尾；paused 合法长驻不动）。**(8) P2 Web 飞书授权误判弹窗失败**——`window.open(url, "_blank", "noreferrer")` 按 MDN 返回值语义成功也返回 null，Web 形态授权页已正常弹出却被判"弹窗被拦截"：错误提示且轮询中断（自动绑定本可完成）。null 不再构成失败证据：桌面壳走系统浏览器兜底照旧，Web 形态按已打开继续（轮询兜底，提示兼顾未见弹出的允许弹窗重试路径）。前七项定向复现测试先行（先红后绿）；第八项经返回值语义确认。

### Added

- **Mission 暂停、恢复与取消**（0.6 领域第七件 / ADR-0029，RFC-0018 §3.2/§9.4/§11.2）：执行闭环的运行时控制面——执行一旦开始即可干预。
  - **暂停/恢复**：`POST /v1/missions/{id}/pause`（ready | running → paused，CAS + reason）——持久状态先行即**阻止新调度**（调度器只取 ready，paused 天然出列；对 RFC §3.2 的一处增列：ready 可暂停——0.6.5 持久调度器周期派发 ready，"暂停先阻止新调度"要求 ready 可暂停）；来自 running 时在途 Run 在安全边界停止（结果拒收 + 围栏失效——Run 终态 `canceled` + `stop_requested{pause}` 审计 + 租约释放），响应不等停止完成。`POST /v1/missions/{id}/resume`（paused → ready）——调度器下一轮自动拾取，不重启已停止的 Run（canceled 终态留档，新执行 attempt 递增）。幂等：已 paused/ready（版本匹配）返回当前；draft 不可暂停（从未被调度）。
  - **取消两段化**：`cancel` 端点扩义——非终态 → **canceling**（停止意图持久化先行，RFC §9.4"先持久化停止意图"）→ 停止在途 Run（有界等待缺省 10s，独立上下文——请求断开不丢终态翻转）→ **canceled**；无在途同步直达。超时返回 canceling（"待停止"可见——可重复 cancel 续驱；重启恢复分派兜底）——RFC §3.2"无法安全终止……显示待核验"。取消不抹去既有动作：已落库的 completed Run 与结果保留；canceled 不被迟到回调翻转。
  - **执行脱离请求上下文**（行为变更）：每 Run 独立 Background 派生取消上下文（停止注册表 runID → {cancel, cause}）——HTTP 请求断开不再中止执行（执行是持久事实，中止只经控制面或进程退出）。
  - **重启恢复按停止意图分派**：canceling → Run canceled（"停止请求在途时重启——结果未知"）+ 收尾兑现（→ canceled）；paused → Run canceled + 停 paused；其余照旧（failed 结果未知 + 停 running，禁止盲目重跑）。paused 跨重启持久且仍不入调度列。
  - **迟到结果不落**：被放弃的执行 goroutine 结果经缓冲通道丢弃（围栏已随租约释放失效——无第二写入者）。
  - **修订保持执行控制投影**：running/paused/canceling 中修订契约不回写 draft/ready（在途 Run 不静默换输入，停止/完成后下一 attempt 自然用新版本）——顺带修复 0.6.5 遗留投影缺陷（running 中修订会把状态覆写为 ready）。
  - **执行器指令传透**：目标（objective）作为 instruction 入执行 Inline——目标即执行指令（受控执行桩/真实 CLI 均可消费）。
  - **SPA**：任务详情控制区（暂停/恢复/取消/续驱停止按钮按状态呈现）+ paused/canceling 状态徽章与说明（停止进行中不掩盖、不通用重试）+ Run canceled 徽章。
  - 契约面：MissionView/MissionSummary `status` 枚举 += paused, canceling；MissionRunView `state` 枚举 += canceled；MissionPause/MissionResume 请求体；无 migration（状态值扩展）。

### Fixed

- 表格文档打开必崩修复（v0.3.0 回归）：DocEditorPage 在组件顶部对 spreadsheet 做条件提前 return（分派给 SheetEditorPage），doc 加载前首渲染走完整 hooks 路径、加载后提前 return——hook 数在渲染间变化触发 React「Rendered fewer hooks…accidental early return」，经文档列表点击或直链打开任何表格均白屏报错（数据无损；真机实证：空表同样崩溃，服务端三层 API 全部正常）。拆分为外层分派壳 + 内层 MarkdownEditor 组件（hooks 恒定）。
- zcode macOS 桌面渠道弹窗根除：无头运行的主二进制即便带 `ELECTRON_RUN_AS_NODE=1` 也会以 `dev.zcode.app` 主 bundle 注册 LaunchServices——Dock 里弹出"第二个 ZCode"应用实体、随任务出现/消失（真机实证：ASN 主 bundle 注册计数随任务振荡 8↔10，全程无新窗口）。改以内嵌 `ZCode Helper` 二进制承载同一 node 语义（与桌面版自身 zcode-cli 工人同机制，注册于 `dev.zcode.app.helper` 无 Dock 面）——适配器与 harness 探针两处同口径（从 exe 路径派生安装位——bundle 派生版对缓存副本路径静默失效，2026-10-08 复验抓出修正），Helper 缺失回退主二进制，Windows 不变；任务全链路验证模型回复正常、运行期解释器确为 Helper、主 bundle 注册零出现。
- 中文输入法 Enter 确认候选误触发送/提交（macOS WKWebView）：WebKit 在按 Enter 确认候选时先派发 compositionend、后派发 isComposing=false 的 keydown——单查事件内标志必然失效（Chrome/Edge 系组合中 isComposing=true 的路径本就正常，两内核都要拦）。新增 `useIMEGuard`（组合态跟踪 + 组合刚结束一拍守卫，setTimeout(0) 清除——事件派发先于定时器），接入全部 9 处 Enter 提交点（聊天发送/@提及候选、房间与文档重命名、文档/表格标题、房内搜索、云表格链接、表格公式栏/单元格/工作表名）。
- zcode 子进程残留与弹窗（macOS）：桌面 bundle 的插件宿主（`__zcode-plugin-host` 孙进程）不随主进程退出，残留宿主以 1-2 秒间隔反复重派 ZCode.app 子进程（真机实证）——进程组击杀此前只在超时/取消路径生效（cmd.Cancel），正常结束路径补齐 `killGroup`：Wait 返回后对残余组 SIGKILL（POSIX；Windows 无进程组语义保持不变）。真机验证：跑后无残留宿主、无再发爆发；运行期短命子进程实测均 headless（ELECTRON_RUN_AS_NODE=1 透传），高速连拍无窗口闪现。
- macOS 桌面壳首屏修复：WKWebView 不跟随自定义 scheme 资产桥上的 307（首屏渲染 "Temporary Redirect" 白页、需手点链接才落到回环源）——桥响应按平台拆分（`assetbridge_*.go`），darwin 落地立即 `location.replace` 跳转页（meta refresh/锚点兜底），Windows 保持 dogfood 验证过的 307 原路径不变。
- 桌面壳外链打开修复：WebView（WKWebView/WebView2）不处理 `window.open`，飞书 OAuth 授权页点击无响应——新增壳集成端点 `POST /v1/shell/open-external`（非 OpenAPI 契约、仅壳装配注册，服务端/Web 形态零变化；三层写门全量适用、仅放行 http/https），前端 `window.open` 返回 null 时兜底请壳调系统浏览器（darwin `open` / windows `rundll32`）。
- mcode 0.6 凭据布局适配：0.6.x 起登录凭据移至 `~/.minimax/auth/prod`（0.2.x 为 `~/.minimax/cli-auth`）——探针与 IT 登录门双面任一命中即 logged_in，升级 mcode 后不再误报未登录（真机实证 0.6.1 适配器契约三件套全绿）。
- 飞书多维表格导入修复：快照落格此前直接迭代记录字段 map（JSON 对象），Go map 迭代序随机致 SnapshotHash 两次读取恒漂移，导入执行恒报「预览已过期：远端内容已变化」（真机实证：无人编辑连续两次预览哈希不同——多维表格导入 100% 失败）——改为按字段列序落格恢复确定性，回归测试 20 连读哈希全等 + 行主序断言。
- kimi 2.x 适配（实证 2026-10-02，本机 0.39.1→2.1.1 升级）：默认模型切到 k3-256k，严格 JSON 契约任务首次观测输出形状漂移（意图评估 `public_rationale` 内嵌未转义直引号——`ExtractJSON` 报 `invalid character '婚'`，座位整轮弃权，桌面日志呈现为「意图评估失败，跳过该座」）——补上与 minimax v1.69/zcode 同构的一次矫正重试（原提示词 + CORRECTION 追记、`-S` 同会话续接让模型可见自己的跑偏输出；仍失败保留末次错误并附输出首行，排障不依赖 CLI 侧转录），v1.69 裁定「codex/kimi 未观测不加重试」至此对 kimi 失效（codex 仍未观测、维持不重试）；另实证 Mosaic 的后台 kimi 任务进程会参与用户 CLI 的升级 rollout 判定（`~/.kimi-code/updates/rollout.log`），子进程 env 恒置 `KIMI_CODE_NO_AUTO_UPDATE=1`/`KIMI_CLI_NO_AUTO_UPDATE=1`——后台实例不触碰用户 CLI 升级通道（桌面壳后台纪律：agent 进程零 UI 副作用）。
- zcode 会话库私有化隔离（macOS 实证 2026-10-06）：桌面端出现标题为 "ZCode" 的悬浮小窗被 Mosaic 任务激活弹出——根因不是 Mosaic 拉起 GUI 进程（任务全程 `ELECTRON_RUN_AS_NODE=1` 无头），而是 ZCode 桌面版的 CUA PiP 悬浮球由 GUI 派生的 zcode-cli 镜像进程驱动，该镜像盯默认会话库（`homedir()/.zcode/cli/db/db.sqlite`，bundle 内硬编码、`ZCODE_STORAGE_DIR` 管不到），Mosaic 任务（`--cwd` 项目绑定会话）写入共享库即被观测激活；修复走 bundle 官方 env 配置面 `ZCODE_SESSION_DB_PATH` → `storage.sessionDbPath`，把 Mosaic 全部 zcode 子进程（CLI/桌面/WSL 三面）的会话库重定向到 Mosaic 托管私有路径（native 在缓存位 `zcode-session/db.sqlite`、WSL 在发行版内 `~/.mosaic/zcode-session/`）——桌面版历史不再出现 Mosaic 会话、悬浮球不再被激活；凭据面（`credentials.json`/`provider_config.json` 锚定 `~/.zcode/v2`）不受影响，登录态照常共享（决定性实验：设私有库后共享库 WAL 零写入、会话只进私有库、任务成功）。

## [0.6.5] - 2026-10-31

### Fixed

- 修复 0.6.5 审校发现（独立审校报告"7 项 P1、5 项 P2"，ADR-0028 v0.2）：**(1) P1 执行输入丢失**——清单虽存档但只传 manifest_id，目标/验收标准/近期消息/工具清单均不达执行器；改为 Manifests.Build 传透层内容块（Layers[]→map），服务层 manifestInline 提取契约（objective/scope/acceptance_criteria/deliverable_specs/constraints/budget）+ 近期消息 + 工具清单入 Inline——执行器拿到实际任务正文。**(2) P1 真实执行器未接通**——Agent ID 硬编为 echo adapter，真实 CLI 从未被调用；supervisorExecutor 经 identity.AgentByID 解析 Agent 身份域的 adapter 家族名（echo 仅无身份/harness 时兜底——测试装配），Runtime 绑定工作区决定执行面。**(3) P1 租约释放后仍启动**——提交前无围栏复核，装配期间释放租约后执行照样开始；新增提交前 CheckFence（失效/检查错误均 finalizeFenceRejected → Run failed + 释放 + fence_rejected 审计——执行不启动）。**(4) P1 旧结果仍能发布**——围栏查询报错会放行（ferr != nil == false && !valid 的取反逻辑缺陷）；核对后释放重领旧结果仍可落库；重构为 finalizeRun（围栏核对紧邻终态写入——核对不通过写 fence_rejected 而非终态；检查错误保守拒绝），修复取反缺陷（ferr nil check 先行）。**(5) P1 请求取消使任务卡死**——终态写入/租约释放/Mission 更新沿用请求 ctx（断开后全部失败→running + 活跃租约遗留）；清理面改用独立 30s 超时 context.Background()。**(6) P1 重启后自动重跑未知结果**——恢复置 Mission 回 ready，首次扫描即 attempt+1 执行；改为 Mission 停在 running（不回 ready）——下次扫描不自动调度，等待用户经手动触发决定（"禁止盲目重跑"强化）。**(7) P1 已取消任务仍可开始**——SetRunning 幂等无操作后返回 nil 错误（changed 信息丢失）；TransitionStatus 三存储实现均返回 changed bool（RowsAffected > 0），changed=false 时 Run 置 failed 携"已取消不再 ready——执行不启动"。**(8) P2 空闲 Runtime 被冲突候选挡住**——FirstReady 只按 runtime_id 排除，工作区被占（跨运行时）的候选反复选中后失败；改为按 workspace_key 排除（后续可用候选不被挡）。**(9) P2 公开状态枚举不完整**——Mission running 态已实际返回但 OpenAPI 枚举只列 draft/ready/canceled；补齐两处。**(10) P2 执行时间未返回**——started_at/finished_at 已存储但视图遗漏；补入 missionRunView。**(11) P2 执行页面不随后台更新**——加 10s 轮询。**(12) P2 终态落库失败仍返回成功**——返回 ErrTerminalWrite（含原始执行结果/错误摘要），非静默吞错。

### Added

- 单 Agent Mission 执行（0.6.5 / RFC-0018 §6/§10.2 / ADR-0028，migration v19）：`internal/run` 新域——**Run**（一次执行尝试，run_ 前缀；(mission_id, attempt) 唯一 + 每 Mission 至多一个未终态行（部分唯一索引）；lease/manifest/runtime/binding/agent 绑定 + fence_token/generation；pending → running → completed | failed）+ **run_events** 流（attempt_requested / started / completed / failed / **fence_rejected**——迟到旧实例结果被围栏拒绝的审计事实）。**持久调度器**（5s 周期扫描 + `POST /v1/missions/{id}/runs` 手动触发）：对每个 ready Mission——owner active Binding 解析 → 能力满足的 ready Runtime（单 Agent 首个）→ 领取租约（ADR-0027）→ 装配存档清单（ADR-0025——执行存档输入）→ 创建 Run + Mission → running（状态投影 `TransitionStatus`）→ 提交执行 → 围栏核对（check-fence 四关）→ Run 终态 + 租约释放 + Mission 回 ready（可再执行 attempt+1）。清单 blocked → 释放租约返 manifest_blocked 409；无 ready Runtime/任职 → no_executor 409；资源被占 → scheduler_busy 409（下轮再试）。**执行器端口**（`Submit(ctx, ExecutionInput) → block`——app 注入 agent.Supervisor 适配，Kind=generate）；**启动恢复**：未终态 Run → failed（"结果未知；不会自动重跑"——RFC 禁止盲目重跑裁定）+ 租约释放 + Mission 回 ready。API 五端点（手动调度/Run 历史/Run 详情/Run 事件/调度器诊断面）+ 任务中心详情执行区（手动执行按钮 + Run 历史（attempt/状态/结果摘要/错误/围栏代次））。验证：域 UT（闭环/失败/围栏拒绝/资源忙/无执行面/清单 blocked/恢复不重跑/扫描多 Mission 调度）+ IT（v19 新库/未终态互斥/attempt 递增/事件回读/旧库升级）+ 端点（echo 全链/409 族/503）先红后绿；vet/-race/it/st 串行 + SPA 绿；真机冒烟 10 项 ALL-PASS（真实 CLI 发现链 + echo 适配器执行：completed attempt 1/2、Mission 回 ready、事件 3 条、租约释放、调度器诊断、重启 Run 历史保留——**周期调度器自动重复执行验证了持久调度面**）。

## [0.6.4] - 2026-10-30

### Fixed

- 修复 0.6.4 审校发现（独立审校报告"1 项 P1、4 项 P2、1 项 P3，建议修复后定版"，ADR-0027 v0.2）：**(1) P1 旧续租可复活已过期租约及原令牌**——续租读 active 后并发到期先落，旧续租仍整行写回使失效令牌再次有效；续租/释放改**原子条件更新**（只改目标字段、以 state='active' 为条件、时钟与惰性清障获锁后重取）——并发到期先落则零行 409，不复活不改令牌（RenewLease/ReleaseLease，SQLite 与内存双存储同语义，UT 钉住过期租约状态/到期时刻/令牌三不变）。**(2) P2 目录别名绕过工作区独占**——同一物理目录经符号链接别名注册后两运行时可同时领取且双围栏有效；environments 增 `workspace_key`（规范身份 = EvalSymlinks(Clean(dir))，migration v18）——建面按身份判重（别名 409 workspace_dir_taken）、领取面经 JOIN 按身份排他兜底（空键历史行不参与；真实临时目录 + SQLite 复现转回归）。**(3) P2 旧到期扫描覆盖成功续租**——扫描旧截止时间后续租提交未来截止，旧扫描仍置 expired；惰性到期改**单语句原子判定+转移**（datetime() 比较，无扫描-更新窗口）。**(4) P2 显式 ttl_seconds:0 被当缺省一小时**——字段存在即校验 [60, 86400]（领取与续租同口径，显式 0/越界 400；省略才缺省，空续租请求合法）。**(5) P2 已终止租约仍显示可释放**——释放遇 lease_inactive 刷新诊断列表对齐实际状态并保留说明。**(6) P3 generation 文档对齐实现**——代次仅领取时分配 MAX+1，释放/到期经状态终态化使旧令牌失效（ADR/OpenAPI/本文件措辞同步，实现不变）。

### Added

- RuntimeLease——工作区占用、互斥与围栏代次（0.6.4 / RFC-0018 §5.2/§10.3.1 / ADR-0027，migration v17）：`runtime_leases` 一表 + 双部分唯一索引承载互斥——**同一可写工作区（环境）至多一个活跃租约（跨运行时，§5.2"同一可写工作区须独占租约"）+ 单运行时并发 1**（首版单 Agent 串行；容量 >1 随 0.6.5 资源上限扩展）。租约字段 0.6.4 子集：lease_id（lse_）/runtime_id/environment_id（workspace_ref = 准备绑定环境目录，租约不改绑）/mission_id（占用主体——经端口验存在且非 canceled；run_id 随 0.6.5 Run 对象增列不落空壳）/generation/expires_at（TTL 缺省 3600s 界限 [60, 86400]）/resource_limits（声明不强制——0.6.5 消费）。**代次单源于租约行**（运行时当代 = MAX(generation)，无运行时列——单事实源免列迁移）；领取分配当代 MAX+1 并签发 `fnc_` 围栏令牌（释放/到期经状态终态化使旧令牌失效——代次自此被新领取超过）。**围栏校验四关**（probe 纪律恒 200）：令牌匹配 → active → 未过期 → 代次为当代；失败携 fence_token_mismatch / lease_not_active / lease_expired / generation_stale——0.6.5 结果提交经此拒绝旧实例提交（围栏只保护 Mosaic 接收的状态与受控工具调用，不撤销外部已发生动作——RFC 原文裁定）。生命周期：active →（release|到期）→ released|expired 终态；**renew 即心跳**（从现在起延长）；**到期惰性判定**（任一操作触碰即转 expired——确定性无后台计时器，读面不出现过期仍标 active 的行）。busy 派生化（活跃租约存在即忙，不占运行时状态名——避免双事实源）；draining/stopped 随 0.6.5 执行面。租约行即事实（无事件流，审计经租约历史）。API 六端点（领取/列表/详情/续租/释放/围栏校验）+ 运行环境页租约诊断区（RFC §13：Lease/generation 细节归诊断视图——领取是 0.6.5 调度行为不做人工入口）。验证：域 UT（互斥双面/并发恰一胜者/前置族/TTL 界限/续租从现在延长/惰性到期/围栏四关含播种落后代次）+ IT（v17 新库/库级部分唯一索引兜底直插拒绝/并发恰一/代次 MAX/v0.6.3 旧库升级）+ 端点（全链/409 族/404 族/503）先红后绿；vet/-race/it/st 串行 + SPA 绿；真机冒烟 15 项 ALL-PASS（真实任务领取、容量 409、canceled 拒领、围栏三态、释放后代次 2 再领、重启持久）。

## [0.6.3] - 2026-10-30

### Fixed

- 修复 0.6.3 审校发现（独立审校报告"1 项 P1、7 项 P2、1 项 P3，建议修复后定版"，ADR-0026 v0.2）：**(1) P1 启动后新装 CLI 无法被发现**——手动同步入口只读启动扫描的注册表缓存，新增恒为零；改为同步前先做有超时的宿主扫描再镜像（扫描部分失败不夺同步；启动/手动登记后的自动同步仍走扫描后状态不重扫），补端点级"装后 CLI 可发现"回归。**(2) P2 版本命令失败仍标 ready**——探测把非零退出的带数字错误文本当版本；probeExecutable 两分支（CLI 与桌面 bundle）统一要求 exit 0 才取版本。**(3) P2 迟到的健康检查覆盖准备结果**——健康检查读旧状态后慢探测，期间准备已成功（ready + 新绑定），旧检查整行写回撤销之；健康检查与准备按运行时串行化（读-探测-写全程持锁），补门控竞态测试钉住"最终 ready 且保新绑定"。**(4) P2 同一目录登记两个环境**——/dir 与 /dir/.（及尾斜杠变体）绕过目录唯一；保存与判重统一在 filepath.Clean 规范化之后。**(5) P2 并发首次同步产生孤儿事件**——服务层并发构造的新行 ID 未入库仍写 discovered 事件（引用不存在的 ID）；事件改由存储在事务内只为真正插入的行生成，IT 并发同步钉住"恰一行、恰一事件、零孤儿"。**(6) P2 拼错准备字段静默走默认环境**——prepare 请求未知字段拒收（DisallowUnknownFields 400），合法空体默认环境语义保留。**(7) P2 历史读取失败显示成空列表**——事件区三态（加载中/失败可重试/空列表）区分。**(8) P2 操作后已展开历史不刷新**——成功操作（准备/健康检查）触发该行历史重取。**(9) P3 创建回调清空等待期间改填的输入**——只清空仍等于提交快照的值。验证：装后可发现/门控竞态/目录变体判重/并发零孤儿/拼字段 400/退出码版本反例先红后绿；vet/-race + it/st 串行 + SPA 绿；真机回归冒烟 ALL-PASS。

### Added

- 本地 Environment / Runtime（0.6.3 / RFC-0018 §3/§6 / ADR-0026）：`internal/runtime` 新域——migration v16 三表（environments 命名工作区目录：workspace_dir 唯一 + is_default 部分唯一索引恰一默认；runtimes 镜像 harness 登记事实 + 本域状态绑定，(adapter, host, distro, path) 唯一发现幂等键；runtime_events 流 discovered/health_checked/state_changed——健康历史可查）。**状态机 0.6.3 子集**：discovered →（prepare 绑环境 + 确保目录 + 探测）→ ready；探测失败 → unhealthy（note：executable_gone / version_probe_failed 可见）；恢复且已绑环境 → ready；busy/draining/stopped 随 0.6.4/0.6.5，provisioning 不落库（同步操作不把瞬时态伪装成事实）。**发现同步**：harness 注册表单向搬运不回写（新行 discovered 事件，既有行只刷镜像事实不动状态/绑定/发现时间）；启动扫描后自动同步 + `POST /v1/runtimes/sync` 手动 + 手动登记可执行程序后即同步；消失/重绑归 harness 既有语义。**探测面**：harness 新增导出 `Reprobe`（按登记 ID 重跑完整探测，只读）；探测成功 = 文件存在且版本探测非空；登录态是记录事实非 ready 门（"当前可执行"调度裁定随 0.6.5，RFC §9.2 三种判断分层）；ready 后可 re-prepare 换绑环境（无租约无在途，随 0.6.4 收紧）。默认环境启动自动确保（`<data>/workspace`，目录落盘）。API 九端点（runtimes 列表/详情/事件/同步/健康检查/准备 + environments 列表/创建/详情）+ 运行环境页（状态徽标/健康检查/准备（环境选择）/健康与状态历史/环境管理/发现同步）。验证：域 UT（发现幂等/转移矩阵/准备语义（成功才换绑、目录确保失败可见）/环境校验与占用）+ IT（v16 新库/同步不覆盖本域事实/事件回读/默认恰一/目录占用/v0.6.2 旧库升级）+ 端点（全链/错误族/503）先红后绿；vet/-race/it/st 串行 + SPA 绿；真机冒烟 14 项 ALL-PASS（真实 CLI 发现链：启动自动同步、默认环境目录落盘、探测/准备/换绑、事件流、占用冲突、重启不重建行且状态保留）。

## [0.6.2] - 2026-10-30

### Fixed

- 修复 0.6.2 审校发现（独立审校报告"1 项 P1、7 项 P2，建议修复后定版"，ADR-0025 v0.2）：**(1) P1 合法模板不填 tools 装配崩溃**——tools 是可选指针（SlotSpec.Tools），新路径直接解引用 nil 指针；改经既有空值安全访问（无声明即空清单，不虚构能力不崩溃），补无 tools/无 runtime 模板装配回归。**(2) P2 换负责人后仍可提交旧任职**——选择值残留且按钮可提交（后端会 blocked owner_mismatch 并非越权）；负责人变化清空选择，提交前核对任职仍属当前负责人，失效就地报错。**(3) P2 迟到响应覆盖最新查看结果**——历史查看 A、B 连续请求，A 后返覆盖 B，历史也可覆盖新装配结果；装配与历史查看共用结果请求序号，只接纳最新响应。**(4) P2 准入报告与工具清单使用不同授权快照**——两次读取间撤销授权可存下"必需工具满足 Ready=true 而有效工具为空"的矛盾清单；移除 Readiness 端口，单次装配只读一份授权：报告经 ADR-0022 单源纯函数 EvaluateReadiness 与工具清单/域授权同快照推导。**(5) P2 异常大输入突破总预算**——可选层只受自身 64KiB 配额约束，必需层近满时总量可超 256KiB 仍标可用；可选层配额同时受剩余总预算约束（首条消息也不例外），总量超限防御性 blocked。**(6) P2 存档读取后摘要不能按原算法复算**——SQLite 把层内容读回 map，重编码键序与首次 typed 序列化不同；摘要改规范化编码（JSON 往返 map 字典序 + json.Number 保真）后哈希，模板版本入水位向量（slot_spec:{id}）使 digest 输入在清单行内自包含——存储往返可复算（DigestOf 导出，UT/IT 双钉）。**(7) P2 取消任务后历史入口消失**——整卡卸载致既有层内容与装配历史不可看（数据仍在、API 仍可读）；保留只读卡，仅禁新装配。**(8) P2 声明未授权的选配 room.recent 仍被存档**——准入已记降级，清单却含消息/计数/水位；模板声明该域而授权未含时在读取、计数前省略（omitted context_not_authorized，房间读取不发生）；未声明该域不扩大要求照常装配。另按审校注记补清单/回执同事务回滚 IT（回执插入失败整事务回滚，清单行不留半成品）。验证：无 tools 模板/同快照一致性/剩余预算收紧/往返复算/未授权域零读取（读取计数器钉住）先红后绿；SPA 结果序号与只读卡类型检查绿；vet/-race/it/st 串行 + SPA 绿；真机回归冒烟 ALL-PASS。

### Added

- 执行上下文清单（0.6.2 / RFC-0018 §7.3/§6 / ADR-0025）：`internal/execctx` 新域——ContextManifest 存档装配内容（钉契约当前版本行 + 任职钉版本模板行 + 授权交集（ADR-0022 单源）+ 来源房间水位向量），ContextReceipt 记交付账目不复制完整 Prompt（`run_id` 随 0.6.5 增列）。**层模型 0.6.2 子集**（RFC 组装顺序）：必需层 constraints（readiness 报告/有效格栅/工具有效集/有效委派/预算）、responsibility（任职 + 模板职责钉版）、contract（全量契约）、tools（声明序∩授权）不得截断——合计超 256 KiB 总预算 blocked `required_over_budget`；可选层 sources（引用原文）、recent_messages（来源房 public `message.posted`，64 KiB 独立配额、单条 8 KiB 上限、从新往旧装满披露 dropped/truncated）独立截断；checkpoint/memory/decision_capsule 未建模层账本如实 omitted 携原因（0.7/0.6.5+/0.6.11）。**授权过滤先于排序、摘要与计数**：非 public 事件不进入、不计数、不列名（不泄漏存在性）；水位 = 实际交付的最新 seq（交付前沿非读取前沿）。**digest 即身份**：SHA-256(层账本+水位+钉住版本)，`UNIQUE(mission_id, digest)`——同状态重建重放原清单（replayed=true，授权撤销再恢复也重放原清单），状态变化（房间新事件/授权变更/契约修订）追加新行——每 Mission 保留"各时点会交付什么"的装配历史；0.6.5 Runtime 执行存档清单不采信现场重装（ADR-0024 同型纪律）。blocked 恒 200 携 reason_code（probe 纪律；优先级 owner_mismatch > binding_not_active > readiness_not_met > required_over_budget，blocked 也存完整账本）；`context_manifest_ref` 兑现为查询时联查（ADR-0023 决策 2——不改契约内容，manifest 以 mission_id 寻址钉 contract_version）。migration v15 两表（`exec_context_manifests`/`exec_context_receipts`——表名冠 exec_ 与房间域 RFC-0007 context_receipts 区分）；无事件流（RFC §6 有明确所有者的持久记录，mission_events 契约生命周期专属）。API 三端点（POST/GET `/v1/missions/{id}/context-manifests`、GET `/v1/context-manifests/{id}`）+ 任务中心详情"执行上下文"区（选负责人任职装配、层账本与内容块、装配历史）。验证：域 UT（层账本/过滤先于计数/配额与单条截断/blocked 优先级/deterministic digest 与重放/巨引用超预算）+ IT（v15 新库/撞键恰一落库/v0.6.1 旧库升级/JSON 回读）+ 端点（全链/错误族/503）+ 适配器跨页回归先红后绿；vet/-race/it/st 串行 + SPA 绿；真机冒烟 13 项 ALL-PASS（装配 9 层、私有事件零泄漏、水位=公开前沿、重放、授权撤销 blocked/恢复重放原清单、新事件追加、错误族、重启持久）。

## [0.6.1] - 2026-10-30

### Fixed

- 修复 0.6.1 审校发现（独立审校报告"1 项 P1、4 项 P2，建议修复后定版"，ADR-0024 v0.2）：**(1) P1 同键并发冲突仍创建额外 Mission**——两个不同来源同时使用同一幂等键时，双方都通过幂等检查各自创建后才在回执落库处分胜负（实测 1 成功 + 1 冲突 + 2 份 Mission）；改为**创建前原子认领幂等键并绑定摘要**（ClaimKey 两段式：pending 认领 → 创建成功后 FinalizeReceipt 完结）——异摘要请求在写入前退出零副作用，并发孪生恰一认领（门控测试双向钉住：后到者不再进入创建）；已接受的两事务恢复窗口保留（创建后完结前崩溃 → 重试命中 pending 认领重驱动，映射撞键回显已建 mission_id）。**(2) P2 只填一项来源静默丢失**——仅填 room_id 或 task_id 时表单仍可提交且整个来源被省略（来源创建后不可补）；两项须同时为空或同时非空，半填就地报错阻止请求。**(3) P2 跨页历史丢失任务上下文**——房间超 500 条事件时逐页重建任务清单会丢失前页参与者索引（@指派解析）与既有申报状态，真实任务被拒；改为跨页累积全量有序历史后统一重建，并补跨页提及解析回归测试（admitted 在前页、申报在后页命中派生 TaskID）。**(4) P2 同摘要请求重新解析后不能重放**——同一请求解析两次得不同路由，首投后第二路由同键重试被判冲突，违反同键同摘要重放约定；幂等身份从路由改为**摘要**（同摘要异路由重放原回执），已提交回执查验先于决策时效判定（投递成功改变预检状态是常态，重试不得因此失去重放）。**(5) P2 Dispatch 绕过契约校验**——网关 resolve 缺必填列表或显式全零预算可解析投递成功并以空列表/默认预算入库（直接创建会 400）；resolve 入口复用与直接创建同源的必填五键存在性 + 显式预算严格校验，两条入口一致。验证：门控并发证明（P1 双向确定）、同摘要异路由重放、跨页提及解析、resolve 契约反例先红后绿；IT 改造 ClaimKey/FinalizeReceipt 两段（并发同摘要恰一认领、异摘要 conflict、同摘要完结重放）；全量 vet/-race/it/st（串行）+ SPA 绿；真机回归冒烟 9 项（同键异源 409 且恰一 Mission、同摘要重放、resolve 400 ×2、映射冲突、重启持久）ALL-PASS。

### Added

- 新增房间交办与 Dispatch 首切片（0.6.1，0.6 Mission 与本地执行领域第二件；负责人指令 2026-10-30："开始0.6.1"；ADR-0024）：Mission 与房间任务卡建立可审计关联，网关两段式骨架定型。**房间来源唯一映射**（migration v14 `mission_sources`）：MissionCreate 增可选结构化 `room_source {room_id, task_id}`——房间与任务卡经 SourceLookup 端口校验存在（app 装配：EventReader 分页读全量事件 → room.TasksOf 纯函数重建）；(room_id, task_id) 全局唯一，撞键 409 `source_already_mapped` 回显已映射 mission_id（同一任务卡至多交办一次）；映射为创建事实，修订不可改（换来源 = 新交办）；直接创建与网关投递共用同一约束。**网关首切片 internal/dispatch**（RFC-0018 §4.4–4.8/§11.2/§11.2.1 确定性子集，兑现 ADR-0023 排布裁定）：`resolve`（只预检不创建——房间/任务卡存在、映射未占、owner 存在、契约合法 → resolved，任一不满足 → blocked 决策携 reason_code，恒 200；决策落库含请求全文与 SHA-256 摘要，10 分钟有效）；`dispatch`（复核决策未过期且状态仍满足——变化 409 `route_stale` 重新解析；执行**决策存档的请求**而非客户端重发内容；mission 创建后回执落库）；**幂等语义**：(caller_ref, scope, idempotency_key) 库级唯一（caller/scope 服务端推导为 owner/workspace，请求体不得自称）——同键同路由 200 重放原回执（replayed 标注）、同键异路由 409 `idempotency_conflict`、并发孪生恰一胜者（败者重放胜者回执或可见冲突，绝不二次派单）；回执查询按幂等键重放。**两事务窗口如实登记**（ADR-0024）：mission 创建与回执落库分属两事务，崩溃其间留下孤儿 Mission 时重试经映射冲突可见恢复（回显 mission_id），不产生重复派单。**API 三端点**（resolve/dispatch/receipts，写面过生产写门）+ mission 创建扩展（room_source / 冲突映射）。**UI**：房间任务 Tab pending 卡新增「交办」入口（跳转 /missions 预填来源）；MissionsPage 创建表单增房间来源区（room_id + task_id，冲突 409 如实展示）；契约详情来源徽标。设计与测试先行：ADR-0024 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ UT 先红（resolve blocked 族/幂等三态/决策过期/复核变化/并发孪生恰一 Mission）、SQLite IT（v14 新库 + v0.6.0 真旧库升级 + 映射 UNIQUE + 回执唯一并发恰一胜者——顺带修复事务内借池连接的死锁形态：池上限 4 × 持连接等写锁，existing 读取改经事务连接）、端点全链先红后绿；vet（it st）+ -race + -tags it/st（串行）+ SPA 全绿；真机冒烟 12 项（真库注入合法 mosaic-todo 申报驱动任务投影——派生 TaskID 命中证 SourceLookup 全链；resolve resolved/blocked/400、dispatch 回执、幂等重放、同键异路由 409、回执查询、Mission 来源回显、映射冲突回显、重启持久）ALL-PASS。语义选择（AT-17）/续接/IM 入口按 RFC §13 阶段表随后续小版本；执行随 0.6.5。

## [0.6.0] - 2026-10-30

### Fixed

- 修复 0.6.0 审校发现（独立审校报告"2 项 P1、1 项 P2，建议修复后定版"，ADR-0023 v0.2）：**(1) P1 切换任务可能修改或取消错误任务**——详情组件未按任务隔离，从 A 切到 B 后旧详情与修订草稿残留、写请求已指向 B，两者版本号相同时 CAS 无法拦截；MissionDetail 按 missionID key 重挂载（切换即重置详情与草稿），并加写入归属守卫——详情加载完成且归属当前任务（mission_id 一致）才允许修订/取消，提交前再校验，迟到响应不得触发写入。**(2) P1 普通修订清空未编辑字段、重置预算**——修订请求只传表单编辑的六项，全量替换语义下 constraints/return_targets/budget/delegation/reviewer 被静默清空（实测预算 2 Run/600s 被重置为默认 32/86400）；修订请求改为从当前契约完整快照构造再覆盖编辑字段，未编辑字段与预算原样保留。**(3) P2 缺字段或非法预算仍被接受**——请求解码不查必填键存在性（缺验收清单的修订 200 并清空转 draft），显式全零预算被零值判断当成省略套默认；必填五键（objective/scope/acceptance_criteria/deliverable_specs/source_refs）缺失或 null 一律 400 bad_request（显式空数组才是清空语义），budget 键在场时按界限严格校验（全零等无效值 400 invalid_mission，缺省只在键缺席时成立），无效请求零改动。验证：端点反例（缺键/null/全零预算 ×创建/修订两面 + 基线契约零改动 + 显式预算按请求落）先红后绿；前端 tsc/build 绿；全量 vet/UT/-race/it 绿；真机回归冒烟 10 项（半体 400 ×2、全零预算 400 ×2、零改动、快照修订预算保持、CAS/终态/重启回归）ALL-PASS。

### Added

- 新增 MissionContract 独立事实源——版本化契约与契约状态（0.6.0，0.6 Mission 与本地执行领域第一件；负责人指令 2026-10-30："开工0.6.0"；ADR-0023）：交办从"聊天记录"变为**可验收的独立事实**。**独立事实源**（RFC-0018 §3.1）：`mi_` 生成式 ID；missions 投影 + mission_contract_versions **不可变版本行**（修订即追加，历史永不清除）+ mission_events append-only 流（created / contract_revised / status_changed；写路径单事务[校验→投影→版本行→事件]，migration v13）；归 Workspace 不挂 Room——退出/关闭房间不级联影响 Mission（0.6.1 落房间来源引用映射）。**契约内容**（§6.1 的 0.6.0 子集）：objective/scope/constraints/acceptance_criteria/deliverable_specs/source_refs/return_targets/budget/delegation/reviewer + owner；`decision_ref`/`context_manifest_ref` 不落（对象分别随 0.6.1/0.6.2 出现——不落空壳字段）；requester 恒本地 owner（多入口请求方随网关/委派扩展）。**budget 安全缺省** {并发 1、总 Run 32、墙钟 86400s、重试 0、委派 0}——契约声明事实，运行时强制随 0.6.5 Scheduler。**契约状态子集** draft/ready/canceled：完备性判定单源（创建与修订同源）——objective/scope/验收/交付物/来源齐备且 owner 已指派 → ready，否则 draft（"来源或验收缺失保留 draft"，unassigned 同停 draft——不捏造共识）；canceled 唯一终态（0.6.0 无在途工作单步取消，在途处置版随 0.6.6）。**版本与 CAS**（AT-13）：revise 全量替换 + 版本 +1，expected_version 必填——并发修订恰一胜者，败者 409 `version_conflict` 拿回当前版本可见重试（不静默合并）；完备性在修订上重判（draft ↔ ready 双向事件留痕）；终态拒修订/重复取消 409 `invalid_transition`。**owner 校验**：AgentLookup 端口（本地定义，app 装配注入身份服务）——owner 须存在、任意生命周期可指派（责任归属事实，行为能力由执行/交接时点校验 0.6.5/0.6.12）。**API 七端点**（契约冻结，§11.2 create_mission/revise_mission/cancel_mission 的 REST 化；幂等键与 DispatchReceipt 体系属网关命令面随 0.6.1 Dispatch 首切片落）：创建 / 列表 / 详情 / 修订（CAS）/ 取消（CAS + 原因入事件）/ 版本列表 / 事件流；写面过生产写门。**UI**：新增 `/missions` 任务中心页（AppShell 导航）——创建表单（目标/范围/验收/交付/来源/负责人下拉/预算高级项）、契约列表（状态徽标 + 版本）、详情（当前契约 + 版本历史 + 事件流）、修订（CAS 冲突 409 可见）、取消（原因输入）。**Dispatch 排布裁定**（兑现计划 v2.20 留白，ADR-0023 决策 6）：`internal/dispatch` 首切片随 0.6.1 房间交办（确定性最小实现 + DispatchReceipt + (caller_ref, scope, idempotency_key) 幂等约束）；不增列 0.6.13+；可选语义选择模块（AT-17）待确定性路径稳定后另定；0.6.0 不含网关代码。设计与测试先行：ADR-0023 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ UT 先红（完备性两态/校验族/CAS 并发 8 路恰一胜者/终态全拒/事件序列/缺省填充）、SQLite IT（v13 新库 + v0.5.5 真旧库升级 + 并发 revise 恰一胜者 + 版本快照）、七端点测试先红后绿；vet（it st）+ -race + -tags it/st（串行）+ SPA 全绿；真机冒烟 18 项（draft/ready 两态、完备性修订推进、CAS 409、并发恰一胜者 v3、校验/存在性反例、取消链与原因入事件、终态全拒、写门 401、重启持久）ALL-PASS。执行/调度/Run/验收/回房随 0.6.3–0.6.12；网关 Dispatch 随 0.6.1。

## [0.5.5] - 2026-10-30

### Fixed

- 修复 0.5.5 审校发现（独立审校报告"2 项 P1、4 项 P2，建议修复后定版"，ADR-0022 v0.2）：**(1) P1 切换 Agent 后可能把授权保存给错误对象**——保存按钮未阻止加载期间提交，从 A 切到 B、B 档案未返回时表单仍是 A 的权限，点保存会覆盖 B（可能意外扩大授权）；表单现记录所属 Agent（formAgent），档案加载期间整体禁编辑禁保存，保存前核对归属、保存期间锁定 Agent 选择——切档残留内容无机会覆盖他人授权。**(2) P1 撤销必需授权后旧任职仍能激活**——ready 只代表 prepare 时点就绪，激活不复核当前授权（撤权后试算未就绪、激活却成功落库）；激活现按钉住模板版本重算交集，必需项缺失 409 `readiness_not_met` 可见拒绝（RFC §11"必需能力缺失则拒绝激活"落实；与 Agent active 两时点校验同口径；复核拒绝不落状态，补授权后直接激活）。**(3) P2 旧试算结果显示在新 Agent 下**——试算回填无条件接收响应，A 的迟到结果会显示在 B 的选择下；请求序号守卫，换 Agent/版本即失效旧响应（后端 prepare 仍独立校验）。**(4) P2 非法版本被拿去试算最新版**——试算把显式 0/-1 转为"省略"，与建任职的 400 拒绝不一致；前后端共用解析规则：只有空输入表示最新版，非空必须为正整数，非法就地拒绝。**(5) P2 ADR-0022 强隔离验收阶段归属错误**——原文把强隔离 Runtime 匹配归入 P4，与 RFC-0018 §13.1/§15 P1 出口要求冲突；更正为 0.6 Runtime/Mission 域交付强隔离准入（能力不可强制落实 → 拒绝激活），P4（0.9）仅环境变更后复验。**(6) P2 缺必填字段的 PUT 清空授权**——请求解码不查字段存在性，仅含 runtime 的半体 PUT 返回 200 并清空既有工具/上下文授权；必填键（含 runtime 子对象两键）缺失或 null 一律 400，仅显式空数组保留清空语义，校验失败原档案不动。验证：激活复核 UT（撤权 → ErrReadinessNotMet 保持 ready → 补回激活）+ 端点反例（半体/null PUT 400 且 GET 原值不变、撤权激活 409、试算显式 0/-1 400）先红后绿；全量 vet/race/it/st（串行）+ SPA 绿；真机回归冒烟 13 项（半体 PUT 不清空 ×3、撤权激活 409、详情保持 ready、补回激活、blocked 激活拒绝、重启持久）ALL-PASS。

### Added

- 新增职责准入——能力核对、权限交集与 readiness report（0.5.5，0.5 长期 Agent 与职责领域第六件（收官件）；负责人指令 2026-10-30："继续开发0.5.5版本"；ADR-0022）：任职从"恒 ready"变为真实准入。**AgentGrant 授权档案**（用户授权侧，migration v12 `slot_agent_grants`，一 Agent 一档 upsert 全量替换）：技能（ref@vN）/工具/上下文域/运行能力/工作区权限；**无档案 = 空授权**（安全缺省）——模板声明的一切必需项缺失即 blocked，未声明要求的职责不受影响（无隐式门槛）；Agent 须存在、任意生命周期状态可存档（授权是数据，行为许可仍是 prepare/activate 的 agent_not_active 两时点校验）；不建 grant 事件流（单用户 owner-token 本地应用，权衡记 ADR-0022）。**EvaluateReadiness 交集单源**（纯函数，门/试算/报告共用）：skills 模板列出即必需（授权同 ref 且 version ≥ 引用版——工作方法向后兼容）；tools/context/runtime.required 缺失 → blocked；tools/context.optional 缺失 → **降级有报告**（AT-07）；工作区权限格栅 none < read < read_write——授权 ≥ 要求即过，**有效权限 = min(授权, 模板要求)**（交集语义，授权直通不成立）；模板未声明 runtime 时不构成约束。**ReadinessChecker 端口实装**：签名升级为返回完整报告（缺失/降级/有效权限）；`GrantReadinessChecker`（实例 → 谱系 → 钉住版内容 → 档案 → 交集）由 app 装配注入，生产从此有真实 blocked；blocked 事件 payload 记 `missing`+`degraded` 两键；**不自动换人不自动扩权**（AT-07）——修复路径唯一：owner 改授权档案 → 重试。**API 四端点**（契约冻结）：`GET/PUT /v1/slots/grants/{agent_id}`（未设置 404 grant_not_found = 可编辑空表单非错误态；非法档案 400 invalid_grant）、`GET /v1/slots/instances/{id}/readiness`（**试算**：只读无副作用，未就绪也是 200——报告不是错误；版本三态与 prepare 同规则：省略钉最新、显式 <1 → 400、越界 409）、`POST /v1/slots/bindings/{id}/retry`（blocked → preparing → 门 → ready/blocked 单事务重试——ADR-0021 v0.2 #3 预留路径的 HTTP 暴露；非 blocked 409）；写面过生产写门。**UI**：职责区新增"Agent 授权档案"块（Agent 下拉 + 技能/工具/上下文/运行能力/权限表单，未设置与读取失败两态区分，换 Agent 弃置未保存编辑）；实例任职行新增**准入试算**（就绪/缺失/降级/有效权限即时报告，换 Agent 或版本即失效）与 **blocked 任职展示**（事件取证缺失/降级清单 + 重试准备按钮）。设计与测试先行：ADR-0022 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ UT 先红（纯函数表驱动：四类核对 × 满足/缺失/降级、权限格栅、无档案/无要求、skill 版本边界；PutGrant 校验族；AT-07 域内全链）、SQLite IT（v12 新库 + v0.5.4 真旧库升级 + 并发 upsert 恰一行无半写 + SpecByVersion + blocked→retry 事件序列）、四端点测试先红后绿；vet（it st）+ `-race` + `-tags it/st`（串行全绿）+ SPA tsc/build 全绿；真机冒烟 19 项（AT-07 全链：无授权 blocked 事件含 missing+degraded → 未补授权重试仍 blocked → 补授权（选配缺）retry ready 降级有报告 → 激活；试算无副作用；版本三态 400/409；不自动换人；单 active 409 回归；授权与任职重启持久）ALL-PASS。0.5 领域线至此收官（0.5.0–0.5.5）；后续：0.6 Mission 域（排空门实装与在途工作处置）。

## [0.5.4] - 2026-10-30

### Added

- 新增 Binding 任职状态机与交接（0.5.4，0.5 长期 Agent 与职责领域第五件；负责人指令 2026-10-29："开发0.5.4版本"；ADR-0021）：职责有了"谁在承担"的可审计事实。**状态机**（RFC-0018 §3.2）：`preparing →（readiness 门）ready → active →（排空门）released`，准备失败 `→ blocked → preparing`（0.5.5 起真实）；released 唯一终态留审计；非法转移 409 可见拒绝。**单 active 不变量**（§3.1/§13.5，AT-13）：同实例最多一位在任，库级部分唯一索引（`WHERE status='active'`）兜底——并发激活恰一胜者（8 路交替施压 IT 与真机实测恒 ≤1 在任）；冲突 409 `active_binding_exists`，不静默换人。**固定版本**（§5.2）：Binding 创建即钉 `slot_spec_version`（缺省取谱系最新，显式须 ∈ [1, latest]），模板后续演进不改变在任——升级 = 旧任职排空释放 + 新任职。**双门端口预埋**：`ReadinessChecker`（0.5.5 权限交集/能力核对）与 `DrainChecker`（0.6 在途工作）缺省恒通过/恒空——prepare 一次调用至 ready、release 两段事件与终态**同一事务一次提交**（0.5.1 P2#4 教训不复现），注入非空门时停留 blocked/draining 并返回清单（UT 钉住）。**前置校验**：Agent 须存在且 active（prepare 与 activate 两时点——挂起/退役不接任职，409 `agent_not_active`；AgentLookup 端口由 app 装配注入身份服务，slot 不 import personalagent）。**交接位**：release 接受可选 `handoff_note`（≤4KB，记入事件与投影），接续方经实例任职列表读取最近交接（交接包自动汇编随 0.6/0.7）。**存储（migration v11）**：slot_bindings 投影 + binding_events append-only 流（created/status_changed；建行与 created 事件同事务；转移单事务[校验→投影→事件]——BEGIN IMMEDIATE 全库约定）。**API 六端点**（契约冻结，§11 命令对齐）：prepare（404 instance/agent_not_found、409 agent_not_active/invalid_spec_version）、activate、release、列表（实例过滤）、详情、事件流；写面过生产写门。**UI**：实例行"任职"管理块——在任者徽标与钉住版本、释放（交接备注输入）、就绪列表激活（冲突如实 409）、建任职（Agent 下拉 + 钉版本）、最近交接展示。设计与测试先行：ADR-0021 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ UT 9 例（版本钉住/Agent 门两时点/单 active/排空门停留与重试/准入门 blocked 与重试（0.5.5 预留）/终态全拒/并发恰一/过滤）、SQLite IT（v11 新库 + v0.5.3 真旧库升级 + 部分唯一索引 8 轮交替并发实测恒 1 + 释放取消整体回滚）、端点全链先红后绿；vet（it st）+ `-race` 全量 + `-tags it/st` + SPA 全绿；真机冒烟（全链 + 挂起 Agent 拒任 + 并发激活恰一在任 + 4 任职重启持久）ALL-PASS。0.5.4 仅状态机与交接位：能力核对/权限交集/readiness report（0.5.5）、排空实际处置与交接包（0.6/0.7）随后交付。

### Fixed

- 修复 0.5.4 审校发现（独立审校报告"建议修复后定版，未发现 P0/P1"，ADR-0021 v0.2）：**(1) P2 "最近交接"可能显示旧备注**——选取逻辑取创建列表末项而非最后释放者（先建 A/B、按 B→A 顺序任职释放后仍显示 B 的备注）；改按释放时间（updated_at）取最新，接口保持创建序。**(2) P2 未加载任职就显示"空缺"**——摘要把未加载数据当空列表（已有在任者的实例刷新后也一直"空缺"，读取失败同样误报）；未加载/失败改为中性提示，仅加载完成且无在任才显示"空缺"。**(3) P2 准备中断留下死记录**——prepare 原先先提交 preparing 再推进 ready，两步之间取消会留下重试/激活/释放均拒的持久记录；门判定前置，建行直接落终态（ready/blocked）+ created + preparing→target 两事件**同一事务一次提交**（blocked 重试同口径单事务；取消上下文 IT 证零残留）。**(4) P2 ready 直释丢失交接备注**——状态更新原先只改状态与时间，返回值与后续查询备注为空（仅存于事件）；交接备注现与状态/事件同事务入投影，返回值/详情/事件三处一致。**(5) P2 显式 0/-1 被静默换成最新版**——版本参数原先把显式非法值并入缺省分支返回 201；改三态区分：省略钉最新、显式 <1 → 400 invalid_binding（服务层同拒）、区间内钉住。**(6) P2 任职事件主体字段错**——事件序列化原先把任职 ID 写入 AgentEvent 的 agent_id、缺 binding_id；改用 BindingEvent 类型（binding_id 承载，端点反例断言主体与无泄漏）。验证：原子性/备注一致性/版本边界 UT + 取消回滚与 blocked 单事务 IT + 三态与事件主体端点反例；全量 vet/race/it/st + SPA 绿；真机回归冒烟（显式 0/-1 400、事件恰 2 条主体正确、按释放时间选最新交接、ready 直释三处一致、重启持久）ALL-PASS。

## [0.5.3] - 2026-10-29

### Added

- 新增 SlotInstance 职责实例与独立知识命名空间（0.5.3，0.5 长期 Agent 与职责领域第四件；负责人指令 2026-10-29："开发0.5.3版本"；ADR-0020）：职责从**定义**（0.5.2 模板）走到**存在**——某作用域内实际承担的一份具体职责。**实例语义**（RFC-0018 §3.1/§5.2）：`si_` 前缀生成式实例 ID；引用模板谱系**不固定版本**（固定版本是 0.5.4 Binding 的职责，实例只记 `created_from_version` 审计事实）；同一模板可实例化多份；换人后实例保留（实例属于作用域，不属于任职者）；无状态机（RFC §3.2 未定义，不发明）、无删除（无消费者，与模板同纪律）。**独立知识命名空间**：每实例生成 `slotns_` 前缀全局唯一、终身稳定、永不重派的知识命名空间（SQLite migration v10 UNIQUE 约束兜底）——职责知识挂实例命名空间、绝不挂任职者私有库（§5.2 原文裁定的落地）；命名空间即 0.7 分域记忆 MemoryItem.namespace 的登记事实，本体存储随后。**存档与现势分离**：实例标题缺省取实例化时模板最新名**存档**（模板后续改名不回写）；视图另携模板**现势**连接（spec.name/latest_version 现算，随模板演进）——列表读取单源 `LatestByLineage` 聚合。**API（契约冻结）**：`POST /v1/slots/instances`（默认/显式标题、谱系未知 404 spec_not_found、空白标题 400 invalid_instance）、`GET /v1/slots/instances`（创建序 + 现势连接）、`GET /v1/slots/instances/{id}`（404 instance_not_found）；写面过生产写门。**UI**：设置页职责区新增"职责实例"块——模板下拉 + 可选标题的实例化表单、实例列表（现势徽标/自 vN 实例化/slotns_ 命名空间徽标）。设计与测试先行：ADR-0020 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ UT（默认标题存档/模板演进不回写+现势演进/标题校验两态/未知谱系/多实例命名空间互异/并发 8 路全唯一——测试件生成器原子取号）、SQLite IT（v10 新库 + v0.5.2 真旧库升级 + 命名空间 UNIQUE 实测 + 并发全唯一）、端点测试先红后绿；vet（it st）+ `-race` 全量 + `-tags it/st` + SPA tsc/build 全绿；真机冒烟：默认/显式标题、404/400、模板演进存档标题不变+现势演进、写门 403、并发 6 路全唯一（合计 8 实例）、重启持久 ALL-PASS。0.5.3 仅实例本身：任职/交接/排空（0.5.4）、准入（0.5.5）、知识读写面（0.7）随后交付。

### Fixed

- 修复 0.5.3 审校发现（独立审校报告"建议修复后定版，未发现 P0/P1"，ADR-0020 v0.2）：**(1) P2 并发下列表矛盾版本**——列表组装原先先读模板后读实例，两读之间的并发创建/追加会打出空模板名、v0 或"创建自 v2、最新却 v1"的矛盾快照；改为先实例后模板（版本只追加 ⇒ 现势恒 ≥ 创建版本）并加防御钳制（陈旧快照注入 UT + 真实 SQLite 并发追加/实例化/读取交替施压 IT 钉住不变量）。**(2) P2 模板读取失败伪装成功**——详情现势读取错误原先被吞掉仍返回 200 空名 v0；现传播错误（端点 500），故障注入 UT 钉住。**(3) P2 显式空标题绕过校验**——处理器原先把省略与 `title:""` 合流，两者都 201；现三态区分：省略缺省取模板名、显式空串/纯空白 400（端点反例钉住）。**(4) P2 实例列表失败永远"加载中"**——读取失败原先清成初始态且无重试；现单列错误态、保留最近成功列表（标注陈旧）、显式重试按钮。**(5) P2 修订后实例显示旧模板信息**——模板提交成功后实例现势连接同步刷新（存档标题与 created_from_version 服务端保证不变）。验证：故障注入/陈旧快照 UT、空串标题端点反例、并发不变量 IT ×10；全量 vet/race/it/st + SPA 绿；真机回归冒烟（三态标题 + 并发 3 路追加 ×3 路实例化轮询不变量 10 实例全满足 + 详情现势 + 重启持久）ALL-PASS。

## [0.5.2] - 2026-10-29

### Added

- 新增 SlotSpec 版本化职责模板（0.5.2，0.5 长期 Agent 与职责领域第三件；负责人指令 2026-10-29："开发0.5.2版本"；ADR-0019，RFC-0018 §5.1 候选语义的正式定形）：**不可变版本行**——`slot_specs` 表（migration v9）以 `(slot_spec_id, version)` 为主键，修订即追加新版本（version=latest+1，取号在 BEGIN IMMEDIATE 临界区内——并发追加串行化为连续版本号，无空洞无重复），无 UPDATE/DELETE 路径，历史版本永不清除（0.5.4 Binding 固定版本引用的审计前提）；版本行本身即不可变历史，不另设事件流。**内容 Schema 定形**：名称/职责陈述/交付规范必填，可选段——技能引用（仅引用，不含可执行体）、工具与上下文的 required/optional 分层（required 缺失 blocked、optional 缺失按声明降级——AT-07 准入预埋，两者不得相交）、委派策略（缺省 = 不允许委派的安全缺省）、运行时能力要求（能力名而非安装路径）、交接必备件；校验单源在服务层（必填/上限/模式/枚举/相交全拒）。**谱系标识**：`slot.` 前缀小写 slug（用户选定、跨版本稳定——进配置与导出引用，对齐 RFC 示例 `slot.research-lead`）。**API（契约冻结）**：`POST /v1/slots/specs`（建谱系 409 spec_exists）、`POST /v1/slots/specs/{id}/versions`（追加不可变版本）、`GET /v1/slots/specs`（谱系摘要）、`GET /v1/slots/specs/{id}`（最新全量）、`GET .../versions`（全版本升序）；写面过生产写门（Origin/CT/OwnerToken）。**UI**：设置页新增"职责"区——谱系列表（最新版本徽标/版本数/修订时间）、新建模板表单（核心字段 + 必选/可选工具）、按最新版本预填的修订流、版本历史懒加载明细。设计与测试先行：ADR-0019 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ UT（校验族 20 例 + 版本语义 + 并发追加 8 路）/ SQLite IT（v9 新库 + v0.5.1 真旧库升级 + 并发追加恰连续）/ 端点测试先红后绿；vet（it st）+ `-race` 全量 + `-tags it/st` + SPA tsc/build 全绿；真机冒烟：全链（建/409/追加/版本不可变/校验 400/404/写门 403）+ 并发追加 6 路 1–8 连续唯一 + 重启 8 版持久 ALL-PASS。0.5.2 仅模板本身：实例（0.5.3）、任职（0.5.4）、准入（0.5.5）随后交付。

### Fixed

- 修复 0.5.2 审校发现（独立审校报告"建议修复后定版"，ADR-0019 v0.2）：**(1) P1 修订会丢掉未编辑的配置**——修订提交原先只重建表单可见字段（名称/职责/交付/工具），API 创建的完整模板只改名称也会在新快照丢失 skills/context/delegation/runtime/handoff；修订现以完整最新模板为基线，表单未覆盖段逐字带入（仅改名称其余配置不变，回归冒烟钉住；历史版本本就不受影响，受损的是新快照）。**(2) P2 交付规范被逗号拆散**——表单转换原先把中英文逗号当分隔符，与"每行一项"说明不符（16 项可被拆成 32 项超限失败）；现仅按换行拆分。**(3) P2 空 runtime 绕过校验**——workspace_access 空串/缺失原先被接受并落库（违反契约三值枚举）；现建谱系与追加两条路径均 400 invalid_spec（UT + 端点反例钉住）。**(4) P2 追加后历史可能停留旧版本**——版本历史读取不忽略迟到响应、刷新标记变化时空缓存不触发重读；现读取直接依赖 (open, rev) 且请求序号拒旧响应。**(5) P2 读取失败显示"暂无版本"**——网络/5xx 错误原先被缓存成空数组且不再重试；现单列错误态 + 显式重试入口，只缓存成功结果。**(6) P2 迟到修订响应覆盖草稿**——修订加载返回后无条件替换表单，等待期间的新建草稿会被旧响应覆盖并切回追加模式；现以请求序号守卫，新建/取消/切换模板使在途响应失效。验证：空 runtime UT 两例 + 端点双路径反例；全量 vet/race/it/st + SPA 绿；真机回归冒烟（空 runtime 双路径 400 + 全字段模板仅改名追加其余段逐字保留 + 并发追加 1–6 连续且 v1 全字段不变 + 重启持久）ALL-PASS。

## [0.5.1] - 2026-10-29

### Added

- 新增 Agent 生命周期状态机与事件流（0.5.1，0.5 长期 Agent 与职责领域第二件；负责人指令 2026-10-29："项目计划和设计补充了同步机制，关注下需求变更的内容和范围。还是设计和测试先行，开发0.5.1版本"——需求变更经核对：交付计划 v2.04 新增 0.10 个人数据备份与多设备同步领域（RFC-0019 S1–S6 → 0.10.0–0.10.5；自动同步另立 RFC 预排 0.10.6–0.10.13），0.5–0.9 排期不变，0.5.1 范围不受影响；ADR-0018）。**状态机**（RFC-0018 §3.2）：`active ↔ suspended；active|suspended → retiring → retired`（retired 唯一终态；active 不代表正在运行），suspend/resume/retire 显式转移，非法转移一律 409 可见拒绝（不静默幂等——重放方须重取终态）；retire 经承诺门（"处置所有承诺后才能 retired"）：0.5.1 无 Mission/Binding/Goal 域、承诺集恒空，同调用完成 retiring→retired（两段事件各恰一条），`CommitmentChecker` 端口留作 0.5.2+/0.6 各域接入缝（未处置时停留 retiring 并返回 `outstanding_commitments` 清单）。**事件流（migration v8）**：`agent_events` append-only 流（seq 全表单调 = 流序；事件体 `created`{source,adapter,display_name} / `status_changed`{from,to,reason}）；v7 存量库开库幂等回填（每 Agent 按创建序补恰一条 created，重开不重复），`agents` 表自此降为投影（转移 = 单事务[状态校验 → 更新投影 → 追加事件]，事件流 ⊇ 全部历史可重建投影；建行与 created 事件同事务——原子认领不变量扩展）。**同步与生命周期解耦**：同步只建不改——suspended/retired 身份的 bot_id 重现（重装/重扫/重绑）不复活状态，新 BotID 仍以 active 出生；退役 ≠ 卸载可执行项（身份级状态，行为管制随消费域 0.5.2+/0.6 落地）。**API（契约冻结）**：`POST /v1/identity/agents`（手动建，source=manual、active 出生、无别名——为 Slot 任职/Worker 晋升预留主体）、`POST /v1/identity/agents/{agent_id}/lifecycle`（suspend/resume/retire → `LifecycleResult{agent, outstanding_commitments}`；404 `identity_not_found` / 409 `invalid_transition`）、`GET /v1/identity/agents/{agent_id}/events`（只读审计面）；写面过生产写门（Origin 回环 + Content-Type + X-Owner-Token，与 harness 手动登记同口径）；`IdentityAgent.status` 收紧为四值枚举。**并发**：SQLite 写事务串行化下并发转移恰一胜者（败者 409），8 路并发 UT/IT 钉住（事件恰一条、终态一致）。**UI**：设置页 Agent 区新增"Agent 身份与生命周期"——身份列表（展示名/adapter/状态徽标/手动建标记/挂靠可执行数）、暂停/恢复/退役按钮（退役确认）、生命周期事件懒加载明细。设计与测试先行：ADR-0018 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ 生命周期 UT 9 例 / SQLite IT 5 例（含 v0.5.0 真旧库回填 + 并发恰一胜者）/ 端点测试 3 例先红后绿；vet（it st）+ `-race` 全量 + `-tags it` + `-tags st` + SPA tsc/build 全部通过；真机冒烟（真实 CLI 扫描 5 身份）：全转移链 + 重放 409 + 终态 409 + 事件流 5 条 payload 语义 + 手动建/空名 400 + 写门 401/403 + 重启退役不复活/手动建持久 ALL-PASS。

### Fixed

- 修复 0.5.1 审校发现（独立审校报告"建议修复后定版"，ADR-0018 v0.2）：**(1) P1 生命周期并发操作可能返回 500**——身份写事务原先用 deferred 事务先读状态再升级写锁，WAL 下并发提交后升级撞 `SQLITE_BUSY_SNAPSHOT`，把本应按 409 返回的并发败者外泄成 500（审校 100 次重复并发测试 5 次失败）；身份域全部写事务（`EnsureAgent` / `EnsureAgentWithAlias` / `TransitionAgent` / `RetireAgent`）改经 `BEGIN IMMEDIATE` 进入写锁后再读（与房间 appendTx 同机制，锁等待由 busy_timeout 承担），修复后同口径 100 次重复全绿、HTTP 层 8 路并发实测恰一 200、其余 409、零 500。**(2) P2 操作后审计事件仍显示旧记录**——事件明细首次加载后永不刷新，先展开再暂停/恢复/退役状态会更新而历史不更新；操作成功后按修订号使该身份的事件缓存失效并重读，连续操作每次拿到最新事件史。**(3) P2 手动登记后生命周期列表漏掉新身份**——登记流程只刷新可执行项与座位；手动登记/启停成功后同步刷新身份列表，新身份立即可暂停/退役。**(4) P2 退役中断会留下待重试状态**——retire 原先分 retiring / retired 两次提交，第一步后取消请求会持久停在 retiring（重开数据库仍不变）；空承诺门时改 `RetireAgent` 单事务一次提交两段事件与终态，失败整体回滚；承诺门前置判定（不依赖 retiring 态），已处 retiring 的重试只补终段事件。**(5) P2 旧响应可能覆盖最新生命周期状态**——操作按钮在列表刷新完成前重新启用，连续操作时较慢的旧响应可把已显示的 retired 覆盖回 suspended；操作锁保持至刷新完成，并以请求序号拒绝过期响应落地。验证：补并发 retire UT（8 路恰一胜者、事件恰两条）+ 原子退役 IT（预取消上下文整体回滚、终态再拒不落事件）+ 混合并发写 IT（转移/退役/别名三路 ×8 施压同库，零非语义错误、各 Agent 恰一胜者）；IT 100 次重复 + UT 并发族 `-race` ×30 + 全量 vet/race/it/st + SPA tsc/build 全绿；真机回归冒烟：全转移链 + HTTP 层 8 路并发（恰一 200、7×409、零 500、事件恰一条）+ 重启退役/悬挂持久 ALL-PASS。

## [0.5.0] - 2026-10-29

### Added

- 新增独立 Agent 身份注册表（0.5.0，RFC-0018 获批后首个领域小版本；负责人指令 2026-10-28："按新的项目计划，设计和测试先行，开发0.5.0版本"）：Agent 成为跨房间、跨部署位置的稳定主体，身份权威自 `harness-registry.json` 迁入 SQLite（migration v7：`agents` + `agent_aliases` 两表，ADR-0017）。**迁移与别名**：以 BotID 为等价键（ADR-0013 重绑语义延伸）幂等迁移——建行与 bot_id 别名认领为同一事务的原子 get-or-create（`EnsureAgentWithAlias`：并发竞争回读胜者、中断不留半行，杜绝重复或孤立身份），每个唯一 BotID 建 `ag_` 前缀 AgentID，旧 ID（bot_id / profile_id / participant_id）收敛为只增别名（旧房间事件里的 participantID 从此有统一解析点；派生函数收敛 personalagent 单源，有逐字节等价测试钉住）；同 BotID 多可执行项归一 Agent，可执行项消失不删 Agent，别名碰撞（指向另一 Agent，含同 adapter 路径经 sanitize 归一化的跨身份派生碰撞）不合并、按 (kind, value, 目标 Agent) 去重后照常上报冲突日志待人工处置。**单向同步**：启动装载后、扫描完成后各同步一次，身份 API 与可执行项列表读前惰性幂等同步兜底（手动登记后设置页刷新 executables 即时获得 agent_id 徽标）；可执行发现表只引用身份（`Executable` 增 `agent_id` 增量字段），身份表永不回写注册表身份字段。**API（契约冻结）**：`GET /v1/identity/agents`（列表：别名集 + 可执行引用现算连接）与 `GET /v1/identity/agents/{agent_id}`（详情 / 404 `identity_not_found`），只读无写门；座位面 `GET /v1/agents` 与既有可执行项输出保持旧 ID 不变（兼容断言测试钉住）。**UI**：设置页可执行项行显示 `ag_*` 身份徽标（title 携完整 ID）。设计与测试先行：RFC-0018 批准（v0.4）+ 架构/RFC-0002/0007 边界对齐（P0 门槛）+ ADR-0017 + OpenAPI 契约冻结 → 迁移 fixture（多 adapter/多实例/祖父化/重绑/同 BotID 归一）golden、同步语义族 UT、SQLite IT、端点与兼容断言，先红后绿；vet + `-race` 全量 + `-tags it` 全部通过。

### Fixed

- 修复 0.5.0 审校发现（独立审校报告"建议修复后定版"，ADR-0017 v0.2）：**(1) P1 并发/中断重复身份**——建 Agent 行与 bot_id 别名认领原先分开提交，并发首次同步各建一行、失败者仍回写自己的 ID，建行后中断也留孤立身份；改同事务原子 get-or-create（`EnsureAgentWithAlias`，别名已被占即回滚候选行回读胜者）。**(2) P2 跨身份别名碰撞静默**——同步内去重原先只按别名类型+值、未比对目标 Agent，同 adapter 路径 `/a/b` 与 `/a_b` 经 sanitize 撞出同派生别名时第二身份每轮绕过冲突检测；改按 (kind, value, 目标 Agent) 三元组判同，碰撞照常上报 Conflicts。**(3) P2 手动登记后徽标缺失**——身份同步原先只在启动与身份 API 读前发生，设置页登记后只刷新可执行列表永远拿不到 agent_id；可执行项列表读前现触发同步。**(4) P2 旧库升级测试缺口**——补真实 v0.4.1 表结构建库（含旧房间事件/outbox 记录）的升级 IT（实证升级实现无缺陷）。**附带（CI ubuntu 腿 -race 实证）**：修复 0.4.1 更新任务 `StartUpdate` 在注册表锁内裸读 `job.done` 与 `finish()` 写入的数据竞态（两把锁不同步；改经 `isDone()` 加锁读，20 次压测全绿）。

## [0.4.1] - 2026-10-28

### Added

- 新增 Agent CLI 版本检测与更新（0.4.1，负责人指令 2026-10-27："本机使用的 CLI 通常不是最新的"——原 0.7.0 连接与生态项前移）：设置页实例行「检查更新 / 更新到最新」。**更新通道三分类（ADR-0016，负责人裁定 2026-10-27："现在是三种更新通道：第三方分发仓库 / 自更新 / Mosaic 自带更新通道……代码实现也需要按这三种抽象，便于后续拓展到其它的 agent"）**：**registry**（第三方分发仓库——检测与安装都走仓库客户端，现役 npm：codex；扩展 = 新增 packageRepo 实现，brew/apt/pip 换绑即用）；**self-update**（官方自更新子命令——各家参数/输出有差异、机制同一：claude `update` / kimi `upgrade --yes` / **mcode `update`**（裁定归此 + 子命令实证在席，npm 面仅作检测）；检测面可借仓库分发查最新（claude/mcode），无仓库面如实呈现"无独立查询面"（kimi））；**mosaic**（Mosaic 自带更新通道——自建打包分发（zcode）：更新驱动用户脚本 `$HOME/.mosaic/update/<adapter>.sh`（拉源码+自有打包管线+安装，放置即用、过程日志同一可视化管线；缺位 = 无更新面携指引），检测面对齐自建 releases 信号（产品更新源 feed 优先、源码 tag 回退，负责人桌面 3.16.3 实录：OSS 元数据滞后于产品通道）。API 通道枚举收拢三值（registry | self-update | mosaic）。检测与更新面正交；版本方案不可比不判更新（comparable）。最新版查询结果缓存 30 分钟最新版查询结果缓存 30 分钟（npm 镜像源首查慢启动实测 ~10s / git ls-remote 网络往返；失败不缓存）；异构版本原文（"2.1.172 (Claude Code)" / "codex-cli 0.153.4"）抽 semver 后语义化比较，新增 `comparable` 字段承载方案可比性（跨系时 up_to_date 恒 true 不标红，UI 只展示 tag 与说明）。npm/git 调用形态实证钉死：一律 `bash -lc` 登录 shell（绝对路径 npm 缺 node——shebang `/usr/bin/env node` 需 nvm PATH；`sh -lc` 会解析到 Windows 互操作 npm——dash 登录不读 bash 的 nvm 注入）；Windows native 面走 `cmd /c`。更新完成后重探 `--version` 并回填注册表持久化（扫描只在启动时跑，版本刷新由端点显式触发）。**更新过程可视化（验收反馈轮）**：更新为异步任务（POST `.../update` 受理即返 202——started/在途幂等 running），子进程输出经 Runner 新增的 RunStream 管道逐行转发，`GET .../update/stream` SSE 实时推送（连接即重放已积累行——快照与实时同临界区无丢失窗口；终态 done/failed 后关流；环形保留尾部 1000 行；慢消费者丢行不阻塞更新进程）；设置页内联日志控制台（等宽逐行、命令行高亮、自动贴底、可收起）——npm 下载进度/自更新输出/版本回探全程实时可见，四种通道（claude 自更新/npm install/kimi upgrade/版本重探）同一管线。真机流式实录：kimi 更新任务全链（202 → 命令呈现 → "Kimi Code is already up to date (v2.3.3)" → 版本回探 → done 携 2.3.3）。HTTP 写门（Origin/OwnerToken）与跨运行面执行（WSL 安装的 CLI 在其发行版内更新）不变；流端点为只读观察面无写门（房间 SSE 读面同口径）；OpenAPI/apigen/TS 三链同步。真机端到端冒烟（WSL 真实环境）：claude 2.1.172→2.1.311 与 codex 0.153.4→0.164.0 检出有更新、minimax 0.7.0 比对为最新、kimi 官方自更新通道、zcode 检出源码 tag 3.15.1（不可比不误报）。验收实录：负责人桌面实测点按 codex 更新成功（0.153.4→0.164.0，版本回填注册表）。

### Fixed

- 修复验收反馈三条（2026-10-27 负责人验收）：**(1) 已是最新时更新按钮应不可点**——按钮禁用条件改为"can_check 且比对为最新"（mcode 实录）；无检测面的家族（kimi）不知最新态，按钮保持可用并以 title/note 说明幂等升级语义（已是最新则无动作）。**(2) 同一 CLI 多个安装条目混淆**（claude 实录三条：WSL 原生位 + WSL npm 位 + Windows 损坏 shim）——新增扫描期"等价重复安装去重"：同 (adapter, 运行面, 发行版, 渠道类) 且**版本相同**且均未启用才去重（保登记序首个 = PATH 命中 > 目录 glob 序；已启用项计入等价判定但自身永不移除；版本不同的并存（升级过渡/多版本）、跨渠道（codex/zcode 桌面 App vs CLI）、跨运行面（native vs WSL）、手动登记一律保留——v1.9"多实例不折叠"与 C 轨 2026-08-31 裁定的差异化多实例语义不变，多实例测试同步改为版本各异以钉住本意）；去重注记入启动日志（DrainNotes）。附带修复：版本探测输出清洗（Windows 侧损坏 claude.cmd shim 的 --version 输出为当前目录路径——含路径分隔符/超长/无数字的输出置空，不再冒充版本串）。**(3) kimi 检测不到新版本**——改道官方自更新通道（见上 Added：kimi 无 npm 分发，kimi upgrade --yes 为唯一自动通道）；检测面如实呈现"无独立查询面 + 幂等升级"。

## [0.4.0] - 2026-10-27

路线图 0.4.0「Agent 兼容性扩充」交付：Claude Code 原生 Harness 适配器（第五家）+ 自动发现与登录探测覆盖扩充。

### Added

- 新增 Claude Code 原生 Harness 适配器（第五家）：对接 `claude -p --output-format stream-json --verbose --dangerously-skip-permissions`（实证 2.1.172 = WSL openEuler 原生安装位 `~/.local/bin/claude`；Windows 原生安装位 `%USERPROFILE%\.local\bin` 同构发现）。真机实证钉死三处 CLI 行为并内化为适配器设计：**(1)** `-p --output-format stream-json` 必须搭配 `--verbose`（缺省时退出码 0、stdout 空、真因只在 stderr）——argv 恒带，缺省形态不出现；**(2)** 退出码 0 ≠ 成功——鉴权失败（OAuth token 失效）同样以退出码 0 结束，真因在 result 终止事件 `is_error:true / api_error_status:401`，成功判定必须检查 `result.is_error`、退出码只兜底进程级失败（其余四家以非零退出码为主信号的口径在此不适用；零退出码且无流输出的用法类错误携 stderr 首行诊断）；**(3)** 提示词 stdin 是 `-p` 一等通道（空 stdin 报 "Input must be provided either through stdin or as a prompt argument" 反证）——无 argv 长度上限、无 sh `$(cat)` 代入（kimi/zcode 面的 MAX_ARG_STRLEN 边界不存在，亦无需 MaxPromptBytes 物理闸）。会话连续性：session_id（UUID）自任意流内事件捕获（`system/init` 起即携带）、后续任务 `--resume` 恢复；模型覆盖原生 `--model` flag（别名 sonnet/opus 或全名；评估降档 EvalModel 优先于 Model——zcode 需 provider 配置 overlay 的对应面在此直接生效）；权限面 `--dangerously-skip-permissions`（YOLO 等价，v1.52 裁定口径）；无 `--cwd`——工作目录经 Execer dir 通道（native 进程 cwd / WSL sh cd 包装，kimi 同构）；env 白名单：标准代理/CA 系 + `ANTHROPIC_BASE_URL`（网络路由配置非凭据，中转网关用户随宿主生效；ANTHROPIC_API_KEY/AUTH_TOKEN 属凭据不透传，OQ-20）；Windows native 面 HOME/USERPROFILE 双锚 + 系统必需键（Node os.homedir 走 USERPROFILE，zcode 桌面面同构）。网络注意（实证）：CLI 对 401 内部重试（`system/api_retry`，max_retries 10）——任务超时兜底，勿以短超时驱动。提示词与结果映射与其余四家逐字同源（含 doc_ops/sheet_ops 侧车、history_query 两段式、矫正重试、发布门委托）。
- fixtures 钉 2.1.172 真机捕获（manifest 哈希门禁同制）：`exec-authfail.jsonl`——鉴权失败全程（hook 事件实录 → system/init（tools 清单）→ system/api_retry×2 → assistant(model=`<synthetic>`) → result is_error），一文件钉住 session 捕获/无关事件跳过/错误正文/失败轮 usage 全零四组语义。成功形状（assistant 文本块 + result.is_error=false + usage 非零）暂以 UT 内联文档 schema 钉住——捕获时 OAuth token 失效（刷新端点 429 限流，环境态），负责人重新 `claude auth login` 后补真机成功态捕获进 fixtures 并更新 manifest（IT 同步自动回绿）。
- 自动发现与登录探测扩充：harness 探针新增 claude 条目——`claude --version` 实证 "2.1.172 (Claude Code)"；登录态以 `~/.claude/.credentials.json`（claudeAiOauth）存在性判定（Linux/WSL 与 Windows 均文件形态；macOS Keychain 形态不适用本仓平台面）；KnownDirGlobs 覆盖原生安装位 `.local/bin` 与 npm 全局面（nvm/fnm/volta/.npm-global——`@anthropic-ai/claude-code` 包）。已知限制如实登记：文件存在而 token 失效属环境态（适配器任务错误携 "Failed to authenticate"，ittest 降级承接，登录门只挡从未登录）；ANTHROPIC_API_KEY 纯 env 凭据形态文件面不可见——该形态用户无法过自动登录门（凭据留 CLI 侧纪律的代价，kimi/minimax 文件面同构），后续按需评估 env 探测面。
- ittest 供应商侧不可用判定增 claude 实证措辞（"failed to authenticate" / "oauth access token is invalid"）——真机实录：IT 三件套在 token 失效环境态下降级跳过（携完整 401 诊断），负责人重新登录后自动回绿（mcode 登录过期 v1.81 同例）。
- 设置页运行参数默认值面：adapterDefaultSpecs 增 claude（`~/.claude/settings.json` 顶层 `model`——为此扩 `jsonTopLevelString` JSON 标量键读取，配置优先；未配置回退空——CLI 内置默认模型随版本轮换、别名 sonnet 指向最新 Sonnet，官方无稳定常量，不虚构）。前端 RuntimeOptsEditor 增 claude 模型 placeholder（如 sonnet / opus）。
- 生产路径 ST 第五腿（CI 桩门禁）：TestClaudeProductionPath_ST + CI 桩 CLI（stdin 读提示词、init/result 行流、is_error 判定同适配器）——真二进制 + 真适配器 + 真子进程 + 真发布门，本机桩跑通（par_claude 发布 6.57s）；真机 opt-in `MOSAIC_ST_CLAUDE`。

## [0.3.0] - 2026-09-29

路线图「协作表格」前移定版（负责人指令：协作表格提前，发布 0.3.0）——RFC-0017 全线交付（M1 本地协作表格、M0 双平台真机实测、M2 手动副本交换、M3 原位回写与任务恢复、飞书多维表格只读导入），同窗搭载 RFC-0015/0016 云文档双平台接入与块模型元数据增补。

### Added

- 新增飞书多维表格（Base）只读导入（RFC-0017 §13 补登，真实使用触发：贴 `*.feishu.cn/base/…` 链接导入被拒——多维表格与电子表格是两套 API）：URL 分派认 `/base/`（`/wiki/` 链接给定向提示）；bitable v1 展平导入——数据表=工作表、字段=首行列头、记录=数据行，日期字段（ms 纪元）→ UTC、富字段（人员/单多选/附件/超链接）取文本表示并入展平注记，限额（≤50 表/≤200 列/≤4999 行）截断报告。真机实证 **bitable 只认专属 scope**（drive:drive 报 99991679，要求 bitable:app:readonly 等）——授权 scope 增 `bitable:app:readonly`，后台开通「查看多维表格」+ 重新「登录飞书」即解锁；回写守卫：多维表格不是合法更新目标（bad_sheet_target）。

- 新增表格回写与任务恢复 M3（RFC-0017）：把本地表格的修改覆写回既有云端目标（腾讯/飞书）。值域差异覆写——等格跳过（含 datetime↔text 等价与公式引号规范化比对，避免逐次重写同样的降级结果）、**不清除远端多出格**、结构不匹配（本地表目标缺失）整单拒绝；目标前态（仅差异格，≤256KiB）随记录持久化。异步分块执行（≤500 格/块，块间检查点 + 取消生效）：202 受理 + 单条轮询（进度 = cells_written/total）；传输类失败 → unknown 即停（不盲目续跑）、业务失败 → partial、取消 → canceled/partial（已写保留）；续跑校验"本地版本未变 + 目标未写区域仍等于前态"（被第三方改动 → 409 sheet_transfer_target_changed 重新预览），同记录续写剩余块。HTTP 增 update/preview、update（202）、by-id/{id}（GET 轮询）、by-id/{id}/cancel|resume（by-id 段避开 `{provider}` 通配路由冲突）；前端导出对话框内「更新已有目标」列表 → 差异预览（每表覆写/公式差异/远端多出）→ 进度条 + 取消/续跑。异步仅 update 方向（M2 导入/导出维持同步——已验证面不动）。真机 e2e（腾讯腿）全链通过并抓到关键平台行为：云端把单格区间规范化（写 `SUM(B1:B1)` 读回 `SUM(B1)`）——新增 `sheet.CanonicalFormula`（解析→渲染→单格区间塌缩）统一回写 diff/续跑校验/双家回读校验的公式比对，消灭幽灵差异（收敛验证：二次回写预览零差异）。
- 新增表格手动副本交换 M2（RFC-0017）：云端表格 ↔ 本地表格的双向副本导入导出。中立交换模型 + provider 可选端口（`SheetProvider`：快照读/建副本/能力声明，能力值取 M0 双平台实测档案）；腾讯适配（URL→file_id 解码定案 `D`+base64 并以 query_file_info 校验、include_formula 分块读、类型化分块写 + 回读比对）、飞书适配（三读合并判型——Unformatted×2 判日期格、Formula 与值比对消歧文本转义格；写入侧 `{"type":"text"}` 防 "TRUE"/`=` 强转、布尔 `=TRUE()`、日期序列数+formatter 两步写）。交换服务：预览（每表摘要 + 全量转换损失清单 + snapshot_hash/版本锚点）→ 执行（幂等键重放、stale 预览 409 拒绝、running 落库崩溃收敛 unknown）；导入装配走 `source=import` 修订批（≤1000 格/批），本地容量预检（>20 表/超长文本/超限公式转文本或阻断）全部进报告。存储加 `sheet_transfers` 表（幂等键唯一）；HTTP 加 `POST /v1/extdocs/sheet-transfers/preview|execute` 与历史查询（blocked 预检 409 携全量报告项）。前端 `SheetTransferDialog`（预览损失分色清单 + 执行结果 + 最近记录），入口三处：文档主页「从云文档导入」、房间 DocsTab「导入云表格并附着」、表格编辑器「导出到云」。已知缺口显式呈现不静默：两家样式不迁移（写失效/读缺失互不对称）、合并单元格仅报告、腾讯日期转文本、>15 位数值双向转文本。M3 原位回写未做（同步执行 + hash/版本锚点的取舍记入 RFC §11.2）。
- RFC-0017 M0 冒烟定案——腾讯（2026-09-28）与飞书（2026-09-29）两侧真实账号全链路（负责人提供官方 Skill 文档包并授权个人 token / 自建应用实测，测试表即测即删）。**腾讯侧** `sheet.*` 工具：类型化覆盖写保前导零/布尔/转义，`get_cell_data(include_formula=true)` 一次读回计算值+公式原文（含跨表与错误公式），插行后公式服务端自动改写且符合 Excel 语义（绝对引用随结构平移），xlsx 导出公式与错误码全保真；缺口：NUMBER 通道 float64 双重有损（>15 位必须 STRING）、CSV 写入吞前导零、样式写入静默失效（返回成功但读回/导出均无效果）、datetime 无写类型；主网关与独立 sheet-mcp 端点工具注册面不一致（四工具仅独立端点有）。**飞书侧** Sheets v2/v3：公式为类型化对象写入，`Formula`/`UnformattedValue` 双读分离公式原文与计算值（默认模式实测回计算值，与文档"缺省回公式原文"不符），错误值为字符串 `#DIV/0!`/`#NAME?`，日期两步写（style formatter+序列浮点）全通，样式写入真实生效，插行公式改写与 Excel/腾讯完全同构，跨表按表名自动规范加引号；缺口：字符串 `"TRUE"` 强转布尔（`{"type":"text"}` 对象可防御）、JSON 布尔裸写被拒（唯一路径 `=TRUE()`）、`=` 开头文本须 `{"type":"text"}` 转义（`'` 前缀非转义符）、sheets v2 无读样式接口、单格 range 须 `A1:B1` 全形。M2 裁定：腾讯 MCP-first（Open API 降为备选门槛）、飞书双读+类型化写入；两家结构改写均以远端读回为准，客户端不自行改写。插曲：飞书旧授权因应用未开通 drive:drive 被静默丢 scope（连带现有「共享到飞书」路径被挡 99991679——e2e 桩 token 未覆盖该场景），后台开通即时生效（无需发布版本）+ 重新「登录飞书」即解锁，真机顺带验证了飞书授权回环自动捕获实际生效。证据全文见 RFC-0017 §8.2/§10。
- 新增协作表格 M1（RFC-0017，本地部分全量；选型与限额定案 ADR-0015）：文档资产新增 `spreadsheet` 格式（format 封闭枚举扩展，与 markdown 块模型分离——禁止表格序列化为 Markdown 充当权威内容），新事件 `doc.sheet_revision_committed`（归一化 ops 定稿进事件——结构插删引发的公式改写随批落库，历史回放纯机械应用不依赖解析器）；工作簿/工作表/类型化单元格（text/number/bool/datetime/null + 公式原文）模型，公式服务端权威求值（封闭函数集 SUM/AVERAGE/MIN/MAX/COUNT/COUNTA/IF/ROUND + 算术/比较/拼接/跨表引用，错误值六枚含 #CIRC! 循环与 #BUDGET! 预算耗尽；编辑器不预演——派生值始终来自 range 回读）。HTTP：`commit_sheet_revision` 命令（CAS/幂等/整批原子同 markdown 纪律）、`GET /v1/docs/{id}/sheets`、`.../sheets/{sid}/range`（≤10000 格/次，公式格携派生值，used-range 缺省截断如实标记）、export 按 format 分派（表格 → 指定工作表 CSV 快照，公式格写派生值）。自绘表格编辑器（SheetEditorPage：窗口化网格/公式栏/工作表页签/样式与行列结构工具条/键盘导航；提交复用 DocEditor 工作台纪律——动作即 op、防抖聚合、409 重定基重放）；新建入口：文档主页对话框（文档/表格分段）与房间 DocsTab（新建表格并附着）。Agent 面：`sheet_ops` 块 + public_draft 侧车（≤8 ops/波，编辑类 op 强制 base_version 与显式坐标，409 逐 op 降级不翻波，refs 与 doc_ops 同形）；四家适配器指令与投影同步；表格语境摘录按 format 分派（每表 ≤10 行 ×8 列 TSV 有界注入，公式格显派生值）。副本按源事件流重放（默认表 ID 重映射保真）；容量护栏 ≤20 表/单表 5000×200/单批 64 ops、256KiB、1000 cells/全簿 5000 公式/重算 20 万节点。基准（i7-12700）：万格千公式全簿求值 0.61ms、500 修订回放 38ms。M2/M3（飞书/腾讯表格导入导出与回写）后置——依赖两家真实账号验收（腾讯侧 M0 已冒烟并裁定 MCP-first，Open API 降为备选；飞书侧待样表），UI 不留外部交换死入口。
- 飞书授权免粘 code（RFC-0016 增补，真机反馈迭代）：点「登录飞书」时服务端按登记的 redirect_uri 挂起回环回调监听（10 分钟窗口、可重复发起、首次落点即收摊），授权页跳转落回 `localhost:3000` 即自动换取 token——浏览器收到成功页（1.5s 自关），设置页轮询授权状态自动变绿，全程免复制地址栏 code；authorize 响应新增 `auto_captured`（回调端口被占如开发态 Vite 时为 false，界面降级保留手动粘 code 备用通道）。聊天输入框占位提示"粘贴腾讯/飞书文档链接自动转卡片"（入站粘贴转卡片本就支持，补可发现性）。
- 新增飞书云文档接入与多 provider 泛化（RFC-0016，取代同窗口的 RFC-0015 实现面）：外部文档域重构为 `internal/extdocs`（Provider 插件接口——host 归属/凭据/上游协议各自持，注册表编排共享事务与 resolve 分派），API 面迁移 `/v1/txdocs/*` → `/v1/extdocs/*`（未发版重构），凭据 per-provider 0600 JSON（旧 txdocs-token 启动一次性迁移）。飞书侧用户身份 OAuth（设置页存自建应用 app_id/secret → "登录飞书"开授权页 → 复制地址栏 code 粘贴绑定；user_access_token 过期前自动刷新、refresh_token 按轮换语义持久化，失效 409 `extdocs_reauth_required` 指路重授权），共享走云文档导入三步（上传素材 ccm_import_open → import_tasks md→docx 挂"我的云文档"根目录 → 500ms 轮询 60s 上限取 result.url），错误码（飞书 code/job_status）透传；共享菜单列已配置 provider（同文档可腾讯/飞书并存，per-provider 徽标）；聊天粘贴 `*.feishu.cn` 链接同样渲染卡片（external_refs.kind 加 fdoc，kind→host 归属校验分派，卡片双 provider 样式——腾讯蓝/飞书青 + 各自类型徽标，title 拉取 og:title 优先）。
- 块模型元数据增补（RFC-0016 对 RFC-0014 的修订，修复两家云文档转换缺口）：DocBlock 加可选 `level`（heading 1..6，缺省导出 H2）与 `lang`（代码语言 ≤48 字符）——命令面与 agent 端口双端形状校验（跨类型携带/越界拒收）、引擎代写补拷贝、四家适配器 doc_ops 指令同步（优先元数据而非手写 #/fence）；编辑器 heading 级别下拉（H1-H6）与 code 语言输入、按级渲染标题层级、代码块语言标注；仅改级别/语言的 diff 走 delete+insert 重建。导出渲染加固（存量文档零迁移立即受益）：heading 无 # 前缀自动补、code 未包 fence 自动包（带 lang 标注）、quote 缺 > 前缀逐行补——此前裸文本导出会被云文档当普通段落；text 自带标记仍透传优先。

## [0.2.1] - 2026-09-24

路线图 0.3.0「Agent 兼容性扩充」首员 ZCode 提前交付（负责人指令定版 0.2.1）：CLI 与桌面版双形态接入。

### Added

- 新增 ZCode 原生 Harness 适配器（第四家）：对接 `zcode -p --output-format stream-json --mode yolo`（实证 0.16.9：headless 全自动审批 + `--resume` 跨任务续会话），提示词经 stdin 代入 argv（`-p` 只认 argv，与 Kimi 同传输面）；模型覆盖走 provider 配置 overlay（zcode 无 `--model` flag——实证 `ZCODE_PERSONAL_PROVIDER_CONFIG_FILE` 重定向基座配置生效，重排目标模型所在 provider 的 `modelOrder` 并兼容开源 3.14.3 的 `defaultModelSelection`）；登录态双凭证面探测（OAuth `credentials.json` / API-key `provider_config.json` 任一即可用），设置页运行参数与评估降档口径与既有三家一致。
- ZCode 桌面版无感适配：安装桌面版即可入房，无需另装 CLI——自动发现 NSIS 安装位（per-user / 机器级）的内嵌 bundle 实例（渠道 `app:zcode-desktop`，家族位次 CLI 优先、桌面垫后；命中目录须同存 `resources/glm/zcode.cjs` 才算有效），版本探测经 `ELECTRON_RUN_AS_NODE=1` 无头驱动；驱动实现为 Mosaic 托管缓存副本——bundle 与 stock `resources/config/provider/zcode-builtin.json` 复制到用户缓存位（内容寻址，桌面升级自动换目录），不动安装目录（机器级 Program Files 需管理员、升级即失）；桌面面提示词 `-p` 直进 argv（无 sh 依赖），受 Windows 32KiB 命令行物理边界 fail fast；子进程 env 最小面（USERPROFILE 锚定凭证 + 系统必需键 + 既有网络白名单）；模型覆盖 overlay 与 CLI 面同口径（native 面 os 直读写）。非标准安装位可手动登记 `.exe`，按约定布局自动补全 bundle 路径。

### Fixed

- zcode 子进程透传 `ZCODE_HTTP_PROXY`（官方代理键，0.16.9 实证标准 `http_proxy` 系变量不被其 HTTP 客户端理会）——WSL 直连不稳时走宿主代理，避免重试风暴（maxAttempts 11）吃满任务超时。

## [0.2.0] - 2026-09-23

路线图 0.2.0 主特性「协作文档」落地（RFC-0014），群聊引擎补上沉默机制——发言资格闸（RFC-0012 附录 K，借鉴 MaiBot）。

### Added

- 新增协作文档域（RFC-0014）：讨论产生的结论与素材沉淀为 workspace 级全局文档资产——块级 Markdown 文档（heading / paragraph / list / code / quote / hr 封闭块集，块有稳定 ID 支撑 agent 锚定操作与冲突重放）。核心裁定「文档是共享可变状态，消息是不可变事实」：房间消息流中的文档卡片为引用式投影，文档更新卡片原地刷新（不改消息、不触发反应波）；配套文档主页（列表 / 全文搜索 / 版本历史与回滚 / 归档 / 副本 / 导出）与群文档（房间附着列表，文档可跨房间引用续作）；agent 经结构化 `doc_ops` 块同波读写文档（沿用 memory_ops 引擎校验代写先例），contextx 引用文档摘录注入讨论语境。per-doc 事件流（十事件族 schema）+ `doc:{id}` SSE 频道，修订批乐观并发。
- 新增发言资格闸（RFC-0012 附录 K，借鉴 MaiBot 沉默机制）：Agent 意图自报的相关性/紧急度加权分低于阈值（缺省 0.30）时不发言——被闸意图在讨论区留痕、人类可保送翻转；@点名/定向直通豁免；上一波发过言的座位额外扣冷却分（缺省 0.20）避免刷屏。阈值与冷却强度可在设置页调整，关闭即回"有意愿即发言"。
- 新增评估语境发言状态注入：逐座位向意图评估注入"你上波是否发言/距上次发言隔几条消息"事实锚点，配合新指令口径（"多数消息不需要你回复——沉默是常态"）让自决沉默更诚实。

### Changed

- 侧栏信息架构微调：文档入口（内容资产）移入会话列表分区，底部仅保留设置与主题切换（运维入口）。

### Fixed

- 修复发言资格闸豁免面缺口：Agent 发言经 mosaic-todo 指派任务后，被指派的责任人无任何直通通道（agent 消息从不携带 addressed_to），低自报分或上波发言冷却叠加即被闸，表现为"任务发布后全员沉默无人应答"。现锚点消息开口指派的负责人与 @点名同构直通，@解析与 tasklist 归属判定同源；非被指派方照闸不误（引擎级回归测试钉住）。
- 修复正文 @ 提及不解析：`@参与者ID` 与 `@显示名`（整词匹配、长名优先）现渲染为高亮提及标记；提及链接的生成与解析分处两遍，显示名匹配须先于 ID 替换——修复 pid 替换产物被显示名规则二次包裹成嵌套链接、外层方括号字面泄漏正文（`[@名](#m:pid)`）。
- 修复发言闸冷却缺省回填：旧设置文档零值歧义（显式 0 与未设置不可区分）导致 `/v1/system/settings` 显示冷却 0——回填条件改 `<=0`，阈值与冷却对称下限 0.05（值域 0.05–0.95 / 0.05–0.90）。
- 修复生成位 JSON 裸引号泄漏：模型在 JSON 字符串值内输出未转义英文双引号时，此前散文回退会把 `{"body":…}` 原文冒充发言——现状态机宽松修复可转义的裸引号（幂等，合法输入不动）；修复后仍解析失败且文本呈 JSON 形状则拒绝发布、座位失败原因可见，绝不冒充发言。
- 修复 mcode 退出码非零时的无信息量报错：流内无 error 事件且 stdout 全空时（实证登录态过期：退出码 3），错误此前只携带空 stdout 首行——现优先携带 stderr 真因（"Sign in to MiniMax to use Agent features"），座位失败原因直接可读；测试侧将登录过期按鉴权不可用降级跳过（与未登录同态）。
- 修复文档块编辑器三处：块类型下拉改自绘组件（原生 select 展开后焦点转移、失焦误判秒杀整个编辑器）；Enter 改为块内换行（多行 markdown 可写）；list 块补标记渲染并与导出同口径。

## [0.1.0] - 2026-09-16

首个版本，版本历史的起点。Room 事件溯源运行时、群聊制讨论引擎、三家原生 Harness 适配、收束与记忆、长任务执行与监控唤醒、桌面 / 浏览器双形态全部成形；里程碑 M0–M4 已完成验收，M5 产品化收口进行中，完成后发布 v1.0.0。

### Added

- 新增 Room Protocol 事件溯源运行时：SQLite WAL 权威事件日志、命令面幂等回执与乐观并发（409 校准重试）、SSE 游标续传与快照投影、崩溃恢复演练钉住（SIGKILL 后版本不漂移、断线续传不重投）。
- 新增群聊制讨论引擎（RFC-0012）：消息触发反应波（去抖聚合窗口），Agent 经「观察 → 意图评估 → 话语权 → 生成 → 发布」自决发言或静默（静默携理由留痕）；人类可随时打断、暂停，可对已记录意图保送（endorse）；终止四闸 = 意愿静默 + 发言冷却（@点名豁免）+ 对话环检测 + 预算硬顶；意图、分数与选择理由公开可查，不采集模型隐藏推理链。
- 新增三家原生 Harness 适配器：Codex CLI、Kimi Code、MiniMax mcode——Agent 自带模型与订阅额度，Mosaic 不代理发言流量、不核算费用，预算只作熔断；宿主层自动扫描 PATH / nvm / fnm / volta / WSL 发行版并探测登录态；WSL 执行面环境与网络配置取发行版自身登录环境（不透传宿主秘密）。
- 新增稳定 Bot 身份与固定会话入口（ADR-0013）：注册表 BotID 由路径派生、存量数据祖父化零迁移；消失重绑取保守规则（WSL 发行版暂时不可达不判死）；CLI 会话按「身份 × 房间」隔离，群聊 / 私聊不串线；显式路由（run_task 可携目标连接，未知连接显式拒绝、不回落）。
- 新增收束协议与讨论线程（RFC-0005 / RFC-0004）：六种 Closure Capsule（含保留具名异议与反证条件的优雅收束），预算耗尽只产生 Pause Capsule、不伪装成结论；线程分叉 / 暂停 / 恢复 / 合并；发言显式声明类型化关系（reply_to / supports / challenges 等），话题线与观点关系可视，系统推断只进可重建的版本化投影。
- 新增双平面记忆（RFC-0007）：恒常 Capsule 与策展记忆共享单预算；每波结束后台记忆评审（memory_ops 逐 op 留痕，容量 / 查重 / 注入扫描校验门）；全文检索召回两段式注入；「模型看得到的开发者也能看到」——知识 / 上下文全景 / 运行回执三个只读面。
- 新增任务清单与长任务执行通道（RFC-0002 附录）：带责任人的 tasklist 承诺与执行尝试（run）分离，状态只由执行器回执产生、不由正文「正在执行」推断；崩溃恢复进入「结果未知」、不自动重跑可能已产生副作用的任务；迟到结果按审计留痕；执行超时可配置且活读生效（无须重启）。
- 新增单次 reply-or-pass 限定路径（RFC-0012 附录 J）：仅适用「明确点名单人」与「已有任务交付」两类场景，资格调度前可判定、未命中一律走两阶段；真机 A/B 测量首条回复快约 37%（p50 10.1s 对 16.2s），结论限定两类场景不外推。
- 新增监控变化检测与有效唤醒（RFC-0012 附录 I）：脚本（无 shell）/ URL 两类源的快照哈希与行级差异；观察 / 处理 / 送达三水位分离，无变化不调模型；变化经执行通道投递到指派 Agent 的房间。
- 新增附件上传全链（RFC-0013）：两步上传、令牌单次消费；文本类内容以摘录注入 Agent 语境（过秘密剔除），图像 / 二进制按适配器能力如实降级；房间删除级联清理附件。
- 新增 Web SPA 与 Wails 桌面壳双形态：浏览器访问即用（服务端内嵌托管），Windows / macOS 桌面 App 一等公民；工作台首页、私聊（联系人）与群聊分组侧栏、房间右面板五分段（成员 / 任务 / 记忆 / 讨论 / 调试）、消息引用 / 复制 / 右键上下文菜单、@点名注入、正在输入与座位失败原因直接可见。
- 新增本地数据自主：VACUUM INTO 一致性一键备份、两段式恢复（启动段换库 + 回滚保护副本）、自诊断报告、NDJSON 导出、删除墓碑；owner token 写门 + Origin / Content-Type 门；非回环监听须显式豁免。
- 新增开发者模式：服务端 `-dev` 装配调试端点，SPA 时间线内联基建事件与调试面板；三条生成链（事件 Schema / OpenAPI / TypeScript SDK）漂移门禁与三平台 CI `-race` 全量 + 六目标交叉编译。
