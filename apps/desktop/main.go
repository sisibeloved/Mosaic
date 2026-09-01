//go:build windows || darwin

// Mosaic 桌面壳（ADR-0010：Wails v2；Windows=WebView2 / macOS=WKWebView）。
// M2：进程内装配（internal/app）——产品形态"本地单进程桌面应用"（D-2/D-3），
// 与 mosaic-server 共用同一装配（引擎/分发/宿主层），不再经子进程。
//
// 传输形态（dogfood 修复）：装配以回环 TCP（127.0.0.1:0）启动，Wails 资产服务器的
// Handler 仅做 307 重定向，让 WebView 直接落在真实 HTTP 源上。原因：Windows 上
// WebView2 的 WebResourceRequested 资产桥会缓冲/改写响应——SSE（text/event-stream）
// 过不去，EventSource 永久 CLOSED，UI 卡"恢复中"。落到回环源后 SPA/API/SSE 与
// web 形态完全一致（同源相对路径、同源 owner token bootstrap），流式与后续任何
// 长响应均不再依赖壳的资产桥。wails.localhost 仍保留为跨源写门信任源（重定向前
// 的首个导航页可达），Origin 不可伪造、.localhost 顶级域不落远端。
// Linux 不构建（gtk/webkit2 面不在本仓 CI 覆盖内）；验证在 Windows/macOS 真机
// （M2 dogfood / M4 双平台演练）。
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sisibeloved/Mosaic/internal/app"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

// newAssetServerOptions 不设置 Assets：所有请求落到 Handler——这里是一枚 307
// 重定向（Location: 回环源 + 路径与查询串），WebView 首次导航即离开资产桥。
// 注意资产桥传来的 RequestURI 是绝对 URL 形式（http://wails.localhost/...），
// 须经 r.URL.RequestURI() 取 path+query 再拼接。Wails 初始化会校验非 nil 的
// Assets 根目录，因此不能使用永不命中的虚拟 FS。
func newAssetServerOptions(externalBase string) *assetserver.Options {
	return &assetserver.Options{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, externalBase+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		}),
	}
}

func main() {
	// M2 主线开发默认在开发者模式上进行（计划 v1.8 裁定；设置页有 UI 开关）；
	// 桌面日志走 stderr。
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelDebug)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	configDir, err := os.UserConfigDir()
	if err != nil {
		logger.Error("user config dir 不可用", "err", err)
		os.Exit(1)
	}
	srv, err := app.Start(ctx, app.Options{
		DataDir: filepath.Join(configDir, "mosaic"),
		// 回环 TCP：SSE 等流式响应不经 Wails 资产桥（见文件头注释）。
		Addr:             "127.0.0.1:0",
		Logger:           logger,
		Dev:              true,
		ExtraOriginHosts: []string{"wails.localhost"},
	})
	if err != nil {
		logger.Error("mosaic 桌面装配失败", "err", err)
		os.Exit(1)
	}
	externalBase := "http://" + srv.Addr()

	if err := wails.Run(&options.App{
		Title:       "Mosaic",
		Width:       1180,
		Height:      780,
		MinWidth:    860,
		MinHeight:   600,
		AssetServer: newAssetServerOptions(externalBase),
		OnShutdown: func(context.Context) {
			srv.Shutdown(context.Background())
		},
		Mac: &mac.Options{TitleBar: mac.TitleBarDefault()},
	}); err != nil {
		srv.Shutdown(context.Background())
		logger.Error("mosaic 桌面壳退出异常", "err", err)
		os.Exit(1)
	}
}
