// Package sqlite 是 Room Event Log 的 SQLite（WAL）存储层（ADR-0008 个人版形态）。
// 机制映射：房间串行写 → BEGIN IMMEDIATE 互斥事务；订阅续传 → 全局位（global_pos）
// 构造的 opaque cursor；outbox SKIP LOCKED → 进程内提交后分发 + 崩溃后按表重放。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"

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
CREATE TABLE IF NOT EXISTS command_receipts (
	tenant_id           TEXT NOT NULL,
	idempotency_key     TEXT NOT NULL,
	command_kind        TEXT NOT NULL,
	room_id             TEXT NOT NULL,
	request_fingerprint TEXT NOT NULL,
	event_id            TEXT NOT NULL,
	room_version        INTEGER NOT NULL,
	executed_at         TEXT NOT NULL,
	PRIMARY KEY (tenant_id, idempotency_key, command_kind)
);
CREATE TABLE IF NOT EXISTS context_receipts (
	receipt_id    TEXT PRIMARY KEY,
	room_id       TEXT NOT NULL,
	task_id       TEXT NOT NULL,
	watermark     INTEGER NOT NULL,
	layer_digests TEXT NOT NULL,
	created_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS engine_claims (
	room_id           TEXT NOT NULL,
	stimulus_event_id TEXT NOT NULL,
	envelope          TEXT NOT NULL,
	claimed_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	PRIMARY KEY (room_id, stimulus_event_id)
);
CREATE TABLE IF NOT EXISTS migrations (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
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
	// 二轮审校 #19：DB 文件 owner-only（目录 0700 之外的兜底；WAL/SHM 由目录权限覆盖）
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		db.Close()
		return nil, fmt.Errorf("sqlite: chmod db: %w", err)
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
	return s.appendTx(ctx, envelopes, nil)
}

// AppendWithReceipt 实现 room.AtomicStore：事件 + 幂等回执同事务原子落库；
// 事件 ID 或回执键冲突（并发同命令竞态的后到者）返回 room.ErrDuplicateReceipt，整批回滚。
func (s *Store) AppendWithReceipt(ctx context.Context, envelopes []protocol.Envelope, receipt room.CommandReceipt) ([]protocol.Envelope, error) {
	return s.appendTx(ctx, envelopes, &receipt)
}

// appendTx 共享事务体：BEGIN IMMEDIATE → 分配 seq → 写 room_events + outbox（+ 可选回执）→ COMMIT。
func (s *Store) appendTx(ctx context.Context, envelopes []protocol.Envelope, receipt *room.CommandReceipt) ([]protocol.Envelope, error) {
	if len(envelopes) == 0 && receipt == nil {
		return nil, nil
	}
	roomID := ""
	if len(envelopes) > 0 {
		roomID = envelopes[0].RoomID
		for i := range envelopes {
			if envelopes[i].RoomID != roomID {
				return nil, fmt.Errorf("sqlite: 一批事件必须同 room（%s vs %s）", roomID, envelopes[i].RoomID)
			}
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
	if roomID != "" {
		if err := conn.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(seq), 0) FROM room_events WHERE room_id = ?", roomID,
		).Scan(&maxSeq); err != nil {
			return nil, fmt.Errorf("sqlite: read max seq: %w", err)
		}
	}

	if receipt != nil && roomID != "" {
		// 回执键先查（先于版本校验）：预检与提交之间的竞态重放优先于版本冲突
		var receiptExists bool
		if err := conn.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM command_receipts
			WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind,
		).Scan(&receiptExists); err != nil {
			return nil, fmt.Errorf("sqlite: check receipt: %w", err)
		}
		if receiptExists {
			return nil, fmt.Errorf("%w: %s", room.ErrDuplicateReceipt, receipt.IdempotencyKey)
		}
		// 乐观并发在 BEGIN IMMEDIATE 临界区内强制（P-03：冲突判定在提交事务内）
		if receipt.ExpectedRoomVersion != maxSeq {
			return nil, fmt.Errorf("%w: expected=%d current=%d",
				room.ErrVersionConflict, receipt.ExpectedRoomVersion, maxSeq)
		}
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
				if receipt != nil {
					// 回执式追加中事件撞车 = 并发同命令竞态后到者
					return nil, fmt.Errorf("%w: %s", room.ErrDuplicateReceipt, env.EventID)
				}
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

	if receipt != nil {
		// RoomVersion 权威回填：追加后的最新 seq（调用方传入值不信任，D3 修复）
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO command_receipts
			(tenant_id, idempotency_key, command_kind, room_id, request_fingerprint, event_id, room_version, executed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind, receipt.RoomID,
			receipt.RequestFingerprint, receipt.EventID, maxSeq, receipt.ExecutedAt,
		); err != nil {
			if isUniqueViolation(err) {
				return nil, fmt.Errorf("%w: %s", room.ErrDuplicateReceipt, receipt.IdempotencyKey)
			}
			return nil, fmt.Errorf("sqlite: insert receipt: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return appended, nil
}

// LookupReceipt 实现 room.AtomicStore：未命中返回 (nil, nil)。
func (s *Store) LookupReceipt(ctx context.Context, tenantID, idempotencyKey, commandKind string) (*room.CommandReceipt, error) {
	rc := &room.CommandReceipt{}
	err := s.db.QueryRowContext(ctx, `
		SELECT room_id, request_fingerprint, event_id, room_version, executed_at
		FROM command_receipts
		WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?`,
		tenantID, idempotencyKey, commandKind,
	).Scan(&rc.RoomID, &rc.RequestFingerprint, &rc.EventID, &rc.RoomVersion, &rc.ExecutedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: lookup receipt: %w", err)
	}
	rc.TenantID, rc.IdempotencyKey, rc.CommandKind = tenantID, idempotencyKey, commandKind
	return rc, nil
}

// RoomVersion 实现 room.AtomicStore：房间当前版本（最新 seq；空房 0）。
func (s *Store) RoomVersion(ctx context.Context, roomID string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM room_events WHERE room_id = ?", roomID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: room version: %w", err)
	}
	return v, nil
}

// RoomExists 实现 room.AtomicStore：是否已见 room.created。
func (s *Store) RoomExists(ctx context.Context, roomID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM room_events WHERE room_id = ? AND type = ?)",
		roomID, protocol.EventRoomCreated).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: room exists: %w", err)
	}
	return exists, nil
}

