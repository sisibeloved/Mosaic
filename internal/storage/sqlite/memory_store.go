// memory_store.go —— 分域记忆存储（0.7.0 / ADR-0037 / migration v32）。
// sqlite.Store 直接实现 memory.Store：过滤/排序（rowid 创建序降序）/游标/
// limit+1 语义与 memstore 同源（接口契约单源在 memory.Store 注释）。
// derived_from/edit_history/extractor_version 为预留列（0.7.1/0.7.5 定义
// 域类型时启用），写入固定默认、读行不进域模型——加列不动表。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/memory"
)

// 编译期接口保证（sqlite.Store is memory.Store）。
var _ memory.Store = (*Store)(nil)

const memoryCols = `memory_id, version, namespace, scope_id, kind, title, content,
 source_refs, observed_at, valid_from, valid_to, confidence, status, visibility,
 sensitivity, supersedes, edit_history, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanMemoryItem(row rowScanner) (memory.Item, error) {
	var it memory.Item
	var refs, supersedes, edits string
	if err := row.Scan(&it.MemoryID, &it.Version, &it.Namespace, &it.ScopeID,
		&it.Kind, &it.Title, &it.Content, &refs, &it.ObservedAt,
		&it.ValidFrom, &it.ValidTo, &it.Confidence, &it.Status, &it.Visibility,
		&it.Sensitivity, &supersedes, &edits, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return memory.Item{}, err
	}
	if err := json.Unmarshal([]byte(refs), &it.SourceRefs); err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory source_refs: %w", err)
	}
	if err := json.Unmarshal([]byte(supersedes), &it.Supersedes); err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory supersedes: %w", err)
	}
	if err := json.Unmarshal([]byte(edits), &it.EditHistory); err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory edit_history: %w", err)
	}
	// 一审 #1：库中已有 null 列（v32 首版写入面）读回兼容——统一空数组。
	if it.SourceRefs == nil {
		it.SourceRefs = []string{}
	}
	if it.Supersedes == nil {
		it.Supersedes = []string{}
	}
	if it.EditHistory == nil {
		it.EditHistory = []memory.EditRecord{}
	}
	return it, nil
}

// CreateItem 落库（memory_id 主键冲突 = 上游生成式碰撞，透传约束错误）。
func (s *Store) CreateItem(ctx context.Context, it memory.Item) (memory.Item, error) {
	refs, err := json.Marshal(it.SourceRefs)
	if err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory refs marshal: %w", err)
	}
	sup, err := json.Marshal(it.Supersedes)
	if err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory supersedes marshal: %w", err)
	}
	edits, err := json.Marshal(it.EditHistory)
	if err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory edit_history marshal: %w", err)
	}
	if it.EditHistory == nil {
		edits = []byte("[]")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO memory_items (`+memoryCols+`,
 derived_from, extractor_version)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'', 'manual')`,
		it.MemoryID, it.Version, string(it.Namespace), it.ScopeID, string(it.Kind),
		it.Title, it.Content, string(refs), it.ObservedAt, it.ValidFrom, it.ValidTo,
		it.Confidence, string(it.Status), it.Visibility, string(it.Sensitivity),
		string(sup), string(edits), it.CreatedAt, it.UpdatedAt)
	if err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: create memory item: %w", err)
	}
	return it, nil
}

// ItemByID 单查；未知 ok=false。
func (s *Store) ItemByID(ctx context.Context, memoryID string) (memory.Item, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+memoryCols+` FROM memory_items WHERE memory_id = ?`, memoryID)
	it, err := scanMemoryItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Item{}, false, nil
	}
	if err != nil {
		return memory.Item{}, false, fmt.Errorf("sqlite: memory item by id: %w", err)
	}
	return it, true, nil
}

// updateCols SET 片段（CAS 原位替换——edit_history 随域构造整列替换）。
const memorySetCols = `version=?, title=?, content=?, source_refs=?, observed_at=?,
 valid_from=?, valid_to=?, confidence=?, status=?, visibility=?, sensitivity=?,
 supersedes=?, edit_history=?, updated_at=?`

