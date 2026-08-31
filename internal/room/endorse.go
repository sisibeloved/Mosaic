// 人类保送执行面（RFC-0003 §3.1.11 / OQ-17）：intent.endorsed(effect=grant)
// → 上下文重组（保送指令层）→ floor.granted（causation=endorsed 事件——
// 人类可追溯链：message.posted → floor.granted → intent.endorsed(human actor)）
// → 生成 → 发布。约束：不绕过预算（熔断即跳过）、硬资格（座位须在席）、
// 暂停围栏（暂停期不执行）；失败不虚构事件——结果经事件链可见（有无 grant）。
package room

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func (e *Engine) runEndorse(ctx context.Context, endorsed protocol.Envelope) {
	roomID := endorsed.RoomID
	var payload protocol.IntentEndorsedPayload
	if err := json.Unmarshal(endorsed.Payload, &payload); err != nil || payload.Effect != "grant" {
		return // 毒条目/未开放 effect：跳过（事件本身已如实记录）
	}
	mu := e.lockRoom(roomID)
	mu.Lock()
	defer mu.Unlock()

	history, err := e.roomHistory(ctx, roomID)
	if err != nil {
		e.warn(roomID, "保送执行中止：history 读取失败", "intent", payload.IntentID, "err", err)
		return
	}
	if roomPaused(history) {
		e.debug(roomID, "保送跳过：房间暂停", "intent", payload.IntentID)
		return
	}
	// 目标 intent 与参与者（intent.recorded 定位）
	var target *StoredEvent
	participant := ""
	for i := range history {
		if history[i].Envelope.Type != protocol.EventIntentRecorded {
			continue
		}
		var p struct {
			IntentID      string `json:"intent_id"`
			ParticipantID string `json:"participant_id"`
		}
		if json.Unmarshal(history[i].Envelope.Payload, &p) == nil && p.IntentID == payload.IntentID {
			target = &history[i]
			participant = p.ParticipantID
			break
		}
	}
	if target == nil {
		e.warn(roomID, "保送跳过：目标 intent 不存在", "intent", payload.IntentID)
		return
	}
	// 硬资格：座位须在席（Agent 不能保送 Agent 的执行侧等价物——不在席即无资格）
	seat := e.seatOf(participant)
	if seat == nil {
		e.warn(roomID, "保送跳过：参与者不在席", "intent", payload.IntentID, "participant", participant)
		return
	}
	// 预算：不绕过 admission
	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	ledger := contextx.RebuildBudget(envs)
	policy := RebuildPolicy(envs)
	if !ledger.Admit(e.cfg.Budget) ||
		!ledger.ReserveOK(e.cfg.Budget, 1, policy.Params.ResponseCap) {
		e.debug(roomID, "保送跳过：预算熔断/预留不足", "intent", payload.IntentID)
		return
	}

	// 上下文重组：原轮刺激为锚 + 保送指令层（OQ-17 可追溯）
	anchor := endorsed
	if target.Envelope.CausationID != nil {
		if stim := findEvent(history, *target.Envelope.CausationID); stim != nil {
			anchor = *stim
		}
	}
	seatsMin := make([]contextx.Seat, 0)
	for _, s := range e.seatsSnapshot() {
		seatsMin = append(seatsMin, contextx.Seat{ParticipantID: s.ParticipantID})
	}
	assembled := contextx.Assemble(contextx.Config{
		RoomID: roomID, TaskID: endorsed.EventID, Mode: policy.Params.Mode, Seats: seatsMin,
		RecentWindow: 10, Endorse: true,
		Budget: contextx.BudgetState{
			RemainingTokens: remainingTokens(ledger, e.cfg.Budget),
			Level:           ledger.Level(e.cfg.Budget),
		},
	}, envs, anchor)
	if e.cfg.Receipts != nil {
		assembled.Receipt.CreatedAt = e.cfg.Clock()
		if err := e.cfg.Receipts.InsertReceipt(ctx, assembled.Receipt); err != nil {
			e.warn(roomID, "context receipt 落库失败（保送）", "err", err)
		}
	}
	taskContext := agent.Context{Inline: assembled.Inline, ReceiptRef: assembled.Receipt.ReceiptID}

	sel := attention.Selection{IntentID: payload.IntentID, ParticipantID: participant, Rank: 1}
	version := int64(0)
	if len(history) > 0 {
		version = history[len(history)-1].Envelope.Seq
	}
	// 发授：causation = endorsed 事件（人类可追溯链根）
	grantEnv, grantID, ok := e.issueGrantCustom(ctx, roomID, endorsed.EventID, sel, policy, version, "par_endorse_"+payload.IntentID)
	if !ok {
		return
	}
	draft, genOK := e.runGenerate(ctx, roomID, roomID, anchor, sel, grantEnv, grantID, taskContext, policy)
	if !genOK {
		return // 已按 generation_failed 撤销
	}
	// 发布：暂停围栏（当下态）+ CAS 良性重试
	e.publishEndorsed(ctx, roomID, anchor, sel, grantEnv, grantID, draft)
}

