// Package missionreturn：执行结果回房（0.6.10 / ADR-0033）。
// Mission 四类事实（reviewing / accepted / returned / canceled）从 mission_events
// 派生持久通知行（来源 seq 唯一 = 去重键），由 Worker 周期投影 + 投递回来源房间：
// 投递 = 同一 SQLite 事务内"墓碑/暂停核对 + 追加 message.posted + 标记 delivered"；
// 房间暂停则 held 保留、恢复后发布；房间删除则 canceled + 来源墓碑；其余错误有界重试。
package missionreturn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// Kind 通知源事实类型（表 1，ADR-0033 §1）。
type Kind string

const (
	KindReviewing Kind = "reviewing" // 已提请验收
	KindAccepted  Kind = "accepted"  // 验收通过（终态）
	KindReturned  Kind = "returned"  // 验收退回
	KindCanceled  Kind = "canceled"  // 终局取消
)

// Status 通知行状态。
type Status string

const (
	StatusPending   Status = "pending"   // 待投
	StatusHeld      Status = "held"      // 房间暂停——保留待投递，恢复后发布
	StatusDelivered Status = "delivered" // 已落房间事件（room_event_id）
	StatusCanceled  Status = "canceled"  // 房间已删除——返回目标取消，结果保留
	StatusFailed    Status = "failed"    // 有界重试耗尽（last_error 留痕）
)

// 投递核对结论（Deliver 返回；Worker 据此分派状态迁移）。
var (
	// ErrRoomDeleted 房间已删除（墓碑或不存在）——投递侧兜底核对。
	ErrRoomDeleted = errors.New("missionreturn: 返回目标房间已删除")
	// ErrRoomPaused 房间暂停——不落卡，保留待恢复。
	ErrRoomPaused = errors.New("missionreturn: 房间暂停，通知保留")
	// ErrNoticeNotPending 通知行已非待投态（并发/重放保护）。
	ErrNoticeNotPending = errors.New("missionreturn: 通知已非待投状态")
)

// Notice 通知行（持久投影——每条来源事件恰一行）。
type Notice struct {
	NoticeID        string
	MissionID       string
	RoomID          string
	TaskID          string
	Kind            Kind
	SourceSeq       int64
	ContractVersion int
	ReviewRound     int
	PackageID       string
	Reviewer        string
	Reason          string
	Objective       string // ≤80 字摘要快照（投影时定格）
	Status          Status
	Attempts        int
	NextAttemptAt   string // RFC3339；空 = 立即可试
	LastError       string
	RoomEventID     string // delivered 时的房间事件 ID
	CreatedAt       string
	UpdatedAt       string
}

// EnvelopeInput 投递一条 message.posted 所需的信封材料（存储侧组信封并原子落卡+标记）。
type EnvelopeInput struct {
	EventID    string
	OccurredAt string
	Body       string
	Ref        protocol.MissionRef
}

// Store 最小存储端口（SQLite 实现；UT 用包内假件）。
type Store interface {
	// Project 一轮投影：mission_events 游标 → 通知行（单事务，source_seq UNIQUE 幂等）。
	// 返回本轮新建行数。
	Project(ctx context.Context) (int, error)
	// Due 到期待投通知（status ∈ pending|held 且 next_attempt_at 到期），按 room 分组、
	// 组内 source_seq 升序（房内保序）；now 为 RFC3339。
	Due(ctx context.Context, now string, limit int) ([]Notice, error)
	// Deliver 原子投递：墓碑/存在性核对 → ErrRoomDeleted；暂停核对 → ErrRoomPaused；
	// 行非待投态 → ErrNoticeNotPending；否则同事务追加 message.posted（outbox/FTS 同落）
	// 并标记 delivered + room_event_id。
	Deliver(ctx context.Context, n Notice, env EnvelopeInput) error
	// UpdateStatus 非终态行更新（held / 退避重试 / failed / 投递侧兜底 canceled）。
	UpdateStatus(ctx context.Context, noticeID string, st Status, attempts int, nextAttemptAt, lastError string) error
}

// Worker 投影 + 投递循环（1s 周期；与 server 同生命周期）。
type Worker struct {
	store       Store
	clock       func() time.Time
	newID       func(prefix string) string
	tenant      string
	interval    time.Duration
	maxAttempts int
	heldDelay   time.Duration // 暂停复查间隔
	baseBackoff time.Duration
	logger      *slog.Logger
}

// Option 装配选项。
type Option func(*Worker)

// WithClock 注入时钟（测试）。
func WithClock(c func() time.Time) Option { return func(w *Worker) { w.clock = c } }

// WithNewID 注入 ID 生成（evt_/rn_ 前缀）。
func WithNewID(f func(prefix string) string) Option { return func(w *Worker) { w.newID = f } }

// WithTenant 注入租户（信封 tenant_id）。
func WithTenant(t string) Option { return func(w *Worker) { w.tenant = t } }

// WithInterval 轮询间隔（≤0 取 1s）。
func WithInterval(d time.Duration) Option { return func(w *Worker) { w.interval = d } }

