// Package outbox：进程内提交后分发（ADR-0008 机制映射）。
// outbox 表仍持久化；Dispatcher 轮询未分发条目 → 依序投递消费者 → 标记完成；
// 崩溃后重启动即从表重放（不丢不重：条目按提交序、标记幂等）。
package outbox

import (
	"context"
	"time"
)

// Entry 待分发条目（SQLite outbox 行的端口形态）。
type Entry struct {
	ID        int64
	RoomID    string
	EventID   string
	GlobalPos int64
	Envelope  []byte // 权威信封 JSON
}

// Store 分发器所需的最小存储端口。
type Store interface {
	Pending(ctx context.Context, limit int) ([]Entry, error)
	MarkDispatched(ctx context.Context, ids []int64) error
}

// Consumer 单个条目的消费者（同步调用：消费者自行决定是否异步处理）。
type Consumer interface {
	Deliver(ctx context.Context, entry Entry)
}

// ConsumerFunc 函数式适配器。
type ConsumerFunc func(ctx context.Context, entry Entry)

// Deliver 实现 Consumer。
func (f ConsumerFunc) Deliver(ctx context.Context, entry Entry) { f(ctx, entry) }

// Dispatcher 轮询分发器。
type Dispatcher struct {
	store     Store
	consumers []Consumer
	interval  time.Duration
}

// NewDispatcher 构造；interval ≤0 时取 20ms。
func NewDispatcher(store Store, consumers []Consumer, interval time.Duration) *Dispatcher {
	if interval <= 0 {
		interval = 20 * time.Millisecond
	}
	return &Dispatcher{store: store, consumers: consumers, interval: interval}
}

// Run 阻塞循环；ctx 取消即停（未分发条目留待下次启动重放）。
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.dispatchOnce(ctx); err != nil {
				// 单轮失败不致命：下轮重试（条目未标记，天然重放）
				continue
			}
		}
	}
}

// DispatchOnce 单轮：取一批 → 逐条投递全部消费者 → 统一标记。
// 逐条投递保证同房间顺序；标记在投递后（at-least-once，消费者需幂等或可重放）。
func (d *Dispatcher) dispatchOnce(ctx context.Context) error {
	entries, err := d.store.Pending(ctx, 100)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(entries))
	for _, entry := range entries {
		for _, c := range d.consumers {
			c.Deliver(ctx, entry)
		}
		ids = append(ids, entry.ID)
	}
	return d.store.MarkDispatched(ctx, ids)
}
