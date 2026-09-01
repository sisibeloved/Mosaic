//go:build !windows

package winhide

import "os/exec"

// Hide 非 Windows 平台 no-op（POSIX 侧无控制台窗口概念）。
func Hide(*exec.Cmd) {}
