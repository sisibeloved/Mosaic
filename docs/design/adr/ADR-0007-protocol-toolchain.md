# ADR-0007：协议工具链——JSON Schema 权威 + oapi-codegen + 运行时校验

| 状态 | Accepted（RFC-0001 已于 2026-08-25 Approved，按治理规则同步） |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | RFC-0001 §3.1.6/§3.5；架构 v0.6 §8.1.2、§8.4.1 |

> **修订（2026-08-28，M1 收口）**：OpenAPI 3.1 描述 + oapi-codegen 生成**延期至 M2**。
> 原计划"随 M1 命令 API"落地，实际 M1 以手写 `httpapi` DTO + `command.schema.json`
> 严格校验先行（对外契约由 Schema 门禁把守，不失控）；M2 React SPA 接入时补
> OpenAPI 3.1 + oapi-codegen 并以契约测试回填。延期登记于交付计划 v1.5——
> 延期项不得静默消失。

## 决策

- `api/room-protocol` 以 JSON Schema（draft 2020-12）为事件 payload 的权威源；
- HTTP 命令接口用 OpenAPI 3.1 描述，oapi-codegen strict 模式生成 Go 服务端边界模型；
- Go 运行时校验用 santhosh-tekuri/jsonschema（覆盖 TurnIntent、结构化块、工具参数等"严格写"要求）；
- TS 第一方 SDK 从 Schema 生成边界类型（json-schema-to-typescript 一类工具链）；
- 只生成边界模型，不生成领域模型；AsyncAPI 不作为必需工件（事件流用 JSON Schema 描述），保留为可选文档产物。

## 后果与放弃方案

- 放弃从 JSON Schema 直接生成 Go 类型（生态薄弱）：事件侧手写版本化 struct，用 round-trip fixture 保证与 Schema 一致（进 CI 兼容性门禁）；
- 边界模型不得进入领域层（架构 §8.4.1 依赖方向由 CI 把守）。
