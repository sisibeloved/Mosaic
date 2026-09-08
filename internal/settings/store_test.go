package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreDefaultsAndRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := s.Snapshot().RunTimeoutSeconds; got != DefaultRunTimeoutSeconds {
		t.Fatalf("缺省 run_timeout = %d, want %d", got, DefaultRunTimeoutSeconds)
	}
	if got := s.RunTimeout(); got != 10*time.Minute {
		t.Fatalf("RunTimeout = %v, want 10m", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("未保存前不应落盘（惰性创建）")
	}

	if err := s.UpdateRunTimeoutSeconds(120); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := s.RunTimeout(); got != 2*time.Minute {
		t.Fatalf("更新后 RunTimeout = %v, want 2m", got)
	}

	// 重开：持久化生效
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Snapshot().RunTimeoutSeconds; got != 120 {
		t.Fatalf("重开后 run_timeout = %d, want 120", got)
	}
	// 权限纪律：0600
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("设置文件权限 = %o, want 600", fi.Mode().Perm())
	}
}

func TestStoreValidationAndFailSafe(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, bad := range []int{0, -1, MinRunTimeoutSeconds - 1, MaxRunTimeoutSeconds + 1} {
		if err := s.UpdateRunTimeoutSeconds(bad); err == nil {
			t.Fatalf("越界值 %d 应被拒绝", bad)
		}
	}

	// 损坏文件：Open 报错（装配层 fail safe 回缺省——本测试钉住错误可见）
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("损坏文件应返回错误")
	}

	// 字段缺失（未来版本前向兼容）：回缺省
	path2 := filepath.Join(dir, "settings2.json")
	if err := os.WriteFile(path2, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	s3, err := Open(path2)
	if err != nil {
		t.Fatalf("open empty: %v", err)
	}
	if got := s3.Snapshot().RunTimeoutSeconds; got != DefaultRunTimeoutSeconds {
		t.Fatalf("空文档 run_timeout = %d, want 缺省 %d", got, DefaultRunTimeoutSeconds)
	}
}
