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
	var req apigen.MemoryItemCreateRequest
	if !decodeMemoryBody(w, r, &req) {
		return
	}
	it, err := s.deps.MemorySVC.Create(r.Context(), createCmdOf(req))
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
	if params.IncludeExpired != nil {
		f.IncludeExpired = *params.IncludeExpired
	}
	if params.Q != nil {
		f.Query = *params.Q
	}
	res, err := s.deps.MemorySVC.List(r.Context(), f)
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	view := apigen.MemoryItemListView{Items: make([]apigen.MemoryItemView, 0, len(res.Items)), TotalHits: res.TotalHits}
	for _, it := range res.Items {
		view.Items = append(view.Items, memoryItemView(it))
	}
	if res.NextCursor != "" {
		cursor := res.NextCursor
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
	case errors.Is(err, memory.ErrVersionConflict):
		writeError(w, http.StatusConflict, "memory_version_conflict", "版本冲突——重取后再试")
	case errors.Is(err, memory.ErrTerminal):
		writeError(w, http.StatusConflict, "memory_terminal", "终结态条目不可再动作（superseded 不可复活）")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// nonNilRefs nil 切片兜底为空数组（一审 #1——序列化恒 []，不出 null）。
func nonNilRefs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// editHistoryView 版本留痕视图（null 兜底空数组——0.7.0 一审 #1 同纪律）。
func editHistoryView(h []memory.EditRecord) []apigen.MemoryEditRecordView {
	out := make([]apigen.MemoryEditRecordView, 0, len(h))
	for _, e := range h {
		v := apigen.MemoryEditRecordView{
			Version: e.Version, Cause: apigen.MemoryEditRecordViewCause(e.Cause),
			EditedBy: e.EditedBy, OccurredAt: e.OccurredAt,
		}
		if e.Note != "" {
			note := e.Note
			v.Note = &note
		}
		out = append(out, v)
	}
	return out
}

// decodeMemoryBody 严格解码助手（四策展端点共用：MaxBytesReader + 拒收未知字段）。
func decodeMemoryBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "记忆请求体不合法："+err.Error())
		return false
	}
	return true
}

// createCmdOf 契约创建请求 → 域命令（Create/Supersede replacement 共用）。
func createCmdOf(req apigen.MemoryItemCreateRequest) memory.CreateCmd {
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
	if req.Status != nil {
		cmd.Status = memory.Status(*req.Status)
	}
	return cmd
}

// AcceptMemoryItem 策展接受（apigen.ServerInterface；0.7.1 / ADR-0038）。
func (s *server) AcceptMemoryItem(w http.ResponseWriter, r *http.Request, memoryID string) {
	if s.writeMemoryUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	it, err := s.deps.MemorySVC.Accept(r.Context(), memoryID)
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryItemView(it))
}

// RejectMemoryItem 候选否决 / active 撤回（0.7.1）。
func (s *server) RejectMemoryItem(w http.ResponseWriter, r *http.Request, memoryID string) {
	if s.writeMemoryUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	var req apigen.MemoryItemRejectRequest
	if !decodeMemoryBody(w, r, &req) {
		return
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	it, err := s.deps.MemorySVC.Reject(r.Context(), memoryID, note)
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryItemView(it))
}

// ReviseMemoryItem 原位修订（0.7.1）。
func (s *server) ReviseMemoryItem(w http.ResponseWriter, r *http.Request, memoryID string) {
	if s.writeMemoryUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	var req apigen.MemoryItemReviseRequest
	if !decodeMemoryBody(w, r, &req) {
		return
	}
	cmd := memory.ReviseCmd{ExpectedVersion: req.ExpectedVersion}
	if req.Title != nil {
		cmd.Title = req.Title
	}
	if req.Content != nil {
		cmd.Content = req.Content
	}
	if req.SourceRefs != nil {
		cmd.SourceRefs = req.SourceRefs
	}
	if req.ObservedAt != nil {
		cmd.ObservedAt = req.ObservedAt
	}
	if req.ValidFrom != nil {
		cmd.ValidFrom = req.ValidFrom
	}
	if req.ValidTo != nil {
		cmd.ValidTo = req.ValidTo
	}
	if req.Sensitivity != nil {
		v := memory.Sensitivity(*req.Sensitivity)
		cmd.Sensitivity = &v
	}
	if req.Confidence != nil {
		v := float64(*req.Confidence)
		cmd.Confidence = &v
	}
	if req.Note != nil {
		cmd.Note = *req.Note
	}
	it, err := s.deps.MemorySVC.Revise(r.Context(), memoryID, cmd)
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryItemView(it))
}

// SupersedeMemoryItem 显式替代（0.7.1——单事务双动作）。
func (s *server) SupersedeMemoryItem(w http.ResponseWriter, r *http.Request, memoryID string) {
	if s.writeMemoryUnavailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	var req apigen.MemoryItemSupersedeRequest
	if !decodeMemoryBody(w, r, &req) {
		return
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	oldIt, newIt, err := s.deps.MemorySVC.Supersede(r.Context(), memoryID,
		req.ExpectedVersion, note, createCmdOf(req.Replacement))
	if err != nil {
		s.writeMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.MemoryItemSupersedeResult{
		Superseded: memoryItemView(oldIt), Replacement: memoryItemView(newIt),
	})
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
		SourceRefs:  nonNilRefs(it.SourceRefs),
		Supersedes:  nonNilRefs(it.Supersedes),
		EditHistory: editHistoryView(it.EditHistory),
		ObservedAt:  it.ObservedAt,
		ValidFrom:   &it.ValidFrom,
		ValidTo:     &it.ValidTo,
		Confidence:  float32(it.Confidence),
		Status:      apigen.MemoryItemViewStatus(it.Status),
		Visibility:  it.Visibility,
		Sensitivity: apigen.MemoryItemViewSensitivity(it.Sensitivity),
		CreatedAt:   it.CreatedAt,
		UpdatedAt:   it.UpdatedAt,
	}
}
