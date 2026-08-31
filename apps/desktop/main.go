//go:build windows || darwin

// Mosaic 桌面壳（ADR-0010：Wails v2；Windows=WebView2 / macOS=WKWebView）。
// M2：进程内装配（internal/app）——产品形态"本地单进程桌面应用"（D-2/D-3），
// 与 mosaic-server 共用同一装配（引擎/分发/宿主层），不再经子进程。
// WebView 的全部请求（SPA 资产 + API + SSE）经资产服务器 Handler 直连 httpapi
// mux——同源相对路径调用；wails.localhost 作为跨源写门信任源（Origin 不可伪造、
// .localhost 顶级域不落远端，只能来自本应用自带 WebView 的页面）。
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

// newAssetServerOptions 不设置 Assets，使所有请求直接落到 Handler（httpapi mux 直连）。
// Wails 会在初始化时校验非 nil 的 Assets 根目录，因此不能使用永不命中的虚拟 FS。
func newAssetServerOptions(handler http.Handler) *assetserver.Options {
	return &assetserver.Options{Handler: handler}
}

func main() {
	// M2 主线开发默认在开发者模式上进行（计划 v1.8 裁定）；桌面日志走 stderr。
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
		DataDir:          filepath.Join(configDir, "mosaic"),
		Logger:           logger,
		Dev:              true,
		ExtraOriginHosts: []string{"wails.localhost"},
	})
	if err != nil {
		logger.Error("mosaic 桌面装配失败", "err", err)
		os.Exit(1)
	}

	if err := wails.Run(&options.App{
		Title:       "Mosaic",
		Width:       1180,
		Height:      780,
		MinWidth:    860,
		MinHeight:   600,
		AssetServer: newAssetServerOptions(srv.Handler()),
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
