// 表格交换 HTTP 面（RFC-0017 M2）：preview / execute / 历史。写门三件套 +
// 4KiB 体上限照 extdocs 惯例；blocked 预检以 409 sheet_transfer_blocked 携
// 全量报告项返回（前端逐项展示，不静默降级）。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

// transfersAvailable 交换面装配检查（extdocs 与交换服务双依赖）。
func (s *server) transfersAvailable(w http.ResponseWriter) bool {
	if s.deps.ExtDocs == nil || s.deps.SheetTransfers == nil {
		writeError(w, http.StatusNotFound, "extdocs_unavailable", "表格交换面未装配")
		return false
	}
	return true
}

// PreviewSheetTransfer POST /v1/extdocs/sheet-transfers/preview。
func (s *server) PreviewSheetTransfer(w http.ResponseWriter, r *http.Request) {
	if !s.transfersAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.SheetTransferPreviewRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷不合法: "+err.Error())
		return
	}
	tr := transferRequestOf(string(req.Direction), deref(req.Provider), deref(req.Url), deref(req.DocId), derefSlice(req.SheetNames))
	pv, err := s.deps.SheetTransfers.Preview(r.Context(), tr)
	if err != nil {
		s.writeTransferError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sheetTransferPreviewOut(pv))
}

// ExecuteSheetTransfer POST /v1/extdocs/sheet-transfers。
func (s *server) ExecuteSheetTransfer(w http.ResponseWriter, r *http.Request) {
	if !s.transfersAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.SheetTransferExecuteRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷不合法: "+err.Error())
		return
	}
	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 idempotency_key")
		return
	}
	tr := transferRequestOf(string(req.Direction), deref(req.Provider), deref(req.Url), deref(req.DocId), derefSlice(req.SheetNames))
	tr.IdempotencyKey = req.IdempotencyKey
	tr.SnapshotHash = deref(req.SnapshotHash)
	tr.SourceVersion = deref(req.SourceVersion)
	tr.Actor = s.deps.Actor.ParticipantID
	rec, err := s.deps.SheetTransfers.Execute(r.Context(), tr)
	if err != nil {
		s.writeTransferError(w, err, rec)
		return
	}
	writeJSON(w, http.StatusCreated, sheetTransferRecordOut(rec))
}

