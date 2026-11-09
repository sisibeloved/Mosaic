// bundle.go —— Portable Agent Bundle 导出端点（0.7.5 / ADR-0042，RFC-0018 §8）。
package httpapi

import (
	"errors"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/bundle"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

func (s *server) writeBundleUnavailable(w http.ResponseWriter) bool {
	if s.deps.BundleSVC == nil {
		writeError(w, http.StatusServiceUnavailable, "bundle_unavailable", "Bundle 导出面未装配")
		return true
	}
	return false
}

// CreateAgentBundleExport 创建导出（apigen.ServerInterface）。
//
// 响应体直接序列化 bundle.View——域视图的 JSON 形状即契约形状
// （AgentBundleExportView 同构；backup Summary 先例，零转换）。
func (s *server) CreateAgentBundleExport(w http.ResponseWriter, r *http.Request, agentID string) {
	if s.writeBundleUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	var req apigen.AgentBundleExportRequest
	if !decodeMemoryBody(w, r, &req) { // 同款严格解码（MaxBytes + 拒收未知字段）
		return
	}
	cmd := bundle.ScopeRequest{IncludeSensitive: req.IncludeSensitive != nil && *req.IncludeSensitive}
	if req.Namespaces != nil {
		cmd.Namespaces = make([]string, 0, len(*req.Namespaces))
		for _, ns := range *req.Namespaces {
			cmd.Namespaces = append(cmd.Namespaces, string(ns))
		}
	}
	if req.MissionIds != nil {
		cmd.MissionIDs = *req.MissionIds
	}
	view, err := s.deps.BundleSVC.Export(r.Context(), agentID, cmd)
	if err != nil {
		s.writeBundleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// ListAgentBundleExports 该 Agent 的导出列表（新→旧）。
func (s *server) ListAgentBundleExports(w http.ResponseWriter, r *http.Request, agentID string) {
	if s.writeBundleUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	list, err := s.deps.BundleSVC.List(agentID)
	if err != nil {
		s.writeBundleError(w, err)
		return
	}
	out := apigen.AgentBundleExportListView{Exports: make([]apigen.AgentBundleSummary, 0, len(list))}
	for _, sum := range list {
		item := apigen.AgentBundleSummary{
			BundleId:  sum.BundleID,
			AgentId:   sum.AgentID,
			CreatedAt: sum.CreatedAt,
			SizeBytes: int(sum.SizeBytes),
		}
		item.Counts.Memories = sum.Counts.Memories
		item.Counts.Missions = sum.Counts.Missions
		item.Counts.Checkpoints = sum.Counts.Checkpoints
		out.Exports = append(out.Exports, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// GetAgentBundleExport 导出详情（传输前核对脱敏报告）。
func (s *server) GetAgentBundleExport(w http.ResponseWriter, r *http.Request, bundleID string) {
	if s.writeBundleUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	view, err := s.deps.BundleSVC.Get(bundleID)
	if err != nil {
		s.writeBundleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// writeBundleError 错误族映射。
func (s *server) writeBundleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bundle.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, bundle.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "agent_not_found", "Agent 不存在")
	case errors.Is(err, bundle.ErrNotFound):
		writeError(w, http.StatusNotFound, "bundle_not_found", "导出不存在")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