// publishEndorsed 保送正文发布（独立于轮次 fence——保送不在轮内，
// 仅受当下暂停态与 CAS 交控制约）。
func (e *Engine) publishEndorsed(ctx context.Context, roomID string, stimulus protocol.Envelope,
	sel attention.Selection, grantEnv protocol.Envelope, grantID string, draft agent.Result) {

	fresh, err := e.roomHistory(ctx, roomID)
	if err != nil {
		return
	}
	if roomPaused(fresh) {
		e.revoke(ctx, roomID, grantEnv.EventID, grantID, "", stimulus, "room_paused", draft.Usage)
		return
	}
	msg := e.newEnv(roomID, protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: sel.ParticipantID, Kind: "agent"}, grantEnv.EventID, "", draft.Data)
	msg.ThreadID = stimulus.ThreadID
	if draft.Usage != nil {
		msg.Metadata = map[string]any{
			"usage": map[string]any{
				"input_tokens":  draft.Usage.InputTokens,
				"output_tokens": draft.Usage.OutputTokens,
			},
		}
	}
	expected := expectedVersionOf(fresh)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := e.appendCAS(ctx, msg, expected); err == nil {
			e.debug(roomID, "保送发言已发布", "grant", grantID, "participant", sel.ParticipantID)
			return
		}
		recheck, err := e.roomHistory(ctx, roomID)
		if err != nil || roomPaused(recheck) {
			if err == nil {
				e.revoke(ctx, roomID, grantEnv.EventID, grantID, "", stimulus, "room_paused", draft.Usage)
			}
			return
		}
		expected = expectedVersionOf(recheck)
	}
	e.warn(roomID, "保送正文 CAS 重试耗尽", "grant", grantID)
}

// issueGrantCustom 保送发授（grant_id 语义化前缀 par_endorse_*，与轮内 grant 区分；
// rank=1；causation=endorsed 事件——人类可追溯）。
func (e *Engine) issueGrantCustom(ctx context.Context, roomID, causationEventID string,
	sel attention.Selection, policy RoundPolicy, watermark int64, grantID string) (protocol.Envelope, string, bool) {

	grant := e.newEnv(roomID, protocol.EventFloorGranted,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, causationEventID, "",
		protocol.FloorGrantedPayload{
			GrantID:          grantID,
			RoundID:          "",
			ParticipantID:    sel.ParticipantID,
			Rank:             1,
			RevealStrategy:   policy.Params.RevealStrategy,
			ContextWatermark: int(watermark),
			Epoch:            0,
			ExpiresAt:        e.cfg.Now().Add(policy.IntentWindow).UTC().Format(time.RFC3339Nano),
			ResponseCap:      int(policy.Params.ResponseCap),
			Directed:         false,
		})
	grant.Metadata = map[string]any{"endorsed": true}
	appended, err := e.append(ctx, grant)
	if err != nil {
		e.warn(roomID, "保送发授落库失败", "grant", grantID, "err", err)
		return protocol.Envelope{}, "", false
	}
	e.debug(roomID, "floor 已授予（保送）", "grant", grantID, "participant", sel.ParticipantID)
	return grant, appended[0].EventID, true
}

// seatOf 参与者在席查找。
func (e *Engine) seatOf(participantID string) *AgentSeat {
	for _, s := range e.seatsSnapshot() {
		if s.ParticipantID == participantID {
			seat := s
			return &seat
		}
	}
	return nil
}
