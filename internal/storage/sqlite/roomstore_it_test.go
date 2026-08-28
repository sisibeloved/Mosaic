//go:build it

// IT 层：SQLite 实现的 room.AtomicStore 端口——回执原子性、竞态语义、版本/存在查询。
package sqlite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func roomEnvelope(id, roomID, typ string) protocol.Envelope {
	return protocol.Envelope{
		EventID:       id,
		TenantID:      "ten_local",
		RoomID:        roomID,
		Type:          typ,
		SchemaVersion: 1,
		OccurredAt:    "2026-08-28T10:00:00.000Z",
		Actor:         protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       []byte(`{}`),
		Metadata:      map[string]any{},
	}
}

func sampleReceipt(eventID, roomID, idem string, version int64) room.CommandReceipt {
	return room.CommandReceipt{
		TenantID:           "ten_local",
		RoomID:             roomID,
		IdempotencyKey:     idem,
		CommandKind:        "post_message",
		RequestFingerprint: "fp_" + idem,
		EventID:            eventID,
		RoomVersion:        version,
		ExecutedAt:         "2026-08-28T10:00:00.000Z",
	}
}

// 事件与回执同事务：回执冲突时整批回滚（竞态后到者什么都不写）。
func TestAppendWithReceiptAtomic_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	appended, err := store.AppendWithReceipt(ctx,
		[]protocol.Envelope{roomEnvelope("evt_rc1", "room_rc", protocol.EventMessagePosted)},
		sampleReceipt("evt_rc1", "room_rc", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f01", 1))
	if err != nil {
		t.Fatalf("append with receipt: %v", err)
	}
	if appended[0].Seq != 1 {
		t.Fatalf("seq = %d（期望 1）", appended[0].Seq)
	}

	rc, err := store.LookupReceipt(ctx, "ten_local", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f01", "post_message")
	if err != nil || rc == nil {
		t.Fatalf("回执必须可查回：%v %v", rc, err)
	}
	if rc.EventID != "evt_rc1" || rc.RoomVersion != 1 {
		t.Fatalf("回执内容不符：%+v", rc)
	}

	// 同键重放（不同事件）：必须整批回滚
	_, err = store.AppendWithReceipt(ctx,
		[]protocol.Envelope{roomEnvelope("evt_rc2", "room_rc", protocol.EventMessagePosted)},
		sampleReceipt("evt_rc2", "room_rc", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f01", 2))
	if !errors.Is(err, room.ErrDuplicateReceipt) {
		t.Fatalf("同键回执冲突应报 room.ErrDuplicateReceipt，got %v", err)
	}
	events, _, err := store.EventsAfter(ctx, "room_rc", "", 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("竞态后到者不得留事件，got %d", len(events))
	}

	// 未命中返回 nil,nil
	rc, err = store.LookupReceipt(ctx, "ten_local", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f02", "post_message")
	if err != nil || rc != nil {
		t.Fatalf("未命中应返回 (nil,nil)，got %v %v", rc, err)
	}
}

func TestRoomVersionAndExists_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	if v, err := store.RoomVersion(ctx, "room_ve"); err != nil || v != 0 {
		t.Fatalf("空房 version 应为 0，got %d err=%v", v, err)
	}
	if ok, err := store.RoomExists(ctx, "room_ve"); err != nil || ok {
		t.Fatalf("空房 exists 应为 false，got %v err=%v", ok, err)
	}
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{
		roomEnvelope("evt_ve1", "room_ve", protocol.EventRoomCreated),
	}); err != nil {
		t.Fatalf("append room.created: %v", err)
	}
	if ok, err := store.RoomExists(ctx, "room_ve"); err != nil || !ok {
		t.Fatalf("room.created 后 exists 应为 true，got %v err=%v", ok, err)
	}
	if v, err := store.RoomVersion(ctx, "room_ve"); err != nil || v != 1 {
		t.Fatalf("version 应为 1，got %d err=%v", v, err)
	}
	// 只有 message 没有 room.created 的房间不算存在（事件日志无 room.created）
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{
		roomEnvelope("evt_ve2", "room_ghost", protocol.EventMessagePosted),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if ok, _ := store.RoomExists(ctx, "room_ghost"); ok {
		t.Fatal("无 room.created 的房间不存在")
	}
}

// 并发同命令竞态：N 个 goroutine 同幂等键 → 恰好一个成功，其余 ErrDuplicateReceipt。
func TestConcurrentSameCommandRace_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{
		roomEnvelope("evt_cr0", "room_cr", protocol.EventRoomCreated),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 8
	var okCount, dupCount int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.AppendWithReceipt(ctx,
				[]protocol.Envelope{roomEnvelope("evt_cr_dup", "room_cr", protocol.EventMessagePosted)}, // 同 event id：只有一个能进
				sampleReceipt("evt_cr_dup", "room_cr", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5f03", 2))
			switch {
			case err == nil:
				atomic.AddInt64(&okCount, 1)
			case errors.Is(err, room.ErrDuplicateReceipt), errors.Is(err, room.ErrDuplicateEvent):
				atomic.AddInt64(&dupCount, 1)
			default:
				t.Errorf("竞态不应产生其他错误: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if okCount != 1 {
		t.Fatalf("恰好一个成功，got %d（dup=%d）", okCount, dupCount)
	}
	events, _, _ := store.EventsAfter(ctx, "room_cr", "", 100)
	if len(events) != 2 {
		t.Fatalf("房间应只有 2 个事件，got %d", len(events))
	}
}
