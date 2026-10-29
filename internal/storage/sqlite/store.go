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
	"sort"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/slot"

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
	position          INTEGER NOT NULL DEFAULT 0,
	claimed_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	PRIMARY KEY (room_id, stimulus_event_id)
);
CREATE TABLE IF NOT EXISTS room_tombstones (
  room_id TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  deleted_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (2, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (3, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (4, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (5, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(id) WHERE dispatched_at IS NULL;

-- v4（RFC-0014 / ADR-0014）：per-doc 事件流（同一 SQLite 库、不同表）——
-- version per-doc 单调递增，兼任文档级 CAS 序号与 doc:{id} 频道 SSE cursor。
-- outbox/receipts/tombstones 独立成表：房间表面 room_id NOT NULL 且假设房间信封，不可复用。
CREATE TABLE IF NOT EXISTS doc_events (
	event_id       TEXT NOT NULL UNIQUE,
	tenant_id      TEXT NOT NULL,
	doc_id         TEXT NOT NULL,
	version        INTEGER NOT NULL,
	type           TEXT NOT NULL,
	schema_version INTEGER NOT NULL,
	occurred_at    TEXT NOT NULL,
	envelope       TEXT NOT NULL,
	UNIQUE (doc_id, version)
);
CREATE INDEX IF NOT EXISTS idx_doc_events_doc ON doc_events(doc_id, version);

CREATE TABLE IF NOT EXISTS doc_outbox (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	doc_id       TEXT NOT NULL,
	version      INTEGER NOT NULL,
	event_id     TEXT NOT NULL UNIQUE,
	envelope     TEXT NOT NULL,
	dispatched_at TEXT,
	created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_doc_outbox_pending ON doc_outbox(id) WHERE dispatched_at IS NULL;

CREATE TABLE IF NOT EXISTS doc_command_receipts (
	tenant_id           TEXT NOT NULL,
	idempotency_key     TEXT NOT NULL,
	command_kind        TEXT NOT NULL,
	doc_id              TEXT NOT NULL,
	request_fingerprint TEXT NOT NULL,
	event_id            TEXT NOT NULL,
	doc_version         INTEGER NOT NULL,
	executed_at         TEXT NOT NULL,
	PRIMARY KEY (tenant_id, idempotency_key, command_kind)
);
CREATE TABLE IF NOT EXISTS doc_tombstones (
  doc_id TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  deleted_at TEXT NOT NULL
);

-- v5（RFC-0015）：文档级外部共享映射（快照语义——记录创建时点，远端内容
-- 不回同步）。非事件派生的权威状态（外部副作用不可由本地事件流重建），
-- 故独立成表而非投影；文档删除不级联（解除映射是显式动作）。
CREATE TABLE IF NOT EXISTS doc_external_shares (
  doc_id       TEXT NOT NULL,
  provider     TEXT NOT NULL,
  external_url TEXT NOT NULL,
  shared_by    TEXT NOT NULL,
  shared_at    TEXT NOT NULL,
  PRIMARY KEY (doc_id, provider)
);

-- v6（RFC-0017 M2）：表格手动交换记录（副本语义——远端副作用为权威事实，
-- 非事件派生，独立成表）。幂等键唯一支撑 execute 重放去重；running 态
-- 仅崩溃窗口存在（启动扫成 unknown，M2 不做自动恢复）。
CREATE TABLE IF NOT EXISTS sheet_transfers (
  transfer_id     TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  direction       TEXT NOT NULL,
  provider        TEXT NOT NULL,
  status          TEXT NOT NULL,
  doc_id          TEXT NOT NULL DEFAULT '',
  external_url    TEXT NOT NULL DEFAULT '',
  title           TEXT NOT NULL DEFAULT '',
  source_version  INTEGER NOT NULL DEFAULT 0,
  snapshot_hash   TEXT NOT NULL DEFAULT '',
  report_json     TEXT NOT NULL DEFAULT '',
  error           TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  -- M3 回写：进度与恢复状态（state_json 含目标前态/剩余计划——不出网）
  cells_total     INTEGER NOT NULL DEFAULT 0,
  cells_written   INTEGER NOT NULL DEFAULT 0,
  state_json      TEXT NOT NULL DEFAULT ''
);

-- v7（RFC-0018 / ADR-0017，0.5.0）：独立 Agent 身份域。agents 为聚合快照
-- （0.5.0 无状态转移，事件流随 0.5.1 引入后本表降为投影重建基线）；
-- agent_aliases 为旧 ID → AgentID 的只增解析面（bot_id / profile_id /
-- participant_id 三类），(kind, value) 唯一——别名一经登记不改写、不删除。
CREATE TABLE IF NOT EXISTS agents (
  agent_id     TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  adapter      TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'active',
  source       TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_aliases (
  alias_kind  TEXT NOT NULL,
  alias_value TEXT NOT NULL,
  agent_id    TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  PRIMARY KEY (alias_kind, alias_value)
);
CREATE INDEX IF NOT EXISTS idx_agent_aliases_agent ON agent_aliases(agent_id);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (7, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v8（RFC-0018 §3.2 / ADR-0018，0.5.1）：Agent 生命周期事件流（append-only）。
-- agents 自 v8 起为投影——转移 = 单事务 [状态校验 → 更新投影 → 追加事件]，
-- 事件流 ⊇ 全部历史，投影可由流重建。回填：v7 存量行按创建序（rowid）补
-- created 事件（payload 对齐行值、occurred_at 取行内 created_at）；
-- NOT EXISTS 守卫使回填幂等——新库空集、重开不重复。
CREATE TABLE IF NOT EXISTS agent_events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  agent_id    TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL,
  occurred_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_events_agent ON agent_events(agent_id, seq);
INSERT INTO agent_events (agent_id, event_type, payload, occurred_at)
SELECT a.agent_id, 'created',
       json_object('source', a.source, 'adapter', a.adapter, 'display_name', a.display_name),
       a.created_at
  FROM agents a
 WHERE NOT EXISTS (SELECT 1 FROM agent_events e
                    WHERE e.agent_id = a.agent_id AND e.event_type = 'created')
 ORDER BY a.rowid;
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (8, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v9（RFC-0018 §5.1 / ADR-0019，0.5.2）：职责模板。slot_specs 为不可变版本行
-- ——(slot_spec_id, version) 主键，修订即追加新版本，无 UPDATE/DELETE 路径
-- （版本行本身即不可变历史，不另设事件流）。content_json 为完整内容（权威
-- 事实；name 冗余列便于谱系列表直查）。新域首表，无存量回填。
CREATE TABLE IF NOT EXISTS slot_specs (
  slot_spec_id    TEXT NOT NULL,
  version         INTEGER NOT NULL,
  schema_version  INTEGER NOT NULL,
  name            TEXT NOT NULL,
  responsibility  TEXT NOT NULL,
  content_json    TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (slot_spec_id, version)
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (9, strftime('%Y-%m-%dT%H:%M:%fZ','now'));

-- v10（RFC-0018 §3.1/§5.2 / ADR-0020，0.5.3）：职责实例。slot_instances 为
-- 某作用域内实际承担的一份具体职责：引用模板谱系不固定版本（Binding 才
-- 固定，随 0.5.4）；knowledge_namespace 全局唯一、终身稳定、永不重派——
-- 职责知识（0.7 分域记忆）挂该命名空间，不挂任职者私有库。实例无状态机
-- （RFC §3.2 未定义），无 UPDATE/DELETE 路径。
CREATE TABLE IF NOT EXISTS slot_instances (
  slot_instance_id    TEXT PRIMARY KEY,
  slot_spec_id        TEXT NOT NULL,
  scope               TEXT NOT NULL DEFAULT 'workspace',
  title               TEXT NOT NULL,
  knowledge_namespace TEXT NOT NULL UNIQUE,
  created_from_version INTEGER NOT NULL,
  created_at          TEXT NOT NULL
);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (10, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// roomFTSDDL v3（M3-3 按需平面）：FTS5 trigram 全文索引（v1.46 spike 实证
// modernc v1.57.0 / SQLite 3.53.3：unicode61 对中文整串成单 token 不可用，
// trigram 对 CJK 子串 ≥3 字与英文均正确命中；<3 字查询由 SearchMessages
// 回退 LIKE 子串）。虚拟表 CREATE 不在 schemaDDL 事务语义里幂等问题：
// IF NOT EXISTS 即幂等；存量库由 Open 的 v3 迁移执行。
const roomFTSDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS room_fts USING fts5(
	room_id UNINDEXED,
	event_id UNINDEXED,
	actor UNINDEXED,
	actor_kind UNINDEXED,
	thread_id UNINDEXED,
	occurred_at UNINDEXED,
	seq UNINDEXED,
	global_pos UNINDEXED,
	body,
	tokenize='trigram'
);
`

// docFTSDDL v4（RFC-0014 §2.8 文档全文搜索）：FTS5 trigram 派生索引（标题+正文；
// 与 room_fts 同 tokenizer 裁定——trigram 对 CJK 子串 ≥3 字与英文均正确命中，
// <3 字查询由 SearchDocs 回退 LIKE 子串）。doc.created 索标题；doc.revision_committed
// 索本批 ops 文本（增量派生，索引缺行由 SearchDocs 的自愈校验兜底重建）。
const docFTSDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS doc_fts USING fts5(
	doc_id UNINDEXED,
	event_id UNINDEXED,
	version UNINDEXED,
	actor UNINDEXED,
	occurred_at UNINDEXED,
	title,
	body,
	tokenize='trigram'
);
`

// migrationV2 既有库补 engine_claims.position 列（四轮复审 #14：恢复重驱动需按
// 持久顺序）。CREATE TABLE 只对新建库生效；ALTER 幂等（duplicate column 忽略）。
const migrationV2 = `ALTER TABLE engine_claims ADD COLUMN position INTEGER NOT NULL DEFAULT 0`

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
	// v2 迁移（四轮复审 #14）：既有库补 engine_claims.position；duplicate column 忽略
	if _, err := db.Exec(migrationV2); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v2: %w", err)
	}
	// v3 迁移（M3-3）：FTS5 trigram 全文索引（存量库建虚拟表；新库由同 DDL 幂等）
	if _, err := db.Exec(roomFTSDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v3 (room_fts): %w", err)
	}
	// v4 迁移（RFC-0014 / ADR-0014）：doc_fts 全文索引（doc 事件表在 schemaDDL 内幂等）
	if _, err := db.Exec(docFTSDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migration v4 (doc_fts): %w", err)
	}
	// 二轮审校 #19：DB 文件 owner-only（目录 0700 之外的兜底；WAL/SHM 由目录权限覆盖）
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		db.Close()
		return nil, fmt.Errorf("sqlite: chmod db: %w", err)
	}
	// 复审 #17：既有目录权限补收紧（MkdirAll 只在新建时生效；WAL/SHM 也在该目录）。
	// best-effort：失败不阻断打开（只读介质等场景）；Windows 无 POSIX 权限语义，
	// 依赖用户目录 ACL（platform-notes W-6）。
	_ = os.Chmod(filepath.Dir(path), 0o700)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// JournalMode 暴露当前日志模式（spike 断言 WAL 用）。
func (s *Store) JournalMode(ctx context.Context) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	return mode, err
}

// BackupTo 生成一致快照到 destPath（M4-0 备份面：VACUUM INTO——SQLite 官方
// 备份路径，无需停写、不搬 WAL/SHM，产物自包含单文件）。destPath 不得已存在
// （VACUUM INTO 语义：已存在即报错——由调用方保证新目录）。
func (s *Store) BackupTo(ctx context.Context, destPath string) error {
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("sqlite: backup dest 已存在: %s", destPath)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("sqlite: vacuum into: %w", err)
	}
	return os.Chmod(destPath, 0o600)
}

// AppendEvents 在单个 BEGIN IMMEDIATE 事务内追加一批事件并同事务写 outbox：
// seq 由存储按房间分配（调用方不指定）；任一 event_id 重复则整批回滚。
// 返回落库后的信封（含分配的 seq），顺序与入参一致。
func (s *Store) AppendEvents(ctx context.Context, envelopes []protocol.Envelope) ([]protocol.Envelope, error) {
	return s.appendTx(ctx, envelopes, nil, nil)
}

// AppendWithReceipt 实现 room.AtomicStore：事件 + 幂等回执同事务原子落库；
// 事件 ID 或回执键冲突（并发同命令竞态的后到者）返回 room.ErrDuplicateReceipt，整批回滚。
func (s *Store) AppendWithReceipt(ctx context.Context, envelopes []protocol.Envelope, receipt room.CommandReceipt) ([]protocol.Envelope, error) {
	return s.appendTx(ctx, envelopes, &receipt, nil)
}

// AppendEventsIf 实现 room.CASStore（四轮复审 #12）：当前房间版本 != expected 即
// ErrVersionConflict（BEGIN IMMEDIATE 临界区内判定），整批回滚。
func (s *Store) AppendEventsIf(ctx context.Context, envelopes []protocol.Envelope, expectedRoomVersion int64) ([]protocol.Envelope, error) {
	return s.appendTx(ctx, envelopes, nil, &expectedRoomVersion)
}

// appendTx 共享事务体：BEGIN IMMEDIATE → 分配 seq → 写 room_events + outbox（+ 可选回执）→ COMMIT。
func (s *Store) appendTx(ctx context.Context, envelopes []protocol.Envelope, receipt *room.CommandReceipt, expectedVersion *int64) ([]protocol.Envelope, error) {
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

	// CAS 期望版本（四轮复审 #12）：与回执的乐观并发同在 BEGIN IMMEDIATE 临界区内判定
	if expectedVersion != nil && roomID != "" && *expectedVersion != maxSeq {
		return nil, fmt.Errorf("%w: expected=%d current=%d",
			room.ErrVersionConflict, *expectedVersion, maxSeq)
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
		// M3-3 按需平面：message.posted 正文同事务入 FTS5 索引（与事件账本一致——
		// 索引缺行由 SearchMessages 的自愈校验兜底重建）。
		if env.Type == protocol.EventMessagePosted {
			if body, actor, actorKind, threadID, ok := ftsFieldsOf(env); ok {
				if _, err := conn.ExecContext(ctx, `
					INSERT INTO room_fts (room_id, event_id, actor, actor_kind, thread_id, occurred_at, seq, global_pos, body)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					env.RoomID, env.EventID, actor, actorKind, threadID, env.OccurredAt, env.Seq, pos, body,
				); err != nil {
					return nil, fmt.Errorf("sqlite: insert fts: %w", err)
				}
			}
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

// ListRooms 实现 room.RoomLister：单查询聚合（个人版房间/事件规模，相关子查询足够）。
// display_name 取序内最新 room.created/room.renamed 载荷；paused = 最新生命周期事件为
// room.paused；message_count 计 message.posted。按 last_event_at 倒序（同刻 room_id
// 升序兜底，排序确定性——与 MemStore 实现同规则）。
// v1.72：agents 聚合与 room.RosterOf 同语义分层（侧栏私聊/群聊分型依据）——
// explicit = room.created.payload.agents（建房间时物化快照）+ participant.admitted
// 载荷；无 explicit 的旧房间 → 历史推导（agent actor 参与者 + intent.recorded/
// floor.granted 载荷目标）；两者皆无 → NULL = 全席（首轮兼容；引擎写库数据均为
// par_* 形态，SQL 不重复 participantIDPattern 校验）。
func (s *Store) ListRooms(ctx context.Context) ([]room.RoomSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH roster_rows AS (
			SELECT e.room_id AS room_id, je.value AS pid, 1 AS is_explicit
				FROM room_events e, json_each(COALESCE(json_extract(e.envelope, '$.payload.agents'), '[]')) je
				WHERE e.type = 'room.created'
			UNION
			SELECT e.room_id, json_extract(e.envelope, '$.payload.participant_id'), 1
				FROM room_events e
				WHERE e.type = 'participant.admitted'
			UNION
			SELECT e.room_id, json_extract(e.envelope, '$.actor.participant_id'), 0
				FROM room_events e
				WHERE json_extract(e.envelope, '$.actor.kind') = 'agent'
			UNION
			SELECT e.room_id, json_extract(e.envelope, '$.payload.participant_id'), 0
				FROM room_events e
				WHERE e.type IN ('intent.recorded', 'floor.granted')
		),
		roster AS (
			SELECT room_id,
				CASE WHEN SUM(is_explicit) > 0
					THEN GROUP_CONCAT(CASE WHEN is_explicit = 1 THEN pid END)
					ELSE GROUP_CONCAT(pid)
				END AS agents
			FROM roster_rows
			GROUP BY room_id
		)
		SELECT
			r.room_id,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.display_name') FROM room_events e
				WHERE e.room_id = r.room_id AND e.type IN ('room.created', 'room.renamed')
				ORDER BY e.seq DESC LIMIT 1), '') AS display_name,
			(SELECT e.occurred_at FROM room_events e
				WHERE e.room_id = r.room_id AND e.type = 'room.created'
				ORDER BY e.seq ASC LIMIT 1) AS created_at,
			(SELECT e.occurred_at FROM room_events e
				WHERE e.room_id = r.room_id
				ORDER BY e.seq DESC LIMIT 1) AS last_event_at,
			COALESCE((SELECT e.type = 'room.paused' FROM room_events e
				WHERE e.room_id = r.room_id AND e.type IN ('room.paused', 'room.started')
				ORDER BY e.seq DESC LIMIT 1), 0) AS paused,
			(SELECT COUNT(*) FROM room_events e
				WHERE e.room_id = r.room_id AND e.type = 'message.posted') AS message_count,
			ro.agents
		FROM (SELECT DISTINCT room_id FROM room_events WHERE type = 'room.created') r
		LEFT JOIN roster ro ON ro.room_id = r.room_id
		ORDER BY last_event_at DESC, r.room_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list rooms: %w", err)
	}
	defer rows.Close()
	out := []room.RoomSummary{}
	for rows.Next() {
		var sum room.RoomSummary
		var agents sql.NullString
		if err := rows.Scan(&sum.RoomID, &sum.DisplayName, &sum.CreatedAt, &sum.LastEventAt,
			&sum.Paused, &sum.MessageCount, &agents); err != nil {
			return nil, fmt.Errorf("sqlite: scan room summary: %w", err)
		}
		if agents.Valid && agents.String != "" {
			// GROUP_CONCAT 无序：拆分后排序保输出确定性（与 MemStore 投影一致）。
			list := strings.Split(agents.String, ",")
			sort.Strings(list)
			sum.Agents = list
		}
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: rows: %w", err)
	}
	return out, nil
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
func (s *Store) ClaimStimulus(ctx context.Context, roomID, stimulusEventID string, envelope []byte, position int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO engine_claims (room_id, stimulus_event_id, envelope, position) VALUES (?, ?, ?, ?)`,
		roomID, stimulusEventID, string(envelope), position)
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

// PendingClaims 实现 room.ClaimStore：按持久位升序（四轮复审 #14——
// 恢复重驱动顺序 = 刺激到达顺序，无序查询会反转同房间人类消息顺序）。
func (s *Store) PendingClaims(ctx context.Context) ([]room.StimulusClaim, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT room_id, stimulus_event_id, envelope, position FROM engine_claims ORDER BY position ASC, room_id ASC")
	if err != nil {
		return nil, fmt.Errorf("sqlite: query claims: %w", err)
	}
	defer rows.Close()
	var out []room.StimulusClaim
	for rows.Next() {
		var c room.StimulusClaim
		var raw string
		if err := rows.Scan(&c.RoomID, &c.StimulusEventID, &raw, &c.Position); err != nil {
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

// ReceiptsOf 实现 room.ReceiptLister：回执流水（最新在前；v1.70 展示对齐——
// "模型实际看到了什么"自正式面可查，不再只进 -dev 调试面）。
func (s *Store) ReceiptsOf(ctx context.Context, roomID string, limit int) ([]contextx.Receipt, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT receipt_id, room_id, task_id, watermark, layer_digests, created_at
		FROM context_receipts WHERE room_id = ? ORDER BY created_at DESC, receipt_id DESC LIMIT ?`,
		roomID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query receipts: %w", err)
	}
	defer rows.Close()
	out := []contextx.Receipt{}
	for rows.Next() {
		var r contextx.Receipt
		var digests string
		if err := rows.Scan(&r.ReceiptID, &r.RoomID, &r.TaskID, &r.Watermark, &digests, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan receipt: %w", err)
		}
		_ = json.Unmarshal([]byte(digests), &r.LayerDigests)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRoom 删除级联（M3-6，RFC-0010）：事务内清事件/outbox/回执/声明，并落
// 墓碑行——"曾存在"可审计、内容不可恢复。
func (s *Store) DeleteRoom(ctx context.Context, roomID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var reason string
	_ = tx.QueryRowContext(ctx,
		"SELECT json_extract(envelope, '$.payload.reason') FROM room_events WHERE room_id=? AND type='room.deleted' LIMIT 1",
		roomID).Scan(&reason)
	if _, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO room_tombstones(room_id, reason, deleted_at) VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
		roomID, reason); err != nil {
		return err
	}
	for _, table := range []string{"room_events", "outbox", "command_receipts", "engine_claims", "room_fts"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE room_id=?", roomID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ftsFieldsOf message.posted 信封 → 索引字段（正文/作者/线程）。
func ftsFieldsOf(env protocol.Envelope) (body, actor, actorKind, threadID string, ok bool) {
	var p struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(env.Payload, &p) != nil {
		return "", "", "", "", false
	}
	if env.ThreadID != nil {
		threadID = *env.ThreadID
	}
	return p.Body, env.Actor.ParticipantID, env.Actor.Kind, threadID, true
}

// SearchMessages 实现 room.MessageSearcher（M3-3 按需平面）：FTS5 trigram 生产
// 路径。语义与线性基准 room.SearchMessages 一致：子串、大小写不敏感、最新在前、
// limit 1..100 默认 20。≥3 字查询走 MATCH（phrase 引号包裹防语法注入；trigram
// 对 CJK/英文子串均正确——v1.46 spike 实证）；<3 字 trigram 不可用，回退 LIKE
// 子串。自愈：索引行数与事件数不符（旧库升级/历史缺行）即全量重建该房间索引。
func (s *Store) SearchMessages(ctx context.Context, roomID, query, actor, threadID string, limit int) ([]room.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []room.SearchHit{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if err := s.ensureFTS(ctx, roomID); err != nil {
		return nil, err
	}
	where := "room_id = ?"
	args := []any{roomID}
	if actor != "" {
		where += " AND actor = ?"
		args = append(args, actor)
	}
	if threadID != "" {
		where += " AND thread_id = ?"
		args = append(args, threadID)
	}
	// trigram tokenizer 要求查询 ≥3 字符（CJK 与 ASCII 同规）；短查询回退 LIKE
	// 子串（语义与线性基准一致——同为大小写不敏感子串）。
	if len([]rune(query)) >= 3 {
		phrase := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
		where += " AND room_fts MATCH ?"
		args = append(args, phrase)
	} else {
		where += " AND body LIKE ? COLLATE NOCASE"
		args = append(args, "%"+query+"%")
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, actor, actor_kind, thread_id, body, occurred_at, global_pos
		FROM room_fts WHERE `+where+`
		ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search: %w", err)
	}
	defer rows.Close()
	hits := []room.SearchHit{}
	for rows.Next() {
		var h room.SearchHit
		var threadID string
		var globalPos int64
		if err := rows.Scan(&h.EventID, &h.Actor, &h.ActorKind, &threadID, &h.Body, &h.OccurredAt, &globalPos); err != nil {
			return nil, fmt.Errorf("sqlite: scan search: %w", err)
		}
		if threadID != "" {
			t := threadID
			h.ThreadID = &t
		}
		h.Position = protocol.EncodeCursor(globalPos)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ensureFTS 索引自愈校验：FTS 行数 != 事件数（v3 前旧库/异常缺口）即全量重建
// 该房间索引（个人版房间规模毫秒级；重建后增量路径恢复）。
func (s *Store) ensureFTS(ctx context.Context, roomID string) error {
	var ftsCount, evtCount int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM room_fts WHERE room_id = ?", roomID).Scan(&ftsCount); err != nil {
		return fmt.Errorf("sqlite: fts count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM room_events WHERE room_id = ? AND type = ?", roomID, protocol.EventMessagePosted,
	).Scan(&evtCount); err != nil {
		return fmt.Errorf("sqlite: event count: %w", err)
	}
	if ftsCount == evtCount {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: fts rebuild begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM room_fts WHERE room_id = ?", roomID); err != nil {
		return fmt.Errorf("sqlite: fts rebuild clear: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, envelope, global_pos FROM room_events
		WHERE room_id = ? AND type = ? ORDER BY seq`, roomID, protocol.EventMessagePosted)
	if err != nil {
		return fmt.Errorf("sqlite: fts rebuild scan: %w", err)
	}
	type row struct {
		id, env string
		pos     int64
	}
	var batch []row
	for rows.Next() {
		var r row
		var raw string
		if err := rows.Scan(&r.id, &raw, &r.pos); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: fts rebuild row: %w", err)
		}
		r.env = raw
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: fts rebuild rows: %w", err)
	}
	for _, r := range batch {
		var env protocol.Envelope
		if json.Unmarshal([]byte(r.env), &env) != nil {
			continue
		}
		body, actor, actorKind, threadID, ok := ftsFieldsOf(env)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO room_fts (room_id, event_id, actor, actor_kind, thread_id, occurred_at, seq, global_pos, body)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			roomID, env.EventID, actor, actorKind, threadID, env.OccurredAt, env.Seq, r.pos, body); err != nil {
			return fmt.Errorf("sqlite: fts rebuild insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: fts rebuild commit: %w", err)
	}
	return nil
}

// ---- DocStore：per-doc 事件流（RFC-0014 / ADR-0014，appendTx 先例平移）----

// AppendDocEvents 实现 doc.DocStore：version 由存储按文档分配（调用方不指定）；
// 任一 event_id 重复则整批回滚。返回落库后的信封（含分配的 version），顺序与入参一致。
func (s *Store) AppendDocEvents(ctx context.Context, envelopes []protocol.DocEnvelope) ([]protocol.DocEnvelope, error) {
	return s.appendDocTx(ctx, envelopes, nil, nil)
}

// AppendDocEventsWithReceipt 实现 doc.DocStore：事件 + 幂等回执同事务原子落库；
// 事件 ID 或回执键冲突（并发同命令竞态的后到者）返回 doc.ErrDuplicateReceipt，整批回滚。
func (s *Store) AppendDocEventsWithReceipt(ctx context.Context, envelopes []protocol.DocEnvelope, receipt doc.CommandReceipt) ([]protocol.DocEnvelope, error) {
	return s.appendDocTx(ctx, envelopes, &receipt, nil)
}

// AppendDocEventsIf 实现 doc.DocCASStore：当前文档版本 != expected 即
// doc.ErrVersionConflict（BEGIN IMMEDIATE 临界区内判定），整批回滚。
func (s *Store) AppendDocEventsIf(ctx context.Context, envelopes []protocol.DocEnvelope, expectedDocVersion int64) ([]protocol.DocEnvelope, error) {
	return s.appendDocTx(ctx, envelopes, nil, &expectedDocVersion)
}

// appendDocTx 共享事务体：BEGIN IMMEDIATE → 分配 version → 写 doc_events +
// doc_outbox（+ doc.created/doc.revision_committed 文本入 doc_fts；+ 可选回执）→ COMMIT。
func (s *Store) appendDocTx(ctx context.Context, envelopes []protocol.DocEnvelope, receipt *doc.CommandReceipt, expectedVersion *int64) ([]protocol.DocEnvelope, error) {
	if len(envelopes) == 0 && receipt == nil {
		return nil, nil
	}
	docID := ""
	if len(envelopes) > 0 {
		docID = envelopes[0].DocID
		for i := range envelopes {
			if envelopes[i].DocID != docID {
				return nil, fmt.Errorf("sqlite: 一批事件必须同 doc（%s vs %s）", docID, envelopes[i].DocID)
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

	var maxVersion int64
	if docID != "" {
		if err := conn.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(version), 0) FROM doc_events WHERE doc_id = ?", docID,
		).Scan(&maxVersion); err != nil {
			return nil, fmt.Errorf("sqlite: read max version: %w", err)
		}
	}

	// CAS 期望版本（base_version）：与回执的乐观并发同在 BEGIN IMMEDIATE 临界区内判定
	if expectedVersion != nil && docID != "" && *expectedVersion != maxVersion {
		return nil, fmt.Errorf("%w: expected=%d current=%d",
			doc.ErrVersionConflict, *expectedVersion, maxVersion)
	}

	if receipt != nil && docID != "" {
		// 回执键先查（先于版本校验）：预检与提交之间的竞态重放优先于版本冲突
		var receiptExists bool
		if err := conn.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM doc_command_receipts
			WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind,
		).Scan(&receiptExists); err != nil {
			return nil, fmt.Errorf("sqlite: check receipt: %w", err)
		}
		if receiptExists {
			return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateReceipt, receipt.IdempotencyKey)
		}
		// 乐观并发在 BEGIN IMMEDIATE 临界区内强制（P-03：冲突判定在提交事务内）
		if receipt.ExpectedDocVersion != maxVersion {
			return nil, fmt.Errorf("%w: expected=%d current=%d",
				doc.ErrVersionConflict, receipt.ExpectedDocVersion, maxVersion)
		}
	}

	appended := make([]protocol.DocEnvelope, len(envelopes))
	for i := range envelopes {
		env := envelopes[i]
		maxVersion++
		env.Version = maxVersion
		raw, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("sqlite: marshal doc envelope %s: %w", env.EventID, err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO doc_events (event_id, tenant_id, doc_id, version, type, schema_version, occurred_at, envelope)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			env.EventID, env.TenantID, env.DocID, env.Version, env.Type, env.SchemaVersion, env.OccurredAt, string(raw),
		); err != nil {
			if isUniqueViolation(err) {
				if receipt != nil {
					// 回执式追加中事件撞车 = 并发同命令竞态后到者
					return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateReceipt, env.EventID)
				}
				return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateEvent, env.EventID)
			}
			return nil, fmt.Errorf("sqlite: insert doc event: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO doc_outbox (doc_id, version, event_id, envelope) VALUES (?, ?, ?, ?)`,
			env.DocID, env.Version, env.EventID, string(raw),
		); err != nil {
			return nil, fmt.Errorf("sqlite: insert doc outbox: %w", err)
		}
		// 全文索引（RFC-0014 §2.8：标题+正文派生）：doc.created 索标题、
		// doc.revision_committed 索本批 ops 文本，与事件账本同事务——
		// 索引缺行由 SearchDocs 的自愈校验兜底重建。
		if env.Type == protocol.EventDocCreated || env.Type == protocol.EventDocRevisionCommitted {
			if title, body, ok := docFTSFieldsOf(env); ok {
				if _, err := conn.ExecContext(ctx, `
					INSERT INTO doc_fts (doc_id, event_id, version, actor, occurred_at, title, body)
					VALUES (?, ?, ?, ?, ?, ?, ?)`,
					env.DocID, env.EventID, env.Version, env.Actor.ParticipantID, env.OccurredAt, title, body,
				); err != nil {
					return nil, fmt.Errorf("sqlite: insert doc fts: %w", err)
				}
			}
		}
		appended[i] = env
	}

	if receipt != nil {
		// DocVersion 权威回填：追加后的最新 version（调用方传入值不信任）
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO doc_command_receipts
			(tenant_id, idempotency_key, command_kind, doc_id, request_fingerprint, event_id, doc_version, executed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			receipt.TenantID, receipt.IdempotencyKey, receipt.CommandKind, receipt.DocID,
			receipt.RequestFingerprint, receipt.EventID, maxVersion, receipt.ExecutedAt,
		); err != nil {
			if isUniqueViolation(err) {
				return nil, fmt.Errorf("%w: %s", doc.ErrDuplicateReceipt, receipt.IdempotencyKey)
			}
			return nil, fmt.Errorf("sqlite: insert doc receipt: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return appended, nil
}

// LookupDocReceipt 实现 doc.DocStore：未命中返回 (nil, nil)。
func (s *Store) LookupDocReceipt(ctx context.Context, tenantID, idempotencyKey, commandKind string) (*doc.CommandReceipt, error) {
	rc := &doc.CommandReceipt{}
	err := s.db.QueryRowContext(ctx, `
		SELECT doc_id, request_fingerprint, event_id, doc_version, executed_at
		FROM doc_command_receipts
		WHERE tenant_id = ? AND idempotency_key = ? AND command_kind = ?`,
		tenantID, idempotencyKey, commandKind,
	).Scan(&rc.DocID, &rc.RequestFingerprint, &rc.EventID, &rc.DocVersion, &rc.ExecutedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: lookup doc receipt: %w", err)
	}
	rc.TenantID, rc.IdempotencyKey, rc.CommandKind = tenantID, idempotencyKey, commandKind
	return rc, nil
}

// DocVersion 实现 doc.DocStore：文档当前版本（最新 version；未见事件为 0）。
func (s *Store) DocVersion(ctx context.Context, docID string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM doc_events WHERE doc_id = ?", docID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: doc version: %w", err)
	}
	return v, nil
}

// DocExists 实现 doc.DocStore：是否已见 doc.created。
func (s *Store) DocExists(ctx context.Context, docID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM doc_events WHERE doc_id = ? AND type = ?)",
		docID, protocol.EventDocCreated).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: doc exists: %w", err)
	}
	return exists, nil
}

// DocEventsAfter 实现 doc.DocEventReader：从 cursor 之后按 version 续读某文档的事件
// （doc:{id} 频道续传与历史读共用——version 即 SSE cursor，ADR-0014）。
// limit ≤0 时取 100。返回下一游标；无更多事件时 next 为空串。
func (s *Store) DocEventsAfter(ctx context.Context, docID, cursor string, limit int) (events []doc.StoredDocEvent, next string, err error) {
	pos, err := protocol.DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT version, envelope FROM doc_events
		WHERE doc_id = ? AND version > ?
		ORDER BY version LIMIT ?`, docID, pos, limit)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: query doc events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		var raw string
		if err := rows.Scan(&version, &raw); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan doc event: %w", err)
		}
		var env protocol.DocEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return nil, "", fmt.Errorf("sqlite: unmarshal doc envelope %d: %w", version, err)
		}
		events = append(events, doc.StoredDocEvent{Envelope: env, Cursor: protocol.EncodeCursor(version)})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: rows: %w", err)
	}
	if len(events) == limit {
		next = events[len(events)-1].Cursor
	}
	return events, next, nil
}

// DeleteDoc 实现 doc.DocStore 删除级联（DeleteRoom 先例平移）：事务内清
// 事件/outbox/回执/FTS 索引，并落墓碑行——曾存在可审计、内容不可恢复。
func (s *Store) DeleteDoc(ctx context.Context, docID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var reason string
	_ = tx.QueryRowContext(ctx,
		"SELECT json_extract(envelope, '$.payload.reason') FROM doc_events WHERE doc_id=? AND type='doc.deleted' LIMIT 1",
		docID).Scan(&reason)
	if _, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO doc_tombstones(doc_id, reason, deleted_at) VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
		docID, reason); err != nil {
		return err
	}
	for _, table := range []string{"doc_events", "doc_outbox", "doc_command_receipts", "doc_fts"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE doc_id=?", docID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingDoc doc_outbox 待分发条目（房间 Pending 先例平移：按提交序取未分发）。
// Entry 结构复用：RoomID 字段承载 doc_id、GlobalPos 承载 doc version（version 即
// doc:{id} 频道 SSE cursor——分发消费者据此编码 position）。
func (s *Store) PendingDoc(ctx context.Context, limit int) ([]outbox.Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, doc_id, event_id, version, envelope FROM doc_outbox
		WHERE dispatched_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query doc outbox: %w", err)
	}
	defer rows.Close()
	var entries []outbox.Entry
	for rows.Next() {
		var e outbox.Entry
		var raw string
		if err := rows.Scan(&e.ID, &e.RoomID, &e.EventID, &e.GlobalPos, &raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan doc outbox: %w", err)
		}
		e.Envelope = []byte(raw)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// MarkDocDispatched doc_outbox 条目分发完成标记（幂等，房间 MarkDispatched 先例）。
func (s *Store) MarkDocDispatched(ctx context.Context, ids []int64) error {
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
		"UPDATE doc_outbox SET dispatched_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id IN ("+
			strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return fmt.Errorf("sqlite: mark doc dispatched: %w", err)
	}
	return nil
}

// DocOutboxStore doc_outbox → outbox.Store 端口适配（doc 分发器接线用；
// 条目字段语义见 PendingDoc 注记）。
type DocOutboxStore struct{ Store *Store }

// Pending 实现 outbox.Store。
func (a DocOutboxStore) Pending(ctx context.Context, limit int) ([]outbox.Entry, error) {
	return a.Store.PendingDoc(ctx, limit)
}

// MarkDispatched 实现 outbox.Store。
func (a DocOutboxStore) MarkDispatched(ctx context.Context, ids []int64) error {
	return a.Store.MarkDocDispatched(ctx, ids)
}

// ListExternalShares doc_external_shares 查询（extdocs.ShareStore 端口；
// docID/provider 空串 = 全量该维度，按 shared_at 倒序）。
func (s *Store) ListExternalShares(ctx context.Context, docID, provider string) ([]extdocs.Share, error) {
	q := "SELECT doc_id, provider, external_url, shared_by, shared_at FROM doc_external_shares"
	var conds []string
	var args []any
	if docID != "" {
		conds = append(conds, "doc_id=?")
		args = append(args, docID)
	}
	if provider != "" {
		conds = append(conds, "provider=?")
		args = append(args, provider)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY shared_at DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list external shares: %w", err)
	}
	defer rows.Close()
	var out []extdocs.Share
	for rows.Next() {
		var sh extdocs.Share
		if err := rows.Scan(&sh.DocID, &sh.Provider, &sh.ExternalURL, &sh.SharedBy, &sh.SharedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan external share: %w", err)
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// GetExternalShare 单条映射（不存在 = extdocs.ErrShareNotFound）。
func (s *Store) GetExternalShare(ctx context.Context, docID, provider string) (extdocs.Share, error) {
	var sh extdocs.Share
	err := s.db.QueryRowContext(ctx,
		"SELECT doc_id, provider, external_url, shared_by, shared_at FROM doc_external_shares WHERE doc_id=? AND provider=?",
		docID, provider).Scan(&sh.DocID, &sh.Provider, &sh.ExternalURL, &sh.SharedBy, &sh.SharedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return extdocs.Share{}, extdocs.ErrShareNotFound
	}
	if err != nil {
		return extdocs.Share{}, fmt.Errorf("sqlite: get external share: %w", err)
	}
	return sh, nil
}

// UpsertExternalShare 插入或替换映射（force 重共享 = 新 URL 覆盖旧值）。
func (s *Store) UpsertExternalShare(ctx context.Context, sh extdocs.Share) error {
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO doc_external_shares(doc_id, provider, external_url, shared_by, shared_at) VALUES(?,?,?,?,?) "+
			"ON CONFLICT(doc_id, provider) DO UPDATE SET external_url=excluded.external_url, shared_by=excluded.shared_by, shared_at=excluded.shared_at",
		sh.DocID, sh.Provider, sh.ExternalURL, sh.SharedBy, sh.SharedAt); err != nil {
		return fmt.Errorf("sqlite: upsert external share: %w", err)
	}
	return nil
}

// DeleteExternalShare 删除映射（行数 0 = ErrShareNotFound；不删远端文档）。
func (s *Store) DeleteExternalShare(ctx context.Context, docID, provider string) error {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM doc_external_shares WHERE doc_id=? AND provider=?", docID, provider)
	if err != nil {
		return fmt.Errorf("sqlite: delete external share: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return extdocs.ErrShareNotFound
	}
	return nil
}

// ---- sheet_transfers（RFC-0017 M2 extdocs.SheetTransferStore 端口）----

// transferColumns 全字段列序（INSERT/SELECT/Scan 共用）。
const transferColumns = "transfer_id, idempotency_key, direction, provider, status, doc_id, external_url, title, source_version, snapshot_hash, report_json, error, created_at, updated_at, cells_total, cells_written, state_json"

func scanTransfer(row interface{ Scan(...any) error }) (extdocs.SheetTransferRecord, error) {
	var r extdocs.SheetTransferRecord
	if err := row.Scan(&r.TransferID, &r.IdempotencyKey, &r.Direction, &r.Provider, &r.Status,
		&r.DocID, &r.ExternalURL, &r.Title, &r.SourceVersion, &r.SnapshotHash,
		&r.ReportJSON, &r.Error, &r.CreatedAt, &r.UpdatedAt,
		&r.CellsTotal, &r.CellsWritten, &r.StateJSON); err != nil {
		return extdocs.SheetTransferRecord{}, err
	}
	return r, nil
}

// InsertSheetTransfer 插入记录（幂等键被占 = extdocs.ErrTransferKeyConflict）。
func (s *Store) InsertSheetTransfer(ctx context.Context, rec extdocs.SheetTransferRecord) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sheet_transfers("+transferColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		rec.TransferID, rec.IdempotencyKey, rec.Direction, rec.Provider, rec.Status,
		rec.DocID, rec.ExternalURL, rec.Title, rec.SourceVersion, rec.SnapshotHash,
		rec.ReportJSON, rec.Error, rec.CreatedAt, rec.UpdatedAt,
		rec.CellsTotal, rec.CellsWritten, rec.StateJSON)
	if err != nil {
		if strings.Contains(err.Error(), "sheet_transfers.idempotency_key") {
			return extdocs.ErrTransferKeyConflict
		}
		return fmt.Errorf("sqlite: insert sheet transfer: %w", err)
	}
	return nil
}

// GetSheetTransfer 按 ID 取（不存在 = extdocs.ErrTransferNotFound）。
func (s *Store) GetSheetTransfer(ctx context.Context, transferID string) (extdocs.SheetTransferRecord, error) {
	r, err := scanTransfer(s.db.QueryRowContext(ctx,
		"SELECT "+transferColumns+" FROM sheet_transfers WHERE transfer_id=?", transferID))
	if errors.Is(err, sql.ErrNoRows) {
		return extdocs.SheetTransferRecord{}, extdocs.ErrTransferNotFound
	}
	if err != nil {
		return extdocs.SheetTransferRecord{}, fmt.Errorf("sqlite: get sheet transfer: %w", err)
	}
	return r, nil
}

// GetSheetTransferByKey 按幂等键取（不存在 = extdocs.ErrTransferNotFound）。
func (s *Store) GetSheetTransferByKey(ctx context.Context, idempotencyKey string) (extdocs.SheetTransferRecord, error) {
	r, err := scanTransfer(s.db.QueryRowContext(ctx,
		"SELECT "+transferColumns+" FROM sheet_transfers WHERE idempotency_key=?", idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return extdocs.SheetTransferRecord{}, extdocs.ErrTransferNotFound
	}
	if err != nil {
		return extdocs.SheetTransferRecord{}, fmt.Errorf("sqlite: get sheet transfer by key: %w", err)
	}
	return r, nil
}

// UpdateSheetTransfer 终态全量更新（按 transfer_id）。
func (s *Store) UpdateSheetTransfer(ctx context.Context, rec extdocs.SheetTransferRecord) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sheet_transfers SET direction=?, provider=?, status=?, doc_id=?, external_url=?, title=?, source_version=?, snapshot_hash=?, report_json=?, error=?, updated_at=?, cells_total=?, cells_written=?, state_json=? WHERE transfer_id=?",
		rec.Direction, rec.Provider, rec.Status, rec.DocID, rec.ExternalURL, rec.Title,
		rec.SourceVersion, rec.SnapshotHash, rec.ReportJSON, rec.Error, rec.UpdatedAt,
		rec.CellsTotal, rec.CellsWritten, rec.StateJSON, rec.TransferID)
	if err != nil {
		return fmt.Errorf("sqlite: update sheet transfer: %w", err)
	}
	return nil
}

// ListSheetTransfers 历史查询（docID/direction 空串 = 全量该维度；按
// created_at 倒序；limit ≤0 = 50）。
func (s *Store) ListSheetTransfers(ctx context.Context, docID, direction string, limit int) ([]extdocs.SheetTransferRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := "SELECT " + transferColumns + " FROM sheet_transfers"
	var conds []string
	var args []any
	if docID != "" {
		conds = append(conds, "doc_id=?")
		args = append(args, docID)
	}
	if direction != "" {
		conds = append(conds, "direction=?")
		args = append(args, direction)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sheet transfers: %w", err)
	}
	defer rows.Close()
	var out []extdocs.SheetTransferRecord
	for rows.Next() {
		r, err := scanTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan sheet transfer: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkStaleRunningTransfers 启动扫残留 running → unknown（崩溃窗口收敛；
// 返回迁移行数）。
func (s *Store) MarkStaleRunningTransfers(ctx context.Context, now string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE sheet_transfers SET status=?, error=?, updated_at=? WHERE status=?",
		extdocs.TransferUnknown, "执行中断（应用重启）——请在历史中核对目标后手动处理", now, extdocs.TransferRunning)
	if err != nil {
		return 0, fmt.Errorf("sqlite: mark stale transfers: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// docFTSFieldsOf 文档信封 → 索引字段（标题 / 本批 ops 文本）。
func docFTSFieldsOf(env protocol.DocEnvelope) (title, body string, ok bool) {
	switch env.Type {
	case protocol.EventDocCreated:
		var p protocol.DocCreatedPayload
		if json.Unmarshal(env.Payload, &p) != nil {
			return "", "", false
		}
		return p.Title, "", true
	case protocol.EventDocRevisionCommitted:
		var p protocol.DocRevisionCommittedPayload
		if json.Unmarshal(env.Payload, &p) != nil {
			return "", "", false
		}
		var sb strings.Builder
		for _, op := range p.Ops {
			if op.Block != nil && op.Block.Text != "" {
				sb.WriteString(op.Block.Text)
				sb.WriteString("\n")
			}
			if op.Text != "" {
				sb.WriteString(op.Text)
				sb.WriteString("\n")
			}
		}
		return "", sb.String(), true
	}
	return "", "", false
}

// SearchDocs 实现 doc.DocSearcher（RFC-0014 §2.8 文档全文搜索）：FTS5 trigram
// 生产路径，语义与 SearchMessages 同规——子串、大小写不敏感、limit 1..100 默认 20；
// ≥3 字查询走 MATCH（phrase 引号包裹防语法注入），<3 字回退 LIKE 子串（标题或正文）。
// 每文档只取最新命中版本（同一文档的多条索引行归并，文档主页语义）。
// 自愈：索引行数与可索引事件数不符（旧库升级/历史缺行）即全量重建索引。
func (s *Store) SearchDocs(ctx context.Context, query string, limit int) ([]doc.DocSearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []doc.DocSearchHit{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if err := s.ensureDocFTS(ctx); err != nil {
		return nil, err
	}
	var where string
	var args []any
	// trigram tokenizer 要求查询 ≥3 字符（CJK 与 ASCII 同规）；短查询回退 LIKE 子串。
	if len([]rune(query)) >= 3 {
		phrase := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
		where = "doc_fts MATCH ?"
		args = append(args, phrase)
	} else {
		where = "(title LIKE ? COLLATE NOCASE OR body LIKE ? COLLATE NOCASE)"
		args = append(args, "%"+query+"%", "%"+query+"%")
	}
	args = append(args, limit)
	// SQLite 的 MAX() 聚合语义：裸列取自最大 version 行（每文档归并为最新命中）。
	rows, err := s.db.QueryContext(ctx, `
		SELECT doc_id, event_id, MAX(version) AS version, actor, occurred_at, title, body
		FROM doc_fts WHERE `+where+`
		GROUP BY doc_id ORDER BY version DESC, doc_id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search docs: %w", err)
	}
	defer rows.Close()
	hits := []doc.DocSearchHit{}
	for rows.Next() {
		var h doc.DocSearchHit
		if err := rows.Scan(&h.DocID, &h.EventID, &h.Version, &h.Actor, &h.OccurredAt, &h.Title, &h.Body); err != nil {
			return nil, fmt.Errorf("sqlite: scan doc search: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// ListDocs 实现 doc.DocLister（RFC-0014 §2.8 文档主页）：每文档聚合成摘要行——
// 标题取最新 created/renamed、状态取最新生命周期事件、更新者/时间取最新版本行。
// createdBy 空串 = 全部；删除级联后 doc_events 无行自然不出现。updated_at 倒序。
func (s *Store) ListDocs(ctx context.Context, createdBy string) ([]doc.DocSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			d.doc_id,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.title') FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type IN ('doc.created', 'doc.renamed')
				ORDER BY e.version DESC LIMIT 1), '') AS title,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.format') FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
				ORDER BY e.version ASC LIMIT 1), 'markdown') AS format,
			(SELECT MAX(e.version) FROM doc_events e WHERE e.doc_id = d.doc_id) AS version,
			COALESCE((SELECT CASE WHEN e.type = 'doc.archived' THEN 'archived' ELSE 'active' END
				FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type IN ('doc.archived', 'doc.restored')
				ORDER BY e.version DESC LIMIT 1), 'active') AS status,
			COALESCE((SELECT json_extract(e.envelope, '$.payload.created_by') FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
				ORDER BY e.version ASC LIMIT 1), '') AS created_by,
			COALESCE((SELECT e.occurred_at FROM doc_events e
				WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
				ORDER BY e.version ASC LIMIT 1), '') AS created_at,
			COALESCE((SELECT json_extract(e.envelope, '$.actor.participant_id') FROM doc_events e
				WHERE e.doc_id = d.doc_id
				ORDER BY e.version DESC LIMIT 1), '') AS updated_by,
			COALESCE((SELECT e.occurred_at FROM doc_events e
				WHERE e.doc_id = d.doc_id
				ORDER BY e.version DESC LIMIT 1), '') AS updated_at
		FROM (SELECT DISTINCT doc_id FROM doc_events) d
		WHERE (? = '' OR COALESCE((SELECT json_extract(e.envelope, '$.payload.created_by') FROM doc_events e
			WHERE e.doc_id = d.doc_id AND e.type = 'doc.created'
			ORDER BY e.version ASC LIMIT 1), '') = ?)
		ORDER BY updated_at DESC, d.doc_id ASC`, createdBy, createdBy)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list docs: %w", err)
	}
	defer rows.Close()
	out := []doc.DocSummary{}
	for rows.Next() {
		var sum doc.DocSummary
		if err := rows.Scan(&sum.DocID, &sum.Title, &sum.Format, &sum.Version, &sum.Status,
			&sum.CreatedBy, &sum.CreatedAt, &sum.UpdatedBy, &sum.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan doc summary: %w", err)
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// ensureDocFTS 索引自愈校验：FTS 行数 != 可索引事件数（v4 前旧库/异常缺口）即
// 全量重建索引（个人版文档规模毫秒级；重建后增量路径恢复）。
func (s *Store) ensureDocFTS(ctx context.Context) error {
	var ftsCount, evtCount int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM doc_fts").Scan(&ftsCount); err != nil {
		return fmt.Errorf("sqlite: doc fts count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM doc_events WHERE type IN (?, ?)",
		protocol.EventDocCreated, protocol.EventDocRevisionCommitted,
	).Scan(&evtCount); err != nil {
		return fmt.Errorf("sqlite: doc event count: %w", err)
	}
	if ftsCount == evtCount {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM doc_fts"); err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild clear: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT envelope FROM doc_events
		WHERE type IN (?, ?) ORDER BY doc_id, version`,
		protocol.EventDocCreated, protocol.EventDocRevisionCommitted)
	if err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild scan: %w", err)
	}
	var batch []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: doc fts rebuild row: %w", err)
		}
		batch = append(batch, raw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild rows: %w", err)
	}
	for _, raw := range batch {
		var env protocol.DocEnvelope
		if json.Unmarshal([]byte(raw), &env) != nil {
			continue
		}
		title, body, ok := docFTSFieldsOf(env)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO doc_fts (doc_id, event_id, version, actor, occurred_at, title, body)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			env.DocID, env.EventID, env.Version, env.Actor.ParticipantID, env.OccurredAt, title, body); err != nil {
			return fmt.Errorf("sqlite: doc fts rebuild insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: doc fts rebuild commit: %w", err)
	}
	return nil
}

// ---- Agent 身份域（RFC-0018 / ADR-0017，0.5.0；ADR-0018，0.5.1；端口实现）----

// identityExec 临界区内执行面（*sql.Conn / *sql.Tx 同构消费）。
type identityExec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// identityWriteTx 身份域写临界区（P1 修复，0.5.1 审校）：BEGIN IMMEDIATE——
// 状态/归属读取在写锁内进行，杜绝 WAL 下"读后升级"撞 SQLITE_BUSY_SNAPSHOT
// 把并发冲突外泄成 500（败者应按端口语义返回 ok=false/409）。等待发生在
// BEGIN 取锁处，由 busy_timeout 承担（与房间 appendTx 同机制）。
func (s *Store) identityWriteTx(ctx context.Context, fn func(q identityExec) error) error {
	conn, err := s.db.Conn(ctx)
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
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	commit = true
	return nil
}

// appendAgentEvent 事件落流（临界区内调用）。
func appendAgentEvent(ctx context.Context, q identityExec, agentID, eventType string, payload map[string]string, occurredAt string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sqlite: event payload: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
INSERT INTO agent_events (agent_id, event_type, payload, occurred_at) VALUES (?, ?, ?, ?)`,
		agentID, eventType, string(raw), occurredAt); err != nil {
		return fmt.Errorf("sqlite: append %s: %w", eventType, err)
	}
	return nil
}

// appendAgentCreated 建 Agent 行的 created 事实落流（临界区内调用）。
func appendAgentCreated(ctx context.Context, q identityExec, a personalagent.Agent) error {
	return appendAgentEvent(ctx, q, a.AgentID, personalagent.EventCreated, map[string]string{
		"source": a.Source, "adapter": a.Adapter, "display_name": a.DisplayName,
	}, a.CreatedAt)
}

// EnsureAgent 幂等建 Agent（按 agent_id）；新建时 created 事件同事务落库。
func (s *Store) EnsureAgent(ctx context.Context, a personalagent.Agent) (bool, error) {
	var created bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		res, err := q.ExecContext(ctx, `
INSERT OR IGNORE INTO agents (agent_id, display_name, adapter, status, source, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
			a.AgentID, a.DisplayName, a.Adapter, a.Status, a.Source, a.CreatedAt)
		if err != nil {
			return fmt.Errorf("sqlite: ensure agent: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("sqlite: ensure agent rows: %w", err)
		}
		created = n > 0
		if created {
			return appendAgentCreated(ctx, q, a)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// EnsureAgentWithAlias 原子 get-or-create（审校 P1#1）：建 Agent 行（含 created
// 事件）与别名认领同一 BEGIN IMMEDIATE 事务——别名已被占（含并发竞争）时回滚
// 候选行并回读既有归属者；成功则三行同落。任一时点库内要么"整行落库"要么
// "什么都不留"。
func (s *Store) EnsureAgentWithAlias(ctx context.Context, a personalagent.Agent, kind personalagent.AliasKind, value string) (string, bool, error) {
	var owner string
	var created bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx,
			`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
			Scan(&owner)
		switch {
		case err == nil:
			return nil // 别名已被占（临界区内读到即最终态）：回读归属者
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("sqlite: identity claim probe: %w", err)
		}
		res, err := q.ExecContext(ctx, `
INSERT OR IGNORE INTO agents (agent_id, display_name, adapter, status, source, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
			a.AgentID, a.DisplayName, a.Adapter, a.Status, a.Source, a.CreatedAt)
		if err != nil {
			return fmt.Errorf("sqlite: identity claim agent: %w", err)
		}
		createdN, _ := res.RowsAffected()
		created = createdN > 0
		if created { // 新建行才落 created 事实（同 agent_id 重放不重复记）
			if err := appendAgentCreated(ctx, q, a); err != nil {
				return err
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := q.ExecContext(ctx, `
INSERT INTO agent_aliases (alias_kind, alias_value, agent_id, created_at) VALUES (?, ?, ?, ?)`,
			string(kind), value, a.AgentID, now); err != nil {
			// 防御兜底（BEGIN IMMEDIATE 下正常不可达）：唯一约束碰撞回读胜者
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				var winner string
				if qerr := q.QueryRowContext(ctx,
					`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
					Scan(&winner); qerr == nil {
					owner, created = winner, false
					return nil
				}
			}
			return fmt.Errorf("sqlite: identity claim alias: %w", err)
		}
		owner = a.AgentID
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return owner, created, nil
}

// AgentByID 单查 Agent 行。
func (s *Store) AgentByID(ctx context.Context, agentID string) (personalagent.Agent, bool, error) {
	var a personalagent.Agent
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id, display_name, adapter, status, source, created_at FROM agents WHERE agent_id = ?`, agentID).
		Scan(&a.AgentID, &a.DisplayName, &a.Adapter, &a.Status, &a.Source, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return personalagent.Agent{}, false, nil
	}
	if err != nil {
		return personalagent.Agent{}, false, fmt.Errorf("sqlite: agent by id: %w", err)
	}
	return a, true, nil
}

// TransitionAgent 事务转移（ADR-0018 §2；P1 修复：状态读取在 BEGIN IMMEDIATE
// 写临界区内）：读状态（allowedFrom 校验）→ 更新 agents 投影 → 追加
// status_changed 事件，同一事务。行缺失 ErrAgentNotFound；状态不匹配 ok=false
// （未动投影未落事件——并发败者按 409 语义返回，不外泄锁冲突）。
func (s *Store) TransitionAgent(ctx context.Context, agentID string, allowedFrom []string, to, reason, occurredAt string) (string, bool, error) {
	var from string
	var ok bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx, `SELECT status FROM agents WHERE agent_id = ?`, agentID).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return personalagent.ErrAgentNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: transition probe: %w", err)
		}
		if !allowedFromHas(allowedFrom, from) {
			return nil // ok=false：状态不匹配（含并发败者）
		}
		if _, err := q.ExecContext(ctx, `UPDATE agents SET status = ? WHERE agent_id = ?`, to, agentID); err != nil {
			return fmt.Errorf("sqlite: transition project: %w", err)
		}
		if err := appendAgentEvent(ctx, q, agentID, personalagent.EventStatusChanged,
			map[string]string{"from": from, "to": to, "reason": reason}, occurredAt); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return from, ok, nil
}

// RetireAgent 原子退役（P2#4 修复，0.5.1 审校）：retiring 与 retired 两段
// status_changed 事件及投影终态在同一 BEGIN IMMEDIATE 事务内一次提交——
// 中途取消/失败整体回滚，不滞留 retiring。allowedFrom = {active, suspended}
// （已处 retiring 的中断重试走 TransitionAgent 只补终段，不重复记 retiring）。
func (s *Store) RetireAgent(ctx context.Context, agentID string, allowedFrom []string, reason, occurredAt string) (string, bool, error) {
	var from string
	var ok bool
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		err := q.QueryRowContext(ctx, `SELECT status FROM agents WHERE agent_id = ?`, agentID).Scan(&from)
		if errors.Is(err, sql.ErrNoRows) {
			return personalagent.ErrAgentNotFound
		}
		if err != nil {
			return fmt.Errorf("sqlite: retire probe: %w", err)
		}
		if !allowedFromHas(allowedFrom, from) {
			return nil
		}
		if err := appendAgentEvent(ctx, q, agentID, personalagent.EventStatusChanged,
			map[string]string{"from": from, "to": personalagent.StatusRetiring, "reason": reason}, occurredAt); err != nil {
			return err
		}
		if err := appendAgentEvent(ctx, q, agentID, personalagent.EventStatusChanged,
			map[string]string{"from": personalagent.StatusRetiring, "to": personalagent.StatusRetired, "reason": reason}, occurredAt); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `UPDATE agents SET status = ? WHERE agent_id = ?`, personalagent.StatusRetired, agentID); err != nil {
			return fmt.Errorf("sqlite: retire project: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return from, ok, nil
}

// allowedFromHas 来源集合判定。
func allowedFromHas(allowedFrom []string, status string) bool {
	for _, f := range allowedFrom {
		if f == status {
			return true
		}
	}
	return false
}

// AgentEvents 某 Agent 的事件流（seq 升序）。
func (s *Store) AgentEvents(ctx context.Context, agentID string) ([]personalagent.AgentEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, agent_id, event_type, payload, occurred_at FROM agent_events WHERE agent_id = ? ORDER BY seq`, agentID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: agent events: %w", err)
	}
	defer rows.Close()
	out := []personalagent.AgentEvent{}
	for rows.Next() {
		var ev personalagent.AgentEvent
		var payload string
		if err := rows.Scan(&ev.Seq, &ev.AgentID, &ev.EventType, &payload, &ev.OccurredAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListAgents 全量 Agent，按创建序（rowid = 首次同步的确定性顺序）。
func (s *Store) ListAgents(ctx context.Context) ([]personalagent.Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT agent_id, display_name, adapter, status, source, created_at FROM agents ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list agents: %w", err)
	}
	defer rows.Close()
	out := []personalagent.Agent{}
	for rows.Next() {
		var a personalagent.Agent
		if err := rows.Scan(&a.AgentID, &a.DisplayName, &a.Adapter, &a.Status, &a.Source, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan agent: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentByAlias 别名解析；未登记 ok=false。
func (s *Store) AgentByAlias(ctx context.Context, kind personalagent.AliasKind, value string) (string, bool, error) {
	var agentID string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
		Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: agent by alias: %w", err)
	}
	return agentID, true, nil
}

// AddAlias 幂等登记别名：同键同目标 no-op（added=false）；同键异目标
// personalagent.ErrAliasConflict。先查后插（单进程写串行化下安全），唯一
// 主键为并发兜底——撞约束时回读归属定性。
func (s *Store) AddAlias(ctx context.Context, kind personalagent.AliasKind, value, agentID string) (bool, error) {
	var owner string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
		Scan(&owner)
	switch {
	case err == nil:
		if owner != agentID {
			return false, personalagent.ErrAliasConflict
		}
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("sqlite: alias owner: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO agent_aliases (alias_kind, alias_value, agent_id, created_at) VALUES (?, ?, ?, ?)`,
		string(kind), value, agentID, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			// 兜底路径：回读归属定性（同目标 = 幂等，异目标 = 冲突）
			var owner2 string
			if qerr := s.db.QueryRowContext(ctx,
				`SELECT agent_id FROM agent_aliases WHERE alias_kind = ? AND alias_value = ?`, string(kind), value).
				Scan(&owner2); qerr == nil && owner2 != agentID {
				return false, personalagent.ErrAliasConflict
			}
			return false, nil
		}
		return false, fmt.Errorf("sqlite: add alias: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AliasesOf 某 Agent 的全部别名行（登记序）。
func (s *Store) AliasesOf(ctx context.Context, agentID string) ([]personalagent.Alias, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT alias_kind, alias_value FROM agent_aliases WHERE agent_id = ? ORDER BY rowid`, agentID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: aliases of: %w", err)
	}
	defer rows.Close()
	out := []personalagent.Alias{}
	for rows.Next() {
		var kind string
		var a personalagent.Alias
		if err := rows.Scan(&kind, &a.Value); err != nil {
			return nil, fmt.Errorf("sqlite: scan alias: %w", err)
		}
		a.Kind = personalagent.AliasKind(kind)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- 职责模板域（RFC-0018 §5.1 / ADR-0019，0.5.2；v9 表，端口实现）----

const slotSpecCols = `slot_spec_id, version, schema_version, name, responsibility, content_json, created_at`

func scanSlotSpec(row interface{ Scan(...any) error }) (slot.SlotSpec, error) {
	var s slot.SlotSpec
	var content string
	if err := row.Scan(&s.SlotSpecID, &s.Version, &s.SchemaVersion, &s.Name, &s.Responsibility, &content, &s.CreatedAt); err != nil {
		return slot.SlotSpec{}, err
	}
	if err := json.Unmarshal([]byte(content), &s.SpecContent); err != nil {
		return slot.SlotSpec{}, fmt.Errorf("sqlite: slot content unmarshal: %w", err)
	}
	return s, nil
}

// CreateSpec 建谱系（首版）：谱系已存在 ErrSpecExists（行级探测在 BEGIN
// IMMEDIATE 临界区内——并发同建恰一胜者）。
func (s *Store) CreateSpec(ctx context.Context, spec slot.SlotSpec) (slot.SlotSpec, error) {
	raw, err := json.Marshal(spec.SpecContent)
	if err != nil {
		return slot.SlotSpec{}, fmt.Errorf("sqlite: slot content marshal: %w", err)
	}
	err = s.identityWriteTx(ctx, func(q identityExec) error {
		var exists int
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM slot_specs WHERE slot_spec_id = ?)`, spec.SlotSpecID).
			Scan(&exists); err != nil {
			return fmt.Errorf("sqlite: slot spec probe: %w", err)
		}
		if exists == 1 {
			return slot.ErrSpecExists
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_specs (`+slotSpecCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			spec.SlotSpecID, spec.Version, spec.SchemaVersion, spec.Name,
			spec.Responsibility, string(raw), spec.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: create slot spec: %w", err)
		}
		return nil
	})
	if err != nil {
		return slot.SlotSpec{}, err
	}
	return spec, nil
}

// AppendSpecVersion 追加下一不可变版本：latest+1 取号在 BEGIN IMMEDIATE
// 临界区内（并发追加串行化为连续版本，无空洞无重复）。
func (s *Store) AppendVersion(ctx context.Context, slotSpecID string, content slot.SpecContent, now string) (slot.SlotSpec, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return slot.SlotSpec{}, fmt.Errorf("sqlite: slot content marshal: %w", err)
	}
	out := slot.SlotSpec{}
	err = s.identityWriteTx(ctx, func(q identityExec) error {
		var latest int
		err := q.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM slot_specs WHERE slot_spec_id = ?`, slotSpecID).
			Scan(&latest)
		if err != nil {
			return fmt.Errorf("sqlite: slot latest: %w", err)
		}
		if latest == 0 {
			return slot.ErrNotFound
		}
		out = slot.SlotSpec{
			SlotSpecID: slotSpecID, Version: latest + 1, SchemaVersion: slot.SchemaVersion,
			SpecContent: content, CreatedAt: now,
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_specs (`+slotSpecCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			out.SlotSpecID, out.Version, out.SchemaVersion, out.Name,
			out.Responsibility, string(raw), out.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: append slot version: %w", err)
		}
		return nil
	})
	if err != nil {
		return slot.SlotSpec{}, err
	}
	return out, nil
}

// ListSummaries 谱系摘要（首版创建序）。
func (s *Store) ListSummaries(ctx context.Context) ([]slot.SpecSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT slot_spec_id,
       (SELECT name FROM slot_specs t WHERE t.slot_spec_id = s.slot_spec_id ORDER BY version DESC LIMIT 1),
       MAX(version) AS latest_version,
       COUNT(*) AS version_count,
       MIN(created_at) AS created_at,
       MAX(created_at) AS updated_at
  FROM slot_specs s
 GROUP BY slot_spec_id
 ORDER BY MIN(rowid)`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: slot summaries: %w", err)
	}
	defer rows.Close()
	out := []slot.SpecSummary{}
	for rows.Next() {
		var sum slot.SpecSummary
		if err := rows.Scan(&sum.SlotSpecID, &sum.Name, &sum.LatestVersion,
			&sum.VersionCount, &sum.CreatedAt, &sum.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan slot summary: %w", err)
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// Latest 最新版本；谱系未知 ok=false。
func (s *Store) Latest(ctx context.Context, slotSpecID string) (slot.SlotSpec, bool, error) {
	var s0 slot.SlotSpec
	var content string
	err := s.db.QueryRowContext(ctx,
		`SELECT `+slotSpecCols+` FROM slot_specs WHERE slot_spec_id = ? ORDER BY version DESC LIMIT 1`, slotSpecID).
		Scan(&s0.SlotSpecID, &s0.Version, &s0.SchemaVersion, &s0.Name, &s0.Responsibility, &content, &s0.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.SlotSpec{}, false, nil
	}
	if err != nil {
		return slot.SlotSpec{}, false, fmt.Errorf("sqlite: slot latest: %w", err)
	}
	if err := json.Unmarshal([]byte(content), &s0.SpecContent); err != nil {
		return slot.SlotSpec{}, false, fmt.Errorf("sqlite: slot content unmarshal: %w", err)
	}
	return s0, true, nil
}

// Versions 全版本升序；谱系未知 ok=false。
func (s *Store) Versions(ctx context.Context, slotSpecID string) ([]slot.SlotSpec, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+slotSpecCols+` FROM slot_specs WHERE slot_spec_id = ? ORDER BY version`, slotSpecID)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: slot versions: %w", err)
	}
	defer rows.Close()
	out := []slot.SlotSpec{}
	for rows.Next() {
		spec, err := scanSlotSpec(rows)
		if err != nil {
			return nil, false, fmt.Errorf("sqlite: scan slot spec: %w", err)
		}
		out = append(out, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, true, nil
}

// ---- 职责实例域（RFC-0018 §3.1/§5.2 / ADR-0020，0.5.3；v10 表，端口实现）----

// CreateInstance 建职责实例（knowledge_namespace UNIQUE 兜底生成器碰撞）。
func (s *Store) CreateInstance(ctx context.Context, inst slot.SlotInstance) error {
	err := s.identityWriteTx(ctx, func(q identityExec) error {
		if _, err := q.ExecContext(ctx, `
INSERT INTO slot_instances (slot_instance_id, slot_spec_id, scope, title, knowledge_namespace, created_from_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			inst.SlotInstanceID, inst.SlotSpecID, inst.Scope, inst.Title,
			inst.KnowledgeNamespace, inst.CreatedFromVersion, inst.CreatedAt); err != nil {
			return fmt.Errorf("sqlite: create slot instance: %w", err)
		}
		return nil
	})
	return err
}

// ListInstances 实例全量（创建序）。
func (s *Store) ListInstances(ctx context.Context) ([]slot.SlotInstance, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT slot_instance_id, slot_spec_id, scope, title, knowledge_namespace, created_from_version, created_at
  FROM slot_instances ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list slot instances: %w", err)
	}
	defer rows.Close()
	out := []slot.SlotInstance{}
	for rows.Next() {
		var in slot.SlotInstance
		if err := rows.Scan(&in.SlotInstanceID, &in.SlotSpecID, &in.Scope, &in.Title,
			&in.KnowledgeNamespace, &in.CreatedFromVersion, &in.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan slot instance: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// InstanceByID 单查实例；未知 ok=false。
func (s *Store) InstanceByID(ctx context.Context, slotInstanceID string) (slot.SlotInstance, bool, error) {
	var in slot.SlotInstance
	err := s.db.QueryRowContext(ctx, `
SELECT slot_instance_id, slot_spec_id, scope, title, knowledge_namespace, created_from_version, created_at
  FROM slot_instances WHERE slot_instance_id = ?`, slotInstanceID).
		Scan(&in.SlotInstanceID, &in.SlotSpecID, &in.Scope, &in.Title,
			&in.KnowledgeNamespace, &in.CreatedFromVersion, &in.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return slot.SlotInstance{}, false, nil
	}
	if err != nil {
		return slot.SlotInstance{}, false, fmt.Errorf("sqlite: slot instance by id: %w", err)
	}
	return in, true, nil
}

// LatestByLineage 各谱系最新版聚合（实例列表的现势连接单源）。
func (s *Store) LatestByLineage(ctx context.Context) (map[string]slot.InstanceSpecNow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT slot_spec_id, name, version FROM slot_specs
 WHERE version = (SELECT MAX(version) FROM slot_specs t WHERE t.slot_spec_id = slot_specs.slot_spec_id)`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: slot latest by lineage: %w", err)
	}
	defer rows.Close()
	out := map[string]slot.InstanceSpecNow{}
	for rows.Next() {
		var id, name string
		var version int
		if err := rows.Scan(&id, &name, &version); err != nil {
			return nil, fmt.Errorf("sqlite: scan latest by lineage: %w", err)
		}
		out[id] = slot.InstanceSpecNow{Name: name, LatestVersion: version}
	}
	return out, rows.Err()
}