// WithMaxAttempts 投递尝试上限（≤0 取 12）。
func WithMaxAttempts(n int) Option { return func(w *Worker) { w.maxAttempts = n } }

// WithLogger 注入日志器（缺省 slog.Default）。
func WithLogger(l *slog.Logger) Option {
	return func(w *Worker) {
		if l != nil {
			w.logger = l
		}
	}
}

// New 构造 Worker。
func New(store Store, opts ...Option) *Worker {
	w := &Worker{
		store:       store,
		clock:       time.Now,
		newID:       func(string) string { return "" },
		interval:    time.Second,
		maxAttempts: 12,
		heldDelay:   5 * time.Second,
		baseBackoff: time.Second,
		logger:      slog.Default(),
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Run 阻塞循环；ctx 取消即停（未投出的行留待下次启动重放）。
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.logger.Warn("missionreturn: 单轮失败（下轮重试）", "err", err)
			}
		}
	}
}

// Tick 单轮：投影 → 到期投递（按房分组、房内 source_seq 保序）。
func (w *Worker) Tick(ctx context.Context) error {
	if _, err := w.store.Project(ctx); err != nil {
		return fmt.Errorf("投影: %w", err)
	}
	now := w.clock()
	due, err := w.store.Due(ctx, now.Format(time.RFC3339), 200)
	if err != nil {
		return fmt.Errorf("取到期通知: %w", err)
	}
	skipRoom := "" // 房头未决（held / 退避）则该房本轮余下不越序
	for _, n := range due {
		if n.RoomID == skipRoom {
			continue
		}
		err := w.store.Deliver(ctx, n, w.envelopeOf(n, now))
		switch {
		case err == nil:
			continue
		case errors.Is(err, ErrRoomPaused):
			w.update(ctx, n.NoticeID, StatusHeld, n.Attempts, now.Add(w.heldDelay), "room paused")
			skipRoom = n.RoomID
		case errors.Is(err, ErrRoomDeleted):
			// 返回目标失效：逐条取消（后继同房同样命中），结果与来源墓碑保留
			w.update(ctx, n.NoticeID, StatusCanceled, n.Attempts, time.Time{}, "room deleted")
		case errors.Is(err, ErrNoticeNotPending):
			continue // 并发/重放已处理
		default:
			attempts := n.Attempts + 1
			if attempts >= w.maxAttempts {
				w.update(ctx, n.NoticeID, StatusFailed, attempts, time.Time{}, err.Error())
			} else {
				delay := w.baseBackoff << uint(attempts)
				if delay > time.Minute {
					delay = time.Minute
				}
				w.update(ctx, n.NoticeID, StatusPending, attempts, now.Add(delay), err.Error())
			}
			skipRoom = n.RoomID // 保序：房头退避期间不越序投后继
		}
	}
	return nil
}

func (w *Worker) update(ctx context.Context, id string, st Status, attempts int, next time.Time, lastErr string) {
	var nextS string
	if !next.IsZero() {
		nextS = next.Format(time.RFC3339)
	}
	if err := w.store.UpdateStatus(ctx, id, st, attempts, nextS, lastErr); err != nil {
		w.logger.Warn("missionreturn: 状态更新失败", "notice", id, "status", st, "err", err)
	}
}

// envelopeOf 组装一条 message.posted 的信封材料（存储侧补 tenant/actor 并原子落卡）。
func (w *Worker) envelopeOf(n Notice, now time.Time) EnvelopeInput {
	return EnvelopeInput{
		EventID:    w.newID("evt"),
		OccurredAt: now.UTC().Format(time.RFC3339Nano),
		Body:       bodyOf(n),
		Ref: protocol.MissionRef{
			MissionID:       n.MissionID,
			Kind:            string(n.Kind),
			ContractVersion: n.ContractVersion,
			ReviewRound:     n.ReviewRound,
			PackageID:       n.PackageID,
			TaskID:          n.TaskID,
			SourceSeq:       n.SourceSeq,
		},
	}
}

// bodyOf 卡片正文（事实一句话 + objective 快照；emoji 仅作类型徽标）。
func bodyOf(n Notice) string {
	var round string
	if n.ReviewRound > 0 {
		round = fmt.Sprintf("（第 %d 轮 · 契约 v%d）", n.ReviewRound, n.ContractVersion)
	} else {
		round = fmt.Sprintf("（契约 v%d）", n.ContractVersion)
	}
	switch n.Kind {
	case KindReviewing:
		return "📋 任务「" + n.Objective + "」已提请验收" + round
	case KindAccepted:
		s := "✅ 任务「" + n.Objective + "」验收通过" + round
		if n.Reviewer != "" {
			s += "——验收人：" + truncate(n.Reviewer, 64)
		}
		return s
	case KindReturned:
		s := "↩️ 任务「" + n.Objective + "」验收退回" + round
		if n.Reason != "" {
			s += "——原因：" + truncate(n.Reason, 120)
		}
		return s
	case KindCanceled:
		return "⛔ 任务「" + n.Objective + "」已取消" + round
	}
	return "任务「" + n.Objective + "」状态更新" + round
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimFunc(s, func(c rune) bool {
		return unicode.IsSpace(c) || c == '\ufeff'
	}))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
