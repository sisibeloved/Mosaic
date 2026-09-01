//go:build windows

package winhide

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW（winbase）：子进程不建控制台；与 DETACHED_PROCESS 互斥，不并用。
const createNoWindow = 0x08000000

// Hide 抑制子进程控制台窗口（幂等；保留调用方已设的 SysProcAttr 字段）。
func Hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	cmd.SysProcAttr.HideWindow = true
}
