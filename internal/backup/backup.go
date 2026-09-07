// Package backup 数据备份/恢复（M4-0：备份恢复一键化 + 自诊断报告的数据面）。
//
// 语义与边界：
//   - 备份 = VACUUM INTO 一致快照（SQLite 官方路径：不需要停写、不搬 WAL/SHM、
//     产物是自包含单文件）+ manifest（逐文件 sha256/大小，创建时间）；
//   - 恢复分两段：在线段只做"校验 + 落 pending-restore 标记"（活动进程握着
//     mosaic.db 的打开句柄，Windows 上被握文件不可替换——在线换库结构性不可行）；
//     启动段（ApplyPendingRestore，在 sqlite.Open 之前调用）执行实际换库，
//     被换下的库文件移入 restore-safety/（时间戳子目录，回滚保护）；
//   - 校验失败 fail safe：启动段发现备份损坏时不换库，标记改名 .failed 留痕，
//     以当前数据继续启动。
//
// 安全：备份目录与安全副本目录都在数据目录内（数据面 owner-only 的既有边界）；
// backup_id 形如 bkp_<ts36>_<hex>（本包生成），外部传入的 id 先过字符白名单
// 再拼路径（防穿越）。
package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// DB 备份源端口（*storage/sqlite.Store 实现：VACUUM INTO）。
type DB interface {
	BackupTo(ctx context.Context, destPath string) error
}

// FileEntry manifest 内的文件条目。
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest 备份清单。
type Manifest struct {
	Format    string      `json:"format"` // "mosaic-backup/1"
	BackupID  string      `json:"backup_id"`
	CreatedAt string      `json:"created_at"`
	Files     []FileEntry `json:"files"`
}

// Summary 备份摘要（列表/创建响应）。
type Summary struct {
	BackupID  string `json:"backup_id"`
	CreatedAt string `json:"created_at"`
	SizeBytes int64  `json:"size_bytes"`
}

// PendingRestore 恢复请求标记（在线段落盘，启动段消费）。
type PendingRestore struct {
	BackupID    string `json:"backup_id"`
	RequestedAt string `json:"requested_at"`
}

// AppliedRestore 启动段换库结果（装配层记日志用）。
type AppliedRestore struct {
	BackupID  string `json:"backup_id"`
	SafetyDir string `json:"safety_dir"`
}

// Restored 备份恢复后被换下的库文件去向（回滚保护）。
const (
	manifestName      = "manifest.json"
	dbFileName        = "mosaic.db"
	pendingName       = "pending-restore.json"
	pendingFailedName = "pending-restore.failed.json"
	safetyRoot        = "restore-safety"
	formatVersion     = "mosaic-backup/1"
)

var backupIDPattern = regexp.MustCompile(`^bkp_[0-9a-z_]+$`)

// Manager 备份管理器（Dir = 数据目录；备份落 Dir/backups/<id>/）。
type Manager struct {
	DB  DB
	Dir string // 数据目录
	Now func() time.Time
}

func (m *Manager) backupsDir() string { return filepath.Join(m.Dir, "backups") }
func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Create 立即备份：VACUUM INTO 到新目录 + manifest。
func (m *Manager) Create(ctx context.Context) (Summary, error) {
	id := newBackupID(m.now())
	dir := filepath.Join(m.backupsDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Summary{}, fmt.Errorf("backup: mkdir: %w", err)
	}
	dbDest := filepath.Join(dir, dbFileName)
	if err := m.DB.BackupTo(ctx, dbDest); err != nil {
		_ = os.RemoveAll(dir)
		return Summary{}, fmt.Errorf("backup: vacuum into: %w", err)
	}
	entry, err := hashFile(dir, dbFileName)
	if err != nil {
		_ = os.RemoveAll(dir)
		return Summary{}, fmt.Errorf("backup: hash: %w", err)
	}
	man := Manifest{Format: formatVersion, BackupID: id, CreatedAt: m.now().UTC().Format(time.RFC3339), Files: []FileEntry{entry}}
	if err := writeJSON(filepath.Join(dir, manifestName), man, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return Summary{}, fmt.Errorf("backup: manifest: %w", err)
	}
	return Summary{BackupID: id, CreatedAt: man.CreatedAt, SizeBytes: entry.Size}, nil
}

