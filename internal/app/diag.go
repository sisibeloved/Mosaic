// M4-0 系统面装配：自诊断 bundle 构造。
// 内容纪律（OQ-20 同源）：不含凭据、不含环境变量——版本/运行时/数据面统计/
// 注册表状态（adapter/runtime/login/version，路径不入包）/日志尾（截断）。
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// diagLogTailLines 诊断包日志尾行数上限（mosaic.log 最近 N 行）。
const diagLogTailLines = 200

// diagnosticsBundle 构造自诊断快照。依赖注入面（store/registry/seats）由
// Start 闭包捕获——此处只做汇总，不做策略。
func diagnosticsBundle(dataDir string, rooms func() (int, error), seats func() int, registry func() []map[string]any) func() (map[string]any, error) {
	return func() (map[string]any, error) {
		bundle := map[string]any{
			"generated_at": time.Now().UTC().Format(time.RFC3339),
			"runtime": map[string]any{
				"go":      runtime.Version(),
				"os":      runtime.GOOS,
				"arch":    runtime.GOARCH,
				"version": buildVersion(),
			},
			"data": map[string]any{
				"dir":            dataDir,
				"db_size_bytes":  fileSize(filepath.Join(dataDir, "mosaic.db")),
				"wal_size_bytes": fileSize(filepath.Join(dataDir, "mosaic.db-wal")),
				"log_size_bytes": fileSize(filepath.Join(dataDir, "logs", "mosaic.log")),
			},
			"pending_restore": map[string]any{
				"exists":       restoreMarkerExists(filepath.Join(dataDir, "pending-restore.json")),
				"failedMarker": restoreMarkerExists(filepath.Join(dataDir, "pending-restore.failed.json")),
			},
		}
		if rooms != nil {
			if n, err := rooms(); err == nil {
				bundle["room_count"] = n
			} else {
				bundle["room_count_error"] = err.Error()
			}
		}
		if seats != nil {
			bundle["seat_count"] = seats()
		}
		if registry != nil {
			bundle["harness"] = registry()
		}
		if tail, err := logTail(filepath.Join(dataDir, "logs", "mosaic.log"), diagLogTailLines); err == nil {
			bundle["log_tail"] = tail
		} else {
			bundle["log_tail_error"] = err.Error()
		}
		return bundle, nil
	}
}

// buildVersion 构建版本（不虚构：无版本信息时如实返回 dev）。
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return -1
}

func restoreMarkerExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// logTail 取文件末 N 行（日志尾快照；文件不存在返回错误由调用方如实记录）。
func logTail(path string, n int) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("log tail: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
