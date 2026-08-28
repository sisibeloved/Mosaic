# Mosaic 交付与进度规划——个人版 v1.0（Windows / macOS 成品）

## 0. 文档控制

| 项目 | 内容 |
|---|---|
| 文档类型 | 交付与进度规划（进度归进度：本文不改设计结论，只裁定交付范围与顺序；设计变更走 RFC/ADR） |
| 版本 | v1.2 |
| 日期 | 2026-08-28 |
| 拟制 | Mosaic 项目组 / ZCode |
| 上游 | [架构设计说明书](../design/2026-08-13-mosaic-architecture-design.md) v0.9；[RFC-0001～0011](../design/rfc/)；[ADR-0001～0007](../design/adr/)；[Harness 调研报告](../design/research/2026-08-25-harness-survey.md) |
| 交付目标 | **个人完全可用的 App**：单用户本地运行，一等公民支持 Windows 与 macOS；不是验证性半成品 |
| 关键假设 | 1 名工程师 + AI 协作开发；工期为日历周估计，已含约 20% 缓冲；ZCode headless 缺口不阻塞主线（分级晋级） |

## 0.1 修订记录

| 版本 | 日期 | 修订人 | 说明 |
|---|---|---|---|
| v1.0 | 2026-08-25 | Mosaic 项目组 / ZCode | 初版：交付目标与 DoD、四个形态决策（含建议）、范围裁定、里程碑 M0–M5、双平台工程要求、风险、治理 |
| v1.1 | 2026-08-25 | Mosaic 项目组 / ZCode | M0 执行更新：形态决策确认（ADR-0008~0010）；新增测试分层约定（§5.5，UT/IT/ST + TDD backlog 机制）；M0 勾选同步（脚手架/端口与 echo/测试分层完成；协议生成链与严格校验、SQLite/Wails spike、Apple 账号待办） |
| v1.2 | 2026-08-28 | Mosaic 项目组 / ZCode | 负责人裁定：**个人使用，不购买 Apple 开发者账号**——删除公证硬性项，macOS 改为 ad-hoc 签名 + 首次打开指引（DoD 1、M4、风险表同步）；sqlite-vec 冒烟移至 M3 记忆层入口（M0 三用例不含向量检索）；M0 出口判据的"命令→事件→游标续传"往返明确为进程内 IT（HTTP/SSE 形态属 M1） |
| v1.3 | 2026-08-28 | Mosaic 项目组 / ZCode | M1 执行更新（TDD 切片 A/B）：命令处理域（幂等 receipt/乐观并发/严格校验，UT+IT 含并发竞态）+ 存储端口化（room.AtomicStore/EventReader，MemStore/SQLite 双实现）+ HTTP 命令 API 与 SSE 游标订阅（对外无 seq/tenant，RFC-0001 P0 落地）+ outbox 分发器 + 房间引擎（echo 全轮事件链）；**TestDiscussionLoop_ST 北极星转绿**（建房→发消息→引擎轮→agent 发言→断线重连续传→HTTP 幂等重放）；TDD 红灯驱动修复 commit 竞态分支 (nil,nil) 缺陷。待做：native-codex 适配器、Attention 正式实现、Context 七层、draft 流、进程管理 |

# 1. 交付目标与"完全可用"定义

产品形态：**本地单进程桌面应用**——一个安装即用的 App，用户在自己电脑上创建房间、接入本机已安装的 agent CLI（Codex / Kimi Code / ZCode 等），进行可回放、可收束的多 Agent 讨论。所有数据在本机，可备份可导出可删除。

**v1.0.0 验收清单（Definition of Done，逐条可检）**：

