#!/usr/bin/env bash
# 构建编排（计划 v1.22：dist 不入库）：唯一构建入口，目标 = server | desktop | all。
# go:embed 在编译期解析 apps/web/dist（apps/web/embed.go）——缺失时 go build/vet/test
# 直接红，这是有意行为（不静默降级）。本脚本先产出 dist 再按目标分发：
#   server   → bin/mosaic-server（默认）
#   desktop  → wails build（apps/desktop/build/bin/，frontend 钩子自动前置 npm）
#   all      → 两者
set -euo pipefail
cd "$(dirname "$0")/../.."

target="${1:-server}"
case "$target" in
  server | desktop | all) ;;
  *) echo "用法：$0 [server|desktop|all]（默认 server）" >&2; exit 2 ;;
esac

command -v npm >/dev/null 2>&1 || { echo "错误：需要 node/npm（构建 SPA）" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "错误：需要 go ≥1.25" >&2; exit 1; }

# 1) SPA（两种目标的编译期依赖；依赖缺失时按 lock 精确还原）
cd apps/web
if [ ! -d node_modules ]; then
  npm ci
fi
npm run build
cd ../..

build_server() {
  go build -o bin/mosaic-server ./cmd/mosaic-server
  echo "bin/mosaic-server 就绪"
}

build_desktop() {
  # 不回退裸 go build：desktop 目标要的是 wails 打包产物（图标/资源），缺 CLI 就红。
  command -v wails >/dev/null 2>&1 || {
    echo "错误：desktop 目标需要 wails CLI：go install github.com/wailsapp/wails/v2/cmd/wails@latest" >&2
    exit 1
  }
  # 桌面壳 build tag 限 windows || darwin——Linux 宿主无法本地构建，
  # 默认交叉 windows/amd64（WAILS_PLATFORM 可覆盖，如 darwin/universal）；
  # windows/darwin 宿主走原生构建。交叉时 -skipbindings：绑定生成按宿主平台
  # 编译桌面壳（linux 下被 build tag 排除即红），且本应用无 wails 绑定方法，
  # 该步本就无产物。
  local platform_args=()
  if [ "$(uname -s)" = "Linux" ]; then
    platform_args=(-platform "${WAILS_PLATFORM:-windows/amd64}" -skipbindings)
  fi
  cd apps/desktop
  wails build "${platform_args[@]}" # frontend 钩子会再跑一次 npm（幂等，~1s）
  cd ../..
  echo "apps/desktop/build/bin/ 桌面产物就绪"
}

case "$target" in
  server) build_server ;;
  desktop) build_desktop ;;
  all)
    build_server
    build_desktop
    ;;
esac
