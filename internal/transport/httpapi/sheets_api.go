// 表格读端点（RFC-0017 M1）：工作表元信息清单 + 有界区域读取（类型化格 +
// 公式原文 + 服务端权威派生值）。只读不设写门；spreadsheet 之外文档 409。
package httpapi

import (
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/sheet"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

// ListDocSheets 工作表元信息（GET /v1/docs/{doc_id}/sheets）。
func (s *server) ListDocSheets(w http.ResponseWriter, r *http.Request, docID apigen.DocID) {
	if !s.docsAvailable(w) {
		return
	}
	state, err := s.deps.DocSVC.GetDoc(r.Context(), docID)
	if err != nil {
		s.writeDocDomainError(w, r, docID, err)
		return
	}
	if state.Format != doc.FormatSpreadsheet || state.Workbook == nil {
		writeError(w, http.StatusConflict, "not_spreadsheet", "该文档不是表格（format != spreadsheet）")
		return
	}
	sheets := []sheetMetaOut{}
	for _, sh := range state.Workbook.Sheets {
		sheets = append(sheets, sheetMetaOut{
			SheetID: sh.SheetID, Name: sh.Name, Rows: sh.Rows, Cols: sh.Cols,
			FilledCells: sh.FilledCells(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"doc_id": state.DocID, "version": state.Version, "sheets": sheets,
	})
}

// GetDocSheetRange 区域读取（GET /v1/docs/{doc_id}/sheets/{sheet_id}/range）。
func (s *server) GetDocSheetRange(w http.ResponseWriter, r *http.Request, docID apigen.DocID, sheetID string, params apigen.GetDocSheetRangeParams) {
	if !s.docsAvailable(w) {
		return
	}
	var rng *protocol.SheetRange
	if params.Range != nil && *params.Range != "" {
		r1, c1, r2, c2, ok := sheet.ParseRange(*params.Range)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_range", "range 须为 A1:B2 形态（列 ≤GR、行 ≤5000）")
			return
		}
		rng = &protocol.SheetRange{R1: r1, C1: c1, R2: r2, C2: c2}
	}
	view, err := s.deps.DocSVC.ReadSheetRange(r.Context(), docID, sheetID, rng)
	if err != nil {
		s.writeDocDomainError(w, r, docID, err)
		return
	}
	writeJSON(w, http.StatusOK, sheetRangeViewOut(view))
}

// sheetRangeViewOut 域视图 → 出网 DTO（派生值随公式格携带）。
func sheetRangeViewOut(v doc.SheetRangeView) map[string]any {
	cells := make([]map[string]any, 0, len(v.Cells))
	for _, c := range v.Cells {
		m := map[string]any{"r": c.R, "c": c.C, "type": c.Type}
		if c.Text != "" {
			m["text"] = c.Text
		}
		if c.Number != nil {
			m["number"] = *c.Number
		}
		if c.Bool != nil {
			m["bool"] = *c.Bool
		}
		if c.Formula != "" {
			m["formula"] = c.Formula
		}
		if c.Style != nil {
			m["style"] = c.Style
		}
		if c.Derived != nil {
			d := map[string]any{"kind": c.Derived.Kind}
			if c.Derived.Err != "" {
				d["err"] = c.Derived.Err
			}
			if c.Derived.Value != nil {
				dv := map[string]any{"type": c.Derived.Value.Type}
				if c.Derived.Value.Text != "" {
					dv["text"] = c.Derived.Value.Text
				}
				if c.Derived.Value.Number != nil {
					dv["number"] = *c.Derived.Value.Number
				}
				if c.Derived.Value.Bool != nil {
					dv["bool"] = *c.Derived.Value.Bool
				}
				d["value"] = dv
			}
			m["derived"] = d
		}
		cells = append(cells, m)
	}
	return map[string]any{
		"doc_id": v.DocID, "sheet_id": v.SheetID, "version": v.Version,
		"range": v.Range, "cells": cells, "truncated": v.Truncated,
	}
}
