// 房间读投影：Timeline 与快照四元组（RFC-0001——投影可由事件全量重建，
// 快照携带 projection_version/algorithm_version/event_watermark + opaque cursor）。
// 纯函数：同事件流必同快照（回放一致性 CI 门禁的基础）。
package room

import (
	"encoding/json"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// 投影与算法版本：投影结构或算法变更时递增（旧快照可识别过期）。
const (
	ProjectionVersion = 1
	AlgorithmVersion  = 1
)

// TimelineItem Timeline 视图项（对外形态：无 seq/tenant）。
type TimelineItem struct {
	Position   string  `json:"position"`
	EventID    string  `json:"event_id"`
	Type       string  `json:"type"`
	ActorID    string  `json:"actor_id"`
	ActorKind  string  `json:"actor_kind"`
	Body       string  `json:"body,omitempty"`
	ThreadID   *string `json:"thread_id,omitempty"`
	OccurredAt string  `json:"occurred_at"`
}

// Snapshot 快照四元组载体：版本三元组 + 水位（opaque cursor）。
type Snapshot struct {
	RoomID            string         `json:"room_id"`
	RoomVersion       int64          `json:"room_version"`
	Watermark         string         `json:"watermark"`
	ProjectionVersion int            `json:"projection_version"`
	AlgorithmVersion  int            `json:"algorithm_version"`
	Timeline          []TimelineItem `json:"timeline"`
}

// ProjectSnapshot 从房间事件重建快照（仅 message 族入 Timeline；控制事件不入列表）。
func ProjectSnapshot(roomID string, events []StoredEvent) Snapshot {
	snap := Snapshot{
		RoomID:            roomID,
		ProjectionVersion: ProjectionVersion,
		AlgorithmVersion:  AlgorithmVersion,
		Timeline:          []TimelineItem{},
	}
	for _, ev := range events {
		if ev.Envelope.Seq > snap.RoomVersion {
			snap.RoomVersion = ev.Envelope.Seq
		}
		snap.Watermark = ev.Cursor
		if ev.Envelope.Type != protocol.EventMessagePosted {
			continue
		}
		var body struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(ev.Envelope.Payload, &body)
		snap.Timeline = append(snap.Timeline, TimelineItem{
			Position:   ev.Cursor,
			EventID:    ev.Envelope.EventID,
			Type:       ev.Envelope.Type,
			ActorID:    ev.Envelope.Actor.ParticipantID,
			ActorKind:  ev.Envelope.Actor.Kind,
			Body:       body.Body,
			ThreadID:   ev.Envelope.ThreadID,
			OccurredAt: ev.Envelope.OccurredAt,
		})
	}
	return snap
}
