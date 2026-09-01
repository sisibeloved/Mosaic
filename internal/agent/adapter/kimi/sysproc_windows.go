// Windows 变体：Job Object 整组管理属 M2 进程管理项（platform-notes 登记）；
// 当前以 WaitDelay 兜底（进程退出后放弃残留 IO）。
// 子进程一律不建控制台窗口（winhide）——桌面壳内 kimi/wsl.exe 调用不再闪黑框。
//go:build windows

package kimi

import (
	"os/exec"

	"github.com/sisibeloved/Mosaic/internal/winhide"
)

func applySysProc(cmd *exec.Cmd) {
	winhide.Hide(cmd)
}
