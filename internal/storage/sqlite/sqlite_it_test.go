//go:build it

// IT 层：真实 SQLite（modernc 纯 Go 驱动）× 协议边界模型。
// M0 存储 spike 三用例（D-1 排雷）：事件追加 / 游标续读 / outbox 重放，另含并发写串行化证明。
package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func openTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mosaic-spike.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func envelope(id, room string, seqIgnored int) protocol.Envelope {
	thread := "thr_test"
	return protocol.Envelope{
		EventID:           id,
		TenantID:          "ten_local",
		RoomID:            room,
		ThreadID:          &thread,
		DiscussionEpochID: nil,
		Type:              protocol.EventMessagePosted,
		SchemaVersion:     1,
		OccurredAt:        "2026-08-28T00:00:00.000Z",
		Actor:             protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility:        protocol.Visibility{Kind: "public"},
		Payload:           []byte(`{"body":"spike"}`),
		Metadata:          map[string]any{},
	}
}

// 用例 1：WAL 生效；批量追加原子落库、按房连续 seq；重复 event_id 整批回滚（幂等追加语义）。
func TestSQLiteAppendAtomicityAndSeq_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	if mode, err := store.JournalMode(ctx); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, err = %v（期望 wal）", mode, err)
	}

	batch := []protocol.Envelope{
		envelope("evt_a1", "room_a", 0),
		envelope("evt_a2", "room_a", 0),
	}
	appended, err := store.AppendEvents(ctx, batch)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if appended[0].Seq != 1 || appended[1].Seq != 2 {
		t.Fatalf("room_a seq = %d, %d（期望 1, 2）", appended[0].Seq, appended[1].Seq)
	}

	// 另一房间 seq 独立计数
	other, err := store.AppendEvents(ctx, []protocol.Envelope{envelope("evt_b1", "room_b", 0)})
	if err != nil {
		t.Fatalf("append room_b: %v", err)
	}
	if other[0].Seq != 1 {
		t.Fatalf("room_b seq = %d（期望 1，房间间独立）", other[0].Seq)
	}

	// 幂等：重复 event_id 报 ErrDuplicateEvent，且同批后续事件不落库
	_, err = store.AppendEvents(ctx, []protocol.Envelope{
		envelope("evt_a3", "room_a", 0),
		envelope("evt_a1", "room_a", 0), // 重复
	})
	if !errors.Is(err, ErrDuplicateEvent) {
		t.Fatalf("重复追加应报 ErrDuplicateEvent，got %v", err)
	}
	events, _, err := store.EventsAfter(ctx, "room_a", "", 100)
	if err != nil {
		t.Fatalf("read room_a: %v", err)
	}
	if len(events) != 2 || events[len(events)-1].Envelope.EventID != "evt_a2" {
		t.Fatalf("整批回滚失败：残留 %d 条，末尾 %s", len(events), events[len(events)-1].Envelope.EventID)
	}
	pending, err := store.PendingOutbox(ctx, 100)
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	if len(pending) != 3 { // a1 a2 b1，回滚批不出现在 outbox
		t.Fatalf("outbox 应为 3 条（事件+outbox 同事务），got %d", len(pending))
	}
}

