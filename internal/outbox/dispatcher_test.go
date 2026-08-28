// UT 层：outbox 分发器——顺序投递、标记幂等、空轮零成本、取消即停。
package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu       sync.Mutex
	pending  []Entry
	marked   []int64
	failNext bool
}

func (f *fakeStore) Pending(ctx context.Context, limit int) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return nil, errors.New("boom")
	}
	return append([]Entry(nil), f.pending...), nil
}

func (f *fakeStore) MarkDispatched(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, ids...)
	f.pending = nil
	return nil
}

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) Deliver(_ context.Context, e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e.EventID)
}

func TestDispatchOnceDeliversInOrderAndMarks(t *testing.T) {
	store := &fakeStore{pending: []Entry{
		{ID: 1, EventID: "e1", RoomID: "r"},
		{ID: 2, EventID: "e2", RoomID: "r"},
		{ID: 3, EventID: "e3", RoomID: "r"},
	}}
	rec := &recorder{}
	d := NewDispatcher(store, []Consumer{rec}, time.Millisecond)

	if err := d.dispatchOnce(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(rec.events) != 3 || rec.events[0] != "e1" || rec.events[2] != "e3" {
		t.Fatalf("顺序投递被破坏：%v", rec.events)
	}
	if len(store.marked) != 3 {
		t.Fatalf("标记数量 = %d", len(store.marked))
	}
	// 空轮：无投递无标记
	if err := d.dispatchOnce(context.Background()); err != nil {
		t.Fatalf("empty dispatch: %v", err)
	}
	if len(rec.events) != 3 {
		t.Fatalf("空轮不应投递：%v", rec.events)
	}
}

func TestDispatchOnceToleratesStoreError(t *testing.T) {
	store := &fakeStore{pending: []Entry{{ID: 1, EventID: "e1"}}, failNext: true}
	rec := &recorder{}
	d := NewDispatcher(store, []Consumer{rec}, time.Millisecond)
	if err := d.dispatchOnce(context.Background()); err == nil {
		t.Fatal("存储错误应上抛（Run 内容忍重试）")
	}
	if len(rec.events) != 0 {
		t.Fatal("失败轮不得投递")
	}
	// 下一轮恢复正常
	if err := d.dispatchOnce(context.Background()); err != nil {
		t.Fatalf("recover dispatch: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("恢复后应投递：%v", rec.events)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	store := &fakeStore{}
	rec := &recorder{}
	d := NewDispatcher(store, []Consumer{rec}, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 未随 ctx 取消退出")
	}
}
