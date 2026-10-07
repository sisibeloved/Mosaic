//go:build !darwin

package zcode

// desktopInterpreter 桌面渠道无头解释器（非 darwin 空实现）：Windows 无
// LaunchServices/Dock 面，主二进制直跑（CREATE_NO_WINDOW 已防控制台闪框）。
func desktopInterpreter(exePath, _ string) string { return exePath }
