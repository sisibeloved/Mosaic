# 特性设计说明书（RFC 提案）：RFC-0016 飞书云文档接入、多 provider 泛化与块模型元数据增补

**状态 (Status):** Accepted（负责人裁定 2026-09-25：飞书按用户身份 OAuth 接入；txdocs 域泛化为多 provider；块模型做渲染加固 + level/lang 结构化字段——"文档设计不合理、不符两家转换逻辑之处一并修改"）

**作者 (Authors):** Mosaic 项目组 / ZCode

**创建日期 (Created):** 2026-09-26

**相关 Issue/PR:** #TBD

# 0. 文档控制

| 项目 | 内容 |
|---|---|
| RFC 编号 | 0016 |
| 系列位置 | RFC-0015（腾讯文档集成 M1）的多 provider 泛化与 API 面重构（0015 的实现面被本 RFC 取代，语义裁定仍有效）；RFC-0014（协作文档）的**块模型增补**（level/lang 元数据 + 渲染加固） |
| 上游输入 | 负责人 2026-09-25 指令（接入飞书云文档；文档设计不合理处一并修改）；飞书开放平台 API 实证（import_tasks 三步链、authen/v2/oauth/token、user_access_token 用户身份——官方 lark-openapi-mcp 同路径佐证） |
| 下游 | internal/extdocs 域（取代 internal/txdocs）、HTTP 契约（/v1/extdocs/*）、room-protocol message.posted.externalRef（kind 加 fdoc）、doc.revision_committed docBlock（level/lang）、doc.RenderMarkdown（渲染加固）、SPA（双 provider 设置/共享菜单/ExtDocCard/编辑器级别与语言控件）、agent 端口校验与四家适配器指令 |

# 1. 动机与范围

RFC-0015 打通了腾讯文档的单向文档级映射；协作生态里的另一半是飞书——接入它暴露出两件事：单 provider 硬编码的域需要泛化（host/凭据/上游协议/设置面全部分化）；更重要的是**块模型的转换缺口**——heading 无级别概念、code 无语言字段、导出 markdown 对 text 纯透传，用户没手写 `#`/fence 时两家云文档都会把标题当代码当普通段落，这是"文档设计不合理、不符两家转换逻辑"的实锤。

范围：

- **飞书云文档 provider**：用户身份 OAuth（授权码 + 手动粘 code；user_access_token 自动刷新轮换持久化）+ 云文档 markdown 导入三步（上传素材 → import_tasks → 轮询）；文档落用户"我的云文档"根目录；
- **多 provider 泛化**：internal/txdocs → internal/extdocs（Provider 插件接口 + 注册表 + host 分派）；API 面 /v1/txdocs/* → /v1/extdocs/*（未发版自由重构）；凭据 per-provider JSON（含旧 txdocs-token 单文件迁移）；
- **块模型增补**：DocBlock 加可选 `level`（heading 1..6）与 `lang`（code ≤48 runes）；RenderMarkdown 渲染加固（heading 无 # 前缀补、code 未包 fence 包、quote 缺 > 前缀补）；
- **链接卡片泛化**：external_refs.kind 加 `fdoc`；kind→host 归属校验分派；Composer 双 host 检测；卡片双 provider 样式。

明确不在本 RFC 范围：

- 变更推送 / 双向自动同步（M1 维持快照语义；平台事件能力的核查修订见 §8）；块级映射（MDX 块模型 ↔ 六块型转换）；
- larksuite.com 国际版 host（个人版国内主场景，后置）；
- agent 侧的 provider 调用（MCP 代理对外端点与 harness 注入维持 RFC-0015 的 M2 后置裁定）；
- 飞书 wiki 空间挂载（mount_key 恒空 = 根目录；指定目录挂载待反馈）。

# 2. 语义设计

## 2.1 Provider 插件面与注册表

`internal/extdocs`：`Provider` 接口（ID/DisplayName/RefKind/HostMatches/Verify/ShareMarkdown/ResolveLink）+ `Service`（注册表 + ShareDoc 事务编排 + ResolveLink host 分派）。腾讯（MCP JSON-RPC，RFC-0015 平移）与飞书（REST + OAuth）各自实现；`room service` 的 external_refs 校验保留**本域封闭表**（kind→host 规则与各 provider 的 HostMatches 同源镜像，避免域间耦合）。

## 2.2 飞书：用户身份 OAuth 与导入三步

- **一次性配置（用户侧）**：open.feishu.cn 创建自建应用 → 权限开通 `drive:drive` + `offline_access` → 安全设置登记重定向 URL `http://localhost:3000/callback`（官方 lark-openapi-mcp 同款推荐值；须完全一致）→ 发布版本。
- **绑定流程**：设置页存 app_id/app_secret → "登录飞书"打开授权 URL（`/open-apis/authen/v1/authorize`，scope 空格分隔）→ 授权后浏览器跳转 redirect_uri?code=...（页面打不开无妨）→ 地址栏复制 code 粘贴 → `POST /v1/extdocs/feishu/callback` 经 `authen/v2/oauth/token`（grant_type=authorization_code）交换并持久化。
- **token 生命周期**：单飞 ensureFresh——过期前 5 分钟以 refresh_token 刷新，**按轮换语义持久化新 refresh_token**；刷新失败/未授权 → `extdocs_reauth_required`（409 指路重新授权）；飞书鉴权失效码（99991661/99991663/99991679）同语义映射。
- **ShareMarkdown 三步**：上传素材 `drive/v1/medias/upload_all`（multipart，parent_type=ccm_import_open、parent_node=0、extra={"obj_type":"docx","file_extension":"md"}；file token 5 分钟有效）→ 创建导入 `drive/v1/import_tasks`（file_extension=md、file_name=标题、point mount_type=1 + mount_key=""）→ 轮询 `GET import_tasks/{ticket}`（500ms 间隔、60s 上限；job_status 1/2 继续、0 成功取 result.url、其他值携 job_error_msg 报 `extdocs_upstream_error`）。
- **Verify** = ensureFresh 成功（token 链健康即证明）+ 记录水位。

## 2.3 API 面（/v1/extdocs/*，全量错误码前缀 extdocs_*）

| 端点 | 语义 |
|---|---|
| GET `/v1/extdocs/providers` | provider 清单与凭据状态 |
| GET/PUT `/v1/extdocs/{provider}/settings` | 状态（无值）/ 凭据写入（tencent: token；feishu: app_id+app_secret） |
| POST `/v1/extdocs/{provider}/verify` | 探活（业务失败 200 ok=false） |
| GET/POST `/v1/extdocs/shares`、DELETE `/v1/extdocs/shares/{doc_id}?provider=` | 映射列表（双过滤）/ 共享（provider 进 body）/ 解除 |
| POST `/v1/extdocs/resolve` | host 分派解析（白名单外 400 `extdocs_bad_link`） |
| GET `/v1/extdocs/feishu/authorize`、POST `/v1/extdocs/feishu/callback` | OAuth 引导与绑定 |

新增错误码：`extdocs_reauth_required`（409）、`extdocs_bad_provider`（404）；其余（token_missing/already_shared/upstream_error/doc_not_found/share_not_found/bad_link）语义同 RFC-0015 换前缀。

## 2.4 块模型增补（对 RFC-0014 的修订）

- **渲染加固（存量零迁移）**：RenderMarkdown——heading text 无 `^#{1,6}\s` 前缀补 `#`×（level 或缺省 2）；code 未以 ``` 开头自动包 fence（lang 标注）；quote 行缺 `>` 前缀补；**text 自带标记透传优先**（模型手写 markdown 约定路径不受影响）。导出面向外部云文档（腾讯智能文档 / 飞书导入）——裸文本行会被当普通段落，这是共享转换链的结构性兜底。
- **结构化字段**：DocBlock 加 `level`（仅 heading，1..6，缺省导出按 2——文档标题占 H1）与 `lang`（仅 code，≤48 runes）。写路径校验（doc.ValidateBlockShape + agent 端口 checkDocOpBlocks）拒收跨类型携带与越界；agent 引擎代写 waveBlocksOf 补拷贝（此前会静默丢弃）；编辑器 heading 级别下拉（缺省 H2）/code 语言输入/按级渲染；docops.ts isNew 比较 type/level/lang（仅改元数据 → delete+insert 重建——replace 只携带 text）；RenderExcerpt 与 doc_fts 不受影响（不改动）。
- **agent 指令**：四家适配器 generateInstruction 的 doc_ops 段落统一追加"heading 用 level 而非手写 #、code 用 lang 而非手写 fence"。

# 3. 安全与隐私

- 凭据 per-provider JSON（`DataDir/extdocs/<provider>.json`，0600 原子写）；app_secret/token/refresh_token 不进日志、不经任何读端点回传（GET 只回状态标志）；启动一次性迁移旧 `txdocs-token` 并删除。
- 出网白名单 = 各 provider HostMatches（docs.qq.com 精确；feishu.cn / *.feishu.cn 后缀）；resolve 的 title 拉取同 host 重定向才跟随（≤3 跳）、10s/1MiB 上限（SSRF 防线）；oauth 交换/刷新仅指向固定官方端点（provider BaseURL，测试装配才可注入）。
- 飞书 OAuth 手动粘 code 的信任模型：code 单次有效、约 5 分钟时效，经写门（Origin + owner token）提交；redirect_uri 是用户在飞书后台登记的任意值，Mosaic 不监听本地回调端口（桌面形态无固定监听）。

# 4. 兼容与迁移

- `doc_external_shares` 表结构不变（PK doc_id+provider 天然多 provider）；ListExternalShares 增加 provider 过滤参数（存储端口签名变更，sqlite 实现同改）。
- message.posted.externalRef：kind 枚举加 fdoc、url pattern 放宽为 https 前缀（host 归属校验移交 room service 语义层）、doc_type 放开自由串 ≤16（枚举随 provider 扩张不可枚举，卡片映射表未知容错）——加法演进，旧事件无影响。
- doc.revision_committed docBlock 加可选 level/lang（additionalProperties:false 下显式枚举）；存量块零迁移。
- API 面 /v1/txdocs/* 移除：RFC-0015 与本 RFC 同处 [Unreleased] 未发版，无兼容负担。

# 5. 里程碑与工作量

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M1 | 块模型增补（schema 四处/渲染加固/校验/编辑器/agent/四家指令/fixtures） | 已实现（2026-09-26） |
| M2 | 域泛化（extdocs Provider 接口/manager/凭据迁移/API 重命名/前端全量） | 已实现 |
| M3 | 飞书 provider（OAuth/导入三步/resolve）+ 前端绑定 UI | 已实现 |
| M4 | 测试（extdocs 域 UT/httpapi UT/渲染与校验 UT）+ 本 RFC + CHANGELOG | 已实现 |
| M2'（RFC-0015 遗留） | MCP 代理端点 + harness 注入 + agent 卡片化 | 未排期 |

# 6. 开放问题（评审点）

- **OQ-1** 飞书 scope 清单当前为 `offline_access drive:drive`（大权限覆盖上传+导入+云空间）；若需最小化（docs:document:import 等）真机验证后收窄——设置页文案同步。
- **OQ-2** 飞书 authorize URL 的 state 参数恒 `mosaic`；§7 回环回调已上线，回调侧校验 state 原样带回（错值 400 不触上游）。升级为 per-session nonce 的残余收益有限（监听仅活授权窗口期、回环绑定、code 单次短时效、交换仅在服务端），记为后续可选项。
- **OQ-3** 指定挂载目录（mount_key 传文件夹 token）与共享后自动发 external_refs 消息进当前房间——两个 UX 增强候选，待用户反馈。
- **OQ-4** 飞书导入的 markdown 方言边界（表格/嵌套列表/公式在飞书侧的转换截断提示 extra 字段）——首轮真机联调观察。

# 7. 实现增补（2026-09-24，真机反馈迭代）

- **飞书授权免粘 code（回环回调自动绑定）**：GET `/v1/extdocs/feishu/authorize` 响应前按登记的 redirect_uri 挂起回环监听（默认 `localhost:3000`，10 分钟空闲上限、重复发起先撤旧监听、首次落点即收摊）；授权页跳转落回时服务端直接换 token 落盘，浏览器收到结果页（成功页 1.5s 自关），设置页轮询 `authorized` 自动变绿——全程免手动复制 code。响应新增 `auto_captured` 字段：回调端口被占（典型：开发态 Vite 占 3000）为 false，前端降级保留手动粘 code 通道（UT 以端口占用/释放双态钉死断言）。
- **入口可发现性**：聊天输入框占位提示"粘贴腾讯/飞书文档链接自动转卡片"——入站路径本就存在（粘贴外链即 resolve 成 external_refs 卡片），缺的是可发现性。

# 8. 后续设计与接口事实修订（2026-09-28）

- [RFC-0017](2026-09-28-rfc-0017-spreadsheets.md)（Draft）提出协作表格及两家平台的手动导入、导出和目标更新。负责人已明确当前版本不做自动同步；本 RFC 的文字文档快照分享语义保持不变。
- 原 §1 的“两家均无推送”表述不准确。飞书已有 [文件编辑事件](https://open.feishu.cn/document/ukTMukTMukTM/uUDN04SN0QjL1QDN/event/file-edited)，但通知载荷没有单元格修改明细；腾讯 [公开事件列表](https://docs.qq.com/open/document/app/openapi/v3/subscription/event.html) 未列出普通表格内容修改事件，不能概括为整个平台没有事件能力。
- 当前版本不接入事件通知或后台内容轮询，是产品范围决策，不依赖上述平台能力是否存在。公式、类型、格式与手动覆盖的兼容边界及待验证项见 RFC-0017。
