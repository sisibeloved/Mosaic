// memory.go —— 分域记忆端点（0.7.0 / ADR-0037）：三端点（写入门 +
// 列表按命名空间隔离 + 单查）。解码本包手工实现（DisallowUnknownFields
// 契约纪律——同命令面）。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/memory"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

// writeMemoryUnavailable 记忆域未装配（测试装配 nil——生产恒置）。
func (s *server) writeMemoryUnavailable(w http.ResponseWriter) bool {
	if s.deps.MemorySVC == nil {
		writeError(w, http.StatusServiceUnavailable, "memory_unavailable", "记忆域未装配")
		return true
	}
	return false
}

// CreateMemoryItem 显式写入（apigen.ServerInterface）。
func (s *server) CreateMemoryItem(w http.ResponseWriter, r *http.Request) {
	if s.writeMemoryUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.MemoryItemCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "记忆写入体不合法："+err.Error())
		return
	}
	cmd := memory.CreateCmd{
		Namespace:  memory.Namespace(req.Namespace),
		ScopeID:    req.ScopeId,
		Kind:       memory.Kind(req.Kind),
		Title:      req.Title,
		Content:    req.Content,
		ObservedAt: req.ObservedAt,
	}
	if req.SourceRefs != nil {
		cmd.SourceRefs = *req.SourceRefs
	}
	if req.ValidFrom != nil {
		cmd.ValidFrom = *req.ValidFrom
	}
	if req.ValidTo != nil {
		cmd.ValidTo = *req.ValidTo
	}
	if req.Confidence != nil {
		v := float64(*req.Confidence)
		cmd.Confidence = &v
	}
	if req.Sensitivity != nil {
		cmd.Sensitivity = memory.Sensitivity(*req.Sensitivity)
	}
	it, err := s.deps.MemorySVC.Create(r.Context(), cmd)
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, memoryItemView(it))
}

// ListMemoryItems 分域列表（namespace 必填隔离）。
func (s *server) ListMemoryItems(w http.ResponseWriter, r *http.Request, params apigen.ListMemoryItemsParams) {
	if s.writeMemoryUnavailable(w) {
		return
	}
	f := memory.ListFilter{
		Namespace: memory.Namespace(params.Namespace),
		Limit:     memory.DefaultListLimit,
		Before:    "",
	}
	if params.ScopeId != nil {
		f.ScopeID = *params.ScopeId
	}
	if params.Kind != nil {
		f.Kind = memory.Kind(*params.Kind)
	}
	if params.Status != nil {
		f.Status = memory.Status(*params.Status)
	}
	if params.Limit != nil {
		f.Limit = *params.Limit
	}
	if params.Before != nil {
		f.Before = *params.Before
	}
	items, cursor, err := s.deps.MemorySVC.List(r.Context(), f)
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	view := apigen.MemoryItemListView{Items: make([]apigen.MemoryItemView, 0, len(items))}
	for _, it := range items {
		view.Items = append(view.Items, memoryItemView(it))
	}
	if cursor != "" {
		view.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, view)
}

// GetMemoryItem 单查。
func (s *server) GetMemoryItem(w http.ResponseWriter, r *http.Request, memoryID string) {
	if s.writeMemoryUnavailable(w) {
		return
	}
	it, err := s.deps.MemorySVC.Get(r.Context(), memoryID)
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryItemView(it))
}

// writeMemoryError 错误族映射：域校验 400 / 锚不存在 409 / 未知项 404。
func (s *server) writeMemoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, memory.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, memory.ErrScopeNotFound):
		writeError(w, http.StatusConflict, "memory_scope_not_found", "归属锚对象不存在")
	case errors.Is(err, memory.ErrNotFound):
		writeError(w, http.StatusNotFound, "memory_not_found", "记忆项不存在")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// memoryItemView 域 → 契约视图（confidence float32 语义等价——[0,1] 内
// 精度无损）。
func memoryItemView(it memory.Item) apigen.MemoryItemView {
	return apigen.MemoryItemView{
		MemoryId:    it.MemoryID,
		Version:     it.Version,
		Namespace:   apigen.MemoryItemViewNamespace(it.Namespace),
		ScopeId:     it.ScopeID,
		Kind:        apigen.MemoryItemViewKind(it.Kind),
		Title:       it.Title,
		Content:     it.Content,
		SourceRefs:  it.SourceRefs,
		ObservedAt:  it.ObservedAt,
		ValidFrom:   &it.ValidFrom,
		ValidTo:     &it.ValidTo,
		Confidence:  float32(it.Confidence),
		Status:      apigen.MemoryItemViewStatus(it.Status),
		Visibility:  it.Visibility,
		Sensitivity: apigen.MemoryItemViewSensitivity(it.Sensitivity),
		Supersedes:  it.Supersedes,
		CreatedAt:   it.CreatedAt,
		UpdatedAt:   it.UpdatedAt,
	}
}
