// Package protocol 的边界模型（ADR-0007：事件侧手写版本化 struct）。
// 只做 wire 形态映射，不承载领域行为；不得被领域层反向依赖（架构 §8.4.1）。
// round-trip 约定：可选可空字段在 fixture 中必须显式出现（null 或值），
// 可选集合为空时写 []——保证 struct ↔ JSON ↔ Schema 三方逐键一致可断言。
package protocol

import "encoding/json"

// 事件类型名（Attention 事件族 RFC-0003 §3.1.11；房间生命周期 RFC-0001）。
const (
	EventRoundOpened         = "round.opened"
	EventIntentRecorded      = "intent.recorded"
	EventIntentEndorsed      = "intent.endorsed"
	EventFloorGranted        = "floor.granted"
	EventFloorRevoked        = "floor.revoked"
	EventRoundClosed         = "round.closed"
	EventMessagePosted       = "message.posted"
	EventPolicyChanged       = "policy.changed"
	EventParticipantAdmitted = "participant.admitted"
	EventThreadForked        = "thread.forked"
	EventThreadPaused        = "thread.paused"
	EventThreadResumed       = "thread.resumed"
	EventThreadClosed        = "thread.closed"
	EventThreadReopened      = "thread.reopened"
	EventThreadMerged        = "thread.merged"
	EventRoomCreated         = "room.created"
	EventRoomPaused          = "room.paused"
	EventRoomStarted         = "room.started"
	EventRoomRenamed         = "room.renamed"
	// 收束协议（RFC-0005，M3-2 落地为群聊制裁剪口径——见 RFC 附录 B）
	EventClosureProposed         = "closure.proposed"
	EventClosureEvaluated        = "closure.evaluated"
	EventClosureRejected         = "closure.rejected"
	EventClosureAccepted         = "closure.accepted"
	EventPauseCapsuleCreated     = "pause_capsule.created"
	EventEvidenceRequestCreated  = "evidence_request.created"
	EventEvidenceRequestResolved = "evidence_request.resolved"
	EventRoomDeleted             = "room.deleted"
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

// RoundOpenedPayload round.opened（RFC-0012 §2.2：反应波内部记账——快照
// Timeline 不收录；策略面已退役，stimulus_event_id 即波锚点=最新一条消息）。
type RoundOpenedPayload struct {
	RoundID         string `json:"round_id"`
	StimulusEventID string `json:"stimulus_event_id"`
}

// IntentRecordedPayload intent.recorded：TurnIntent 的用户可见投影（公开 band，不公开精确分）。
// R-01 全记录：失格/越界/silent 意图也落事件（ScoreBand="unranked"，Type 可空，理由入 payload——记分卡可查 R-08）。
type IntentRecordedPayload struct {
	IntentID         string   `json:"intent_id"`
	ParticipantID    string   `json:"participant_id"`
	Action           string   `json:"action"` // speak | react | fork | summarize | silent
	Type             string   `json:"type"`   // answer | extend | challenge | support | question | redirect | synthesize | ""（silent 可省）
	ReplyTo          *string  `json:"reply_to"`
	AddressedTo      []string `json:"addressed_to,omitempty"`
	PublicRationale  string   `json:"public_rationale"`
	ScoreBand        string   `json:"score_band"` // very_low | low | medium | high | very_high | unranked（未进入记分）
	Selected         bool     `json:"selected"`
	Endorsed         bool     `json:"endorsed"`
	UnselectedReason string   `json:"unselected_reason,omitempty"` // 记分卡透明（R-08）：未选理由（budget/duplicate/…）
}

// PolicyParams/PolicyWeights 已随 RFC-0012 退役（群聊制无房间策略面）；
// 存量 policy.changed 事件投影端忽略（回放容错，事件溯源不动存量日志）。

// ThreadLifecyclePayload thread 生命周期事件族（RFC-0004；forked 携带谱系）。
type ThreadLifecyclePayload struct {
	ThreadID       string   `json:"thread_id"`
	ParentThreadID string   `json:"parent_thread_id,omitempty"` // forked
	SourceEventID  string   `json:"source_event_id,omitempty"`  // forked
	Goal           string   `json:"goal,omitempty"`             // forked（必填于命令面）
	Participants   []string `json:"participants,omitempty"`
	MergedInto     string   `json:"merged_into,omitempty"` // merged
	Reason         string   `json:"reason,omitempty"`
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
	ContextWatermark int    `json:"context_watermark"`
	Epoch            int    `json:"epoch"`
	ExpiresAt        string `json:"expires_at"`
	Directed         bool   `json:"directed"`
}

// FloorRevokedPayload floor.revoked（AR-004：撤销生效 < 500ms，正确性由 epoch 保证）。
type FloorRevokedPayload struct {
	GrantID string `json:"grant_id"`
	Reason  string `json:"reason"` // human_preemption | room_paused | budget | thread_closed | generation_failed
}

// RoundClosedPayload round.closed：零公开发言是合法结果（AR-002）。
type RoundClosedPayload struct {
	RoundID       string `json:"round_id"`
	Outcome       string `json:"outcome"` // published | quiescent | revoked_all（quiescent=意愿静默终止，RFC-0012）
	SelectedCount int    `json:"selected_count"`
	SilentCount   int    `json:"silent_count"`
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

// ClosureProposedPayload closure.proposed：人类显式提议收束（裁剪口径：唯一触发源；
// Policy 收敛信号随 RFC-0006/M3-4，budget_tail 不触发收束——预算只产暂停胶囊）。
type ClosureProposedPayload struct {
	ClosureID   string `json:"closure_id"`
	ThreadID    string `json:"thread_id"`
	Trigger     string `json:"trigger"`                // human（裁剪口径下恒为 human）
	ClosureHint string `json:"closure_hint,omitempty"` // 提议时的类型倾向（缺省=接受时按异议面自动判定）
	Watermark   int64  `json:"watermark"`
}

// ClosureEvaluatedPayload closure.evaluated：一个座的三态表态（conclude/object/abstain）。
// qualified 仅对 object 有意义：确定性合格性判定（新证据/新假设 + 预期影响；
// claim 快照随 RFC-0006，裁剪期 claim_id 可选）。不合格 object 记具名异议不阻塞。
type ClosureEvaluatedPayload struct {
	ClosureID      string   `json:"closure_id"`
	ParticipantID  string   `json:"participant_id"`
	Action         string   `json:"action"` // conclude | object | abstain
	ClaimID        string   `json:"claim_id,omitempty"`
	NewEvidence    []string `json:"new_evidence,omitempty"`
	NewAssumptions []string `json:"new_assumptions,omitempty"`
	ExpectedImpact string   `json:"expected_impact,omitempty"`
	Rationale      string   `json:"rationale"`
	Qualified      bool     `json:"qualified,omitempty"`
	ParkedReason   string   `json:"parked_reason,omitempty"` // 不合格 object 的停放理由
}

// ClosureRejectedPayload closure.rejected：合格异议中止收束（线程回 active，不关不暂停）。
type ClosureRejectedPayload struct {
	ClosureID          string `json:"closure_id"`
	QualifiedObjection string `json:"qualified_objection"` // 合格 object 的 closure.evaluated 事件 id
	Reason             string `json:"reason"`              // new_evidence | new_assumptions
	PhaseTo            string `json:"phase_to"`            // active
}

// ClosureCapsule 收束胶囊（接受时确定性组装自收束轮三态与结构化异议；不可变）。
type ClosureCapsule struct {
	ClosureID      string               `json:"closure_id"`
	ClosureType    string               `json:"closure_type"` // consensus | bounded_disagreement
	ThreadID       string               `json:"thread_id"`
	Watermark      int64                `json:"watermark"`
	Conclusions    []string             `json:"conclusions"`
	NamedDissent   []CapsuleDissent     `json:"named_dissent"`
	Assumptions    []string             `json:"assumptions"`
	Evidence       CapsuleEvidence      `json:"evidence"`
	OpenQuestions  []string             `json:"open_questions"`
	Falsifiers     []string             `json:"falsifiers"`
	ReopenTriggers []string             `json:"reopen_triggers"`
	Participation  CapsuleParticipation `json:"participation"`
}

type CapsuleDissent struct {
	ParticipantID string `json:"participant_id"`
	Basis         string `json:"basis"`
}

type CapsuleEvidence struct {
	Support []string `json:"support"`
	Oppose  []string `json:"oppose"`
}

type CapsuleParticipation struct {
	Concluded   []string `json:"concluded"`
	Objected    []string `json:"objected"`
	Abstained   []string `json:"abstained"`
	Timeout     []string `json:"timeout"`
	Unavailable []string `json:"unavailable"`
}

// ClosureAcceptedPayload closure.accepted：人类接受收束（个人版默认接受权）。
type ClosureAcceptedPayload struct {
	ClosureID   string         `json:"closure_id"`
	ClosureType string         `json:"closure_type"`
	ThreadID    string         `json:"thread_id"`
	Capsule     ClosureCapsule `json:"capsule"`
	AcceptedBy  string         `json:"accepted_by"`
}

// PauseCapsuleCreatedPayload pause_capsule.created：预算熔断的未收敛快照
// （不写 closure.accepted、不关线程——只标记"因预算停"）。
type PauseCapsuleCreatedPayload struct {
	PauseID       string   `json:"pause_id"`
	PauseReason   string   `json:"pause_reason"` // budget
	ThreadID      string   `json:"thread_id,omitempty"`
	Watermark     int64    `json:"watermark"`
	OpenQuestions []string `json:"open_questions"`
}

// Evidence Request（RFC-0005 §3.1.9，M3-5 落地）：争议依赖外部事实的证据需求单。
// 生命周期 open → resolved / dismissed；满足后系统不自动重开——时间线提示人类
// reopen_thread（新证据留痕于重开首条消息）。
type EvidenceRequestCreatedPayload struct {
	RequestID          string   `json:"request_id"`
	ClaimID            string   `json:"claim_id,omitempty"`
	Question           string   `json:"question"`
	RequiredEvidence   []string `json:"required_evidence"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Owners             []string `json:"owners"`
	ReopenOnResolution bool     `json:"reopen_thread_on_resolution"`
}

type EvidenceRequestResolvedPayload struct {
	RequestID    string   `json:"request_id"`
	EvidenceRefs []string `json:"evidence_refs"`
	Resolution   string   `json:"resolution"` // resolved | dismissed
	Note         string   `json:"note,omitempty"`
}
