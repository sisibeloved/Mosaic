# apps/desktop——桌面壳（Wails v2，ADR-0010）

Wails v2（v2.15.x，2026 年仍是稳定维护线；v3 处 beta，按 ADR-0010 不采用）。

## M2：进程内装配 + 回环源

产品形态是"本地单进程桌面应用"（D-2/D-3）——壳与 `cmd/mosaic-server` 共用
`internal/app` 装配（引擎/分发/宿主层），不经子进程。

传输形态（dogfood 修复）：装配以回环 TCP（`127.0.0.1:0`）启动，Wails 资产服务
器 `Handler` 仅做 307 重定向，WebView 首次导航即落到真实 HTTP 源。此前"全部请求
经资产 Handler 直连 mux"的形态在 Windows 上有硬限制：WebView2 的
WebResourceRequested 桥会缓冲/改写响应，SSE（`text/event-stream`）过不去，
EventSource 永久 CLOSED，UI 卡"恢复中"。落到回环源后 SPA/API/SSE 与 web 形态
完全一致（同源相对路径、同源 owner token bootstrap）。`wails.localhost` 仍保留
为跨源写门信任源（`httpapi.Deps.ExtraOriginHosts`，精确主机名匹配，UT 钉住
"未配置即拒/近似名不放行"语义）。

本包属主 module（M0 的独立 module 已并回）；`//go:build windows || darwin`
——Linux（gtk/webkit2 面）不构建、不进 CI 覆盖。

## 构建

```bash
# Windows（go-webview2 纯 Go）
GOOS=windows GOARCH=amd64 go build ./apps/desktop
# macOS arm64（wails v2.15 objc 运行时免 cgo，可交叉编译）
GOOS=darwin GOARCH=arm64 go build ./apps/desktop
```

交叉编译产物**未做真机运行验证**（WebView2/WKWebView 实际拉起窗口、回环源 SSE
长流）——M2 dogfood 与 M4 真机演练覆盖。`.app` bundle、NSIS 安装器、
ad-hoc 签名随 M4（v1.2 裁定：个人分发不做公证）。

## 图标

`build/appicon.png`（1024px）与 `build/windows/icon.ico`（16–256 多尺寸）遵循
Wails 目录约定，`wails build` 打包时自动取用（M4 打包演练生效）；`go build`
直接产出的 exe 不内嵌图标。矢量母版与导出脚本在 `docs/design/icon/`
（改动先改 `mosaic.svg`，再跑 `export_icon.py` 重新导出）。

数据目录 `os.UserConfigDir()/mosaic`；日志 stderr（JSON debug——M2 主线开发
默认开发者模式，计划 v1.8 裁定）。

## 开发（真机）

```bash
# 前置：go ≥1.25、wails CLI（go install github.com/wailsapp/wails/v2/cmd/wails@latest）
wails doctor   # 平台依赖（Windows: WebView2 Runtime；macOS: Xcode CLT）
wails dev      # 热重载（frontend 已并入 apps/web，经 internal/app 进程内装配）
wails build    # 平台打包
```
