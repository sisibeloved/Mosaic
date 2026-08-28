# 特性设计说明书（RFC 提案）：RFC-0008 身份、多租户与授权

**状态 (Status):** Draft

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-08-25

**更新日期 (Updated):** 2026-08-25

**相关 Issue/PR:** #TBD（RFC 入库时创建 tracking issue）

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| RFC 编号 | 0008 |
| 系列位置 | RFC 序列第 8 篇；无外部依赖，可与其他 RFC 并行 |
| 上游输入 | [架构设计说明书](../2026-08-13-mosaic-architecture-design.md) v0.7 §5.1.2、§2.4、§7.3.2、§8.2.5、§9.1.4、§9.2、AR-012 |
| 吸收开放问题 | OQ-06（多人类协作中谁拥有 pause/policy/删除权限）——本 RFC 提出裁决建议 |
| 下游 | 全部命令面的授权检查、`internal/auth`（LE-16）实现、RLS 绑定（ADR-0004 联动） |

## 0.1 修订记录

| 版本 | 日期 | 修订人 | 说明 |
|---|---|---|---|
| v0.1 | 2026-08-25 | Mosaic 项目组 / ZCode | 初稿：OIDC 接入、身份与 Participant 映射、角色模型与权限矩阵（OQ-06 裁决建议）、双重隔离（应用层 + RLS）、bootstrap、会话与审计 |

# 1. 概述

## 1.1 简介

本 RFC 定义"谁是谁、谁能做什么"：OIDC 外接 IdP 的接入契约（Authorization Code + PKCE）、human/agent 统一 Participant 的身份映射、租户/房间两级角色模型与权限矩阵、应用层 + 数据库 RLS 双重隔离、首管理员的 bootstrap 流程与审计边界。"人和 Agent 平等"指发言语义对称，不指控制权限对称（架构 §2.4）——本 RFC 的权限矩阵是该非对称的正式定义。

## 1.2 动机

所有命令（RFC-0001）都需要 actor 解析与授权判定；OQ-06（pause/policy/删除权归属）是 Authorization 详细设计前必须关闭的决策；AR-012 的双重隔离与 11.3 的跨租户适应度函数需要可测试的判定规范。身份是 MVP 切片 1 的验收对象。

## 1.3 目标

### 1.3.1 目标

1. 定义 OIDC 接入契约（流程、claims 映射、会话管理）；
2. 定义用户/Participant 身份模型与映射规则；
3. 定义角色模型与权限矩阵，吸收 OQ-06 裁决建议；
4. 定义授权执行点（Gateway + 领域层双重检查）与 ABAC 条件；
5. 定义 RLS 第二道防线的绑定契约；
6. 定义 bootstrap 与 break-glass 流程；
7. 定义审计事件范围。

### 1.3.2 非目标

- 企业 SCIM/联邦（演进项，架构 §8.2.5）；
- Agent Profile 注册管理（RFC-0002）；
- 房间 Policy 参数（RFC-0003）；
- 细粒度 policy engine 引入（演进项）；
- IdP 选型与部署（部署方提供，架构 §8.6）。

# 2. 用例分析

| 用例 | 功能点 | 关键指标 | 安全隐私与 DFX 约束 |
|---|---|---|---|
| I-01 OIDC 登录 | Code + PKCE；claims → 用户映射；会话建立 | 登录跳转往返 < 2s（IdP 除外） | state/nonce 校验；短 session；本地不存密码 |
| I-02 跨租户隔离 | 双重隔离；token 无法跨租户读写 | 鉴权 P95 < 20ms（角色缓存） | Tenant A 的 token 无法查询 Tenant B 任何资源（11.3 fixture） |
| I-03 Bootstrap | 首管理员一次性 token 或 IdP group | token 使用后失效 | 失败不打印 Secret（9.2.1.1） |
| I-04 权限矩阵执行 | pause/policy/删除/导出/保送/merge/审批各归其位 | 越权同步拒绝 | 全部管理动作入审计 |
| I-05 角色变更 | 房间内升级/降级；事件化 | 变更即时生效（下一命令） | 最小授权原则；owner 不可自降为孤儿（至少保留一名） |
| I-06 会话生命周期 | 过期/续期/登出；WS/SSE 连接随会话 | 过期连接 30s 内切断 | 会话固定防护 |
| I-07 break-glass | owner 紧急接管；强审计 | 触发即告警 | 事后强制 review（9.2.2） |

