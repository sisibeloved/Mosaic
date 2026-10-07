//go:build !darwin

package harness

// DesktopHelperInterpreter 非 darwin 空实现（无 LaunchServices/Dock 面）。
func DesktopHelperInterpreter(exePath, _ string) string { return exePath }
