//go:build !windows

package wslenv

// NetEnv 非 Windows 平台 no-op（WSL 执行面仅存在于 Windows 宿主；
// native 运行面的网络配置由宿主环境直接提供，不经本包）。
func NetEnv(distro string) []string { return nil }
