// 房间引擎（M1 轮循环，切片 C 起接入正式 Attention 选择）：
// 人类消息 → round.opened → 各 seat 串行意图评估 → attention.Select（硬资格+记分卡+MMR）
// → 全量 intent.recorded（含未获选 band，R-08 可查）→ 按 rank 签发 floor.granted（epoch）
// → generate → message.posted(agent, causation=grant) → round.closed。
// epoch = 本房间第 N 轮（round.opened 计数）：同轮各 grant 共享 epoch，迟到拒绝据此判定。
// 崩溃语义（RFC-0003 3.4）：轮状态由事件重建，未提交的选择重算——
// 引擎轮异步执行，dispatch 标记先于轮完成，崩溃丢失的轮由重放重建后重新触发（M2 补触发登记）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// AgentSeat 房间内一个 agent 参与者（Profile 决定适配器，ParticipantID 为房间内身份）。
type AgentSeat struct {
	ParticipantID string
	Profile       agent.Profile
}

// EngineConfig 引擎依赖。
type EngineConfig struct {
	Store  AtomicStore
	Reader EventReader // 历史读取（floor share / epoch 推导）
	Agents *agent.Supervisor
	Seats  []AgentSeat
	Policy attention.Policy // M1：open_floor 默认参数（模式参数面随 M2 Policy 配置）
	Clock  func() string    // occurred_at（RFC3339）
	Now    func() time.Time // 过期时刻计算
	NewID  func(prefix string) string
	Tenant string
	RoomID string // 非空 = 只处理该房间；空 = 全部房间（M1 默认）
}

// Engine 消费 outbox 条目，对人类消息驱动一轮讨论。
type Engine struct {
	cfg EngineConfig
}

// NewEngine 构造。
func NewEngine(cfg EngineConfig) *Engine {
	return &Engine{cfg: cfg}
}

// Deliver 实现 outbox.Consumer：仅对 message.posted 且 actor=human 的条目异步开轮。
// 引擎自产事件（actor=agent/system）不再触发，无反馈环。
func (e *Engine) Deliver(ctx context.Context, entry outbox.Entry) {
	var env protocol.Envelope
	if err := json.Unmarshal(entry.Envelope, &env); err != nil {
		return
	}
	if env.Type != protocol.EventMessagePosted || env.Actor.Kind != "human" {
		return
	}
	if e.cfg.RoomID != "" && env.RoomID != e.cfg.RoomID {
		return
	}
	go e.runRound(context.WithoutCancel(ctx), env)
}

// roomHistory 拉全量房间事件（M1 房间规模小；增量缓存随 M2 性能项）。
func (e *Engine) roomHistory(ctx context.Context, roomID string) ([]StoredEvent, error) {
	var all []StoredEvent
	cursor := ""
	for {
		events, next, err := e.cfg.Reader.EventsAfter(ctx, roomID, cursor, 1000)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
		if next == "" || len(events) == 0 {
			return all, nil
		}
		cursor = next
	}
}

