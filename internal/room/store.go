// Package room 是 Room 命令处理域：命令校验、幂等 receipt、乐观并发（RFC-0001 v0.4）。
// 存储以端口注入（架构 §8.4 依赖方向）：UT 用内存 fake，IT/ST 用 SQLite 实现。
package room

import (
	"context"
	"errors"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// 域错误：调用方以 errors.Is 判别，不依赖错误文本。
var (
	// ErrInvalidCommand 命令校验失败（payload 越界、未知命令、非法幂等键等）。
	ErrInvalidCommand = errors.New("room: invalid command")
	// ErrVersionConflict expected_room_version 与当前版本不符（乐观并发拒绝）。
	ErrVersionConflict = errors.New("room: version conflict")
	// ErrIdempotencyConflict 同幂等键但请求指纹不同（禁止静默改写语义）。
	ErrIdempotencyConflict = errors.New("room: idempotency conflict")
	// ErrRoomNotFound 目标房间不存在（未见 room.created）。
	ErrRoomNotFound = errors.New("room: room not found")
	// ErrDuplicateEvent 事件 ID 冲突（存储层哨兵，透传给上游判定）。
	ErrDuplicateEvent = errors.New("room: duplicate event id")
	// ErrDuplicateReceipt 幂等键冲突（存储层哨兵：并发竞态时后到者收到）。
	ErrDuplicateReceipt = errors.New("room: duplicate receipt")
)

// CommandReceipt 幂等回执（RFC-0001：持久 receipt，tenant+key+kind 唯一）。
type CommandReceipt struct {
	TenantID           string
	RoomID             string
	IdempotencyKey     string
	CommandKind        string
	RequestFingerprint string
	EventID            string
	// ExpectedRoomVersion 乐观并发期望：存储必须在追加事务内校验
	// 当前房间版本与之相等（P-03：冲突判定在提交事务内，封死 check-then-append 竞态）。
	ExpectedRoomVersion int64
	// RoomVersion 由存储在追加成功后权威回填（调用方传入值被忽略），重放/查询据此返回真实版本。
	RoomVersion int64
	ExecutedAt  string
}

// AtomicStore 是命令处理域的存储端口：事件追加与回执写入必须同事务原子完成。
type AtomicStore interface {
	// AppendEvents 追加事件（seq 由存储按房间分配），并同事务写 outbox。
	AppendEvents(ctx context.Context, envelopes []protocol.Envelope) ([]protocol.Envelope, error)
	// AppendWithReceipt 事件 + 幂等回执同事务落库。事务内依次：回执键已存在 →
	// ErrDuplicateReceipt（竞态后到者按回放处理）；ExpectedRoomVersion 与当前版本
	// 不符 → ErrVersionConflict；成功则回执 RoomVersion 权威回填为追加后的版本。
	AppendWithReceipt(ctx context.Context, envelopes []protocol.Envelope, receipt CommandReceipt) ([]protocol.Envelope, error)
	// LookupReceipt 查幂等回执；不存在返回 (nil, nil)。
	LookupReceipt(ctx context.Context, tenantID, idempotencyKey, commandKind string) (*CommandReceipt, error)
	// RoomVersion 房间当前版本（最新 seq；空房为 0）。
	RoomVersion(ctx context.Context, roomID string) (int64, error)
	// RoomExists 是否存在 room.created。
	RoomExists(ctx context.Context, roomID string) (bool, error)
}

// StoredEvent 读路径 DTO：权威信封 + opaque cursor（订阅续传与历史查询共用）。
type StoredEvent struct {
	Envelope protocol.Envelope
	Cursor   string
}

// RoomSummary 房间列表项（GET /v1/rooms 读路径 DTO；存储层聚合产出）。
// LastEventAt 取该房间最新事件时间（无事件则等于 CreatedAt——房间由 room.created 开启，
// 故任何在列房间必有 CreatedAt）；MessageCount 计 message.posted 类事件数。
type RoomSummary struct {
	RoomID       string `json:"room_id"`
	DisplayName  string `json:"display_name"`
	CreatedAt    string `json:"created_at"`
	LastEventAt  string `json:"last_event_at"`
	Paused       bool   `json:"paused"`
	MessageCount int64  `json:"message_count"`
}

// RoomLister 房间列表读端口（MemStore/SQLite 双实现；UI 重设计切片 1）。
type RoomLister interface {
	// ListRooms 全量房间摘要（仅含已见 room.created 的房间），按 last_event_at 倒序
	// （同刻 room_id 升序兜底——排序必须确定性，UI 列表不可抖动）。
	ListRooms(ctx context.Context) ([]RoomSummary, error)
}

// EventReader 读端口（transport 层消费；SQLite 实现于 internal/storage/sqlite）。
type EventReader interface {
	// EventsAfter 从 cursor 之后按全局位续读；next 为空串表示已追平。
	EventsAfter(ctx context.Context, roomID, cursor string, limit int) (events []StoredEvent, next string, err error)
}

// StimulusClaim 轮次交接的持久声明（二轮审校 #9：dispatcher 在 durable handoff 前
// 确认 outbox，崩溃即永久丢轮——声明行必须先于 Deliver 返回而落盘）。
type StimulusClaim struct {
	RoomID          string
	StimulusEventID string
	Envelope        []byte // 人类消息信封 JSON（恢复时重驱动一轮）
	// Position 刺激的持久全局位（outbox global_pos；四轮复审 #14）：
	// 恢复/重驱动必须按刺激的持久顺序进行——无序重放会反转同房间人类消息顺序。
	Position int64
}

// ClaimStore 轮次交接声明端口（MemStore/SQLite 双实现）。
type ClaimStore interface {
	// ClaimStimulus 声明刺激（INSERT OR IGNORE 语义）：true = 首次声明（本方负责开轮）。
	// position 为刺激的持久全局位（重驱动排序依据）。
	ClaimStimulus(ctx context.Context, roomID, stimulusEventID string, envelope []byte, position int64) (bool, error)
	// DeleteClaim round.opened 已落库后删除声明（声明只覆盖"认领但未开轮"窗口）。
	DeleteClaim(ctx context.Context, roomID, stimulusEventID string) error
	// PendingClaims 未清除的声明，按 Position 升序（持久顺序即重驱动顺序）。
	PendingClaims(ctx context.Context) ([]StimulusClaim, error)
}

// CASStore 乐观并发追加（可选能力；四轮复审 #12：迟到检查与正文落库之间的窗口——
// 检查之后、落库之前插入的事件使 CAS 失败，调用方回读判别真迟到与良性交错）。
type CASStore interface {
	// AppendEventsIf 当前房间版本 == expectedRoomVersion 才追加（同事务判定，
	// 不符返回 ErrVersionConflict，整批回滚）。
	AppendEventsIf(ctx context.Context, envelopes []protocol.Envelope, expectedRoomVersion int64) ([]protocol.Envelope, error)
}
