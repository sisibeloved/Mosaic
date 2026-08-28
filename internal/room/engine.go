// 房间引擎（M1 最小轮循环）：人类消息触发一轮 Open Floor——
// round.opened → evaluate_intent → intent.recorded → floor.granted → generate →
// message.posted(agent, causation=grant) → round.closed。
// 切片 C（Attention 正式实现）将替换意图评估/选择内部为硬资格+记分卡+MMR；
// 本文件的轮壳（事件链与 causation 纪律）保持不变。
// 崩溃语义（RFC-0003 3.4）：轮状态由事件重建，未提交的选择重算——
// 引擎轮异步执行，dispatch 标记先于轮完成，崩溃丢失的轮由重放重建后重新触发（M2 补触发登记）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
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
	Agents *agent.Supervisor
	Seats  []AgentSeat
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

// NewEngine 构造（依赖必填由调用方保证）。
func NewEngine(cfg EngineConfig) *Engine {
	return &Engine{cfg: cfg}
}

// Deliver 实现 outbox.Consumer：仅对 message.posted 且 actor=human 的条目异步开轮。
// 引擎自产事件（actor=agent/system）不再触发，无反馈环。
// cfg.RoomID 非空时只处理该房间（多房间单引擎裁剪）；空串 = 服务全部房间（M1 默认）。
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

// runRound 执行一轮（单 agent 串行；多 agent 逐座开轮，M1 语义）。
func (e *Engine) runRound(ctx context.Context, stimulus protocol.Envelope) {
	for _, seat := range e.cfg.Seats {
		if !e.runSeatRound(ctx, stimulus, seat) {
			return // ctx 取消：中止后续轮（已提交事件构成可恢复状态）
		}
	}
}

func (e *Engine) runSeatRound(ctx context.Context, stimulus protocol.Envelope, seat AgentSeat) bool {
	roomID := stimulus.RoomID
	roundID := e.cfg.NewID("rnd")

	// 1) round.opened
	opened := e.newEnv(roomID, protocol.EventRoundOpened,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, stimulus.EventID, roundID,
		protocol.RoundOpenedPayload{
			RoundID:         roundID,
			StimulusEventID: stimulus.EventID,
			Mode:            "open_floor",
			RevealStrategy:  "simultaneous",
			IntentWindow:    "30s",
			PolicyVersion:   "pol_m1",
		})
	if _, err := e.append(ctx, opened); err != nil {
		return false
	}

	// 2) evaluate_intent（适配器任务）
	intentTask := agent.Task{
		TaskID:        e.cfg.NewID("tsk"),
		Kind:          agent.KindEvaluateIntent,
		ParticipantID: seat.ParticipantID,
		RoomID:        roomID,
		Epoch:         roundID,
		Context:       agent.Context{Inline: map[string]any{"stimulus": json.RawMessage(stimulus.Payload)}},
	}
	intent, err := e.runTask(ctx, seat.Profile, intentTask)
	if err != nil {
		return true // agent 失败：跳过该座（M1 无失败事件；M2 补 generation.failed/unavailable 语义）
	}
	action, _ := intent["action"].(string)
	intentType, _ := intent["type"].(string)
	if action == "" || intentType == "" {
		return true
	}
	if action == "silent" {
		// silent 是正常结果：不开 grant，轮以 quiescent 关闭
		e.closeRound(ctx, roomID, roundID, stimulus.EventID, "quiescent", 0, 1)
		return true
	}
	rationale, _ := intent["public_rationale"].(string)

	// 3) intent.recorded（投影：M1 band 由自报 relevance 映射，切片 C 换记分卡）
	intentID := e.cfg.NewID("int")
	recorded := e.newEnv(roomID, protocol.EventIntentRecorded,
		protocol.Actor{ParticipantID: seat.ParticipantID, Kind: "agent"}, stimulus.EventID, roundID,
		protocol.IntentRecordedPayload{
			IntentID:        intentID,
			ParticipantID:   seat.ParticipantID,
			Action:          action,
			Type:            intentType,
			PublicRationale: truncate(rationale, 280),
			ScoreBand:       bandFromIntent(intent),
			Selected:        true,
			Endorsed:        false,
		})
	if _, err := e.append(ctx, recorded); err != nil {
		return false
	}

	// 4) floor.granted（系统签发；causation 指向已记录 Intent）
	version, err := e.cfg.Store.RoomVersion(ctx, roomID)
	if err != nil {
		return false
	}
	grantID := e.cfg.NewID("grant")
	grant := e.newEnv(roomID, protocol.EventFloorGranted,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, recorded.EventID, roundID,
		protocol.FloorGrantedPayload{
			GrantID:          grantID,
			RoundID:          roundID,
			ParticipantID:    seat.ParticipantID,
			Rank:             1,
			RevealStrategy:   "simultaneous",
			ContextWatermark: int(version),
			Epoch:            1, // M1 固定；迟到拒绝随切片 C 的 epoch 计数落地
			ExpiresAt:        e.cfg.Now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano),
			ResponseCap:      600,
			Directed:         false,
		})
	if _, err := e.append(ctx, grant); err != nil {
		return false
	}

	// 5) generate（适配器任务，承载 grant）
	genTask := agent.Task{
		TaskID:        e.cfg.NewID("tsk"),
		Kind:          agent.KindGenerate,
		ParticipantID: seat.ParticipantID,
		RoomID:        roomID,
		Epoch:         roundID,
		Grant: &agent.Grant{
			GrantID:        grantID,
			Rank:           1,
			RevealStrategy: "simultaneous",
			ViewCursor:     "",
			Epoch:          1,
		},
		Context: agent.Context{Inline: map[string]any{"stimulus": json.RawMessage(stimulus.Payload)}},
	}
	draft, err := e.runTask(ctx, seat.Profile, genTask)
	if err != nil {
		e.closeRound(ctx, roomID, roundID, stimulus.EventID, "revoked_all", 1, 0) // grant 未消费：撤销收尾
		return true
	}

	// 6) message.posted（agent 发言；causation 必须指向有效 FloorGrant——RFC-0003）
	msg := e.newEnv(roomID, protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: seat.ParticipantID, Kind: "agent"}, grant.EventID, roundID,
		draft)
	if _, err := e.append(ctx, msg); err != nil {
		return false
	}

	// 7) round.closed
	e.closeRound(ctx, roomID, roundID, stimulus.EventID, "published", 1, 0)
	return true
}

func (e *Engine) closeRound(ctx context.Context, roomID, roundID, stimulusID, outcome string, selected, silent int) {
	closed := e.newEnv(roomID, protocol.EventRoundClosed,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, stimulusID, roundID,
		protocol.RoundClosedPayload{
			RoundID:        roundID,
			Outcome:        outcome,
			SelectedCount:  selected,
			SilentCount:    silent,
			CrossSubrounds: 0,
		})
	_, _ = e.append(ctx, closed)
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

// bandFromIntent 自报分数 → 五档 band（RFC-0003：band 是唯一公开粒度）。
// M1 最小映射取 relevance；切片 C 换成完整记分卡公式（确定性，不信任模型自报排序）。
func bandFromIntent(intent map[string]any) string {
	scores, _ := intent["scores"].(map[string]any)
	relevance, _ := scores["relevance"].(float64)
	switch {
	case relevance < 0.2:
		return "very_low"
	case relevance < 0.4:
		return "low"
	case relevance < 0.6:
		return "medium"
	case relevance < 0.8:
		return "high"
	default:
		return "very_high"
	}
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
