// 发言资格闸（RFC-0012 附录 K，v1.76）：群聊制下"必回"病理的确定性兜底。
// 病理实录（负责人狗粮 2026-09-17）：人类发言后所有 Agent 必回——放行无闸
// （speak 意图直入发布队列）+ 评估问法诱导（单条刺激下模型几乎总选 speak）
// + v1.40 冷却拆除后抑制全靠模型自觉。MaiBot 两代架构的共同启示：沉默必须
// 是数值默认而非模型自觉（经典：兴趣度地板概率 2%；maisaka：reply_necessity
// 评分 ≥80 才唤醒）。本闸为其群聊形态移植：
//   - @点名/定向直通（MaiBot mention=100 同构——人类点名必须有人应）；
//   - 资格分 = 模型自报 relevance 0.6 + urgency 0.4（R-01 已全量自报，不加调用）；
//   - 上波发言者扣冷却分（MaiBot 回复后 willing -1.8 的温和版——软冷却回归，
//     v1.40 拆除的"一刀切跳过"不同形：该座仍参评仍可过闸，只是门槛抬高）；
//   - 阈值与冷却强度入设置族活读（RunTimeout 先例），默认保守（0.30/0.20）。
//
// 被闸意图全记录（R-01：intent.recorded.selected=false + unselected_reason
// 分类），记分卡可见，人类 endorse 保送翻转闸门（保送链路不经闸——人类点名
// 即直通语义）。
package room

import (
	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// SpeakGateSettings 发言资格闸配置（设置族活读面；装配层自 internal/settings 注入）。
type SpeakGateSettings struct {
	Enabled         bool
	Threshold       float64 // 资格分阈值；低于即闸（豁免通道不受限）
	CooldownPenalty float64 // 上波发言者的资格分扣减（软冷却强度）
}

// DefaultSpeakGate 缺省束：闸开、阈值 0.30、冷却 0.20（上波发言者需 0.50 资格分
// 才能连说——真机沉默率数据校准前取保守中位，设置页可调）。
func DefaultSpeakGate() SpeakGateSettings {
	return SpeakGateSettings{Enabled: true, Threshold: 0.30, CooldownPenalty: 0.20}
}

// 闸门 reason 分类（intent.recorded.unselected_reason；自由串契约，非枚举）。
const (
	reasonBelowThreshold = "below_threshold" // 资格分未过阈
	reasonRecentSpeaker  = "recent_speaker"  // 原分过阈、冷却扣减后掉线
)

// SeatActivity 座位发言活动投影（评估语境注入 + 闸门修正输入；波前历史纯派生）。
// "你上波已发言/距上次发言隔几条"是模型自决沉默的事实锚点（MaiBot 式状态注入：
// 没有它模型无从判断"刚说过就别重复"）。
type SeatActivity struct {
	SpokeLastWave    bool `json:"spoke_last_wave"`   // 上一个已收波中该座位发布过
	MessagesSince    int  `json:"messages_since"`    // 距该座位上次发言隔了几条消息（从未发言 = -1）
	RecentUtterances int  `json:"recent_utterances"` // 近窗（10 条消息）内该座位发言数
}

// recentActivityWindow 活动近窗（与生成语境 RecentWindow=10 同量级）。
const recentActivityWindow = 10

// seatActivityOf 从波前事件派生座位活动投影（纯函数；history 为开波前快照，
// 不含本轮 round.opened）。
func seatActivityOf(envs []protocol.Envelope, pid string) SeatActivity {
	act := SeatActivity{MessagesSince: -1}
	// 自尾部单趟扫：近窗发言计数 + 距上次发言条数。
	msgs := 0
	since := 0
	seenMine := false
	for i := len(envs) - 1; i >= 0; i-- {
		e := envs[i]
		if e.Type != protocol.EventMessagePosted {
			continue
		}
		msgs++
		if msgs <= recentActivityWindow && e.Actor.Kind == "agent" && e.Actor.ParticipantID == pid {
			act.RecentUtterances++
		}
		if !seenMine {
			if e.Actor.Kind == "agent" && e.Actor.ParticipantID == pid {
				seenMine = true
				act.MessagesSince = since
			} else {
				since++
			}
		}
	}
	// 上波是否发言：最近一对 round.opened→round.closed 区间内该座位有发布。
	// 从尾找 last round.closed，再回溯到对应 round.opened（其后的 opened 属于
	// 未收波/本轮——波前快照不含本轮，稳健起见仍按区间界扫）。
	closedAt := -1
	for i := len(envs) - 1; i >= 0; i-- {
		if envs[i].Type == protocol.EventRoundClosed {
			closedAt = i
			break
		}
	}
	if closedAt >= 0 {
		for i := closedAt; i >= 0; i-- {
			if envs[i].Type == protocol.EventRoundOpened {
				for j := i; j <= closedAt; j++ {
					if envs[j].Type == protocol.EventMessagePosted &&
						envs[j].Actor.Kind == "agent" && envs[j].Actor.ParticipantID == pid {
						act.SpokeLastWave = true
					}
				}
				break
			}
		}
	}
	return act
}

// speakGateScore 资格分：模型自报四维中取 relevance/urgency 加权（0.6/0.4）。
// 非 silent 意图四维经端口强校验必填；域内映射缺维即 0 分——畸形自报直接
// 被闸，不做中性兜底（闸门保守语义：不确定 = 不说）。
func speakGateScore(s attention.Scores) float64 {
	return 0.6*s.Relevance + 0.4*s.Urgency
}

// SpeakGateVerdict 闸门判定结果。
type SpeakGateVerdict struct {
	Pass   bool    // 是否放行（false = 被闸）
	Reason string  // 被闸主因（below_threshold / recent_speaker）；放行为 ""
	Score  float64 // 冷却扣减后的有效资格分（记分卡透明用）
}

// speakGatePass 发言资格闸判定（纯函数）。豁免：闸关、@点名/定向
// （addressed——人类点名语义直通，与 endorse 保送同源）。修正：上波发言者
// 资格分扣 CooldownPenalty；原分过阈而扣后掉线归因 recent_speaker（记分卡
// 可辨"本来能说、冷却压下"），原分即不足归因 below_threshold。
func speakGatePass(gate SpeakGateSettings, scores attention.Scores, act SeatActivity, addressed bool) SpeakGateVerdict {
	if !gate.Enabled || addressed {
		return SpeakGateVerdict{Pass: true}
	}
	raw := speakGateScore(scores)
	score := raw
	if act.SpokeLastWave {
		score -= gate.CooldownPenalty
	}
	if score >= gate.Threshold {
		return SpeakGateVerdict{Pass: true, Score: score}
	}
	reason := reasonBelowThreshold
	if raw >= gate.Threshold {
		reason = reasonRecentSpeaker
	}
	return SpeakGateVerdict{Pass: false, Reason: reason, Score: score}
}
