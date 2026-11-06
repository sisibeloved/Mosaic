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
	"sort"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/memory"
)

// 编译期接口保证（sqlite.Store is memory.Store）。
var _ memory.Store = (*Store)(nil)

const memoryCols = `memory_id, version, namespace, scope_id, kind, title, content,
 source_refs, observed_at, valid_from, valid_to, confidence, status, visibility,
 sensitivity, supersedes, edit_history, created_at, updated_at`

// memoryColsM 带 m. 前缀的列串（召回 MATCH JOIN 路径——两表 title/content
// 列名冲突，全列显式前缀）。
const memoryColsM = `m.memory_id, m.version, m.namespace, m.scope_id, m.kind, m.title, m.content,
 m.source_refs, m.observed_at, m.valid_from, m.valid_to, m.confidence, m.status, m.visibility,
 m.sensitivity, m.supersedes, m.edit_history, m.created_at, m.updated_at`

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

// memoryCommonWhere 公共过滤条件（授权 + 状态 + 时效——三条路径共用：
// 非召回列表、召回 MATCH、召回 LIKE；prefix 为表别名前缀）。返回条件片段
// 与参数——**不含 before/limit**（分页只在取页时应用；计数用公共条件）。
func memoryCommonWhere(f memory.ListFilter, prefix string) (string, []any) {
	where := prefix + `namespace = ? AND ` + prefix + `status = ?`
	args := []any{string(f.Namespace), string(f.Status)}
	if f.ScopeID != "" {
		where += ` AND ` + prefix + `scope_id = ?`
		args = append(args, f.ScopeID)
	}
	if f.Kind != "" {
		where += ` AND ` + prefix + `kind = ?`
		args = append(args, string(f.Kind))
	}
	// 0.7.1 时效口径（ADR-0038 §3）：active 且未显式包含时排除 valid_to <
	// now（UTC 写入 + NULL 保守包含——0.7.1 一审 #4）。
	if f.Status == memory.StatusActive && !f.IncludeExpired {
		where += ` AND (` + prefix + `valid_to = '' OR julianday(` + prefix + `valid_to) IS NULL OR julianday(` + prefix + `valid_to) >= julianday(?))`
		args = append(args, f.Now)
	}
	return where, args
}

// likePattern 字面 LIKE 模式（0.7.2 一审 #2：%/_/\ 是 LIKE 通配符/转义符——
// 搜索字面量须转义并显式 ESCAPE '\'；语义与 memstore strings.Contains 一致）。
func likePattern(query string) string {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + esc.Replace(query) + "%"
}

// ListItems 过滤 +（可选）召回匹配 + 排序 + 游标 + 计数（0.7.2 / ADR-0039）。
// 三路径：非召回（rowid 降序 + before 游标 + limit+1）；召回 MATCH ≥3 rune
// （**一次 MATCH 驱动** JOIN memory_items 过滤——bm25 在 MATCH 行上评分，
// 一审 #1/#4：逐候选 bm25 子查询既全表扫描又脱离 MATCH 上下文评分恒零）；
// 召回 LIKE <3 rune（独立路径 + ESCAPE 字面匹配）。total = 公共过滤+匹配
// 后全量计数（**不含 before**——一审 #3：每页总数恒定）。
func (s *Store) ListItems(ctx context.Context, f memory.ListFilter) ([]memory.Item, int, error) {
	if f.Query != "" && len([]rune(f.Query)) >= 3 {
		return s.listRecallFTS(ctx, f)
	}
	if f.Query != "" {
		return s.listRecallLike(ctx, f)
	}
	where, args := memoryCommonWhere(f, "")
	// 一审 #3：计数先于分页段——公共过滤条件的全量计数（不含 before）。
	total, err := s.countMemory(ctx, `FROM memory_items WHERE `+where, args)
	if err != nil {
		return nil, 0, err
	}
	if f.Before != "" {
		where += ` AND rowid < COALESCE((SELECT rowid FROM memory_items WHERE memory_id = ? AND namespace = ?), 0)`
		args = append(args, f.Before, string(f.Namespace))
	}
	args = append(args, f.Limit+1)
	return s.queryMemoryItems(ctx,
		`SELECT `+memoryCols+` FROM memory_items WHERE `+where+` ORDER BY rowid DESC LIMIT ?`, args, total)
}

// memoryCommonArgs 公共过滤参数（计数复用——与取页同条件但无分页参数）。
func memoryCommonArgs(f memory.ListFilter) []any {
	_, args := memoryCommonWhere(f, "")
	return args
}

