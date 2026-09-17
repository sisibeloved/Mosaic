//go:build it

// IT 层：SQLite 实现的 doc.DocStore/DocCASStore/DocEventReader/DocSearcher 端口
// （RFC-0014 / ADR-0014 Phase 0）——追加与续读、base_version CAS、回执幂等、
// FTS 命中与回退、删除级联（墓碑留痕）。
package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func docEnvelope(id, docID, typ, payload string) protocol.DocEnvelope {
	return protocol.DocEnvelope{
		EventID:       id,
		TenantID:      "ten_local",
		DocID:         docID,
		Type:          typ,
		SchemaVersion: 1,
		OccurredAt:    "2026-09-16T10:00:00.000Z",
		Actor:         protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload:       []byte(payload),
		Metadata:      map[string]any{},
	}
}

func docReceipt(eventID, docID, idem string, expected int64) doc.CommandReceipt {
	return doc.CommandReceipt{
		TenantID:           "ten_local",
		DocID:              docID,
		IdempotencyKey:     idem,
		CommandKind:        "commit_revision",
		RequestFingerprint: "fp_" + idem,
		EventID:            eventID,
		ExpectedDocVersion: expected,
		ExecutedAt:         "2026-09-16T10:00:00.000Z",
	}
}

// 追加 + 续读：version per-doc 单调分配、文档间独立、游标分页无重无漏。
func TestDocAppendAndReadBack_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	appended, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_d1", "doc_aaaaaaaaaaaa", protocol.EventDocCreated,
			`{"doc_id":"doc_aaaaaaaaaaaa","title":"发布清单","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
		docEnvelope("evt_d2", "doc_aaaaaaaaaaaa", protocol.EventDocRevisionCommitted,
			`{"doc_id":"doc_aaaaaaaaaaaa","base_version":1,"version":2,"ops":[{"op":"append","block_id":null,"block":{"block_id":"blk_01","type":"paragraph","text":"初稿"}}],"actor":"par_owner","source":"human_editor"}`),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if appended[0].Version != 1 || appended[1].Version != 2 {
		t.Fatalf("version = %d, %d（期望 1, 2）", appended[0].Version, appended[1].Version)
	}

	// DocExists：已见 doc.created 为真，未见/其他事件为假
	if exists, err := store.DocExists(ctx, "doc_aaaaaaaaaaaa"); err != nil || !exists {
		t.Fatalf("DocExists = %v, %v（期望 true）", exists, err)
	}
	if exists, err := store.DocExists(ctx, "doc_999999999999"); err != nil || exists {
		t.Fatalf("DocExists（未见） = %v, %v（期望 false）", exists, err)
	}

	// 另一文档 version 独立计数
	other, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_e1", "doc_bbbbbbbbbbbb", protocol.EventDocCreated,
			`{"doc_id":"doc_bbbbbbbbbbbb","title":"另一篇","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
	})
	if err != nil {
		t.Fatalf("append doc_b: %v", err)
	}
	if other[0].Version != 1 {
		t.Fatalf("doc_b version = %d（期望 1，文档间独立）", other[0].Version)
	}

	if v, err := store.DocVersion(ctx, "doc_aaaaaaaaaaaa"); err != nil || v != 2 {
		t.Fatalf("DocVersion = %d, %v（期望 2）", v, err)
	}

	// 游标分页（limit=1 强制翻页）：无重无漏、按 version 升序
	var got []string
	cursor := ""
	for {
		events, next, err := store.DocEventsAfter(ctx, "doc_aaaaaaaaaaaa", cursor, 1)
		if err != nil {
			t.Fatalf("doc events after %q: %v", cursor, err)
		}
		for _, e := range events {
			got = append(got, e.Envelope.EventID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != 2 || got[0] != "evt_d1" || got[1] != "evt_d2" {
		t.Fatalf("续读结果 = %v（期望 [evt_d1 evt_d2]）", got)
	}

	// 文档隔离：doc_b 读不到 doc_a 的事件
	events, _, err := store.DocEventsAfter(ctx, "doc_bbbbbbbbbbbb", "", 100)
	if err != nil || len(events) != 1 || events[0].Envelope.EventID != "evt_e1" {
		t.Fatalf("doc_b 续读 = %+v, %v", events, err)
	}

	// 重复 event_id 整批回滚（幂等追加语义）
	_, err = store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_d3", "doc_aaaaaaaaaaaa", protocol.EventDocRenamed, `{"doc_id":"doc_aaaaaaaaaaaa","title":"x"}`),
		docEnvelope("evt_d1", "doc_aaaaaaaaaaaa", protocol.EventDocRenamed, `{"doc_id":"doc_aaaaaaaaaaaa","title":"y"}`),
	})
	if !errors.Is(err, doc.ErrDuplicateEvent) {
		t.Fatalf("重复追加应报 doc.ErrDuplicateEvent，got %v", err)
	}
	if v, _ := store.DocVersion(ctx, "doc_aaaaaaaaaaaa"); v != 2 {
		t.Fatalf("整批回滚失败：version = %d（期望 2）", v)
	}
}

