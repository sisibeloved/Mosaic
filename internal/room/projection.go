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

// PolicyView 快照的策略区（记分卡透明 OQ-17：权重、模式参数对成员可见、版本化）。
type PolicyView struct {
	PolicyVersion  string                 `json:"policy_version"`
	Mode           string                 `json:"mode"`
	MaxSpeakers    int                    `json:"max_speakers"`
	Lambda         float64                `json:"lambda"`
	Weights        protocol.PolicyWeights `json:"weights"`
	IntentWindow   string                 `json:"intent_window"`
	ResponseCap    int64                  `json:"response_cap"`
	RevealStrategy string                 `json:"reveal_strategy"`
}

// Snapshot 快照载体：版本三元组 + 水位（opaque cursor）+ Timeline + 策略区。
type Snapshot struct {
	RoomID            string          `json:"room_id"`
	RoomVersion       int64           `json:"room_version"`
	Watermark         string          `json:"watermark"`
	ProjectionVersion int             `json:"projection_version"`
	AlgorithmVersion  int             `json:"algorithm_version"`
	Timeline          []TimelineItem  `json:"timeline"`
	Policy            PolicyView      `json:"policy"`
	Scorecard         []ScorecardItem `json:"scorecard"`
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

// ProjectSnapshot 从房间事件重建快照（仅 message 族入 Timeline；控制事件不入列表；
// 策略区经 RebuildPolicy 投影——快照与引擎同源，无双轨；记分卡自 intent.recorded
// 全量投影 + intent.endorsed 合并——事件不回写）。
func ProjectSnapshot(roomID string, events []StoredEvent) Snapshot {
	snap := Snapshot{
		RoomID:            roomID,
		ProjectionVersion: ProjectionVersion,
		AlgorithmVersion:  AlgorithmVersion,
		Timeline:          []TimelineItem{},
		Scorecard:         []ScorecardItem{},
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
	policy := RebuildPolicy(envs)
	snap.Policy = PolicyView{
		PolicyVersion:  policy.PolicyVersion,
		Mode:           policy.Params.Mode,
		MaxSpeakers:    policy.Params.MaxSpeakers,
		Lambda:         policy.Params.Lambda,
		Weights:        policy.Params.Weights,
		IntentWindow:   policy.Params.IntentWindow,
		ResponseCap:    policy.Params.ResponseCap,
		RevealStrategy: policy.Params.RevealStrategy,
	}
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
