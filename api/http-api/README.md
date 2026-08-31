# api/http-api

Mosaic 个人版 HTTP 面的 OpenAPI 3.1 权威描述（ADR-0007，M2 履约——自 M1 延期登记后落地）。

- `openapi.yaml`：对外契约。命令上行（`RoomCommand`）、SSE 订阅（opaque cursor 续传）、
  快照四元组、宿主注册表。事件 payload 的权威源仍是 `api/room-protocol` 的 JSON Schema
  （OpenAPI 只引用描述，不复制）。
- `gen.yaml`：oapi-codegen 生成配置（版本经 go.mod `tool` 指令锁定）。

## 生成链

```
openapi.yaml → internal/transport/httpapi/apigen/api.gen.go（ServerInterface + 边界模型 + 内嵌 spec）
```

- `make gen-api`（本地 WSL：`PATH="$HOME/.local/go/bin:$PATH" make gen-api`）；
- CI 漂移门禁（ubuntu 腿）：spec 改动未重生成即红。

## 边界纪律

- 服务端实现 `apigen.ServerInterface`——操作集与 spec 的一致性是**编译期保证**（漏实现
  即编译失败）；请求解码保留在 `httpapi` 手工实现（`DisallowUnknownFields` 严格拒收是
  M1 复审以来的契约纪律；oapi-codegen strict-server 包装会接管解码并静默放行未知字段，
  故不启用）；
- 契约测试回填：`internal/transport/httpapi/contract_test.go`——spec 驱动的路由覆盖、
  command_kind 枚举一致、请求例受理与响应字段集断言；
- 领域层不得 import `apigen`（架构 §8.4.1 依赖方向）。

## 不在本契约内的 HTTP 面

- `GET /`：内嵌 Timeline 最小 UI（M2 真实 SPA 接入前的过渡形态）；
- `GET /v1/debug/rooms/{room_id}/state|events`：开发者模式只读调试端点，仅 `-dev`
  下注册（404 不暴露面），返回权威信封（含 seq/tenant 内部字段）——属内部语义，
  不受 RFC-0001 对外视图约束，故不入对外契约。
