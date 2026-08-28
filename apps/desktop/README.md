# mosaic-desktop——桌面壳（Wails v2，ADR-0010）

独立 Go module（`github.com/sisobeloved/Mosaic/apps/desktop`），不进服务端主链 CI；
Wails v2（v2.15.x，2026 年仍是稳定维护线；v3 处于 beta，按 ADR-0010 不采用）。

## M0 spike 结论

- 依赖图在无 GUI 的 Linux 环境完整解析（`go mod tidy`）；
- **Windows amd64 与 macOS arm64 均可纯 Go 交叉编译**（`CGO_ENABLED=0`，wails v2.15 的
  objc 运行时已免 cgo）：ubuntu/headless 上产出 PE32+ 与 Mach-O 可执行文件；
- 运行时验证（WebView2/WKWebView 实际拉起窗口）需真机 + wails CLI 打包，属 M2 接入
  SPA 时在用户双平台机器上完成；`.app` bundle 与 ad-hoc 签名随 M4 安装器（交付规划 v1.2：
  个人分发不做公证）。

## 开发

```bash
# 前置：go ≥1.25、wails CLI（go install github.com/wailsapp/wails/v2/cmd/wails@latest）
wails doctor          # 检查平台依赖（Windows: WebView2 Runtime；macOS: Xcode CLT）
wails dev             # 热重载开发
wails build           # 产出平台安装包/二进制
```

M2 起 `frontend/` 替换为 React/Vite SPA（ADR-0002），与 `apps/web` 共用
`api/room-protocol/gen/ts` 边界类型。