// base_version CAS：期望版本不符即 doc.ErrVersionConflict，整批回滚；相符才追加。
func TestDocAppendCASConflict_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	if _, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_c1", "doc_cccccccccccc", protocol.EventDocCreated,
			`{"doc_id":"doc_cccccccccccc","title":"并发","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := store.AppendDocEventsIf(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_c2", "doc_cccccccccccc", protocol.EventDocRevisionCommitted,
			`{"doc_id":"doc_cccccccccccc","base_version":0,"version":2,"ops":[{"op":"delete","block_id":"blk_01"}],"actor":"par_owner","source":"human_editor"}`),
	}, 0)
	if !errors.Is(err, doc.ErrVersionConflict) {
		t.Fatalf("base_version 不符应报 doc.ErrVersionConflict，got %v", err)
	}
	if v, _ := store.DocVersion(ctx, "doc_cccccccccccc"); v != 1 {
		t.Fatalf("冲突批不得落库：version = %d（期望 1）", v)
	}

	appended, err := store.AppendDocEventsIf(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_c2", "doc_cccccccccccc", protocol.EventDocRevisionCommitted,
			`{"doc_id":"doc_cccccccccccc","base_version":1,"version":2,"ops":[{"op":"delete","block_id":"blk_01"}],"actor":"par_owner","source":"human_editor"}`),
	}, 1)
	if err != nil {
		t.Fatalf("CAS 相符追加: %v", err)
	}
	if appended[0].Version != 2 {
		t.Fatalf("version = %d（期望 2）", appended[0].Version)
	}
}

// 回执幂等：事件 + 回执同事务；同键重放报 doc.ErrDuplicateReceipt 且整批回滚；
// 回执可查回（DocVersion 权威回填）；回执内 ExpectedDocVersion 在事务内强制。
func TestDocAppendWithReceiptReplay_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	if _, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_r0", "doc_dddddddddddd", protocol.EventDocCreated,
			`{"doc_id":"doc_dddddddddddd","title":"回执","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	appended, err := store.AppendDocEventsWithReceipt(ctx,
		[]protocol.DocEnvelope{
			docEnvelope("evt_r1", "doc_dddddddddddd", protocol.EventDocRevisionCommitted,
				`{"doc_id":"doc_dddddddddddd","base_version":1,"version":2,"ops":[{"op":"append","block_id":null,"block":{"block_id":"blk_01","type":"paragraph","text":"落笔"}}],"actor":"par_owner","source":"human_editor"}`),
		},
		docReceipt("evt_r1", "doc_dddddddddddd", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f11", 1))
	if err != nil {
		t.Fatalf("append with receipt: %v", err)
	}
	if appended[0].Version != 2 {
		t.Fatalf("version = %d（期望 2）", appended[0].Version)
	}

	rc, err := store.LookupDocReceipt(ctx, "ten_local", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f11", "commit_revision")
	if err != nil || rc == nil {
		t.Fatalf("回执必须可查回：%v %v", rc, err)
	}
	if rc.EventID != "evt_r1" || rc.DocVersion != 2 {
		t.Fatalf("回执内容不符（DocVersion 应权威回填为 2）：%+v", rc)
	}

	// 同键重放（不同事件）：整批回滚
	_, err = store.AppendDocEventsWithReceipt(ctx,
		[]protocol.DocEnvelope{
			docEnvelope("evt_r2", "doc_dddddddddddd", protocol.EventDocRevisionCommitted,
				`{"doc_id":"doc_dddddddddddd","base_version":2,"version":3,"ops":[{"op":"delete","block_id":"blk_01"}],"actor":"par_owner","source":"human_editor"}`),
		},
		docReceipt("evt_r2", "doc_dddddddddddd", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f11", 2))
	if !errors.Is(err, doc.ErrDuplicateReceipt) {
		t.Fatalf("同键回执冲突应报 doc.ErrDuplicateReceipt，got %v", err)
	}
	if v, _ := store.DocVersion(ctx, "doc_dddddddddddd"); v != 2 {
		t.Fatalf("竞态后到者不得留事件：version = %d（期望 2）", v)
	}

	// 回执内期望版本不符：事务内强制乐观并发
	_, err = store.AppendDocEventsWithReceipt(ctx,
		[]protocol.DocEnvelope{
			docEnvelope("evt_r3", "doc_dddddddddddd", protocol.EventDocRevisionCommitted,
				`{"doc_id":"doc_dddddddddddd","base_version":1,"version":3,"ops":[{"op":"delete","block_id":"blk_01"}],"actor":"par_owner","source":"human_editor"}`),
		},
		docReceipt("evt_r3", "doc_dddddddddddd", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f12", 1))
	if !errors.Is(err, doc.ErrVersionConflict) {
		t.Fatalf("回执期望版本不符应报 doc.ErrVersionConflict，got %v", err)
	}

	// 未命中返回 (nil, nil)
	rc, err = store.LookupDocReceipt(ctx, "ten_local", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f99", "commit_revision")
	if err != nil || rc != nil {
		t.Fatalf("未命中应返回 (nil,nil)，got %v %v", rc, err)
	}
}

// FTS：doc.created 标题与 doc.revision_committed ops 文本入索引——
// CJK ≥3 字 trigram 命中、<3 字 LIKE 回退、每文档归并最新命中版本。
func TestDocFTSSearch_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	seed := [][]protocol.DocEnvelope{{
		docEnvelope("evt_s1", "doc_eeeeeeeeeeee", protocol.EventDocCreated,
			`{"doc_id":"doc_eeeeeeeeeeee","title":"迁移预算评估","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
		docEnvelope("evt_s2", "doc_eeeeeeeeeeee", protocol.EventDocRevisionCommitted,
			`{"doc_id":"doc_eeeeeeeeeeee","base_version":1,"version":2,"ops":[{"op":"append","block_id":null,"block":{"block_id":"blk_01","type":"paragraph","text":"预算超限需要先排查迁移脚本"}},{"op":"replace","block_id":"blk_01","text":"budget exceeds limits"}],"actor":"par_kimi","source":"agent"}`),
	}, {
		docEnvelope("evt_s3", "doc_ffffffffffff", protocol.EventDocCreated,
			`{"doc_id":"doc_ffffffffffff","title":"周会纪要","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
	}}
	for _, batch := range seed {
		if _, err := store.AppendDocEvents(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}

	// 标题 CJK ≥3 字命中
	hits, err := store.SearchDocs(ctx, "预算评估", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocID != "doc_eeeeeeeeeeee" || hits[0].Title != "迁移预算评估" {
		t.Fatalf("标题 trigram 命中 = %+v", hits)
	}
	// 正文 CJK 命中（归并后取最新版本行：version=2，actor 为信封行动者）
	hits, _ = store.SearchDocs(ctx, "迁移脚本", 20)
	if len(hits) != 1 || hits[0].Version != 2 || hits[0].Actor != "par_owner" || hits[0].EventID != "evt_s2" {
		t.Fatalf("正文命中应归并最新版本 = %+v", hits)
	}
	// 英文词（大小写不敏感）
	hits, _ = store.SearchDocs(ctx, "Budget", 20)
	if len(hits) != 1 || hits[0].DocID != "doc_eeeeeeeeeeee" {
		t.Fatalf("英文命中 = %+v", hits)
	}
	// <3 字 → LIKE 回退（标题命中另一篇）
	hits, _ = store.SearchDocs(ctx, "周会", 20)
	if len(hits) != 1 || hits[0].DocID != "doc_ffffffffffff" {
		t.Fatalf("短查询 LIKE 回退 = %+v", hits)
	}
	// 空查询 / 无命中
	if hits, _ = store.SearchDocs(ctx, "  ", 20); len(hits) != 0 {
		t.Fatalf("空查询应为空 = %+v", hits)
	}
	if hits, _ = store.SearchDocs(ctx, "不存在的词", 20); len(hits) != 0 {
		t.Fatalf("无命中应为空 = %+v", hits)
	}

	// 自愈：制造索引缺口后搜索触发全量重建
	if _, err := store.db.ExecContext(ctx, "DELETE FROM doc_fts WHERE doc_id = ?", "doc_eeeeeeeeeeee"); err != nil {
		t.Fatal(err)
	}
	hits, err = store.SearchDocs(ctx, "迁移脚本", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocID != "doc_eeeeeeeeeeee" {
		t.Fatalf("自愈重建后命中 = %+v", hits)
	}
}

// 删除级联：doc.deleted 留痕后 DeleteDoc 清事件/outbox/回执/FTS，墓碑行保留（含 reason）。
func TestDocDeleteCascade_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	if _, err := store.AppendDocEventsWithReceipt(ctx,
		[]protocol.DocEnvelope{
			docEnvelope("evt_x1", "doc_121212121212", protocol.EventDocCreated,
				`{"doc_id":"doc_121212121212","title":"待删文档","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
		},
		docReceipt("evt_x1", "doc_121212121212", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f21", 0)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_x2", "doc_121212121212", protocol.EventDocDeleted,
			`{"doc_id":"doc_121212121212","reason":"重复创建"}`),
	}); err != nil {
		t.Fatalf("tombstone event: %v", err)
	}

	if err := store.DeleteDoc(ctx, "doc_121212121212"); err != nil {
		t.Fatalf("delete doc: %v", err)
	}

	events, _, err := store.DocEventsAfter(ctx, "doc_121212121212", "", 100)
	if err != nil || len(events) != 0 {
		t.Fatalf("事件应级联清除：%d, %v", len(events), err)
	}
	for _, table := range []string{"doc_events", "doc_outbox", "doc_command_receipts", "doc_fts"} {
		var n int64
		if err := store.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE doc_id = ?", "doc_121212121212").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s 应级联清除，残留 %d 行", table, n)
		}
	}
	// 墓碑留痕（含 doc.deleted 载荷 reason）
	var reason, deletedAt string
	if err := store.db.QueryRowContext(ctx,
		"SELECT reason, deleted_at FROM doc_tombstones WHERE doc_id = ?", "doc_121212121212",
	).Scan(&reason, &deletedAt); err != nil {
		t.Fatalf("墓碑行必须保留: %v", err)
	}
	if reason != "重复创建" || deletedAt == "" {
		t.Fatalf("墓碑内容不符：reason=%q deleted_at=%q", reason, deletedAt)
	}
	// 全文检索不再命中已删文档
	if hits, _ := store.SearchDocs(ctx, "待删文档", 20); len(hits) != 0 {
		t.Fatalf("已删文档不得命中 = %+v", hits)
	}
}

// 文档列表（DocLister）：聚合摘要行（标题取最新 renamed、状态取最新生命周期、
// 更新者/时间取最新版本行）+ created_by 过滤 + 删除级联后消失。
func TestDocListDocs_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	seed := func(eventID, docID, typ, payload string) {
		t.Helper()
		if _, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
			docEnvelope(eventID, docID, typ, payload),
		}); err != nil {
			t.Fatalf("seed %s: %v", eventID, err)
		}
	}
	// 文档 A：owner 创建 → revision → rename → archive（agent 事件者混入校验 actor 口径）
	seed("evt_la1", "doc_aaaaaaaaaaaa", protocol.EventDocCreated,
		`{"doc_id":"doc_aaaaaaaaaaaa","title":"初名","format":"markdown","created_by":"par_owner","anchor_message_id":null}`)
	seed("evt_la2", "doc_aaaaaaaaaaaa", protocol.EventDocRevisionCommitted,
		`{"doc_id":"doc_aaaaaaaaaaaa","base_version":1,"version":2,"ops":[{"op":"append","block_id":null,"block":{"block_id":"blk_01","type":"paragraph","text":"正文"}}],"actor":"par_codex_x","source":"agent"}`)
	seed("evt_la3", "doc_aaaaaaaaaaaa", protocol.EventDocRenamed,
		`{"doc_id":"doc_aaaaaaaaaaaa","title":"改后名"}`)
	seed("evt_la4", "doc_aaaaaaaaaaaa", protocol.EventDocArchived,
		`{"doc_id":"doc_aaaaaaaaaaaa"}`)
	// 文档 B：agent 创建（created_by 过滤的对照组）
	seed("evt_lb1", "doc_bbbbbbbbbbbb", protocol.EventDocCreated,
		`{"doc_id":"doc_bbbbbbbbbbbb","title":"agent 稿","format":"markdown","created_by":"par_codex_x","anchor_message_id":null}`)
	// 文档 C：删除级联后不得出现
	seed("evt_lc1", "doc_cccccccccccc", protocol.EventDocCreated,
		`{"doc_id":"doc_cccccccccccc","title":"待删","format":"markdown","created_by":"par_owner","anchor_message_id":null}`)
	seed("evt_lc2", "doc_cccccccccccc", protocol.EventDocDeleted,
		`{"doc_id":"doc_cccccccccccc","reason":"清理"}`)
	if err := store.DeleteDoc(ctx, "doc_cccccccccccc"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	all, err := store.ListDocs(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("全量 = %d（期望 2，已删文档不出现）：%+v", len(all), all)
	}
	byID := map[string]doc.DocSummary{}
	for _, s := range all {
		byID[s.DocID] = s
	}
	a := byID["doc_aaaaaaaaaaaa"]
	if a.Title != "改后名" || a.Version != 4 || a.Status != "archived" ||
		a.CreatedBy != "par_owner" || a.CreatedAt == "" || a.UpdatedAt == "" {
		t.Fatalf("文档 A 摘要聚合不符：%+v", a)
	}
	if a.UpdatedBy != "par_owner" { // 最新事件（archive）的 actor
		t.Fatalf("updated_by = %q（期望最新事件 actor par_owner）", a.UpdatedBy)
	}
	b := byID["doc_bbbbbbbbbbbb"]
	if b.Status != "active" || b.CreatedBy != "par_codex_x" || b.Version != 1 {
		t.Fatalf("文档 B 摘要不符：%+v", b)
	}

	mine, err := store.ListDocs(ctx, "par_owner")
	if err != nil || len(mine) != 1 || mine[0].DocID != "doc_aaaaaaaaaaaa" {
		t.Fatalf("created_by=par_owner = %+v, %v（期望仅文档 A）", mine, err)
	}
	none, err := store.ListDocs(ctx, "par_nobody")
	if err != nil || len(none) != 0 {
		t.Fatalf("created_by=par_nobody = %+v, %v（期望空）", none, err)
	}

	// 恢复后状态回 active（最新生命周期事件口径）
	seed("evt_la5", "doc_aaaaaaaaaaaa", protocol.EventDocRestored,
		`{"doc_id":"doc_aaaaaaaaaaaa"}`)
	after, err := store.ListDocs(ctx, "")
	if err != nil {
		t.Fatalf("list after restore: %v", err)
	}
	for _, s := range after {
		if s.DocID == "doc_aaaaaaaaaaaa" && (s.Status != "active" || s.Version != 5) {
			t.Fatalf("恢复后摘要不符：%+v", s)
		}
	}
}

// doc outbox 分发端口：Pending 按提交序、MarkDispatched 幂等标记、
// DocOutboxStore 适配器满足 outbox.Store 端口（Entry.RoomID=doc_id、GlobalPos=version）。
func TestDocOutboxDispatch_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	appended, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_ob1", "doc_aaaaaaaaaaaa", protocol.EventDocCreated,
			`{"doc_id":"doc_aaaaaaaaaaaa","title":"分发","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
		docEnvelope("evt_ob2", "doc_aaaaaaaaaaaa", protocol.EventDocRevisionCommitted,
			`{"doc_id":"doc_aaaaaaaaaaaa","base_version":1,"version":2,"ops":[{"op":"append","block_id":null,"block":{"block_id":"blk_01","type":"paragraph","text":"x"}}],"actor":"par_owner","source":"human_editor"}`),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	other, err := store.AppendDocEvents(ctx, []protocol.DocEnvelope{
		docEnvelope("evt_ob3", "doc_bbbbbbbbbbbb", protocol.EventDocCreated,
			`{"doc_id":"doc_bbbbbbbbbbbb","title":"另一篇","format":"markdown","created_by":"par_owner","anchor_message_id":null}`),
	})
	if err != nil {
		t.Fatalf("seed doc_b: %v", err)
	}
	appended = append(appended, other...)

	adapter := DocOutboxStore{Store: store}
	pending, err := adapter.Pending(ctx, 100)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d（期望 3）", len(pending))
	}
	for i, e := range pending {
		if e.RoomID != appended[i].DocID || e.EventID != appended[i].EventID || e.GlobalPos != appended[i].Version {
			t.Fatalf("条目 %d 字段映射不符：%+v（期望 doc_id/version 承载）", i, e)
		}
		var env protocol.DocEnvelope
		if err := json.Unmarshal(e.Envelope, &env); err != nil || env.EventID != appended[i].EventID {
			t.Fatalf("条目 %d 信封不符：%v", i, err)
		}
	}

	// 标记前两条 → 仅剩第三条；标记幂等（重复标记不报错）
	if err := adapter.MarkDispatched(ctx, []int64{pending[0].ID, pending[1].ID}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := adapter.MarkDispatched(ctx, []int64{pending[0].ID}); err != nil {
		t.Fatalf("重复标记应幂等：%v", err)
	}
	rest, err := adapter.Pending(ctx, 100)
	if err != nil {
		t.Fatalf("pending after mark: %v", err)
	}
	if len(rest) != 1 || rest[0].EventID != "evt_ob3" {
		t.Fatalf("标记后 pending = %+v（期望仅 evt_ob3）", rest)
	}

	// 删除级联清 doc_outbox（已分发/未分发同清）
	if err := store.DeleteDoc(ctx, "doc_bbbbbbbbbbbb"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rest, err = adapter.Pending(ctx, 100)
	if err != nil || len(rest) != 0 {
		t.Fatalf("级联后 pending = %+v, %v（期望空）", rest, err)
	}
}
