package sqlite

// 回房通知存储面（0.6.10 / ADR-0033）：mission_events 游标投影 → 通知行；
// 原子投递（同一事务内核对墓碑/暂停 + 追加 message.posted + 标记 delivered）。
// ReturnStore 适配 internal/missionreturn.Store 端口；信封落库复用 appendTx 的
// 同一路径（room_events + outbox + FTS 同事务）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/sisibeloved/Mosaic/internal/missionreturn"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// migrationV25（ADR-0033 / 0.6.10）：回房通知——通知行 + 投影游标 + 来源墓碑列。
// DDL 幂等；ALTER 的 duplicate-column 容忍走 v20/v22 逐语句模式（多语句单 Exec
// 会在首个 duplicate 处中止）；deleted_at 由 DeleteRoom 级联或投递侧兜底回填，
// 旧库升级默认 NULL（房间仍在）。
var migrationV25 = []string{`
CREATE TABLE IF NOT EXISTS mission_return_notices (
  notice_id       TEXT PRIMARY KEY,
  source_seq      INTEGER NOT NULL UNIQUE,
  mission_id      TEXT NOT NULL,
  room_id         TEXT NOT NULL,
  task_id         TEXT NOT NULL,
  kind            TEXT NOT NULL,
  contract_version INTEGER NOT NULL,
  review_round    INTEGER NOT NULL DEFAULT 0,
  package_id      TEXT NOT NULL DEFAULT '',
  reviewer        TEXT NOT NULL DEFAULT '',
  reason          TEXT NOT NULL DEFAULT '',
  objective       TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'pending',
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL DEFAULT '',
  last_error      TEXT NOT NULL DEFAULT '',
  room_event_id   TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mission_return_notices_mission
  ON mission_return_notices(mission_id, source_seq);
CREATE INDEX IF NOT EXISTS idx_mission_return_notices_due
  ON mission_return_notices(status, next_attempt_at);
CREATE TABLE IF NOT EXISTS mission_return_state (
  id       INTEGER PRIMARY KEY CHECK (id = 1),
  last_seq INTEGER NOT NULL
);
INSERT OR IGNORE INTO mission_return_state (id, last_seq) VALUES (1, 0);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (25, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`,
	"ALTER TABLE mission_sources ADD COLUMN deleted_at TEXT",
}

// insertEnvelopeOnConn 单信封落库（room_events + outbox + message.posted 的 FTS 行）。
// seq 由调用方分配（BEGIN IMMEDIATE 临界区内读 MAX(seq) 后自增）；返回 outbox global_pos。
// 供 appendTx 与 ReturnStore.Deliver（ADR-0033 原子投递）共用——同一事件账本路径。
func insertEnvelopeOnConn(ctx context.Context, conn *sql.Conn, env protocol.Envelope) (int64, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("sqlite: marshal envelope %s: %w", env.EventID, err)
	}
	res, err := conn.ExecContext(ctx, `
		INSERT INTO room_events (event_id, tenant_id, room_id, seq, type, schema_version, occurred_at, envelope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		env.EventID, env.TenantID, env.RoomID, env.Seq, env.Type, env.SchemaVersion, env.OccurredAt, string(raw),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: %s", ErrDuplicateEvent, env.EventID)
		}
		return 0, fmt.Errorf("sqlite: insert event: %w", err)
	}
	pos, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite: last insert id: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO outbox (room_id, global_pos, event_id, envelope) VALUES (?, ?, ?, ?)`,
		env.RoomID, pos, env.EventID, string(raw),
	); err != nil {
		return 0, fmt.Errorf("sqlite: insert outbox: %w", err)
	}
	// M3-3 按需平面：message.posted 正文同事务入 FTS5 索引（与事件账本一致——
	// 索引缺行由 SearchMessages 的自愈校验兜底重建）。
	if env.Type == protocol.EventMessagePosted {
		if body, actor, actorKind, threadID, ok := ftsFieldsOf(env); ok {
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO room_fts (room_id, event_id, actor, actor_kind, thread_id, occurred_at, seq, global_pos, body)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				env.RoomID, env.EventID, actor, actorKind, threadID, env.OccurredAt, env.Seq, pos, body,
			); err != nil {
				return 0, fmt.Errorf("sqlite: insert fts: %w", err)
			}
		}
	}
	return pos, nil
}

