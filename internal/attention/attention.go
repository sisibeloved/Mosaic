// Package attention：Attention/Floor 仲裁的确定性选择引擎（RFC-0003）。
// 全部逻辑为纯函数：同输入必同输出（3.4 公平性回归的 CI 门禁依据）；
// 模型自报分数只是输入，确定性约束不可被模型覆盖（原则 5）。
package attention

import (
	"fmt"
	"math"
)

// Scores TurnIntent 自报分数（输入；∈[0,1]，越界整条拒绝——严格写）。
type Scores struct {
	Relevance  float64
	Novelty    float64
	Urgency    float64
	Confidence float64
}

// Intent 一个已评估的发言意图（RFC-0002 turn_intent 的域内投影）。
type Intent struct {
	IntentID        string
	ParticipantID   string
	Action          string // speak | react | fork | summarize | silent
	Type            string // answer | extend | challenge | support | question | redirect | synthesize
	AddressedTo     []string
	PublicRationale string
	Scores          Scores
}

// ContextFeatures 确定性上下文特征。
// M1 最小版：viewpoint_diversity/repetition_risk 为中性常量输入（结构投影 M3 接入），
// recent_floor_share 由引擎从事件历史计算，direct_address 来自点名/定向。
type ContextFeatures struct {
	ViewpointDiversity float64 `json:"viewpoint_diversity"`
	RecentFloorShare   float64 `json:"recent_floor_share"`
	RepetitionRisk     float64 `json:"repetition_risk"`
	DirectAddress      float64 `json:"direct_address"`
}

// Eligibility 硬资格（RFC-0003 §3.1.3，全部确定性）。
type Eligibility struct {
	Enabled        bool `json:"enabled"`
	CooldownOK     bool `json:"cooldown_ok"`
	ThreadWritable bool `json:"thread_writable"`
	BudgetOK       bool `json:"budget_ok"`
}

// Candidate 选择输入：意图 + 上下文特征 + 硬资格。
type Candidate struct {
	Intent      Intent
	Ctx         ContextFeatures
	Eligibility Eligibility
}

// Weights 记分卡权重（§3.1.5 默认值；floor_share/repetition 为惩罚项）。
type Weights struct {
	Relevance     float64 // w_r
	Novelty       float64 // w_n
	Diversity     float64 // w_d
	Urgency       float64 // w_u
	DirectAddress float64 // w_t
	FloorShare    float64 // w_f（负项）
	Repetition    float64 // w_p（负项）
}

// DefaultWeights OQ-04 裁决建议的默认权重。
var DefaultWeights = Weights{
	Relevance: 0.30, Novelty: 0.20, Diversity: 0.15, Urgency: 0.10,
	DirectAddress: 0.15, FloorShare: 0.05, Repetition: 0.05,
}

// Policy 模式参数束（模式 = 参数，不改变协议）。
type Policy struct {
	Mode        string  // roundtable | open_floor | deep_dive | review | decision
	MaxSpeakers int     // 单轮最大自动发言者
	Lambda      float64 // MMR 多样性惩罚系数（M1 默认 0.30，OQ-04 校准前可配）
	Weights     Weights
}

// Validate 权重与参数边界（单项 0–0.50；λ ∈ [0,1]）。
func (p Policy) Validate() error {
	w := p.Weights
	for name, v := range map[string]float64{
		"relevance": w.Relevance, "novelty": w.Novelty, "diversity": w.Diversity,
		"urgency": w.Urgency, "direct_address": w.DirectAddress,
		"floor_share": w.FloorShare, "repetition": w.Repetition,
	} {
		if v < 0 || v > 0.50 {
			return fmt.Errorf("attention: 权重 %s=%v 越界 [0,0.50]", name, v)
		}
	}
	if p.Lambda < 0 || p.Lambda > 1 {
		return fmt.Errorf("attention: lambda=%v 越界 [0,1]", p.Lambda)
	}
	switch p.Mode {
	case "roundtable", "open_floor", "deep_dive", "review", "decision":
	default:
		return fmt.Errorf("attention: 未知模式 %q", p.Mode)
	}
	return nil
}