| # | 类别 | 验收项 |
|---|---|---|
| 1 | 安装 | Windows 10/11 提供安装器（或免安装绿色版）；macOS 13+（Intel/Apple Silicon）提供 dmg（个人分发：ad-hoc 签名，不做公证，随附首次打开指引）；双平台首次启动向导 ≤ 5 分钟进入第一个房间 |
| 2 | Agent 接入 | 向导检测本机已安装的 CLI（Codex/Kimi Code/ZCode），引导安装与登录态检查；v1.0 至少 **2 个真实适配器 + echo** 通过 conformance |
| 3 | 讨论闭环 | 创建房间、加入人类与 ≥2 个真实 agent；Open Floor / Roundtable（含 rebuttals）/ Deep Dive 三模式；消息、点名与定向交锋、暂停/打断、人类保送、draft 流、记分卡查询 |
| 4 | 结构 | Thread fork / pause / resume / close / reopen / merge；Timeline + 最小 Graph 视图（显式关系与系统推断视觉区分） |
| 5 | 收束 | closure round（conclude/object/abstain）、六种 Capsule 类型查看、重开流程；预算耗尽产生 Pause Capsule 而非伪结论 |
| 6 | 记忆与历史 | 全量历史回看与检索；Capsule 入 Memory；Context Receipt 可查 |
| 7 | 数据自主 | 一键备份/恢复（单目录/单文件）；全量导出（manifest + NDJSON）；删除与墓碑；版本升级自动迁移且不丢数据 |
| 8 | 稳定性 | 双平台 72h 无人值守连续运行：无内存失控、无卡死、事件日志零丢失；进程崩溃后重启自恢复（grant epoch 保证无迟到污染）；本地日志 + 一键自诊断报告 |
| 9 | 文案与文档 | 中英双语界面；快速上手指南 + FAQ；本地数据与隐私说明（不上传任何内容） |
| 10 | 反半成品清单 | 无占位按钮、无"coming soon"、无必须看日志才能理解的失败；所有失败有用户可读的状态与恢复建议 |

# 2. 形态决策（进入 M0 前必须裁决，均为新 ADR 候选）

架构基线的机制设计（事件溯源、Room 单写序、适配器、Attention/收束协议）**全部保留**；以下只裁定个人版交付形态。

## D-1 存储：SQLite（WAL）+ sqlite-vec —— 建议

- **建议**：个人版用 SQLite（WAL 模式）+ sqlite-vec 替代 PostgreSQL/pgvector；
- **理由**：零依赖安装（个人 App 第一原则）；单文件备份即达标；单进程内 Room 单写序退化为互斥 + 事务，天然契合；sqlite-vec 覆盖有界混合检索（个人数据量下精确/遍历检索足够）；
- **传导**：ADR-0003/0004 追加"个人版形态"修订（PG 保留为未来服务端部署形态的选型，不是被推翻）；机制映射——advisory lock → `BEGIN IMMEDIATE`，outbox `SKIP LOCKED` → 进程内提交后分发（崩溃恢复仍靠 outbox 表重放），RLS → 单用户天然满足（保留 tenant 常量列）；RFC-0001 契约本身存储无关，不受影响；
- **替代（放弃）**：内嵌 PostgreSQL（fergusstrange/embedded-postgres 类 + 自打包 pgvector）：保留全部 PG 机制，但安装体积、initdb 本地化问题、PG 自身升级迁移对个人 App 是持续负担——仅当 D-1 落地后发现 SQLite 并发/迁移硬伤时回退。

## D-2 身份：本地单用户免登录 —— 建议

- **建议**：OS 用户即 owner，无登录界面；Participant 模型保留（human = 本地 owner，agent = Profile 实例化）；
- **传导**：RFC-0008 在个人版降级为"内置 owner + 全权限"（OIDC/多租户/角色矩阵归服务端形态，代码保留接口、UI 不暴露）；审计仍全量落本地库。

## D-3 应用壳：Wails（Go + 系统 WebView）—— 建议

- **建议**：Wails v2 打包（Windows 用 WebView2、macOS 用 WKWebView），复用 React/Vite SPA（ADR-0002 不变）与 Go 单二进制；托盘常驻 + 点击打开窗口；
- **理由**：不捆绑 Chromium（体积/内存），Go 侧无语言切换；双平台原生安装器（NSIS / dmg）路径成熟；
- **替代（放弃）**：Electron/Tauri（引入 TS 主进程或重壳）；纯浏览器模式保留为开发态（`--browser` 启动参数），不作为交付形态。

