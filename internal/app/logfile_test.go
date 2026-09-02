package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 桌面日志落盘：常规路径追加写入；超 5MB 启动轮转（.old 只保一代）。
// 背景：GUI 模式 stderr 丢失，适配层失败的 warn 无处可查（2026-09-01 Kimi 定位实证）。
func TestOpenLogFileAppendsAndRotates(t *testing.T) {
	dir := t.TempDir()

	f1, err := OpenLogFile(dir)
	if err != nil {
		t.Fatalf("OpenLogFile: %v", err)
	}
	if _, err := f1.WriteString("line1\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f1.Close()

	f2, err := OpenLogFile(dir) // 再次打开：追加不截断
	if err != nil {
		t.Fatalf("OpenLogFile: %v", err)
	}
	if _, err := f2.WriteString("line2\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f2.Close()
	got, _ := os.ReadFile(filepath.Join(dir, "logs", "mosaic.log"))
	if string(got) != "line1\nline2\n" {
		t.Fatalf("log = %q, want 追加两行", got)
	}

	// 轮转：把当前日志撑过 5MB 再打开——旧内容进 .old，新文件从空开始。
	big, err := os.OpenFile(filepath.Join(dir, "logs", "mosaic.log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	if _, err := big.Write(bytes.Repeat([]byte("x"), 5<<20+1)); err != nil {
		t.Fatalf("grow write: %v", err)
	}
	big.Close()

	f3, err := OpenLogFile(dir)
	if err != nil {
		t.Fatalf("OpenLogFile rotate: %v", err)
	}
	f3.Close()
	if fi, err := os.Stat(filepath.Join(dir, "logs", "mosaic.log.old")); err != nil || fi.Size() < 5<<20 {
		t.Fatalf("mosaic.log.old 应保留旧内容（%v, %v）", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "logs", "mosaic.log")); err != nil || fi.Size() != 0 {
		t.Fatalf("轮转后 mosaic.log 应为空（size=%v, err=%v）", fi, err)
	}
}

// errWriter 恒失败的写路（GUI 子系统进程 stderr 无句柄的代理）。
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("invalid handle") }

// TestTeeWriterSurvivesBrokenWriter：首路恒败（GUI stderr）不得阻断日志文件路——
// v1.34 实证 io.MultiWriter 首路失败即中止、mosaic.log 恒空的根因。
func TestTeeWriterSurvivesBrokenWriter(t *testing.T) {
	var buf bytes.Buffer
	w := TeeWriter(errWriter{}, &buf)
	if _, err := w.Write([]byte("line1\n")); err == nil {
		t.Fatal("首路错误应聚合返回（不静默吞）")
	}
	if _, err := w.Write([]byte("line2\n")); err == nil {
		t.Fatal("错误应持续可见")
	}
	if buf.String() != "line1\nline2\n" {
		t.Fatalf("文件路应完整收到写入：%q", buf.String())
	}
}
