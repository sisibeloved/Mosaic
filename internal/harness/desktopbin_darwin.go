//go:build darwin

package harness

import (
	"os"
	"path/filepath"
)

// DesktopHelperInterpreter 桌面 bundle 实例（zcode 桌面渠道）的无头解释器路径：
// 主二进制以主 bundle 身份注册 LaunchServices（Dock 弹出第二个 ZCode，2026-10-07
// 真机实证），改用内嵌 Helper 二进制（dev.zcode.app.helper，无 Dock 面；同
// desktopInterpreter，适配器包——分层不互通，双份实现）。Helper 缺失回退原路径。
func DesktopHelperInterpreter(exePath, bundle string) string {
	if exePath == "" || bundle == "" {
		return exePath
	}
	app := bundle
	for i := 0; i < 3; i++ {
		app = filepath.Dir(app)
	}
	helper := filepath.Join(app, "Frameworks", "ZCode Helper.app", "Contents", "MacOS", "ZCode Helper")
	if _, err := os.Stat(helper); err == nil {
		return helper
	}
	return exePath
}
