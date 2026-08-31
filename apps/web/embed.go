// Package web：前端 SPA（ADR-0002 React+Vite）构建产物的 Go 侧载体。
// dist/ 由 `npm run build` 生成并入库；新鲜度由 CI 构建门禁把守
// （源码改动未重建即红，与 gen-ts/gen-api 同一纪律）。
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
