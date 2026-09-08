// Package room 内文件：任务执行通道（M4-1 切片 A，RFC-0002 执行生命周期补编）。
//
// run 表示独立于群聊波的任务执行：人类经 run_task 命令发起（run.requested），
// 引擎托管专用 exec 进程（run.started → completed/failed/canceled）；
// 重启后 running 态无活进程 → run.unknown（结果未知，不自动重跑——可能已
// 产生副作用）；取消后迟到结果以 run.completed{late:true} 审计、不发布正文。
// tasklist 仍表示带责任人的承诺（v1.50 requester/owner 规则不变），run 通过
// task_id 可选关联——执行完成不自动结案（人工门控权威）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// RunView 快照 runs 投影（事件折叠；终态吸收后续迟到审计事件不改状态）。
type RunView struct {
	RunID         string `json:"run_id"`
	TaskID        string `json:"task_id,omitempty"`
	Assignee      string `json:"assignee"`
	Instruction   string `json:"instruction"`
	Requester     string `json:"requester"`
	Status        string `json:"status"` // requested | running | completed | failed | canceled | unknown
	Error         string `json:"error,omitempty"`
	ResultEventID string `json:"result_event_id,omitempty"`
	Late          bool   `json:"late,omitempty"`
	// ResultBody 迟到审计保留的执行结果正文（late 时唯一留存——未作为消息
	// 发布，投影带出供人类查看/复制救济；正常完成的正文在结果消息里，不重复）。
	ResultBody  string `json:"result_body,omitempty"`
	RequestedAt string `json:"requested_at"`
	UpdatedAt   string `json:"updated_at"`
}

