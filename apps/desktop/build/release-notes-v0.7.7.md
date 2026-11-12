# Release v0.7.7 — 导入激活与执行权接管：防止同身份双写（0.7 记忆与可移植恢复领域·第八件）

迁移包（0.7.5 导出、0.7.6 预检登记）从 inactive 只读副本变为活的域状态——身份、任务承诺、续办检查点与授权记忆落库目标实例，由该 Agent 接管执行权。核心风险即标题：同一 agent_id 在源与目标两实例同时对外写入。

## 新增

- **两种激活模式**（RFC §8.3 两分支）：`takeover`（同身份承接——本实例已有同 ID Agent，必须 `confirm_source_stopped` **人工确认**源端已撤销执行权；服务端不验证只记录声明——"不能靠包内一个'已停机'字段证明外部执行者已经停止"）/ `copy`（新身份复制——新 AgentID + `source` 记 `bundle:<bundle_id>:<origin_agent_id>`，无须确认）。模式与实例事实不匹配 409——不自动推断，显式声明是防双写第一道门。
- **激活时实时复核**（blockers 是预检快照不是激活时刻事实）：重解析包（完整性顺带重验）+ 同 ID 任务冲突（同库重复激活防线）+ 依赖可用性——任一存在 422 `activation_blocked`。
- **跨域写入 + origin 标记幂等**（无跨域事务——顺序写入，每项以 `bundle:<bdl>:<id>` 标记溯源）：
  - 任务**原子创建为 paused**（`mission += CreateMissionPaused` 单事务——无 ready 调度窗口）+ 新 mission_id 承接（"显式承接和对象映射"——origin 标记进契约 SourceRefs，映射是激活结果一等公民）；draft 保持 draft。
  - 检查点：`checkpoint += CreateImported`——目标**全新 checkpoint_id/run_id**（同源多包不撞主键）、**Source 保留源语义**（业务/system——0.7.4 续办选取口径不变）、内容校验与普通创建同源；源版本事实入 `bundle.origin_versions` 水位（连续迁移 A→B→C 保最早未解决漂移证据）；`execctx += OriginDrift/OriginVersions`——源契约漂移传导进续办上下文。
  - 记忆三命名空间**重锚**（agent→目标身份 / workspace→default / mission→映射后新任务）+ 新 mem_* ID。
  - 失败保持 inactive（attempt.json 留痕）；重激活四层不重复写（恢复路径统一实时核验补暂停 + origin 持久关联找回身份）；已激活幂等返回既有结果。
- **UI**：历史行"激活"操作（模式选择 + takeover 确认勾选醒目警示）+ 已激活徽标 + 报告内激活结果（身份映射/逐任务映射/计数/确认声明时间）。
- Slot 任职不恢复（0.7.9 题目）；授权与唤醒不继承（包内本就无）。

## 防线与修复（三轮独立审校钉住）

- 防同身份双写三层：模式显式声明 + takeover 确认门 + 复制新 AgentID；任务承接无 ready 调度窗口（原子 paused）；恢复/重试路径状态未知即中止（不默认 paused 继续）。
- 检查点幂等查重全量覆盖业务与系统两类（按 origin 水位）；源漂移证据在连续迁移链上不被重映射抹平。
- 一审 7 项 + 复审 4 项 + 三审 1 项全关闭——见 [CHANGELOG](https://github.com/sisibeloved/Mosaic/blob/main/CHANGELOG.md#077---2026-11-12)。

---

**产物**：mosaic.exe（Windows/amd64）。数据格式迁移链无新增（v35 保持）；导入/激活记录在库外文件系统（bundle-imports/）。
