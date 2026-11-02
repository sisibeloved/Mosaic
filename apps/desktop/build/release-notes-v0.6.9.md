# Mosaic v0.6.9 — Mission 验收：`request_review` 与 `review_mission`（0.6 Mission 与本地执行领域·第十件）

0.6.8 交付了第二个事实"成果提交"，并把完备性检查明确推给送验面。本件交付 RFC-0018 §3.3 三事实中的第三件——**验收通过**：Mission 首次拥有"完成"终态。送验以交付包为锚并做必需交付项检查（AT-10）；裁定**接受**为终态，**退回**则回 ready 再执行 / 再交付 / 再送验。三轮独立审校（3 P1 + 12 P2 → 2 P1 + 8 P2 + 2 P3 → 4 P2）共 31 项全部关闭后定版；另并入一项 MiniMax 原生 `<deliver-assets>` 附件投递修复（负责人裁定属 bugfix，随本版发布）。

## 新增

- **`reviewing` 验收门 + `accepted` 终态**（ADR-0032；migration v22 `missions.review_package_id` 追踪锚、v23 `missions.review_round` 送验轮次、v24 幂等数据补正）：`ready → reviewing → accepted | ready`（退回）。送验入口为 ready——本版执行投影在 Run 终态后回 ready，与 RFC §3.2 的 `running → reviewing` 语义等价；waiting / paused / draft / running 不可送验。
- **`POST /v1/missions/{id}/request-review`**（submit_result 的对面）：以 `package_id` 为锚——包须存在、属该 Mission、对应**当前**契约版本（陈旧 409 `stale_package`，须补证新包）；**必需交付项检查**（AT-10）：成果引用非空 + 当前每条验收标准在包内有**非空证据**（判空统一 Unicode White_Space ∪ U+FEFF，前后端同集），缺项 409 `missing_required_items` **携缺项清单**——补证后重试（"先交付再补证"闭环）；送验事务内复核在途 / 未处置 Run（409 `runs_unsettled`——未核验的运行不能随任务被接受）；每次送验 `review_round` **单调递增**（同包重审也递增）；同包同版本在审的重试幂等 200；reviewing 中换包 409（先退回）。
- **`POST /v1/missions/{id}/review`**（裁定）：`decision ∈ accepted | returned`，**必填 `review_round`**（缺 400）——裁定事务内核对**轮次 + 包锚 + 契约版本 + 状态**四守卫，旧标签页 / 迟到 / 重放一律 409；`accepted` 为**终态**：修订 / 取消 / 暂停 / 送验 / 再交付 / 执行 / 新装配 / 租约领取全部拒绝（409 `mission_accepted`，历史读取照常），锚保留以追溯"按哪个包验收"；`returned` → ready（锚清空，须携 `reason`；可再执行、补证再交付、再送验）。**验收权限 = 契约一致性校验**：契约 reviewer 非空时 body.reviewer 须相等（403 `review_not_authorized`；空 = 请求人自验），验收人 ≤ 64 字；`item_notes` 逐项依据与退回原因留痕于 `status_changed` 事件。
- **验收期间契约冻结**：reviewing 下修订（验收基线不得变动）、暂停、交付提交均 409 `mission_reviewing`——受审包已固定；取消两段照常（非终态皆可取消，ADR-0029）。
- **终态写入守卫**：CreateRun、Run 处置、执行上下文清单 / 回执、租约领取各自在**写事务内**重读任务状态——预读 ready / reviewing 后终态落地的迟到写入不再落库，既有查询照常。
- **`review_package_id` 追踪锚**（AT-10"任务卡、产物、决定可互相追踪"）：Mission → 交付包 → 来源 Run → 事件流直达，无需翻事件流重建。
- **SPA**：ready + 有包 → 「提请验收」选包（较早完整包可选并显示证据覆盖；缺项 409 回显清单）；reviewing → 受审包详情 + 验收面板（接受 / 退回 + 原因，验收人预填契约 reviewer，逐项备注，受审包加载失败可原地重试）；accepted → 终态徽章，控制区 / 交付表单 / 装配卡只读。
- 契约面：两端点 + `MissionReviewRequest` / `MissionReviewDecision` / `ReviewItemNote`；状态枚举 += `reviewing`, `accepted`；`MissionView` += `review_package_id` / `review_round`（仅 reviewing / accepted 公开）。

