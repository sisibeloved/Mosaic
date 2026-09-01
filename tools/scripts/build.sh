#!/usr/bin/env bash
# 构建编排（计划 v1.22：dist 不入库）：SPA → Go 服务端二进制。
# go:embed 在编译期解析 apps/web/dist（apps/web/dist.go）——缺失时 go build/vet/test
# 直接红，这是有意行为（不静默降级）。本脚本负责先产出 dist 再编译。
# 桌面产物不在此列：cd apps/desktop && wails build（wails.json frontend 钩子自动前置 npm）。
set -euo pipefail
cd "$(dirname "$0")/../.."

command -v npm >/dev/null 2>&1 || { echo "错误：需要 node/npm（构建 SPA）" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "错误：需要 go ≥1.25" >&2; exit 1; }

# 1) SPA（依赖缺失时按 lock 精确还原）
cd apps/web
if [ ! -d node_modules ]; then
  npm ci
fi
npm run build
cd ../..

# 2) Go 服务端二进制（bin/ 已忽略）
go build -o bin/mosaic-server ./cmd/mosaic-server
echo "bin/mosaic-server 就绪；桌面产物：cd apps/desktop && wails build"
