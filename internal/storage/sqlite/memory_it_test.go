// 分域记忆存储 IT（0.7.0 / ADR-0037 / migration v32）：新库建表与登记、
// CRUD/过滤/游标（与 memstore 同语义）、幂等重开（旧库升级面——v32 重放）。
package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/memory"
)

func openMemItStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mem-it.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func memIt(ns memory.Namespace, scope, title string) memory.Item {
	return memory.Item{
		MemoryID: "mem_it_" + title, Version: 1, Namespace: ns, ScopeID: scope,
		Kind: memory.KindSemantic, Title: title, Content: "c-" + title,
		SourceRefs: []string{"room:evt_" + title}, ObservedAt: "2026-11-04T10:00:00Z",
		Confidence: 1.0, Status: memory.StatusActive, Visibility: "namespace",
		Sensitivity: memory.SensNormal, Supersedes: []string{},
		CreatedAt: "2026-11-04T12:00:00Z", UpdatedAt: "2026-11-04T12:00:00Z",
	}
}

func TestMemoryItemsMigrationV32IT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	var got int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM migrations WHERE version = 32`).Scan(&got); err != nil || got != 1 {
		t.Fatalf("v32 未登记：%v %d", err, got)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_items'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("memory_items 未建：%v %d", err, n)
	}
}

func TestMemoryItemsCRUDAndCursorIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	// agent 3 条 + mission 1 条（异域）+ proposed 1 条（同域异状态）。
	for _, title := range []string{"a1", "a2", "a3"} {
		if _, err := s.CreateItem(ctx, memIt(memory.NSAgent, "ag_1", title)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateItem(ctx, memIt(memory.NSMission, "m_1", "m1")); err != nil {
		t.Fatal(err)
	}
	proposed := memIt(memory.NSAgent, "ag_1", "p1")
	proposed.MemoryID, proposed.Status = "mem_it_proposed", memory.StatusProposed
	if _, err := s.CreateItem(ctx, proposed); err != nil {
		t.Fatal(err)
	}

	// 单查。
	it, ok, err := s.ItemByID(ctx, "mem_it_a2")
	if err != nil || !ok || it.Title != "a2" || it.SourceRefs[0] != "room:evt_a2" {
		t.Fatalf("单查错：%v %v %+v", err, ok, it)
	}
	if _, ok, _ := s.ItemByID(ctx, "mem_404"); ok {
		t.Fatal("未知项应 ok=false")
	}

	// 默认 active 域隔离：agent 3 条降序。
	items, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 50})
	if err != nil || len(items) != 3 {
		t.Fatalf("agent active 应 3 条，得 %d %v", len(items), err)
	}
	if items[0].Title != "a3" || items[2].Title != "a1" {
		t.Fatalf("rowid 降序错：%s %s", items[0].Title, items[2].Title)
	}
	// proposed 不入默认口径；显式请求可见。
	items, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusProposed, Limit: 50})
	if err != nil || len(items) != 1 || items[0].MemoryID != "mem_it_proposed" {
		t.Fatalf("proposed 口径错：%d %v", len(items), err)
	}
	// scope/kind 过滤。
	if items, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, ScopeID: "ag_404", Limit: 50}); err != nil || len(items) != 0 {
		t.Fatal("scope 过滤应空")
	}
	if items, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Kind: memory.KindEpisodic, Limit: 50}); err != nil || len(items) != 0 {
		t.Fatal("kind 过滤应空")
	}

	// 游标分页（limit=2）：Store 返回 limit+1 条供 service 截断；次页
	// before=mem_it_a2 → [a1]。
	p1, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 2})
	if err != nil || len(p1) != 3 {
		t.Fatalf("首页应 limit+1 条，得 %d %v", len(p1), err)
	}
	if p1[0].Title != "a3" || p1[1].Title != "a2" || p1[2].Title != "a1" {
		t.Fatal("首页序错")
	}
	p2, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 2, Before: "mem_it_a2"})
	if err != nil || len(p2) != 1 || p2[0].Title != "a1" {
		t.Fatalf("次页错：%d %v", len(p2), err)
	}
	// 游标失效 = 空页。
	p3, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 2, Before: "mem_deleted"})
	if err != nil || len(p3) != 0 {
		t.Fatalf("游标失效应空页：%d %v", len(p3), err)
	}
}

func TestMemoryItemsIdempotentReopenIT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateItem(ctx, memIt(memory.NSWorkspace, memory.WorkspaceScope, "w1")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// 模拟旧库（v31 面）：删表 + 撤销 v32 登记，重开走迁移重放。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.ExecContext(ctx, "DROP TABLE memory_items"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.ExecContext(ctx, "DELETE FROM migrations WHERE version = 32"); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	// v32 重放：表恢复 + 数据可再写（旧行已随 DROP 丢失——幂等重放语义）。
	if _, err := s3.CreateItem(ctx, memIt(memory.NSWorkspace, memory.WorkspaceScope, "w2")); err != nil {
		t.Fatalf("重放后不可写：%v", err)
	}
	it, ok, err := s3.ItemByID(ctx, "mem_it_w2")
	if err != nil || !ok || it.Title != "w2" {
		t.Fatalf("重放后读回错：%v %v", err, ok)
	}
}
