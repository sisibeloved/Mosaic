// 房间引擎（M1 轮循环；切片 F/G 后含预算 admission、上下文组装、draft 流、迟到拒绝）：
// 人类消息 →（预算门控/暂停检查）→ round.opened → contextx.Assemble（七层+Receipt）
// → 各 seat 意图评估 → attention.Select（硬资格含预算 admission + 记分卡 + MMR）
// → 全量 intent.recorded（band+usage）→ 按 rank floor.granted（epoch）
// → generate（DraftUpdate 安全子集经 OnDraft 透传）→ 迟到检查（暂停/新 epoch → floor.revoked）
// → message.posted(agent, causation=grant, usage 入 metadata) → round.closed。
// 崩溃语义（RFC-0003 3.4）：轮状态由事件重建，未提交的选择重算。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// AgentSeat 房间内一个 agent 参与者（Profile 决定适配器，ParticipantID 为房间内身份）。
type AgentSeat struct {
	ParticipantID string
	Profile       agent.Profile
}

// ReceiptStore 上下文回执落库端口（nil 则跳过落库，M1 测试场景）。
type ReceiptStore interface {
	InsertReceipt(ctx context.Context, receipt contextx.Receipt) error
}

// DraftSink 草稿流出口（安全子集：text_delta/stage；广播侧负责可见性，M1 仅 public）。
type DraftSink func(roomID, participantID string, update agent.DraftUpdate)

// EngineConfig 引擎依赖。
type EngineConfig struct {
	Store       AtomicStore
	Reader      EventReader // 历史读取（floor share / epoch / 预算账本重建）
	Agents      *agent.Supervisor
	Seats       []AgentSeat
	Policy      attention.Policy // open_floor 默认参数（模式参数面随 M2 Policy 配置）
	Budget      contextx.Limits  // 预算上限（0 = 不限；M1 默认宽裕，防失控而非精确计费）
	Receipts    ReceiptStore     // 可选
	OnDraft     DraftSink        // 可选：草稿流出口
	Logger      *slog.Logger     // 可选：缺省 slog.Default()（轮中止/门控不再静默）
	ResponseCap int64            // 对称预留的单发言 token 上限（默认 600）
	Clock       func() string    // occurred_at（RFC3339）
	Now         func() time.Time // 过期时刻计算
	NewID       func(prefix string) string
	Tenant      string
	RoomID      string // 非空 = 只处理该房间；空 = 全部房间（M1 默认）
}

// Engine 消费 outbox 条目，对人类消息驱动一轮讨论。
type Engine struct {
	cfg EngineConfig
	// lifecycle 是引擎自有的生命周期 ctx（不随分发器 ctx 结束——分发停止不等于轮取消；
	// 但 Close() 会取消它，驱动在途任务取消，进程退出不孤儿化 agent 子进程）。
	lifecycle context.Context
	stop      context.CancelFunc
	// roomLocks 同房间轮串行（roomID → *sync.Mutex）：两条人类消息并发到达时
	// 不产生同 epoch 双轮——epoch 机制只兜底跨进程竞态，进程内先串行。
	roomLocks sync.Map
}

// NewEngine 构造。
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.ResponseCap <= 0 {
		cfg.ResponseCap = 600
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{cfg: cfg, lifecycle: ctx, stop: cancel}
}

// Close 关停引擎：取消在途轮与 agent 任务（适配器经 ctx 击杀子进程组），
// 拒绝新轮。已提交事件构成可恢复状态（RFC-0003 3.4）。幂等。
func (e *Engine) Close() { e.stop() }

// Deliver 实现 outbox.Consumer：仅对 message.posted 且 actor=human 的条目异步开轮。
// 引擎自产事件（actor=agent/system）不再触发，无反馈环。
func (e *Engine) Deliver(_ context.Context, entry outbox.Entry) {
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
	go e.runRound(e.lifecycle, env)
}

