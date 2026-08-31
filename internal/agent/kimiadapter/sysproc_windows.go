// Windows 变体：Job Object 整组管理属 M2 进程管理项（platform-notes 登记）；
// 当前以 WaitDelay 兜底（进程退出后放弃残留 IO）。
//go:build windows

package kimiadapter

import "os/exec"

func applySysProc(cmd *exec.Cmd) {}
