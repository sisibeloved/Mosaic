// Package sqlite 是 Room Event Log 的 SQLite（WAL）存储层（ADR-0008 个人版形态）。
// 机制映射：房间串行写 → BEGIN IMMEDIATE 互斥事务；订阅续传 → 全局位（global_pos）
// 构造的 opaque cursor；outbox SKIP LOCKED → 进程内提交后分发 + 崩溃后按表重放。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"

	_ "modernc.org/sqlite"
)

// ErrDuplicateEvent 表示 event_id 已存在（幂等追加语义：上游应视作已成功）。
var ErrDuplicateEvent = errors.New("sqlite: duplicate event_id")

const schemaDDL = `
CREATE TABLE IF NOT EXISTS room_events (
	global_pos    INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id      TEXT NOT NULL UNIQUE,
	tenant_id     TEXT NOT NULL,
	room_id       TEXT NOT NULL,
	seq           INTEGER NOT NULL,
	type          TEXT NOT NULL,
	schema_version INTEGER NOT NULL,
	occurred_at   TEXT NOT NULL,
	envelope      TEXT NOT NULL,
	UNIQUE (room_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_room_events_room_pos ON room_events(room_id, global_pos);

CREATE TABLE IF NOT EXISTS outbox (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id      TEXT NOT NULL,
	global_pos   INTEGER NOT NULL UNIQUE,
	event_id     TEXT NOT NULL UNIQUE,
	envelope     TEXT NOT NULL,
	dispatched_at TEXT,
	created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(id) WHERE dispatched_at IS NULL;
`

// Store 持有单个 SQLite 文件的连接池。个人版单进程内使用。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）path 处的数据库，启用 WAL 与 busy 等待，确保 schema 就绪。
// per-connection pragma 必须走 DSN：database/sql 连接池里每条连接都要生效，
// 用 db.Exec 设置只会命中当时那条连接（spike 实测并发下其余连接立刻 SQLITE_BUSY）。
func Open(path string) (*Store, error) {
	dsn := "file:" + strings.ReplaceAll(filepath.ToSlash(path), "?", "%3F") +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	// 个人版：进程内并发写由 BEGIN IMMEDIATE + busy_timeout 串行化；
	// 多连接读写在 WAL 下安全。写竞争激烈时可收敛为 1（机制不变）。
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: ensure schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// JournalMode 暴露当前日志模式（spike 断言 WAL 用）。
func (s *Store) JournalMode(ctx context.Context) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	return mode, err
}

// AppendEvents 在单个 BEGIN IMMEDIATE 事务内追加一批事件并同事务写 outbox：
// seq 由存储按房间分配（调用方不指定）；任一 event_id 重复则整批回滚。
// 返回落库后的信封（含分配的 seq），顺序与入参一致。
func (s *Store) AppendEvents(ctx context.Context, envelopes []protocol.Envelope) ([]protocol.Envelope, error) {
	if len(envelopes) == 0 {
		return nil, nil
	}
	roomID := envelopes[0].RoomID
	for i := range envelopes {
		if envelopes[i].RoomID != roomID {
			return nil, fmt.Errorf("sqlite: 一批事件必须同 room（%s vs %s）", roomID, envelopes[i].RoomID)
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var maxSeq int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM room_events WHERE room_id = ?", roomID,
	).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("sqlite: read max seq: %w", err)
	}

	appended := make([]protocol.Envelope, len(envelopes))
	for i := range envelopes {
		env := envelopes[i]
		maxSeq++
		env.Seq = maxSeq
		raw, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("sqlite: marshal envelope %s: %w", env.EventID, err)
		}
		res, err := conn.ExecContext(ctx, `
			INSERT INTO room_events (event_id, tenant_id, room_id, seq, type, schema_version, occurred_at, envelope)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			env.EventID, env.TenantID, env.RoomID, env.Seq, env.Type, env.SchemaVersion, env.OccurredAt, string(raw),
		)
		if err != nil {
			if isUniqueViolation(err) {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateEvent, env.EventID)
			}
			return nil, fmt.Errorf("sqlite: insert event: %w", err)
		}
		pos, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("sqlite: last insert id: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO outbox (room_id, global_pos, event_id, envelope) VALUES (?, ?, ?, ?)`,
			env.RoomID, pos, env.EventID, string(raw),
		); err != nil {
			return nil, fmt.Errorf("sqlite: insert outbox: %w", err)
		}
		appended[i] = env
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return appended, nil
}

// StoredEvent 是读路径的返回形态：权威信封 + opaque cursor（对外不暴露 global_pos）。
type StoredEvent struct {
	Envelope protocol.Envelope
	Cursor   string
}

// EncodeCursor 把全局位编码为对外不透明游标（v1 前缀留作格式演进）。
func EncodeCursor(globalPos int64) string {
	return base64.URLEncoding.EncodeToString([]byte("v1:" + strconv.FormatInt(globalPos, 10)))
}

// DecodeCursor 解析不透明游标；空串返回 0（从头开始）。非法格式报错。
func DecodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("sqlite: 非法游标: %w", err)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] != "v1" {
		return 0, fmt.Errorf("sqlite: 无法识别的游标版本: %q", cursor)
	}
	pos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || pos < 0 {
		return 0, fmt.Errorf("sqlite: 游标位非法: %q", cursor)
	}
	return pos, nil
}

// EventsAfter 从 cursor 之后按全局位续读某房间的事件（订阅续传与历史读共用）。
// limit ≤0 时取 100。返回下一游标；无更多事件时 next 为空串（已追平）。
func (s *Store) EventsAfter(ctx context.Context, roomID, cursor string, limit int) (events []StoredEvent, next string, err error) {
	pos, err := DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT global_pos, envelope FROM room_events
		WHERE room_id = ? AND global_pos > ?
		ORDER BY global_pos LIMIT ?`, roomID, pos, limit)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: query events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var globalPos int64
		var raw string
		if err := rows.Scan(&globalPos, &raw); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan event: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return nil, "", fmt.Errorf("sqlite: unmarshal envelope %d: %w", globalPos, err)
		}
		events = append(events, StoredEvent{Envelope: env, Cursor: EncodeCursor(globalPos)})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: rows: %w", err)
	}
	if len(events) == limit {
		next = events[len(events)-1].Cursor
	}
	return events, next, nil
}

// OutboxEntry 待分发事件（进程内提交后分发；崩溃后由 PendingOutbox 重放）。
type OutboxEntry struct {
	ID      int64
	RoomID  string
	EventID string
	Raw     []byte
}

// PendingOutbox 按提交序取未分发条目（崩溃恢复重放入口）。
func (s *Store) PendingOutbox(ctx context.Context, limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, room_id, event_id, envelope FROM outbox
		WHERE dispatched_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query outbox: %w", err)
	}
	defer rows.Close()
	var entries []OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		var raw string
		if err := rows.Scan(&e.ID, &e.RoomID, &e.EventID, &raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan outbox: %w", err)
		}
		e.Raw = []byte(raw)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// MarkDispatched 幂等标记分发完成（进程内分发成功后调用）。
func (s *Store) MarkDispatched(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE outbox SET dispatched_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id IN ("+
			strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return fmt.Errorf("sqlite: mark dispatched: %w", err)
	}
	return nil
}

// isUniqueViolation 识别 modernc 驱动的 UNIQUE 约束错误（SQLSTATE 23 概念在 SQLite 中为 19/2067）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "constraint failed") &&
		(strings.Contains(msg, "UNIQUE") || strings.Contains(msg, "unique"))
}
