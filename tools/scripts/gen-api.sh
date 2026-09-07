#!/usr/bin/env bash
# HTTP 边界模型生成链（ADR-0007）：api/http-api/openapi.yaml（权威源）
# → internal/transport/httpapi/apigen/api.gen.go（ServerInterface + 模型 + 内嵌 spec）。
# 版本经 go.mod tool 指令锁定（升级 = 改 go.mod + 重生成 + 随代码提交）。
# 本地 WSL 开发：PATH="$HOME/.local/go/bin:$PATH" bash tools/scripts/gen-api.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

go tool oapi-codegen -config api/http-api/gen.yaml api/http-api/openapi.yaml
echo "生成 internal/transport/httpapi/apigen/api.gen.go"
