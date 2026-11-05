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

	"github.com/sisibeloved/Mosaic/internal/memory"
)

// 编译期接口保证（sqlite.Store is memory.Store）。
var _ memory.Store = (*Store)(nil)

const memoryCols = `memory_id, version, namespace, scope_id, kind, title, content,
 source_refs, observed_at, valid_from, valid_to, confidence, status, visibility,
 sensitivity, supersedes, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanMemoryItem(row rowScanner) (memory.Item, error) {
	var it memory.Item
	var refs, supersedes string
	if err := row.Scan(&it.MemoryID, &it.Version, &it.Namespace, &it.ScopeID,
		&it.Kind, &it.Title, &it.Content, &refs, &it.ObservedAt,
		&it.ValidFrom, &it.ValidTo, &it.Confidence, &it.Status, &it.Visibility,
		&it.Sensitivity, &supersedes, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return memory.Item{}, err
	}
	if err := json.Unmarshal([]byte(refs), &it.SourceRefs); err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory source_refs: %w", err)
	}
	if err := json.Unmarshal([]byte(supersedes), &it.Supersedes); err != nil {
		return memory.Item{}, fmt.Errorf("sqlite: memory supersedes: %w", err)
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO memory_items (`+memoryCols+`,
 derived_from, edit_history, extractor_version)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'', '[]', 'manual')`,
		it.MemoryID, it.Version, string(it.Namespace), it.ScopeID, string(it.Kind),
		it.Title, it.Content, string(refs), it.ObservedAt, it.ValidFrom, it.ValidTo,
		it.Confidence, string(it.Status), it.Visibility, string(it.Sensitivity),
		string(sup), it.CreatedAt, it.UpdatedAt)
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

// ListItems 过滤 + rowid 创建序降序 + 游标（before 行之前的更早条目）+
// limit+1 条（第 limit+1 条供 service 判 next_cursor）。before 未知
// （已删除/游标失效）= 空页——与 memstore 同口径。
func (s *Store) ListItems(ctx context.Context, f memory.ListFilter) ([]memory.Item, error) {
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
	if f.Before != "" {
		q += ` AND rowid < COALESCE((SELECT rowid FROM memory_items WHERE memory_id = ?), 0)`
		args = append(args, f.Before)
	}
	q += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, f.Limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list memory items: %w", err)
	}
	defer rows.Close()
	out := make([]memory.Item, 0, f.Limit+1)
	for rows.Next() {
		it, err := scanMemoryItem(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list memory items scan: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
