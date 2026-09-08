// Package room 内文件：M4-3 单次 reply-or-pass 限定路径（RFC-0012 附录 J）。
//
// 两类允许场景（负责人范围约束；资格必须在调度前可明确判断，未命中或歧义即走
// 两阶段流程）：(1) 明确点名单人——刺激消息首行的 @提及 恰解析到一个在席 agent
// 且无其他 agent 提及；(2) 已有任务交付——刺激是对某 pending 任务申报消息的
// 回复（reply_to），作者为任务提出方，任务负责人在席 → 负责人座位资格。
//
// 路径纪律：仅作用于资格座位（其余成员照走两阶段自主判断）；记录实际路径
// （intent.recorded/message.posted 的 metadata.path = reply_or_pass，不伪造独立
// 评估评分）；调用开始后的超时/结构错误按执行状态处理（座位失败可见，不盲目
// 回退重跑）。设置族开关 reply_or_pass_mode（auto|off）承担 A/B 控制组。
package room

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ROP 场景常量（metadata.scenario）。
const (
	ROPScenarioNamedSingle = "named_single"
	ROPScenarioTaskOwner   = "task_owner"
)

// ROPEligibility 资格判定（纯函数）：返回 座位 → 场景。主动波恒不进入
// （proactive=false 由调用方保证）；两类场景可同时命中时点名单人优先
// （显式意图最强的信号）。
func ROPEligibility(anchor protocol.Envelope, seats []AgentSeat, history []StoredEvent) map[string]string {
	out := map[string]string{}
	if anchor.Actor.Kind != "human" {
		return out // agent 消息不进 ROP（自言自语/互答走两阶段）
	}
	var p struct {
		Body    string  `json:"body"`
		ReplyTo *string `json:"reply_to"`
	}
	_ = json.Unmarshal(anchor.Payload, &p)

	// 场景 1：首行 @提及 → 恰一个在席 agent，且首行再无其他提及
	if seat, ok, definite := namedSingleSeat(firstLine(p.Body), seats); ok && definite {
		out[seat] = ROPScenarioNamedSingle
		return out
	}
	// 场景 2：回复锚定 pending 任务的申报消息（reply_to = source_event_id）——
	// 线程定位了任务、任务定位了唯一在席负责人（tasklist 语义：申报人是 agent，
	// requester 即申报 agent——"谁在问"不可结构判定，"问的是这个任务"可以）。
	if p.ReplyTo != nil && *p.ReplyTo != "" {
		for _, t := range TasksOf(history) {
			if t.Status != "pending" || t.SourceEventID != *p.ReplyTo {
				continue
			}
			if seatOfPID(seats, t.Owner) != nil {
				out[t.Owner] = ROPScenarioTaskOwner
			}
			break
		}
	}
	return out
}

// namedSingleSeat 首行提及解析：@Name/@PID 精确匹配座位显示名或 PID。
// definite=false 表示有提及但歧义/不可解析（不进入，也不冒充场景 2——
// 保守：本函数只报场景 1 的结果，歧义信号由 ok=false 表达）。
func namedSingleSeat(line string, seats []AgentSeat) (pid string, ok bool, definite bool) {
	mentions := extractMentions(line)
	if len(mentions) == 0 {
		return "", false, true // 无提及：明确不是场景 1（可探场景 2）
	}
	hit := ""
	for _, m := range mentions {
		resolved := ""
		for _, s := range seats {
			if m == s.ParticipantID || (s.Profile.DisplayName != "" && m == s.Profile.DisplayName) {
				resolved = s.ParticipantID
				break
			}
		}
		if resolved == "" {
			return "", false, false // 有提及但不可解析 → 歧义：不进入
		}
		if hit != "" && hit != resolved {
			return "", false, false // 多个不同 agent → 不进入
		}
		hit = resolved
	}
	return hit, hit != "", true
}

