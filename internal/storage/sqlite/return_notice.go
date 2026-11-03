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

// migrationV28（0.6.11 / ADR-0034）：重开通知快照列——decision_ref（受影响决定
// 引用）与 affected_count（受影响工作项数）。逐语句加列（重复容忍）。
var migrationV28 = []string{
	"ALTER TABLE mission_return_notices ADD COLUMN decision_ref TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE mission_return_notices ADD COLUMN affected_count INTEGER NOT NULL DEFAULT 0",
	"INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (28, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
}

// migrationV27（0.6.10 四审·既有补正）：老式 `to=returned` 退回事件（0.6.9
// 初版形态——#10 审校前 to 即 returned，v0.2 起才为 to=ready+decision=returned）
// 此前不被 classify 识别，且游标已越过——重置投影游标使下一轮全量重扫：
// INSERT OR IGNORE 按 source_seq 幂等（已投行含状态不动，漏行补出；notice_id
// 同式派生）。迁移语句每次 Open 都会执行（项目惯例）——以登记守卫使重置只在**首次**发生，
// 避免每次启动全量重扫；崩溃窗口（UPDATE 后登记前）最多多重置一次，幂等无害。
const migrationV27 = `
UPDATE mission_return_state SET last_seq = 0
 WHERE id = 1 AND NOT EXISTS (SELECT 1 FROM migrations WHERE version = 27);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (27, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// migrationV26（0.6.10 审校 #8）：升级前已删房的来源墓碑回填——按
// room_tombstones 原删除时间补 mission_sources.deleted_at（幂等 UPDATE，v24
// 数据补正先例；覆盖 v25 加列之前的历史删除：v0.6.9 删房不触碰 mission_sources）。
const migrationV26 = `
UPDATE mission_sources
   SET deleted_at = (SELECT t.deleted_at FROM room_tombstones t WHERE t.room_id = mission_sources.room_id)
 WHERE deleted_at IS NULL
   AND EXISTS (SELECT 1 FROM room_tombstones t WHERE t.room_id = mission_sources.room_id);
INSERT OR IGNORE INTO migrations (version, applied_at) VALUES (26, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

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
	// 0.6.10 审校 #1/#5：新事件的送验/裁定事务内核对后写入（InjectReviewFacts /
	// 服务层载荷）——旧事件无此字段，由投影按事件流重建（见 versionAtEvent 等）。
	ContractVersion int `json:"contract_version"`
}

// versionAtEvent 来源事件时点生效的契约版本（0.6.10 审校 #1）：created /
// contract_revised 流内回溯（seq ≤ 来源 seq 的最近一条；revised 取 to_version）。
func versionAtEvent(ctx context.Context, conn *sql.Conn, missionID string, seq int64) int {
	var v int
	err := conn.QueryRowContext(ctx, `
		SELECT CASE event_type WHEN 'created' THEN json_extract(payload, '$.contract_version')
		                          ELSE json_extract(payload, '$.to_version') END
		FROM mission_events
		WHERE mission_id = ? AND seq <= ? AND event_type IN ('created', 'contract_revised')
		ORDER BY seq DESC LIMIT 1`, missionID, seq).Scan(&v)
	if err != nil {
		return 0
	}
	return v
}

// objectiveAtVersion 版本行目标快照（mission_contract_versions 不可变 content）。
// found = 版本行存在——快照定格：即使摘要为空（如 v1 目标仅 BOM，判空口径下为空）
// 也不回退 missions 当前行（0.6.10 复审 #6：历史卡不得写成修订后的目标）。
func objectiveAtVersion(ctx context.Context, conn *sql.Conn, missionID string, version int) (string, bool) {
	if version <= 0 {
		return "", false
	}
	var content string
	if err := conn.QueryRowContext(ctx,
		`SELECT content FROM mission_contract_versions WHERE mission_id = ? AND version = ?`,
		missionID, version).Scan(&content); err != nil {
		return "", false
	}
	var c struct {
		Objective string `json:"objective"`
	}
	_ = json.Unmarshal([]byte(content), &c)
	return truncateRunes(c.Objective, 80), true
}