// runRound 一轮：评估全部 seat → 确定性选择 → 按 rank 揭示。
func (e *Engine) runRound(ctx context.Context, stimulus protocol.Envelope) {
	roomID := stimulus.RoomID
	roundID := e.cfg.NewID("rnd")

	history, err := e.roomHistory(ctx, roomID)
	if err != nil {
		return
	}
	epoch := int64(1)
	for _, ev := range history {
		if ev.Envelope.Type == protocol.EventRoundOpened {
			epoch++
		}
	}

	// 1) round.opened
	opened := e.newEnv(roomID, protocol.EventRoundOpened,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, stimulus.EventID, roundID,
		protocol.RoundOpenedPayload{
			RoundID:         roundID,
			StimulusEventID: stimulus.EventID,
			Mode:            e.cfg.Policy.Mode,
			RevealStrategy:  "simultaneous",
			IntentWindow:    "30s",
			PolicyVersion:   "pol_m1",
		})
	if _, err := e.append(ctx, opened); err != nil {
		return
	}

	// 2) 各 seat 意图评估 → 选择输入
	var candidates []attention.Candidate
	for _, seat := range e.cfg.Seats {
		intentData, err := e.runTask(ctx, seat.Profile, agent.Task{
			TaskID:        e.cfg.NewID("tsk"),
			Kind:          agent.KindEvaluateIntent,
			ParticipantID: seat.ParticipantID,
			RoomID:        roomID,
			Epoch:         roundID,
			Context:       agent.Context{Inline: map[string]any{"stimulus": json.RawMessage(stimulus.Payload)}},
		})
		if err != nil {
			continue // agent 失败：跳过该座（M2 补 generation.failed/unavailable 事件语义）
		}
		intent, ok := intentFromData(seat.ParticipantID, intentData)
		if !ok {
			continue
		}
		intent.IntentID = e.cfg.NewID("int") // 选择前分配：Selection/Rejection 以此为键
		candidates = append(candidates, attention.Candidate{
			Intent: intent,
			Ctx: attention.ContextFeatures{
				ViewpointDiversity: 0.5, // M1 中性；结构投影 M3 接入（RFC-0006 降级路径）
				RecentFloorShare:   recentFloorShare(history, seat.ParticipantID),
				DirectAddress:      directAddress(stimulus, seat.ParticipantID),
			},
			Eligibility: attention.Eligibility{Enabled: true, CooldownOK: true, ThreadWritable: true, BudgetOK: true},
		})
	}

	// 3) 确定性选择（硬资格 + 记分卡 + MMR）
	selection := attention.Select(candidates, e.cfg.Policy)
	bandByIntent := map[string]attention.Selection{}
	for _, s := range selection.Selected {
		bandByIntent[s.IntentID] = s
	}
	rejectionByIntent := map[string]attention.Rejection{}
	for _, r := range selection.Rejected {
		rejectionByIntent[r.IntentID] = r
	}

	// 4) 全量 intent.recorded（公开 band；未获选理由进 metadata，记分卡可查 R-08）
	recordedEventByIntent := map[string]string{} // intent_id → intent.recorded 事件 id（grant causation 用）
	for _, c := range candidates {
		band, selected := "", false
		if s, ok := bandByIntent[c.Intent.IntentID]; ok {
			band, selected = s.Band, true
		} else if r, ok := rejectionByIntent[c.Intent.IntentID]; ok {
			band = r.Band
		}
		if band == "" {
			continue // 无 band（失格/越界）：M1 不落公开投影
		}
		recorded := e.newEnv(roomID, protocol.EventIntentRecorded,
			protocol.Actor{ParticipantID: c.Intent.ParticipantID, Kind: "agent"}, stimulus.EventID, roundID,
			protocol.IntentRecordedPayload{
				IntentID:        c.Intent.IntentID,
				ParticipantID:   c.Intent.ParticipantID,
				Action:          c.Intent.Action,
				Type:            c.Intent.Type,
				PublicRationale: truncate(c.Intent.PublicRationale, 280),
				ScoreBand:       band,
				Selected:        selected,
				Endorsed:        false,
			})
		if !selected {
			recorded.Metadata = map[string]any{"unselected_reason": rejectionByIntent[c.Intent.IntentID].Reason}
		}
		appendedIntent, err := e.append(ctx, recorded)
		if err != nil {
			return
		}
		recordedEventByIntent[c.Intent.IntentID] = appendedIntent[0].EventID
	}

	// 5) 按 rank 揭示：grant → generate → agent 发言
	published := 0
	for _, sel := range selection.Selected {
		if !e.revealCandidate(ctx, roomID, roundID, stimulus, sel, epoch, recordedEventByIntent[sel.IntentID]) {
			return
		}
		published++
	}

	// 6) round.closed（零公开发言是合法结果，AR-002）
	outcome := "published"
	if published == 0 {
		outcome = "quiescent"
	}
	closed := e.newEnv(roomID, protocol.EventRoundClosed,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, stimulus.EventID, roundID,
		protocol.RoundClosedPayload{
			RoundID:        roundID,
			Outcome:        outcome,
			SelectedCount:  published,
			SilentCount:    selection.SilentCount,
			CrossSubrounds: 0,
		})
	_, _ = e.append(ctx, closed)
}

// revealCandidate 单个获选者的揭示链：floor.granted（causation=该候选 intent.recorded，
// RFC-0003：Intent → 授权可追溯）→ generate → message.posted。
func (e *Engine) revealCandidate(ctx context.Context, roomID, roundID string, stimulus protocol.Envelope,
	sel attention.Selection, epoch int64, intentEventID string) bool {

	version, err := e.cfg.Store.RoomVersion(ctx, roomID)
	if err != nil {
		return false
	}
	grantID := e.cfg.NewID("grant")
	grant := e.newEnv(roomID, protocol.EventFloorGranted,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, intentEventID, roundID,
		protocol.FloorGrantedPayload{
			GrantID:          grantID,
			RoundID:          roundID,
			ParticipantID:    sel.ParticipantID,
			Rank:             sel.Rank,
			RevealStrategy:   "simultaneous",
			ContextWatermark: int(version),
			Epoch:            int(epoch),
			ExpiresAt:        e.cfg.Now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano),
			ResponseCap:      600,
			Directed:         false,
		})
	appended, err := e.append(ctx, grant)
	if err != nil {
		return false
	}

	draft, err := e.runTask(ctx, e.profileOf(sel.ParticipantID), agent.Task{
		TaskID:        e.cfg.NewID("tsk"),
		Kind:          agent.KindGenerate,
		ParticipantID: sel.ParticipantID,
		RoomID:        roomID,
		Epoch:         roundID,
		Grant: &agent.Grant{
			GrantID:        grantID,
			Rank:           sel.Rank,
			RevealStrategy: "simultaneous",
			ViewCursor:     "",
			Epoch:          epoch,
		},
		Context: agent.Context{Inline: map[string]any{"stimulus": json.RawMessage(stimulus.Payload)}},
	})
	if err != nil {
		// grant 未消费：撤销收尾（本轮其余获选者继续——AR-008 语义）
		revoked := e.newEnv(roomID, protocol.EventFloorRevoked,
			protocol.Actor{ParticipantID: "par_system", Kind: "system"}, grant.EventID, roundID,
			protocol.FloorRevokedPayload{GrantID: grantID, Reason: "human_preemption"})
		_, _ = e.append(ctx, revoked)
		return true
	}

	msg := e.newEnv(roomID, protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: sel.ParticipantID, Kind: "agent"}, appended[0].EventID, roundID, draft)
	_, err = e.append(ctx, msg)
	return err == nil
}

