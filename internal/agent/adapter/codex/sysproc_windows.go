// Windows 变体：Job Object 整组管理随 M1 进程管理切片落地；
// 当前以 WaitDelay 兜底（进程退出后放弃残留 IO）。
// 子进程一律不建控制台窗口（winhide）——桌面壳内 codex/wsl.exe 调用不再闪黑框。
//go:build windows

package codex

import (
	"os/exec"

	"github.com/sisibeloved/Mosaic/internal/winhide"
)

func applySysProc(cmd *exec.Cmd) {
	winhide.Hide(cmd)
}
