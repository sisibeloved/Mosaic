// Package protocol 的外部视图模型（RFC-0001 v0.4 P0 决议）：
// 对外订阅/快照/历史不含 seq 与 tenant_id，以 opaque position 替代；
// 水位按主体视图重写由 transport 层在上游完成（M1 最小版直接透传 position）。
package protocol

import "encoding/json"

// EventView 是事件的对外表象（HistoryItem 的流式形态）。
type EventView struct {
	EventID       string          `json:"event_id"`
	RoomID        string          `json:"room_id"`
	ThreadID      *string         `json:"thread_id"`
	Type          string          `json:"type"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    string          `json:"occurred_at"`
	Actor         Actor           `json:"actor"`
	CausationID   *string         `json:"causation_id"`
	CorrelationID *string         `json:"correlation_id"`
	Visibility    Visibility      `json:"visibility"`
	Payload       json.RawMessage `json:"payload"`
	Position      string          `json:"position"` // opaque cursor：续传位点
}

// ToEventView 权威信封 → 外部视图（剥离 seq/tenant_id/metadata，附 position）。
func ToEventView(env Envelope, cursor string) EventView {
	return EventView{
		EventID:       env.EventID,
		RoomID:        env.RoomID,
		ThreadID:      env.ThreadID,
		Type:          env.Type,
		SchemaVersion: env.SchemaVersion,
		OccurredAt:    env.OccurredAt,
		Actor:         env.Actor,
		CausationID:   env.CausationID,
		CorrelationID: env.CorrelationID,
		Visibility:    env.Visibility,
		Payload:       env.Payload,
		Position:      cursor,
	}
}
