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
	"errors"
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
	Claims      ClaimStore       // 可选：durable handoff（二轮审校 #9；nil = 无声明直驱，测试场景）
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
	// roomQueues 同房间 FIFO（roomID → *roomQueue；复审 #16）：stimulus 到达序即
	// 处理序——此前每条刺激各起 goroutine 抢房间锁，调度序 ≠ 到达序，同房间事件
	// 可能乱序开轮。roomLocks 保留为双保险（队列外直调 runRound 时仍串行）。
	roomQueues sync.Map
	roomLocks  sync.Map
	// seats 动态座位（二轮审校 #1：运行时启用的适配器要能加入当前引擎）。
	seatsMu sync.RWMutex
	seats   []AgentSeat
}

// roomQueue 单房间串行队列：FIFO channel + 懒启动常驻 worker。
type roomQueue struct {
	ch    chan protocol.Envelope
	start sync.Once
}

// enqueue 入队并确保该房间 worker 存活：严格按到达序处理（复审 #16）。
// 缓冲 256 对个人版单房间足够；满时阻塞形成背压（不丢、不乱序）；Close 后丢弃。
func (e *Engine) enqueue(env protocol.Envelope) {
	qAny, _ := e.roomQueues.LoadOrStore(env.RoomID, &roomQueue{ch: make(chan protocol.Envelope, 256)})
	q := qAny.(*roomQueue)
	q.start.Do(func() { go e.roomWorker(q) })
	select {
	case q.ch <- env:
	case <-e.lifecycle.Done():
	}
}

// roomWorker 单房间常驻消费者：逐条跑轮（天然串行，保到达序）。
func (e *Engine) roomWorker(q *roomQueue) {
	for {
		select {
		case <-e.lifecycle.Done():
			return
		case env := <-q.ch:
			e.runRound(e.lifecycle, env)
		}
	}
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
	return &Engine{cfg: cfg, lifecycle: ctx, stop: cancel, seats: cfg.Seats}
}

// SetSeats 运行时更新座位（宿主注册表启用状态变化后由装配方调用；快照语义——
// 只影响之后的轮，在途轮沿用其开始时的座位集）。
func (e *Engine) SetSeats(seats []AgentSeat) {
	e.seatsMu.Lock()
	defer e.seatsMu.Unlock()
	e.seats = append([]AgentSeat(nil), seats...)
}

// seatsSnapshot 当前座位副本。
func (e *Engine) seatsSnapshot() []AgentSeat {
	e.seatsMu.RLock()
	defer e.seatsMu.RUnlock()
	return append([]AgentSeat(nil), e.seats...)
}

// Close 关停引擎：取消在途轮与 agent 任务（适配器经 ctx 击杀子进程组），
// 拒绝新轮。已提交事件构成可恢复状态（RFC-0003 3.4）。幂等。
func (e *Engine) Close() { e.stop() }

// Deliver 实现 outbox.Consumer：仅对 message.posted 且 actor=human 的条目开轮；
// room.started（resume）触发该房间未开轮声明的重驱动（复审 #10：暂停期间到达的
// 刺激不再只能等进程重启才重放）。引擎自产事件（actor=agent/system）不再触发，无反馈环。
// durable handoff（二轮审校 #9）：配置 ClaimStore 时先落声明行再返回——dispatcher
// 随后才确认 outbox。声明落库失败返回错误：分发器不确认该条目、按原序重投
// （复审 #15：fail closed——内存直驱在崩溃窗口内会丢轮，宁可重试不可丢）。
func (e *Engine) Deliver(ctx context.Context, entry outbox.Entry) error {
	var env protocol.Envelope
	if err := json.Unmarshal(entry.Envelope, &env); err != nil {
		return nil // 非信封条目：跳过（毒条目不阻塞分发）
	}
	if e.cfg.RoomID != "" && env.RoomID != e.cfg.RoomID {
		return nil
	}
	switch {
	case env.Type == protocol.EventRoomStarted:
		// 复审 #19：重驱动扫描失败退回分发器（resume 条目不确认、按序重投）——
		// 只告警会让该次 resume 的重驱动静默丢失。
		if err := e.redriveRoomClaims(env.RoomID); err != nil {
			return fmt.Errorf("engine: redrive room %s claims: %w", env.RoomID, err)
		}
		return nil
	case env.Type != protocol.EventMessagePosted || env.Actor.Kind != "human":
		return nil
	}
	if e.cfg.Claims != nil {
		newly, err := e.cfg.Claims.ClaimStimulus(ctx, env.RoomID, env.EventID, entry.Envelope, entry.GlobalPos)
		if err != nil {
			e.warn(env.RoomID, "stimulus 声明落库失败（退回分发器待重投）", "stimulus", env.EventID, "err", err)
			return fmt.Errorf("engine: claim stimulus %s: %w", env.EventID, err)
		}
		if !newly {
			return nil // 已声明过：outbox 重放/恢复并发的去重
		}
	}
	e.enqueue(env) // per-room FIFO（复审 #16）：到达序即处理序
	e.debug(env.RoomID, "刺激已入队", "stimulus", env.EventID)
	return nil
}

