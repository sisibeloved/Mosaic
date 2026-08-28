// Mosaic 桌面壳（ADR-0010：Wails v2，Windows=WebView2 / macOS=WKWebView）。
// M0 最小壳 spike：验证模块依赖图与无 CLI 纯 go build 可达；
// 绑定生成、托盘、安装器随 M2/M4 演进。独立 go module——不进服务端 CI 主链。
package main

import (
	"context"
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend
var assets embed.FS

// App 暴露给前端的绑定方法（M0 占位：健康探针）。
type App struct{}

func (a *App) Ping() string { return "pong" }

func main() {
	app := &App{}
	err := wails.Run(&options.App{
		Title:     "Mosaic",
		Width:     1180,
		Height:    780,
		MinWidth:  860,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: func(ctx context.Context) {
			// M2 起：SSE 订阅游标、命令 API 客户端在此接线
			_ = ctx
		},
		Bind: []interface{}{app},
		Mac:  &mac.Options{TitleBar: mac.TitleBarDefault()},
	})
	if err != nil {
		println("mosaic-desktop:", err.Error())
	}
}