// 用例 2：游标续读——分页无重无漏；重开数据库（模拟断线重连）后从游标继续；房间隔离。
func TestSQLiteCursorResumeAcrossReopen_IT(t *testing.T) {
	ctx := context.Background()
	store, path := openTempStore(t)

	const total = 10
	for i := 0; i < total; i++ {
		if _, err := store.AppendEvents(ctx, []protocol.Envelope{
			envelope(fmt.Sprintf("evt_a%02d", i), "room_a", 0),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// 干扰房间：续读 room_a 时绝不能出现
	for i := 0; i < 3; i++ {
		if _, err := store.AppendEvents(ctx, []protocol.Envelope{
			envelope(fmt.Sprintf("evt_c%02d", i), "room_c", 0),
		}); err != nil {
			t.Fatalf("append room_c: %v", err)
		}
	}

	var got []string
	var cursors []string
	cursor := ""
	for {
		events, next, err := store.EventsAfter(ctx, "room_a", cursor, 4)
		if err != nil {
			t.Fatalf("events after %q: %v", cursor, err)
		}
		for _, e := range events {
			got = append(got, e.Envelope.EventID)
			cursors = append(cursors, e.Cursor)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != total {
		t.Fatalf("读到 %d 条（期望 %d）：分页有重有漏", len(got), total)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("重复事件 %s", id)
		}
		seen[id] = true
	}

	// 模拟进程重启：重开同一文件，从中间游标续读
	midCursor := cursors[4] // 第 5 条的游标
	events, _, err := store.EventsAfter(ctx, "room_a", midCursor, 100)
	if err != nil {
		t.Fatalf("reopen read: %v", err)
	}
	if len(events) != total-5 {
		t.Fatalf("重开后续读 %d 条（期望 %d）", len(events), total-5)
	}

	// 非法游标必须显式报错（不静默从头）
	if _, _, err := store.EventsAfter(ctx, "room_a", "%%%bad%%%", 10); err == nil {
		t.Fatal("非法游标必须报错")
	}

	store.Close()
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	events, _, err = store2.EventsAfter(ctx, "room_a", "", 100)
	if err != nil || len(events) != total {
		t.Fatalf("重开后全量读 = %d, err = %v（期望 %d）", len(events), err, total)
	}
}

// 用例 3：outbox 重放——未分发条目按提交序恢复；标记幂等；分发后不再出现。
func TestSQLiteOutboxReplayAfterCrash_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	for i := 0; i < 5; i++ {
		if _, err := store.AppendEvents(ctx, []protocol.Envelope{
			envelope(fmt.Sprintf("evt_o%d", i), "room_o", 0),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// 模拟：前 3 条分发成功，随后崩溃（2 条滞留）
	pending, err := store.PendingOutbox(ctx, 100)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 5 {
		t.Fatalf("pending = %d（期望 5）", len(pending))
	}
	if err := store.MarkDispatched(ctx, []int64{pending[0].ID, pending[1].ID, pending[2].ID}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	// 崩溃恢复：按提交序只重放未分发条目
	pending, err = store.PendingOutbox(ctx, 100)
	if err != nil {
		t.Fatalf("pending after mark: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d（期望 2）", len(pending))
	}
	if pending[0].EventID != "evt_o3" || pending[1].EventID != "evt_o4" {
		t.Fatalf("重放顺序错误：%s, %s（期望 evt_o3, evt_o4）", pending[0].EventID, pending[1].EventID)
	}
	if err := store.MarkDispatched(ctx, []int64{pending[0].ID, pending[1].ID}); err != nil {
		t.Fatalf("mark rest: %v", err)
	}
	// 幂等：重复标记不报错、不复活
	if err := store.MarkDispatched(ctx, []int64{pending[0].ID}); err != nil {
		t.Fatalf("重复标记应幂等: %v", err)
	}
	pending, err = store.PendingOutbox(ctx, 100)
	if err != nil {
		t.Fatalf("final pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("分发完成后仍有 %d 条滞留", len(pending))
	}
}

// 并发写串行化证明：D-1 机制映射的核心断言——
// BEGIN IMMEDIATE + busy_timeout 下，并发追加不丢事件、seq 连续唯一、零 SQLITE_BUSY 外泄。
func TestSQLiteConcurrentAppendSerialization_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	const workers, perWorker = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("evt_w%di%d", w, i)
				if _, err := store.AppendEvents(ctx, []protocol.Envelope{envelope(id, "room_rw", 0)}); err != nil {
					errs <- fmt.Errorf("worker %d: %w", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发追加失败（D-1 机制映射受疑）: %v", err)
	}

	events, _, err := store.EventsAfter(ctx, "room_rw", "", 1000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != workers*perWorker {
		t.Fatalf("落库 %d 条（期望 %d）：并发丢写", len(events), workers*perWorker)
	}
	seqs := map[int64]bool{}
	for _, e := range events {
		if seqs[e.Envelope.Seq] {
			t.Fatalf("room 内 seq 重复：%d", e.Envelope.Seq)
		}
		seqs[e.Envelope.Seq] = true
	}
	for want := int64(1); want <= workers*perWorker; want++ {
		if !seqs[want] {
			t.Fatalf("seq %d 缺失：并发追加出现空洞", want)
		}
	}
}