// roundCalibratingDecision 送验缺轮次的区间校准（0.6.10 四审 #2）：同包、
// 本次送验之后、下一次送验之前最近一条**显式轮次**的裁定——v22→v24 升级库的
// 裁定带迁移后的显式值（如 1），而送验事件仍无轮次（按全历史计数会得 3/4，
// 与其裁定错位）。同包重复送验（区间内无裁定）返回 0——回退计数。
func roundCalibratingDecision(ctx context.Context, conn *sql.Conn, missionID string, seq int64, packageID string) int {
	args := []any{missionID, seq, packageID}
	nextBoundary := ""
	if packageID == "" {
		nextBoundary = ` AND NOT EXISTS (SELECT 1 FROM mission_events nx WHERE nx.mission_id = e.mission_id AND nx.seq > ? AND nx.seq < e.seq AND nx.event_type = 'status_changed' AND json_extract(nx.payload, '$.to') = 'reviewing')`
		args = append(args, seq)
	} else {
		nextBoundary = ` AND NOT EXISTS (SELECT 1 FROM mission_events nx WHERE nx.mission_id = e.mission_id AND nx.seq > ? AND nx.seq < e.seq AND nx.event_type = 'status_changed' AND json_extract(nx.payload, '$.to') = 'reviewing' AND COALESCE(json_extract(nx.payload, '$.package_id'), '') = ?)`
		args = append(args, seq, packageID)
	}
	var round int
	err := conn.QueryRowContext(ctx, `
		SELECT COALESCE(json_extract(e.payload, '$.review_round'), 0) FROM mission_events e
		WHERE e.mission_id = ? AND e.seq > ? AND e.event_type = 'status_changed'
		  AND COALESCE(json_extract(e.payload, '$.package_id'), '') = COALESCE(?, '')
		  AND (json_extract(e.payload, '$.to') IN ('accepted', 'returned')
		       OR (json_extract(e.payload, '$.to') = 'ready' AND json_extract(e.payload, '$.decision') = 'returned'))
		  AND COALESCE(json_extract(e.payload, '$.review_round'), 0) > 0`+nextBoundary+`
		ORDER BY e.seq DESC LIMIT 1`, args...).Scan(&round)
	if err != nil {
		return 0
	}
	return round
}

// roundOfPrecedingRequest 裁定（returned/accepted）轮次还原（0.6.10 复审 #5）：
// 取同包（payload 缺包 ID 则最近一次）先行送验的轮次——其载荷轮次优先，旧式
// 无轮次按该送验的计数重建。同包送验/裁定同轮：v22 两送验退回升级到 v24 再
// 送验时，库内补正计数与事件计数不串轮。
func roundOfPrecedingRequest(ctx context.Context, conn *sql.Conn, missionID string, seq int64, packageID string) int {
	var pseq int64
	var pround int
	err := conn.QueryRowContext(ctx, `
		SELECT seq, COALESCE(json_extract(payload, '$.review_round'), 0) FROM mission_events
		WHERE mission_id = ? AND seq < ? AND event_type = 'status_changed'
		  AND json_extract(payload, '$.to') = 'reviewing'
		  AND (? = '' OR COALESCE(json_extract(payload, '$.package_id'), '') = ?)
		ORDER BY seq DESC LIMIT 1`, missionID, seq, packageID, packageID).Scan(&pseq, &pround)
	if err != nil {
		return 0
	}
	if pround > 0 {
		return pround
	}
	return roundAtEvent(ctx, conn, missionID, pseq)
}