# 3. 方案设计

## 3.1 总体方案

### 3.1.1 OIDC 接入契约

- 流程：Authorization Code + PKCE（S256）；`state` + `nonce` 强校验；
- claims 映射：`sub → external_subject`；`(tenant_id, external_subject)` 唯一（架构 §7.3.2 users 表）；`email/name` 仅展示；
- 会话：短 TTL 访问令牌（默认 60 分钟）+ 刷新；key rotation 按 IdP JWKS 自动；
- 多 IdP：v0.1 单 IdP per 部署；多 IdP 为演进（租户级绑定）。

### 3.1.2 身份与 Participant 模型

- Human：OIDC 登录 → user → 以 Participant 加入 Room（`participant.admitted`）；
- Agent：Agent Profile（RFC-0002 注册）实例化为 Participant；进程信任边界即身份边界（无网络认证）；
- `identity_ref`：human 为 user_id、agent 为 profile_id+version；Session 生命周期独立于身份（agent 离开不删历史，架构 §3.3.1）。

### 3.1.3 角色模型与权限矩阵（OQ-06 裁决建议）

租户级角色：`owner / admin`；房间级角色：`moderator / member / observer`。语义对称、控制非对称（架构 §2.4）：

| 动作 | human member | moderator | admin/owner | Agent |
|---|---|---|---|---|
| 发言 / 点名 / 提交 Intent（fork/merge 提议） | ✓ | ✓ | ✓ | 经协议 |
| **pause 自动讨论**（含打断） | ✓ | ✓ | ✓ | ✗ |
| endorse（保送 Intent） | ✓ | ✓ | ✓ | ✗（不能保送） |
| 工具审批（只读） | ✓ | ✓ | ✓ | ✗ |
| policy.changed（模式/权重） | ✗ | ✓ | ✓ | ✗ |
| merge 确认 / 收束接受（consensus/decision 型） | ✗ | ✓ | ✓ | ✗（纯 Agent 房间按 quorum，0005） |
| thread 生命周期（close/reopen/abandon） | ✗ | ✓ | ✓ | ✗ |
| 工具审批（带副作用，v0.1 无） | ✗ | ✓ | ✓ | ✗ |
| 删除 / 导出 / Agent Profile 注册 | ✗ | ✗ | ✓ | ✗ |
| break-glass / 租户管理 | ✗ | ✗ | owner | ✗ |

裁决原则：**高紧急低风险动作放宽**（pause 全员——500ms 抢占承诺要求低摩擦）；**高破坏动作收紧**（删除 admin+）；**判断权归人**（保送、收束确认仅人类）。

### 3.1.4 授权执行

- 双重检查：Gateway 统一解析 actor + 领域层复核（UI 禁用不是安全边界，9.2.1.3）；
- actor 绑定：命令中身份字段被忽略并告警（RFC-0001）；
- ABAC 条件：资源可见性（visibility/participant_scope）与资源状态（如 Thread 可写）由领域层判定；RBAC 判"角色能否"，ABAC 判"此对象此人此刻"。

### 3.1.5 RLS 第二道防线

- 所有查询经事务级 `SET LOCAL app.tenant_id = ...` 绑定（ADR-0004）；
- 关键表 RLS 策略以 tenant 为首要条件；应用层过滤之外的兜底；
- CI：跨租户 fixture（伪造 tenant 注入、连接池串号）。

### 3.1.6 Bootstrap 与 break-glass

