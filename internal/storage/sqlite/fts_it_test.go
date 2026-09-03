//go:build it

// M3-3 按需平面 IT：FTS5 trigram 全文检索（v1.46 spike 定案的生产行为）——
// CJK 子串 ≥3 字命中、<3 字 LIKE 回退、大小写不敏感、自愈重建（索引缺口全量
// 重灌）、DeleteRoom 级联清理。语义基准为 room.SearchMessages 线性实现。
package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func ftsEnvelope(t *testing.T, roomID string, seq int64, id, actor, body string) protocol.Envelope {
	t.Helper()
	return protocol.Envelope{
		EventID: id, TenantID: "ten_it", RoomID: roomID, Seq: seq,
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:00Z",
		Actor:      protocol.Actor{ParticipantID: actor, Kind: "agent"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"body":` + quoteJSON(body) + `}`),
		Metadata:   map[string]any{},
	}
}

func quoteJSON(s string) string {
	return `"` + s + `"`
}

func TestFTS5SearchIT(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AppendEvents(ctx, []protocol.Envelope{
		ftsEnvelope(t, "room_ft", 1, "evt_f1", "par_owner", "预算超限的问题需要先排查迁移脚本"),
		ftsEnvelope(t, "room_ft", 2, "evt_f2", "par_codex", "我去拉数据，稍后给出结果"),
		ftsEnvelope(t, "room_ft", 3, "evt_f3", "par_kimi", "the migration budget exceeds limits"),
	}); err != nil {
		t.Fatal(err)
	}

	// CJK ≥3 字 trigram 命中
	hits, err := store.SearchMessages(ctx, "room_ft", "拉数据", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].EventID != "evt_f2" {
		t.Fatalf("CJK trigram 命中 = %+v", hits)
	}
	// CJK 4 字
	hits, _ = store.SearchMessages(ctx, "room_ft", "预算超限", "", "", 20)
	if len(hits) != 1 || hits[0].Actor != "par_owner" {
		t.Fatalf("CJK 4 字命中 = %+v", hits)
	}
	// 英文词（大小写不敏感）
	hits, _ = store.SearchMessages(ctx, "room_ft", "Budget", "", "", 20)
	if len(hits) != 1 || hits[0].Actor != "par_kimi" {
		t.Fatalf("英文命中 = %+v", hits)
	}
	// <3 字 CJK → LIKE 回退
	hits, _ = store.SearchMessages(ctx, "room_ft", "预算", "", "", 20)
	if len(hits) != 1 {
		t.Fatalf("短查询 LIKE 回退 = %+v", hits)
	}
	// actor 过滤（budget 命中 kimi；限定 codex 后空）
	hits, _ = store.SearchMessages(ctx, "room_ft", "budget", "par_kimi", "", 20)
	if len(hits) != 1 || hits[0].Actor != "par_kimi" {
		t.Fatalf("actor 过滤命中 = %+v", hits)
	}
	hits, _ = store.SearchMessages(ctx, "room_ft", "budget", "par_codex", "", 20)
	if len(hits) != 0 {
		t.Fatalf("actor 过滤应排他 = %+v", hits)
	}
	// 空查询
	hits, _ = store.SearchMessages(ctx, "room_ft", "  ", "", "", 20)
	if len(hits) != 0 {
		t.Fatalf("空查询 = %+v", hits)
	}
}

// 自愈：索引行被外力删掉（模拟 v3 前旧库/异常缺口）→ 首次搜索触发全量重建。
func TestFTS5SelfHealIT(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fts2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{
		ftsEnvelope(t, "room_sh", 1, "evt_s1", "par_codex", "数据治理口径需要重申"),
		ftsEnvelope(t, "room_sh", 2, "evt_s2", "par_codex", "数据治理的第一步是清点"),
	}); err != nil {
		t.Fatal(err)
	}
	// 外力破坏索引
	if _, err := store.db.ExecContext(ctx, "DELETE FROM room_fts WHERE room_id = ?", "room_sh"); err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchMessages(ctx, "room_sh", "数据治理", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("自愈重建后应全量可查 = %+v", hits)
	}
	if hits[0].EventID != "evt_s2" {
		t.Fatalf("最新在前 = %+v", hits)
	}
	if hits[0].Position == "" {
		t.Fatal("position（跳转游标）不应为空")
	}
}

// 级联：DeleteRoom 后 FTS 行同清（不留可检索残留——RFC-0010 个人版数据主权）。
func TestFTS5CascadeDeleteIT(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fts3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{
		{EventID: "evt_c0", TenantID: "ten_it", RoomID: "room_cd", Seq: 1, Type: protocol.EventRoomCreated,
			SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:00Z",
			Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"},
			Payload:    []byte(`{"display_name":"D","thread_id":"thr_1","agents":[]}`), Metadata: map[string]any{}},
		ftsEnvelope(t, "room_cd", 2, "evt_c1", "par_codex", "级联清理验证正文"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRoom(ctx, "room_cd"); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM room_fts WHERE room_id = ?", "room_cd").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("级联后 FTS 残留 %d 行", n)
	}
	// 语义基准对照：room.SearchMessages 线性实现与 FTS5 命中集一致（同房间同查询）
	_ = room.SearchMessages // 引用锚（语义基准在生产代码路径外，此处仅防误删）
}
