package httpapi

import _ "embed"

// indexHTML：Timeline 最小 UI（M1 内嵌单页；React/Vite SPA 随 M2 接入）。
//
//go:embed webui.html
var indexHTML string
