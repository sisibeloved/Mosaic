// UT 层：表格读端点（RFC-0017 M1）——sheets 元信息/范围读取（used range 缺省
// 与显式 A1 参数）、格式门（markdown 409）、commit_sheet_revision 命令往返与
// 409、CSV 导出分派、快照不整簿出网（无 workbook 键）。
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

const sheetUUID1 = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6f01"
const sheetUUID2 = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6f02"
const sheetUUID3 = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6f03"

// createTestSheet 经 API 建表格文档（format=spreadsheet，version=1）。
func createTestSheet(t *testing.T, ts *httptest.Server, idem, title string, headers map[string]string) apigen.DocCommandResponse {
	t.Helper()
	resp, created := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, idem, map[string]any{"title": title, "format": "spreadsheet"}), headers)
	if resp.StatusCode != 200 || created.DocId == "" || created.DocVersion != 1 {
		t.Fatalf("create sheet doc: %d %+v", resp.StatusCode, created)
	}
	return created
}

func sheetIDOf(docID string) string { return "sht_" + docID[len("doc_"):] }

func getJSONMap(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func TestSheetEndpointsRoundTrip(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	created := createTestSheet(t, ts, sheetUUID1, "预算表", nil)
	docID := created.DocId
	sheetID := sheetIDOf(docID)

	// 提交首批数据（含公式）
	resp, _ := postDocJSON(t, ts.URL+"/v1/docs/"+docID+"/commands", docCommand("commit_sheet_revision", 1, sheetUUID2, map[string]any{
		"ops": []map[string]any{
			{"op": "set_cells", "sheet_id": sheetID, "cells": []map[string]any{
				{"r": 1, "c": 1, "type": "text", "text": "项目"},
				{"r": 2, "c": 1, "type": "number", "number": 5},
				{"r": 3, "c": 1, "type": "formula", "formula": "A2*4"},
			}},
			{"op": "add_sheet", "name": "备份"},
		},
	}), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("commit: %d", resp.StatusCode)
	}

	// sheets 元信息
	code, m := getJSONMap(t, ts.URL+"/v1/docs/"+docID+"/sheets")
	if code != 200 || m["version"].(float64) != 2 {
		t.Fatalf("sheets: %d %v", code, m)
	}
	sheets := m["sheets"].([]any)
	if len(sheets) != 2 || sheets[1].(map[string]any)["name"] != "备份" {
		t.Fatalf("sheets 清单 = %v", sheets)
	}
	meta := sheets[0].(map[string]any)
	if meta["filled_cells"].(float64) != 3 || meta["rows"].(float64) < 32 {
		t.Fatalf("默认表 meta = %v", meta)
	}

	// used-range 缺省读取（公式派生值随行）
	code, m = getJSONMap(t, fmt.Sprintf("%s/v1/docs/%s/sheets/%s/range", ts.URL, docID, sheetID))
	if code != 200 {
		t.Fatalf("range: %d %v", code, m)
	}
	cells := m["cells"].([]any)
	if len(cells) != 3 {
		t.Fatalf("cells = %v", cells)
	}
	var formulaDerived float64
	for _, c := range cells {
		cm := c.(map[string]any)
		if cm["type"] == "formula" {
			d := cm["derived"].(map[string]any)
			if d["kind"] != "value" {
				t.Fatalf("derived = %v", d)
			}
			formulaDerived = d["value"].(map[string]any)["number"].(float64)
		}
	}
	if formulaDerived != 20 {
		t.Fatalf("派生值 = %v", formulaDerived)
	}

	// 显式范围
	code, m = getJSONMap(t, fmt.Sprintf("%s/v1/docs/%s/sheets/%s/range?range=A1:A2", ts.URL, docID, sheetID))
	if code != 200 || len(m["cells"].([]any)) != 2 {
		t.Fatalf("显式范围: %d %v", code, m)
	}
	// 坏 range / 未知表
	if code, _ := getJSONMap(t, fmt.Sprintf("%s/v1/docs/%s/sheets/%s/range?range=ZZZ9", ts.URL, docID, sheetID)); code != 400 {
		t.Fatalf("坏 range = %d", code)
	}
	if code, _ := getJSONMap(t, fmt.Sprintf("%s/v1/docs/%s/sheets/sht_missing/range", ts.URL, docID)); code != 404 {
		t.Fatalf("未知表 = %d", code)
	}

	// 快照不整簿出网
	code, m = getJSONMap(t, ts.URL+"/v1/docs/"+docID)
	if code != 200 {
		t.Fatal("getDoc")
	}
	if _, leak := m["workbook"]; leak {
		t.Fatal("快照泄漏整簿 workbook")
	}
	if m["format"] != "spreadsheet" || len(m["sheets"].([]any)) != 2 {
		t.Fatalf("快照分派 = %v", m)
	}
	if _, hasBlocks := m["blocks"]; hasBlocks {
		t.Fatal("spreadsheet 快照不应携带 blocks")
	}

	// CSV 导出分派（公式格写派生值 20）
	resp2, err := http.Get(ts.URL + "/v1/docs/" + docID + "/export")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp2.Body.Close() })
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 200 || resp2.Header.Get("Content-Type") != "text/csv; charset=utf-8" || string(body) == "" || !strings.Contains(string(body), "20") {
		t.Fatalf("CSV 导出: %d %s", resp2.StatusCode, body)
	}
}