- 首管理员：一次性 bootstrap token（部署时生成，使用即失效）或 IdP 特权 group；
- break-glass：owner 紧急接管（改 Policy/移除成员），触发即时告警并进入强制事后 review 队列。

### 3.1.7 审计事件

登录/登出、成员与角色变更、Policy 变更、审批、导出、删除、break-glass、管理员读取敏感资源（架构 §8.2.3 Audit 列表）；审计独立于日志管道，不可由普通成员关闭（11.4"审计关闭尝试告警"）。

## 3.2 技术选型

| 决策点 | 选择 | 放弃方案与原因 |
|---|---|---|
| OIDC 库 | go-oidc + x/oauth2 | Auth.js/双会话权威：与 Go Gateway 侧会话冲突（评审共识） |
| 授权模型 | 内部 RBAC 表 + ABAC 条件函数 | OPA 类 engine：规则规模不需要，增加组件与延迟 |
| 隔离 | 应用层 + RLS 双重 | 仅应用层：AR-012 明确要求双重 |
| 会话 | 自签短 TTL 令牌 + 刷新 | 三方 session 存储（Redis）：v0.1 无多实例共享需求（ADR-0001 联动） |

## 3.3 功能与性能设计

- **内部端口原型**：`AuthzPort: ResolveActor(token/session) -> Actor / Decide(actor, action, resource) -> allow|deny+reason / IsHuman(actor)`；
- **性能目标**：鉴权 P95 < 20ms（角色缓存）；RLS 开销 < 5% 查询延迟；
- **影响范围**：`internal/auth`（LE-16）、Gateway 中间件、RLS 迁移、跨租户 fixture、审计 sink。

## 3.4 安全隐私与 DFX 设计

- STRIDE 对应：Spoofing（actor 绑定 + PKCE）、Elevation（权限矩阵 + 双重检查）、Repudiation（审计）、Info Disclosure（RLS + 可见性）；
- CSRF：命令均 Bearer 令牌 + 非 cookie 会话（SPA 形态天然缓解）；CSP 基线归 Web 端；
- 会话固定/重放：nonce + 短 TTL；
- 审计完整性：审计写路径独立凭据，应用只增不删（删除归 0010 法定策略）。

## 3.5 编程与调用设计

### 3.5.1 编程模型基本设计

- **开发环境**：本地 Dex/Keycloak 容器做 IdP（9.2 建议，不自建）；权限矩阵 fixture；
- **开发约束**：新命令必须登记权限矩阵行，缺行 CI 拒绝（防漏判权）；
- **可验收设计**：跨租户 fixture、矩阵全行正反用例、bootstrap 一次性断言。

### 3.5.2 接口定义与设计

#### OIDC 回调端点

接口描述：`GET /auth/callback`：Code + PKCE verifier 换令牌，建立会话。

- 异常处理：state/nonce 失败 → 拒绝并审计；用户映射失败进入注册流程或拒绝（Policy）。
- 约束说明：仅 HTTPS；token 不落日志。
- 变更说明：首版。
- 调用参考代码：（SPA 发起 PKCE 跳转。）

#### Decide（内部端口）

接口描述：统一授权判定（Gateway 与领域层共用）。

接口原型：`AuthzPort.Decide(actor, action, resource) -> Decision`

输入/输出参数：

| 参数名称 | 输入/输出 | 类型 | 描述 | 取值范围 |
| --- | --- | --- | --- | --- |
| actor | 输入 | Actor | 解析后的主体（含角色缓存） | — |
| action / resource | 输入 | — | 动作与目标（room/thread/...) | 矩阵登记动作 |
| allow / deny + reason | 输出 | — | 判定与原因（入审计） | — |

- 异常处理：未登记动作 → deny + 告警。
- 约束说明：判定确定性、可回放。
- 变更说明：首版。
- 调用参考代码：`if !authz.Decide(actor, "pause_automation", room) { return denied }`。

#### 角色管理命令

