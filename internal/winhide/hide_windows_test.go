//go:build windows

package winhide

import (
	"os/exec"
	"testing"
)

// 桌面壳防闪窗契约：Hide 必须设 CREATE_NO_WINDOW + HideWindow，且幂等。
// 仅 Windows CI 腿执行（build tag）。
func TestHideSetsNoWindowFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo hi")
	Hide(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr 未设置")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatal("CREATE_NO_WINDOW 未设置")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow 未设置")
	}
	Hide(cmd) // 幂等：重复调用不得清位
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatal("重复调用丢失 CREATE_NO_WINDOW")
	}
}
