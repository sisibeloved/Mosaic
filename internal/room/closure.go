// 收束协议（RFC-0005，M3-2 群聊制裁剪口径——RFC 附录 B）：人类显式提议 →
// 全员三态评估（conclude/object/abstain）→ 合格异议确定性判定（新证据/新假设 +
// 预期影响；claim 快照随 RFC-0006/M3-4，裁剪期 claim_id 可选）→ 合格则中止收束
// （线程回 active）、不合格 object 记具名异议不阻塞 → 人类 accept_closure 时自
// 事件流确定性组装 Capsule 并关线程。silent/timeout/unavailable 永不计为同意；
// 预算熔断只产 Pause Capsule（不写 closure.accepted、不关线程）。
package room

import (
	"context"
	"encoding/json"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// closureIntentOf 适配器 closure_intent 结构化块 → 三态评估（未知 action = 结构非法，
// 该座记 unavailable；object 的增量字段缺失不是非法——是不合格，停放为具名异议）。
func closureIntentOf(participantID string, data map[string]any) (protocol.ClosureEvaluatedPayload, bool) {
	action, _ := data["action"].(string)
	switch action {
	case "conclude", "object", "abstain":
	default:
		return protocol.ClosureEvaluatedPayload{}, false
	}
	strs := func(key string) []string {
		raw, _ := data[key].([]any)
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	rationale, _ := data["rationale"].(string)
	if rationale == "" {
		rationale, _ = data["public_rationale"].(string) // 适配器措辞容错
	}
	p := protocol.ClosureEvaluatedPayload{
		ClosureID:      "", // 引擎填
		ParticipantID:  participantID,
		Action:         action,
		ClaimID:        strField(data, "claim_id"),
		NewEvidence:    strs("new_evidence"),
		NewAssumptions: strs("new_assumptions"),
		ExpectedImpact: strField(data, "expected_impact"),
		Rationale:      truncate(rationale, 280),
	}
	if action == "object" {
		// 确定性合格性：新证据或新假设（至少一项）+ 预期影响（RFC-0005 §3.1.4 裁剪）
		switch {
		case (len(p.NewEvidence) > 0 || len(p.NewAssumptions) > 0) && p.ExpectedImpact != "":
			p.Qualified = true
		case len(p.NewEvidence) == 0 && len(p.NewAssumptions) == 0:
			p.ParkedReason = "no_new_evidence_or_assumptions"
		default:
			p.ParkedReason = "no_expected_impact"
		}
	}
	return p, true
}

func strField(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return s
}

// runClosure 收束评估路径（经 outbox 的 closure.proposed 驱动；与反应波共用房间
// 串行队列——评估期间不开新波）。abort/异常不产生 rejected：提议仍 pending，
// 人类可再次 propose（新 closure_id）。
func (e *Engine) runClosure(ctx context.Context, roomID, proposedEventID string) {
	if ctx.Err() != nil {
		return
	}
	mu := e.lockRoom(roomID)
	mu.Lock()
	defer mu.Unlock()

	history, err := e.roomHistory(ctx, roomID)
	if err != nil {
		e.warn(roomID, "收束：history 读取失败，中止", "err", err)
		return
	}
	proposed := findEvent(history, proposedEventID)
	if proposed == nil {
		return
	}
	var pp protocol.ClosureProposedPayload
	if json.Unmarshal(proposed.Payload, &pp) != nil || pp.ClosureID == "" {
		return
	}
	// 幂等：该提议已有评估（重投/恢复并发的去重）
	for _, ev := range history {
		if ev.Envelope.Type == protocol.EventClosureEvaluated {
			var q protocol.ClosureEvaluatedPayload
			if json.Unmarshal(ev.Envelope.Payload, &q) == nil && q.ClosureID == pp.ClosureID {
				return
			}
		}
	}
	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	if roomPaused(history) {
		e.debug(roomID, "收束跳过：房间暂停", "closure", pp.ClosureID)
		return
	}
	if ThreadStateOf(envs, pp.ThreadID) != ThreadActive {
		e.debug(roomID, "收束跳过：线程不在活跃态", "closure", pp.ClosureID, "thread", pp.ThreadID)
		return
	}
	// 预算熔断不触发收束（只产暂停胶囊）；评估自身也耗预算——熔断即止
	ledger := contextx.RebuildBudget(envs)
	if !ledger.Admit(e.cfg.Budget) {
		e.debug(roomID, "收束跳过：预算熔断", "closure", pp.ClosureID)
		return
	}

	seats := e.roomSeats(history)
	if len(seats) == 0 {
		e.debug(roomID, "收束跳过：无座", "closure", pp.ClosureID)
		return
	}
	seatsMin := make([]contextx.Seat, len(seats))
	for i, s := range seats {
		seatsMin[i] = contextx.Seat{ParticipantID: s.ParticipantID}
	}
	var last protocol.Envelope
	for _, ev := range history {
		if ev.Envelope.Type == protocol.EventMessagePosted {
			last = ev.Envelope
		}
	}
	assembled := contextx.Assemble(contextx.Config{
		RoomID: roomID, TaskID: pp.ClosureID, Mode: "chat", Seats: seatsMin,
		RecentWindow: 10,
		Budget: contextx.BudgetState{
			RemainingTokens: remainingTokens(ledger, e.cfg.Budget),
			Level:           ledger.Level(e.cfg.Budget),
		},
	}, envs, last)
	taskContext := agent.Context{Inline: assembled.Inline, ReceiptRef: assembled.Receipt.ReceiptID}

	e.debug(roomID, "收束评估开始", "closure", pp.ClosureID, "seats", len(seats))
	var qualified *protocol.ClosureEvaluatedPayload
	var qualifiedEventID string
	for _, seat := range seats {
		result, err := e.runTask(ctx, seat.Profile, seat.ParticipantID, agent.Task{
			TaskID:        e.cfg.NewID("tsk"),
			Kind:          agent.KindEvaluateClosure,
			ParticipantID: seat.ParticipantID,
			RoomID:        roomID,
			ThreadID:      pp.ThreadID,
			Epoch:         pp.ClosureID,
			Context:       taskContext,
		})
		if err != nil {
			if ctx.Err() != nil {
				return // Close/取消：提议保持 pending
			}
			// 评估失败记 unavailable——永不计为同意，收束继续（其余座仍有表态）
			e.warn(roomID, "收束评估失败，该座记 unavailable", "seat", seat.ParticipantID, "err", err)
			if _, err := e.append(ctx, e.newEnv(roomID, protocol.EventClosureEvaluated,
				protocol.Actor{ParticipantID: seat.ParticipantID, Kind: "agent"}, proposedEventID, pp.ClosureID,
				protocol.ClosureEvaluatedPayload{
					ClosureID:     pp.ClosureID,
					ParticipantID: seat.ParticipantID,
					Action:        "abstain",
					Rationale:     "评估不可用（unavailable）——不计为同意",
				})); err != nil {
				return
			}
			continue
		}
		intent, valid := closureIntentOf(seat.ParticipantID, result.Data)
		if !valid {
			e.warn(roomID, "收束意图结构非法，该座记 unavailable", "seat", seat.ParticipantID)
			intent = protocol.ClosureEvaluatedPayload{
				ClosureID:     pp.ClosureID,
				ParticipantID: seat.ParticipantID,
				Action:        "abstain",
				Rationale:     "评估不可用（unavailable）——不计为同意",
			}
		}
		intent.ClosureID = pp.ClosureID
		appended, err := e.append(ctx, e.newEnv(roomID, protocol.EventClosureEvaluated,
			protocol.Actor{ParticipantID: seat.ParticipantID, Kind: "agent"}, proposedEventID, pp.ClosureID, intent))
		if err != nil {
			return
		}
		if intent.Action == "object" && intent.Qualified && qualified == nil {
			q := intent
			qualified = &q
			qualifiedEventID = appended[0].EventID
		}
	}

	// 合格异议 → 中止收束（线程保持 active，人类可再次提议）
	if qualified != nil {
		reason := "new_assumptions"
		if len(qualified.NewEvidence) > 0 {
			reason = "new_evidence"
		}
		_, _ = e.append(ctx, e.newEnv(roomID, protocol.EventClosureRejected,
			protocol.Actor{ParticipantID: "par_system", Kind: "system"}, qualifiedEventID, pp.ClosureID,
			protocol.ClosureRejectedPayload{
				ClosureID:          pp.ClosureID,
				QualifiedObjection: qualifiedEventID,
				Reason:             reason,
				PhaseTo:            "active",
			}))
		e.debug(roomID, "收束被合格异议中止", "closure", pp.ClosureID, "seat", qualified.ParticipantID)
		return
	}
	e.debug(roomID, "收束评估完成，待人类接受", "closure", pp.ClosureID)
}

// PendingClosure 快照/命令侧的待决收束视图。
type PendingClosure struct {
	ClosureID      string `json:"closure_id"`
	ThreadID       string `json:"thread_id"`
	ClosureHint    string `json:"closure_hint,omitempty"`
	Ready          bool   `json:"ready"` // 全员已表态且未被异议中止
	EvaluatedCount int    `json:"evaluated_count"`
	ConcludedCount int    `json:"concluded_count"`
	ObjectionCount int    `json:"objection_count"` // 全部 object（合格与否；合格即已中止）
}

// PendingClosureOf 自事件流重建当前待决收束（最后一个 proposed 且无 accepted/rejected）。
func PendingClosureOf(events []StoredEvent) (PendingClosure, bool) {
	var pending *PendingClosure
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventClosureProposed:
			var p protocol.ClosureProposedPayload
			if json.Unmarshal(env.Payload, &p) == nil && p.ClosureID != "" {
				pending = &PendingClosure{ClosureID: p.ClosureID, ThreadID: p.ThreadID, ClosureHint: p.ClosureHint}
			}
		case protocol.EventClosureAccepted, protocol.EventClosureRejected:
			var id struct {
				ClosureID string `json:"closure_id"`
			}
			if json.Unmarshal(env.Payload, &id) == nil && pending != nil && id.ClosureID == pending.ClosureID {
				pending = nil
			}
		case protocol.EventClosureEvaluated:
			var p protocol.ClosureEvaluatedPayload
			if json.Unmarshal(env.Payload, &p) == nil && pending != nil && p.ClosureID == pending.ClosureID {
				pending.EvaluatedCount++
				if p.Action == "conclude" {
					pending.ConcludedCount++
				}
				if p.Action == "object" {
					pending.ObjectionCount++
				}
			}
		}
	}
	if pending == nil {
		return PendingClosure{}, false
	}
	pending.Ready = pending.EvaluatedCount > 0
	return *pending, true
}