func (e *Engine) profileOf(participantID string) agent.Profile {
	for _, seat := range e.cfg.Seats {
		if seat.ParticipantID == participantID {
			return seat.Profile
		}
	}
	return agent.Profile{}
}

// intentFromData 适配器 turn_intent 结果 → 域 Intent（严格校验字段存在性）。
// IntentID 此时尚未分配（intent.recorded 时生成）——选择内部以 participant 为键。
func intentFromData(participantID string, data map[string]any) (attention.Intent, bool) {
	action, _ := data["action"].(string)
	intentType, _ := data["type"].(string)
	if action == "" || (intentType == "" && action != "silent") {
		return attention.Intent{}, false
	}
	scores, _ := data["scores"].(map[string]any)
	rationale, _ := data["public_rationale"].(string)
	intent := attention.Intent{
		ParticipantID:   participantID,
		Action:          action,
		Type:            intentType,
		PublicRationale: rationale,
	}
	if scores != nil {
		intent.Scores = attention.Scores{
			Relevance:  floatOf(scores["relevance"]),
			Novelty:    floatOf(scores["novelty"]),
			Urgency:    floatOf(scores["urgency"]),
			Confidence: floatOf(scores["confidence"]),
		}
	}
	return intent, true
}

func floatOf(v any) float64 {
	f, _ := v.(float64)
	return f
}

// recentFloorShare 该参与者最近发言占比（M1：全历史 agent 消息窗口；
// 有界窗口与半衰期随公平机制完善——RFC-0003 §3.1.6 校准门内做）。
func recentFloorShare(history []StoredEvent, participantID string) float64 {
	total, mine := 0, 0
	for _, ev := range history {
		if ev.Envelope.Type != protocol.EventMessagePosted || ev.Envelope.Actor.Kind != "agent" {
			continue
		}
		total++
		if ev.Envelope.Actor.ParticipantID == participantID {
			mine++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(mine) / float64(total)
}

// directAddress 刺激是否点名该参与者（定向交锋快速通道 §3.1.9 的特征输入）。
func directAddress(stimulus protocol.Envelope, participantID string) float64 {
	var payload struct {
		AddressedTo []string `json:"addressed_to"`
	}
	if err := json.Unmarshal(stimulus.Payload, &payload); err != nil {
		return 0
	}
	for _, p := range payload.AddressedTo {
		if p == participantID {
			return 1.0
		}
	}
	return 0
}

func (e *Engine) runTask(ctx context.Context, profile agent.Profile, task agent.Task) (map[string]any, error) {
	handle, err := e.cfg.Agents.Submit(ctx, profile, task)
	if err != nil {
		return nil, fmt.Errorf("engine: submit %s: %w", task.Kind, err)
	}
	result, err := handle.Result()
	if err != nil {
		return nil, fmt.Errorf("engine: result %s: %w", task.Kind, err)
	}
	return result.Data, nil
}

func (e *Engine) append(ctx context.Context, env protocol.Envelope) ([]protocol.Envelope, error) {
	appended, err := e.cfg.Store.AppendEvents(ctx, []protocol.Envelope{env})
	if err != nil {
		return nil, fmt.Errorf("engine: append %s: %w", env.Type, err)
	}
	return appended, nil
}

func (e *Engine) newEnv(roomID, typ string, actor protocol.Actor, causation, correlation string, payload any) protocol.Envelope {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{}`)
	}
	var causationPtr *string
	if causation != "" {
		causationPtr = &causation
	}
	var correlationPtr *string
	if correlation != "" {
		correlationPtr = &correlation
	}
	return protocol.Envelope{
		EventID:       e.cfg.NewID("evt"),
		TenantID:      e.cfg.Tenant,
		RoomID:        roomID,
		Type:          typ,
		SchemaVersion: 1,
		OccurredAt:    e.cfg.Clock(),
		Actor:         actor,
		CausationID:   causationPtr,
		CorrelationID: correlationPtr,
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       raw,
		Metadata:      map[string]any{},
	}
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
