# ADR-0026：本地 Environment / Runtime（0.6.3）

- 状态：Accepted（2026-10-30，随 0.6.3 实施）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3 对象表——Environment 工作目录/依赖/数据卷…、Runtime 一份可执行实例的 Harness/主机/能力与健康状态、RuntimeLease；§3 状态行——Runtime discovered→provisioning→ready↔busy、→unhealthy→provisioning；§6 候选流 runtime_events 与"先支持本机/已有 WSL 路径、复用 internal/harness"；§9.2 三种判断之"当前可执行"实时复核；§10.1 IF-RUNTIME 当前能力与就绪检查）、[ADR-0022](ADR-0022-readiness-admission.md)（授权交集——本域不做授权判定）、[ADR-0025](ADR-0025-execution-context-manifest.md)（readiness 是装配时点预演——实时复核归本域）、交付计划 v2.00（0.6.3：本地 Environment / Runtime：发现、探测、准备与健康检查）

## 背景与问题

0.6.2 之后，"执行什么"已成存档事实；但"在哪执行、用什么执行"仍无
权威记录：harness 注册表只有发现面（CLI 可执行程序与版本/登录态），
没有可准备、可健康检查、可被 0.6.5 调度消费的运行时对象，也没有
工作目录（Environment）的持久事实。RFC §9.2 明确：active Binding 或
旧的 readiness report 不能代替"当前可执行"（文件、登录态、工具、
Runtime 健康）的实时复核——本件交付这个复核的对象与操作面。

## 决策

### 1. 新域 internal/runtime（migration v16 三表）

```
environments：environment_id（env_ 前缀）主键；name + workspace_dir
  （UNIQUE）+ is_default（部分唯一索引——全库恰一默认）+ created_at/
  updated_at。无事件流（有明确所有者的持久记录，RFC §6）。
runtimes：runtime_id（rtx_ 前缀）主键；(adapter, host, distro, path)
  UNIQUE——发现幂等键；镜像 harness 登记事实（version/login/channel/
  bundle）+ 本域状态与绑定；host ∈ native|wsl（沿用 harness Runtime 词，
  字段名 host 免与域包名撞）。
runtime_events：runtime_id 流（seq 全表单调）——discovered /
  health_checked / state_changed。
```

包名 `runtime`（RFC §6 拟增 internal/runtime）；与他包同文件引用
标准库 runtime 时沿用既有 `goruntime` 别名惯例。

### 2. Environment 0.6.3 子集（命名工作区目录）

RFC 对 Environment 的完整定义（工作目录、依赖、数据卷、工具配置、
资源与连接要求）在 0.6.3 收敛为**命名工作区目录**：`name`（1–64 字）
+ `workspace_dir`（创建时验证/创建目录，失败 400）。默认环境启动时
自动确保（`<data>/workspace`，全库恰一）；不改不删——删除前处理
所有租用者随 0.6.4 租约落地（依赖/数据卷等扩展同批按需增列，
不落空壳字段）。

### 3. Runtime 状态机 0.6.3 子集

RFC 状态行的 0.6.3 裁剪：

```
discovered ──prepare（绑环境+确保目录+探测）──▶ ready
ready / unhealthy ──探测失败──▶ unhealthy（携 note）
unhealthy ──prepare（或探测恢复且已绑环境）──▶ ready
```

- `busy/draining/stopped` 随 0.6.4 租约与 0.6.5 执行；准备是同步
  操作，`provisioning` 不落库（不占状态名——避免把瞬时态伪装成事实）。
- 登录态是**记录事实不是 ready 门**：Runtime 健康指可执行实例可探测，
  Agent 认证（登录/凭据）由 0.6.5 调度门核（§9.2 三种判断分层——
  本域提供事实，不做授权与调度裁定）。
- ready 后允许 re-prepare 换绑环境（0.6.3 无租约无在途工作；换环境
  重建语义随 0.6.4 围栏代次收紧）。

### 4. 发现（sync）：harness 注册表 → runtime 行

- 批量单事务 upsert：新行入库 + `discovered` 事件；既有行只刷新镜像
  事实（version/login/channel），**不动状态与绑定**。
- 触发面：启动扫描完成后自动同步（app 装配，扫描 goroutine 内）；
  `POST /v1/runtimes/sync` 手动重同步（装新 CLI 免重启）；httpapi
  手动登记可执行程序成功后即同步。
