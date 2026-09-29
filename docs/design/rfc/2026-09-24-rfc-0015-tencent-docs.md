# 特性设计说明书（RFC 提案）：RFC-0015 腾讯文档集成 M1（人工面：token、文档共享与链接卡片）

**状态 (Status):** Accepted（负责人裁定 2026-09-24：按"路径 B 自建薄 MCP 客户端"实现；本轮纯人工面——不做 MCP 代理对外端点、不做 harness 注入；**文档级映射**而非块级同步；不需要变更推送）　**实现面于 2026-09-26 被 RFC-0016 泛化取代（txdocs → extdocs 多 provider，API 面 /v1/txdocs/* → /v1/extdocs/*）；本文档级映射与快照语义裁定仍有效**

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-09-24

**相关 Issue/PR:** #TBD

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| RFC 编号 | 0015 |
| 系列位置 | RFC-0014（协作文档）之后的首个**外部服务集成**——文档域与外部文档服务的文档级桥接；上游调研：tutti 的 tencent-docs Connector（官方 MCP + 个人 token，经 tsh-server 云端网关）——本 RFC 取其"薄客户端"精神、去云端网关（Mosaic 为 local-first 单进程，无云端对应物） |
| 上游输入 | 负责人 2026-09-24 指令（先按路径 B 实现；不要变更推送；要文档级映射——Mosaic 文档可共享到腾讯文档、腾讯文档链接发进群聊变卡片）；上游能力实证：`https://docs.qq.com/openapi/mcp`（curl 探测 401 Bearer token required；官方使用指南 2026-05-14） |
| 下游 | internal/txdocs 域、storage v5（doc_external_shares）、HTTP 契约（/v1/txdocs/*）、room-protocol message.posted.external_refs、SPA（设置页集成区 / 共享按钮 / TxDocCard）、sqlite IT、httpapi UT |

# 1. 动机与范围

群聊产出的 Mosaic 文档经常需要进入协作者的腾讯文档空间（公司分发、微信分享、在线批注）；反向，讨论中粘贴的 docs.qq.com 链接目前只是一条裸 URL。M1 把这两条路走通为**文档级映射**：共享 = 某时点的 markdown 快照在腾讯文档创建智能文档；链接卡片 = 发送时刻定稿的描述子快照。两者都是"一份事实"，不含双向同步与变更推送。

范围（M1 人工面）：

- 本地 token 管理：设置页保存/清除/测试连接（上游 tools/list 探活）；
- Mosaic 文档一键共享到腾讯文档（`create_smartcanvas_by_markdown`，官方推荐首选——markdown 语义映射最全），doc_id ↔ 远端 URL 映射持久化；
- docs.qq.com 链接进聊天 → resolve 定稿为 `external_refs` 描述子 → 消息卡片（标题/类型徽标/外链）。

明确不在本 RFC 范围（M2 候选）：

- **MCP 代理对外端点**（`POST /v1/txdocs/mcp` 透传）与 harness 配置注入（codex `-c` / kimi `KIMI_CODE_HOME` / WSL 回环策略）、四适配器 prompt 指令——负责人裁定后置；
- **变更推送**（腾讯文档侧编辑 → Mosaic 感知）：MCP 为纯拉取式工具调用协议，无推送；双向同步需 Open API（OAuth 2.0 应用审核制）另行立项；
- **块级映射**（腾讯智能文档 MDX 块模型 ↔ Mosaic 六块型的转换与冲突合并）；
- agent 路径的链接解析与 agent 发起的共享（ROP/run 发布面封闭构造，结构性不携带 external_refs——见 §2.4）；
- 删除远端腾讯文档（远端资产的删除权留给用户在腾讯文档客户端操作）。

# 2. 语义设计

## 2.1 上游与鉴权

上游唯一：`https://docs.qq.com/openapi/mcp`（官方 MCP，streamable HTTP）。鉴权 = 个人 token（官方授权页 `https://docs.qq.com/open/auth/mcp.html` 登录 QQ/微信获取；2026-09-24 勘误：该页现 0 秒跳转官方「龙虾专用入口」`https://docs.qq.com/scenario/open-claw.html`——登录腾讯文档 → 个人身份（微信扫码/QQ/手机号）→ 获取/重置 token；MCP 端点与 token 用法不变，真机以「测试连接」实证为准），请求头 `Authorization` 注入——官方配置示例为裸 token 值，端点错误文案提示 Bearer，客户端实现为**裸值先行、401 时按 `Bearer <token>` 前缀重试一次**（两种形态兼容不赌注，不落日志）。频控：免费 2000 次/天、超级会员 20000、超级会员Plus 40000；错误码 400006（token 无效）/ 400007（VIP 权限）透传到 verify 结果与共享错误信息。

## 2.2 共享语义：文档级快照

- `POST /v1/txdocs/shares {doc_id, force?}`：导出 markdown（复用 doc 域 ExportDoc）→ 上游创建智能文档 → 记录映射。
- **每文档至多一条 tencent_docs 映射**（PK doc_id+provider）。已有映射且未 force → 409 `txdocs_already_shared`（响应携带现有映射——前端免二次查询）；force=true = 重新创建并替换映射，旧远端文档保留。
- 映射表 `doc_external_shares`（storage v5）：**非事件派生的权威状态**（外部副作用不可由本地事件流重建），独立成表而非投影；文档删除不级联（解除映射是显式动作）。
- DELETE 仅解除本地映射；`GET /v1/txdocs/shares?doc_id=` 供 UI 徽标态。
- 快照语义的可容忍竞态：上游创建成功但映射落库失败时远端文档已存在——错误如实上报，重试 force 即可。

## 2.3 链接卡片：external_refs 描述子

- 前端 Composer 发送前检测正文 docs.qq.com 链接（https、去重、≤4）→ `POST /v1/txdocs/resolve` → `{url, title?, doc_type}`（title 尽力拉取页面 `<title>`——同 host 重定向才跟随、10s/1MiB 上限、实体解码、200 runes 截断；失败降级空 title，卡片显示 URL）→ 填入 post_message 载荷 `external_refs`。
- **描述子是发送时刻的快照**，不随外部服务刷新——与 DocRef 的投影原地刷新语义相反（外部资源无本地事件流）。卡片渲染面 TxDocCard：腾讯文档标识 + 标题（或 URL）+ 类型徽标（sheet/doc/slide/aio/form 按路径推断）+ 外链。
- 服务端校验（room service `validateExternalRefs`）：kind 封闭枚举（首版仅 `txdoc`）、host 白名单（https + `docs.qq.com`，与 resolve 同源）、≤4、url ≤512、title ≤200 runes、同 URL 去重。无存在性校验——外部资源无本地权威。
- resolve 失败（端点未装配/网络错误）不阻断发送：前端降级构造裸 `{kind:"txdoc", url}`（服务端只校验形状）。

## 2.4 agent 路径的封闭性

agent 消息的三个发布面（engine publishMessage / ROP / runs）中，ROP 与 runs 的载荷是显式封闭构造（`{body, addressed_to, relations}` + 白名单化 refs），适配器投影封闭 DTO——**external_refs 结构性无法从 agent 路径进入事件流**，无需剥离逻辑。若 M2 让 agent 经 MCP 创建腾讯文档，卡片化另行设计（候选：agent 结构化块声明 url，引擎代写 external_refs——doc_ops 同构）。

## 2.5 API 面（全部过 guardWrite 三件套 + owner token；读端点无 token 门）

| 端点 | 语义 | 稳定错误码 |
|---|---|---|
| GET/PUT `/v1/txdocs/settings` | 配置状态 / 写入清除 token | `bad_request` |
| POST `/v1/txdocs/verify` | tools/list 探活（业务失败也 200——ok=false 携带上游码） | `txdocs_token_missing`(409) / `txdocs_upstream_error`(502) |
| GET/POST/DELETE `/v1/txdocs/shares` | 映射列表 / 共享 / 解除 | `txdocs_already_shared`(409) / `txdocs_doc_not_found`(404) / `txdocs_share_not_found`(404) / 上两者 |
| POST `/v1/txdocs/resolve` | 链接 → 描述子 | `txdocs_bad_link`(400) |

契约四处同步（既有纪律）：openapi.yaml、room-protocol message.posted.schema.json、protocol/events.go、apigen/TS 生成物（gen-api / gen-ts / npm gen:api 三链 + CI 漂移门禁）。

# 3. 安全与隐私

- **token 持久化**：`DataDir/txdocs-token` 独立 0600 文件（owner-token 先例；临时文件 + rename 原子替换）；不进 settings 文档、不进日志、**不经任何读端点回传**（GET settings 只回 configured 标志与验证水位）。验证水位 `txdocs-state.json` 同 0600。
- **出网白名单唯一**：`docs.qq.com`。MCP 调用与 resolve title 拉取同源；resolve 重定向仅同 host 跟随（≤3 跳）、10s 超时、1MiB 上限（monitor.FetchURL 先例）——SSRF 防线；url 参数解析先行（非 https/非白名单 host 直接 400，不出网）。
- 上游错误摘要截断（300/200 runes）且不含凭据；`internal/room.txDocHost` 与 `txdocs.UpstreamHost` 两处常量注释互指（改一处必改两处）。
- 日志纪律：共享成功日志只记 doc_id 与远端 URL，不记 token、不记 markdown 内容。

# 4. 兼容与迁移

- storage v5：`doc_external_shares` 表（schemaDDL 幂等 CREATE + migrations INSERT OR IGNORE；存量库由 Open 时同 DDL 补建——doc_events 表族同模式）。
- message.posted 新增可选 `external_refs`：加法演进，旧事件无此字段（投影解码零值即无卡片）；旧客户端忽略未知字段。
- 未装配面（测试装配 Deps.TxDocs=nil）：全部 txdocs 端点 404 不暴露面；post_message 的 external_refs 校验不依赖 txdocs 装配（纯形状校验）。

# 5. 里程碑与工作量

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M1-1 | 契约（openapi + room-protocol schema + 三链重生成） | 已实现（2026-09-24） |
| M1-2 | internal/txdocs 域（Manager/Service/错误哨兵）+ sqlite v5 + app 装配 | 已实现 |
| M1-3 | httpapi handlers + room service external_refs 校验 | 已实现 |
| M1-4 | 前端（api client / 设置页集成区 / DocsTab+编辑页共享入口 / TxDocCard / Composer 检测） | 已实现 |
| M1-5 | 测试（txdocs UT / httpapi UT / sqlite IT）+ 本 RFC + CHANGELOG | 已实现 |
| M2（候选） | MCP 代理端点 + harness 注入 + prompt 指令 + agent 创建文档的卡片化 | 未排期 |

# 6. 开放问题（评审点）

- **OQ-1** `create_smartcanvas_by_markdown` 的入参名（title/markdown）与响应形状（URL 在 content 文本中）当前以官方使用指南文字描述为准、经假上游 fixture 钉住；首次真机联调若上游形状有出入，修改面收敛在 `internal/txdocs/service.go` 的 CreateSmartCanvas 一处（含请求形状编译期断言测试）。
- **OQ-2** 共享时是否顺带在当前房间自动发一条带 external_refs 的消息（当前设计：不自动发——共享与聊天解耦，用户自行粘贴链接；DocsTab 内共享后的"转发到房间"快捷键待反馈）。
- **OQ-3** 多 provider 扩展（飞书/Notion）：映射表已按 (doc_id, provider) 建键、ExternalRef.kind 已是封闭枚举——扩展时增设 kind 成员与 resolve 白名单分支即可，API 面的 provider 维度暂不暴露。
- **OQ-4** title 拉取的礼貌性（频率、UA）：当前每链接每次发送拉一次、无缓存；若上游出现限流，考虑 per-URL 短 TTL 缓存。
