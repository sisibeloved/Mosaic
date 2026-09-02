// POSIX 进程组：超时击杀必须整组，否则孙进程握着管道导致 Wait 永挂
// （codex 适配器 2026-08-28 IT 实证教训，kimi 同形态继承）。
//go:build !windows

package minimax

import (
	"os/exec"
	"syscall"
)

func applySysProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 组击杀：ctx 取消时 CommandContext 只杀直接子进程，这里接管为杀整个进程组
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
