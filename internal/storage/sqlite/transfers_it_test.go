//go:build it

// IT 层：v6 sheet_transfers 表（RFC-0017 M2）——CRUD 端口契约、幂等键唯一、
// running 残留收敛、存量库重开幂等。
package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/extdocs"
)

func TestSheetTransfersCRUD(t *testing.T) {
	store, path := openTempStore(t)
	ctx := context.Background()

	rec := extdocs.SheetTransferRecord{
		TransferID: "xfr_000000000001", IdempotencyKey: "key-1",
		Direction: extdocs.TransferImport, Provider: extdocs.ProviderTencent,
		Status: extdocs.TransferRunning, Title: "t", ExternalURL: "https://docs.qq.com/sheet/X",
		ReportJSON: "[]", CreatedAt: "2026-09-29T10:00:00Z", UpdatedAt: "2026-09-29T10:00:00Z",
	}
	if err := store.InsertSheetTransfer(ctx, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 幂等键唯一：异参同键 → ErrTransferKeyConflict
	rec2 := rec
	rec2.TransferID, rec2.Direction = "xfr_000000000002", extdocs.TransferExport
	if err := store.InsertSheetTransfer(ctx, rec2); !errors.Is(err, extdocs.ErrTransferKeyConflict) {
		t.Fatalf("key conflict err = %v", err)
	}
	// Get by id / key
	got, err := store.GetSheetTransfer(ctx, rec.TransferID)
	if err != nil || got.IdempotencyKey != "key-1" || got.Direction != extdocs.TransferImport {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	got, err = store.GetSheetTransferByKey(ctx, "key-1")
	if err != nil || got.TransferID != rec.TransferID {
		t.Fatalf("get by key = %+v err=%v", got, err)
	}
	if _, err := store.GetSheetTransferByKey(ctx, "nope"); !errors.Is(err, extdocs.ErrTransferNotFound) {
		t.Fatalf("notfound err = %v", err)
	}
	// 终态更新（含 M3 回写进度/状态列）
	got.Status = extdocs.TransferSucceededWithWarning
	got.DocID = "doc_000000000009"
	got.ReportJSON = `[{"feature":"x"}]`
	got.CellsTotal, got.CellsWritten = 750, 500
	got.StateJSON = `{"done":1,"chunks":[{"sheet":"S"}]}`
	if err := store.UpdateSheetTransfer(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	// List 过滤：doc 命中 / 方向过滤
	list, err := store.ListSheetTransfers(ctx, "doc_000000000009", "", 0)
	if err != nil || len(list) != 1 || list[0].Status != extdocs.TransferSucceededWithWarning {
		t.Fatalf("list(doc) = %+v err=%v", list, err)
	}
	if list[0].CellsTotal != 750 || list[0].CellsWritten != 500 || list[0].StateJSON == "" {
		t.Fatalf("进度/状态列 = %+v", list[0])
	}
	list, err = store.ListSheetTransfers(ctx, "", extdocs.TransferExport, 0)
	if err != nil || len(list) != 0 {
		t.Fatalf("list(export) = %+v err=%v", list, err)
	}
	// running 残留 → unknown
	stale := extdocs.SheetTransferRecord{
		TransferID: "xfr_000000000003", IdempotencyKey: "key-3",
		Direction: extdocs.TransferExport, Provider: extdocs.ProviderFeishu,
		Status: extdocs.TransferRunning, ReportJSON: "[]",
		CreatedAt: "2026-09-29T11:00:00Z", UpdatedAt: "2026-09-29T11:00:00Z",
	}
	if err := store.InsertSheetTransfer(ctx, stale); err != nil {
		t.Fatalf("insert stale: %v", err)
	}
	n, err := store.MarkStaleRunningTransfers(ctx, "2026-09-29T12:00:00Z")
	if err != nil || n != 1 {
		t.Fatalf("mark stale n=%d err=%v", n, err)
	}
	got, _ = store.GetSheetTransfer(ctx, stale.TransferID)
	if got.Status != extdocs.TransferUnknown || got.Error == "" {
		t.Fatalf("stale = %+v", got)
	}
	// 二次扫描无残留
	if n, _ := store.MarkStaleRunningTransfers(ctx, "2026-09-29T12:01:00Z"); n != 0 {
		t.Fatalf("二次扫描 n=%d", n)
	}
	// 存量库重开（DDL 幂等 + 数据仍在）
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	list, err = store2.ListSheetTransfers(ctx, "", "", 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("reopen list = %+v err=%v", list, err)
	}
}
