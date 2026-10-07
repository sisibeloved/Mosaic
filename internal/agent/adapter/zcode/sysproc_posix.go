// POSIX 进程组：超时击杀必须整组，否则孙进程握着管道导致 Wait 永挂
// （codex 适配器 2026-08-28 IT 实证教训，kimi/minimax 同形态继承）。
//go:build !windows

package zcode

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

// killGroup 主进程退出后的组收尾。真机实证（2026-10-07，macOS）：zcode 桌面
// bundle 的插件宿主（__zcode-plugin-host 孙进程）不随主进程退出——残留宿主以
// 约两秒间隔反复重派 ZCode.app 子进程，非 headless 形态启动即闪 GUI 窗口
// （桌面壳"单独进程窗口弹出"根因）。Wait 已回收主进程，对残余组发 SIGKILL；
// 组已空时的错误静默即可。
func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
