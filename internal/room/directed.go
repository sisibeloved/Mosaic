// 定向交锋快速通道（RFC-0003 §3.1.9）：公开消息点名 addressed_to → 被点名者
// 在下一轮获定向回应 slot（优先资格 + 发言顺序前置）。纯函数——自事件历史推导，
// 回放一致。约束（RFC 原文语义）：
//   - slot 上限：每轮 ≤ ceil(单轮最大自动发言者/2)，且任何模式每轮至多 2 个；
//   - Roundtable 等全员发言模式：定向 slot 只影响顺序与优先资格，不增加名额；
//   - slot 仍受硬资格、预算、cooldown 约束（不绕过 admission）；
//   - 交锋链（连续定向轮）：窗口缩短 2/3，深度达上限（默认 4）后回正常评分队列；
//   - 快速通道不改变评分公式对其他候选者的约束。
package room

import (
	"encoding/json"
	"sort"

	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// maxDirectedChainDepth 模式最大交锋深度（RFC §3.1.9 默认 4）。
const maxDirectedChainDepth = 4

// directedSlotsFor 推导本轮定向集合、slot 上限与交锋链长度。
// 链长 = 以当前刺激为终点的连续定向轮数（当前刺激带点名即链 ≥1）；
// 深度超限 → 定向失效（回正常队列），上限语义保持。
func directedSlotsFor(history []StoredEvent, stimulus protocol.Envelope, seats []AgentSeat, maxSpeakers int) (directed map[string]bool, slotCap, chainLen int) {
	directed = map[string]bool{}
	targets := addressedTargetsOf(stimulus)
	seatSet := map[string]bool{}
	for _, s := range seats {
		seatSet[s.ParticipantID] = true
	}
	// 链长：历史轮（由新到旧）连续定向 + 当前
	chain := 0
	if len(targets) > 0 {
		chain = 1
	}
	// 收集历史各轮刺激（round.opened.causation → 消息 addressed_to），由新到旧
	for i := len(history) - 1; i >= 0 && chain < maxDirectedChainDepth+1; i-- {
		ev := history[i].Envelope
		if ev.Type != protocol.EventRoundOpened {
			continue
		}
		if ev.CausationID == nil || *ev.CausationID == stimulus.EventID {
			continue // 当前轮尚未开——其刺激即当前
		}
		stim := findEvent(history, *ev.CausationID)
		if stim == nil || len(addressedTargetsOf(*stim)) == 0 {
			break // 链断
		}
		chain++
	}
	if len(targets) == 0 || chain > maxDirectedChainDepth {
		return directed, 0, chain
	}
	for _, t := range targets {
		if seatSet[t] {
			directed[t] = true
		}
	}
	slotCap = (maxSpeakers + 1) / 2 // ceil(名额/2)
	if slotCap > 2 {
		slotCap = 2
	}
	return directed, slotCap, chain
}

// addressedTargetsOf 消息的点名对象（非消息/无点名返回空）。
func addressedTargetsOf(env protocol.Envelope) []string {
	if env.Type != protocol.EventMessagePosted {
		return nil
	}
	var payload struct {
		AddressedTo []string `json:"addressed_to"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return nil
	}
	return payload.AddressedTo
}

func findEvent(history []StoredEvent, eventID string) *protocol.Envelope {
	for i := range history {
		if history[i].Envelope.EventID == eventID {
			return &history[i].Envelope
		}
	}
	return nil
}

// applyDirectedSlots 定向 slot 调整（选择后、intent.recorded 前）：
// 非全员模式——未获选的合格定向候选强制入围前排（≤ slotCap），超出名额时
// 自末位挤掉非定向；全员模式——仅顺序前置 + 重排 rank。返回调整后选择。
func applyDirectedSlots(selection attention.Result, candidates []attention.Candidate,
	directed map[string]bool, slotCap, maxSpeakers int, fullHouse bool) attention.Result {

	if len(directed) == 0 || slotCap <= 0 || len(selection.Selected) == 0 && !fullHouse {
		return selection
	}
	selected := make([]attention.Selection, len(selection.Selected))
	copy(selected, selection.Selected)

	if !fullHouse {
		inSel := map[string]bool{}
		for _, s := range selected {
			inSel[s.IntentID] = true
		}
		bandByIntent := map[string]string{}
		for _, r := range selection.Rejected {
			bandByIntent[r.IntentID] = r.Band
		}
		forward := []attention.Selection{}
		for _, c := range candidates { // 座位序（确定性）
			if !directed[c.Intent.ParticipantID] || inSel[c.Intent.IntentID] {
				continue
			}
			if !c.Eligibility.Enabled || !c.Eligibility.BudgetOK {
				continue // 定向不绕过硬资格/预算
			}
			if len(forward) >= slotCap {
				break
			}
			forward = append(forward, attention.Selection{
				IntentID:      c.Intent.IntentID,
				ParticipantID: c.Intent.ParticipantID,
				Band:          bandByIntent[c.Intent.IntentID],
			})
			inSel[c.Intent.IntentID] = true
		}
		if len(forward) > 0 {
			merged := append(forward, selected...)
			// 名额收敛：自末位移除非定向（定向保留至 slotCap）
			for len(merged) > maxSpeakers {
				drop := -1
				for i := len(merged) - 1; i >= 0; i-- {
					if !directed[merged[i].ParticipantID] {
						drop = i
						break
					}
				}
				if drop < 0 {
					break // 全定向：不再强挤
				}
				merged = append(merged[:drop], merged[drop+1:]...)
			}
			selected = merged
		}
	}
	// 顺序前置（稳定排序：定向在前，组内保持既有相对序）+ 重排 rank
	sort.SliceStable(selected, func(i, j int) bool {
		return directed[selected[i].ParticipantID] && !directed[selected[j].ParticipantID]
	})
	for i := range selected {
		selected[i].Rank = i + 1
	}
	selection.Selected = selected
	return selection
}

// directedIntentIDs 最终选择中的定向授予集（floor.granted.directed 用）。
func directedIntentIDs(selection attention.Result, directed map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range selection.Selected {
		if directed[s.ParticipantID] {
			out[s.IntentID] = true
		}
	}
	return out
}
