// Windows 变体：Job Object 整组管理属 M2 进程管理项（platform-notes 登记）；
// 当前以 WaitDelay 兜底（进程退出后放弃残留 IO）。
// 子进程一律不建控制台窗口（winhide）——桌面壳内 zcode/wsl.exe 调用不再闪黑框。
//go:build windows

package zcode

import (
	"os/exec"

	"github.com/sisibeloved/Mosaic/internal/winhide"
)

func applySysProc(cmd *exec.Cmd) {
	winhide.Hide(cmd)
}

// killGroup POSIX 专属组收尾的 Windows 空实现：无进程组语义（Job Object 属 M2
// 进程管理项），Windows 侧未观察到弹窗/残留问题，行为保持不变。
func killGroup(int) {}