// ReturnStore missionreturn.Store 端口适配（回房通知投影/投递/状态/台账）。
type ReturnStore struct{ Store *Store }

// statusChangedPayload 投影侧快照的 payload 字段（ADR-0032 status_changed 族）。
type statusChangedPayload struct {
	To          string `json:"to"`
	Decision    string `json:"decision"`
	Reviewer    string `json:"reviewer"`
	Reason      string `json:"reason"`
	PackageID   string `json:"package_id"`
	ReviewRound int    `json:"review_round"`
}

// classify 事实 → 通知 kind（表 1，ADR-0033 §1）；非四类返回空。
func classify(p statusChangedPayload) missionreturn.Kind {
	switch {
	case p.To == "reviewing":
		return missionreturn.KindReviewing
	case p.To == "accepted":
		return missionreturn.KindAccepted
	case p.To == "ready" && p.Decision == "returned":
		return missionreturn.KindReturned
	case p.To == "canceled":
		return missionreturn.KindCanceled
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimFunc(s, func(c rune) bool {
		return unicode.IsSpace(c) || c == '\ufeff'
	}))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n])
}

// Project 一轮投影（单 BEGIN IMMEDIATE 事务）：游标后的 status_changed 逐条
// 判类 → 有房间来源的产行（source_seq UNIQUE 幂等）→ 推进游标。无关事件也推进
// 游标（不重扫）。返回本轮新建行数。
func (a ReturnStore) Project(ctx context.Context) (int, error) {
	conn, err := a.Store.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("sqlite: conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var lastSeq int64
	if err := conn.QueryRowContext(ctx,
		"SELECT last_seq FROM mission_return_state WHERE id = 1").Scan(&lastSeq); err != nil {
		return 0, fmt.Errorf("sqlite: read return cursor: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT seq, mission_id, payload FROM mission_events
		WHERE seq > ? AND event_type = 'status_changed' ORDER BY seq LIMIT 500`, lastSeq)
	if err != nil {
		return 0, fmt.Errorf("sqlite: scan mission_events: %w", err)
	}
	type fact struct {
		seq       int64
		missionID string
		payload   statusChangedPayload
	}
	var facts []fact
	for rows.Next() {
		var f fact
		var payload string
		if err := rows.Scan(&f.seq, &f.missionID, &payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sqlite: scan mission_events row: %w", err)
		}
		_ = json.Unmarshal([]byte(payload), &f.payload)
		facts = append(facts, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlite: mission_events rows: %w", err)
	}
	created := 0
	for _, f := range facts {
		lastSeq = f.seq
		kind := classify(f.payload)
		if kind == "" {
			continue
		}
		var roomID, taskID string
		err := conn.QueryRowContext(ctx,
			"SELECT room_id, task_id FROM mission_sources WHERE mission_id = ?", f.missionID).Scan(&roomID, &taskID)
		if errors.Is(err, sql.ErrNoRows) {
			continue // 未关联 Room 的任务不产生通知（RFC §11.3）
		}
		if err != nil {
			return 0, fmt.Errorf("sqlite: mission source: %w", err)
		}
		var objective string
		var contractVersion int
		if err := conn.QueryRowContext(ctx,
			"SELECT objective, contract_version FROM missions WHERE mission_id = ?", f.missionID,
		).Scan(&objective, &contractVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return 0, fmt.Errorf("sqlite: mission row: %w", err)
		}
		res, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO mission_return_notices
			(notice_id, source_seq, mission_id, room_id, task_id, kind, contract_version,
			 review_round, package_id, reviewer, reason, objective, status, attempts,
			 next_attempt_at, last_error, room_event_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, '', '', '',
			        strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
			"rn_"+strconv.FormatInt(f.seq, 10), f.seq, f.missionID, roomID, taskID, string(kind),
			contractVersion, f.payload.ReviewRound, f.payload.PackageID, f.payload.Reviewer,
			truncateRunes(f.payload.Reason, 200), truncateRunes(objective, 80))
		if err != nil {
			return 0, fmt.Errorf("sqlite: insert return notice: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			created++
		}
	}
	if _, err := conn.ExecContext(ctx,
		"UPDATE mission_return_state SET last_seq = ? WHERE id = 1", lastSeq); err != nil {
		return 0, fmt.Errorf("sqlite: advance return cursor: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return created, nil
}

const returnNoticeCols = `notice_id, source_seq, mission_id, room_id, task_id, kind,
 contract_version, review_round, package_id, reviewer, reason, objective, status,
 attempts, next_attempt_at, last_error, room_event_id, created_at, updated_at`

func scanReturnNotice(row interface{ Scan(...any) error }) (missionreturn.Notice, error) {
	var n missionreturn.Notice
	err := row.Scan(&n.NoticeID, &n.SourceSeq, &n.MissionID, &n.RoomID, &n.TaskID, &n.Kind,
		&n.ContractVersion, &n.ReviewRound, &n.PackageID, &n.Reviewer, &n.Reason, &n.Objective,
		&n.Status, &n.Attempts, &n.NextAttemptAt, &n.LastError, &n.RoomEventID, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return missionreturn.Notice{}, err
	}
	return n, nil
}

// Due 到期待投通知（pending|held 且 next_attempt_at 到期），按 room 分组、组内
// source_seq 升序。now 为 RFC3339（datetime() 解析比较，容忍亚秒与 +00:00 形差）。
func (a ReturnStore) Due(ctx context.Context, now string, limit int) ([]missionreturn.Notice, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := a.Store.db.QueryContext(ctx, `
		SELECT `+returnNoticeCols+` FROM mission_return_notices
		WHERE status IN ('pending', 'held')
		  AND (next_attempt_at = '' OR datetime(next_attempt_at) <= datetime(?))
		ORDER BY room_id, source_seq LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: due return notices: %w", err)
	}
	defer rows.Close()
	var out []missionreturn.Notice
	for rows.Next() {
		n, err := scanReturnNotice(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan return notice: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListReturnNotices 通知台账（读面；source_seq 升序）。
func (a ReturnStore) ListReturnNotices(ctx context.Context, missionID string) ([]missionreturn.Notice, error) {
	rows, err := a.Store.db.QueryContext(ctx, `
		SELECT `+returnNoticeCols+` FROM mission_return_notices
		WHERE mission_id = ? ORDER BY source_seq`, missionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list return notices: %w", err)
	}
	defer rows.Close()
	var out []missionreturn.Notice
	for rows.Next() {
		n, err := scanReturnNotice(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan return notice: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// messageCardPayload 回房卡片的 message.posted 载荷（命令面同形的封闭投影）。
type messageCardPayload struct {
	Body        string               `json:"body"`
	ReplyTo     *string              `json:"reply_to"`
	AddressedTo []string             `json:"addressed_to"`
	Relations   []map[string]string  `json:"relations"`
	MissionRef  *protocol.MissionRef `json:"mission_ref,omitempty"`
}

// Deliver 原子投递（ADR-0033 §3）：BEGIN IMMEDIATE 内行守卫 → 墓碑/存在性核对
// （ErrRoomDeleted）→ 暂停核对（ErrRoomPaused）→ 复用 insertEnvelopeOnConn 落卡
// （room_events + outbox + FTS 同事务）→ 标记 delivered。卡与标记同事务落地，
// 崩溃窗口不存在"卡落了标记没落"——去重由构造保证。
func (a ReturnStore) Deliver(ctx context.Context, n missionreturn.Notice, env missionreturn.EnvelopeInput) error {
	conn, err := a.Store.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var status string
	err = conn.QueryRowContext(ctx,
		"SELECT status FROM mission_return_notices WHERE notice_id = ?", n.NoticeID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status != "pending" && status != "held") {
		return missionreturn.ErrNoticeNotPending
	}
	if err != nil {
		return fmt.Errorf("sqlite: notice status: %w", err)
	}
	// 房间失效兜底：墓碑（DeleteRoom 落）或事件账本不存在（含先删房后升级的旧库）。
	var tombstoned bool
	if err := conn.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM room_tombstones WHERE room_id = ?)", n.RoomID).Scan(&tombstoned); err != nil {
		return fmt.Errorf("sqlite: room tombstone: %w", err)
	}
	var hasRoom bool
	if err := conn.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM room_events WHERE room_id = ? LIMIT 1)", n.RoomID).Scan(&hasRoom); err != nil {
		return fmt.Errorf("sqlite: room exists: %w", err)
	}
	if tombstoned || !hasRoom {
		return missionreturn.ErrRoomDeleted
	}
	// 暂停保留：最新 room.paused|room.started 为 paused 即不落卡（AT：恢复后发布）。
	var lastLifecycle string
	err = conn.QueryRowContext(ctx, `
		SELECT type FROM room_events WHERE room_id = ? AND type IN ('room.paused', 'room.started')
		ORDER BY seq DESC LIMIT 1`, n.RoomID).Scan(&lastLifecycle)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: room lifecycle: %w", err)
	}
	if err == nil && lastLifecycle == "room.paused" {
		return missionreturn.ErrRoomPaused
	}
	var tenant string
	if err := conn.QueryRowContext(ctx,
		"SELECT tenant_id FROM room_events WHERE room_id = ? LIMIT 1", n.RoomID).Scan(&tenant); err != nil {
		return fmt.Errorf("sqlite: room tenant: %w", err)
	}
	payload, err := json.Marshal(messageCardPayload{
		Body:        env.Body,
		AddressedTo: []string{},
		Relations:   []map[string]string{},
		MissionRef: &protocol.MissionRef{
			MissionID:       n.MissionID,
			Kind:            string(n.Kind),
			ContractVersion: n.ContractVersion,
			ReviewRound:     n.ReviewRound,
			PackageID:       n.PackageID,
			TaskID:          n.TaskID,
			SourceSeq:       n.SourceSeq,
		},
	})
	if err != nil {
		return fmt.Errorf("sqlite: marshal card payload: %w", err)
	}
	var maxSeq int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM room_events WHERE room_id = ?", n.RoomID).Scan(&maxSeq); err != nil {
		return fmt.Errorf("sqlite: read max seq: %w", err)
	}
	maxSeq++
	envelope := protocol.Envelope{
		EventID:       env.EventID,
		TenantID:      tenant,
		RoomID:        n.RoomID,
		Seq:           maxSeq,
		Type:          protocol.EventMessagePosted,
		SchemaVersion: 1,
		OccurredAt:    env.OccurredAt,
		Actor:         protocol.Actor{ParticipantID: "par_system", Kind: "system"},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       payload,
		Metadata:      map[string]any{},
	}
	if _, err := insertEnvelopeOnConn(ctx, conn, envelope); err != nil {
		return err
	}
	res, err := conn.ExecContext(ctx, `
		UPDATE mission_return_notices
		SET status = 'delivered', room_event_id = ?,
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE notice_id = ? AND status IN ('pending', 'held')`,
		env.EventID, n.NoticeID)
	if err != nil {
		return fmt.Errorf("sqlite: mark notice delivered: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return missionreturn.ErrNoticeNotPending
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return nil
}

// UpdateStatus 非终态行状态更新（held / 退避重试 / failed / 投递侧兜底 canceled）；
// 终态行（delivered|canceled|failed）不受影响。
func (a ReturnStore) UpdateStatus(ctx context.Context, noticeID string, st missionreturn.Status, attempts int, nextAttemptAt, lastError string) error {
	res, err := a.Store.db.ExecContext(ctx, `
		UPDATE mission_return_notices
		SET status = ?, attempts = ?, next_attempt_at = ?, last_error = ?,
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE notice_id = ? AND status IN ('pending', 'held')`,
		string(st), attempts, nextAttemptAt, lastError, noticeID)
	if err != nil {
		return fmt.Errorf("sqlite: update return notice: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return missionreturn.ErrNoticeNotPending
	}
	return nil
}
