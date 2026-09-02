// 房间引擎（RFC-0012 群聊交互模型）：消息流驱动，任何 message.posted（人类或
// agent）触发去抖反应窗口（默认 3s，窗内新消息重锚合并）→ 到期开一个反应波：
// round.opened（内部记账——快照 Timeline 不收录）→ 全员意图评估（观察→判断，
// silent=自决不回）→ 意愿放行（无中央选人；记分卡分仅作波内排序与 band 透明）
// → sequential 发授/生成/发布（CAS 迟到围栏）→ 每条发布再开新窗口 → round.closed。
// 终止：意愿静默（全员 silent → quiescent，不再开窗）/ 发言冷却（上波发言者跳过，
// @点名豁免）/ 对话环检测（尾部连续 ≥6 条 agent 消息不开波）/ 预算 100% 硬顶 /
// 暂停。崩溃语义：反应窗口为内存 timer——崩溃即静默，不重复开波（人类补发即续）。
package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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

// WaveSkipSink 波门控跳过通知出口（reason：paused | ring | thread_inactive | budget |
// cooldown）。瞬态信号——门控跳过不是语义事件，不落事件日志；广播侧以瞬态帧投递，
// 供开发者模式在房间内向用户解释"为什么没人说话"。
type WaveSkipSink func(roomID, reason string)

// EngineConfig 引擎依赖。
type EngineConfig struct {
	Store          AtomicStore
	Reader         EventReader // 历史读取（floor share / epoch / 预算账本重建）
	Agents         *agent.Supervisor
	Seats          []AgentSeat
	Budget         contextx.Limits  // 预算上限（0 = 不限；防失控而非精确计费）
	ReactionWindow time.Duration    // 反应窗口去抖时长（RFC-0012 §2.1；0 = 默认 3s）
	Receipts       ReceiptStore     // 可选
	OnDraft        DraftSink        // 可选：草稿流出口
	OnWaveSkip     WaveSkipSink     // 可选：波门控跳过通知（开发者模式可观测性）
	Logger         *slog.Logger     // 可选，缺省 slog.Default()（波中止/门控不再静默）
	Claims         ClaimStore       // 可选：durable handoff（二轮审校 #9；nil = 无声明直驱，测试场景）
	Clock          func() string    // occurred_at（RFC3339）
	Now            func() time.Time // 过期时刻计算
	NewID          func(prefix string) string
	Tenant         string
	RoomID         string // 非空 = 只处理该房间；空 = 全部房间（M1 默认）
}

// chatGrantPolicy 群聊模型的引擎内固定策略（RFC-0012：无房间策略面——
// 单条消息长度上限与发授期限为引擎常量，预算为引擎配置）。
type chatGrantPolicy struct {
	GrantExpiry time.Duration // floor.granted 过期期限（生成 fence 提示）
	ResponseCap int64         // 单条消息 rune 上限（发布门沿用）
}

func defaultChatPolicy() chatGrantPolicy {
	return chatGrantPolicy{GrantExpiry: 30 * time.Second, ResponseCap: 500}
}

// 对话环检测阈值：历史尾部连续 agent 消息 ≥ 该数（无人类介入）即不开新波
// （双 agent 无限互相客气的强制收口；RFC-0012 §2.3）。
const maxAgentMessageTail = 6

// 评估近窗（dogfood 性能治理）：评估只判"回不回/怎么回"，无需生成的全量语境；
// 收小窗口直接降每次评估的输入 token——单座评估延迟的最大构成。
const evalRecentWindow = 4

// Engine 消费消息流并驱动反应波。
type Engine struct {
	cfg EngineConfig
	// lifecycle 是引擎自有的生命周期 ctx（不随分发器 ctx 结束——分发停止不等于波取消；
	// 但 Close() 会取消它，驱动在途任务取消，进程退出不孤儿化 agent 子进程）。
	lifecycle context.Context
	stop      context.CancelFunc
	// roomQueues 同房间串行队列（反应波执行面）：timer 到期入队，worker 逐波执行，
	// 天然单飞。roomLocks 保留为双保险（endorse 直调路径仍串行）。
	roomQueues sync.Map
	roomLocks  sync.Map
	// reactionTimers per-room 去抖计时器（RFC-0012 §2.1：新消息重置并重锚）。
	reactionTimers sync.Map
	// seats 动态座位（二轮审校 #1：运行时启用的适配器要能加入当前引擎）。
	seatsMu sync.RWMutex
	seats   []AgentSeat
}

// roomQueue 单房间串行队列：FIFO channel + 懒启动常驻 worker。
type roomQueue struct {
	ch    chan string
	start sync.Once
}

// NewEngine 构造。ReactionWindow 缺省 3s（RFC-0012 §2.1）。
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ReactionWindow <= 0 {
		cfg.ReactionWindow = 3 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{cfg: cfg, lifecycle: ctx, stop: cancel, seats: cfg.Seats}
}