// BuildCapsule 接受时自事件流确定性组装胶囊（无模型调用；conclusion=conclude 理由、
// dissent=不合格 object、assumptions=object 新假设、falsifiers=object 预期影响 +
// 兜底重开条件——reopen_triggers 强制非空，RFC-0005 §3.4 可逆性）。
func BuildCapsule(events []StoredEvent, closureID string) (protocol.ClosureCapsule, bool) {
	capsule := protocol.ClosureCapsule{
		ClosureID: closureID,
		Evidence:  protocol.CapsuleEvidence{Support: []string{}, Oppose: []string{}},
		ReopenTriggers: []string{
			"任一 falsifier 条件成立时由人类 reopen_thread 重开（新证据/新假设留痕于重开首条消息）",
		},
		Participation: protocol.CapsuleParticipation{
			Concluded: []string{}, Objected: []string{}, Abstained: []string{},
			Timeout: []string{}, Unavailable: []string{},
		},
	}
	seen := map[string]bool{}
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventClosureProposed:
			var p protocol.ClosureProposedPayload
			if json.Unmarshal(env.Payload, &p) == nil && p.ClosureID == capsule.ClosureID {
				capsule.ThreadID = p.ThreadID
				capsule.Watermark = p.Watermark
			}
		case protocol.EventClosureEvaluated:
			var p protocol.ClosureEvaluatedPayload
			if json.Unmarshal(env.Payload, &p) != nil || p.ClosureID != capsule.ClosureID {
				continue
			}
			switch p.Action {
			case "conclude":
				capsule.Participation.Concluded = append(capsule.Participation.Concluded, p.ParticipantID)
				if p.Rationale != "" && !seen["c:"+p.Rationale] {
					seen["c:"+p.Rationale] = true
					capsule.Conclusions = append(capsule.Conclusions, p.Rationale)
				}
			case "object":
				capsule.Participation.Objected = append(capsule.Participation.Objected, p.ParticipantID)
				capsule.NamedDissent = append(capsule.NamedDissent, protocol.CapsuleDissent{
					ParticipantID: p.ParticipantID,
					Basis:         joinNonEmpty("；", p.Rationale, "预期影响："+p.ExpectedImpact),
				})
				capsule.Assumptions = append(capsule.Assumptions, p.NewAssumptions...)
				if p.ExpectedImpact != "" {
					capsule.Falsifiers = append(capsule.Falsifiers, p.ExpectedImpact)
				}
			case "abstain":
				capsule.Participation.Abstained = append(capsule.Participation.Abstained, p.ParticipantID)
				if p.Rationale != "" {
					capsule.OpenQuestions = append(capsule.OpenQuestions, p.Rationale)
				}
			}
		}
	}
	if capsule.ThreadID == "" {
		return protocol.ClosureCapsule{}, false
	}
	// 类型判定：有具名异议/未决问题 → bounded_disagreement（分歧边界明确）；否则 consensus。
	// 提议 hint 合法时优先（人类显式指定）。
	if len(capsule.NamedDissent) > 0 || len(capsule.OpenQuestions) > 0 {
		capsule.ClosureType = "bounded_disagreement"
	} else {
		capsule.ClosureType = "consensus"
	}
	for _, ev := range events { // hint 取自该提议事件
		if ev.Envelope.Type != protocol.EventClosureProposed {
			continue
		}
		var p protocol.ClosureProposedPayload
		if json.Unmarshal(ev.Envelope.Payload, &p) == nil && p.ClosureID == closureID &&
			(p.ClosureHint == "consensus" || p.ClosureHint == "bounded_disagreement") {
			capsule.ClosureType = p.ClosureHint
		}
	}
	return capsule, true
}

