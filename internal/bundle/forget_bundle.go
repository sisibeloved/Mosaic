// forget_bundle.go —— 0.7.8 / ADR-0045：包生命周期删除 + 托管包扫描 +
// 墓碑防复活（预检 finding 与激活阻断）。
//
// 语义与边界（RFC §11.4 / AT-06）：
//   - 导出包删除：数据目录 bundles/<id>/ 整目录清除（zip+边车）；已传输/
//     已在他实例激活的副本无法召回——视图如实注记（不宣称全域擦除）；
//   - 导入记录删除：inactive 可删（zip/边车/attempt）；activated → 409
//     （激活产物是独立域状态——撤销激活不在本件，显式拒绝非级联猜测）；
//   - ExportsContaining：托管包内含指定记忆的清单（Forget 影响报告输入
//     ——zip 解包 items.ndjson 逐行精确比对；个人实例包量小）；
//   - 防复活两把键（包内原 mem_* ID / 激活 origin 标记 bundle:<bdl>:<mem>）
//     × 两道门（预检 tombstone_hits warn / 激活跳过不复活）。
package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// ExportDeletionView 导出包删除结果（不可达注记——RFC"不能宣称全域擦除"）。
type ExportDeletionView struct {
	BundleID     string   `json:"bundle_id"`
	DeletedAt    string   `json:"deleted_at"`
	DeletedFiles []string `json:"deleted_files"`
	OutOfReach   []string `json:"out_of_reach"`
}

// ImportDeletionView 导入记录删除结果。
type ImportDeletionView struct {
	ImportID     string   `json:"import_id"`
	DeletedAt    string   `json:"deleted_at"`
	DeletedFiles []string `json:"deleted_files"`
	OutOfReach   []string `json:"out_of_reach"`
}

// 导出删除不可达注记（固定事实面）。
var exportOutOfReach = []string{
	"已传输或已在他实例激活的副本无法召回（包已离开本实例）",
	"已激活导入记录的产物是独立域状态——本删除不级联（activated 记录另由导入删除门治理）",
}

// DeleteExport 删除导出包（ADR-0045 §4）：bundle_id 白名单 → 目录整体清除。
func (s *Service) DeleteExport(_ context.Context, bundleID string) (ExportDeletionView, error) {
	if !bundleIDRe.MatchString(bundleID) {
		return ExportDeletionView{}, fmt.Errorf("%w: bundle_id 不合法", ErrInvalid)
	}
	dir := filepath.Join(s.dir, "bundles", bundleID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ExportDeletionView{}, ErrNotFound
		}
		return ExportDeletionView{}, err
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		files = append(files, "bundles/"+bundleID+"/"+e.Name())
	}
	if err := os.RemoveAll(dir); err != nil {
		return ExportDeletionView{}, fmt.Errorf("bundle: 删除导出包: %w", err)
	}
	return ExportDeletionView{
		BundleID: bundleID, DeletedAt: s.now(),
		DeletedFiles: files, OutOfReach: append([]string{}, exportOutOfReach...),
	}, nil
}

// ExportsContaining 含指定记忆（任一身份键）的托管导出包清单（复审 #3：
// keys = 本地墓碑 ID + 来源标记尾段——**源 zip 内存的是源实例 ID**，只按
// 本地新 ID 扫描会漏报；Forget 影响报告输入）。
func (s *Service) ExportsContaining(_ context.Context, keys []string) ([]string, error) {
	return scanContaining(filepath.Join(s.dir, "bundles"), bundleIDRe, keys)
}

// scanContaining 目录下各记录 zip 是否含任一身份键（items.ndjson 逐行
// 精确比对——坏包跳过：扫描是报告面不阻断删除）。
func scanContaining(root string, idRe *regexp.Regexp, keys []string) ([]string, error) {
	set := map[string]bool{}
	for _, k := range keys {
		if k != "" {
			set[k] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("%w: 身份键为空", ErrInvalid)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !idRe.MatchString(e.Name()) {
			continue
		}
		hit, err := zipContainsAnyMemory(filepath.Join(root, e.Name(), zipName), set)
		if err != nil {
			continue
		}
		if hit {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// zipContainsAnyMemory 包内 items.ndjson 是否含键集中任一 memory_id。
func zipContainsAnyMemory(zipPath string, set map[string]bool) (bool, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return false, err
	}
	defer zr.Close()
	var f *zip.File
	for _, e := range zr.File {
		if e.Name == "memory/items.ndjson" {
			f = e
			break
		}
	}
	if f == nil {
		return false, nil
	}
	rc, err := f.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	raw := &bytes.Buffer{}
	if _, err := raw.ReadFrom(rc); err != nil {
		return false, err
	}
	for _, line := range bytes.Split(raw.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe struct {
			MemoryID string `json:"memory_id"`
		}
		if json.Unmarshal(line, &probe) == nil && set[probe.MemoryID] {
			return true, nil
		}
	}
	return false, nil
}

// ImportsContaining 含指定记忆（任一身份键）的导入记录清单（审校 #4 +
// 复审 #3：源 zip 内存源实例 ID——按墓碑本地 ID 与来源标记尾段多键扫描）。
func (s *ImportService) ImportsContaining(_ context.Context, keys []string) ([]string, error) {
	return scanContaining(filepath.Join(s.dir, importDirName), importIDRe, keys)
}

// Delete 删除导入记录：inactive → 清除（zip/边车/attempt）；activated →
// ErrConflict（激活产物是独立域状态——先治理产物，撤销激活不在本件）。
func (s *ImportService) Delete(importID string) (ImportDeletionView, error) {
	if !importIDRe.MatchString(importID) {
		return ImportDeletionView{}, fmt.Errorf("%w: import_id 不合法", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	view, ok, err := s.readSidecar(importID)
	if err != nil {
		return ImportDeletionView{}, err
	}
	if !ok {
		return ImportDeletionView{}, ErrNotFound
	}
	if view.Status == "activated" {
		return ImportDeletionView{}, fmt.Errorf("%w: 导入已激活（%s）——激活产物是独立域状态，须先治理产物（撤销激活另行交付）", ErrConflict, importID)
	}
	dir := filepath.Join(s.dir, importDirName, importID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ImportDeletionView{}, err
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		files = append(files, importDirName+"/"+importID+"/"+e.Name())
	}
	if err := os.RemoveAll(dir); err != nil {
		return ImportDeletionView{}, fmt.Errorf("bundle: 删除导入记录: %w", err)
	}
	return ImportDeletionView{
		ImportID: importID, DeletedAt: s.now(),
		DeletedFiles: files, OutOfReach: []string{"激活产物（若有）是独立域状态——本删除不级联"},
	}, nil
}
