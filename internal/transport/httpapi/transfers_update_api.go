// 表格回写 HTTP 面（RFC-0017 M3）：update 预览 / 异步执行（202）/ 单条轮询 /
// 取消 / 续跑。错误面沿 M2 + sheet_transfer_target_changed。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

// updateRequestOf 生成侧请求 → 域请求。
func updateRequestOf(docID, provider, externalURL string, sheetNames *[]string) extdocs.UpdateRequest {
	var names []string
	if sheetNames != nil {
		names = *sheetNames
	}
	return extdocs.UpdateRequest{DocID: docID, Provider: provider, ExternalURL: externalURL, SheetNames: names}
}

// PreviewSheetTransferUpdate POST /v1/extdocs/sheet-transfers/update/preview。
func (s *server) PreviewSheetTransferUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.transfersAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.SheetTransferUpdatePreviewRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷不合法: "+err.Error())
		return
	}
	tr := updateRequestOf(req.DocId, req.Provider, req.ExternalUrl, req.SheetNames)
	pv, err := s.deps.SheetTransfers.UpdatePreview(r.Context(), tr)
	if err != nil {
		s.writeTransferError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sheetUpdatePreviewOut(pv))
}

// ExecuteSheetTransferUpdate POST /v1/extdocs/sheet-transfers/update（202 异步）。
func (s *server) ExecuteSheetTransferUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.transfersAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req apigen.SheetTransferUpdateExecuteRequest
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
	tr := updateRequestOf(req.DocId, req.Provider, req.ExternalUrl, req.SheetNames)
	tr.TargetHash = deref(req.TargetHash)
	tr.SourceVersion = deref(req.SourceVersion)
	tr.IdempotencyKey = req.IdempotencyKey
	tr.Actor = s.deps.Actor.ParticipantID
	rec, err := s.deps.SheetTransfers.ExecuteUpdate(r.Context(), tr)
	if err != nil {
		s.writeTransferError(w, err)
		return
	}
	// 零差异 → 终态 200；否则 202（异步受理）。
	status := http.StatusAccepted
	if rec.Status != extdocs.TransferRunning {
		status = http.StatusOK
	}
	writeJSON(w, status, sheetTransferRecordOut(rec))
}

// GetSheetTransfer GET /v1/extdocs/sheet-transfers/{transfer_id}（轮询）。
func (s *server) GetSheetTransfer(w http.ResponseWriter, r *http.Request, transferId string) {
	if !s.transfersAvailable(w) {
		return
	}
	rec, err := s.deps.SheetTransfers.Get(r.Context(), transferId)
	if err != nil {
		s.writeTransferError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sheetTransferRecordOut(rec))
}

// CancelSheetTransfer POST .../cancel（块间生效；幂等）。
func (s *server) CancelSheetTransfer(w http.ResponseWriter, r *http.Request, transferId string) {
	if !s.transfersAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	rec, err := s.deps.SheetTransfers.CancelUpdate(r.Context(), transferId)
	if err != nil {
		if errors.Is(err, extdocs.ErrTransferNotResumable) {
			writeError(w, http.StatusConflict, "sheet_transfer_not_cancelable", "非回写方向或不可取消")
			return
		}
		s.writeTransferError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sheetTransferRecordOut(rec))
}

// ResumeSheetTransfer POST .../resume（校验未写区域前态一致后续跑）。
func (s *server) ResumeSheetTransfer(w http.ResponseWriter, r *http.Request, transferId string) {
	if !s.transfersAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	rec, err := s.deps.SheetTransfers.ResumeUpdate(r.Context(), transferId)
	if err != nil {
		switch {
		case errors.Is(err, extdocs.ErrTargetChanged):
			writeError(w, http.StatusConflict, "sheet_transfer_target_changed", err.Error())
		case errors.Is(err, extdocs.ErrTransferNotResumable):
			writeError(w, http.StatusConflict, "sheet_transfer_not_resumable", err.Error())
		case errors.Is(err, extdocs.ErrStalePreview):
			writeError(w, http.StatusConflict, "sheet_transfer_stale_preview", err.Error())
		default:
			s.writeTransferError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, sheetTransferRecordOut(rec))
}

// sheetUpdatePreviewOut 域预览 → 契约视图。
func sheetUpdatePreviewOut(pv *extdocs.UpdatePreview) apigen.SheetTransferUpdatePreview {
	out := apigen.SheetTransferUpdatePreview{
		Direction:     apigen.SheetTransferUpdatePreviewDirection(extdocs.TransferUpdate),
		Provider:      pv.Provider,
		DocId:         pv.DocID,
		ExternalUrl:   pv.ExternalURL,
		SourceVersion: pv.SourceVer,
		TargetHash:    pv.TargetHash,
		CellsTotal:    pv.CellsTotal,
		Diffs:         make([]apigen.SheetTransferUpdateDiffSheet, 0, len(pv.Diffs)),
		Issues:        sheetTransferIssuesOut(pv.Issues),
	}
	if pv.Title != "" {
		out.Title = &pv.Title
	}
	for _, d := range pv.Diffs {
		out.Diffs = append(out.Diffs, apigen.SheetTransferUpdateDiffSheet{
			Name: d.Name, ToWrite: d.ToWrite, FormulaDiffs: d.FormulaDiffs, RemoteExtra: d.RemoteExtra,
		})
	}
	return out
}