接口描述：`command_kind = "change_role" | "admit_participant" | "remove_participant"`。

- 异常处理：自降保护（最后一名 owner/moderator 不可降/移）。
- 约束说明：moderator+ 可执行；全审计。
- 变更说明：首版。
- 调用参考代码：（命令通道。）

### 3.5.3 编程手册设计

《权限与角色说明》（面向部署方与管理员）：角色职责、矩阵速查、bootstrap 步骤、break-glass 流程、与 IdP group 的映射建议。

# 4. 缺点和风险

| 风险 | 影响 | 应对 |
|---|---|---|
| 矩阵过严卡主流程 | pause/保送被阻 | 高紧急动作放宽（member 可 pause/endorse）；矩阵 Policy 内微调空间 |
| 矩阵过松越权 | 破坏性误操作 | 破坏动作 admin+ + 审计 + break-glass 告警 |
| IdP 单点 | 无法登录 | 部署方 HA；演进多 IdP |
| 角色缓存失效延迟 | 变更后短暂旧权限 | TTL 短（30s）+ 敏感动作旁路缓存 |
| RLS 与应用层规则漂移 | 双重防线假象 | fixture 同源生成两侧用例 |

# 5. 现有技术

| 参考 | 借鉴 | 差异 |
|---|---|---|
| OIDC/OAuth2.1 规范 | PKCE、state/nonce、短会话基线 | Mosaic 单 IdP 起步 |
| NIST RBAC 模型 | 角色-权限-会话分离 | Mosaic 叠加 ABAC 可见性条件 |
| Slack/Discord 频道权限 | 房间级角色与操作粒度参考 | Mosaic 动作绑定讨论协议（pause/endorse/收束） |
| Postgres RLS 多租户实践 | 行级隔离兜底模式 | 绑定契约与 fixture 化为本 RFC 要求 |

# 6. 未解决问题

1. **OQ-06 终裁**：矩阵默认值确认（尤其 pause 全员与收束确认 moderator+ 两条）。
2. observer 角色是否进 v0.1（当前建议进——只读旁听）。
3. 会话 TTL 与刷新窗口默认值。
4. break-glass 的告警通道（复用观测告警 vs 独立）。
5. Agent 的权限继承：Profile capability 与房间角色正交的边界确认。
6. 多 IdP 的租户绑定模型（演进预案）。

# 7. 附录

## 7.1 参考资料

- 架构设计说明书 v0.7：§2.4 对称性、§5.1.2 IF-AUTH-OIDC、§7.3.2 身份表、§9.1.4 信任边界、§9.2 安全设计、AR-012
- [RFC-0001](2026-08-25-rfc-0001-room-protocol.md)（actor 绑定）、[RFC-0002](2026-08-25-rfc-0002-agent-protocol.md)（Profile 注册）、[ADR-0004](../adr/ADR-0004-data-access-migration.md)（RLS 绑定）
- [OpenID Connect Core 1.0](https://openid.net/specs/openid-connect-core-1_0.html)、[OAuth 2.1](https://oauth.net/2.1/)

## 7.2 术语表

| 术语 | 英文 | 含义 |
|---|---|---|
| 主体 | Actor | 命令的发起者（human/agent/system），由鉴权解析 |
| 语义对称 | Semantic Symmetry | 人与 Agent 都是可观察、可发言、可回应的 Participant |
| 双重隔离 | Dual Isolation | 应用层过滤 + 数据库 RLS |
| 紧急接管 | Break-glass | owner 的紧急特权路径，强审计与事后 review |
| 一次性引导令牌 | Bootstrap Token | 首管理员建立通道，使用即失效 |

## 7.3 文档更新计划

- 评审期（Draft → Reviewing）：收敛矩阵默认值与未决 1–3；
- Accepted 后：RLS 迁移与跨租户 fixture 进 CI；`internal/auth` 实现排期；
- 后续：SCIM/多 IdP 演进时修订。
