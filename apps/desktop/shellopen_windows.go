//go:build windows

package main

import (
	"fmt"
	"os/exec"
)

// openExternal 经 rundll32 url.dll,FileProtocolHandler 调系统默认浏览器
// （URL 已由 httpapi 层校验 http/https，无空白/控制字符）。Start 不等待：
// FileProtocolHandler 的成败由系统弹窗体现，退出码不可靠。
func openExternal(u string) error {
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start(); err != nil {
		return fmt.Errorf("rundll32 open %s: %w", u, err)
	}
	return nil
}
