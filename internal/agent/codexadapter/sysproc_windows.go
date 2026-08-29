// Windows 变体：Job Object 整组管理随 M1 进程管理切片落地；
// 当前以 WaitDelay 兜底（进程退出后放弃残留 IO）。
//go:build windows

package codexadapter

import "os/exec"

func applySysProc(cmd *exec.Cmd) {}
