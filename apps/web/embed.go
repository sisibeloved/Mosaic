// Package web：前端 SPA（ADR-0002 React+Vite）构建产物的 Go 侧载体。
// dist/ 不入库（计划 v1.22 裁定，撤销 v1.7 入库口径）：由 npm run build 生成——
// 本机走 tools/scripts/build.sh；桌面 wails build 经 wails.json frontend 钩子自动
// 前置；CI 在一切 Go 门禁前先建。go:embed 编译期解析 dist——缺失时
// go build/vet/test 直接红（有意：不静默降级）。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist 返回 SPA 产物根（index.html 位于根）。
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("web: dist 嵌入异常: " + err.Error())
	}
	return sub
}
