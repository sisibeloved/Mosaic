package app

import (
	"io"
	"os"
	"path/filepath"
)

// TeeWriter 逐路写入、单路失败不阻断其余路。v1.34 实证：GUI 子系统进程（wails
// windowsgui）无有效 stderr 句柄，io.MultiWriter 首路失败即中止——后续路（日志
// 文件）永远收不到写入，桌面 mosaic.log 恒空的根因。写入错误聚合返回首个
// （slog 静默吞，不遮蔽其余路）。
func TeeWriter(writers ...io.Writer) io.Writer {
	return teeWriter{writers: writers}
}

type teeWriter struct {
	writers []io.Writer
}

func (t teeWriter) Write(p []byte) (int, error) {
	var firstErr error
	for _, w := range t.writers {
		if _, err := w.Write(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(p), firstErr
}

// OpenLogFile 桌面壳日志落盘：数据目录 logs/mosaic.log（stderr 双写由调用方组合）。
// GUI 模式下 stderr 随进程丢失——2026-09-01 Kimi 定位实证：WSL 适配层每波失败的
// warn 无处可查，排障只能靠 DB 反推。轮转：启动时超 5MB 改名 .old（只保一代，
// 磁盘上限 ~10MB）。放本包（无构建标签）使桌面壳与测试在三个平台同源可跑。
func OpenLogFile(dataDir string) (*os.File, error) {
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "mosaic.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(path, path+".old") // 已有 .old 即被覆盖
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
