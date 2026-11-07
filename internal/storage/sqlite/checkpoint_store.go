// checkpoint_store.go —— Run 检查点存储（0.7.3 / ADR-0040 / migration v34）。
// checkpoint_version 事务内原子分配（MAX+1——并发落盘不撞号）；列表降序。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sisibeloved/Mosaic/internal/checkpoint"
)

var _ checkpoint.Store = (*Store)(nil)

const checkpointCols = `checkpoint_id, run_id, mission_id, mission_version,
 checkpoint_version, work_item_id, completed_steps, pending_steps, artifact_refs,
 action_receipts, uncertain_actions, waiting_conditions, source_watermarks,
 progress_summary, created_at`

func scanCheckpoint(row rowScanner) (checkpoint.Checkpoint, error) {
	var cp checkpoint.Checkpoint
	var completed, pending, arts, receipts, uncertain, waiting, marks string
	if err := row.Scan(&cp.CheckpointID, &cp.RunID, &cp.MissionID, &cp.MissionVersion,
		&cp.CheckpointVersion, &cp.WorkItemID, &completed, &pending, &arts,
		&receipts, &uncertain, &waiting, &marks, &cp.ProgressSummary, &cp.CreatedAt); err != nil {
		return checkpoint.Checkpoint{}, err
	}
	if err := json.Unmarshal([]byte(completed), &cp.CompletedSteps); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint completed: %w", err)
	}
	if err := json.Unmarshal([]byte(pending), &cp.PendingSteps); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint pending: %w", err)
	}
	if err := json.Unmarshal([]byte(arts), &cp.ArtifactRefs); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint artifacts: %w", err)
	}
	if err := json.Unmarshal([]byte(receipts), &cp.ActionReceipts); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint receipts: %w", err)
	}
	if err := json.Unmarshal([]byte(uncertain), &cp.UncertainActions); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint uncertain: %w", err)
	}
	if err := json.Unmarshal([]byte(waiting), &cp.WaitingConditions); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint waiting: %w", err)
	}
	if err := json.Unmarshal([]byte(marks), &cp.SourceWatermarks); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint watermarks: %w", err)
	}
	return cp, nil
}

func marshalList(v []string) (string, error) {
	if v == nil {
		return "[]", nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// CreateCheckpoint 落盘（版本原子分配：INSERT 语句内子查询 MAX+1——单语句
// 原子，并发落盘经 (run_id, checkpoint_version) 唯一索引兜底不撞号；读回
// 实际分配版本）。
func (s *Store) CreateCheckpoint(ctx context.Context, cp checkpoint.Checkpoint) (checkpoint.Checkpoint, error) {
	completed, err := marshalList(cp.CompletedSteps)
	if err != nil {
		return checkpoint.Checkpoint{}, err
	}
	pending, err := marshalList(cp.PendingSteps)
	if err != nil {
		return checkpoint.Checkpoint{}, err
	}
	arts, err := marshalList(cp.ArtifactRefs)
	if err != nil {
		return checkpoint.Checkpoint{}, err
	}
	receipts, err := marshalList(cp.ActionReceipts)
	if err != nil {
		return checkpoint.Checkpoint{}, err
	}
	uncertain, err := marshalList(cp.UncertainActions)
	if err != nil {
		return checkpoint.Checkpoint{}, err
	}
	waiting, err := marshalList(cp.WaitingConditions)
	if err != nil {
		return checkpoint.Checkpoint{}, err
	}
	if cp.SourceWatermarks == nil {
		cp.SourceWatermarks = map[string]string{}
	}
	marks, err := json.Marshal(cp.SourceWatermarks)
	if err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint watermarks marshal: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO checkpoints (`+checkpointCols+`)
 VALUES (?, ?, ?, ?,
  (SELECT COALESCE(MAX(checkpoint_version), 0) + 1 FROM checkpoints WHERE run_id = ?),
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.CheckpointID, cp.RunID, cp.MissionID, cp.MissionVersion, cp.RunID,
		cp.WorkItemID, completed, pending, arts,
		receipts, uncertain, waiting, string(marks), cp.ProgressSummary, cp.CreatedAt)
	if err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: insert checkpoint: %w", err)
	}
	// 读回实际分配版本（原子分配事实）。
	if err := s.db.QueryRowContext(ctx,
		`SELECT checkpoint_version FROM checkpoints WHERE checkpoint_id = ?`,
		cp.CheckpointID).Scan(&cp.CheckpointVersion); err != nil {
		return checkpoint.Checkpoint{}, fmt.Errorf("sqlite: checkpoint version readback: %w", err)
	}
	return cp, nil
}

// LatestOfMission 任务级最新（跨 Run——rowid DESC 取最新插入行）。
func (s *Store) LatestOfMission(ctx context.Context, missionID string) (checkpoint.Checkpoint, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+checkpointCols+` FROM checkpoints WHERE mission_id = ?
 ORDER BY rowid DESC LIMIT 1`, missionID)
	cp, err := scanCheckpoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return checkpoint.Checkpoint{}, false, nil
	}
	if err != nil {
		return checkpoint.Checkpoint{}, false, fmt.Errorf("sqlite: latest checkpoint of mission: %w", err)
	}
	return cp, true, nil
}

// CheckpointsOfRun 列表（checkpoint_version 降序——最新在前）。
func (s *Store) CheckpointsOfRun(ctx context.Context, runID string) ([]checkpoint.Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+checkpointCols+` FROM checkpoints WHERE run_id = ?
 ORDER BY checkpoint_version DESC`, runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: checkpoints of run: %w", err)
	}
	defer rows.Close()
	out := []checkpoint.Checkpoint{}
	for rows.Next() {
		cp, err := scanCheckpoint(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: checkpoints scan: %w", err)
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

var _ = errors.Is // 预留（sql.ErrNoRows 语义经 ok=false 判定——本面列表空即无）
var _ = sql.ErrNoRows
