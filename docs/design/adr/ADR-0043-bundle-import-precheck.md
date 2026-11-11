# ADR-0043：0.7.6 Bundle 导入预检——安全校验、兼容性报告及默认 inactive

- 状态：Accepted（2026-11-11，随 0.7.6 实施；v0.2–v0.3 两轮审校修订）
- 关联：[RFC-0018](../rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§8.2"导入先检查格式、大小、压缩展开上限、路径穿越、符号链接与引用闭包；不执行包内代码，不因导入启用 MCP server、工具或自动唤醒"、"新版本消费者不理解的必需字段应拒绝"、"首版支持本格式同版本与已定义的前向迁移"；§8.3"目标导入默认为 inactive"、"必要证据缺失就阻止激活；可选证据缺失可降级并展示"）；[ADR-0042](ADR-0042-portable-agent-bundle-export.md)（0.7.5 导出格式 v1——本件是其导入侧首个消费者：manifest 摘要/水位/脱敏报告、可移植别名投影与 `agent_id` 主键重映射口径）；[ADR-0037](ADR-0037-domained-memory-items.md)～[ADR-0041](ADR-0041-cross-harness-resume.md)（包内内容的域口径）；[交付计划](../../plan/2026-08-25-delivery-plan.md) 0.7 领域表（0.7.6 唯一交付特性：Bundle 导入预检——安全校验、兼容性报告及默认 inactive）。

## 背景与问题

0.7.5 交付了导出面：Agent 连同授权记忆、任务承诺与检查点打成版本化 zip（`mosaic-agent-bundle/1`）。RFC §8 的迁移链下一步是**目标实例接收**——导入预检（本件）→ 激活接管（0.7.7）→ 删除传播（0.7.8）。本件只交付**导入面**三件事：

1. **安全校验**——包在进入实例前过结构化安全门（格式/大小/展开上限/路径穿越/符号链接/引用闭包），任一不过**整体拒绝、不留记录**；
2. **兼容性报告**——通过安全门的包逐项对照本实例事实（身份冲突/任务漂移/依赖可用性/途中过期……），产出分级 findings 与总裁定（verdict）——**非阻断**：导入始终只落 inactive 记录，不触碰任何域状态；
3. **默认 inactive**——RFC §8.3"目标导入默认为 inactive"：导入产物是一份**待核验只读副本**（不执行包内代码、不启用 MCP/工具、不自动唤醒、不写任何业务域），激活与执行权接管整体归 0.7.7。

## 决策

### 1. 总体编排：上传 → 安全门（硬拒）→ 兼容报告（非阻断）→ inactive 登记

```
POST /v1/bundle-imports（multipart，字段 file）
  └→ staging 落盘（限流读，超限 413）
      └→ 安全门（§3——任一失败 → 422 bundle_unsafe，staging 清除，无记录）
          └→ 兼容性检查（§4——对照本实例事实，产出 checks[]/verdict/blockers）
              └→ 登记：bundle-imports/<bdi>/（zip + import.json 边车）→ 201 ImportView（status 恒 inactive）
```

安全门与兼容报告的分工即 RFC 两句话的分工：**"导入先检查"** 的六项是安全门（硬拒）；**"必要证据缺失就阻止激活；可选证据缺失可降级并展示"** 是激活前证据分级——预检把它们**提前亮出**为 `block`/`warn` findings 与 `blockers[]`（0.7.7 激活时消费），但不因 findings 拒绝导入（inactive 记录本身就是"可导入只读待核验副本"）。

### 2. 上传与大小边界

- **multipart/form-data 字段 `file`**（RFC-0013 附件上传先例：写门 Origin+Token 走 requireJSON=false 变体、`http.MaxBytesReader` 限体）。
- 上限：压缩体 **64 MiB**（`MaxZipBytes`，超限 413 `bundle_too_large`）；展开总量 **256 MiB**（`MaxExpandedBytes`）；条目数 **64**；单文件 **256 MiB**；ndjson 单文件行数 **200,000**。包是纯文本事实（无 artifacts 实体——ADR-0042 refs_only），此余量足够且封死 zip 炸弹（声明展开量与实际读取双向计量——`UncompressedSize64` 可伪造，实际读取超限同样拒绝）。

### 3. 安全门（RFC §8.2 六项的结构化落定；任一失败整体拒绝）

| 门 | 检查 |
|---|---|
| `format` | manifest 严格解码（`DisallowUnknownFields`——"不理解即拒绝，不静默丢失约束"）；`format == "mosaic-agent-bundle/1"`（v1 仅同版本——前向迁移待定义即拒绝，不承诺加载其他产品包）；必需字段齐全（bundle_id/agent.agent_id/exported_at/files）；manifest 与 profile 的 agent_id 一致 |
| `layout` | 条目恰为 0.7.5 九文件**精确集合**（未知文件拒绝）；无目录条目；无重复条目 |
| `path` | 包内相对路径、`/` 分隔、无 `..`/绝对路径/反斜杠/空段/盘符——生成侧保证的导入侧强制 |
| `symlink` | 条目须普通文件（`Mode().IsRegular()`——符号链接/设备/套接字全拒） |
| `digest` | manifest.files 恰覆盖 8 个内容文件（manifest 自身不在内——0.7.5 口径）；逐文件 sha256 与字节数和实际相符 |
| `closure` | 引用闭包：checkpoints/context manifests 的 mission_id ⊆ commitments 任务集且无重复（检查点每任务至多一条）；memory 命名空间 ∈ {agent, workspace, mission}、mission 域 scope ∈ 承诺任务集、status 恒 active、sensitivity ∈ {normal, sensitive}；commitments 任务无重复 ID；`default_on_import == "paused"`；ndjson 逐行严格可解析 |

失败响应聚合**全部**违规（不挤牙膏式逐条报）——错误体 message 内逐行列出 `门: 详情`。

### 4. 兼容性报告（findings 分级 + verdict；对照本实例事实，非阻断）

每项 finding = `{check, result, detail}`，`result ∈ {pass, info, warn, block}`；**block = 激活前必须解决**（非导入失败——0.7.7 激活时复核），`blockers[]` 汇总 block 级 check 键。

| check | 依据与分级 |
|---|---|
| `zip_structure` / `integrity` / `reference_closure` | 安全门通过摘要（恒 pass——失败根本到不了报告；报告里保留三行让使用者看见"安全校验已过"） |
| `source_version` | 包内 `product_version` vs 本实例版本（info——格式门已裁定兼容性，此行是事实面） |
| `identity_conflict` | 本实例已存在同 `agent_id` → **block**（同身份双写防线起点：激活前须确认源端已撤销执行权——RFC §8.3；AT-13 同身份导入场景的可视化）；不存在 → pass |
| `mission_conflict` | 逐承诺任务对照本实例：同 ID 存在且 contract_version 不同 → **block**（漂移——水位即为此设）；同 ID 同版本 → warn（重复——激活时去重）；不存在 → pass（全新承接） |
| `harness_availability` | 包内 harness 可执行项（portable 标识 `adapter@runtime[:distro]`）vs 本实例注册表投影集：有缺 → **block**（"必要证据缺失就阻止激活"——目标环境须先注册可执行项；凭据本就不随包）；全在 → pass；包内无引用 → info |
| `expired_in_transit` | 包内记忆 `valid_to` 已过当前时刻（包可能旅行多日）→ warn（计数+ID——激活时按 0.7.1 时效口径重新排除）；无 → pass |
| `sensitive_payload` | `scope.include_sensitive == true` → warn（敏感记忆在包内——接收方信任确认）；false → pass |
| `recovery_limits` | 透传包内脱敏报告 recovery_limits（无检查点任务——导入后从契约裸启动）→ warn；空 → pass |
| `room_source_unbound` | 房间交办来源任务数 > 0 → info（原房间不存在于本实例——回传目标需重绑定，RFC §8.3"不能仅复制任务摘要当作责任转移"） |
| `alias_projection` | 恒 info（别名体系为可移植投影——ADR-0042 二审 #1：原始派生别名按 agent_id 主键在激活时重映射） |

**verdict**：任一 block → `blocked`；任一 warn → `eligible_with_warnings`；否则 `eligible`。View 内联脱敏报告原文（redaction）与包内 scope/水位——报告自足，传输核对面与登记面同源。

### 5. 默认 inactive 与不执行边界

- 导入记录 `status` 恒 `"inactive"`（0.7.6 无任何改态路径——激活端点归 0.7.7，届时 `imported_inactive → activated` 迁移事务语义一并交付）。
- **不执行包内代码、不因导入启用 MCP server/工具、不自动唤醒**（RFC §8.2 原文落定）：导入产物是文件系统上的 zip + 报告边车，不进 SQLite、不建 Agent/任务/记忆行、不动调度——域零感知，零 migration（backup/0.7.5 同例）。
- 幂等：同 `bundle_id`（源导出 ID）重复导入 → 409 `bundle_already_imported`（指向既有 import_id——重复点击不产生双记录）。

### 6. 存储、生命周期与 HTTP 面

- 产物落数据目录 `bundle-imports/<import_id>/`：`mosaic-agent-bundle.zip`（staging 原子 rename 入位）+ `import.json`（边车 = 创建响应全文，列表/详情零重算）。`import_id = bdi_<ts36>_<hex8>`（bdl_/bkp_ 同族）；详情端点 id 先过 `^bdi_[0-9a-z_]+$` 白名单再拼路径（防穿越纪律）。
- 三端点（写门三层照旧；上传走 multipart 变体）：

| 端点 | 语义 |
|---|---|
| `POST /v1/bundle-imports` | 上传 + 预检 + 登记 → 201 ImportView；400 multipart/字段缺失；413 超大小；422 `bundle_unsafe`（安全门聚合明细）；409 重复导入；503 bundle_unavailable |
| `GET /v1/bundle-imports` | 导入列表（新→旧，Summary；实例级——导入不隶属某 Agent 选择） |
| `GET /v1/bundle-imports/{import_id}` | 详情（View 全量——激活决策前的复核面） |

- 删除端点不做（0.7.8 删除传播统一治理包生命周期）。

### 7. 域结构（internal/bundle 增导入面——同包共享格式常量与投影类型）

`bundle.ImportService` 与导出 `Service` 同包不互 import 域；对照端口全闭包（`ImportPorts`——导入不需要读源实例业务数据，只需本实例**对照事实**）：

```go
type ImportPorts struct {
    AgentExists  func(ctx, agentID string) (bool, error)                  // 身份冲突对照
    MissionState func(ctx, missionID string) (MissionState, bool, error)  // {ContractVersion, Status}——漂移对照
    Harnesses    func(ctx) ([]string, error)                              // 本实例可执行项 portable 标识集
}
```

wiring 映射：AgentExists ← personalagent.Get；MissionState ← mission.Get；Harnesses ← personalagent.List 全量 Executables 经 `portableExeID` 投影去重（与导出投影同源——比较两侧同口径）。ImportView/ImportSummary 的 JSON 形状即 API 契约形状（handler 零转换）。

### 8. 前端（设置页"迁移包"面板增"导入与预检"区）

文件选择（accept .zip）→ 上传（multipart）→ 预检结果卡：verdict 徽标（eligible/带警告/blocked 三色）、检查表（check/result/detail 逐行）、计数、脱敏报告透传、blockers 列表；导入历史列表（新→旧，status 恒"未激活"标注 + verdict 徽标）。导入是实例级操作，不依赖导出侧的 Agent 选择。

### 9. 测试与验证

import UT（fake ImportPorts + 临时目录 + 导出往返）：安全门族（非 zip/多余文件/路径穿越/符号链接条目/摘要不符/展开超限/行数超限/未知格式/manifest 未知字段/闭包破裂各拒并留因）；导出→导入**往返**（0.7.5 Service 产物直接进 ImportService——eligible、计数相符、报告内联）；兼容族（身份冲突 block/任务漂移 block 与同版重复 warn/依赖缺失 block/途中过期 warn/敏感携带 warn）；重复导入 409；列表/详情边车 + bdi_ 白名单。httpapi IT：三端点全语义（201/400/413/422/409/503/未装配/列表序/详情）。真机冒烟：两实例数据目录对导——实例 A 导出 → 实例 B 上传导入 → 报告人检（身份冲突/依赖缺失正确亮 block）→ zip 与边车落位核对。

## 审校修订记录

- v0.3（2026-11-11）：二审 3 项 P2 修复（上轮 7 项复测关闭）——
  1. **依赖清单缺失绕过依赖对照（#1）**：依赖检查只读 dependencies.harness——保留 profile 可执行项而把 harness 置 null/清空（重算摘要）后，即使目标无任何 Harness 仍 eligible 并报"包内无可执行项引用"。闭包门新增一致性核验：dependencies.harness 与 agent/profile.executables 的标识集合须相等（导出侧两处同源——矛盾即包损坏/篡改，结构性拒绝，双向缺项各成违规）；兼容对照面改为两处并集（防御纵深——清单被清空也不得静默缩圈）。
  2. **严格解码漏检非法尾随内容（#2）**：`Decoder.More()` 是"容器还有没有元素"的语义（遇 `]`/`}` 恒 false），不能证明顶层已结束——合法 manifest 后追加 `]` 与额外对象（json.Valid 判非法）仍过关。`decodeStrict` 尾随判定改为第二次 `Decode` 严格 `io.EOF`（有第二值或语法错皆拒）。
  3. **导入历史无法重开完整报告（#3）**：完整报告只在组件 result 态，历史行仅摘要——切分区/刷新即失，重复上传又 409。历史行新增"查看报告"操作，经既有详情端点 `GET /v1/bundle-imports/{import_id}` 恢复检查明细/阻断原因/脱敏报告展示。
- v0.2（2026-11-11）：一审 1 P1 + 6 P2 修复——
  1. **空行载荷内存放大（P1）**：`ndjsonLineCount`/`parseNDJSON` 用 bytes.Split 切行——每行一个切片头，1 MiB 纯换行计 0 行却分配约 24 MiB；近 256 MiB 展开上限的高压缩空行包可放大数 GiB 分配并绕过 20 万行门。改扫描式切行（切片引用零分配，行数门语义不变）；`TestNDJSONCountNoAlloc` 以 AllocsPerRun=0 钉住。
  2. **受保护 GET 不带令牌（#1）**：前端 `request` 只在有 method 时附 X-Owner-Token——导入列表/详情（token 门内 GET）恒 401 且 bootstrap 重试仍不带，导入历史被静默吞掉。四处调用显式 `method: "GET"`（导入两处 + 导出列表/详情两处——后者为 0.7.5 存量同病，一并修）。
  3. **并发导入绕过查重（#2）**：findByBundleID 与登记之间无锁——两并发请求可同时查空并各自登记双记录。查重~登记段（含 rename 与边车落盘）加 `sync.Mutex` 临界区；staging/安全门留在锁外（重 IO 无共享态）。`TestImportConcurrentDuplicate`（-race）钉住"恰一成功一 409"。
  4. **闭包未验上下文检查点引用（#3）**：原只核上下文的 mission_id——清空 checkpoints.ndjson（重算摘要）后上下文仍引用 ck_1、水位仍声明 v3，包以 eligible 过关。闭包门补双向核验：上下文 checkpoint 引用须落在包内检查点上且 ID/版本一致；水位声明检查点版本须与包内相符（未声明而包内有亦断链）。
  5. **敏感判定只信 manifest 开关（#4）**：scope.include_sensitive 是导出侧声明，不能证明载荷——注入 sensitive 记忆 + 开关 false 仍报"无敏感记忆随包" pass。改以包内实际载荷判定：载荷有敏感即 warn；与声明不一致时 detail 明示"载荷与声明不一致（来源可疑或包被改动）"，不静默按声明放行。
  6. **redaction 缺必需字段可崩消费端（#5）**：DisallowUnknownFields 不查必需字段——`{}` 可过解码，View 序列化出 null 数组，前端 `.length` 直接抛异常。门 7 解码后校验 omitted_sensitive.item_ids / recovery_limits / notes 须为数组（缺失即 `content:` 违规拒绝）。
  7. **必要对照失败降级 warn（#6）**：AgentExists/Harnesses 查询失败原记 warn——verdict eligible_with_warnings、blockers 空，界面显示"可激活"，但此时身份冲突与依赖可用性均不可知（必要证据缺失）。改 block 并入 blockers，detail 注明"激活前须重跑预检"。

## 后果

- 迁移链第二环落地：导出（0.7.5）→ **导入预检（本件）** → 激活接管（0.7.7 消费 blockers[]/commitments 暂停语义）→ 删除传播（0.7.8）。
- 0.7.5 的 manifest 摘要与水位首次被消费：完整性核对与任务级漂移判定从设计意图变为运行事实。
- 安全校验是**完整性与结构性**的，不是信任性的——sha256 防损坏不防恶意（"不等同于可信签名"，RFC 原文）；签名/封印信任模型另行决策。上传门（OwnerToken/Origin）与文件系统边界是本件的安全假设。
- v1 仅同版本导入：前向迁移（`mosaic-agent-bundle/2` 的定义与升级路径）在格式演进时另行决策；本件的严格解码让"不理解"显式失败而非静默丢失约束。
- 激活语义（同身份双写防线、执行代次递增、源端撤销确认）整体欠账归 0.7.7——本件 blockers[] 是其输入契约。