// RunsOf 事件折叠投影：按 run_id 聚合，序内后到事件更新状态；迟到审计
// （completed late）不改终态。
func RunsOf(events []StoredEvent) []RunView {
	order := []string{}
	byID := map[string]*RunView{}
	for _, ev := range events {
		var runID string
		switch ev.Envelope.Type {
		case protocol.EventRunRequested:
			var p protocol.RunRequestedPayload
			if decode(ev, &p) != nil || p.RunID == "" {
				continue
			}
			runID = p.RunID
			if _, ok := byID[runID]; !ok {
				byID[runID] = &RunView{RunID: runID, Status: "requested", RequestedAt: ev.Envelope.OccurredAt}
				order = append(order, runID)
			}
			v := byID[runID]
			v.Assignee, v.Instruction, v.TaskID, v.Requester = p.Assignee, p.Instruction, p.TaskID, p.Requester
		case protocol.EventRunStarted:
			var p protocol.RunStartedPayload
			if decode(ev, &p) != nil {
				continue
			}
			runID = p.RunID
			mutate(byID, runID, func(v *RunView) { v.Status = "running" })
		case protocol.EventRunCompleted:
			var p protocol.RunCompletedPayload
			if decode(ev, &p) != nil {
				continue
			}
			runID = p.RunID
			// 迟到审计（late）只在非终态外的任意态记录，不翻转已完成终态
			if p.Late {
				mutate(byID, runID, func(v *RunView) { v.Late = true; v.ResultBody = p.Body })
				continue
			}
			mutate(byID, runID, func(v *RunView) { v.Status = "completed"; v.ResultEventID = p.ResultEventID })
		case protocol.EventRunFailed:
			var p protocol.RunFailedPayload
			if decode(ev, &p) != nil {
				continue
			}
			runID = p.RunID
			mutate(byID, runID, func(v *RunView) { v.Status = "failed"; v.Error = p.Error })
		case protocol.EventRunCanceled:
			var p protocol.RunCanceledPayload
			if decode(ev, &p) != nil {
				continue
			}
			runID = p.RunID
			mutate(byID, runID, func(v *RunView) { v.Status = "canceled"; v.Error = p.Reason })
		case protocol.EventRunUnknown:
			var p protocol.RunUnknownPayload
			if decode(ev, &p) != nil {
				continue
			}
			runID = p.RunID
			mutate(byID, runID, func(v *RunView) { v.Status = "unknown"; v.Error = p.Note })
		}
		if runID != "" {
			if v, ok := byID[runID]; ok {
				v.UpdatedAt = ev.Envelope.OccurredAt
			}
		}
	}
	out := make([]RunView, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

func decode(ev StoredEvent, v any) error { return jsonUnmarshal(ev.Envelope.Payload, v) }
func mutate(byID map[string]*RunView, runID string, f func(*RunView)) {
	if v, ok := byID[runID]; ok {
		f(v)
	}
}

// ---- 命令面 ----

// runTask 人类发起执行（run_task：assignee 指派 agent 座位；task_id 可选关联）。
func (s *Service) runTask(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if res, err := s.roomVersionPrecheck(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		Assignee    string `json:"assignee"`
		Instruction string `json:"instruction"`
		TaskID      string `json:"task_id"`
	}
	if err := jsonUnmarshal(cmd.Payload, &payload); err != nil {
		return nil, fmt.Errorf("%w: run_task payload: %v", ErrInvalidCommand, err)
	}
	if !agentParticipantPattern.MatchString(payload.Assignee) {
		return nil, fmt.Errorf("%w: assignee 形如 par_*（agent 座位）", ErrInvalidCommand)
	}
	if n := len([]rune(payload.Instruction)); n < 1 || n > 4000 {
		return nil, fmt.Errorf("%w: instruction 1..4000 字", ErrInvalidCommand)
	}
	if payload.TaskID != "" && !taskIDPattern.MatchString(payload.TaskID) {
		return nil, fmt.Errorf("%w: task_id 形如 tsk_*", ErrInvalidCommand)
	}
	// 能力门：assignee 的适配器须支持后台执行（echo 等测试桩不支持）
	if s.cfg.RunCapable != nil && !s.cfg.RunCapable(payload.Assignee) {
		return nil, fmt.Errorf("%w: 该 Agent 不支持独立任务执行", ErrInvalidCommand)
	}
	env := protocol.Envelope{
		EventID: s.cfg.NewID("evt"), TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		Type: protocol.EventRunRequested, SchemaVersion: 1, OccurredAt: s.cfg.Clock(),
		Actor:      protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.RunRequestedPayload{
			RunID: s.cfg.NewID("run"), Assignee: payload.Assignee, Instruction: payload.Instruction,
			TaskID: payload.TaskID, Requester: actor.ParticipantID,
		}),
		Metadata: map[string]any{},
	}
	return s.commit(ctx, env, s.receiptFor(cmd, actor, env))
}

// cancelRun 取消执行（终态不可取消；进程取消由引擎按 run_id 处置）。
func (s *Service) cancelRun(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if res, err := s.roomVersionPrecheck(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		RunID  string `json:"run_id"`
		Reason string `json:"reason"`
	}
	if err := jsonUnmarshal(cmd.Payload, &payload); err != nil {
		return nil, fmt.Errorf("%w: cancel_run payload: %v", ErrInvalidCommand, err)
	}
	if !runIDPatternS.MatchString(payload.RunID) {
		return nil, fmt.Errorf("%w: run_id 形如 run_*", ErrInvalidCommand)
	}
	if n := len([]rune(payload.Reason)); n < 1 || n > 280 {
		return nil, fmt.Errorf("%w: reason 1..280 字", ErrInvalidCommand)
	}
	reader := s.cfg.Reader
	if reader == nil {
		reader, _ = s.cfg.Store.(EventReader)
	}
	if reader == nil {
		return nil, fmt.Errorf("%w: 存储无事件读面", ErrInvalidCommand)
	}
	events, _, err := reader.EventsAfter(ctx, cmd.RoomID, "", 100000)
	if err != nil {
		return nil, fmt.Errorf("room: read runs: %w", err)
	}
	state := ""
	for _, v := range RunsOf(events) {
		if v.RunID == payload.RunID {
			state = v.Status
		}
	}
	if state == "" {
		return nil, fmt.Errorf("%w: run 不存在 %s", ErrInvalidCommand, payload.RunID)
	}
	if state != "requested" && state != "running" {
		return nil, fmt.Errorf("%w: 终态 run 不可取消（%s）", ErrInvalidCommand, state)
	}
	env := protocol.Envelope{
		EventID: s.cfg.NewID("evt"), TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		Type: protocol.EventRunCanceled, SchemaVersion: 1, OccurredAt: s.cfg.Clock(),
		Actor:      protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    mustJSON(protocol.RunCanceledPayload{RunID: payload.RunID, Reason: payload.Reason}),
		Metadata:   map[string]any{},
	}
	return s.commit(ctx, env, s.receiptFor(cmd, actor, env))
}

// ---- 引擎侧 RunManager ----

// defaultRunTimeout 独立执行的时长上限（长于单轮 180s——"长任务"的服务面）。
const defaultRunTimeout = 10 * time.Minute

type runState struct {
	cancel context.CancelFunc
	handle agent.Handle
}

type runManager struct {
	mu   sync.Mutex
	runs map[string]*runState
}

// runTimeoutLimit 本次执行的超时上限：活读面（设置族）优先，其次静态配置，
// 最后缺省。每次拉起时解析——设置变更对后续 run 生效。
func (e *Engine) runTimeoutLimit() time.Duration {
	if e.cfg.RunTimeoutFunc != nil {
		if t := e.cfg.RunTimeoutFunc(); t > 0 {
			return t
		}
	}
	if e.cfg.RunTimeout > 0 {
		return e.cfg.RunTimeout
	}
	return defaultRunTimeout
}

// LaunchRun 执行一个已落 run.requested 的运行：started → exec → 结果回传。
// 结果以 assignee 名义发布 message.posted（AppendEventsIf 迟到围栏 + 发布门），
// metadata 携 run_id/task_id（可追溯关联）；迟到（取消后/房间暂停）→ 审计事件。
func (e *Engine) LaunchRun(roomID string, req protocol.RunRequestedPayload) {
	e.runsMu.Lock()
	if e.runsRuns == nil {
		e.runsRuns = map[string]*runState{}
	}
	if _, inflight := e.runsRuns[req.RunID]; inflight {
		e.runsMu.Unlock()
		return // 同一 run 不重复拉起（重启恢复/重复投递防御）
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.runsRuns[req.RunID] = &runState{cancel: cancel}
	e.runsMu.Unlock()

	go e.executeRun(ctx, roomID, req, cancel)
}

// CancelRun 取消在途运行（进程组击杀由适配器 ctx 取消承担）。
func (e *Engine) CancelRun(runID string) {
	e.runsMu.Lock()
	rs := e.runsRuns[runID]
	e.runsMu.Unlock()
	if rs == nil {
		return
	}
	if rs.handle != nil {
		rs.handle.Cancel()
	}
	rs.cancel()
}

// closeRuns 引擎关停：全部在途执行取消（防孤儿进程）。
func (e *Engine) closeRuns() {
	e.runsMu.Lock()
	all := e.runsRuns
	e.runsRuns = nil
	e.runsMu.Unlock()
	for _, rs := range all {
		if rs.handle != nil {
			rs.handle.Cancel()
		}
		rs.cancel()
	}
}

// RecoverRuns 启动恢复：历史投影里 running/requested 态且本进程无活句柄 →
// run.unknown（结果未知——不自动重跑，可能已产生副作用）。
func (e *Engine) RecoverRuns() {
	ctx := context.Background()
	for _, roomID := range e.knownRooms() {
		events, err := e.roomHistory(ctx, roomID)
		if err != nil {
			continue
		}
		for _, v := range RunsOf(events) {
			if v.Status != "running" && v.Status != "requested" {
				continue
			}
			e.runsMu.Lock()
			_, inflight := e.runsRuns[v.RunID]
			e.runsMu.Unlock()
			if inflight {
				continue
			}
			env := protocol.Envelope{
				EventID: e.cfg.NewID("evt"), TenantID: e.cfg.Tenant, RoomID: roomID,
				Type: protocol.EventRunUnknown, SchemaVersion: 1, OccurredAt: e.cfg.Clock(),
				Actor:      protocol.Actor{ParticipantID: "system", Kind: "system"},
				Visibility: protocol.Visibility{Kind: "public"},
				Payload: mustJSON(protocol.RunUnknownPayload{
					RunID: v.RunID, Assignee: v.Assignee,
					Note: "进程重启时仍在执行——结果未知；不会自动重跑（可能已产生副作用），请确认后重发或忽略",
				}),
				Metadata: map[string]any{},
			}
			if _, err := e.cfg.Store.AppendEvents(ctx, []protocol.Envelope{env}); err != nil {
				e.warn(roomID, "run.unknown 落库失败", "run", v.RunID, "err", err)
			}
		}
	}
}

func (e *Engine) executeRun(ctx context.Context, roomID string, req protocol.RunRequestedPayload, cancel context.CancelFunc) {
	defer func() {
		e.runsMu.Lock()
		delete(e.runsRuns, req.RunID)
		e.runsMu.Unlock()
		cancel()
	}()

	// started（执行器回执——状态由回执产生，不由正文推断）
	if !e.appendRunEvent(ctx, roomID, protocol.EventRunStarted,
		mustJSON(protocol.RunStartedPayload{RunID: req.RunID, Assignee: req.Assignee})) {
		e.appendRunTerminal(ctx, roomID, req, protocol.EventRunFailed,
			mustJSON(protocol.RunFailedPayload{RunID: req.RunID, Assignee: req.Assignee, Error: "run.started 落库失败"}))
		return
	}

	timeout := e.runTimeoutLimit()

	runCtx, runCancel := context.WithTimeout(ctx, timeout)
	defer runCancel()

	seat := e.seatOf(req.Assignee)
	if seat == nil {
		e.appendRunTerminal(ctx, roomID, req, protocol.EventRunFailed,
			mustJSON(protocol.RunFailedPayload{RunID: req.RunID, Assignee: req.Assignee, Error: "座位不在席（执行期被移出）"}))
		return
	}
	h, err := e.cfg.Agents.Submit(runCtx, seat.Profile, agent.Task{
		TaskID: "run:" + req.RunID, Kind: agent.KindGenerate,
		ParticipantID: req.Assignee, RoomID: roomID, Deadline: time.Now().Add(timeout),
		Context: agent.Context{Inline: e.runContext(roomID, req)},
	})
	if err == nil {
		e.runsMu.Lock()
		if rs := e.runsRuns[req.RunID]; rs != nil {
			rs.handle = h
		}
		e.runsMu.Unlock()
		var res agent.Result
		res, err = h.Result()
		if err == nil {
			e.completeRun(ctx, roomID, req, res)
			return
		}
	}
	if ctx.Err() != nil || runCtx.Err() == context.DeadlineExceeded && ctx.Err() != nil {
		return // 取消路径：canceled 事件由 cancel_run 命令落；迟到结果走 late 审计
	}
	errStr := "执行失败"
	if err != nil {
		errStr = err.Error()
	}
	if runCtx.Err() == context.DeadlineExceeded {
		errStr = fmt.Sprintf("执行超时（上限 %s）", timeout)
	}
	e.appendRunTerminal(ctx, roomID, req, protocol.EventRunFailed,
		mustJSON(protocol.RunFailedPayload{RunID: req.RunID, Assignee: req.Assignee, Error: truncateStr(errStr, 1000)}))
}

// completeRun 结果回传：正文过发布门 → 以 assignee 名义发布 message.posted
// （AppendEventsIf 迟到围栏 + 良性交错重试 3 次；房间暂停/被取消 → 迟到审计）。
func (e *Engine) completeRun(ctx context.Context, roomID string, req protocol.RunRequestedPayload, res agent.Result) {
	body, _, err := agent.PublishGate(strOf(res.Data["body"]), res.Data["declared_relations"])
	if err != nil || strings.TrimSpace(body) == "" {
		e.appendRunTerminal(ctx, roomID, req, protocol.EventRunFailed,
			mustJSON(protocol.RunFailedPayload{RunID: req.RunID, Assignee: req.Assignee, Error: "执行结果不可发布（空正文/发布门拒绝）: " + truncateStr(fmt.Sprint(err), 400)}))
		return
	}
	// 取消代次检查：canceled 已落则迟到——审计不发布
	events, err := e.roomHistory(ctx, roomID)
	if err == nil {
		for _, v := range RunsOf(events) {
			if v.RunID == req.RunID && (v.Status == "canceled" || v.Status == "unknown") {
				e.appendRunTerminal(ctx, roomID, req, protocol.EventRunCompleted,
					mustJSON(protocol.RunCompletedPayload{RunID: req.RunID, Assignee: req.Assignee, Late: true, Body: truncateStr(body, 8000)}))
				return
			}
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		version, err := e.cfg.Store.RoomVersion(ctx, roomID)
		if err != nil {
			break
		}
		msgEnv := protocol.Envelope{
			EventID: e.cfg.NewID("evt"), TenantID: e.cfg.Tenant, RoomID: roomID,
			Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: e.cfg.Clock(),
			Actor:      protocol.Actor{ParticipantID: req.Assignee, Kind: "agent"},
			Visibility: protocol.Visibility{Kind: "public"},
			Payload:    mustJSON(map[string]any{"body": body, "addressed_to": []string{}, "relations": []any{}}),
			Metadata:   map[string]any{"run_id": req.RunID, "task_id": req.TaskID},
		}
		appended, err := e.appendCASRun(ctx, roomID, msgEnv, version)
		if err == nil && len(appended) > 0 {
			e.appendRunTerminal(ctx, roomID, req, protocol.EventRunCompleted,
				mustJSON(protocol.RunCompletedPayload{RunID: req.RunID, Assignee: req.Assignee, ResultEventID: msgEnv.EventID}))
			return
		}
		if err != nil {
			break
		}
		// CAS 失败：回读判别——暂停=迟到（不发布）；良性交错=换期位重试
		if e.roomPausedSince(ctx, roomID) {
			e.appendRunTerminal(ctx, roomID, req, protocol.EventRunCompleted,
				mustJSON(protocol.RunCompletedPayload{RunID: req.RunID, Assignee: req.Assignee, Late: true, Body: truncateStr(body, 8000)}))
			return
		}
	}
	e.appendRunTerminal(ctx, roomID, req, protocol.EventRunFailed,
		mustJSON(protocol.RunFailedPayload{RunID: req.RunID, Assignee: req.Assignee, Error: "结果发布失败（存储竞态）"}))
}

// runContext 独立执行语境：指令 + 房间近窗 + 任务关联（不锚定波——run 与波解耦）。
func (e *Engine) runContext(roomID string, req protocol.RunRequestedPayload) map[string]any {
	ctx := context.Background()
	history := []map[string]any{}
	if events, err := e.roomHistory(ctx, roomID); err == nil {
		msgs := []protocol.Envelope{}
		for _, ev := range events {
			if ev.Envelope.Type == protocol.EventMessagePosted {
				msgs = append(msgs, ev.Envelope)
			}
		}
		start := len(msgs) - 10
		if start < 0 {
			start = 0
		}
		for _, m := range msgs[start:] {
			var p struct {
				Body string `json:"body"`
			}
			_ = jsonUnmarshal(m.Payload, &p)
			history = append(history, map[string]any{"actor": m.Actor.ParticipantID, "body": p.Body})
		}
	}
	return map[string]any{
		"task_directive":  "你被指派执行一项独立任务（与群聊回复不同：这是任务执行通道）。完成后给出结果报告。",
		"instruction":     req.Instruction,
		"task_id":         req.TaskID,
		"recent_messages": history,
	}
}

func (e *Engine) appendRunEvent(ctx context.Context, roomID, typ string, payload []byte) bool {
	env := protocol.Envelope{
		EventID: e.cfg.NewID("evt"), TenantID: e.cfg.Tenant, RoomID: roomID,
		Type: typ, SchemaVersion: 1, OccurredAt: e.cfg.Clock(),
		Actor:      protocol.Actor{ParticipantID: "system", Kind: "system"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: payload, Metadata: map[string]any{},
	}
	_, err := e.cfg.Store.AppendEvents(ctx, []protocol.Envelope{env})
	return err == nil
}

func (e *Engine) appendRunTerminal(ctx context.Context, roomID string, req protocol.RunRequestedPayload, typ string, payload []byte) {
	if !e.appendRunEvent(ctx, roomID, typ, payload) {
		e.warn(roomID, "run 终态事件落库失败", "run", req.RunID, "type", typ)
	}
}

func (e *Engine) seatOf(pid string) *AgentSeat {
	for _, s := range e.seatsSnapshot() {
		if s.ParticipantID == pid {
			return &s
		}
	}
	return nil
}

func (e *Engine) roomPausedSince(ctx context.Context, roomID string) bool {
	events, err := e.roomHistory(ctx, roomID)
	if err != nil {
		return false
	}
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventRoomPaused {
			return true
		}
		if ev.Envelope.Type == protocol.EventRoomStarted {
			return false
		}
	}
	return false
}

func (e *Engine) knownRooms() []string {
	ctx := context.Background()
	lister, ok := e.cfg.Store.(RoomLister)
	if !ok {
		return nil
	}
	rooms, err := lister.ListRooms(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rooms))
	for _, r := range rooms {
		out = append(out, r.RoomID)
	}
	return out
}

func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// CanRun assignee 座位的适配器是否声明 TaskRuns（service.RunCapable 的引擎面）。
func (e *Engine) CanRun(assignee string) bool {
	seat := e.seatOf(assignee)
	if seat == nil {
		return false
	}
	return e.cfg.Agents.CapableOf(seat.Profile, func(c agent.Capabilities) bool { return c.TaskRuns })
}

// ---- 助手 ----

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func (s *Service) receiptFor(cmd Command, actor Actor, env protocol.Envelope) CommandReceipt {
	return CommandReceipt{
		TenantID: s.cfg.Tenant, RoomID: cmd.RoomID,
		IdempotencyKey: cmd.IdempotencyKey, CommandKind: cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor), EventID: env.EventID,
		ExpectedRoomVersion: cmd.ExpectedRoomVersion, ExecutedAt: s.cfg.Clock(),
	}
}

var (
	agentParticipantPattern = participantIDPattern
	runIDPatternS           = regexp.MustCompile(`^run_[0-9A-Za-z_-]+$`)
)

// appendCASRun 条件追加（appendCAS 的本文件面：单事件、返回追加与否）。
func (e *Engine) appendCASRun(ctx context.Context, roomID string, env protocol.Envelope, expected int64) ([]protocol.Envelope, error) {
	if cas, ok := e.cfg.Store.(CASStore); ok {
		return cas.AppendEventsIf(ctx, []protocol.Envelope{env}, expected)
	}
	appended, err := e.cfg.Store.AppendEvents(ctx, []protocol.Envelope{env})
	if err != nil {
		return nil, err
	}
	if appended[0].Seq != expected+1 { // 非 CAS 存储近似判定：版本已前进视为冲突
		return nil, ErrVersionConflict
	}
	return appended, nil
}
