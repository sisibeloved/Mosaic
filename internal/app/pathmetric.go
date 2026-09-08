// Package app 内文件：M4-3 路径指标出口——reply-or-pass 与两阶段的
// 场景/路径/结果/耗时追加写 JSONL（数据目录 metrics/reply-or-pass.jsonl）。
// A/B 测量数据面：分析脚本按 path 分组算 p50/p95 与调用数。
package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var metricMu sync.Mutex

func pathMetricSink(dataDir string) func(roomID, seat, scenario, path, outcome string, ms int64) {
	dir := filepath.Join(dataDir, "metrics")
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "reply-or-pass.jsonl")
	return func(roomID, seat, scenario, pathName, outcome string, ms int64) {
		row := map[string]any{
			"ts": time.Now().UTC().Format(time.RFC3339Nano), "room": roomID,
			"seat": seat, "scenario": scenario, "path": pathName,
			"outcome": outcome, "latency_ms": ms,
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return
		}
		metricMu.Lock()
		defer metricMu.Unlock()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(append(raw, '\n'))
	}
}
