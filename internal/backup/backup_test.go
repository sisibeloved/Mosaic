// UT 层：备份面——创建/校验/列表、两段式恢复（标记→启动换库）、损坏 fail safe、
// 路径穿越防御。DB 用 fake（backup 包不依赖具体存储实现）。
package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDB BackupTo 的假实现：往 destPath 写固定内容（模拟 VACUUM INTO 产物）。
type fakeDB struct{ content []byte }

func (f *fakeDB) BackupTo(_ context.Context, destPath string) error {
	return os.WriteFile(destPath, f.content, 0o600)
}

func newManager(t *testing.T, content []byte) *Manager {
	t.Helper()
	return &Manager{DB: &fakeDB{content: content}, Dir: t.TempDir()}
}

func writeCurrentDB(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, dbFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write current db: %v", err)
	}
}

func readCurrentDB(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatalf("read current db: %v", err)
	}
	return string(raw)
}

func TestCreateVerifyList(t *testing.T) {
	m := newManager(t, []byte("v1-data"))
	sum, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !backupIDPattern.MatchString(sum.BackupID) || sum.SizeBytes != int64(len("v1-data")) {
		t.Fatalf("summary 异常：%+v", sum)
	}
	if err := m.Verify(sum.BackupID); err != nil {
		t.Fatalf("create 后校验应通过: %v", err)
	}
	list, err := m.List()
	if err != nil || len(list) != 1 || list[0].BackupID != sum.BackupID {
		t.Fatalf("list 异常: %v %v", list, err)
	}
	// 篡改备份文件 → 校验必失败
	target := filepath.Join(m.backupsDir(), sum.BackupID, dbFileName)
	if err := os.WriteFile(target, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := m.Verify(sum.BackupID); err == nil {
		t.Fatal("篡改后校验应失败")
	}
}

func TestListNewestFirstAndSkipsCorrupt(t *testing.T) {
	m := &Manager{DB: &fakeDB{content: []byte("x")}, Dir: t.TempDir(), Now: func() time.Time {
		return time.Unix(1000, 0)
	}}
	first, _ := m.Create(context.Background())
	m.Now = func() time.Time { return time.Unix(2000, 0) }
	second, _ := m.Create(context.Background())
	// 损坏目录（无 manifest）：列表跳过不失败
	_ = os.MkdirAll(filepath.Join(m.backupsDir(), "bkp_broken"), 0o700)
	list, err := m.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].BackupID != second.BackupID || list[1].BackupID != first.BackupID {
		t.Fatalf("列表应为新→旧且跳过损坏目录: %+v", list)
	}
}

func TestRequestRestoreAndApply(t *testing.T) {
	m := newManager(t, []byte("backup-content"))
	sum, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	writeCurrentDB(t, m.Dir, "newer-current")

	if err := m.RequestRestore(sum.BackupID); err != nil {
		t.Fatalf("request: %v", err)
	}
	if !PendingExists(m.Dir) {
		t.Fatal("pending 标记应存在")
	}
	applied, err := ApplyPendingRestore(m.Dir)
	if err != nil || applied == nil {
		t.Fatalf("apply: %v %+v", err, applied)
	}
	if got := readCurrentDB(t, m.Dir); got != "backup-content" {
		t.Fatalf("换库后内容应为备份内容: %q", got)
	}
	if PendingExists(m.Dir) {
		t.Fatal("apply 后标记应清除")
	}
	// 回滚保护：被换下的库在安全副本目录
	safetyDB := filepath.Join(applied.SafetyDir, dbFileName)
	if raw, err := os.ReadFile(safetyDB); err != nil || string(raw) != "newer-current" {
		t.Fatalf("安全副本应保留原库: %v %q", err, raw)
	}
}

func TestApplyCorruptBackupFailsSafe(t *testing.T) {
	m := newManager(t, []byte("good"))
	sum, _ := m.Create(context.Background())
	writeCurrentDB(t, m.Dir, "current")
	if err := m.RequestRestore(sum.BackupID); err != nil {
		t.Fatalf("request: %v", err)
	}
	// 标记落盘后备份被损坏
	if err := os.WriteFile(filepath.Join(m.backupsDir(), sum.BackupID, dbFileName), []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	applied, err := ApplyPendingRestore(m.Dir)
	if err == nil || applied != nil {
		t.Fatalf("损坏备份应拒绝换库: %+v %v", applied, err)
	}
	if got := readCurrentDB(t, m.Dir); got != "current" {
		t.Fatalf("当前数据不得被破坏: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, pendingFailedName)); err != nil {
		t.Fatal("标记应转 .failed 留痕")
	}
}

func TestApplyNoMarkerIsNoop(t *testing.T) {
	dir := t.TempDir()
	applied, err := ApplyPendingRestore(dir)
	if err != nil || applied != nil {
		t.Fatalf("无标记应为 no-op: %+v %v", applied, err)
	}
}

func TestDirOfRejectsTraversal(t *testing.T) {
	m := newManager(t, []byte("x"))
	for _, id := range []string{"../evil", "..\\evil", "bkp_", "bkp_UPCASE", "bkp_a/b"} {
		if _, err := m.dirOf(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("id %q 应拒绝（ErrNotFound）", id)
		}
	}
}

func TestRequestRestoreUnknownID(t *testing.T) {
	m := newManager(t, []byte("x"))
	if err := m.RequestRestore("bkp_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知备份应 ErrNotFound: %v", err)
	}
	if PendingExists(m.Dir) {
		t.Fatal("未知备份不得落标记")
	}
}

func TestManifestFormatGuard(t *testing.T) {
	m := newManager(t, []byte("x"))
	sum, _ := m.Create(context.Background())
	mpath := filepath.Join(m.backupsDir(), sum.BackupID, manifestName)
	if err := os.WriteFile(mpath, []byte(`{"format":"weird","backup_id":"`+sum.BackupID+`","files":[]}`), 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	if err := m.Verify(sum.BackupID); err == nil || !strings.Contains(err.Error(), "格式") {
		t.Fatalf("未知格式应拒绝: %v", err)
	}
}