- **消失路径归 harness**：可执行程序消失/重绑是 harness 的既有语义
  （ADR-0013 消失重绑）；本域不抢——runtime 行保留，下次探测/准备
  失败转 unhealthy 可见（`executable_gone`）。

### 5. 探测（health-check）与准备（prepare）

- harness 新增导出 `Reprobe(ctx, runner, id)`：按登记 ID 重跑
  `probeExecutable`（只读，不回写注册表）；**探测成功 = 文件存在
  （runner.Exists）且版本探测非空**，失败 note 指明 `executable_gone`
  或 `version_probe_failed`。
- `health-check`：单次实时探测，刷新 version/login/last_health_at；
  转移：成功且 state=unhealthy 且已绑环境 → ready（恢复）；成功且
  discovered → 保持（ready 须绑环境经 prepare）；失败 → unhealthy。
  每次记 `health_checked` 事件（健康历史可查——RFC §3"健康状态"）。
- `prepare`：绑定环境（缺省 = 默认环境；不存在 404）→ 确保工作区
  目录在（创建失败 409/500 按因）→ 探测 → 成功 ready / 失败
  unhealthy；记 `state_changed`（from/to/reason）。

### 6. HTTP 契约（写门禁照旧；读面无门）

| 端点 | 语义 |
|---|---|
| `GET /v1/runtimes` | 运行时列表（状态/健康/绑定） |
| `POST /v1/runtimes/sync` | 手动重同步（发现） |
| `GET /v1/runtimes/{runtime_id}` | 详情；`GET …/events` 健康与状态历史 |
| `POST /v1/runtimes/{runtime_id}/health-check` | 实时探测（RFC §9.2 复核面） |
| `POST /v1/runtimes/{runtime_id}/prepare` `{environment_id?}` | 准备（绑环境 + 探测 → ready/unhealthy） |
| `GET/POST /v1/environments`、`GET /v1/environments/{environment_id}` | 环境管理（POST 建即验目录） |

### 7. 端口与装配

runtime 域自定义 `Prober` 端口（app 适配 harness.Reprobe——域不
import harness）；`Sync` 的发现源由 app 适配（harness.List →
DiscoveredEntry）。0.6.5 Scheduler 经本域读"当前可执行"事实；
IF-RUNTIME 维护/排空随 0.6.4+。

## 非目标

- RuntimeLease/围栏代次（0.6.4）；执行、busy/draining/stopped、
  Scheduler 消费（0.6.5）。
- Environment 依赖/数据卷/资源与连接要求、改与删（租用者处置随 0.6.4）。
- 远端连接、容器隔离（RuntimeProvider 插件，RFC §6）；能力协商
  （capabilities 深度面）——0.6.3 只镜像 version/login 事实。
- 周期性自动健康巡检（手动 + 启动探测已覆盖首版；随 0.6.5 调度按需）。

## 验收要点

- AT-09 相关（受限环境维护的记录面）；AT-14/18 的就绪事实供给。
- 门控反例：探测失败 → unhealthy（executable_gone / version_probe_
  failed 可见）→ prepare 恢复 ready；重复发现幂等（同 (adapter,
  host, distro, path) 不新建行、状态不动）；默认环境恰一；未绑环境
  的 discovered 探测成功不升 ready。

## 后果

- "在哪执行、用什么执行"从隐式（harness 发现表）变为显式可管理事实；
  0.6.5 调度的实时复核有对象可查。
- harness 注册表仍是发现权威；本域是运行时生命周期的权威——两表
  镜像事实由 sync 单向搬运，不回写。

## 修订记录

- v0.1 2026-10-30 初稿（0.6.3 开工——设计先行）。
- v0.2 2026-10-30 审校修复登记（独立审校"1 P1 + 7 P2 + 1 P3，建议修复后定版"）：**(P1)** 手动同步入口先做有超时的宿主扫描再镜像（启动后新装 CLI 可发现；扫描失败不夺同步），补入决策 4。**(P2)** probeExecutable 版本探测两分支要求 exit 0（错误文本不得充当版本）。**(P2)** 健康检查与准备按运行时串行化（读-探测-写全程持锁——迟到检查不撤销期间准备）。**(P2)** 工作区目录 filepath.Clean 规范化后判重保存。**(P2)** discovered 事件由存储在事务内只为真正插入的行生成（并发首次同步零孤儿）。**(P2)** prepare 未知字段拒收。**(P2/P2/P3)** SPA：事件三态可重试、操作后历史重取、创建回调只清空等于提交快照的值。