// EventsAfter 实现 room.EventReader：从 cursor 之后按全局位续读某房间的事件
// （订阅续传与历史读共用）。limit ≤0 时取 100。返回下一游标；无更多事件时 next 为空串。
func (s *Store) EventsAfter(ctx context.Context, roomID, cursor string, limit int) (events []room.StoredEvent, next string, err error) {
	pos, err := protocol.DecodeCursor(cursor)
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
		events = append(events, room.StoredEvent{Envelope: env, Cursor: protocol.EncodeCursor(globalPos)})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: rows: %w", err)
	}
	if len(events) == limit {
		next = events[len(events)-1].Cursor
	}
	return events, next, nil
}

// OutboxEntry 待分发事件（outbox.Entry 的本地别名；进程内提交后分发，崩溃后重放）。
type OutboxEntry = outbox.Entry

// Pending 实现 outbox.Store：按提交序取未分发条目（崩溃恢复重放入口）。
func (s *Store) Pending(ctx context.Context, limit int) ([]outbox.Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, room_id, event_id, global_pos, envelope FROM outbox
		WHERE dispatched_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query outbox: %w", err)
	}
	defer rows.Close()
	var entries []outbox.Entry
	for rows.Next() {
		var e outbox.Entry
		var raw string
		if err := rows.Scan(&e.ID, &e.RoomID, &e.EventID, &e.GlobalPos, &raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan outbox: %w", err)
		}
		e.Envelope = []byte(raw)
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

// ---- ClaimStore：轮次交接声明（二轮审校 #9，room.ClaimStore 端口）----

// ClaimStimulus 实现 room.ClaimStore：INSERT OR IGNORE，true = 首次声明。
func (s *Store) ClaimStimulus(ctx context.Context, roomID, stimulusEventID string, envelope []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO engine_claims (room_id, stimulus_event_id, envelope) VALUES (?, ?, ?)`,
		roomID, stimulusEventID, string(envelope))
	if err != nil {
		return false, fmt.Errorf("sqlite: claim stimulus: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: claim rows: %w", err)
	}
	return n > 0, nil
}

// DeleteClaim 实现 room.ClaimStore。
func (s *Store) DeleteClaim(ctx context.Context, roomID, stimulusEventID string) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM engine_claims WHERE room_id = ? AND stimulus_event_id = ?",
		roomID, stimulusEventID); err != nil {
		return fmt.Errorf("sqlite: delete claim: %w", err)
	}
	return nil
}

// PendingClaims 实现 room.ClaimStore。
func (s *Store) PendingClaims(ctx context.Context) ([]room.StimulusClaim, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT room_id, stimulus_event_id, envelope FROM engine_claims")
	if err != nil {
		return nil, fmt.Errorf("sqlite: query claims: %w", err)
	}
	defer rows.Close()
	var out []room.StimulusClaim
	for rows.Next() {
		var c room.StimulusClaim
		var raw string
		if err := rows.Scan(&c.RoomID, &c.StimulusEventID, &raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan claim: %w", err)
		}
		c.Envelope = []byte(raw)
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertReceipt 实现 room.ReceiptStore：上下文回执落库（可查可审计）。
func (s *Store) InsertReceipt(ctx context.Context, receipt contextx.Receipt) error {
	raw, err := json.Marshal(receipt.LayerDigests)
	if err != nil {
		return fmt.Errorf("sqlite: marshal digests: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO context_receipts (receipt_id, room_id, task_id, watermark, layer_digests, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(receipt_id) DO NOTHING`,
		receipt.ReceiptID, receipt.RoomID, receipt.TaskID, receipt.Watermark, string(raw), receipt.CreatedAt)
	if err != nil {
		return fmt.Errorf("sqlite: insert receipt: %w", err)
	}
	return nil
}
