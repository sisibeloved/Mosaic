//go:build darwin

package zcode

import (
	"os"
	"path/filepath"
	"testing"
)

// desktopInterpreter 派生契约：bundle 上溯三级定位 app 根，Helper 存在即替换、
// 缺失回退主二进制；exe 已是 Helper 时不重复替换（防御幂等）。
func TestDesktopInterpreterDerivation(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "ZCode.app")
	bundle := filepath.Join(app, "Contents", "Resources", "glm", "zcode.cjs")
	helper := filepath.Join(app, "Contents", "Frameworks", "ZCode Helper.app", "Contents", "MacOS", "ZCode Helper")
	if err := os.MkdirAll(filepath.Dir(helper), 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(app, "Contents", "MacOS", "ZCode")

	// Helper 不存在：回退主二进制
	if got := desktopInterpreter(main, bundle); got != main {
		t.Fatalf("Helper 缺失应回退主二进制，got %q", got)
	}
	// Helper 存在：替换
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := desktopInterpreter(main, bundle); got != helper {
		t.Fatalf("Helper 存在应替换，got %q want %q", got, helper)
	}
	// 幂等：exe 已是 Helper 时不替换（恒等）
	if got := desktopInterpreter(helper, bundle); got != helper {
		t.Fatalf("exe 已是 Helper 应恒等，got %q", got)
	}
	// 空参直通
	if got := desktopInterpreter("", bundle); got != "" {
		t.Fatalf("空 exe 应直通，got %q", got)
	}
	if got := desktopInterpreter(main, ""); got != main {
		t.Fatalf("空 bundle 应直通，got %q", got)
	}
}