// roundAtEvent 旧 reviewing 事件轮次重建（0.6.10 审校 #5）：同一 Mission 截至该
// seq 的送验计数——轮次随每次送验单调递增（同包重审也递增），计数即当轮。
func roundAtEvent(ctx context.Context, conn *sql.Conn, missionID string, seq int64) int {
	var n int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mission_events
		WHERE mission_id = ? AND seq <= ? AND event_type = 'status_changed'
		  AND json_extract(payload, '$.to') = 'reviewing'`, missionID, seq).Scan(&n); err != nil {
		return 0
	}
	return n
}

// reopenRequestedPayload reopen_requested 事件的载荷字段（ADR-0034 / 0.6.11）。
type reopenRequestedPayload struct {
	RequestID          string   `json:"request_id"`
	DecisionRef        string   `json:"decision_ref"`
	EvidenceRefs       []string `json:"evidence_refs"`
	AffectedMissionIDs []string `json:"affected_mission_ids"`
	Reason             string   `json:"reason"`
	ContractVersion    int      `json:"contract_version"`
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
	case p.To == "returned":
		// 老式形态（0.6.9 初版 / v22 时代事件）：to 即 returned——四审实测
		// 真实 v22 历史两组共 3 条因此漏卡；新式 to=ready 上面已认。
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
		SELECT seq, mission_id, event_type, payload FROM mission_events
		WHERE seq > ? AND event_type IN ('status_changed', 'reopen_requested')
		ORDER BY seq LIMIT 500`, lastSeq)
	if err != nil {
		return 0, fmt.Errorf("sqlite: scan mission_events: %w", err)
	}
	type fact struct {
		seq       int64
		missionID string
		eventType string
		payload   statusChangedPayload
		reopen    reopenRequestedPayload
		hasReopen bool
	}
	var facts []fact
	for rows.Next() {
		var f fact
		var payload string
		if err := rows.Scan(&f.seq, &f.missionID, &f.eventType, &payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sqlite: scan mission_events row: %w", err)
		}
		if f.eventType == "reopen_requested" {
			_ = json.Unmarshal([]byte(payload), &f.reopen)
			f.hasReopen = true
		} else {
			_ = json.Unmarshal([]byte(payload), &f.payload)
		}
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
		if f.hasReopen {
			kind = missionreturn.KindReopen
		}
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
		// 0.6.10 审校 #1/#5：事实归属**当时**契约与轮次——payload 携带（新事件的
		// 送验/裁定事务内写入）优先，否则按事件流重建（旧事件 / 升级重放）；
		// objective 取该版本行的不可变快照，不读 missions 当前行（v1 退回后修订
		// v2 的重放不得把旧包记到 v2、显示 v2 目标）。
		contractVersion := f.payload.ContractVersion
		if f.hasReopen {
			contractVersion = f.reopen.ContractVersion
		}
		if contractVersion <= 0 {
			contractVersion = versionAtEvent(ctx, conn, f.missionID, f.seq)
		}
		objective, versionFound := objectiveAtVersion(ctx, conn, f.missionID, contractVersion)
		if !versionFound {
			var cur string
			if err := conn.QueryRowContext(ctx,
				"SELECT objective FROM missions WHERE mission_id = ?", f.missionID).Scan(&cur); err == nil {
				objective = truncateRunes(cur, 80)
			}
		}
		reviewRound := f.payload.ReviewRound
		if f.hasReopen {
			reviewRound = 0 // 重开请求无轮次语义（mission_ref 契约 reopen 为 0）
		}
		if reviewRound <= 0 && !f.hasReopen {
			switch kind {
			case missionreturn.KindReviewing:
				// 四审 #2：先按本次送验区间的显式裁定校准（v22→v24 混合历史——
				// 送验无轮次而裁定带迁移显式值），无则按全历史送验计数。
				if r := roundCalibratingDecision(ctx, conn, f.missionID, f.seq, f.payload.PackageID); r > 0 {
					reviewRound = r
				} else {
					reviewRound = roundAtEvent(ctx, conn, f.missionID, f.seq)
				}
			case missionreturn.KindReturned, missionreturn.KindAccepted:
				// 复审 #5：裁定按同包（缺省最近）先行送验还原轮次。
				reviewRound = roundOfPrecedingRequest(ctx, conn, f.missionID, f.seq, f.payload.PackageID)
			case missionreturn.KindCanceled:
				// 四审 #3：公开契约取消恒 0（mission_ref/卡片/台账三面一致）——
				// 不回查上一次送验（v1 退回后 v2 未送验即取消不得显示第 1 轮）。
				reviewRound = 0
			}
		}
		// reopen 快照（0.6.11 / ADR-0034）：reason / decision_ref / affected_count 取事件载荷
		reasonSnap, decisionRef, affectedCount := f.payload.Reason, "", 0
		if f.hasReopen {
			reasonSnap = f.reopen.Reason
			decisionRef = truncateRunes(f.reopen.DecisionRef, 256)
			affectedCount = len(f.reopen.AffectedMissionIDs)
		}
		res, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO mission_return_notices
			(notice_id, source_seq, mission_id, room_id, task_id, kind, contract_version,
			 review_round, package_id, reviewer, reason, objective, decision_ref, affected_count,
			 status, attempts, next_attempt_at, last_error, room_event_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, '', '', '',
			        strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
			"rn_"+strconv.FormatInt(f.seq, 10), f.seq, f.missionID, roomID, taskID, string(kind),
			contractVersion, reviewRound, f.payload.PackageID, f.payload.Reviewer,
			truncateRunes(reasonSnap, 200), objective, decisionRef, affectedCount)
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
 contract_version, review_round, package_id, reviewer, reason, objective,
 decision_ref, affected_count, status,
 attempts, next_attempt_at, last_error, room_event_id, created_at, updated_at`

func scanReturnNotice(row interface{ Scan(...any) error }) (missionreturn.Notice, error) {
	var n missionreturn.Notice
	err := row.Scan(&n.NoticeID, &n.SourceSeq, &n.MissionID, &n.RoomID, &n.TaskID, &n.Kind,
		&n.ContractVersion, &n.ReviewRound, &n.PackageID, &n.Reviewer, &n.Reason, &n.Objective,
		&n.DecisionRef, &n.AffectedCount,
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
	// 0.6.10 审校 #2/#6：先取各房**队首**未完成行（NOT EXISTS 更早 pending/held），
	// 再判队首到期——队首在暂停复查/退避中时同房后继不越序（accepted 不会抢在
	// reviewing 之前落房）；每房每轮至多一条，全局 LIMIT 是房间数上限——暂停房
	// 积压不再占满批次饿死他房。房头成功/终态后，后继自然成为新队首。
	rows, err := a.Store.db.QueryContext(ctx, `
		SELECT `+returnNoticeCols+` FROM mission_return_notices n
		WHERE n.status IN ('pending', 'held')
		  AND NOT EXISTS (
			SELECT 1 FROM mission_return_notices p
			WHERE p.room_id = n.room_id AND p.status IN ('pending', 'held')
			  AND p.source_seq < n.source_seq)
		  AND (n.next_attempt_at = '' OR datetime(n.next_attempt_at) <= datetime(?))
		ORDER BY n.room_id, n.source_seq LIMIT ?`, now, limit)
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
		// 0.6.10 审校 #8：投递侧兜底回填来源墓碑（先删房后升级 / 账本缺失——
		// DeleteRoom 级联与 migration v26 之外的补口）；结果与映射保留。
		// 该 UPDATE 是本分支的既定副作用——显式提交后再返回错误（早退的 defer
		// ROLLBACK 会把回填一并回滚）。
		// 复审 #7：优先取房间**实际**删除时间（墓碑）——预检后、来源写入前删房
		// 的场景不得记成投递时才发现的时间；无墓碑（极端缺失账本）才用当下。
		if _, err := conn.ExecContext(ctx, `
			UPDATE mission_sources
			SET deleted_at = COALESCE(deleted_at,
			    COALESCE((SELECT t.deleted_at FROM room_tombstones t WHERE t.room_id = mission_sources.room_id),
			             strftime('%Y-%m-%dT%H:%M:%fZ','now')))
			WHERE room_id = ?`, n.RoomID); err != nil {
			return fmt.Errorf("sqlite: backfill source tombstone: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("sqlite: commit: %w", err)
		}
		commit = true
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
			DecisionRef:     n.DecisionRef,
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