// lockRoom 取（或建）房间互斥锁。
func (e *Engine) lockRoom(roomID string) *sync.Mutex {
	mu, _ := e.roomLocks.LoadOrStore(roomID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (e *Engine) warn(roomID, msg string, args ...any) {
	e.cfg.Logger.Warn("engine: "+msg, append([]any{"room", roomID}, args...)...)
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

// roomPaused 自事件重建暂停态：最后一个 room.paused 之后无 room.started 即暂停。
func roomPaused(events []StoredEvent) bool {
	paused := false
	for _, ev := range events {
		switch ev.Envelope.Type {
		case protocol.EventRoomPaused:
			paused = true
		case protocol.EventRoomStarted:
			paused = false
		}
	}
	return paused
}

func countRounds(events []StoredEvent) int64 {
	var n int64
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventRoundOpened {
			n++
		}
	}
	return n
}

// runRound 一轮：预算/暂停门控 → 评估全部 seat → 确定性选择 → 按 rank 揭示。
func (e *Engine) runRound(ctx context.Context, stimulus protocol.Envelope) {
	if ctx.Err() != nil { // 已 Close：不再开轮
		return
	}
	roomID := stimulus.RoomID
	roundID := e.cfg.NewID("rnd")

	mu := e.lockRoom(roomID)
	mu.Lock()
	defer mu.Unlock()

	history, err := e.roomHistory(ctx, roomID)
	if err != nil {
		e.warn(roomID, "history 读取失败，轮中止", "err", err)
		return
	}

	// 门控 1：暂停（R-03：人类打断提升优先级——暂停期间不开自动轮，人类消息不受限）
	if roomPaused(history) {
		return
	}
	// 门控 2：预算 admission（100% 硬停自动续聊；90% 降级 speaker；只作 admission 不进排序）
	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	ledger := contextx.RebuildBudget(envs)
	if !ledger.Admit(e.cfg.Budget) {
		return
	}
	policy := e.cfg.Policy
	policy.MaxSpeakers = ledger.ReducedSpeakers(e.cfg.Budget, policy.MaxSpeakers)
	if policy.MaxSpeakers <= 0 {
		return
	}

	epoch := countRounds(history) + 1

	// 1) round.opened
	opened := e.newEnv(roomID, protocol.EventRoundOpened,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, stimulus.EventID, roundID,
		protocol.RoundOpenedPayload{
			RoundID:         roundID,
			StimulusEventID: stimulus.EventID,
			Mode:            policy.Mode,
			RevealStrategy:  "simultaneous",
			IntentWindow:    "30s",
			PolicyVersion:   "pol_m1",
		})
	if _, err := e.append(ctx, opened); err != nil {
		e.warn(roomID, "round.opened 落库失败，轮中止", "err", err)
		return
	}

	// 2) 上下文组装（七层最小 + Receipt；同轮各任务共享组装、逐任务 Receipt）
	seatsMin := make([]contextx.Seat, len(e.cfg.Seats))
	for i, s := range e.cfg.Seats {
		seatsMin[i] = contextx.Seat{ParticipantID: s.ParticipantID}
	}
	assembled := contextx.Assemble(contextx.Config{
		RoomID: roomID, TaskID: roundID, Mode: policy.Mode, Seats: seatsMin,
		RecentWindow: 10,
		Budget: contextx.BudgetState{
			RemainingTokens: remainingTokens(ledger, e.cfg.Budget),
			Level:           ledger.Level(e.cfg.Budget),
		},
	}, envs, stimulus)
	if e.cfg.Receipts != nil {
		assembled.Receipt.CreatedAt = e.cfg.Clock() // 引擎时钟赋值（恒空串是审计缺口）
		if err := e.cfg.Receipts.InsertReceipt(ctx, assembled.Receipt); err != nil {
			// 回执落库失败不阻塞讨论（Receipt 可由层摘要复算重建）
			e.warn(roomID, "context receipt 落库失败", "err", err)
		}
	}
	taskContext := agent.Context{
		Inline:     assembled.Inline,
		ReceiptRef: assembled.Receipt.ReceiptID,
	}

	// 3) 各 seat 意图评估 → 选择输入（预算 admission：对称预留不足者失格）
	var candidates []attention.Candidate
	evalUsage := map[string]*agent.Usage{} // 评估 token 入账（三维账本的评估侧）
	for _, seat := range e.cfg.Seats {
		intentResult, err := e.runTask(ctx, seat.Profile, seat.ParticipantID, agent.Task{
			TaskID:        e.cfg.NewID("tsk"),
			Kind:          agent.KindEvaluateIntent,
			ParticipantID: seat.ParticipantID,
			RoomID:        roomID,
			Epoch:         roundID,
			Context:       taskContext,
		})
		if err != nil {
			if ctx.Err() != nil {
				return // Close/取消：整轮中止，不留半截事件链
			}
			e.warn(roomID, "意图评估失败，跳过该座", "seat", seat.ParticipantID, "err", err)
			continue // agent 失败：跳过该座（M2 补 generation.failed/unavailable 事件语义）
		}
		evalUsage[seat.ParticipantID] = intentResult.Usage
		intent, ok := intentFromData(seat.ParticipantID, intentResult.Data)
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
			Eligibility: attention.Eligibility{
				Enabled:        true,
				CooldownOK:     true,
				ThreadWritable: true,
				BudgetOK:       ledger.ReserveOK(e.cfg.Budget, policy.MaxSpeakers, e.cfg.ResponseCap),
			},
		})
	}

	// 4) 确定性选择（硬资格 + 记分卡 + MMR）
	selection := attention.Select(candidates, policy)
	bandByIntent := map[string]attention.Selection{}
	for _, s := range selection.Selected {
		bandByIntent[s.IntentID] = s
	}
	rejectionByIntent := map[string]attention.Rejection{}
	for _, r := range selection.Rejected {
		rejectionByIntent[r.IntentID] = r
	}

	// 5) 全量 intent.recorded（R-01：失格/越界也记录——band=unranked，理由进 metadata；
	// 公开 band；未获选理由与 usage 进 metadata，记分卡可查 R-08）
	recordedEventByIntent := map[string]string{}
	for _, c := range candidates {
		band, selected := "", false
		if s, ok := bandByIntent[c.Intent.IntentID]; ok {
			band, selected = s.Band, true
		} else if r, ok := rejectionByIntent[c.Intent.IntentID]; ok {
			band = r.Band
		}
		if band == "" {
			band = "unranked" // 未进入记分（硬失格/silent/越界）：零痕迹违反 R-01 全记录
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
		recorded.Metadata = intentMetadata(rejectionByIntent[c.Intent.IntentID], selected, evalUsage[c.Intent.ParticipantID])
		appendedIntent, err := e.append(ctx, recorded)
		if err != nil {
			return
		}
		recordedEventByIntent[c.Intent.IntentID] = appendedIntent[0].EventID
	}

	// 6) 按 rank 揭示：grant → generate（draft 流）→ 迟到检查 → agent 发言
	published, revoked := 0, 0
	for _, sel := range selection.Selected {
		outcome := e.revealCandidate(ctx, roomID, roundID, stimulus, sel, epoch, recordedEventByIntent[sel.IntentID], taskContext)
		switch outcome {
		case revealPublished:
			published++
		case revealRevoked:
			revoked++
		case revealAbort:
			return
		}
	}

	// 7) round.closed（零公开发言是合法结果，AR-002；全撤销 → revoked_all）
	outcome := "published"
	switch {
	case published > 0:
		outcome = "published"
	case revoked > 0:
		outcome = "revoked_all"
	default:
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

type revealOutcome int

const (
	revealAbort revealOutcome = iota // 存储失败：中止本轮（已提交事件构成可恢复状态）
	revealPublished
	revealRevoked
)

// intentMetadata 汇总未选原因与 usage（评估 usage 来自适配器自报，入账供 RebuildBudget 汇总）。
func intentMetadata(rejection attention.Rejection, selected bool, usage *agent.Usage) map[string]any {
	md := map[string]any{}
	if !selected && rejection.Reason != "" {
		md["unselected_reason"] = rejection.Reason
	}
	if usage != nil {
		md["usage"] = map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens}
	}
	return md
}

// revealCandidate 单个获选者的揭示链：floor.granted（causation=该候选 intent.recorded）
// → generate（DraftUpdate 经 OnDraft 透传）→ 迟到检查（暂停/更新 epoch → floor.revoked，
// AR-004：正确性由 epoch 保证，在途取消尽力而为）→ message.posted。
func (e *Engine) revealCandidate(ctx context.Context, roomID, roundID string, stimulus protocol.Envelope,
	sel attention.Selection, epoch int64, intentEventID string, taskContext agent.Context) revealOutcome {

	version, err := e.cfg.Store.RoomVersion(ctx, roomID)
	if err != nil {
		return revealAbort
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
			ResponseCap:      int(e.cfg.ResponseCap),
			Directed:         false,
		})
	appended, err := e.append(ctx, grant)
	if err != nil {
		return revealAbort
	}

	draftResult, err := e.runTask(ctx, e.profileOf(sel.ParticipantID), sel.ParticipantID, agent.Task{
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
		Context: taskContext,
	})
	if err != nil {
		if ctx.Err() != nil {
			return revealAbort // 引擎关停：不写撤销收尾（事件链由恢复语义接手）
		}
		// grant 未消费：撤销收尾（本轮其余获选者继续——AR-008 语义）
		e.revoke(ctx, roomID, grant.EventID, grantID, roundID, stimulus, "generation_failed")
		e.warn(roomID, "generate 失败，撤销 grant", "seat", sel.ParticipantID, "err", err)
		return revealRevoked
	}

	// 迟到检查：生成期间房间被暂停或进入更新 epoch → 结果不发布（正文事件零迟到污染）
	fresh, err := e.roomHistory(ctx, roomID)
	if err != nil {
		return revealAbort
	}
	if roomPaused(fresh) || countRounds(fresh) > epoch {
		e.revoke(ctx, roomID, grant.EventID, grantID, roundID, stimulus, "room_paused")
		return revealRevoked
	}

	msg := e.newEnv(roomID, protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: sel.ParticipantID, Kind: "agent"}, appended[0].EventID, roundID, draftResult.Data)
	if draftResult.Usage != nil {
		msg.Metadata = map[string]any{
			"usage": map[string]any{
				"input_tokens":  draftResult.Usage.InputTokens,
				"output_tokens": draftResult.Usage.OutputTokens,
			},
		}
	}
	if _, err := e.append(ctx, msg); err != nil {
		return revealAbort
	}
	return revealPublished
}

