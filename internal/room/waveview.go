// 波链路投影（M3-1 开发者模式持久化，计划 v1.31）：自事件流重建反应波全貌——
// 意图（含弃权/未选理由）、发授（含撤销）、发布、收波结局。dogfood 反馈：[dev]
// 内联时间线为瞬态（SPA 内存，重启即失）；本投影以事件溯源为唯一事实源，任意
// 历史波重启后完整可复盘。纯函数、确定性：同事件流必同视图。
package room

import (
	"encoding/json"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// WaveIntentView 波内一条意图记录（R-01 全记录投影：silent/弃权也在案）。
type WaveIntentView struct {
	EventID          string `json:"event_id"`
	ParticipantID    string `json:"participant_id"`
	Action           string `json:"action"`
	Type             string `json:"type,omitempty"`
	ScoreBand        string `json:"score_band"`
	PublicRationale  string `json:"public_rationale,omitempty"`
	UnselectedReason string `json:"unselected_reason,omitempty"`
	Selected         bool   `json:"selected"`
	Endorsed         bool   `json:"endorsed"`
}

// WaveGrantView 波内一次发授的终态（发布/撤销二居一，均未发生 = 在途或静默撤回）。
type WaveGrantView struct {
	GrantID       string `json:"grant_id"`
	ParticipantID string `json:"participant_id"`
	Rank          int    `json:"rank"`
	Published     bool   `json:"published"`
	Revoked       bool   `json:"revoked"`
	RevokeReason  string `json:"revoke_reason,omitempty"`
}

// WaveTimingView 波链路分段耗时（性能定位套件 v1，自 round.closed.metadata.timing
// 投影）。EvalTotalMs 是串行求和——波内并行化候选的首要观测位。
type WaveTimingView struct {
	TotalMs     int64            `json:"total_ms"`
	HistoryMs   int64            `json:"history_ms"`
	AssembleMs  int64            `json:"assemble_ms"`
	EvalMs      map[string]int64 `json:"eval_ms"`
	EvalTotalMs int64            `json:"eval_total_ms"`
	GenerateMs  map[string]int64 `json:"generate_ms"`
}

// WaveView 一个反应波的全貌（round.opened → round.closed 间的引擎足迹）。
type WaveView struct {
	RoundID         string           `json:"round_id"`
	StimulusEventID string           `json:"stimulus_event_id"`
	OpenedSeq       int64            `json:"opened_seq"`
	ClosedSeq       int64            `json:"closed_seq,omitempty"` // 0 = 未收波（崩溃/在途）
	Outcome         string           `json:"outcome,omitempty"`    // published | quiescent | revoked_all
	Published       int              `json:"published"`
	SilentCount     int              `json:"silent_count"`
	WindowMs        int64            `json:"window_ms,omitempty"` // 锚点消息→开波（去抖窗+队列等待）
	Timing          *WaveTimingView  `json:"timing,omitempty"`
	Intents         []WaveIntentView `json:"intents"`
	Grants          []WaveGrantView  `json:"grants"`
}

// WaveChainOf 自房间事件重建波链（按开波序）。归属规则：round.opened 之后的
// intent.recorded / floor.granted / floor.revoked / agent message.posted 归当前波
// （引擎执行面同波串行，correlation=round_id 与此一致；endorse 走独立发授面，
// 不在本投影内——记分卡面板已覆盖）。开波前的事件忽略。
func WaveChainOf(events []StoredEvent) []WaveView {
	var waves []WaveView
	var openedAts []string // 与 waves 平行：开波事件 occurred_at（窗口耗时计算用）
	var cur *WaveView
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventRoundOpened:
			var p protocol.RoundOpenedPayload
			if json.Unmarshal(env.Payload, &p) != nil || p.RoundID == "" {
				continue
			}
			waves = append(waves, WaveView{
				RoundID:         p.RoundID,
				StimulusEventID: p.StimulusEventID,
				OpenedSeq:       env.Seq,
				Intents:         []WaveIntentView{},
				Grants:          []WaveGrantView{},
			})
			openedAts = append(openedAts, env.OccurredAt)
			cur = &waves[len(waves)-1]
		case protocol.EventIntentRecorded:
			if cur == nil {
				continue
			}
			var p protocol.IntentRecordedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			cur.Intents = append(cur.Intents, WaveIntentView{
				EventID:          env.EventID,
				ParticipantID:    p.ParticipantID,
				Action:           p.Action,
				Type:             p.Type,
				ScoreBand:        p.ScoreBand,
				PublicRationale:  p.PublicRationale,
				UnselectedReason: p.UnselectedReason,
				Selected:         p.Selected,
				Endorsed:         p.Endorsed,
			})
		case protocol.EventFloorGranted:
			if cur == nil {
				continue
			}
			var p protocol.FloorGrantedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			cur.Grants = append(cur.Grants, WaveGrantView{
				GrantID:       p.GrantID,
				ParticipantID: p.ParticipantID,
				Rank:          p.Rank,
			})
		case protocol.EventFloorRevoked:
			if cur == nil {
				continue
			}
			var p protocol.FloorRevokedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			for i := range cur.Grants {
				if cur.Grants[i].GrantID == p.GrantID {
					cur.Grants[i].Revoked = true
					cur.Grants[i].RevokeReason = p.Reason
				}
			}
		case protocol.EventMessagePosted:
			if cur == nil || env.Actor.Kind != "agent" {
				continue
			}
			cur.Published++
			for i := len(cur.Grants) - 1; i >= 0; i-- { // 最近一次未结发授归账（同座可多次发授）
				if cur.Grants[i].ParticipantID == env.Actor.ParticipantID && !cur.Grants[i].Published && !cur.Grants[i].Revoked {
					cur.Grants[i].Published = true
					break
				}
			}
		case protocol.EventRoundClosed:
			if cur == nil {
				continue
			}
			var p protocol.RoundClosedPayload
			if json.Unmarshal(env.Payload, &p) != nil || p.RoundID != cur.RoundID {
				continue
			}
			cur.ClosedSeq = env.Seq
			cur.Outcome = p.Outcome
			cur.SilentCount = p.SilentCount
			cur.Timing = timingViewOf(env.Metadata["timing"])
		}
	}
	// 窗口耗时：锚点消息 → 开波（去抖窗 + 队列等待）。锚点定位按刺激事件 id 回溯。
	for i := range waves {
		if waves[i].StimulusEventID == "" {
			continue
		}
		if anchor := findEvent(events, waves[i].StimulusEventID); anchor != nil {
			waves[i].WindowMs = occurredMsDiff(anchor.OccurredAt, openedAts[i])
		}
	}
	return waves
}

// timingViewOf metadata.timing → 视图（双重表示兼容：内存 Store 存原结构体、
// 持久层 JSON 往返后是 map[string]any——经重新序列化统一解析）。
func timingViewOf(raw any) *WaveTimingView {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var tv WaveTimingView
	if json.Unmarshal(b, &tv) != nil {
		return nil
	}
	return &tv
}

// occurredMsDiff 两个 RFC3339 时间戳的毫秒差（解析失败返回 0）。
func occurredMsDiff(a, b string) int64 {
	ta, err1 := time.Parse(time.RFC3339Nano, a)
	tb, err2 := time.Parse(time.RFC3339Nano, b)
	if err1 != nil || err2 != nil {
		return 0
	}
	return tb.Sub(ta).Milliseconds()
}
