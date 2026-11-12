// bundle.go —— Portable Agent Bundle 导出端点（0.7.5 / ADR-0042，RFC-0018 §8）
// 与导入预检端点（0.7.6 / ADR-0043——multipart 上传，附件先例）。
package httpapi

import (
	"errors"
	"io"
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

func (s *server) writeBundleImportUnavailable(w http.ResponseWriter) bool {
	if s.deps.BundleImportSVC == nil {
		writeError(w, http.StatusServiceUnavailable, "bundle_unavailable", "Bundle 导入面未装配")
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

// ---- 导入预检（0.7.6 / ADR-0043）----

// maxImportUploadOverhead multipart 包装开销余量（64MiB 体上限 + 边界/头部）。
const maxImportUploadOverhead = 64 << 10

// CreateBundleImport 上传 + 预检 + inactive 登记（apigen.ServerInterface）。
//
// multipart 字段 file（附件先例——guardWrite requireJSON=false 变体）；
// 响应体直接序列化 bundle.ImportView（域视图形状即契约形状，零转换）。
func (s *server) CreateBundleImport(w http.ResponseWriter, r *http.Request) {
	if s.writeBundleImportUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, bundle.MaxZipBytes+maxImportUploadOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "bundle_too_large", "上传体超上限")
			return
		}
		writeError(w, http.StatusBadRequest, "multipart_required", "上传须为 multipart/form-data（字段 file）")
		return
	}
	var file io.Reader
	for {
		part, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			writeError(w, http.StatusBadRequest, "multipart_invalid", err.Error())
			return
		}
		if part.FormName() != "file" {
			continue // 其余字段忽略（字段面封闭——附件同例）
		}
		file = part
		break
	}
	if file == nil {
		writeError(w, http.StatusBadRequest, "file_field_required", "缺少 file 字段")
		return
	}
	view, err := s.deps.BundleImportSVC.Import(r.Context(), file)
	if err != nil {
		s.writeBundleImportError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// ListBundleImports 导入列表（新→旧；实例级）。
func (s *server) ListBundleImports(w http.ResponseWriter, r *http.Request) {
	if s.writeBundleImportUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	list, err := s.deps.BundleImportSVC.List()
	if err != nil {
		s.writeBundleImportError(w, err)
		return
	}
	out := apigen.BundleImportListView{Imports: make([]apigen.BundleImportSummary, 0, len(list))}
	for _, item := range list {
		sum := apigen.BundleImportSummary{
			ImportId: item.ImportID, BundleId: item.BundleID, AgentId: item.AgentID,
			Status:     apigen.BundleImportSummaryStatus(item.Status),
			Verdict:    apigen.BundleImportSummaryVerdict(item.Verdict),
			ImportedAt: item.ImportedAt, SizeBytes: int(item.SizeBytes),
		}
		sum.Counts.Memories = item.Counts.Memories
		sum.Counts.Missions = item.Counts.Missions
		sum.Counts.Checkpoints = item.Counts.Checkpoints
		out.Imports = append(out.Imports, sum)
	}
	writeJSON(w, http.StatusOK, out)
}

// GetBundleImport 导入详情（激活决策前的复核面）。
func (s *server) GetBundleImport(w http.ResponseWriter, r *http.Request, importID string) {
	if s.writeBundleImportUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, false) {
		return
	}
	view, err := s.deps.BundleImportSVC.Get(importID)
	if err != nil {
		s.writeBundleImportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// writeBundleImportError 导入错误族映射（413/422/409 为导入面新增语义）。
func (s *server) writeBundleImportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bundle.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "bundle_too_large", err.Error())
	case errors.Is(err, bundle.ErrUnsafe):
		writeError(w, http.StatusUnprocessableEntity, "bundle_unsafe", err.Error())
	case errors.Is(err, bundle.ErrConflict):
		writeError(w, http.StatusConflict, "bundle_already_imported", err.Error())
	case errors.Is(err, bundle.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, bundle.ErrNotFound):
		writeError(w, http.StatusNotFound, "bundle_not_found", "导入不存在")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// ---- 激活（0.7.7 / ADR-0044）----

// ActivateBundleImport 激活与执行权接管（apigen.ServerInterface）。
//
// 响应体直接序列化 bundle.ActivationView（域视图形状即契约形状，零转换）。
func (s *server) ActivateBundleImport(w http.ResponseWriter, r *http.Request, importID string) {
	if s.deps.BundleActivationSVC == nil {
		writeError(w, http.StatusServiceUnavailable, "bundle_unavailable", "Bundle 激活面未装配")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	var req apigen.BundleActivationRequest
	if !decodeMemoryBody(w, r, &req) { // 同款严格解码（MaxBytes + 拒收未知字段）
		return
	}
	view, err := s.deps.BundleActivationSVC.Activate(r.Context(), importID, bundle.ActivationRequest{
		Mode:                 string(req.Mode),
		ConfirmSourceStopped: req.ConfirmSourceStopped != nil && *req.ConfirmSourceStopped,
	})
	if err != nil {
		s.writeBundleActivationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// writeBundleActivationError 激活错误族映射（409 模式错配 / 422 复核不过）。
func (s *server) writeBundleActivationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bundle.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, bundle.ErrConfirmRequired):
		writeError(w, http.StatusBadRequest, "source_stop_confirmation_required", err.Error())
	case errors.Is(err, bundle.ErrModeMismatch):
		writeError(w, http.StatusConflict, "activation_mode_mismatch", err.Error())
	case errors.Is(err, bundle.ErrActivationBlocked):
		writeError(w, http.StatusUnprocessableEntity, "activation_blocked", err.Error())
	case errors.Is(err, bundle.ErrNotFound):
		writeError(w, http.StatusNotFound, "bundle_not_found", "导入不存在")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
