//go:build darwin

package zcode

import (
	"os"
	"path/filepath"
	"strings"
)

// desktopInterpreter 桌面渠道无头解释器（darwin）：主二进制即便以
// ELECTRON_RUN_AS_NODE 运行，也会以 dev.zcode.app 主 bundle 身份注册
// LaunchServices——任务期间 Dock 弹出"第二个 ZCode"应用实体、任务结束消失
// （2026-10-07 真机实证：主 bundle ASN 注册随每次任务出现/消失；复制品因代码
// 签名被 SIGKILL）。改用内嵌 Helper 二进制承载同一 node 语义——与桌面版自身
// zcode-cli 工人同机制，注册于 dev.zcode.app.helper（helper bundle，无 Dock 面，
// 同日实证：任务前后主 bundle 注册恒为 1）。Helper 缺失时回退主二进制。
func desktopInterpreter(exePath, bundle string) string {
	if exePath == "" || bundle == "" {
		return exePath
	}
	// bundle = <app>/Contents/Resources/glm/zcode.cjs → app 根上溯三级
	app := bundle
	for i := 0; i < 3; i++ {
		app = filepath.Dir(app)
	}
	helper := app + string(filepath.Separator) + filepath.Join("Frameworks", "ZCode Helper.app", "Contents", "MacOS", "ZCode Helper")
	if _, err := os.Stat(helper); err == nil && !strings.HasSuffix(exePath, "ZCode Helper") {
		return helper
	}
	return exePath
}
