// Package protocol 的边界模型（ADR-0007：事件侧手写版本化 struct）。
// 只做 wire 形态映射，不承载领域行为；不得被领域层反向依赖（架构 §8.4.1）。
// round-trip 约定：可选可空字段在 fixture 中必须显式出现（null 或值），
// 可选集合为空时写 []——保证 struct ↔ JSON ↔ Schema 三方逐键一致可断言。
package protocol

import "encoding/json"

// 事件类型名（Attention 事件族 RFC-0003 §3.1.11；房间生命周期 RFC-0001）。
const (
	EventRoundOpened    = "round.opened"
	EventIntentRecorded = "intent.recorded"
	EventIntentEndorsed = "intent.endorsed"
	EventFloorGranted   = "floor.granted"
	EventFloorRevoked   = "floor.revoked"
	EventRoundClosed    = "round.closed"
	EventMessagePosted  = "message.posted"
	EventRoomCreated    = "room.created"
	EventRoomPaused     = "room.paused"
	EventRoomStarted    = "room.started"
)

// Envelope 是 room_events 的权威/内部形态（RFC-0001 v0.4）。
// 对外视图（HistoryItem/订阅）不含 seq，以 opaque position 替代。
type Envelope struct {
	EventID           string          `json:"event_id"`
	TenantID          string          `json:"tenant_id"`
	RoomID            string          `json:"room_id"`
	ThreadID          *string         `json:"thread_id"`
	DiscussionEpochID *string         `json:"discussion_epoch_id"`
	Seq               int64           `json:"seq"`
	Type              string          `json:"type"`
	SchemaVersion     int             `json:"schema_version"`
	OccurredAt        string          `json:"occurred_at"`
	Actor             Actor           `json:"actor"`
	CausationID       *string         `json:"causation_id"`
	CorrelationID     *string         `json:"correlation_id"`
	Visibility        Visibility      `json:"visibility"`
	Payload           json.RawMessage `json:"payload"`
	Metadata          map[string]any  `json:"metadata"`
}

// Actor 事件的行动者；kind 区分 human/agent/system（message.posted 统一命名依赖此字段）。
type Actor struct {
	ParticipantID string `json:"participant_id"`
	Kind          string `json:"kind"` // human | agent | system
}

// Visibility 可见性视图（RFC-0001：seq 内部专用，外部按视图重写水位）。
type Visibility struct {
	Kind         string   `json:"kind"` // public | participants | moderators | system
	Participants []string `json:"participants,omitempty"`
}

// RoundOpenedPayload round.opened（RFC-0003 §3.1.11）。
type RoundOpenedPayload struct {
	RoundID         string `json:"round_id"`
	StimulusEventID string `json:"stimulus_event_id"`
	Mode            string `json:"mode"`            // roundtable | open_floor | deep_dive | review | decision
	RevealStrategy  string `json:"reveal_strategy"` // sequential | simultaneous | independent_then_cross
	IntentWindow    string `json:"intent_window"`
	PolicyVersion   string `json:"policy_version"`
}

// IntentRecordedPayload intent.recorded：TurnIntent 的用户可见投影（公开 band，不公开精确分）。
type IntentRecordedPayload struct {
	IntentID        string   `json:"intent_id"`
	ParticipantID   string   `json:"participant_id"`
	Action          string   `json:"action"` // speak | react | fork | summarize | silent
	Type            string   `json:"type"`   // answer | extend | challenge | support | question | redirect | synthesize
	ReplyTo         *string  `json:"reply_to"`
	AddressedTo     []string `json:"addressed_to,omitempty"`
	PublicRationale string   `json:"public_rationale"`
	ScoreBand       string   `json:"score_band"` // very_low | low | medium | high | very_high
	Selected        bool     `json:"selected"`
	Endorsed        bool     `json:"endorsed"`
}

// IntentEndorsedPayload intent.endorsed：人类保送（OQ-17；Agent 不能保送 Agent）。
type IntentEndorsedPayload struct {
	IntentID   string `json:"intent_id"`
	EndorsedBy string `json:"endorsed_by"`
	Effect     string `json:"effect"` // grant | boost
}

// FloorGrantedPayload floor.granted：epoch 供迟到拒绝；消费以任务结果提交为准。
type FloorGrantedPayload struct {
	GrantID          string `json:"grant_id"`
	RoundID          string `json:"round_id"`
	ParticipantID    string `json:"participant_id"`
	Rank             int    `json:"rank"`
	RevealStrategy   string `json:"reveal_strategy"`
	ContextWatermark int    `json:"context_watermark"`
	Epoch            int    `json:"epoch"`
	ExpiresAt        string `json:"expires_at"`
	ResponseCap      int    `json:"response_cap"`
	Directed         bool   `json:"directed"`
}

// FloorRevokedPayload floor.revoked（AR-004：撤销生效 < 500ms，正确性由 epoch 保证）。
type FloorRevokedPayload struct {
	GrantID string `json:"grant_id"`
	Reason  string `json:"reason"` // human_preemption | room_paused | budget | thread_closed
}

// RoundClosedPayload round.closed：零公开发言是合法结果（AR-002）。
type RoundClosedPayload struct {
	RoundID        string `json:"round_id"`
	Outcome        string `json:"outcome"` // published | quiescent | budget_stopped | revoked_all
	SelectedCount  int    `json:"selected_count"`
	SilentCount    int    `json:"silent_count"`
	CrossSubrounds int    `json:"cross_subrounds"`
}

// DecodePayload 按事件类型把 Envelope.Payload 解码为对应边界结构。
// 未纳入事件族的类型（如 message.posted）返回 nil——payload Schema 尚未定稿，属 M1。
func (e *Envelope) DecodePayload() any {
	switch e.Type {
	case EventRoundOpened:
		var p RoundOpenedPayload
		_ = json.Unmarshal(e.Payload, &p)
		return p
	case EventIntentRecorded:
		var p IntentRecordedPayload
		_ = json.Unmarshal(e.Payload, &p)
		return p
	case EventIntentEndorsed:
		var p IntentEndorsedPayload
		_ = json.Unmarshal(e.Payload, &p)
		return p
	case EventFloorGranted:
		var p FloorGrantedPayload
		_ = json.Unmarshal(e.Payload, &p)
		return p
	case EventFloorRevoked:
		var p FloorRevokedPayload
		_ = json.Unmarshal(e.Payload, &p)
		return p
	case EventRoundClosed:
		var p RoundClosedPayload
		_ = json.Unmarshal(e.Payload, &p)
		return p
	default:
		return nil
	}
}
