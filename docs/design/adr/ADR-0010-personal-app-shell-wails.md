# ADR-0010：个人版应用壳采用 Wails

| 状态 | Accepted |
|---|---|
| 日期 | 2026-08-25 |
| 关联 | [交付规划 D-3](../../plan/2026-08-25-delivery-plan.md)；ADR-0002（React/Vite SPA） |


> **修订（2026-08-31，M2）**：壳落地为**进程内装配**—— 抽取后与
>  共用同一装配（引擎/分发/宿主层），Wails 资产服务器 Handler
> 直连 httpapi mux（SPA 资产 + API + SSE 同源）； 进跨源写门
> 信任源（Origin 不可伪造 + .localhost 不落远端）。desktop 自 M0 的独立 module
> 并回主 module（，Linux 不构建）。真机拉起与
> SSE 经资产服务器长流的运行验证随 M2 dogfood / M4 真机演练。

## 决策

- 个人版应用壳用 **Wails v2**：Windows 走 WebView2、macOS 走 WKWebView，前端复用既有 React/Vite SPA，后端复用 Go 单二进制（`cmd/mosaic-server`）；
- 托盘常驻 + 点击开窗；开发态保留 `--browser` 参数直开系统浏览器（无壳调试）；
- v1.0 更新机制为"检查更新 + 引导下载替换"（应用内检查、安装手动）；系统级通知降级为应用内提示 + 托盘角标。

## 后果与放弃方案

- 不捆绑 Chromium：安装体积与常驻内存显著优于 Electron；Go 主进程无语言切换成本；
- 放弃 Electron（重壳、双运行时）与 Tauri（主进程转 Rust，与 Go 栈冲突）；
- 深水区预案：若托盘/自动更新在 Wails 上成本超预期，降级为"单二进制 + 默认浏览器"交付形态（DoD 的安装体验条款相应放宽为平台原生可执行 + 文档引导），该回退需修订交付规划 D-3。