// SetSeats 运行时更新座位（宿主注册表启用状态变化后由装配方调用；快照语义——
// 只影响之后的波，在途波沿用其开始时的座位集）。
func (e *Engine) SetSeats(seats []AgentSeat) {
	e.seatsMu.Lock()
	defer e.seatsMu.Unlock()
	e.seats = append([]AgentSeat(nil), seats...)
}

func (e *Engine) seatsSnapshot() []AgentSeat {
	e.seatsMu.RLock()
	defer e.seatsMu.RUnlock()
	return append([]AgentSeat(nil), e.seats...)
}

// Close 关停引擎：取消在途波与 agent 任务（适配器经 ctx 击杀子进程组）、
// 停掉全部反应窗口、拒绝新波。已提交事件构成可恢复状态。幂等。
func (e *Engine) Close() {
	e.stop()
	e.reactionTimers.Range(func(_, t any) bool {
		t.(*time.Timer).Stop()
		return true
	})
}

// Deliver 实现 outbox.Consumer：人类 message.posted 调度反应窗口（RFC-0012：
// 群聊制——消息是语境而非开会指令）；room.started（resume）重驱动该房间未开波
// 的刺激声明；intent.endorsed 触发人类保送执行面。引擎自产事件不再反馈。
// durable handoff（二轮审校 #9）：配置 ClaimStore 时先落声明行再返回——声明在
// 反应波开波时统一清除（群聊语义：波锚点已覆盖更早刺激——消息是语境不是席位）。
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
		if err := e.redriveRoomClaims(env.RoomID); err != nil {
			return fmt.Errorf("engine: redrive room %s claims: %w", env.RoomID, err)
		}
		return nil
	case env.Type == protocol.EventIntentEndorsed:
		e.runEndorse(ctx, env)
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
	e.scheduleReaction(env.RoomID)
	e.debug(env.RoomID, "反应窗口已调度", "stimulus", env.EventID)
	return nil
}

// scheduleReaction 去抖反应窗口（RFC-0012 §2.1）：窗口内新消息重置计时并重锚
// （以最新语境开波）；到期把房间号入队，由 worker 串行执行反应波。
func (e *Engine) scheduleReaction(roomID string) {
	if e.lifecycle.Err() != nil {
		return
	}
	if t, ok := e.reactionTimers.Load(roomID); ok {
		t.(*time.Timer).Stop()
	}
	t := time.AfterFunc(e.cfg.ReactionWindow, func() {
		e.reactionTimers.Delete(roomID)
		e.enqueue(roomID)
	})
	e.reactionTimers.Store(roomID, t)
}

// enqueue 入队并确保该房间 worker 存活（反应波串行执行面）。
func (e *Engine) enqueue(roomID string) {
	qAny, _ := e.roomQueues.LoadOrStore(roomID, &roomQueue{ch: make(chan string, 256)})
	q := qAny.(*roomQueue)
	q.start.Do(func() { go e.roomWorker(q) })
	select {
	case q.ch <- roomID:
	case <-e.lifecycle.Done():
	}
}

// roomWorker 单房间常驻消费者：逐波执行（天然串行）。
func (e *Engine) roomWorker(q *roomQueue) {
	for {
		select {
		case <-e.lifecycle.Done():
			return
		case roomID := <-q.ch:
			e.runReaction(e.lifecycle, roomID)
		}
	}
}

// redriveRoomClaims resume 后重驱动该房间未开波的刺激声明（复审 #10）——
// 群聊语义：调度反应窗口即可（波锚定最新语境，声明在开波时清除）。
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
		if c.RoomID == roomID {
			e.warn(roomID, "resume 重驱动未开波的刺激声明", "stimulus", c.StimulusEventID)
			e.scheduleReaction(roomID)
			return nil
		}
	}
	return nil
}

// RecoverClaims 启动恢复：扫描声明未清的刺激——重调度反应窗口。
func (e *Engine) RecoverClaims() {
	if e.cfg.Claims == nil {
		return
	}
	claims, err := e.cfg.Claims.PendingClaims(e.lifecycle)
	if err != nil {
		e.warn("", "claim 恢复扫描失败", "err", err)
		return
	}
	rooms := map[string]bool{}
	for _, c := range claims {
		var env protocol.Envelope
		if json.Unmarshal(c.Envelope, &env) != nil {
			_ = e.cfg.Claims.DeleteClaim(e.lifecycle, c.RoomID, c.StimulusEventID) // 信封损坏：清除毒声明
			continue
		}
		rooms[c.RoomID] = true
	}
	for roomID := range rooms {
		e.warn(roomID, "恢复未开波的刺激声明，重调度反应窗口")
		e.scheduleReaction(roomID)
	}
}

// roundOpenedForStimulus 历史中是否存在该刺激的 round.opened（幂等护栏）。
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

// findEvent 历史内事件定位（endorse 锚点回溯等）。
func findEvent(history []StoredEvent, eventID string) *protocol.Envelope {
	for i := range history {
		if history[i].Envelope.EventID == eventID {
			return &history[i].Envelope
		}
	}
	return nil
}