// NormalizeWeights 正值项之和 >1 时归一化（RFC：正值项之和归一化）；≤1 保持字面值。
func NormalizeWeights(w Weights) Weights {
	sum := w.Relevance + w.Novelty + w.Diversity + w.Urgency + w.DirectAddress
	if sum > 1 {
		k := 1 / sum
		w.Relevance *= k
		w.Novelty *= k
		w.Diversity *= k
		w.Urgency *= k
		w.DirectAddress *= k
	}
	return w
}

// Score 记分卡：score = w_r·relevance + w_n·novelty + w_d·diversity + w_u·urgency
// + w_t·direct − w_f·floor_share − w_p·repetition（§3.1.5 公式）。
func Score(c Candidate, w Weights) float64 {
	return w.Relevance*c.Intent.Scores.Relevance +
		w.Novelty*c.Intent.Scores.Novelty +
		w.Diversity*c.Ctx.ViewpointDiversity +
		w.Urgency*c.Intent.Scores.Urgency +
		w.DirectAddress*c.Ctx.DirectAddress -
		w.FloorShare*c.Ctx.RecentFloorShare -
		w.Repetition*c.Ctx.RepetitionRisk
}

// Band 五档公开粒度（<0.2 / 0.2–0.4 / 0.4–0.6 / 0.6–0.8 / ≥0.8；OQ-04）。
// 精确分仅入内部评测（RFC-0011），对外只出 band（反 Goodhart）。
func Band(score float64) string {
	switch {
	case score < 0.2:
		return "very_low"
	case score < 0.4:
		return "low"
	case score < 0.6:
		return "medium"
	case score < 0.8:
		return "high"
	default:
		return "very_high"
	}
}

// Selection 获选者（rank 从 1 起；Score 仅内部，勿直接外泄——对外走 Band）。
type Selection struct {
	IntentID      string
	ParticipantID string
	Score         float64
	Band          string
	Rank          int
}

// Rejection 未获选记录（band 与理由保留可查，R-08 记分卡透明）。
type Rejection struct {
	IntentID      string
	ParticipantID string
	Reason        string
	Band          string
}

// Result 一轮选择结果。
type Result struct {
	Selected    []Selection
	Rejected    []Rejection
	Quiescent   bool
	SilentCount int
}