## D-4 部署面裁剪（个人版不出现的东西）

| 裁剪项 | 处置 |
|---|---|
| OIDC IdP / 多租户 / RLS | 不出现（见 D-2） |
| Edge/Ingress/多副本/Worker 分离 | 单进程；API+worker 角色合并 |
| S3 对象存储 | 本地文件系统 Artifact 仓库（quarantine/扫描/引用机制照旧，RFC-0009 不变） |
| egress proxy / SSRF 面 | 本地无回调面；agent 进程出网不设强制边界（个人机器，用户自管），保留可选 allowlist 配置 |
| OTel 后端 / Prometheus | 本地结构化日志（slog JSON）+ 内置诊断页（指标只读视图）；OTLP 导出保留为可选开关 |
| NATS / Temporal / Redis | 永不（架构既定） |
| 远端 Harness 形态 | 不做（RFC-0002 演进项） |

# 3. 范围裁定（对照架构 §11.1 MVP 切片与 §11.2 演进阶段）

- **保留并交付 v1.0**：MVP 切片 1–13 的实质内容，但按"可用产品"而非"验证切片"组织（映射见第 4 节）；
- **降级/延后**：多人类协作与权限矩阵（服务端形态）；Claim/Evidence 在线投影（维持 RFC-0006 建议：离线 + 只读谱系）；外部 MCP 工具生态（RFC-0009 延后项）；
- **新增（原架构未覆盖的产品化项）**：安装器与签名公证、升级迁移、备份/恢复、onboarding 向导、自诊断、i18n、长跑稳定性——集中排在 M4；
- **演进阶段重述**：原"v0.1 验证（云端单区域）"重定向为"**v1.0 个人版（双平台桌面）**"；云端/多租户/SDK 外化保留为 v2+ 服务端形态的既有设计，不在本规划排期。

# 4. 里程碑

工期为 1 人 + AI 协作的日历周估计（含缓冲）。每个里程碑出口判据全部满足才进入下一个；进度以本文勾选框 + git tag（`milestone/mN`）跟踪。

## M0 定形与骨架（2 周）

目标：形态决策落地，仓库与协议工程立起来，主循环最小路径在双平台可跑。

- [x] D-1/D-2/D-3 裁决并落 ADR（2026-08-25 负责人确认：ADR-0008 存储、ADR-0009 身份、ADR-0010 应用壳；ADR-0003/0004 已追加个人版形态注记）
- [x] RFC-0001/0002 推进 Approved（2026-08-25 负责人确认；首轮审校修订 v0.4/v0.5 后批准）
- [x] monorepo 脚手架：Go module + apps/web + api/room-protocol + CI（vet/test/race/build，windows+macos+ubuntu 三矩阵）
- [x] 协议工程：envelope/command/Attention 事件族 Schema + TS/Go 边界模型生成 + 兼容性 fixture 门禁（2026-08-28 落地：Attention 六事件 payload Schema（严格写 additionalProperties=false）+ valid/invalid fixture 双侧 + santhosh-tekuri 严格校验门禁（变异验证过）+ Go 边界 struct round-trip 门禁 + `gen-ts.sh`→`gen/ts` TS 产物；OpenAPI/oapi-codegen 随 M1 命令 API）
- [x] HarnessPort + supervisor 最小实现 + echo 适配器（conformance 起步：确定性/取消/Stale/注册链路测试）
- [x] 测试分层体系（TDD）：UT/IT/ST 三层（构建标签 it/st + CI 分步执行）+ supervisor UT 全覆盖（含并发首启唯一性红灯用例）+ 真二进制 ST（healthz/优雅退出/非法参数）+ TDD backlog 占位（严格 Schema 校验门禁、M1 端到端闭环）
- [x] 存储 spike：SQLite WAL 冒烟（事件 append / 游标续传 / outbox 重放各一条 IT 用例，驱动 modernc.org/sqlite 纯 Go 免 CGO）；sqlite-vec 向量冒烟移至 M3 记忆层入口（M0 主流程不含向量检索）；不通过则触发 D-1 回退评审（**2026-08-28 通过**：三用例 + 8×5 并发追加串行化证明全绿（-race）；发现并修复 per-connection pragma 须走 DSN 的坑，已记 ADR-0008；D-1 维持，不触发回退评审）
- [x] Wails 最小壳 spike（2026-08-28：apps/desktop 独立 module，wails v2.15；Windows amd64 与 macOS arm64 双目标在 headless Linux 纯 Go 交叉编译通过；真机运行时验证随 M2 SPA 接入）
- [x] 进程内"命令→事件→游标续传"往返 IT（echo 适配器参与，post_message fixture → message.posted → echo 三段任务 → agent 消息 → 续传只收新事件 → outbox 排空）
- **出口判据**：三个 ADR 落档；CI 在 Windows 与 macOS runner 双绿；存储 spike 三用例 + 进程内"命令→事件→游标续传"往返 IT（echo 适配器参与）通过；严格 Schema 校验门禁（valid 全过 / invalid 全拒）进 CI。HTTP/SSE 形态的同名闭环属 M1 出口判据。