// lockRoom 取（或建）房间互斥锁。
func (e *Engine) lockRoom(roomID string) *sync.Mutex {
	mu, _ := e.roomLocks.LoadOrStore(roomID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (e *Engine) warn(roomID, msg string, args ...any) {
	e.cfg.Logger.Warn("engine: "+msg, append([]any{"room", roomID}, args...)...)
}

// debug 调试级日志（开发者模式 M1 v1.8）：波链路各环节的 ids 全部落日志——
// 复盘时以任一 id（stimulus/round/grant/task）grep 即可还原完整链路。
func (e *Engine) debug(roomID, msg string, args ...any) {
	e.cfg.Logger.Debug("engine: "+msg, append([]any{"room", roomID}, args...)...)
}

// waveSkip 门控跳过通知（nil 安全；debug 日志之外的房间内可观测面——
// 跳过不落任何事件，没有这条路开发者模式只能看到死寂）。
func (e *Engine) waveSkip(roomID, reason string) {
	if e.cfg.OnWaveSkip != nil {
		e.cfg.OnWaveSkip(roomID, reason)
	}
}

// Seats 当前座位快照（开发者模式状态端点用；与波执行同一份数据）。
func (e *Engine) Seats() []AgentSeat { return e.seatsSnapshot() }

// roomSeats 房间有效座位：全局座位 ∩ 房间 roster（roster 为空 = 全部在席）。
func (e *Engine) roomSeats(history []StoredEvent) []AgentSeat {
	all := e.seatsSnapshot()
	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	roster := RosterOf(envs)
	if roster == nil {
		return all
	}
	out := make([]AgentSeat, 0, len(all))
	for _, s := range all {
		if roster[s.ParticipantID] {
			out = append(out, s)
		}
	}
	return out
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

// pausedAfter fence（二轮审校 #7；复审 #13）：锚点 seq 之后出现过 room.paused
// （即便其后已 resume）即视为失效——"最终是否暂停"检查挡不住 pause→resume 快速往返。
func pausedAfter(events []StoredEvent, anchorSeq int64) bool {
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventRoomPaused && ev.Envelope.Seq > anchorSeq {
			return true
		}
	}
	return false
}

// waveTiming 波链路分段耗时（性能定位套件 v1，dogfood 反馈"非常慢"）：事件正文
// 之外的时间维度落 round.closed.metadata.timing——开发者面板波链路视图与波结束
// 日志双面可查；事件溯源的可审计性不受影响（timing 是度量不是语义）。
type waveTiming struct {
	TotalMs     int64            `json:"total_ms"`      // 波全程（入 runReaction → 收波）
	HistoryMs   int64            `json:"history_ms"`    // 历史拉取
	AssembleMs  int64            `json:"assemble_ms"`   // 上下文组装
	EvalMs      map[string]int64 `json:"eval_ms"`       // 逐座意图评估（观察→判断；座位间并行）
	EvalTotalMs int64            `json:"eval_total_ms"` // 评估相墙钟（并行后 ≈ 最慢一座；历史版本为串行求和）
	GenerateMs  map[string]int64 `json:"generate_ms"`   // 逐发言人生成
}

func newWaveTiming() *waveTiming {
	return &waveTiming{EvalMs: map[string]int64{}, GenerateMs: map[string]int64{}}
}

func msSince(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

// runReaction 一个反应波（RFC-0012 §2.2）：门控 → 开波 → 全员评估（观察→判断）
// → 意愿放行 → sequential 发布 → 收波。锚点 = 最新 message.posted（事件溯源，
// timer 入队时的快照不作数）。
func (e *Engine) runReaction(ctx context.Context, roomID string) {
	if ctx.Err() != nil { // 已 Close：不再开波
		return
	}
	roundID := e.cfg.NewID("rnd")
	timing := newWaveTiming()
	tWave := time.Now()
	// 注意：timing 指针会进 round.closed 事件 metadata（出站盒/SSE 在其它 goroutine
	// marshal 该事件）——收波后不得再写 timing（曾以 defer 补写 TotalMs，race 检测
	// 抓回：落库后的写与异步 marshal 竞态）。中止路径无收波事件，不补账。

	mu := e.lockRoom(roomID)
	mu.Lock()
	defer mu.Unlock()

	tStage := time.Now()
	history, err := e.roomHistory(ctx, roomID)
	timing.HistoryMs = msSince(tStage)
	if err != nil {
		e.warn(roomID, "history 读取失败，波中止", "err", err)
		return
	}

	// 锚点：最新消息（人类或 agent——群聊里任何消息都是观察事件）
	anchor := lastMessage(history)
	if anchor == nil {
		return
	}
	// 幂等护栏：该锚已开过波（timer 竞态/重投）——不双开
	if roundOpenedForStimulus(history, anchor.EventID) {
		return
	}
	// 门控：暂停（人类随时掐断；resume 后经声明重驱动或新消息复活）
	if roomPaused(history) {
		e.debug(roomID, "波跳过：房间暂停", "anchor", anchor.EventID)
		e.waveSkip(roomID, "paused")
		return
	}
	// 门控：对话环检测——尾部连续 agent 消息无人类介入 ≥ 阈值 → 强制收口
	if tail := agentMessageTail(history); tail >= maxAgentMessageTail {
		e.debug(roomID, "波跳过：对话环收口", "agent_tail", tail)
		e.waveSkip(roomID, "ring")
		return
	}
	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	// 门控：线程状态（RFC-0004）——锚点落暂停/关闭/已合并线程不开波
	if anchor.ThreadID != nil && *anchor.ThreadID != "" {
		switch ThreadStateOf(envs, *anchor.ThreadID) {
		case ThreadPaused, ThreadClosed, ThreadMerged:
			e.debug(roomID, "波跳过：线程不在活跃态", "thread", *anchor.ThreadID)
			e.waveSkip(roomID, "thread_inactive")
			return
		}
	}
	// 门控：预算 admission（100% 硬停——群聊的最终硬顶）
	ledger := contextx.RebuildBudget(envs)
	if !ledger.Admit(e.cfg.Budget) {
		e.debug(roomID, "波跳过：预算熔断", "anchor", anchor.EventID,
			"rounds", ledger.Rounds, "utterances", ledger.Utterances, "tokens", ledger.Tokens)
		e.waveSkip(roomID, "budget")
		return
	}
	// 冷却：上波发言者跳过评估（防自言自语）；@点名豁免（RFC-0012 §2.3）。
	// 仅对 agent 锚点生效——人类消息不受冷却约束：冷却集取自最近 round.opened，
	// 被跳过的波不落 round.opened，若人类锚点也吃冷却，全员发言一波后冷却集
	// 永不更新，房间对人类后续输入永久静默（dogfood 实测死锁"输入无回复"）。
	cooldown := map[string]bool{}
	if anchor.Actor.Kind != "human" {
		cooldown = lastWaveSpeakers(history)
	}
	addressed := addressedSet(*anchor)
	roomSeats := e.roomSeats(history)
	seats := make([]AgentSeat, 0, len(roomSeats))
	for _, s := range roomSeats {
		if cooldown[s.ParticipantID] && !addressed[s.ParticipantID] {
			e.debug(roomID, "座冷却跳过", "seat", s.ParticipantID)
			continue
		}
		seats = append(seats, s)
	}
	if len(seats) == 0 {
		e.debug(roomID, "波跳过：全员冷却（静默）", "anchor", anchor.EventID)
		e.waveSkip(roomID, "cooldown")
		return
	}

	policy := defaultChatPolicy()
	epoch := countRounds(history) + 1
	e.debug(roomID, "波开始", "round", roundID, "anchor", anchor.EventID, "epoch", epoch,
		"seats", len(seats), "cooldown", len(cooldown))

	// 1) round.opened（内部记账：快照 Timeline 不收录；策略面已退役）
	opened := e.newEnv(roomID, protocol.EventRoundOpened,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, anchor.EventID, roundID,
		protocol.RoundOpenedPayload{RoundID: roundID, StimulusEventID: anchor.EventID})
	if _, err := e.append(ctx, opened); err != nil {
		e.warn(roomID, "round.opened 落库失败，波中止", "err", err)
		return
	}
	// 声明清账（群聊语义：波锚定最新语境，覆盖更早刺激——消息是语境不是席位）
	e.clearRoomClaims(ctx, roomID)

	// 2) 上下文组装（七层最小 + Receipt）。评估与生成各组一份：评估收小近窗
	// （evalRecentWindow，降输入 token），生成仍用全量近窗；逐组装各落一张
	// Receipt（溯源按实——评估 Receipt 以 ":eval" 后缀区分任务号）。
	tStage = time.Now()
	seatsMin := make([]contextx.Seat, len(seats))
	for i, s := range seats {
		seatsMin[i] = contextx.Seat{ParticipantID: s.ParticipantID}
	}
	assembleCfg := contextx.Config{
		RoomID: roomID, TaskID: roundID, Mode: "chat", Seats: seatsMin,
		RecentWindow: 10,
		Budget: contextx.BudgetState{
			RemainingTokens: remainingTokens(ledger, e.cfg.Budget),
			Level:           ledger.Level(e.cfg.Budget),
		},
	}
	assembled := contextx.Assemble(assembleCfg, envs, *anchor)
	assembleCfg.TaskID = roundID + ":eval"
	assembleCfg.RecentWindow = evalRecentWindow
	evalAssembled := contextx.Assemble(assembleCfg, envs, *anchor)
	timing.AssembleMs = msSince(tStage)
	if e.cfg.Receipts != nil {
		for _, asm := range []*contextx.Assembled{&assembled, &evalAssembled} {
			asm.Receipt.CreatedAt = e.cfg.Clock()
			if err := e.cfg.Receipts.InsertReceipt(ctx, asm.Receipt); err != nil {
				e.warn(roomID, "context receipt 落库失败", "err", err)
			}
		}
	}
	taskContext := agent.Context{
		Inline:     assembled.Inline,
		ReceiptRef: assembled.Receipt.ReceiptID,
	}
	evalContext := agent.Context{
		Inline:     evalAssembled.Inline,
		ReceiptRef: evalAssembled.Receipt.ReceiptID,
	}

	// 3-4) 全员评估（观察→判断，瘦身上下文）→ intent.recorded 全记录（R-01）→ 意愿清单
	willing, silentCount, ok := e.evaluateWave(ctx, roomID, roundID, *anchor, history, seats, evalContext, ledger, timing)
	if !ok {
		return
	}

	// 5) 意愿放行 + sequential 发布（记分卡分排序，@点名前置；CAS 迟到围栏）
	published := 0
	for _, w := range willing {
		outcome := e.revealCandidate(ctx, roomID, roundID, *anchor, w.selection(), epoch, w.intentEventID, taskContext, policy, timing)
		switch outcome {
		case revealPublished:
			published++
			// 每条发布即新消息 → 开新反应窗口（群聊链式语义）
			e.scheduleReaction(roomID)
		case revealRevoked:
			// 迟到/失败：单座撤销（AR-008），波继续
		case revealAbort:
			return // 存储失败中止（已提交事件构成可恢复状态）
		}
	}

	// 6) round.closed（全员 silent → quiescent = 意愿静默终止，不再开窗）
	outcome := "quiescent"
	if published > 0 {
		outcome = "published"
	}
	timing.TotalMs = msSince(tWave)
	closed := e.newEnv(roomID, protocol.EventRoundClosed,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, anchor.EventID, roundID,
		protocol.RoundClosedPayload{
			RoundID:       roundID,
			Outcome:       outcome,
			SelectedCount: published,
			SilentCount:   silentCount,
		})
	closed.Metadata = map[string]any{"timing": timing}
	_, _ = e.append(ctx, closed)
	e.debug(roomID, "波结束", "round", roundID, "outcome", outcome,
		"published", published, "silent", silentCount,
		"total_ms", timing.TotalMs, "eval_total_ms", timing.EvalTotalMs)
}

// clearRoomClaims 波开后的声明清账：本房间全部 pending 声明清除——锚点已覆盖
// 更早刺激（群聊语义）。失败不致命（恢复扫描按已开波清理兜底）。
func (e *Engine) clearRoomClaims(ctx context.Context, roomID string) {
	if e.cfg.Claims == nil {
		return
	}
	claims, err := e.cfg.Claims.PendingClaims(ctx)
	if err != nil {
		return
	}
	for _, c := range claims {
		if c.RoomID == roomID {
			_ = e.cfg.Claims.DeleteClaim(ctx, roomID, c.StimulusEventID)
		}
	}
}

// willingIntent 意愿清单项（评估产物 → 发布顺序）。
type willingIntent struct {
	intent        attention.Intent
	score         float64
	band          string
	intentEventID string
	addressed     bool
	rank          int // 发布顺序（@点名前置，其余记分卡分降序；平局按座位字典序）
}

func (w willingIntent) selection() attention.Selection {
	return attention.Selection{
		IntentID: w.intent.IntentID, ParticipantID: w.intent.ParticipantID,
		Rank: w.rank, Band: w.band,
	}
}

// evaluateWave 全员意图评估（观察→判断）→ intent.recorded 全记录（R-01）→
// 意愿清单（action∈{speak,react} 且预算允许 = 放行；silent = agent 自决不回）。
// 排序：@点名前置，其余按记分卡分降序（band 透明保留，反 Goodhart：不外发精确分）。
func (e *Engine) evaluateWave(ctx context.Context, roomID, roundID string, anchor protocol.Envelope,
	history []StoredEvent, seats []AgentSeat, taskContext agent.Context, baseLedger contextx.Ledger,
	timing *waveTiming,
) (willing []willingIntent, silentCount int, ok bool) {
	policy := defaultChatPolicy()
	weights := attention.DefaultWeights
	evalUsage := map[string]*agent.Usage{}
	anchorThread := ""
	if anchor.ThreadID != nil {
		anchorThread = *anchor.ThreadID
	}
	// 相 1：全部评估并行跑完。座位间无依赖（不同 Profile/Session，thread 连续性
	// 互不干扰），本相纯适配器调用、零存储写——事件落账与 usage 汇总留在相 1.5/2
	// 串行做，四轮复审 #10 的 admission 语义不变。串行求和曾是 dogfood "非常慢"
	// 的最大头（3 座 ≈ 3×17s），并行后评估相墙钟 ≈ 最慢一座。
	type seatEval struct {
		seat   AgentSeat
		result agent.Result
		ms     int64
		ok     bool
	}
	evals := make([]seatEval, len(seats)) // 定址写：保座位序，相 2 记录顺序与串行版一致
	tPhase := time.Now()
	var wg sync.WaitGroup
	for i, seat := range seats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 阶段信号（dogfood 静默期反馈）：评估启动即上"正在评估"——
			// intent.recorded 要等 gather 才批量落地，空窗期 UI 不该死寂。
			if e.cfg.OnDraft != nil {
				e.cfg.OnDraft(roomID, seat.ParticipantID, agent.DraftUpdate{Kind: "stage", Stage: "evaluating"})
			}
			tEval := time.Now()
			intentResult, err := e.runTask(ctx, seat.Profile, seat.ParticipantID, agent.Task{
				TaskID:        e.cfg.NewID("tsk"),
				Kind:          agent.KindEvaluateIntent,
				ParticipantID: seat.ParticipantID,
				RoomID:        roomID,
				ThreadID:      anchorThread,
				Epoch:         roundID,
				Context:       taskContext,
			})
			if err != nil {
				if ctx.Err() == nil { // 取消路径静默——gather 后统一判定整波中止
					e.warn(roomID, "意图评估失败，跳过该座", "seat", seat.ParticipantID, "err", err)
				}
				return
			}
			evals[i] = seatEval{seat: seat, result: intentResult, ms: msSince(tEval), ok: true}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, 0, false // Close/取消：整波中止
	}
	timing.EvalTotalMs = msSince(tPhase) // 评估相墙钟（并行后 = max 而非 Σ）
	kept := evals[:0]
	for _, ev := range evals {
		if !ev.ok {
			continue
		}
		evalUsage[ev.seat.ParticipantID] = ev.result.Usage
		timing.EvalMs[ev.seat.ParticipantID] = ev.ms
		kept = append(kept, ev)
	}
	evals = kept
	// 相 1.5：评估消耗入"现在"的账本。
	ledgerNow := baseLedger
	for _, u := range evalUsage {
		if u != nil {
			ledgerNow.Tokens += u.InputTokens + u.OutputTokens
		}
	}
	// 相 2：结构校验 + 全记录 + 意愿构建。
	for _, ev := range evals {
		intent, valid := intentFromData(ev.seat.ParticipantID, ev.result.Data)
		if !valid {
			// 结构校验失败即弃权（二轮审校 #8）：全记录（R-01）+ usage 入账（复审 #12）
			e.warn(roomID, "intent 结构非法，该座弃权", "seat", ev.seat.ParticipantID)
			invalid := e.newEnv(roomID, protocol.EventIntentRecorded,
				protocol.Actor{ParticipantID: ev.seat.ParticipantID, Kind: "agent"}, anchor.EventID, roundID,
				protocol.IntentRecordedPayload{
					IntentID:        e.cfg.NewID("int"),
					ParticipantID:   ev.seat.ParticipantID,
					Action:          "silent", // 弃权语义（schema 合法枚举），band=unranked
					ScoreBand:       "unranked",
					Selected:        false,
					PublicRationale: "intent 结构非法，弃权",
				})
			invalid.Metadata = intentMetadata(
				attention.Rejection{Reason: "invalid_intent_structure"}, false, ev.result.Usage)
			if _, err := e.append(ctx, invalid); err != nil {
				return nil, 0, false
			}
			silentCount++
			continue
		}
		intent.IntentID = e.cfg.NewID("int")
		// 自决判定：silent = 不回（记录在案，记分卡可查）；speak/react = 意愿
		selected := intent.Action == "speak" || intent.Action == "react"
		band, score, reason := "unranked", 0.0, ""
		if selected {
			cand := attention.Candidate{
				Intent: intent,
				Ctx: attention.ContextFeatures{
					ViewpointDiversity: 0.5, // M1 中性；结构投影 M3 接入（RFC-0006 降级路径）
					RecentFloorShare:   recentFloorShare(history, ev.seat.ParticipantID),
					DirectAddress:      directAddress(anchor, ev.seat.ParticipantID),
				},
			}
			score = attention.Score(cand, weights)
			band = attention.Band(score)
			if !ledgerNow.ReserveOK(e.cfg.Budget, 1, policy.ResponseCap) {
				// 硬失格（预算）不进记分：band 回落 unranked（记分卡透明——理由入 metadata）
				selected, band, score, reason = false, "unranked", 0.0, "budget"
			}
		} else {
			silentCount++
		}
		recorded := e.newEnv(roomID, protocol.EventIntentRecorded,
			protocol.Actor{ParticipantID: ev.seat.ParticipantID, Kind: "agent"}, anchor.EventID, roundID,
			protocol.IntentRecordedPayload{
				IntentID:         intent.IntentID,
				ParticipantID:    intent.ParticipantID,
				Action:           intent.Action,
				Type:             intent.Type,
				PublicRationale:  truncate(intent.PublicRationale, 280),
				ScoreBand:        band,
				Selected:         selected,
				Endorsed:         false,
				UnselectedReason: reason,
			})
		recorded.Metadata = intentMetadata(attention.Rejection{Reason: reason}, selected, evalUsage[ev.seat.ParticipantID])
		appendedIntent, err := e.append(ctx, recorded)
		if err != nil {
			return nil, 0, false
		}
		if selected {
			willing = append(willing, willingIntent{
				intent:        intent,
				score:         score,
				band:          band,
				intentEventID: appendedIntent[0].EventID,
				addressed:     directAddress(anchor, ev.seat.ParticipantID) > 0,
			})
		}
	}
	// 相 3：发布顺序——@点名前置，其余记分卡分降序；平局按参与者字典序（确定性）。
	sort.SliceStable(willing, func(i, j int) bool {
		if willing[i].addressed != willing[j].addressed {
			return willing[i].addressed
		}
		if willing[i].score != willing[j].score {
			return willing[i].score > willing[j].score
		}
		return willing[i].intent.ParticipantID < willing[j].intent.ParticipantID
	})
	for i := range willing {
		willing[i].rank = i + 1
	}
	e.debug(roomID, "评估完成", "round", roundID,
		"evaluated", len(evals), "willing", len(willing), "silent", silentCount)
	return willing, silentCount, true
}

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

type revealOutcome int

const (
	revealAbort revealOutcome = iota // 存储失败：中止本波（已提交事件构成可恢复状态）
	revealPublished
	revealRevoked
)

// lastMessage 历史中最新 message.posted（反应波锚点——人类或 agent）。
func lastMessage(events []StoredEvent) *protocol.Envelope {
	for i := len(events) - 1; i >= 0; i-- {
		env := events[i].Envelope
		if env.Type == protocol.EventMessagePosted {
			return &env
		}
	}
	return nil
}

// lastWaveSpeakers 上一波发言者（冷却集）：最近一个 round.opened 之后发言的
// agent 集合。无轮历史 → 空集（首波无人冷却）。
func lastWaveSpeakers(events []StoredEvent) map[string]bool {
	lastOpened := int64(0)
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventRoundOpened {
			lastOpened = ev.Envelope.Seq
		}
	}
	speakers := map[string]bool{}
	if lastOpened == 0 {
		return speakers
	}
	for _, ev := range events {
		if ev.Envelope.Seq > lastOpened &&
			ev.Envelope.Type == protocol.EventMessagePosted && ev.Envelope.Actor.Kind == "agent" {
			speakers[ev.Envelope.Actor.ParticipantID] = true
		}
	}
	return speakers
}

// agentMessageTail 尾部连续 agent 消息数（对话环检测；遇人类消息即止）。
func agentMessageTail(events []StoredEvent) int {
	n := 0
	for i := len(events) - 1; i >= 0; i-- {
		env := events[i].Envelope
		if env.Type != protocol.EventMessagePosted {
			continue
		}
		if env.Actor.Kind == "agent" {
			n++
			continue
		}
		break
	}
	return n
}

// addressedSet 锚点消息的 @点名集合（addressed_to 载荷）。
func addressedSet(anchor protocol.Envelope) map[string]bool {
	out := map[string]bool{}
	var payload struct {
		AddressedTo []string `json:"addressed_to"`
	}
	if json.Unmarshal(anchor.Payload, &payload) != nil {
		return out
	}
	for _, p := range payload.AddressedTo {
		out[p] = true
	}
	return out
}

// revealCandidate sequential 揭示链：发授（水位取当下）→ 生成 → 发布。
func (e *Engine) revealCandidate(ctx context.Context, roomID, roundID string, stimulus protocol.Envelope,
	sel attention.Selection, epoch int64, intentEventID string, taskContext agent.Context,
	policy chatGrantPolicy, timing *waveTiming) revealOutcome {

	version, err := e.cfg.Store.RoomVersion(ctx, roomID)
	if err != nil {
		return revealAbort
	}
	grantEnv, grantID, ok := e.issueGrant(ctx, roomID, roundID, sel, epoch, intentEventID, policy, version)
	if !ok {
		return revealAbort
	}
	tGen := time.Now()
	draft, genOK := e.runGenerate(ctx, roomID, roundID, stimulus, sel, grantEnv, grantID, taskContext, policy)
	timing.GenerateMs[sel.ParticipantID] = msSince(tGen)
	if !genOK {
		return revealRevoked
	}
	return e.publishMessage(ctx, roomID, roundID, stimulus, sel, grantEnv, grantID, draft, epoch)
}

// issueGrant 发授：floor.granted，causation=该意愿 intent.recorded；水位取当下。
func (e *Engine) issueGrant(ctx context.Context, roomID, roundID string, sel attention.Selection,
	epoch int64, intentEventID string, policy chatGrantPolicy, watermark int64,
) (protocol.Envelope, string, bool) {

	grantID := e.cfg.NewID("grant")
	grant := e.newEnv(roomID, protocol.EventFloorGranted,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, intentEventID, roundID,
		protocol.FloorGrantedPayload{
			GrantID:          grantID,
			RoundID:          roundID,
			ParticipantID:    sel.ParticipantID,
			Rank:             sel.Rank,
			ContextWatermark: int(watermark),
			Epoch:            int(epoch),
			ExpiresAt:        e.cfg.Now().Add(policy.GrantExpiry).UTC().Format(time.RFC3339Nano),
			ResponseCap:      int(policy.ResponseCap),
		})
	appended, err := e.append(ctx, grant)
	if err != nil {
		return protocol.Envelope{}, "", false
	}
	e.debug(roomID, "floor 已授予", "round", roundID, "grant", grantID,
		"participant", sel.ParticipantID, "epoch", epoch)
	return grant, appended[0].EventID, true
}

// runGenerate 生成：DraftUpdate 流经 OnDraft 透传；失败（非引擎关停）按
// generation_failed 撤销该授并返回 false（其余继续 AR-008）。
func (e *Engine) runGenerate(ctx context.Context, roomID, roundID string, stimulus protocol.Envelope,
	sel attention.Selection, grantEnv protocol.Envelope, grantID string,
	taskContext agent.Context, policy chatGrantPolicy) (agent.Result, bool) {

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
			GrantID:     grantID,
			Rank:        sel.Rank,
			ViewCursor:  "",
			Epoch:       0,
			ResponseCap: policy.ResponseCap, // 宣告值必须传入适配器并约束发布
		},
		Context: taskContext,
	})
	if err != nil {
		if ctx.Err() != nil {
			return agent.Result{}, false
		}
		e.warn(roomID, "生成失败，撤销该授", "round", roundID, "grant", grantID, "err", err)
		e.revoke(ctx, roomID, grantEnv.EventID, grantID, roundID, stimulus, "generation_failed", nil)
		return agent.Result{}, false
	}
	return draftResult, true
}