// redriveRoomClaims resume 后重驱动该房间未开轮的刺激声明（复审 #10；
// 声明按持久位升序返回——四轮复审 #14：重驱动顺序 = 刺激到达顺序）。
// 双入队竞态（多次 resume / 与 RecoverClaims 并发）由 runRound 的幂等护栏去重。
func (e *Engine) redriveRoomClaims(roomID string) error {
	if e.cfg.Claims == nil {
		return nil
	}
	claims, err := e.cfg.Claims.PendingClaims(e.lifecycle)
	if err != nil {
		e.warn(roomID, "resume 重驱动扫描失败（退回分发器待重投）", "err", err)
		return err
	}
	for _, c := range claims {
		if c.RoomID != roomID {
			continue
		}
		var env protocol.Envelope
		if json.Unmarshal(c.Envelope, &env) != nil {
			_ = e.cfg.Claims.DeleteClaim(e.lifecycle, c.RoomID, c.StimulusEventID) // 信封损坏：清毒声明
			continue
		}
		e.warn(roomID, "resume 重驱动未开轮的刺激声明", "stimulus", c.StimulusEventID)
		e.enqueue(env)
	}
	return nil
}

// RecoverClaims 启动恢复：扫描声明未清的刺激——已开轮的清声明，未开轮的重驱动。
func (e *Engine) RecoverClaims() {
	if e.cfg.Claims == nil {
		return
	}
	claims, err := e.cfg.Claims.PendingClaims(e.lifecycle)
	if err != nil {
		e.warn("", "claim 恢复扫描失败", "err", err)
		return
	}
	for _, c := range claims {
		var env protocol.Envelope
		if json.Unmarshal(c.Envelope, &env) != nil {
			_ = e.cfg.Claims.DeleteClaim(e.lifecycle, c.RoomID, c.StimulusEventID) // 信封损坏：清除毒声明
			continue
		}
		if e.roundForStimulus(c.RoomID, c.StimulusEventID) {
			_ = e.cfg.Claims.DeleteClaim(e.lifecycle, c.RoomID, c.StimulusEventID)
			continue
		}
		e.warn(c.RoomID, "恢复未开轮的刺激声明", "stimulus", c.StimulusEventID)
		e.enqueue(env)
	}
}

// roundForStimulus 房间是否已有该刺激的 round.opened（恢复去重依据）。
func (e *Engine) roundForStimulus(roomID, stimulusEventID string) bool {
	events, err := e.roomHistory(e.lifecycle, roomID)
	if err != nil {
		return false
	}
	return roundOpenedForStimulus(events, stimulusEventID)
}

// roundOpenedForStimulus 历史中是否存在该刺激的 round.opened（纯函数）。
func roundOpenedForStimulus(events []StoredEvent, stimulusEventID string) bool {
	for _, ev := range events {
		if ev.Envelope.Type != protocol.EventRoundOpened {
			continue
		}
		var p protocol.RoundOpenedPayload
		if json.Unmarshal(ev.Envelope.Payload, &p) == nil && p.StimulusEventID == stimulusEventID {
			return true
		}
	}
	return false
}

// roundOpenedSeq 指定轮 round.opened 的 seq（迟到检查的 fence 锚点；0 = 未找到）。
func roundOpenedSeq(events []StoredEvent, roundID string) int64 {
	for _, ev := range events {
		if ev.Envelope.Type != protocol.EventRoundOpened {
			continue
		}
		var p protocol.RoundOpenedPayload
		if json.Unmarshal(ev.Envelope.Payload, &p) == nil && p.RoundID == roundID {
			return ev.Envelope.Seq
		}
	}
	return 0
}