## M1 核心闭环（4 周）

目标：在双平台上，人与 1 个真实 agent 完成完整、可回放的一轮讨论。

- [ ] Room/Thread 最小生命周期 + `message.posted` 全链路（命令→事件→SSE 游标订阅→UI Timeline）**（2026-08-28：Room 创建 + message.posted + SSE 游标订阅/断线重连已落地（ST 北极星绿）；Thread 生命周期与 UI 待做）**
- [ ] native-codex 适配器（headless 模式对接 + 结构化块解析 + 会话恢复）通过 conformance 三件套
- [ ] Attention 最小实现：硬资格 + 记分卡（默认权重）+ MMR + FloorGrant（epoch）；Open Floor 单模式先行
- [ ] Context 组装七层最小版 + Context Receipt 落库
- [ ] draft 流（DraftUpdate 安全子集）+ 暂停/取消 + 迟到拒绝（epoch）
- [ ] 双平台进程管理：spawn/健康检查/退避重启/优雅退出（含 Windows 信号语义）；CLI 检测（codex/kimi/zcode 是否安装）
- **出口判据**：Windows 与 macOS 各完成一次"创建房间 → 人发消息 → Codex agent 评估→获选→生成→发布 → 断线重连续传 → 回放重建一致"；崩溃注入后无迟到污染（fixture 断言）。

## M2 可用的讨论体验（4 周）

目标：日常讨论体验成形，开始自用（dogfood）。

- [ ] 三模式（Open Floor / Roundtable 含 rebuttals / Deep Dive）+ Policy 参数配置面
- [ ] reveal 三策略（sequential / simultaneous / independent_then_cross）
- [ ] 点名与定向交锋快速通道（slot 上限 + 交锋链）
- [ ] 人类保送（intent.endorsed）+ 记分卡面板（band + 未选 Intent 可查）
- [ ] Thread fork/pause/resume/close/reopen/merge + Timeline/Graph 双视图（显式 vs 推断区分）
- [ ] Kimi Code 适配器晋级（native 或 ACP，按 spike 证据）→ 凑足 2 个真实适配器
- [ ] 历史查询双通道之一落地（MCP server 或结构化请求，HistoryItem 视图）
- **出口判据**：连续 5 个工作日真实自用（≥1 场多 agent 讨论含分叉与合并）；零数据丢失；体验阻塞项清单清空或降级记录。

## M3 收束、记忆与投影（3 周）

目标：讨论能优雅收束、被记住、可检索。