// publishMessage 正文发布：暂停/迟到围栏 + CAS 良性重试（四轮复审 #12）。
func (e *Engine) publishMessage(ctx context.Context, roomID, roundID string, stimulus protocol.Envelope,
	sel attention.Selection, grantEnv protocol.Envelope, grantID string, draft agent.Result, epoch int64) revealOutcome {

	fresh, err := e.roomHistory(ctx, roomID)
	if err != nil {
		return revealAbort
	}
	if e.fenceViolated(fresh, roundID, roundOpenedSeq(fresh, roundID), epoch) {
		e.revoke(ctx, roomID, grantEnv.EventID, grantID, roundID, stimulus, "room_paused", draft.Usage)
		return revealRevoked
	}
	msg := e.newEnv(roomID, protocol.EventMessagePosted,
		protocol.Actor{ParticipantID: sel.ParticipantID, Kind: "agent"}, grantEnv.EventID, roundID, draft.Data)
	msg.ThreadID = stimulus.ThreadID // 线程归属透传（agent-native：回复不丢线程）
	if draft.Usage != nil {
		msg.Metadata = map[string]any{
			"usage": map[string]any{
				"input_tokens":  draft.Usage.InputTokens,
				"output_tokens": draft.Usage.OutputTokens,
			},
		}
	}
	// 正文落库走 CAS：良性交错换期位重试，真迟到撤销。
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
		if e.fenceViolated(recheck, roomID, roundOpenedSeq(recheck, roundID), epoch) {
			e.revoke(ctx, roomID, grantEnv.EventID, grantID, roundID, stimulus, "room_paused", draft.Usage)
			return revealRevoked
		}
		expected = expectedVersionOf(recheck) // 良性交错：换新期位重试
	}
	e.warn(roomID, "正文 CAS 重试耗尽，波中止", "round", roundID, "grant", grantID)
	return revealAbort
}

