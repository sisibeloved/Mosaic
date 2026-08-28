// Package sse：进程内订阅枢纽——按房间路由、慢消费者断流（靠 opaque cursor 重连追平）。
package sse

import "sync"

// ViewEvent 是投递给订阅者的外部视图事件（Data 为视图 JSON）。
type ViewEvent struct {
	Cursor string
	Type   string
	Data   []byte
}

// Hub 房间 → 订阅者集合。发布按房间路由；不做持久化（续传靠存储游标）。
type Hub struct {
	mu    sync.Mutex
	rooms map[string]map[*Subscription]struct{}
}

// Subscription 单个订阅；C 关闭即被断流（重连后从最后 cursor 追平）。
type Subscription struct {
	C    <-chan ViewEvent
	ch   chan ViewEvent
	hub  *Hub
	room string
	once sync.Once
}

// NewHub 构造。
func NewHub() *Hub {
	return &Hub{rooms: map[string]map[*Subscription]struct{}{}}
}

// Subscribe 订阅房间；buffer 为通道缓冲（慢于缓冲即被断流）。
func (h *Hub) Subscribe(roomID string, buffer int) *Subscription {
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan ViewEvent, buffer)
	sub := &Subscription{C: ch, ch: ch, hub: h, room: roomID}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.rooms[roomID]
	if set == nil {
		set = map[*Subscription]struct{}{}
		h.rooms[roomID] = set
	}
	set[sub] = struct{}{}
	return sub
}

// Close 退订（幂等）。
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		if set := s.hub.rooms[s.room]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(s.hub.rooms, s.room)
			}
		}
		close(s.ch)
	})
}

// Publish 向房间全部订阅者投递；满缓冲者被断流（不静默丢事件——逼客户端重连追平）。
func (h *Hub) Publish(roomID string, ev ViewEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.rooms[roomID]
	if len(set) == 0 {
		return
	}
	for sub := range set {
		select {
		case sub.ch <- ev:
		default:
			delete(set, sub)
			close(sub.ch)
		}
	}
	if len(set) == 0 {
		delete(h.rooms, roomID)
	}
}
