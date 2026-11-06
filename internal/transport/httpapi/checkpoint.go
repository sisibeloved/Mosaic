// checkpoint.go —— Run 检查点端点（0.7.3 / ADR-0040，RFC-0018 §7.3）。
package httpapi

import (
	"errors"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/checkpoint"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

func (s *server) writeCheckpointUnavailable(w http.ResponseWriter) bool {
	if s.deps.CheckpointSVC == nil {
		writeError(w, http.StatusServiceUnavailable, "checkpoint_unavailable", "检查点域未装配")
		return true
	}
	return false
}

// CreateRunCheckpoint 落盘（apigen.ServerInterface）。
func (s *server) CreateRunCheckpoint(w http.ResponseWriter, r *http.Request, runID string) {
	if s.writeCheckpointUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	var req apigen.RunCheckpointCreateRequest
	if !decodeMemoryBody(w, r, &req) { // 同款严格解码（MaxBytes + 拒收未知字段）
		return
	}
	cmd := checkpoint.CreateCmd{
		RunID:             runID,
		MissionID:         req.MissionId,
		MissionVersion:    req.MissionVersion,
		WorkItemID:        strOr(req.WorkItemId),
		CompletedSteps:    strsOr(req.CompletedSteps),
		PendingSteps:      strsOr(req.PendingSteps),
		ArtifactRefs:      strsOr(req.ArtifactRefs),
		ActionReceipts:    strsOr(req.ActionReceipts),
		UncertainActions:  strsOr(req.UncertainActions),
		WaitingConditions: strsOr(req.WaitingConditions),
		SourceWatermarks:  mapsOr(req.SourceWatermarks),
		ProgressSummary:   strOr(req.ProgressSummary),
	}
	cp, err := s.deps.CheckpointSVC.Create(r.Context(), cmd)
	if err != nil {
		s.writeCheckpointError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, checkpointView(cp))
}

// ListRunCheckpoints 列表（降序）。
func (s *server) ListRunCheckpoints(w http.ResponseWriter, r *http.Request, runID string) {
	if s.writeCheckpointUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	items, err := s.deps.CheckpointSVC.ListOfRun(r.Context(), runID)
	if err != nil {
		s.writeCheckpointError(w, err)
		return
	}
	view := apigen.RunCheckpointListView{Checkpoints: make([]apigen.RunCheckpointView, 0, len(items))}
	for _, cp := range items {
		view.Checkpoints = append(view.Checkpoints, checkpointView(cp))
	}
	writeJSON(w, http.StatusOK, view)
}

// GetLatestRunCheckpoint 最新一条（恢复面入口）。
func (s *server) GetLatestRunCheckpoint(w http.ResponseWriter, r *http.Request, runID string) {
	if s.writeCheckpointUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	cp, err := s.deps.CheckpointSVC.Latest(r.Context(), runID)
	if err != nil {
		s.writeCheckpointError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, checkpointView(cp))
}

// writeCheckpointError 错误族映射。
func (s *server) writeCheckpointError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checkpoint.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, checkpoint.ErrRunNotFound):
		writeError(w, http.StatusNotFound, "run_not_found", "Run 不存在")
	case errors.Is(err, checkpoint.ErrScopeMismatch):
		writeError(w, http.StatusConflict, "checkpoint_scope_mismatch", "mission_id 与 Run 归属不一致")
	case errors.Is(err, checkpoint.ErrNotFound):
		writeError(w, http.StatusNotFound, "checkpoint_not_found", "尚未落盘检查点")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

func checkpointView(cp checkpoint.Checkpoint) apigen.RunCheckpointView {
	return apigen.RunCheckpointView{
		CheckpointId:      cp.CheckpointID,
		RunId:             cp.RunID,
		MissionId:         cp.MissionID,
		MissionVersion:    cp.MissionVersion,
		CheckpointVersion: cp.CheckpointVersion,
		WorkItemId:        cpStrPtr(cp.WorkItemID),
		CompletedSteps:    nonNil(cp.CompletedSteps),
		PendingSteps:      nonNil(cp.PendingSteps),
		ArtifactRefs:      nonNil(cp.ArtifactRefs),
		ActionReceipts:    nonNil(cp.ActionReceipts),
		UncertainActions:  nonNil(cp.UncertainActions),
		WaitingConditions: nonNil(cp.WaitingConditions),
		SourceWatermarks:  cp.SourceWatermarks,
		ProgressSummary:   cp.ProgressSummary,
		CreatedAt:         cp.CreatedAt,
	}
}

// 视图兜底（nil → 空数组——与 0.7.0 一审 #1 同纪律）。
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func strsOr(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

func watermarkOr(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	return m
}

func mapsOr(m *map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	return *m
}

// cpStrPtr 视图字段（work_item_id 可选透出——server.go 已有 strPtr）。
func cpStrPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
