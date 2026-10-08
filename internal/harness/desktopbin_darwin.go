//go:build darwin

package harness

import (
	"os"
	"path/filepath"
)

// DesktopHelperInterpreter 桌面 bundle 实例（zcode 桌面渠道）的无头解释器路径：
// 主二进制以主 bundle 身份注册 LaunchServices（Dock 弹出第二个 ZCode，2026-10-07
// 真机实证），改用内嵌 Helper 二进制（dev.zcode.app.helper，无 Dock 面；同
// desktopInterpreter，适配器包——分层不互通，双份实现）。从 exe 路径派生
// （bundle 可能为缓存副本路径，上溯不可达安装位，2026-10-08 实证）。Helper
// 缺失回退原路径。
func DesktopHelperInterpreter(exePath, bundle string) string {
	if exePath == "" {
		return exePath
	}
	// exe = <app>/Contents/MacOS/<name> → app 根上溯三级（文件 → MacOS → Contents → app）
	app := filepath.Dir(filepath.Dir(filepath.Dir(exePath)))
	helper := filepath.Join(app, "Contents", "Frameworks", "ZCode Helper.app", "Contents", "MacOS", "ZCode Helper")
	if _, err := os.Stat(helper); err == nil {
		return helper
	}
	return exePath
}