func marshalMemoryJSON(v any, what string) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlite: memory %s marshal: %w", what, err)
	}
	return string(raw), nil
}

func memoryUpdateArgs(it memory.Item) ([]any, error) {
	refs, err := marshalMemoryJSON(it.SourceRefs, "source_refs")
	if err != nil {
		return nil, err
	}
	sup, err := marshalMemoryJSON(it.Supersedes, "supersedes")
	if err != nil {
		return nil, err
	}
	edits, err := marshalMemoryJSON(it.EditHistory, "edit_history")
	if err != nil {
		return nil, err
	}
	return []any{it.Version, it.Title, it.Content, refs, it.ObservedAt,
		it.ValidFrom, it.ValidTo, it.Confidence, string(it.Status), it.Visibility,
		string(it.Sensitivity), sup, edits, it.UpdatedAt}, nil
}

// UpdateItem CAS 原位替换（**版本 + 状态**双基准——一审 #2；任一不符
// rows=0 → ok=false）。
func (s *Store) UpdateItem(ctx context.Context, memoryID string, expectedVersion int, expectedStatus memory.Status, next memory.Item) (memory.Item, bool, error) {
	args, err := memoryUpdateArgs(next)
	if err != nil {
		return memory.Item{}, false, err
	}
	args = append(args, memoryID, expectedVersion, string(expectedStatus))
	res, err := s.db.ExecContext(ctx,
		`UPDATE memory_items SET `+memorySetCols+
			` WHERE memory_id = ? AND version = ? AND status = ?`, args...)
	if err != nil {
		return memory.Item{}, false, fmt.Errorf("sqlite: update memory item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return memory.Item{}, false, err
	}
	return next, true, nil
}

// SupersedeItem 单事务双动作（ADR-0038 §2：旧终结 CAS + 新建 supersedes 链
// 同一事务——不存在半状态窗口）。
func (s *Store) SupersedeItem(ctx context.Context, oldID string, expectedVersion int, expectedStatus memory.Status, oldNext, replacement memory.Item) (memory.Item, memory.Item, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Item{}, memory.Item{}, false, fmt.Errorf("sqlite: supersede begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // 已提交时无害
	updArgs, err := memoryUpdateArgs(oldNext)
	if err != nil {
		return memory.Item{}, memory.Item{}, false, err
	}
	updArgs = append(updArgs, oldID, expectedVersion, string(expectedStatus))
	res, err := tx.ExecContext(ctx,
		`UPDATE memory_items SET `+memorySetCols+
			` WHERE memory_id = ? AND version = ? AND status = ?`, updArgs...)
	if err != nil {
		return memory.Item{}, memory.Item{}, false, fmt.Errorf("sqlite: supersede old: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return memory.Item{}, memory.Item{}, false, nil // CAS 失败——整体不落
	}
	if err := insertMemoryItemTx(ctx, tx, replacement); err != nil {
		return memory.Item{}, memory.Item{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return memory.Item{}, memory.Item{}, false, fmt.Errorf("sqlite: supersede commit: %w", err)
	}
	return oldNext, replacement, true, nil
}

// insertMemoryItemTx 事务连接上的插入（SupersedeItem 复用）。
func insertMemoryItemTx(ctx context.Context, tx *sql.Tx, it memory.Item) error {
	refs, err := marshalMemoryJSON(it.SourceRefs, "source_refs")
	if err != nil {
		return err
	}
	sup, err := marshalMemoryJSON(it.Supersedes, "supersedes")
	if err != nil {
		return err
	}
	edits, err := marshalMemoryJSON(it.EditHistory, "edit_history")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_items (`+memoryCols+`,
 derived_from, extractor_version)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'', 'manual')`,
		it.MemoryID, it.Version, string(it.Namespace), it.ScopeID, string(it.Kind),
		it.Title, it.Content, refs, it.ObservedAt, it.ValidFrom, it.ValidTo,
		it.Confidence, string(it.Status), it.Visibility, string(it.Sensitivity),
		sup, edits, it.CreatedAt, it.UpdatedAt)
	if err != nil {
		return fmt.Errorf("sqlite: insert memory item tx: %w", err)
	}
	return nil
}

// ListItems 过滤 +（可选）召回匹配 + 排序 + 游标 + 计数（0.7.2 / ADR-0039）。
// 召回模式（q 非空）：授权/时效过滤与 FTS MATCH/LIKE **同一 WHERE**（过滤
// 先于排序与计数——他域命中不出现在结果与 total）；bm25 升序 + rowid DESC
// 兜底；无游标（limit 截断，total 为全量计数）。非召回模式：rowid 降序 +
// before 游标 + limit+1 条。before 未知 = 空页——与 memstore 同口径。
func (s *Store) ListItems(ctx context.Context, f memory.ListFilter) ([]memory.Item, int, error) {
	q := `SELECT ` + memoryCols + ` FROM memory_items WHERE namespace = ? AND status = ?`
	args := []any{string(f.Namespace), string(f.Status)}
	if f.ScopeID != "" {
		q += ` AND scope_id = ?`
		args = append(args, f.ScopeID)
	}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(f.Kind))
	}
	// 0.7.1 时效口径（ADR-0038 §3）：active 且未显式包含时排除 valid_to <
	// now——RFC3339 **时刻**比较（0.6.13 审校 #15 教训：字典序在可变小数
	// 位下漏判），经 datetime() 归一比较。
	if f.Status == memory.StatusActive && !f.IncludeExpired {
		// 一审 #4：写入侧已统一 UTC（julianday 全认）；旧数据的合法偏移格式
		//（如 +15:00）julianday 返回 NULL——**保守包含**不误隐（宁可多显
		// 不漏显——0.7.2 召回前置过滤再做精确语义）。
		q += ` AND (valid_to = '' OR julianday(valid_to) IS NULL OR julianday(valid_to) >= julianday(?))`
		args = append(args, f.Now)
	}
	if f.Before != "" {
		q += ` AND rowid < COALESCE((SELECT rowid FROM memory_items WHERE memory_id = ? AND namespace = ?), 0)`
		args = append(args, f.Before, string(f.Namespace))
	}
	// 0.7.2 召回匹配（过滤先行——WHERE 前段已是授权/时效过滤）。
	if f.Query != "" {
		if len([]rune(f.Query)) >= 3 {
			// trigram ≥3 rune：FTS MATCH（phrase 引号防语法注入——room 先例）。
			phrase := `"` + strings.ReplaceAll(f.Query, `"`, `""`) + `"`
			q += ` AND memory_id IN (SELECT memory_id FROM memory_fts WHERE memory_fts MATCH ?)`
			args = append(args, phrase)
		} else {
			q += ` AND (title LIKE ? COLLATE NOCASE OR content LIKE ? COLLATE NOCASE)`
			args = append(args, "%"+f.Query+"%", "%"+f.Query+"%")
		}
	}
	// total：过滤+匹配后全量计数（不含分页截断——"先过滤再计数"）。
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_items WHERE `+strings.TrimPrefix(q, `SELECT `+memoryCols+` FROM memory_items WHERE `), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("sqlite: count memory items: %w", err)
	}
	if f.Query != "" {
		// 召回模式：bm25 相关性（升序——越小越相关）+ rowid DESC 兜底；无游标。
		order := ` ORDER BY (SELECT bm25(memory_fts) FROM memory_fts WHERE memory_fts.memory_id = memory_items.memory_id) ASC, rowid DESC LIMIT ?`
		args = append(args, f.Limit)
		return s.queryMemoryItems(ctx, q+order, args, total)
	}
	q += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, f.Limit+1)
	return s.queryMemoryItems(ctx, q, args, total)
}

// queryMemoryItems 执行 + 扫描（召回/非召回共用）。
func (s *Store) queryMemoryItems(ctx context.Context, q string, args []any, total int) ([]memory.Item, int, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: list memory items: %w", err)
	}
	defer rows.Close()
	out := make([]memory.Item, 0, 16)
	for rows.Next() {
		it, err := scanMemoryItem(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("sqlite: list memory items scan: %w", err)
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}
