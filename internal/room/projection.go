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

// TimelineItem Timeline 视图项（对外形态：无 seq/tenant；RFC-0012：消息族 +
// 暂停/恢复系统提醒——round.* 已内部化不入列表）。
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

// Snapshot 快照载体：版本三元组 + 水位（opaque cursor）+ Timeline + 策略区。
// DisplayName 为投影产物（room.created/room.renamed）；Participants 由装配层注入
// （ADR-0011 注记：非投影产物，不进 room_version/水位语义，投影恒为空切片）。
type Snapshot struct {
	RoomID            string            `json:"room_id"`
	RoomVersion       int64             `json:"room_version"`
	Watermark         string            `json:"watermark"`
	ProjectionVersion int               `json:"projection_version"`
	AlgorithmVersion  int               `json:"algorithm_version"`
	DisplayName       string            `json:"display_name"`
	Timeline          []TimelineItem    `json:"timeline"`
	Scorecard         []ScorecardItem   `json:"scorecard"`
	Threads           []ThreadView      `json:"threads"`
	Roster            []string          `json:"roster"`
	Graph             []GraphEdge       `json:"graph"`
	Participants      []ParticipantView `json:"participants"`
}

// ParticipantView 快照参与者视图项（装配层注入：本地 owner + 引擎座位）。
// Adapter/Channel 仅 agent 座位携带（channel 为 harness 渠道标签，空则省略）；
// SeatStatus 本切片恒为 seated（离座语义随 UI 重设计后续切片定稿）。
type ParticipantView struct {
	ParticipantID string `json:"participant_id"`
	Kind          string `json:"kind"` // human | agent | system
	DisplayName   string `json:"display_name"`
	Adapter       string `json:"adapter,omitempty"`
	Channel       string `json:"channel,omitempty"`
	SeatStatus    string `json:"seat_status"`
}

// ScorecardItem 记分卡视图项（R-08/OQ-17：band + 未选理由 + 保送状态对成员可查）。
type ScorecardItem struct {
	IntentID         string `json:"intent_id"`
	ParticipantID    string `json:"participant_id"`
	Type             string `json:"type"`
	Action           string `json:"action"`
	ScoreBand        string `json:"score_band"`
	Selected         bool   `json:"selected"`
	Endorsed         bool   `json:"endorsed"`
	PublicRationale  string `json:"public_rationale"`
	UnselectedReason string `json:"unselected_reason,omitempty"`
	RoundID          string `json:"round_id,omitempty"`
	OccurredAt       string `json:"occurred_at"`
}

// ProjectSnapshot 从房间事件重建快照（RFC-0012：Timeline = message 族 +
// 暂停/恢复系统提醒——round.* 内部化不入列表；策略区已退役；记分卡自
// intent.recorded 全量投影 + intent.endorsed 合并——事件不回写）。
func ProjectSnapshot(roomID string, events []StoredEvent) Snapshot {
	snap := Snapshot{
		RoomID:            roomID,
		ProjectionVersion: ProjectionVersion,
		AlgorithmVersion:  AlgorithmVersion,
		Timeline:          []TimelineItem{},
		Scorecard:         []ScorecardItem{},
		Participants:      []ParticipantView{}, // 装配层注入位：投影恒空（ADR-0011 注记）
	}
	endorsedSet := map[string]bool{} // intent.endorsed 合并键
	for _, ev := range events {
		if ev.Envelope.Type != protocol.EventIntentEndorsed {
			continue
		}
		var p struct {
			IntentID string `json:"intent_id"`
		}
		if json.Unmarshal(ev.Envelope.Payload, &p) == nil && p.IntentID != "" {
			endorsedSet[p.IntentID] = true
		}
	}
	envs := make([]protocol.Envelope, len(events))
	for i := range events {
		envs[i] = events[i].Envelope
	}
	threads, graph := RebuildThreads(envs)
	if roster := RosterOf(envs); roster != nil {
		snap.Roster = make([]string, 0, len(roster))
		for id := range roster {
			snap.Roster = append(snap.Roster, id)
		}
	}
	snap.Threads = make([]ThreadView, 0, len(threads))
	for _, th := range threads {
		snap.Threads = append(snap.Threads, *th)
	}
	snap.Graph = graph
	for _, ev := range events {
		if ev.Envelope.Seq > snap.RoomVersion {
			snap.RoomVersion = ev.Envelope.Seq
		}
		snap.Watermark = ev.Cursor
		if ev.Envelope.Type == protocol.EventIntentRecorded {
			var p protocol.IntentRecordedPayload
			if json.Unmarshal(ev.Envelope.Payload, &p) == nil {
				p.Endorsed = endorsedSet[p.IntentID]
				item := ScorecardItem{
					IntentID:         p.IntentID,
					ParticipantID:    p.ParticipantID,
					Type:             p.Type,
					Action:           p.Action,
					ScoreBand:        p.ScoreBand,
					Selected:         p.Selected,
					Endorsed:         p.Endorsed,
					PublicRationale:  p.PublicRationale,
					UnselectedReason: p.UnselectedReason,
					OccurredAt:       ev.Envelope.OccurredAt,
				}
				if ev.Envelope.CorrelationID != nil {
					item.RoundID = *ev.Envelope.CorrelationID
				}
				snap.Scorecard = append(snap.Scorecard, item)
			}
			continue
		}
		// 房间名投影：room.created 命名、room.renamed 覆盖（序内后者胜）。
		if ev.Envelope.Type == protocol.EventRoomCreated || ev.Envelope.Type == protocol.EventRoomRenamed {
			var p struct {
				DisplayName string `json:"display_name"`
			}
			if json.Unmarshal(ev.Envelope.Payload, &p) == nil {
				snap.DisplayName = p.DisplayName
			}
			continue
		}
		if ev.Envelope.Type != protocol.EventMessagePosted {
			// 系统提醒入 Timeline（暂停/恢复——SSE 瞬态不是唯一来源；
			// RFC-0012：round.* 内部化，不入用户可见时间线）
			switch ev.Envelope.Type {
			case protocol.EventRoomPaused, protocol.EventRoomStarted:
				item := TimelineItem{
					Position:   ev.Cursor,
					EventID:    ev.Envelope.EventID,
					Type:       ev.Envelope.Type,
					ActorID:    ev.Envelope.Actor.ParticipantID,
					ActorKind:  ev.Envelope.Actor.Kind,
					ThreadID:   ev.Envelope.ThreadID,
					OccurredAt: ev.Envelope.OccurredAt,
				}
				snap.Timeline = append(snap.Timeline, item)
			}
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
