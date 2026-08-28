// UT 层：SSE Hub——房间路由、跨房隔离、慢消费者断流重连语义。
// TDD：先于实现（红→绿）。
package sse

import (
	"testing"
	"time"
)

func viewOf(cursor string) ViewEvent {
	return ViewEvent{Cursor: cursor, Type: "message.posted", Data: []byte(`{}`)}
}

func TestHubRoutesByRoom(t *testing.T) {
	h := NewHub()
	subA := h.Subscribe("room_a", 8)
	defer subA.Close()
	subB := h.Subscribe("room_b", 8)
	defer subB.Close()

	h.Publish("room_a", viewOf("c1"))

	select {
	case ev := <-subA.C:
		if ev.Cursor != "c1" {
			t.Fatalf("cursor = %s", ev.Cursor)
		}
	case <-time.After(time.Second):
		t.Fatal("room_a 未收到事件")
	}
	select {
	case ev, ok := <-subB.C:
		if ok {
			t.Fatalf("跨房泄漏：room_b 收到 %s", ev.Cursor)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubCloseUnsubscribes(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("room_x", 8)
	sub.Close()
	h.Publish("room_x", viewOf("c1"))
	select {
	case ev, ok := <-sub.C:
		if ok {
			t.Fatalf("关闭后不应再收到：%s", ev.Cursor)
		}
	case <-time.After(50 * time.Millisecond):
	}
	// 无人房间发布不 panic
	h.Publish("room_ghost", viewOf("c0"))
}

func TestHubSlowConsumerIsDropped(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("room_slow", 2) // 缓冲 2
	for i := 0; i < 10; i++ {
		h.Publish("room_slow", viewOf("c"))
	}
	// 慢消费者必须被断流（收到关闭），靠 cursor 重连追平——不静默丢部分事件
	deadline := time.After(time.Second)
	dropped := false
	for !dropped {
		select {
		case _, ok := <-sub.C:
			if !ok {
				dropped = true
			}
		case <-deadline:
			t.Fatal("慢消费者未被断流：会造成静默丢失")
		}
	}
}

func TestHubManyRooms(t *testing.T) {
	h := NewHub()
	subs := make([]*Subscription, 20)
	for i := range subs {
		subs[i] = h.Subscribe("room", 4)
	}
	h.Publish("room", viewOf("c"))
	for i, sub := range subs {
		select {
		case <-sub.C:
		case <-time.After(time.Second):
			t.Fatalf("订阅者 %d 未收到", i)
		}
		sub.Close()
	}
}