- [ ] closure round + 合格异议判定 + 六种 Capsule + Pause Capsule + reopen 流程（RFC-0005）
- [ ] 四层 Memory + 混合检索（sqlite-vec）+ Capsule 一等 Memory；记忆查看/编辑 UI
- [ ] 结构投影最小版：reply 基图 + 重复检测 + 漂移签名（供 repetition_risk 与重聚焦）；Claim 投影按 flag 离线启用
- [ ] Evidence Request 创建/认领/解决 + 重开提议
- [ ] 导出（manifest + NDJSON）与删除级联 + 墓碑（RFC-0010 个人版范围）
- **出口判据**：一场真实讨论以 bounded_disagreement 收束并成功按新证据重开；导出包在干净环境重放一致；删除后全库无残留（fixture）。

## M4 产品化——"不是半成品"专项（4 周）

目标：把工程闭环变成任何人可安装可日常使用的产品。验收对照第 1 节 DoD 全表。

- [ ] Wails 壳：安装器（Windows NSIS/绿色版 + macOS dmg）、应用图标、托盘常驻、首次启动向导（存储初始化 → CLI 检测/安装引导 → agent 登录态检查 → 建房演示）
- [ ] macOS ad-hoc 签名 + 首次打开指引（个人分发不做公证：未购 Apple 开发者账号，指引右键→打开或 `xattr -cr`；若未来公开分发再评估购号公证）；Windows 代码签名（可选，无则 SmartScreen 提示文案）
- [ ] 升级与迁移：版本检查 + 一键下载替换 + 启动时自动迁移 + 迁移失败回滚保护
- [ ] 备份/恢复一键化（副本即拷贝 + 校验）；自诊断报告（版本/环境/日志尾/指标快照打包）
- [ ] 72h 无人值守稳定性专项：内存/句柄监控、泄漏修复、崩溃自恢复演练（双平台各一轮）
- [ ] i18n 中英；快速上手 + FAQ + 本地隐私说明
- [ ] 反半成品走查：全 UI 走查失败态文案与恢复建议；无占位功能
- **出口判据**：第 1 节 DoD 1–10 全绿（由用户按清单逐项验收）；双平台各一台干净机器完成全新安装→向导→首场讨论全程录屏存档。

## M5 第二梯队与打磨（M4 后并行/后续，2–3 周）

- [ ] ZCode 适配器（依赖 headless 缺口解除；未解除则保持能力降级可见，不阻塞 v1.0.0 发布）
- [ ] 评测框架 lite：回放一致性门禁 + 指标只读页（RFC-0011 个人版裁剪）
- [ ] 性能：10 万事件房间快照/续传达标；冷启动 < 10s
- [ ] macOS/Windows 细节打磨（通知、快捷键、深色模式跟随系统）

## 时间线汇总

| 里程碑 | 工期 | 累计（日历周） |
|---|---|---|
| M0 定形与骨架 | 2 周 | 2 |
| M1 核心闭环 | 4 周 | 6 |
| M2 可用讨论体验 | 4 周 | 10 |
| M3 收束/记忆/投影 | 3 周 | 13 |
| M4 产品化 | 4 周 | 17 |
| M5 二梯队与打磨 | 2–3 周（可并行尾部） | ~18–20 |

约 4.5–5 个月交付 v1.0.0；v1.0.0 的定义不含 ZCode 适配器（分级晋级语义，RFC-0002 v0.5）。

# 5. 双平台工程要求（贯穿所有里程碑）