func (s *Store) countMemory(ctx context.Context, fromWhere string, args []any) (int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) `+fromWhere, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("sqlite: count memory items: %w", err)
	}
	return total, nil
}

// listRecallFTS 召回 ≥3 rune：FTS 索引一次 MATCH 驱动 → JOIN 过滤 → bm25
// listRecallFTS 召回 ≥3 rune：MATCH 索引驱动 JOIN 过滤取**全量候选行**，
// Go 侧按**语料内 BM25** 精排（二审 #2：FTS5 bm25() 的统计是全索引级——
// 过滤外条目改变 avgdl 会翻转本域排名，违反"过滤先于评分"；自算评分的
// dl/avgdl 限定于过滤后语料，排序键对语料外增删不敏感）。输入序 rowid DESC
// + 稳定排序保持同名分兜底。
func (s *Store) listRecallFTS(ctx context.Context, f memory.ListFilter) ([]memory.Item, int, error) {
	phrase := `"` + strings.ReplaceAll(f.Query, `"`, `""`) + `"` // phrase 防语法注入
	common, cargs := memoryCommonWhere(f, "m.")
	where := `f.memory_fts MATCH ? AND ` + common
	// MATCH 参数在最前（FTS 驱动表），过滤参数随后。
	args := append([]any{phrase}, cargs...)
	total, err := s.countMemory(ctx,
		`FROM memory_fts f JOIN memory_items m ON m.memory_id = f.memory_id WHERE `+where, args)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []memory.Item{}, 0, nil
	}
	cands, _, err := s.queryMemoryItems(ctx,
		`SELECT `+memoryColsM+`
FROM memory_fts f JOIN memory_items m ON m.memory_id = f.memory_id
WHERE `+where+` ORDER BY m.rowid DESC`, args, total)
	if err != nil {
		return nil, 0, err
	}
	scored := corpusBM25(cands, f.Query)
	if len(scored) > f.Limit {
		scored = scored[:f.Limit]
	}
	return scored, total, nil
}

// corpusBM25 语料内 BM25 精排（k1=1.2 / b=0.75——SQLite FTS5 同参；dl/avgdl
// 与 tf 均取过滤后候选集；同查询的 IDF 项对全体候选相同，排序可略）。
// 返回相关度降序；输入已是 rowid 降序，sort.SliceStable 保同名分兜底。
func corpusBM25(cands []memory.Item, query string) []memory.Item {
	const k1, b = 1.2, 0.75
	q := strings.ToLower(query)
	type scoredItem struct {
		it    memory.Item
		score float64
	}
	list := make([]scoredItem, 0, len(cands))
	sumLen := 0
	for _, it := range cands {
		list = append(list, scoredItem{it: it})
		sumLen += len([]rune(it.Title)) + len([]rune(it.Content))
	}
	avgdl := float64(sumLen) / float64(len(cands))
	if avgdl <= 0 {
		avgdl = 1
	}
	for i := range list {
		it := list[i].it
		tf := float64(strings.Count(strings.ToLower(it.Title), q) +
			strings.Count(strings.ToLower(it.Content), q))
		if tf == 0 {
			continue // FTS 命中但分词粒度差异兜底——零频不加分
		}
		dl := float64(len([]rune(it.Title)) + len([]rune(it.Content)))
		list[i].score = tf * (k1 + 1) / (tf + k1*(1-b+b*dl/avgdl))
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].score > list[j].score })
	out := make([]memory.Item, 0, len(list))
	for _, s := range list {
		out = append(out, s.it)
	}
	return out
}

// listRecallLike 召回 <3 rune：独立 LIKE 路径（trigram 不可用区间）——
// ESCAPE 字面匹配（一审 #2）+ 创建序（LIKE 无相关性评分）。
func (s *Store) listRecallLike(ctx context.Context, f memory.ListFilter) ([]memory.Item, int, error) {
	common, cargs := memoryCommonWhere(f, "")
	pat := likePattern(f.Query)
	// 一审 #2：ESCAPE 声明转义符（SQL 字面单反斜杠）——pattern 内 \%/ \_ 为字面量。
	where := `(title LIKE ? COLLATE NOCASE ESCAPE '\' OR content LIKE ? COLLATE NOCASE ESCAPE '\') AND ` + common
	args := append([]any{pat, pat}, cargs...)
	total, err := s.countMemory(ctx, `FROM memory_items WHERE `+where, args)
	if err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit)
	return s.queryMemoryItems(ctx,
		`SELECT `+memoryCols+` FROM memory_items WHERE `+where+` ORDER BY rowid DESC LIMIT ?`, args, total)
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