// extractMentions 提取 @提及（连续非空白 token 去掉前导 @，剔尾部标点）。
// @par_xxx 与 @显示名均可。
func extractMentions(line string) []string {
	var out []string
	for _, tok := range strings.Fields(line) {
		if rest, ok := strings.CutPrefix(tok, "@"); ok && rest != "" {
			rest = strings.Trim(rest, "，,.。:：!！?？;；")
			if rest != "" {
				out = append(out, rest)
			}
		}
	}
	seen := map[string]bool{}
	dedup := out[:0]
	for _, m := range out {
		if !seen[m] {
			seen[m] = true
			dedup = append(dedup, m)
		}
	}
	return dedup
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func seatOfPID(seats []AgentSeat, pid string) *AgentSeat {
	for i := range seats {
		if seats[i].ParticipantID == pid {
			return &seats[i]
		}
	}
	return nil
}

// runReplyOrPass 资格座位的单次路径。返回：剩余（走两阶段的）座位、本路径
// 发布数、沉默数。speak → 发布门 + message.posted（agent 名义，metadata.path）；
// pass → intent.recorded silent（携路径与理由）；失败 → seat.status 瞬态 +
// intent.recorded 弃权（不盲目回退）。指标两路径都入账（A/B 面）。
func (e *Engine) runReplyOrPass(ctx context.Context, roomID, roundID string, anchor protocol.Envelope,
	elig map[string]string, seats []AgentSeat, evalContext agent.Context) (remaining []AgentSeat, published, silent int) {
	remaining = seats[:0:0]
	for _, s := range seats {
		scenario, isEligible := elig[s.ParticipantID]
		if !isEligible {
			remaining = append(remaining, s)
			continue
		}
		// 能力门 + 设置门（off = A/B 控制组：资格座位照走两阶段，指标入账）
		capable := e.cfg.Agents != nil && e.cfg.Agents.CapableOf(s.Profile, func(c agent.Capabilities) bool { return c.ReplyOrPass })
		enabled := e.cfg.ReplyOrPassEnabled == nil || e.cfg.ReplyOrPassEnabled()
		if !capable || !enabled {
			e.ropMetric(roomID, s.ParticipantID, scenario, "two_phase", "deferred", 0)
			remaining = append(remaining, s)
			continue
		}
		t := time.Now()
		if e.cfg.OnDraft != nil {
			e.cfg.OnDraft(roomID, s.ParticipantID, agent.DraftUpdate{Kind: "stage", Stage: "reply_or_pass"})
		}
		res, err := e.runTask(ctx, s.Profile, s.ParticipantID, agent.Task{
			TaskID:        e.cfg.NewID("rop"),
			Kind:          agent.KindReplyOrPass,
			ParticipantID: s.ParticipantID,
			RoomID:        roomID,
			Epoch:         roundID,
			Context:       evalContext,
		})
		ms := msSince(t)
		if err != nil {
			if ctx.Err() == nil {
				e.warn(roomID, "reply_or_pass 调用失败，该座按失败收口（不回退重跑）", "seat", s.ParticipantID, "err", err)
				e.seatStatus(roomID, s.ParticipantID, "rop_failed", err.Error())
				e.recordROPSilent(ctx, roomID, roundID, anchor, s.ParticipantID, "reply_or_pass 调用失败，弃权（不回退）", scenario)
				e.ropMetric(roomID, s.ParticipantID, scenario, "reply_or_pass", "failed", ms)
			}
			silent++
			continue
		}
		action, _ := res.Data["action"].(string)
		switch action {
		case "speak":
			body, _ := res.Data["body"].(string)
			clean, _, gerr := agent.PublishGate(body, res.Data["declared_relations"])
			if gerr != nil || strings.TrimSpace(clean) == "" {
				e.warn(roomID, "reply_or_pass speak 未过发布门，按失败收口", "seat", s.ParticipantID)
				e.seatStatus(roomID, s.ParticipantID, "rop_failed", "speak 未过发布门")
				e.recordROPSilent(ctx, roomID, roundID, anchor, s.ParticipantID, "reply_or_pass 结果不可发布，弃权", scenario)
				e.ropMetric(roomID, s.ParticipantID, scenario, "reply_or_pass", "failed", ms)
				silent++
				continue
			}
			env := e.newEnv(roomID, protocol.EventMessagePosted,
				protocol.Actor{ParticipantID: s.ParticipantID, Kind: "agent"}, anchor.EventID, roundID,
				map[string]any{"body": clean, "addressed_to": []string{}, "relations": []any{}})
			env.Metadata = map[string]any{"path": "reply_or_pass", "scenario": scenario}
			if _, err := e.append(ctx, env); err != nil {
				e.warn(roomID, "reply_or_pass 发布落库失败", "seat", s.ParticipantID, "err", err)
				silent++
				continue
			}
			e.recordROPIntent(ctx, roomID, roundID, anchor, s.ParticipantID, "speak", rationaleOf(res.Data), scenario)
			published++
			e.ropMetric(roomID, s.ParticipantID, scenario, "reply_or_pass", "published", ms)
		case "pass":
			e.recordROPSilent(ctx, roomID, roundID, anchor, s.ParticipantID, rationaleOf(res.Data), scenario)
			silent++
			e.ropMetric(roomID, s.ParticipantID, scenario, "reply_or_pass", "passed", ms)
		default:
			e.warn(roomID, "reply_or_pass 结构非法，该座失败收口", "seat", s.ParticipantID, "action", action)
			e.seatStatus(roomID, s.ParticipantID, "rop_failed", "结构非法")
			e.recordROPSilent(ctx, roomID, roundID, anchor, s.ParticipantID, "reply_or_pass 结构非法，弃权", scenario)
			e.ropMetric(roomID, s.ParticipantID, scenario, "reply_or_pass", "failed", ms)
			silent++
		}
	}
	return remaining, published, silent
}

func rationaleOf(data map[string]any) string {
	r, _ := data["public_rationale"].(string)
	if r == "" {
		r = "reply-or-pass 决定"
	}
	return truncate(r, 280)
}

// recordROPIntent 路径透明的意愿记录（speak 也记 intent——活动 Tab 重建依据；
// 不伪造评分：band=unranked、无 scores）。
func (e *Engine) recordROPIntent(ctx context.Context, roomID, roundID string, anchor protocol.Envelope, pid, action, rationale, scenario string) {
	env := e.newEnv(roomID, protocol.EventIntentRecorded,
		protocol.Actor{ParticipantID: pid, Kind: "agent"}, anchor.EventID, roundID,
		protocol.IntentRecordedPayload{
			IntentID:        e.cfg.NewID("int"),
			ParticipantID:   pid,
			Action:          action,
			ScoreBand:       "unranked",
			Selected:        action == "speak",
			PublicRationale: rationale,
		})
	env.Metadata = map[string]any{"path": "reply_or_pass", "scenario": scenario}
	_, _ = e.append(ctx, env)
}

func (e *Engine) recordROPSilent(ctx context.Context, roomID, roundID string, anchor protocol.Envelope, pid, rationale, scenario string) {
	env := e.newEnv(roomID, protocol.EventIntentRecorded,
		protocol.Actor{ParticipantID: pid, Kind: "agent"}, anchor.EventID, roundID,
		protocol.IntentRecordedPayload{
			IntentID:        e.cfg.NewID("int"),
			ParticipantID:   pid,
			Action:          "silent",
			ScoreBand:       "unranked",
			Selected:        false,
			PublicRationale: rationale,
		})
	env.Metadata = map[string]any{"path": "reply_or_pass", "scenario": scenario}
	_, _ = e.append(ctx, env)
}

func (e *Engine) ropMetric(roomID, seat, scenario, path, outcome string, ms int64) {
	if e.cfg.OnPathMetric != nil {
		e.cfg.OnPathMetric(roomID, seat, scenario, path, outcome, ms)
	}
}
