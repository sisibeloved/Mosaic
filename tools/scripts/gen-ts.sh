#!/usr/bin/env bash
# TS 边界类型生成链（ADR-0007）：JSON Schema（权威源）→ gen/ts/*.d.ts。
# 需本机 node/npx；产物入库。版本锁定（二轮审校 #23）：漂移门禁要求生成确定性，
# 不锁版本时上游 minor 更新即可让 CI 假红/假绿。升级版本 = 改此行 + 重生成 + 随代码提交。
set -euo pipefail
cd "$(dirname "$0")/../.."
cd api/room-protocol

JSTT_VERSION="15.0.3"
command -v npx >/dev/null 2>&1 || { echo "错误：需要 node/npx（用于 json-schema-to-typescript）" >&2; exit 1; }
mkdir -p gen/ts
for f in events/*.schema.json envelope.schema.json command.schema.json; do
  base="$(basename "$f" .schema.json)"
  npx --yes "json-schema-to-typescript@${JSTT_VERSION}" "$f" -o "gen/ts/${base}.d.ts"
  echo "生成 gen/ts/${base}.d.ts"
done
