//go:build it

// IT 层：真实探测面——本机安装的 CLI 实际扫描与登录态判定。
// CI 无 codex/kimi 时自动跳过（探测面在 UT fake 已覆盖语义）。
package harness

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
)

func TestHostRunnerScanRealCLIs_IT(t *testing.T) {
	runner := NewHostRunner()
	reg, err := LoadOrCreate(t.TempDir() + "/harness.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{IncludeWSL: runtime.GOOS == "windows"}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) == 0 {
		t.Skip("本机未安装任何首批 CLI（CI 常态）：探测语义由 UT 覆盖")
	}
	for _, exe := range list {
		t.Logf("发现 %s: %s login=%s version=%q", exe.Adapter, exe.Path, exe.Login, exe.Version)
		switch exe.Adapter {
		case "codex":
			if _, err := exec.LookPath("codex"); err == nil {
				if exe.Login != LoginLoggedIn {
					t.Errorf("本机 codex 应为 logged_in（已实证 login status 输出 Logged in）：got %s", exe.Login)
				}
				if exe.Version == "" || exe.Digest == "" {
					t.Errorf("codex 版本/摘要缺失：%+v", exe)
				}
			}
		case "kimi":
			if _, err := exec.LookPath("kimi"); err == nil {
				if exe.Login != LoginLoggedIn && exe.Login != LoginLoggedOut {
					t.Errorf("kimi 登录态应可判定：got %s", exe.Login)
				}
			}
		}
	}
}

// 登录门控在真实注册表上：对发现项做启用翻转（已登录者必须成功）。
func TestHostRunnerEnableGate_IT(t *testing.T) {
	runner := NewHostRunner()
	reg, err := LoadOrCreate(t.TempDir() + "/harness.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := reg.Scan(context.Background(), runner, BuiltinProbes, ScanOptions{IncludeWSL: runtime.GOOS == "windows"}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	list := reg.List()
	if len(list) == 0 {
		t.Skip("本机无 CLI")
	}
	for _, exe := range list {
		err := reg.SetEnabled(exe.ID, true)
		switch exe.Login {
		case LoginLoggedIn:
			if err != nil {
				t.Errorf("已登录的 %s 应可启用: %v", exe.Adapter, err)
			}
		case LoginLoggedOut, LoginUnknown:
			if err == nil {
				t.Errorf("未登录/未知的 %s 不得启用", exe.Adapter)
			}
		}
	}
}
