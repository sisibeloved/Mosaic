# Changelog

本文件记录 Mosaic 的显著变更，面向使用者和维护者，而不是 git 日志的简单堆砌。格式基于 [Keep a Changelog 1.1.0](https://keepachangelog.com/zh-CN/1.1.0/)。版本号位置沿用 `X.Y.Z`；自 0.5 领域线起采用项目约定的“大版本 / 领域中版本 / 特性小版本”规则：第二段按领域聚合系统性交付，第三段每次只交付一个特性，不宣称遵循严格 [Semantic Versioning](https://semver.org/spec/v2.0.0.html) 的功能与修复升位规则；历史版本不回溯改写。

## [Unreleased]

### Added

- 新增 Agent 生命周期状态机与事件流（0.5.1，0.5 长期 Agent 与职责领域第二件；负责人指令 2026-10-29："项目计划和设计补充了同步机制，关注下需求变更的内容和范围。还是设计和测试先行，开发0.5.1版本"——需求变更经核对：交付计划 v2.04 新增 0.10 个人数据备份与多设备同步领域（RFC-0019 S1–S6 → 0.10.0–0.10.5；自动同步另立 RFC 预排 0.10.6–0.10.13），0.5–0.9 排期不变，0.5.1 范围不受影响；ADR-0018）。**状态机**（RFC-0018 §3.2）：`active ↔ suspended；active|suspended → retiring → retired`（retired 唯一终态；active 不代表正在运行），suspend/resume/retire 显式转移，非法转移一律 409 可见拒绝（不静默幂等——重放方须重取终态）；retire 经承诺门（"处置所有承诺后才能 retired"）：0.5.1 无 Mission/Binding/Goal 域、承诺集恒空，同调用完成 retiring→retired（两段事件各恰一条），`CommitmentChecker` 端口留作 0.5.2+/0.6 各域接入缝（未处置时停留 retiring 并返回 `outstanding_commitments` 清单）。**事件流（migration v8）**：`agent_events` append-only 流（seq 全表单调 = 流序；事件体 `created`{source,adapter,display_name} / `status_changed`{from,to,reason}）；v7 存量库开库幂等回填（每 Agent 按创建序补恰一条 created，重开不重复），`agents` 表自此降为投影（转移 = 单事务[状态校验 → 更新投影 → 追加事件]，事件流 ⊇ 全部历史可重建投影；建行与 created 事件同事务——原子认领不变量扩展）。**同步与生命周期解耦**：同步只建不改——suspended/retired 身份的 bot_id 重现（重装/重扫/重绑）不复活状态，新 BotID 仍以 active 出生；退役 ≠ 卸载可执行项（身份级状态，行为管制随消费域 0.5.2+/0.6 落地）。**API（契约冻结）**：`POST /v1/identity/agents`（手动建，source=manual、active 出生、无别名——为 Slot 任职/Worker 晋升预留主体）、`POST /v1/identity/agents/{agent_id}/lifecycle`（suspend/resume/retire → `LifecycleResult{agent, outstanding_commitments}`；404 `identity_not_found` / 409 `invalid_transition`）、`GET /v1/identity/agents/{agent_id}/events`（只读审计面）；写面过生产写门（Origin 回环 + Content-Type + X-Owner-Token，与 harness 手动登记同口径）；`IdentityAgent.status` 收紧为四值枚举。**并发**：SQLite 写事务串行化下并发转移恰一胜者（败者 409），8 路并发 UT/IT 钉住（事件恰一条、终态一致）。**UI**：设置页 Agent 区新增"Agent 身份与生命周期"——身份列表（展示名/adapter/状态徽标/手动建标记/挂靠可执行数）、暂停/恢复/退役按钮（退役确认）、生命周期事件懒加载明细。设计与测试先行：ADR-0018 + OpenAPI 契约冻结（apigen/TS 双链重生成）→ 生命周期 UT 9 例 / SQLite IT 5 例（含 v0.5.0 真旧库回填 + 并发恰一胜者）/ 端点测试 3 例先红后绿；vet（it st）+ `-race` 全量 + `-tags it` + `-tags st` + SPA tsc/build 全部通过；真机冒烟（真实 CLI 扫描 5 身份）：全转移链 + 重放 409 + 终态 409 + 事件流 5 条 payload 语义 + 手动建/空名 400 + 写门 401/403 + 重启退役不复活/手动建持久 ALL-PASS。

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