// Select 硬资格过滤 → 记分 → MMR 贪心选择（§3.1.5）。
// 停选条件：名额用尽或无正边际候选；Roundtable/Review 下 challenge/question
// 的 λ 乘 0.5（降低相似度惩罚，避免少数派被压掉）。
func Select(candidates []Candidate, policy Policy) Result {
	w := NormalizeWeights(policy.Weights)
	res := Result{}

	seen := map[string]bool{}
	var pool []scoredCandidate

	for _, c := range candidates {
		switch {
		case !c.Eligibility.Enabled:
			res.Rejected = append(res.Rejected, Rejection{c.Intent.IntentID, c.Intent.ParticipantID, "not_enabled", ""})
			continue
		case !c.Eligibility.ThreadWritable:
			res.Rejected = append(res.Rejected, Rejection{c.Intent.IntentID, c.Intent.ParticipantID, "thread_readonly", ""})
			continue
		case !c.Eligibility.CooldownOK:
			res.Rejected = append(res.Rejected, Rejection{c.Intent.IntentID, c.Intent.ParticipantID, "cooldown", ""})
			continue
		case seen[c.Intent.ParticipantID]:
			// 同轮同 participant 唯一（§3.1.3 硬资格 4）
			res.Rejected = append(res.Rejected, Rejection{c.Intent.IntentID, c.Intent.ParticipantID, "duplicate_intent", ""})
			continue
		case !c.Eligibility.BudgetOK:
			res.Rejected = append(res.Rejected, Rejection{c.Intent.IntentID, c.Intent.ParticipantID, "budget", ""})
			continue
		case c.Intent.Action == "silent":
			res.SilentCount++
			res.Rejected = append(res.Rejected, Rejection{c.Intent.IntentID, c.Intent.ParticipantID, "silent", ""})
			continue
		case scoreOutOfRange(c.Intent.Scores):
			// 严格写：超范围拒绝而非静默修正（§3.1.2）
			res.Rejected = append(res.Rejected, Rejection{c.Intent.IntentID, c.Intent.ParticipantID, "score_out_of_range", ""})
			continue
		}
		seen[c.Intent.ParticipantID] = true
		pool = append(pool, scoredCandidate{c, Score(c, w)})
	}

	// MMR 贪心
	selected := []scoredCandidate{}
	for len(selected) < policy.MaxSpeakers && len(pool) > 0 {
		bestIdx := -1
		var bestMMR float64
		for i, cand := range pool {
			mmr := cand.score - effectiveLambda(policy, cand.candidate.Intent.Type)*maxSimilarity(cand.candidate, selected)
			better := false
			switch {
			case bestIdx < 0:
				better = true
			case mmr > bestMMR:
				better = true
			case mmr == bestMMR:
				// 确定性平分决胜：记分卡分高者优先，再按 participant_id 字典序
				cur := pool[bestIdx]
				if cand.score > cur.score ||
					(cand.score == cur.score && cand.candidate.Intent.ParticipantID < cur.candidate.Intent.ParticipantID) {
					better = true
				}
			}
			if better && mmr > 0 {
				bestIdx, bestMMR = i, mmr
			}
		}
		if bestIdx < 0 {
			break // 无正边际候选
		}
		selected = append(selected, pool[bestIdx])
		pool = append(pool[:bestIdx], pool[bestIdx+1:]...)
	}

	for i, s := range selected {
		res.Selected = append(res.Selected, Selection{
			IntentID:      s.candidate.Intent.IntentID,
			ParticipantID: s.candidate.Intent.ParticipantID,
			Score:         s.score,
			Band:          Band(s.score),
			Rank:          i + 1,
		})
	}
	// 未获选但资格完备者：quota 用尽或无正边际（含空 quota）
	for _, c := range pool {
		res.Rejected = append(res.Rejected, Rejection{
			c.candidate.Intent.IntentID, c.candidate.Intent.ParticipantID,
			rejectReason(policy, len(selected)), Band(c.score),
		})
	}
	res.Quiescent = len(res.Selected) == 0
	return res
}

func rejectReason(policy Policy, selectedCount int) string {
	if selectedCount >= policy.MaxSpeakers {
		return "quota_exhausted"
	}
	return "non_positive_margin"
}

// effectiveLambda Roundtable/Review 下 challenge/question 的相似度惩罚减半（§3.1.5）。
func effectiveLambda(policy Policy, intentType string) float64 {
	if (policy.Mode == "roundtable" || policy.Mode == "review") &&
		(intentType == "challenge" || intentType == "question") {
		return policy.Lambda * 0.5
	}
	return policy.Lambda
}

// scoredCandidate 已过资格与校验、带记分卡分的候选。
type scoredCandidate struct {
	candidate Candidate
	score     float64
}

// maxSimilarity 候选与已选集的最大相似度；空集为 0。
func maxSimilarity(c Candidate, selected []scoredCandidate) float64 {
	max := 0.0
	for _, s := range selected {
		if v := similarity(c.Intent, s.candidate.Intent); v > max {
			max = v
		}
	}
	return max
}

// similarity M1 相似度替身：0.1 基线 + 0.6×同类型 + 0.3×共同点名对象（上限 1.0）。
// 结构投影（cluster/重访/embedding，RFC-0006）就绪后替换本函数——接口不变。
func similarity(a, b Intent) float64 {
	sim := 0.1
	if a.Type == b.Type && a.Type != "" {
		sim += 0.6
	}
	if len(a.AddressedTo) > 0 && len(b.AddressedTo) > 0 && intersects(a.AddressedTo, b.AddressedTo) {
		sim += 0.3
	}
	return math.Min(sim, 1.0)
}

func intersects(a, b []string) bool {
	set := map[string]struct{}{}
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

func scoreOutOfRange(s Scores) bool {
	for _, v := range []float64{s.Relevance, s.Novelty, s.Urgency, s.Confidence} {
		if v < 0 || v > 1 {
			return true
		}
	}
	return false
}