1. **CI 矩阵**：Windows（latest）+ macOS（arm64 + amd64）双 runner，从 M0 起即为合入门禁；conformance fixture 双平台各跑一遍；
2. **平台差异登记**：进程信号语义（Windows 控制台事件 vs POSIX）、路径/编码、agent CLI 的 per-OS 启动参数（adapter_options per-OS 覆盖，RFC-0002）、WebView 差异（WebView2 运行时检测）——统一登记在 `docs/plan/platform-notes.md`（M1 建立）；
3. **不做 WSL 依赖**：Windows 上一等公民为原生；WSL 仅作为用户自选的 agent 运行环境，不做要求；
4. **发布物**：每里程碑出双平台构建产物；v1.0.0 起附带 checksum 与（如签署）签名；
5. **测试分层（TDD）**：**UT**——无构建标签、进程内纯内存、随包存放，`go test ./...` 常跑；**IT**——构建标签 `it`，跨模块真实组件装配（如 supervisor × echo），`go test -tags it`；**ST**——构建标签 `st`，真实二进制 + 真实 HTTP（现场 `go build` 后拉起进程），`go test -tags st`。CI 依次执行 vet（含全部标签）→ UT → IT → ST。新行为先写测试：未实现的以 `t.Skip` 标注 **TDD backlog**（转绿即销账），禁止无测试的实现合入。

# 6. 依赖与风险

| 依赖/风险 | 影响 | 缓解 |
|---|---|---|
| ZCode headless 缺口（issue #29） | ZCode 适配器无法进入 v1.0 | 分级晋级（已定）：v1.0 至少 2 个真实适配器（Codex + Kimi）；内部推动需求，解除后按 M5 晋级 |
| macOS Gatekeeper 拦截未公证 app | 个人机器首次打开多一步确认 | 已裁定不购 Apple 开发者账号：ad-hoc 签名 + 首次打开指引（右键→打开 / `xattr -cr`）写入 README 与 FAQ；个人使用可接受，公开分发需求出现时再购号公证 |
| Codex/Kimi 输出 schema 漂移 | 适配器解析失败率上升 | 版本固定（RFC-0002 修订 #14）+ fixture 阈值告警 + 适配器更新通道 |
| D-1 SQLite 回退风险 | 存储层返工 | M0 内 spike 冒烟三用例前置排雷；回退预案（内嵌 PG）已评估 |
| 单人带宽 | 排期滑移 | 范围刚性顺序：M4 产品化不可裁（"不是半成品"是硬要求）；可裁项仅 M5 与 M2 的非主流程模式增强 |
| agent CLI 本机安装/登录态多样性 | onboarding 复杂 | 向导只承诺支持矩阵内版本；检测失败给出明确指引与降级说明 |
| Wails 深水区（托盘/更新/通知） | M4 拖期 | M0 做最小壳 spike；通知类降级为应用内 + 托盘角标 |

# 7. 治理

- **进度跟踪**：本文勾选框为唯一进度事实源，每周更新；里程碑完成打 git tag 并附验收记录（录屏/清单勾选）；
- **范围变更**：任何影响 DoD、形态决策或里程碑出口判据的变更，须先修订本文并在修订记录登记（涉及设计的同步走 RFC/ADR，不逆向）；
- **验收**：M1/M2/M3 由开发者自验 + 录屏；M4 DoD 由用户逐项验收；v1.0.0 发布 = DoD 全绿 + 双平台安装录屏 + 干净机演练通过。

# 8. 附录：M0/M1 任务级分解（示例粒度）

**M0**：ADR 三篇 / RFC-0001-0002 Approved 流程 / 脚手架与 CI 双平台 / envelope+command+Attention 事件族 Schema 与生成链 / fixture 门禁（含 invalid 反例与严格校验）/ HarnessPort+supervisor 骨架 / echo 适配器 / SQLite spike（append、cursor、outbox 重放）/ 进程内命令→事件→游标续传往返 IT / Wails 最小壳 spike。

**M1**：迁移框架（goose→SQLite 方言评估）/ room_events+outbox 表 / 命令 API+幂等 receipt / SSE 订阅+opaque cursor / 快照四元组 / Timeline 最小 UI / codex 适配器（spawn、--json 解析、会话句柄、取消升级）/ 结构化块校验 / Attention 硬资格+评分+MMR+grant / Context 七层最小 / Receipt / DraftUpdate 流 / pause 命令链 / 崩溃注入测试。

（M2 起保持里程碑级，进入前一周细化。）
