// Package outbox：进程内提交后分发（ADR-0008 机制映射）。
// outbox 表仍持久化；Dispatcher 轮询未分发条目 → 依序投递消费者 → 标记完成；
// 崩溃后重启动即从表重放（不丢不重：条目按提交序、标记幂等）。
package outbox

import (
	"context"
	"encoding/json"
	"log/slog"
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
// 返回错误 = 本条未消化：分发器不标记该条目（含其后整批），下轮按原序重投——
// 持久消费者（如引擎的 durable claim）由此获得 at-least-once 重试而非静默丢失
// （复审 #15：此前 Deliver 无错误通道，声明失败只能告警后内存直驱，崩溃即丢轮）。
type Consumer interface {
	Deliver(ctx context.Context, entry Entry) error
}

// ConsumerFunc 函数式适配器。
type ConsumerFunc func(ctx context.Context, entry Entry) error

// Deliver 实现 Consumer。
func (f ConsumerFunc) Deliver(ctx context.Context, entry Entry) error { return f(ctx, entry) }

// Dispatcher 轮询分发器。
type Dispatcher struct {
	store     Store
	consumers []Consumer
	interval  time.Duration
	logger    *slog.Logger
}

// NewDispatcher 构造；interval ≤0 时取 20ms。
func NewDispatcher(store Store, consumers []Consumer, interval time.Duration) *Dispatcher {
	if interval <= 0 {
		interval = 20 * time.Millisecond
	}
	return &Dispatcher{store: store, consumers: consumers, interval: interval, logger: slog.Default()}
}

// WithLogger 注入日志器（缺省 slog.Default；重投告警不再静默）。
func (d *Dispatcher) WithLogger(l *slog.Logger) *Dispatcher {
	if l != nil {
		d.logger = l
	}
	return d
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
				d.logger.Warn("outbox: 分发轮失败（下轮重试）", "err", err)
			}
		}
	}
}

// DispatchOnce 单轮：取一批 → 逐条投递全部消费者 → 统一标记。
// 逐条投递保证同房间顺序；标记在投递后（at-least-once，消费者需幂等或可重放）。
// 消费失败即中断本批：失败条目及其后继均不标记——重投时按原提交序补齐，
// 后继条目先标记会破坏同房间顺序（SSE 已收到的帧按 at-least-once 语义容忍重发）。
func (d *Dispatcher) dispatchOnce(ctx context.Context) error {
	entries, err := d.store.Pending(ctx, 100)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(entries))
	// 开发者模式（M1 v1.8）：分发环节是命令→事件→引擎链路的中转——
	// 每条分发落 debug 日志（含事件类型，解析开销仅在 debug 开启时承担）。
	verbose := d.logger.Enabled(ctx, slog.LevelDebug)
	for _, entry := range entries {
		if verbose {
			eventType := ""
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(entry.Envelope, &probe) == nil {
				eventType = probe.Type
			}
			d.logger.Debug("outbox: 分发条目", "event", entry.EventID,
				"room", entry.RoomID, "pos", entry.GlobalPos, "type", eventType)
		}
		failed := false
		for _, c := range d.consumers {
			if err := c.Deliver(ctx, entry); err != nil {
				d.logger.Warn("outbox: 消费失败，条目待重投（本批后继暂缓）", "event", entry.EventID, "err", err)
				failed = true
				break
			}
		}
		if failed {
			break
		}
		ids = append(ids, entry.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	return d.store.MarkDispatched(ctx, ids)
}
