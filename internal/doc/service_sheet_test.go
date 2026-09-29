// UT 层：doc 域表格分派（RFC-0017 M1）——spreadsheet 创建/表格修订命令
// （CAS/幂等/格式门/归档只读）、agent 代写面（CreateSheetDoc/CommitSheetRevision）、
// 副本重放保真、CSV 导出、范围读取与摘录分派。
package doc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// createSheetForTest 建表格文档；返回 (docID, 默认表 ID)。
func createSheetForTest(t *testing.T, svc *Service, idem string) (string, string) {
	t.Helper()
	res, err := svc.ExecuteCommand(context.Background(), humanActor, docCmd("create_doc", "", idem, 0, map[string]any{
		"title": "测试表格", "format": "spreadsheet",
	}))
	if err != nil {
		t.Fatalf("create_doc(spreadsheet): %v", err)
	}
	return res.DocID, "sht_" + strings.TrimPrefix(res.DocID, "doc_")
}

func TestSheetCreateFormatGate(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()

	// markdown 缺省不受影响
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", "", validUUIDv7, 0, map[string]any{"title": "md"})); err != nil {
		t.Fatalf("markdown create: %v", err)
	}
	// spreadsheet 拒绝 initial_blocks
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", "", validUUIDv7b, 0, map[string]any{
		"title": "s", "format": "spreadsheet",
		"initial_blocks": []map[string]any{{"block_id": "b", "type": "paragraph", "text": "x"}},
	})); err == nil {
		t.Fatal("spreadsheet + initial_blocks 应拒绝")
	}
	// html 仍拒绝
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", "", validUUIDv7c, 0, map[string]any{"title": "x", "format": "html"})); err == nil {
		t.Fatal("html 应拒绝")
	}
	docID, sheetID := createSheetForTest(t, svc, validUUIDv7d)
	state, err := svc.GetDoc(ctx, docID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Format != FormatSpreadsheet || state.Workbook == nil || len(state.Workbook.Sheets) != 1 {
		t.Fatalf("表格初态 = %+v", state.Format)
	}
	if state.Workbook.Sheets[0].SheetID != sheetID || state.Workbook.Sheets[0].Name != "Sheet1" {
		t.Fatalf("默认表 = %+v", state.Workbook.Sheets[0])
	}
}

func TestSheetRevisionCommandLifecycle(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID, sheetID := createSheetForTest(t, svc, validUUIDv7)

	num := 7.0
	firstPayload := map[string]any{
		"ops": []map[string]any{
			{"op": "set_cells", "sheet_id": sheetID, "cells": []map[string]any{
				{"r": 1, "c": 1, "type": "number", "number": num},
				{"r": 1, "c": 2, "type": "formula", "formula": "A1*6"},
			}},
			{"op": "add_sheet", "name": "二号"},
		},
	}
	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_sheet_revision", docID, validUUIDv7b, 1, firstPayload))
	if err != nil {
		t.Fatalf("commit_sheet_revision: %v", err)
	}
	if res.DocVersion != 2 {
		t.Fatalf("版本 = %d", res.DocVersion)
	}
	state, _ := svc.GetDoc(ctx, docID)
	if len(state.Workbook.Sheets) != 2 || state.Workbook.Sheets[1].Name != "二号" {
		t.Fatal("add_sheet 未生效")
	}
	if got := state.Workbook.Sheets[0].CellAt(1, 2); got == nil || got.Formula != "A1*6" {
		t.Fatalf("公式格 = %+v", got)
	}

	// 幂等回放（同键同指纹）
	res2, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_sheet_revision", docID, validUUIDv7b, 1, firstPayload))
	if err != nil || !res2.Replayed || res2.DocVersion != 2 {
		t.Fatalf("幂等回放 = %+v err=%v", res2, err)
	}

	// CAS 冲突（旧版本号）
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("commit_sheet_revision", docID, validUUIDv7c, 1, map[string]any{
		"ops": []map[string]any{{"op": "set_cells", "sheet_id": sheetID, "cells": []map[string]any{{"r": 2, "c": 1, "type": "null"}}}},
	}))
	var vc *VersionConflictError
	if !errors.As(err, &vc) || vc.Current != 2 {
		t.Fatalf("CAS 冲突 = %v", err)
	}

	// markdown 文档拒绝表格修订
	mdID := createForTest(t, svc, validUUIDv7c)
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_sheet_revision", mdID, validUUIDv7d, 1, map[string]any{
		"ops": []map[string]any{{"op": "add_sheet", "name": "x"}},
	})); err == nil {
		t.Fatal("markdown 文档应拒绝表格修订")
	}

	// 归档只读
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("archive_doc", docID, validUUIDv7d, 2, map[string]any{})); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_sheet_revision", docID, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e73", 3, map[string]any{
		"ops": []map[string]any{{"op": "add_sheet", "name": "三号"}},
	})); !errors.Is(err, ErrDocArchived) {
		t.Fatalf("归档只读 = %v", err)
	}
}

