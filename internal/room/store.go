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

// EventReader 读端口（transport 层消费；SQLite 实现于 internal/storage/sqlite）。
type EventReader interface {
	// EventsAfter 从 cursor 之后按全局位续读；next 为空串表示已追平。
	EventsAfter(ctx context.Context, roomID, cursor string, limit int) (events []StoredEvent, next string, err error)
}