func TestSheetFormatGateAndConflict(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "tok")

	// markdown 文档：sheets/range 409、commit 拒绝
	hdr := map[string]string{"X-Owner-Token": "tok"}
	resp, created := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, sheetUUID1, map[string]any{"title": "md"}), hdr)
	if resp.StatusCode != 200 {
		t.Fatal("create md")
	}
	docID := created.DocId
	if code, _ := getJSONMap(t, ts.URL+"/v1/docs/"+docID+"/sheets"); code != 409 {
		t.Fatalf("markdown sheets = %d", code)
	}

	// 建表（写门生效：无 token 401）
	if resp, _ := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, sheetUUID2, map[string]any{"title": "s", "format": "spreadsheet"}), nil); resp.StatusCode != 401 {
		t.Fatalf("写门 = %d", resp.StatusCode)
	}
	created2 := createTestSheet(t, ts, sheetUUID2, "表格", hdr)
	sDocID := created2.DocId
	sheetID := sheetIDOf(sDocID)

	// 写门：commit 无 token 401
	if resp, _ := postDocJSON(t, ts.URL+"/v1/docs/"+sDocID+"/commands", docCommand("commit_sheet_revision", 1, sheetUUID3, map[string]any{
		"ops": []map[string]any{{"op": "set_cells", "sheet_id": sheetID, "cells": []map[string]any{{"r": 1, "c": 1, "type": "number", "number": 1}}}},
	}), nil); resp.StatusCode != 401 {
		t.Fatalf("写门 commit = %d", resp.StatusCode)
	}

	// 带 token 提交后，旧版本 409 携带当前态（sheets 视图）
	resp3, _ := postDocJSON(t, ts.URL+"/v1/docs/"+sDocID+"/commands", docCommand("commit_sheet_revision", 1, sheetUUID3, map[string]any{
		"ops": []map[string]any{{"op": "set_cells", "sheet_id": sheetID, "cells": []map[string]any{{"r": 1, "c": 1, "type": "number", "number": 1}}}},
	}), hdr)
	if resp3.StatusCode != 200 {
		t.Fatalf("commit = %d", resp3.StatusCode)
	}
	resp4, _ := postDocJSON(t, ts.URL+"/v1/docs/"+sDocID+"/commands", docCommand("commit_sheet_revision", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6f04", map[string]any{
		"ops": []map[string]any{{"op": "add_sheet", "name": "x"}},
	}), hdr)
	if resp4.StatusCode != 409 {
		t.Fatalf("409 = %d", resp4.StatusCode)
	}
	raw, _ := io.ReadAll(resp4.Body)
	var conflict struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		State *struct {
			Sheets []map[string]any `json:"sheets"`
		} `json:"state"`
	}
	_ = json.Unmarshal(raw, &conflict)
	if conflict.Error.Code != "version_conflict" || conflict.State == nil || len(conflict.State.Sheets) != 1 {
		t.Fatalf("409 体 = %s", raw)
	}
}
