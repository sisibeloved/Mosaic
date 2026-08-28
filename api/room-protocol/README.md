# room-protocol——Room 协议工件（权威源）

RFC-0001（Room 协议，Approved）与 RFC-0003（Attention/Floor，Draft）的协议工件。**设计归 RFC，本目录只放机器可校验的权威形态**。

## 目录

| 路径 | 内容 | 权威性 |
|---|---|---|
| `envelope.schema.json` | Room Event Envelope（内部/存储形态；对外视图不含 seq，用 opaque position） | RFC-0001 v0.4 |
| `command.schema.json` | Room Command（幂等键 UUIDv7；tenant/room/actor 由鉴权与路径绑定） | RFC-0001 v0.4 |
| `events/*.schema.json` | 事件 payload Schema（严格写：`additionalProperties: false`，超范围拒绝）。已覆盖 RFC-0003 Attention 事件族六事件；`message.posted` 等 Conversation 平面事件属 M1 | 各 owning RFC |
| `fixtures/valid/` | 必须通过校验的正例（每个事件 Schema 至少被一个 fixture 覆盖） | 门禁输入 |
| `fixtures/invalid/` | 必须被拒绝的反例（每个反例对准一条具体校验规则，登记在门禁测试里） | 门禁输入 |
| `gen/ts/*.d.ts` | TS 边界类型（生成产物，勿手改） | 由 Schema 生成 |

## 生成链与门禁（ADR-0007）

```
JSON Schema（权威源）
  ├─ go: 手写版本化边界 struct（internal/protocol/events.go）
  │      └─ round-trip 门禁：fixture → struct → 重序列化 → 逐键一致（CI）
  ├─ ts: json-schema-to-typescript → gen/ts/（`./tools/scripts/gen-ts.sh`，产物入库）
  └─ 严格校验门禁：valid 全过 / invalid 全拒（internal/protocol/schema_gate_test.go，CI）
```

Go 运行时校验（TurnIntent、结构化块等"严格写"场景）统一走 `santhosh-tekuri/jsonschema/v6`（draft 2020-12，`AssertFormat` 开启）。HTTP 命令接口的 OpenAPI + oapi-codegen 生成随 M1 命令 API 落地。

## 新增/修改事件的流程

1. owning RFC 定稿 payload（类型、必填、枚举、约束）；
2. 写/改 `events/<type>.schema.json` + `fixtures/valid/` 正例 + `fixtures/invalid/` 反例（反例同步登记进门禁测试的 `requiredInvalid`）；
3. 补 `internal/protocol/events.go` 边界 struct（round-trip 会逼你覆盖全部键）；
4. 跑 `./tools/scripts/gen-ts.sh` 更新 TS 产物；
5. `go test ./internal/protocol/` 全绿后提交。

Schema 演进遵守 RFC-0001 的 expand-first：只加字段不删不改语义，永久 upcast；删字段/改语义必须升 `schema_version` 并提供迁移。