// fenceViolated 迟到判定（fresh 重读后复用）：当前暂停 / 本波开启后出现过暂停 /
// 进入更新 epoch。
func (e *Engine) fenceViolated(events []StoredEvent, roundID string, openedSeq, epoch int64) bool {
	return roomPaused(events) || (openedSeq > 0 && pausedAfter(events, openedSeq)) || countRounds(events) > epoch
}

// expectedVersionOf CAS 期位：fresh 历史的最新 seq。
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

// runTask 适配器任务执行（评估/生成共用；Supervisor 会话与进程组管理）。
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
// 二轮审校 #8：未知枚举/非数值分数一律拒收）。
var intentActionSet = map[string]bool{
	"speak": true, "react": true, "fork": true, "summarize": true, "silent": true,
}

var intentTypeSet = map[string]bool{
	"answer": true, "extend": true, "challenge": true, "support": true,
	"question": true, "redirect": true, "synthesize": true,
}

// intentFromData 适配器 turn_intent 结果 → 域 Intent（严格校验：枚举合法、分数必为
// 数值且字段齐全；silent 允许省略 type/scores。校验失败返回 false——该座弃权）。
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
		return attention.Intent{}, false // 缺字段/非数值不得默认 0
	}
	intent.Scores = attention.Scores{
		Relevance:  relevance,
		Novelty:    novelty,
		Urgency:    urgency,
		Confidence: confidence,
	}
	return intent, true
}

// recentFloorShare 该参与者最近发言占比（M1：全历史 agent 消息窗口）。
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

// directAddress 锚点是否 @点名该参与者（冷却豁免与顺序前置的特征输入）。
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