// List 全部备份（新→旧；损坏 manifest 的目录跳过——列表不因单点损坏失败）。
func (m *Manager) List() ([]Summary, error) {
	entries, err := os.ReadDir(m.backupsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Summary{}, nil
		}
		return nil, fmt.Errorf("backup: list: %w", err)
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !backupIDPattern.MatchString(e.Name()) {
			continue
		}
		man, err := readManifest(filepath.Join(m.backupsDir(), e.Name()))
		if err != nil {
			continue
		}
		var total int64
		for _, f := range man.Files {
			total += f.Size
		}
		out = append(out, Summary{BackupID: man.BackupID, CreatedAt: man.CreatedAt, SizeBytes: total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// ErrNotFound 指定备份不存在。
var ErrNotFound = errors.New("backup: not found")

// Verify 校验备份目录（manifest 可读 + 逐文件 sha256 一致）。
func (m *Manager) Verify(backupID string) error {
	dir, err := m.dirOf(backupID)
	if err != nil {
		return err
	}
	man, err := readManifest(dir)
	if err != nil {
		return err
	}
	for _, f := range man.Files {
		got, err := hashFile(dir, f.Name)
		if err != nil {
			return fmt.Errorf("backup: %s: %w", f.Name, err)
		}
		if got.SHA256 != f.SHA256 || got.Size != f.Size {
			return fmt.Errorf("backup: %s 校验不符（manifest %d/%s，实际 %d/%s）", f.Name, f.Size, f.SHA256, got.Size, got.SHA256)
		}
	}
	return nil
}

// RequestRestore 在线段：校验备份 + 落 pending 标记（实际换库在下次启动）。
func (m *Manager) RequestRestore(backupID string) error {
	if err := m.Verify(backupID); err != nil {
		return err
	}
	p := PendingRestore{BackupID: backupID, RequestedAt: m.now().UTC().Format(time.RFC3339)}
	if err := writeJSON(filepath.Join(m.Dir, pendingName), p, 0o600); err != nil {
		return fmt.Errorf("backup: pending marker: %w", err)
	}
	return nil
}

// ApplyPendingRestore 启动段：消费 pending 标记，在开库前换库。
// 无标记返回 (nil, nil)；备份损坏时不换库（标记转 .failed 留痕）并返回错误。
// 调用方对错误的正确处置是"带当前数据继续启动 + 记错误日志"。
func ApplyPendingRestore(dataDir string) (*AppliedRestore, error) {
	pendingPath := filepath.Join(dataDir, pendingName)
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("backup: read pending: %w", err)
	}
	var p PendingRestore
	if err := json.Unmarshal(raw, &p); err != nil {
		// 标记本身损坏：换库不可信，转 .failed 保数据
		_ = os.Rename(pendingPath, filepath.Join(dataDir, pendingFailedName))
		return nil, fmt.Errorf("backup: pending marker 损坏（不换库）: %w", err)
	}
	m := &Manager{Dir: dataDir}
	if err := m.Verify(p.BackupID); err != nil {
		_ = os.Rename(pendingPath, filepath.Join(dataDir, pendingFailedName))
		return nil, fmt.Errorf("backup: 待恢复备份校验失败（不换库，标记转 .failed）: %w", err)
	}
	// 安全副本：被换下的库文件（含 -wal/-shm 若在）移入 restore-safety/<ts>/
	ts := strconv.FormatInt(time.Now().UnixNano(), 36)
	safetyDir := filepath.Join(dataDir, safetyRoot, ts)
	if err := os.MkdirAll(safetyDir, 0o700); err != nil {
		return nil, fmt.Errorf("backup: safety dir: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := filepath.Join(dataDir, dbFileName+suffix)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, filepath.Join(safetyDir, dbFileName+suffix)); err != nil {
				return nil, fmt.Errorf("backup: safety move %s: %w", dbFileName+suffix, err)
			}
		}
	}
	src := filepath.Join(m.backupsDir(), p.BackupID, dbFileName)
	if err := copyFile(src, filepath.Join(dataDir, dbFileName), 0o600); err != nil {
		return nil, fmt.Errorf("backup: apply: %w（原库在 %s）", err, safetyDir)
	}
	if err := os.Remove(pendingPath); err != nil {
		return nil, fmt.Errorf("backup: clear pending: %w", err)
	}
	return &AppliedRestore{BackupID: p.BackupID, SafetyDir: safetyDir}, nil
}

// dirOf id → 备份目录（白名单先行，防路径穿越）。
func (m *Manager) dirOf(backupID string) (string, error) {
	if !backupIDPattern.MatchString(backupID) {
		return "", fmt.Errorf("%w: %q", ErrNotFound, backupID)
	}
	dir := filepath.Join(m.backupsDir(), backupID)
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, backupID)
	}
	return dir, nil
}

func readManifest(dir string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return Manifest{}, err
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return Manifest{}, err
	}
	if man.Format != formatVersion {
		return Manifest{}, fmt.Errorf("未知备份格式 %q", man.Format)
	}
	return man, nil
}

func hashFile(dir, name string) (FileEntry, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return FileEntry{}, err
	}
	sum := sha256.Sum256(raw)
	return FileEntry{Name: name, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func writeJSON(path string, v any, perm os.FileMode) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), perm)
}

func copyFile(src, dst string, perm os.FileMode) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, perm)
}

func newBackupID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "bkp_" + strconv.FormatInt(now.UnixNano(), 36) + "_" + hex.EncodeToString(b[:])
}

// PendingExists 报告数据目录是否有待恢复标记（装配层启动横幅/诊断面用）。
func PendingExists(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, pendingName))
	return err == nil
}
