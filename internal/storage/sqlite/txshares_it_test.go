//go:build it

// IT 层：v5 doc_external_shares 表（RFC-0015 §2.2）——CRUD 端口契约、
// Upsert 替换语义、迁移幂等（存量库重开）。
package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/extdocs"
)

func TestExternalSharesCRUD(t *testing.T) {
	store, path := openTempStore(t)
	ctx := context.Background()

	sh := extdocs.Share{
		DocID: "doc_000000000001", Provider: extdocs.ProviderTencent,
		ExternalURL: "https://docs.qq.com/aio/p/first", SharedBy: "par_owner", SharedAt: "2026-09-24T12:00:00Z",
	}
	if err := store.UpsertExternalShare(ctx, sh); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Get 命中
	got, err := store.GetExternalShare(ctx, sh.DocID, extdocs.ProviderTencent)
	if err != nil || got.ExternalURL != sh.ExternalURL {
		t.Fatalf("get = %+v, err = %v", got, err)
	}

	// Get 未命中 → 哨兵
	if _, err := store.GetExternalShare(ctx, "doc_000000000002", extdocs.ProviderTencent); !errors.Is(err, extdocs.ErrShareNotFound) {
		t.Fatalf("miss err = %v", err)
	}

	// Upsert 替换（force 重共享语义）
	sh.ExternalURL = "https://docs.qq.com/aio/p/second"
	if err := store.UpsertExternalShare(ctx, sh); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	list, err := store.ListExternalShares(ctx, sh.DocID, "")
	if err != nil || len(list) != 1 || list[0].ExternalURL != "https://docs.qq.com/aio/p/second" {
		t.Fatalf("list = %+v, err = %v", list, err)
	}

	// 第二文档 + 全量列表
	sh2 := sh
	sh2.DocID = "doc_000000000002"
	sh2.ExternalURL = "https://docs.qq.com/aio/p/third"
	if err := store.UpsertExternalShare(ctx, sh2); err != nil {
		t.Fatalf("upsert3: %v", err)
	}
	all, err := store.ListExternalShares(ctx, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v, err = %v", all, err)
	}

	// Delete 命中 / 再删哨兵
	if err := store.DeleteExternalShare(ctx, sh.DocID, extdocs.ProviderTencent); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteExternalShare(ctx, sh.DocID, extdocs.ProviderTencent); !errors.Is(err, extdocs.ErrShareNotFound) {
		t.Fatalf("re-delete err = %v", err)
	}

	// 迁移幂等：重开同一库（schemaDDL IF NOT EXISTS + migrations INSERT OR IGNORE）
	store.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.GetExternalShare(ctx, sh2.DocID, extdocs.ProviderTencent); err != nil {
		t.Fatalf("reopen 后映射应存活: %v", err)
	}
}