func TestSheetAgentPaths(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()

	// CreateSheetDoc 带初始格
	num := 3.0
	res, err := svc.CreateSheetDoc(ctx, botActor, "机器人预算表", []protocol.SheetCell{
		{R: 1, C: 1, Type: "number", Number: &num},
		{R: 2, C: 1, Type: "formula", Formula: "A1+1"},
	}, "agent", "首轮")
	if err != nil {
		t.Fatalf("CreateSheetDoc: %v", err)
	}
	state, _ := svc.GetDoc(ctx, res.DocID)
	if state.Format != FormatSpreadsheet || state.Workbook.Sheets[0].CellAt(2, 1) == nil {
		t.Fatal("agent 建表初始格未落")
	}

	// CommitSheetRevision：结构操作归一化回填改写公式
	sheetID := state.Workbook.Sheets[0].SheetID
	if _, err := svc.CommitSheetRevision(ctx, botActor, res.DocID, state.Version, []protocol.SheetOp{
		{Op: "insert_rows", SheetID: sheetID, At: 1, Count: 1},
	}, "agent", "插行"); err != nil {
		t.Fatalf("CommitSheetRevision: %v", err)
	}
	state, _ = svc.GetDoc(ctx, res.DocID)
	if got := state.Workbook.Sheets[0].CellAt(3, 1); got == nil || got.Formula != "A2+1" {
		t.Fatalf("改写公式 = %+v", got)
	}

	// 旧 base_version → 版本冲突
	if _, err := svc.CommitSheetRevision(ctx, botActor, res.DocID, state.Version-1, []protocol.SheetOp{
		{Op: "add_sheet", Name: "x"},
	}, "agent", ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本冲突 = %v", err)
	}
}

func TestSheetDuplicateAndExport(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID, sheetID := createSheetForTest(t, svc, validUUIDv7)

	num := 2.0
	batches := []struct {
		idem    string
		version int64
		payload map[string]any
	}{
		{validUUIDv7b, 1, map[string]any{
			"ops": []map[string]any{{"op": "set_cells", "sheet_id": sheetID, "cells": []map[string]any{
				{"r": 1, "c": 1, "type": "number", "number": num},
				{"r": 1, "c": 2, "type": "formula", "formula": "A1*10"},
			}}}}},
		{validUUIDv7c, 2, map[string]any{
			"ops": []map[string]any{{"op": "insert_rows", "sheet_id": sheetID, "at": 1, "count": 1}}}},
	}
	for _, b := range batches {
		if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_sheet_revision", docID, b.idem, b.version, b.payload)); err != nil {
			t.Fatalf("提交（v%d）: %v", b.version, err)
		}
	}
	state, err := svc.GetDoc(ctx, docID)
	if err != nil {
		t.Fatal(err)
	}
	// 结构改写已生效：A1*10 → A2*10 且公式格平移到 2,2
	if got := state.Workbook.Sheets[0].CellAt(2, 2); got == nil || got.Formula != "A2*10" {
		t.Fatalf("改写后公式 = %+v", got)
	}

	dup, err := svc.ExecuteCommand(ctx, humanActor, docCmd("duplicate_doc", docID, validUUIDv7d, state.Version, map[string]any{}))
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	dupState, _ := svc.GetDoc(ctx, dup.DocID)
	if dupState.Format != FormatSpreadsheet || dupState.Workbook == nil {
		t.Fatal("副本格式")
	}
	origCell := state.Workbook.Sheets[0].CellAt(2, 2)
	dupCell := dupState.Workbook.Sheets[0].CellAt(2, 2)
	if origCell == nil || dupCell == nil || dupCell.Formula != origCell.Formula {
		t.Fatalf("副本公式保真：%+v vs %+v", origCell, dupCell)
	}

	// markdown 导出对表格拒绝；CSV 导出可用（公式格写派生值 20）
	if _, _, err := svc.ExportDoc(ctx, docID); err == nil {
		t.Fatal("表格走 markdown 导出应拒绝")
	}
	csv, _, err := svc.ExportSheetCSV(ctx, docID, "")
	if err != nil || !strings.Contains(csv, "20") {
		t.Fatalf("CSV = %q err=%v", csv, err)
	}
}

func TestSheetReadRangeAndView(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID, sheetID := createSheetForTest(t, svc, validUUIDv7)

	num := 5.0
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_sheet_revision", docID, validUUIDv7b, 1, map[string]any{
		"ops": []map[string]any{{"op": "set_cells", "sheet_id": sheetID, "cells": []map[string]any{
			{"r": 1, "c": 1, "type": "number", "number": num},
			{"r": 2, "c": 1, "type": "formula", "formula": "A1*2"},
		}}},
	})); err != nil {
		t.Fatal(err)
	}
	// used range 缺省
	view, err := svc.ReadSheetRange(ctx, docID, sheetID, nil)
	if err != nil || len(view.Cells) != 2 || view.Range.R2 != 2 {
		t.Fatalf("used range = %+v err=%v", view, err)
	}
	var derivedVal string
	for _, c := range view.Cells {
		if c.Type == "formula" {
			if c.Derived == nil || c.Derived.Kind != "value" || c.Derived.Value == nil || *c.Derived.Value.Number != 10 {
				t.Fatalf("派生 = %+v", c.Derived)
			}
		}
	}
	_ = derivedVal
	// 显式范围 + 未知表 + markdown 文档
	r := protocol.SheetRange{R1: 1, C1: 1, R2: 1, C2: 1}
	if v, err := svc.ReadSheetRange(ctx, docID, sheetID, &r); err != nil || len(v.Cells) != 1 {
		t.Fatalf("显式范围 = %+v err=%v", v, err)
	}
	if _, err := svc.ReadSheetRange(ctx, docID, "sht_missing", nil); err == nil {
		t.Fatal("未知表应拒绝")
	}
	mdID := createForTest(t, svc, validUUIDv7c)
	if _, err := svc.ReadSheetRange(ctx, mdID, "sht_x", nil); err == nil {
		t.Fatal("markdown 应拒绝")
	}
	// 摘录分派
	state, _ := svc.GetDoc(ctx, docID)
	ex := RenderExcerpt(state, 8000, nil)
	if !strings.Contains(ex, "[Sheet1]") || !strings.Contains(ex, "10") {
		t.Fatalf("表格摘录 = %q", ex)
	}
}