func (e *Engine) revoke(ctx context.Context, roomID, causationEventID, grantID, roundID string, stimulus protocol.Envelope, reason string) {
	revoked := e.newEnv(roomID, protocol.EventFloorRevoked,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, causationEventID, roundID,
		protocol.FloorRevokedPayload{GrantID: grantID, Reason: reason})
	_, _ = e.append(ctx, revoked)
}

// runTask 提交任务：DraftUpdate 流经 OnDraft 透传（安全子集），阻塞至 Result。
func (e *Engine) runTask(ctx context.Context, profile agent.Profile, participantID string, task agent.Task) (agent.Result, error) {
	handle, err := e.cfg.Agents.Submit(ctx, profile, task)
	if err != nil {
		return agent.Result{}, fmt.Errorf("engine: submit %s: %w", task.Kind, err)
	}
	if e.cfg.OnDraft != nil {
		go func() {
			for update := range handle.Updates() {
				e.cfg.OnDraft(task.RoomID, participantID, update)
			}
		}()
	}
	result, err := handle.Result()
	if err != nil {
		return agent.Result{}, fmt.Errorf("engine: result %s: %w", task.Kind, err)
	}
	return result, nil
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

func remainingTokens(ledger contextx.Ledger, limits contextx.Limits) int64 {
	if limits.MaxTokens <= 0 {
		return -1 // 不限
	}
	r := limits.MaxTokens - ledger.Tokens
	if r < 0 {
		return 0
	}
	return r
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
