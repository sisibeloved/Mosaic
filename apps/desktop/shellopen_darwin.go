//go:build darwin

package main

import (
	"fmt"
	"os/exec"
)

// openExternal 经 open(1) 调系统默认浏览器（URL 已由 httpapi 层校验 http/https）。
func openExternal(u string) error {
	if err := exec.Command("open", u).Run(); err != nil {
		return fmt.Errorf("open %s: %w", u, err)
	}
	return nil
}
