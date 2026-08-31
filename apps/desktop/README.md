# apps/desktop——桌面壳（Wails v2，ADR-0010）

Wails v2（v2.15.x，2026 年仍是稳定维护线；v3 处 beta，按 ADR-0010 不采用）。

## M2：进程内装配

产品形态是"本地单进程桌面应用"（D-2/D-3）——壳与 `cmd/mosaic-server` 共用
`internal/app` 装配（引擎/分发/宿主层），不经子进程。WebView 的全部请求
（SPA 资产 + API + SSE）经资产服务器 `Handler` 直连 httpapi mux——同源相对
路径调用；`wails.localhost` 作为跨源写门信任源（`httpapi.Deps.ExtraOriginHosts`，
精确主机名匹配，UT 钉住"未配置即拒/近似名不放行"语义）。

本包属主 module（M0 的独立 module 已并回）；`//go:build windows || darwin`
——Linux（gtk/webkit2 面）不构建、不进 CI 覆盖。

## 构建

```bash
# Windows（go-webview2 纯 Go）
GOOS=windows GOARCH=amd64 go build ./apps/desktop
# macOS arm64（wails v2.15 objc 运行时免 cgo，可交叉编译）
GOOS=darwin GOARCH=arm64 go build ./apps/desktop
```

交叉编译产物**未做真机运行验证**（WebView2/WKWebView 实际拉起窗口、SSE 经
资产服务器长流）——M2 dogfood 与 M4 真机演练覆盖。`.app` bundle、NSIS 安装器、
ad-hoc 签名随 M4（v1.2 裁定：个人分发不做公证）。

数据目录 `os.UserConfigDir()/mosaic`；日志 stderr（JSON debug——M2 主线开发
默认开发者模式，计划 v1.8 裁定）。

## 开发（真机）

```bash
# 前置：go ≥1.25、wails CLI（go install github.com/wailsapp/wails/v2/cmd/wails@latest）
wails doctor   # 平台依赖（Windows: WebView2 Runtime；macOS: Xcode CLT）
wails dev      # 热重载（frontend 已并入 apps/web，经 internal/app 进程内装配）
wails build    # 平台打包
```