// ListSheetTransfers GET /v1/extdocs/sheet-transfers。
func (s *server) ListSheetTransfers(w http.ResponseWriter, r *http.Request, params apigen.ListSheetTransfersParams) {
	if !s.transfersAvailable(w) {
		return
	}
	limit := 50
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}
	direction := ""
	if params.Direction != nil {
		direction = string(*params.Direction)
	}
	docID := ""
	if params.DocId != nil {
		docID = *params.DocId
	}
	recs, err := s.deps.SheetTransfers.List(r.Context(), docID, direction, limit)
	if err != nil {
		s.writeTransferError(w, err)
		return
	}
	out := make([]apigen.SheetTransferRecord, 0, len(recs))
	for i := range recs {
		out = append(out, sheetTransferRecordOut(&recs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": out})
}

// transferRequestOf 生成侧请求 → 域请求。
func transferRequestOf(direction, provider, url, docID string, sheetNames []string) extdocs.TransferRequest {
	names := make([]string, 0, len(sheetNames))
	for _, n := range sheetNames {
		names = append(names, n)
	}
	return extdocs.TransferRequest{
		Direction: direction, Provider: provider, URL: url, DocID: docID, SheetNames: names,
	}
}

// writeTransferError 域错误 → HTTP（blocked 携报告；failed 记录仍给目标链接）。
func (s *server) writeTransferError(w http.ResponseWriter, err error, recs ...*extdocs.SheetTransferRecord) {
	var blocked *extdocs.BlockedTransferError
	if errors.As(err, &blocked) {
		body := map[string]any{
			"error":  map[string]string{"code": "sheet_transfer_blocked", "message": "预检存在阻止项（见 report）"},
			"report": sheetTransferIssuesOut(blocked.Issues),
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	switch {
	case errors.Is(err, extdocs.ErrStalePreview):
		writeError(w, http.StatusConflict, "sheet_transfer_stale_preview", err.Error())
		return
	case errors.Is(err, extdocs.ErrIdempotencyKeyRequired):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case errors.Is(err, extdocs.ErrBadDirection), errors.Is(err, extdocs.ErrDocRequired),
		errors.Is(err, extdocs.ErrBadLink), errors.Is(err, extdocs.ErrNotSpreadsheet),
		errors.Is(err, extdocs.ErrProviderMismatch):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case errors.Is(err, extdocs.ErrUnknownProvider):
		writeError(w, http.StatusNotFound, "extdocs_bad_provider", err.Error())
		return
	case errors.Is(err, doc.ErrDocNotFound):
		writeError(w, http.StatusNotFound, "extdocs_doc_not_found", err.Error())
		return
	case errors.Is(err, extdocs.ErrTokenMissing), errors.Is(err, extdocs.ErrReauthRequired):
		s.writeExtDocUpstreamError(w, err) // 409 按既有凭据语义（设置页可指路重授权）
		return
	}
	// 其余（token/reauth/upstream/域校验）沿既有 extdocs 错误面；执行失败时
	// 错误体之外附带已落库记录（部分写入场景保留目标链接）。
	for _, rec := range recs {
		if rec != nil && rec.Status == extdocs.TransferFailed {
			body := map[string]any{
				"error":    map[string]string{"code": "sheet_transfer_failed", "message": truncateStr(rec.Error, 300)},
				"transfer": sheetTransferRecordOut(rec),
			}
			writeJSON(w, http.StatusConflict, body)
			return
		}
	}
	s.writeExtDocUpstreamError(w, err)
}

// ---- 出参视图 ----

func sheetTransferPreviewOut(pv *extdocs.TransferPreview) apigen.SheetTransferPreview {
	out := apigen.SheetTransferPreview{
		Direction: apigen.SheetTransferPreviewDirection(pv.Direction),
		Provider:  pv.Provider,
		Title:     &pv.Title,
		Sheets:    make([]apigen.SheetTransferPreviewSheet, 0, len(pv.Sheets)),
		Issues:    sheetTransferIssuesOut(pv.Issues),
	}
	if pv.SourceURL != "" {
		out.SourceUrl = &pv.SourceURL
	}
	if pv.DocID != "" {
		out.DocId = &pv.DocID
	}
	if pv.SourceVer != 0 {
		out.SourceVersion = &pv.SourceVer
	}
	if pv.SnapshotHash != "" {
		out.SnapshotHash = &pv.SnapshotHash
	}
	for _, sh := range pv.Sheets {
		out.Sheets = append(out.Sheets, apigen.SheetTransferPreviewSheet{
			Name: sh.Name, Rows: sh.Rows, Cols: sh.Cols,
			Filled: sh.Filled, Formulas: sh.Formulas, MergeCount: &sh.MergeCount,
		})
	}
	return out
}

func sheetTransferRecordOut(rec *extdocs.SheetTransferRecord) apigen.SheetTransferRecord {
	issues := []apigen.SheetTransferIssue{}
	_ = json.Unmarshal([]byte(rec.ReportJSON), &issues)
	out := apigen.SheetTransferRecord{
		TransferId:     rec.TransferID,
		IdempotencyKey: rec.IdempotencyKey,
		Direction:      apigen.SheetTransferRecordDirection(rec.Direction),
		Provider:       rec.Provider,
		Status:         apigen.SheetTransferRecordStatus(rec.Status),
		Report:         issues,
		CreatedAt:      rec.CreatedAt,
		UpdatedAt:      rec.UpdatedAt,
	}
	if rec.DocID != "" {
		out.DocId = &rec.DocID
	}
	if rec.ExternalURL != "" {
		out.ExternalUrl = &rec.ExternalURL
	}
	if rec.Title != "" {
		out.Title = &rec.Title
	}
	if rec.SourceVersion != 0 {
		out.SourceVersion = &rec.SourceVersion
	}
	if rec.SnapshotHash != "" {
		out.SnapshotHash = &rec.SnapshotHash
	}
	if rec.Error != "" {
		out.Error = &rec.Error
	}
	if rec.CellsTotal > 0 {
		out.CellsTotal = &rec.CellsTotal
	}
	if rec.CellsWritten > 0 {
		out.CellsWritten = &rec.CellsWritten
	}
	return out
}

func sheetTransferIssuesOut(issues []extdocs.TransferIssue) []apigen.SheetTransferIssue {
	out := make([]apigen.SheetTransferIssue, 0, len(issues))
	for _, is := range issues {
		item := apigen.SheetTransferIssue{
			Feature:  is.Feature,
			Severity: apigen.SheetTransferIssueSeverity(is.Severity),
			Reason:   is.Reason,
		}
		if is.Sheet != "" {
			item.Sheet = &is.Sheet
		}
		if is.Range != "" {
			item.Range = &is.Range
		}
		if is.Action != "" {
			item.Action = ptrOf(apigen.SheetTransferIssueAction(is.Action))
		}
		out = append(out, item)
	}
	return out
}

func ptrOf[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func derefSlice(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// truncateStr 错误摘要截断（报告/日志纪律）。
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
