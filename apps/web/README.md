# apps/web

React + Vite SPA（ADR-0002）——Mosaic 的真实界面（M2，计划 v1.7 制度化"界面原型验证"）。

## 结构

- `src/api/schema.gen.ts`：从 `api/http-api/openapi.yaml` 生成的 API 类型（openapi-typescript，
  版本经 package-lock 锁定；`npm run gen:api` 重生成，CI 漂移门禁把守）；
- `src/api/client.ts`：契约客户端（幂等键 UUIDv7、错误稳定码、trace 捕获）；
- `src/api/room.ts`：房间订阅状态机——快照重建 + EventSource 自水位续传 +
  `resync_required` 具名信号恢复 + 时间线按 event_id 去重 + `draft.update` 瞬态帧草稿预览；
- `src/app/`：应用壳（ChatGPT 式侧栏 + Outlet）与路由（`/`、`/new`、`/rooms/:id`、`/settings`）；
- `src/state/`：房间列表 store（useSyncExternalStore，事件驱动刷新、不轮询）与主题（暗色默认 +
  亮色，`<html data-theme>` + localStorage）；
- `src/pages/`：房间页（消息流 + 打字区 + 输入框 + 成员/记分卡/谱系抽屉）、空态引导页、设置页
  （实例 / 策略 / 外观 / 开发者区——MOSAIC_DEV 注入机制沿用 M1）；
- `src/components/chat/`：气泡消息流 / 座位级打字条 / @ 点名输入框 / 抽屉 / 头像；
- `src/styles.css`：Tailwind v4 CSS-first 入口（语义 token 双主题）；
- `dist/`：构建产物**入库**（`apps/web/embed.go` 经 go:embed 服务；新鲜度由 CI 构建门禁把守）。

## 命令

```bash
npm install          # 首次
npm run gen:api      # OpenAPI → TS 类型（契约变更后）
npm run build        # tsc 严格检查 + vite 构建（产物入库）
npm run dev          # 开发态：vite dev server，/v1 代理到 127.0.0.1:7420
```

开发浏览器模式（D-3）：`mosaic-server -dev` 起服务后直接访问其根路径即可（SPA 由服务端
嵌入服务）；Wails 壳接入见 `apps/desktop`。
