package minimax

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sisibeloved/Mosaic/internal/winhide"
)

// ReadDeliverAsset 只开放当前 Profile 工作区内的普通文件，返回二进制流。
func (a *Adapter) ReadDeliverAsset(ctx context.Context, source string) (io.ReadCloser, error) {
	if a.cfg.WorkDir == "" {
		return nil, fmt.Errorf("minimax: 未配置 Agent 工作区")
	}
	if a.cfg.WSLDistro == "" {
		return openNativeDeliverAsset(a.cfg.WorkDir, source)
	}
	var streamer assetStreamExecer
	if a.cfg.Execer != nil {
		var ok bool
		streamer, ok = a.cfg.Execer.(assetStreamExecer)
		if !ok {
			return nil, fmt.Errorf("minimax: 当前执行面不支持附件流")
		}
	} else {
		streamer = &processExecer{}
	}
	argv := []string{"wsl.exe", "-d", a.cfg.WSLDistro, "--exec", "sh", "-c", wslDeliverAssetScript, "sh", a.cfg.WorkDir, source}
	return streamer.Stream(ctx, argv, nil)
}

func openNativeDeliverAsset(root, source string) (io.ReadCloser, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("minimax: Agent 工作区不可读")
	}
	if !filepath.IsAbs(source) {
		source = filepath.Join(rootReal, source)
	}
	sourceReal, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, fmt.Errorf("minimax: 附件不存在或不可读")
	}
	rel, err := filepath.Rel(rootReal, sourceReal)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("minimax: 附件路径超出 Agent 工作区")
	}
	f, err := os.Open(sourceReal)
	if err != nil {
		return nil, fmt.Errorf("minimax: 附件打开失败")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("minimax: 附件不是普通文件")
	}
	return f, nil
}

const wslDeliverAssetScript = `set -eu
root=$(realpath -e -- "$1")
source=$(realpath -e -- "$2")
case "$source" in
  "$root"/*) ;;
  *) exit 64 ;;
esac
[ -f "$source" ] || exit 65
exec cat -- "$source"`

type assetStreamExecer interface {
	Stream(ctx context.Context, argv []string, env []string) (io.ReadCloser, error)
}

// Stream 启动受控命令并返回原始 stdout 字节流；调用方负责 Close。
func (p *processExecer) Stream(ctx context.Context, argv []string, env []string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	winhide.Hide(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return nil, err
	}
	return &processAssetReader{cmd: cmd, stdout: stdout, stderr: &stderr}, nil
}

type processAssetReader struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  *bytes.Buffer
	waitOne sync.Once
	waitErr error
}

func (r *processAssetReader) wait() error {
	r.waitOne.Do(func() { r.waitErr = r.cmd.Wait() })
	return r.waitErr
}

func (r *processAssetReader) Read(p []byte) (int, error) {
	n, err := r.stdout.Read(p)
	if err == io.EOF {
		if waitErr := r.wait(); waitErr != nil {
			return n, fmt.Errorf("minimax: 附件读取进程失败: %w: %s", waitErr, strings.TrimSpace(r.stderr.String()))
		}
	}
	return n, err
}

func (r *processAssetReader) Close() error {
	_ = r.stdout.Close()
	if r.cmd.ProcessState == nil || !r.cmd.ProcessState.Exited() {
		_ = r.cmd.Process.Kill()
	}
	return r.wait()
}