## 修复（三轮独立审校 31 项：3 P1 + 12 P2 / 2 P1 + 8 P2 + 2 P3 / 4 P2，全部关闭）

- **旧裁定落到下一轮**（P1）：裁定请求必携送验轮次，事务内核对轮次 / 包 / 版本 / 状态——读过 A 的接受请求不能在 A 退回、B 送审后把 B 标为 accepted。
- **纯空白证据可通过 AT-10**（P1）：去首尾空白再判非空，前后端字符集统一（BOM / NEL 同罪）。
- **未核验 Run 随任务被接受**（P1）：送验前置 + 送验事务内复核未决 Run；CreateRun 写事务内核对任务仍是 ready|running（送验后的过期调度不再落 Run）；接受事务内复查；ready 残留崩溃形态恢复同样进 waiting；accepted / canceled 终态封处置入口。
- **v22 已在审任务升级后卡死**（P1）：migration v24 幂等补正——reviewing / accepted 轮次 0 → 1（恢复可裁定），canceled 行清受审包引用；兼容已跑 v23 的库。
- **预填未来轮次能接受 B 却把事件记作 A**（P2）：裁定事务核对包锚，事件按核对后的实际锚写入。
- **终态迟到写入**（P2）：新建清单 / 回执、Run 处置、租约领取各自写事务内核对终态。
- **升级库在审包的匹配重试被证据口径拒**（P2）：同版本同包在审的重试先行走幂等路径返回 200，必需项检查只对新一轮送验生效。
- **其余 P2 / P3**：并发送验同包幂等（不再误报 409）；`expected_version` 预读校验（与修订交错时旧包不能按新契约通过）；退回事件 `to` 为真实状态 ready；两段取消清受审包锚；验收人 > 64 字拒绝；视图轮次仅 reviewing / accepted 公开。
- **SPA**：送验列表随提交即时刷新、随契约版本切换 / 重载即清（加载失败同清，发送前再核所选包版本）；受审包加载失败禁用裁定并可原地重试；裁定草稿按轮次隔离（退回原因不再串入新一轮）；验收人预填 + 逐项备注；accepted 后修订 / 取消按钮隐藏、装配卡只读。

## 修复（MiniMax 附件投递——随本版一并发布）

- **MiniMax 原生 `<deliver-assets><media …/></deliver-assets>` 被当作消息正文或附件解析错误展示**（RFC-0013 §2.6）：适配器先分离正文与最多 4 个工作区产物，正文照常过发布门（畸形块拒绝发布，不原样展示内部标签）；发布围栏通过后房间引擎按发言 Profile 把文件安全导入既有受管附件库——Native 与 WSL 均只读取当前 Agent 工作区内的普通文件（拒绝路径穿越、符号链接逃逸、目录与其他文件类型；WSL 走原始二进制流），复用 8 MiB / 房间配额 / MIME 探测 / SHA-256 校验与房间删除级联；消息显示可下载附件及可选 `caption`（≤ 500 字，过秘密剔除），不再泄漏内部 XML。单件读取或导入失败保留正文与其他可用附件，并在正文明确标记该附件不可用——不伪装导入成功。
- 契约面：附件描述子新增可选 `caption`（OpenAPI `AttachmentDescriptor` / room-protocol `message.posted`）；旧事件无该字段投影为零值，人工上传链路不变。

## 升级说明

- v0.6.8 数据库首次启动自动执行 migration v22–v24（幂等 DDL + 数据补正：已在审 / 已接受行轮次补为 1，已取消行清受审包引用），无需手工步骤；停留在 v22 / v23 中间形态的库同样可直接升级，重复执行安全。
- 裁定请求新增必填 `review_round`（取自任务视图 `review_round`）；缺省将收到 400。
- 设计基线：[RFC-0018](docs/design/rfc/2026-10-28-rfc-0018-personal-agent-swarm.md)（§3.2/§3.3/§10.2/§11.2、AT-10）与 [ADR-0032](docs/design/adr/ADR-0032-mission-review.md)（v0.4）。
