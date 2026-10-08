//go:build darwin

package zcode

import (
	"os"
	"path/filepath"
	"testing"
)

// desktopInterpreter 派生契约：从 exe 路径上溯两级定位 app 根，Helper 存在即
// 替换、缺失回退主二进制；exe 已是 Helper 时不重复替换（防御幂等）；bundle 为
// 缓存副本路径时不影响派生（2026-10-08 回归：bundle 派生版对缓存路径失效）。
func TestDesktopInterpreterDerivation(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "ZCode.app")
	main := filepath.Join(app, "Contents", "MacOS", "ZCode")
	helper := filepath.Join(app, "Contents", "Frameworks", "ZCode Helper.app", "Contents", "MacOS", "ZCode Helper")
	if err := os.MkdirAll(filepath.Dir(main), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 运行时真实形态：bundle 指向缓存副本（不在安装位内）
	cacheBundle := filepath.Join(root, "Library", "Caches", "mosaic", "zcode-desktop", "abc123", "zcode.cjs")

	// Helper 不存在：回退主二进制（即便 bundle 是缓存路径）
	if got := desktopInterpreter(main, cacheBundle); got != main {
		t.Fatalf("Helper 缺失应回退主二进制，got %q", got)
	}
	// Helper 存在：替换
	if err := os.MkdirAll(filepath.Dir(helper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := desktopInterpreter(main, cacheBundle); got != helper {
		t.Fatalf("Helper 存在应替换（缓存 bundle 不影响派生），got %q want %q", got, helper)
	}
	// 幂等：exe 已是 Helper 时恒等
	if got := desktopInterpreter(helper, cacheBundle); got != helper {
		t.Fatalf("exe 已是 Helper 应恒等，got %q", got)
	}
	// 空 exe 直通
	if got := desktopInterpreter("", cacheBundle); got != "" {
		t.Fatalf("空 exe 应直通，got %q", got)
	}
}
