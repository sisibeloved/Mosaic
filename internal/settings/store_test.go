package settings

import (
	"os"
	"path/filepath"
	goruntime "runtime"
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
	// 附录 K：旧设置文件（无 speak gate 字段）零值回填缺省束——cooldown 的
	// <=0 回填不可写成 <0（2026-09-17 真机实证：旧文件迁移后冷却恒 0）。
	doc := s.Snapshot()
	if doc.SpeakGateOff {
		t.Fatal("缺省 speak_gate_off 应为 false（闸开）")
	}
	if doc.SpeakGateThresh != DefaultSpeakGateThreshold {
		t.Fatalf("缺省 speak_gate_threshold = %v, want %v", doc.SpeakGateThresh, DefaultSpeakGateThreshold)
	}
	if doc.SpeakGateCooldown != DefaultSpeakGateCooldown {
		t.Fatalf("缺省 speak_gate_cooldown_penalty = %v, want %v", doc.SpeakGateCooldown, DefaultSpeakGateCooldown)
	}
	// 旧文档（JSON 无三字段）读入同样回填——迁移无歧义。
	path2 := filepath.Join(t.TempDir(), "old-settings.json")
	if err := os.WriteFile(path2, []byte(`{"run_timeout_seconds":300}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sOld, err := Open(path2)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	if d := sOld.Snapshot(); d.SpeakGateThresh != DefaultSpeakGateThreshold || d.SpeakGateCooldown != DefaultSpeakGateCooldown || d.SpeakGateOff {
		t.Fatalf("旧文档回填缺省束失败：%+v", d)
	}
	if err := sOld.UpdateSpeakGate(true, 0.5, 0.4); err != nil {
		t.Fatalf("update gate: %v", err)
	}
	if d := sOld.Snapshot(); !d.SpeakGateOff || d.SpeakGateThresh != 0.5 || d.SpeakGateCooldown != 0.4 {
		t.Fatalf("UpdateSpeakGate 未生效：%+v", d)
	}
	// 值域：cooldown 0 与 threshold 越界同拒（0.05 对称下限）。
	if err := sOld.UpdateSpeakGate(false, 0.3, 0); err == nil {
		t.Fatal("cooldown=0 应拒绝（0.05 下限——零值歧义由缺省回填承担）")
	}
	if err := sOld.UpdateSpeakGate(false, 1.0, 0.2); err == nil {
		t.Fatal("threshold=1.0 应拒绝")
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
	// 权限纪律：0600（POSIX 断言——Windows 的 Stat 权限位由只读属性合成，
	// 可写文件恒显示 0666，Chmod 不改变该显示；Windows 面由数据目录 ACL 承担）
	if goruntime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("设置文件权限 = %o, want 600", fi.Mode().Perm())
		}
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
