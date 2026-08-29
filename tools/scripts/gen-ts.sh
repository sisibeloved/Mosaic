#!/usr/bin/env bash
# TS 边界类型生成链（ADR-0007）：JSON Schema（权威源）→ gen/ts/*.d.ts。
# 需本机 node/npx；产物入库，CI 不执行生成、只消费。
# Schema 变更后必须重跑本脚本并随代码一起提交。
set -euo pipefail
cd "$(dirname "$0")/../.."
cd api/room-protocol

command -v npx >/dev/null 2>&1 || { echo "错误：需要 node/npx（用于 json-schema-to-typescript）" >&2; exit 1; }
mkdir -p gen/ts
for f in events/*.schema.json envelope.schema.json command.schema.json; do
  base="$(basename "$f" .schema.json)"
  npx --yes json-schema-to-typescript "$f" -o "gen/ts/${base}.d.ts"
  echo "生成 gen/ts/${base}.d.ts"
done
