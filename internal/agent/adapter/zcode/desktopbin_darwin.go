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
// zcode-cli 工人同机制，注册于 dev.zcode.app.helper（helper bundle，无 Dock 面）。
// 派生必须从 exe 路径出发：运行时 bundle 是缓存副本路径
// （~/Library/Caches/mosaic/zcode-desktop/<hash>/zcode.cjs），从中上溯定位不到
// 安装位（2026-10-08 真机实证：bundle 派生版静默回退主二进制，注册照旧）。
func desktopInterpreter(exePath, bundle string) string {
	if exePath == "" {
		return exePath
	}
	// exe = <app>/Contents/MacOS/<name> → app 根上溯三级（文件 → MacOS → Contents → app）
	app := filepath.Dir(filepath.Dir(filepath.Dir(exePath)))
	helper := filepath.Join(app, "Contents", "Frameworks", "ZCode Helper.app", "Contents", "MacOS", "ZCode Helper")
	if _, err := os.Stat(helper); err == nil && !strings.HasSuffix(exePath, "ZCode Helper") {
		return helper
	}
	return exePath
}
