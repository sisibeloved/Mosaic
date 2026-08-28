//go:build st

// ST 层：真实二进制 + 真实 HTTP 的系统级验证。
// 构建标签 st；CI 独立步骤执行（go test -tags st ./...）。
// 测试现场构建 cmd/mosaic-server 二进制（需要 go 在 PATH）。
package main_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func buildServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain 不在 PATH（ST 需要现场构建二进制）")
	}
	bin := filepath.Join(t.TempDir(), "mosaic-server")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// go test 的工作目录即本包目录（cmd/mosaic-server），故构建目标为 "."。
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("构建 mosaic-server 失败: %v\n%s", err, out)
	}
	return bin
}

type logLine struct {
	Msg  string `json:"msg"`
	Addr string `json:"addr"`
}

// waitListening 从 stdout 的 JSON 日志中解析实际监听地址（main 先 Listen 再 Serve，
// 日志携带 ln.Addr() 的真实值，支持 -addr 127.0.0.1:0 随机端口）。
func waitListening(t *testing.T, stdout io.Reader) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var l logLine
			if json.Unmarshal(sc.Bytes(), &l) == nil &&
				l.Msg == "mosaic-server listening" && l.Addr != "" {
				ch <- l.Addr
				return
			}
		}
	}()
	select {
	case addr, ok := <-ch:
		if !ok || addr == "" {
			t.Fatal("进程退出前未捕获监听地址（启动失败或日志格式变化）")
		}
		return addr
	case <-time.After(15 * time.Second):
		t.Fatal("等待监听地址超时")
		return ""
	}
}

// TestHealthz_ST：真实进程启动 → 随机端口 → /healthz 返回 200 与既定 JSON。
func TestHealthz_ST(t *testing.T) {
	bin := buildServer(t)
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	addr := waitListening(t, stdout)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if want := "{\"status\":\"ok\"}\n"; string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}
}

// TestGracefulShutdown_ST：SIGINT → 优雅退出（零码）。Windows 无法跨进程投递
// SIGINT，该路径在 Windows 上以代码审查 + UT 覆盖，此处仅 POSIX 端到端。
func TestGracefulShutdown_ST(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 进程不支持跨进程 SIGINT 投递；优雅退出由 POSIX 矩阵覆盖")
	}
	bin := buildServer(t)
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = waitListening(t, stdout)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("优雅退出应以零码结束: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("优雅退出超时")
	}
}

// TestInvalidFlagExitsNonZero_ST：非法参数快速失败且非零码退出。
func TestInvalidFlagExitsNonZero_ST(t *testing.T) {
	bin := buildServer(t)
	err := exec.Command(bin, "-definitely-not-a-flag").Run()
	if err == nil {
		t.Fatal("非法参数应以非零码退出")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("退出错误类型 = %T, want *exec.ExitError", err)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("退出码 = 0, want 非零")
	}
}

// TestDiscussionLoop_ST：北极星规格——命令 → 事件 → 游标订阅 → 回放的端到端闭环。
func TestDiscussionLoop_ST(t *testing.T) {
	t.Skip("TDD backlog（M1）：随命令 API + 事件存储 + SSE 订阅落地转绿；此用例固定验收口径")
}