func joinNonEmpty(sep string, parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

// pauseCapsuleActive 预算暂停胶囊在位判定（去重护栏）：存在 pause_capsule.created
// 且其后无 room.started（恢复即清位——恢复后的下一次熔断可再产新胶囊）。
func pauseCapsuleActive(events []StoredEvent) bool {
	active := false
	for _, ev := range events {
		switch ev.Envelope.Type {
		case protocol.EventPauseCapsuleCreated:
			active = true
		case protocol.EventRoomStarted:
			active = false
		}
	}
	return active
}

// emitPauseCapsule 预算熔断路径：未收敛快照（不写 closure.accepted、不关线程）。
func (e *Engine) emitPauseCapsule(ctx context.Context, roomID string, history []StoredEvent) {
	rootThread := ""
	for _, ev := range history {
		if ev.Envelope.Type == protocol.EventRoomCreated {
			var p struct {
				ThreadID string `json:"thread_id"`
			}
			if json.Unmarshal(ev.Envelope.Payload, &p) == nil {
				rootThread = p.ThreadID
			}
		}
	}
	watermark := int64(0)
	if len(history) > 0 {
		watermark = history[len(history)-1].Envelope.Seq
	}
	env := e.newEnv(roomID, protocol.EventPauseCapsuleCreated,
		protocol.Actor{ParticipantID: "par_system", Kind: "system"}, "", "",
		protocol.PauseCapsuleCreatedPayload{
			PauseID:       e.cfg.NewID("pse"),
			PauseReason:   "budget",
			ThreadID:      rootThread,
			Watermark:     watermark,
			OpenQuestions: []string{},
		})
	if _, err := e.append(ctx, env); err != nil {
		e.warn(roomID, "暂停胶囊落库失败", "err", err)
		return
	}
	e.warn(roomID, "预算熔断：已写暂停胶囊（未收敛快照，非结论；恢复后可继续）",
		"watermark", watermark)
}