// lockRoom 取（或建）房间互斥锁。
func (e *Engine) lockRoom(roomID string) *sync.Mutex {
	mu, _ := e.roomLocks.LoadOrStore(roomID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (e *Engine) warn(roomID, msg string, args ...any) {
	e.cfg.Logger.Warn("engine: "+msg, append([]any{"room", roomID}, args...)...)
}

// debug 调试级日志（开发者模式 M1 v1.8）：轮链路各环节的 ids 全部落日志——
// 复盘时以任一 id（stimulus/round/grant/task）grep 即可还原完整链路。
func (e *Engine) debug(roomID, msg string, args ...any) {
	e.cfg.Logger.Debug("engine: "+msg, append([]any{"room", roomID}, args...)...)
}

// Seats 当前座位快照（开发者模式状态端点用；与轮执行同一份数据）。
func (e *Engine) Seats() []AgentSeat { return e.seatsSnapshot() }

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

// pausedAfter fence（二轮审校 #7；复审 #13）：锚点 seq 之后出现过 room.paused
// （即便其后已 resume）即视为失效——"最终是否暂停"检查挡不住 pause→resume 快速往返。
// 锚点由调用方给定（本轮 round.opened 的 seq）。
func pausedAfter(events []StoredEvent, anchorSeq int64) bool {
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventRoomPaused && ev.Envelope.Seq > anchorSeq {
			return true
		}
	}
	return false
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

	// 幂等护栏（复审 #10/#16）：同刺激已有 round.opened（outbox 重投 / resume 重驱动 /
	// RecoverClaims 与在途轮竞态）——清声明即返回，不双开轮。
	if roundOpenedForStimulus(history, stimulus.EventID) {
		if e.cfg.Claims != nil {
			_ = e.cfg.Claims.DeleteClaim(ctx, roomID, stimulus.EventID)
		}
		return
	}

	// 门控 1：暂停（R-03：人类打断提升优先级——暂停期间不开自动轮，人类消息不受限）
	if roomPaused(history) {
		e.debug(roomID, "轮跳过：房间暂停", "stimulus", stimulus.EventID)
		return
	}
	// 门控 2：预算 admission（100% 硬停自动续聊；90% 降级 speaker；只作 admission 不进排序）
	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	ledger := contextx.RebuildBudget(envs)
	if !ledger.Admit(e.cfg.Budget) {
		e.debug(roomID, "轮跳过：预算熔断", "stimulus", stimulus.EventID,
			"rounds", ledger.Rounds, "utterances", ledger.Utterances, "tokens", ledger.Tokens)
		return
	}
	policy := e.cfg.Policy
	policy.MaxSpeakers = ledger.ReducedSpeakers(e.cfg.Budget, policy.MaxSpeakers)
	if policy.MaxSpeakers <= 0 {
		e.debug(roomID, "轮跳过：speaker 降级至零（预算 100%）", "stimulus", stimulus.EventID)
		return
	}

	epoch := countRounds(history) + 1
	e.debug(roomID, "轮开始", "round", roundID, "stimulus", stimulus.EventID, "epoch", epoch)

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
	if e.cfg.Claims != nil {
		// 声明使命完成（认领→开轮）：清除（失败不致命——恢复扫描会按已开轮清理）
		if err := e.cfg.Claims.DeleteClaim(ctx, roomID, stimulus.EventID); err != nil {
			e.warn(roomID, "claim 清除失败（恢复扫描兜底）", "err", err)
		}
	}

	// 2) 上下文组装（七层最小 + Receipt；同轮各任务共享组装、逐任务 Receipt）
	seats := e.seatsSnapshot()
	seatsMin := make([]contextx.Seat, len(seats))
	for i, s := range seats {
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
	stimulusThread := ""
	if stimulus.ThreadID != nil {
		stimulusThread = *stimulus.ThreadID // 线程归属透传（agent-native：回复不丢线程）
	}
	// 相 1：全部评估先跑完（四轮复审 #10——本轮评估消耗要进同一轮的 admission，
	// 必须先汇总用量再判预算资格；按座位序收集，确定性保持）。
	type seatEval struct {
		seat   AgentSeat
		result agent.Result
	}
	var evals []seatEval
	for _, seat := range e.seatsSnapshot() {
		intentResult, err := e.runTask(ctx, seat.Profile, seat.ParticipantID, agent.Task{
			TaskID:        e.cfg.NewID("tsk"),
			Kind:          agent.KindEvaluateIntent,
			ParticipantID: seat.ParticipantID,
			RoomID:        roomID,
			ThreadID:      stimulusThread,
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
		evals = append(evals, seatEval{seat: seat, result: intentResult})
	}
	// 相 1.5：评估消耗入"现在"的账本——BudgetOK 用当下余额判对称预留，
	// 评估前的旧账本会让本轮 eval 开销绕过同轮 admission（四轮复审 #10）。
	ledgerNow := ledger
	for _, u := range evalUsage {
		if u != nil {
			ledgerNow.Tokens += u.InputTokens + u.OutputTokens
		}
	}
	// 相 2：结构校验 + 全记录 + 候选构建（座位序）。
	for _, ev := range evals {
		intent, ok := intentFromData(ev.seat.ParticipantID, ev.result.Data)
		if !ok {
			// 结构校验失败即弃权：不虚构零分参与排序（二轮审校 #8）。
			// 复审 #12：弃权仍全记录（R-01）且真实 usage 必须入账——畸形输出的
			// 评估开销不得绕过预算账本（token 已花，事件零痕迹即账本漏记）。
			e.warn(roomID, "intent 结构非法，该座弃权", "seat", ev.seat.ParticipantID)
			invalid := e.newEnv(roomID, protocol.EventIntentRecorded,
				protocol.Actor{ParticipantID: ev.seat.ParticipantID, Kind: "agent"}, stimulus.EventID, roundID,
				protocol.IntentRecordedPayload{
					IntentID:        e.cfg.NewID("int"),
					ParticipantID:   ev.seat.ParticipantID,
					Action:          "silent", // 弃权语义（schema 合法枚举），band=unranked
					ScoreBand:       "unranked",
					Selected:        false,
					Endorsed:        false,
					PublicRationale: "intent 结构非法，弃权",
				})
			invalid.Metadata = intentMetadata(
				attention.Rejection{Reason: "invalid_intent_structure"}, false, ev.result.Usage)
			if _, err := e.append(ctx, invalid); err != nil {
				return
			}
			continue
		}
		intent.IntentID = e.cfg.NewID("int") // 选择前分配：Selection/Rejection 以此为键
		candidates = append(candidates, attention.Candidate{
			Intent: intent,
			Ctx: attention.ContextFeatures{
				ViewpointDiversity: 0.5, // M1 中性；结构投影 M3 接入（RFC-0006 降级路径）
				RecentFloorShare:   recentFloorShare(history, ev.seat.ParticipantID),
				DirectAddress:      directAddress(stimulus, ev.seat.ParticipantID),
			},
			Eligibility: attention.Eligibility{
				Enabled:        true,
				CooldownOK:     true,
				ThreadWritable: true,
				BudgetOK:       ledgerNow.ReserveOK(e.cfg.Budget, policy.MaxSpeakers, e.cfg.ResponseCap),
			},
		})
	}

	// 4) 确定性选择（硬资格 + 记分卡 + MMR）
	selection := attention.Select(candidates, policy)
	e.debug(roomID, "选择完成", "round", roundID,
		"candidates", len(candidates), "selected", len(selection.Selected),
		"rejected", len(selection.Rejected), "silent", selection.SilentCount)
	for _, sel := range selection.Selected {
		e.debug(roomID, "获选", "round", roundID,
			"intent", sel.IntentID, "participant", sel.ParticipantID, "rank", sel.Rank, "band", sel.Band)
	}
	for _, rej := range selection.Rejected {
		e.debug(roomID, "未获选", "round", roundID,
			"intent", rej.IntentID, "reason", rej.Reason, "band", rej.Band)
	}
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
	e.debug(roomID, "轮结束", "round", roundID, "outcome", outcome,
		"published", published, "revoked", revoked)
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
	e.debug(roomID, "floor 已授予", "round", roundID, "grant", grantID,
		"participant", sel.ParticipantID, "rank", sel.Rank, "epoch", epoch)

	generateThread := ""
	if stimulus.ThreadID != nil {
		generateThread = *stimulus.ThreadID
	}
	draftResult, err := e.runTask(ctx, e.profileOf(sel.ParticipantID), sel.ParticipantID, agent.Task{
		TaskID:        e.cfg.NewID("tsk"),
		Kind:          agent.KindGenerate,
		ParticipantID: sel.ParticipantID,
		RoomID:        roomID,
		ThreadID:      generateThread,
		Epoch:         roundID,
		Grant: &agent.Grant{
			GrantID:        grantID,
			Rank:           sel.Rank,
			RevealStrategy: "simultaneous",
			ViewCursor:     "",
			Epoch:          epoch,
			ResponseCap:    e.cfg.ResponseCap, // 复审 #9：宣告值必须传入适配器并约束发布
		},
		Context: taskContext,
	})
	if err != nil {
		if ctx.Err() != nil {
			return revealAbort // 引擎关停：不写撤销收尾（事件链由恢复语义接手）
		}
		// grant 未消费：撤销收尾（本轮其余获选者继续——AR-008 语义）
		e.revoke(ctx, roomID, grant.EventID, grantID, roundID, stimulus, "generation_failed", nil)
		e.warn(roomID, "generate 失败，撤销 grant", "seat", sel.ParticipantID, "err", err)
		return revealRevoked
	}

	// 迟到检查（复审 #13：锚点 = 本轮 round.opened 的 seq，不再只是 grant）——
	// 本轮开轮之后出现过 room.paused 即失效：覆盖"评估阶段 pause→grant 前 resume"
	// 的往返（旧锚点只看 grant 之后，grant 前的暂停漏网）；开轮前的暂停历史
	// 不毒化重驱动的新轮（暂停期到达的刺激在 resume 后重开轮，属合法新轮）。
	// 更新 epoch 同理失效（正文事件零迟到污染）。
	fresh, err := e.roomHistory(ctx, roomID)
	if err != nil {
		return revealAbort
	}
	openedSeq := roundOpenedSeq(fresh, roundID)
	if e.fenceViolated(fresh, roundID, openedSeq, epoch) {
		e.revoke(ctx, roomID, grant.EventID, grantID, roundID, stimulus, "room_paused", draftResult.Usage)
		return revealRevoked
	}

	msg := e.newEnv(roomID, protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: sel.ParticipantID, Kind: "agent"}, appended[0].EventID, roundID, draftResult.Data)
	msg.ThreadID = stimulus.ThreadID // 线程归属：回复落在刺激线程（agent-native 缺口修复）
	if draftResult.Usage != nil {
		msg.Metadata = map[string]any{
			"usage": map[string]any{
				"input_tokens":  draftResult.Usage.InputTokens,
				"output_tokens": draftResult.Usage.OutputTokens,
			},
		}
	}
	// 正文落库走 CAS（四轮复审 #12）：以迟到检查读到的版本为期位——检查与落库
	// 之间插入的事件使 CAS 失败；回读后真迟到（暂停/epoch）→ 撤销，良性交错
	// （如人类消息）→ 换新期位重试。无 CAS 能力的测试存储退化为普通追加。
	expected := expectedVersionOf(fresh)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := e.appendCAS(ctx, msg, expected); err == nil {
			e.debug(roomID, "agent 发言已发布", "round", roundID, "grant", grantID,
				"participant", sel.ParticipantID, "event", msg.EventID)
			return revealPublished
		} else if !errors.Is(err, ErrVersionConflict) {
			return revealAbort
		}
		recheck, err := e.roomHistory(ctx, roomID)
		if err != nil {
			return revealAbort
		}
		if e.fenceViolated(recheck, roundID, roundOpenedSeq(recheck, roundID), epoch) {
			e.revoke(ctx, roomID, grant.EventID, grantID, roundID, stimulus, "room_paused", draftResult.Usage)
			return revealRevoked
		}
		expected = expectedVersionOf(recheck) // 良性交错：换新期位重试
	}
	e.warn(roomID, "正文 CAS 重试耗尽，轮中止", "round", roundID, "grant", grantID)
	return revealAbort
}

// fenceViolated 迟到判定（fresh 重读后复用）：当前暂停 / 本轮开轮后出现过暂停 /
// 进入更新 epoch。
func (e *Engine) fenceViolated(events []StoredEvent, roundID string, openedSeq, epoch int64) bool {
	return roomPaused(events) || (openedSeq > 0 && pausedAfter(events, openedSeq)) || countRounds(events) > epoch
}

// expectedVersionOf CAS 期位：fresh 历史的最新 seq（EventsAfter 按 seq 序，末元素即最大）。
func expectedVersionOf(events []StoredEvent) int64 {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Envelope.Seq
}

// appendCAS 条件追加：存储具备 CASStore 能力走乐观并发，否则普通追加（测试存储）。
func (e *Engine) appendCAS(ctx context.Context, env protocol.Envelope, expected int64) ([]protocol.Envelope, error) {
	if cas, ok := e.cfg.Store.(CASStore); ok {
		return cas.AppendEventsIf(ctx, []protocol.Envelope{env}, expected)
	}
	return e.append(ctx, env)
}

func (e *Engine) revoke(ctx context.Context, roomID, causationEventID, grantID, roundID string, stimulus protocol.Envelope, reason string, usage *agent.Usage) {
	revoked := e.newEnv(roomID, protocol.EventFloorRevoked,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, causationEventID, roundID,
		protocol.FloorRevokedPayload{GrantID: grantID, Reason: reason})
	// 四轮复审 #13：被撤销的生成也已消耗 token——usage 入 metadata，
	// RebuildBudget 汇总 floor.revoked（否则账本永久漏计这批开销）。
	if usage != nil {
		revoked.Metadata = map[string]any{
			"usage": map[string]any{
				"input_tokens":  usage.InputTokens,
				"output_tokens": usage.OutputTokens,
			},
		}
	}
	_, _ = e.append(ctx, revoked)
}

// runTask 提交任务：DraftUpdate 流经 OnDraft 透传（安全子集），阻塞至 Result。
func (e *Engine) runTask(ctx context.Context, profile agent.Profile, participantID string, task agent.Task) (agent.Result, error) {
	e.debug(task.RoomID, "适配器任务提交", "task", task.TaskID, "kind", task.Kind,
		"participant", participantID, "adapter", profile.Adapter, "epoch", task.Epoch)
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
	e.debug(task.RoomID, "适配器任务完成", "task", task.TaskID, "kind", task.Kind,
		"participant", participantID)
	return result, nil
}

func (e *Engine) profileOf(participantID string) agent.Profile {
	for _, seat := range e.seatsSnapshot() {
		if seat.ParticipantID == participantID {
			return seat.Profile
		}
	}
	return agent.Profile{}
}

// intentActionSet/intentTypeSet 适配器输出的合法枚举（RFC-0003 §3.1.2；
// 二轮审校 #8：未知枚举/非数值分数此前被静默转成合法零分进入选择引擎——严格写拒收）。
var intentActionSet = map[string]bool{
	"speak": true, "react": true, "fork": true, "summarize": true, "silent": true,
}

var intentTypeSet = map[string]bool{
	"answer": true, "extend": true, "challenge": true, "support": true,
	"question": true, "redirect": true, "synthesize": true,
}

// intentFromData 适配器 turn_intent 结果 → 域 Intent（严格校验：枚举合法、分数必为
// 数值且字段齐全；silent 允许省略 type/scores。校验失败返回 false——该座弃权，
// 不得以虚构零分参与排序）。IntentID 此时尚未分配（intent.recorded 时生成）。
func intentFromData(participantID string, data map[string]any) (attention.Intent, bool) {
	action, _ := data["action"].(string)
	if !intentActionSet[action] {
		return attention.Intent{}, false
	}
	intentType, _ := data["type"].(string)
	if intentType == "" {
		if action != "silent" { // silent 可省略 type；其余必须有
			return attention.Intent{}, false
		}
	} else if !intentTypeSet[intentType] {
		return attention.Intent{}, false
	}
	rationale, _ := data["public_rationale"].(string)
	intent := attention.Intent{
		ParticipantID:   participantID,
		Action:          action,
		Type:            intentType,
		PublicRationale: rationale,
	}
	if action == "silent" {
		return intent, true
	}
	scores, ok := data["scores"].(map[string]any)
	if !ok {
		return attention.Intent{}, false
	}
	num := func(key string) (float64, bool) {
		v, ok := scores[key].(float64) // JSON 数值；字符串数字/布尔一律拒收
		return v, ok
	}
	relevance, okR := num("relevance")
	novelty, okN := num("novelty")
	urgency, okU := num("urgency")
	confidence, okC := num("confidence")
	if !okR || !okN || !okU || !okC {
		return attention.Intent{}, false // 缺字段/非数值不得默认 0（越界仍由 Select 严格拒）
	}
	intent.Scores = attention.Scores{
		Relevance:  relevance,
		Novelty:    novelty,
		Urgency:    urgency,
		Confidence: confidence,
	}
	return intent, true
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
