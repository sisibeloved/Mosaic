// 人类可读日志构造（v1.49）：长静默期排障需要一眼可读的绝对时间——JSON handler
// 的 RFC3339 时间戳阅读成本高（dogfood 反馈）。TextHandler + 本地时区
// "2006-01-02 15:04:05.000"，服务端与桌面壳共用同一出口。
package app

import (
	"io"
	"log/slog"
	"time"
)

// NewLogger 人类可读日志器（time 字段格式化为本地时区日期时间）。
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.Format("2006-01-02 15:04:05.000"))
				}
			}
			return a
		},
	}))
}
