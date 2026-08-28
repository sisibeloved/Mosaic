// UT 层：Attention 选择引擎（切片 C）——记分卡公式、band 边界、硬资格、
// MMR 确定性选择、λ 衰减、无正边际停选、严格校验。
// TDD：本文件先行于 attention.go（红→绿）。全部期望值手工推导自 RFC-0003 §3.1。
package attention

import (
	"math"
	"testing"
)

func cand(id, par, action, typ string, rel, nov, urg, conf float64, addressed []string) Candidate {
	return Candidate{
		Intent: Intent{
			IntentID:     id,
			ParticipantID: par,
			Action:       action,
			Type:         typ,
			AddressedTo:  addressed,
			Scores:       Scores{Relevance: rel, Novelty: nov, Urgency: urg, Confidence: conf},
		},
		Ctx:       ContextFeatures{ViewpointDiversity: 0.5},
		Eligibility: Eligibility{Enabled: true, CooldownOK: true, ThreadWritable: true, BudgetOK: true},
	}
}

// 公式（默认权重）：score = .3r+.2n+.15d+.1u+.15t −.05f −.05p
func TestScorecardFormula(t *testing.T) {
	c := cand("i", "p", "speak", "extend", 0.8, 0.6, 0.4, 0.9, nil)
	c.Ctx = ContextFeatures{
		ViewpointDiversity: 0.5,
		RecentFloorShare:   0.2,
		RepetitionRisk:     0.1,
		DirectAddress:      0,
	}
	// .3×.8 + .2×.6 + .15×.5 + .1×.4 + .15×0 − .05×.2 − .05×.1 = .46
	got := Score(c, DefaultWeights)
	if want := 0.46; math.Abs(got-want) > 1e-9 {
		t.Fatalf("score = %v（期望 %v）", got, want)
	}
}

func TestBandBoundaries(t *testing.T) {
	cases := []struct {
		score float64
		band  string
	}{
		{0.0, "very_low"}, {0.19, "very_low"},
		{0.2, "low"}, {0.39, "low"},
		{0.4, "medium"}, {0.59, "medium"},
		{0.6, "high"}, {0.79, "high"},
		{0.8, "very_high"}, {1.0, "very_high"},
	}
	for _, tc := range cases {
		if got := Band(tc.score); got != tc.band {
			t.Errorf("Band(%v) = %s（期望 %s）", tc.score, got, tc.band)
		}
	}
}

func TestHardEligibilityFiltersWithReason(t *testing.T) {
	c := cand("i", "p", "speak", "extend", 0.9, 0.9, 0.9, 0.9, nil)
	c.Eligibility.Enabled = false
	res := Select([]Candidate{c}, Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.3, Weights: DefaultWeights})
	if !res.Quiescent || len(res.Selected) != 0 {
		t.Fatalf("唯一候选失格应 quiescent：%+v", res)
	}
	if res.Rejected[0].Reason != "not_enabled" {
		t.Fatalf("失格原因 = %s（期望 not_enabled，按 RFC 检查顺序）", res.Rejected[0].Reason)
	}
	// Thread 只读次之
	c2 := cand("i2", "p", "speak", "extend", 0.9, 0.9, 0.9, 0.9, nil)
	c2.Eligibility.ThreadWritable = false
	res2 := Select([]Candidate{c2}, Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.3, Weights: DefaultWeights})
	if res2.Rejected[0].Reason != "thread_readonly" {
		t.Fatalf("原因 = %s（期望 thread_readonly）", res2.Rejected[0].Reason)
	}
}

func TestDuplicateIntentPerRoundRejected(t *testing.T) {
	a := cand("i1", "par_x", "speak", "extend", 0.9, 0.9, 0.9, 0.9, nil)
	b := cand("i2", "par_x", "speak", "challenge", 0.8, 0.8, 0.8, 0.8, nil) // 同 participant
	res := Select([]Candidate{a, b}, Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.3, Weights: DefaultWeights})
	if len(res.Selected) != 1 || res.Selected[0].IntentID != "i1" {
		t.Fatalf("同轮同 participant 唯一：保留先到者，got %+v", res.Selected)
	}
	dup := false
	for _, r := range res.Rejected {
		if r.IntentID == "i2" && r.Reason == "duplicate_intent" {
			dup = true
		}
	}
	if !dup {
		t.Fatalf("i2 应以 duplicate_intent 拒绝：%+v", res.Rejected)
	}
}

func TestScoreOutOfRangeStrictReject(t *testing.T) {
	c := cand("i", "p", "speak", "extend", 1.2, 0.5, 0.5, 0.5, nil) // relevance > 1
	res := Select([]Candidate{c}, Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.3, Weights: DefaultWeights})
	if !res.Quiescent {
		t.Fatal("越界分数必须整条拒绝（严格写：不静默修正）")
	}
	if res.Rejected[0].Reason != "score_out_of_range" {
		t.Fatalf("原因 = %s（期望 score_out_of_range）", res.Rejected[0].Reason)
	}
}

func TestSilentExcludedButCounted(t *testing.T) {
	s := cand("i1", "par_a", "silent", "", 0, 0, 0, 0, nil)
	b := cand("i2", "par_b", "speak", "extend", 0.5, 0.5, 0.5, 0.5, nil)
	res := Select([]Candidate{s, b}, Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.3, Weights: DefaultWeights})
	if len(res.Selected) != 1 || res.Selected[0].IntentID != "i2" {
		t.Fatalf("silent 不参与选择：%+v", res.Selected)
	}
	if res.SilentCount != 1 {
		t.Fatalf("silent_count = %d（期望 1）", res.SilentCount)
	}
	if res.Rejected[0].Reason != "silent" {
		t.Fatalf("silent 应以 reason=silent 记录：%+v", res.Rejected[0])
	}
}

// MMR 手工推导：λ=0.3、quota=3、open_floor
//   A(challenge,[beta]) score=.77；C(question) .575；B(extend,[beta]) .42；D .09
//   rank1=A；C mmr=.575−.3×.1=.545；B mmr=.42−.3×.4=.30；D mmr=.06>0 但 quota 满
//   期望选择序 [A, C, B]，D 以 quota 拒绝且 band 可查（R-08）
func TestMMRSelectionHandComputed(t *testing.T) {
	A := cand("int_a", "par_alpha", "speak", "challenge", 0.9, 0.8, 0.7, 0.9, []string{"par_beta"})
	A.Ctx = ContextFeatures{ViewpointDiversity: 0.8, DirectAddress: 1.0} // .27+.16+.12+.07+.15=.77
	B := cand("int_b", "par_beta", "speak", "extend", 0.85, 0.5, 0.3, 0.6, []string{"par_beta"})
	B.Ctx = ContextFeatures{ViewpointDiversity: 0.4, RecentFloorShare: 0.3, RepetitionRisk: 0.2} // .255+.1+.06+.03−.015−.01=.42
	C := cand("int_c", "par_gamma", "speak", "question", 0.7, 0.9, 0.5, 0.7, nil)
	C.Ctx = ContextFeatures{ViewpointDiversity: 0.9} // .21+.18+.135+.05=.575
	D := cand("int_d", "par_delta", "speak", "support", 0.3, 0.2, 0.1, 0.4, nil)
	D.Ctx = ContextFeatures{ViewpointDiversity: 0.1, RecentFloorShare: 0.8, RepetitionRisk: 0.5} // .09+.04+.015+.01−.04−.025=.09
	E := cand("int_e", "par_eps", "speak", "extend", 0.9, 0.9, 0.9, 0.9, nil)
	E.Eligibility.CooldownOK = false

	policy := Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.3, Weights: DefaultWeights}
	res := Select([]Candidate{A, B, C, D, E}, policy)

	wantOrder := []string{"int_a", "int_c", "int_b"}
	if len(res.Selected) != len(wantOrder) {
		t.Fatalf("选择数 = %d（期望 3）：%+v", len(res.Selected), res.Selected)
	}
	for i, want := range wantOrder {
		if res.Selected[i].IntentID != want || res.Selected[i].Rank != i+1 {
			t.Fatalf("rank %d = %+v（期望 %s）", i+1, res.Selected[i], want)
		}
	}
	if res.Selected[0].Band != "high" || res.Selected[1].Band != "medium" || res.Selected[2].Band != "medium" {
		t.Fatalf("band 序列不符：%+v", res.Selected)
	}
	if res.Quiescent {
		t.Fatal("有获选不应 quiescent")
	}
	reasons := map[string]string{}
	for _, r := range res.Rejected {
		reasons[r.IntentID] = r.Reason
	}
	if reasons["int_d"] != "quota_exhausted" || reasons["int_e"] != "cooldown" {
		t.Fatalf("拒绝原因不符：%v", reasons)
	}
	// 未获选者的 band 保留可查（R-08：记分卡透明）
	for _, r := range res.Rejected {
		if r.IntentID == "int_d" && r.Band != "very_low" {
			t.Fatalf("int_d band = %s（期望 very_low）", r.Band)
		}
	}
}

// Roundtable/Review 下 challenge/question 的 λ 减半（RFC-0003 §3.1.5）：
// Z(challenge) 高分先选；Y(extend,[beta]) 与 Z 相似度 0.4（异类型+共同点名）；
// W(challenge,[beta]) 与 Z 相似度 1.0（同类型+共同点名）。λ=.5
//   Y score=.14；W score=.40
//   open_floor：Y 边际 .14−.5×.4=−.06；W 边际 .40−.5×1.0=−.10 → 仅 [Z]
//   roundtable：W λ_eff=.25 → 边际 .40−.25=.15>0（λ 减半翻转）→ [Z, W]
func TestLambdaReductionForChallengeInRoundtable(t *testing.T) {
	Z := cand("z", "par_z", "speak", "challenge", 0.9, 0.9, 0.9, 0.9, []string{"par_beta"})
	Z.Ctx = ContextFeatures{ViewpointDiversity: 0.9, DirectAddress: 1.0} // .825
	Y := cand("y", "par_y", "speak", "extend", 0.3, 0.1, 0.0, 0.5, []string{"par_beta"})
	Y.Ctx = ContextFeatures{ViewpointDiversity: 0.2} // .09+.02+.03=.14
	W := cand("w", "par_w", "speak", "challenge", 0.8, 0.4, 0.2, 0.5, []string{"par_beta"})
	W.Ctx = ContextFeatures{ViewpointDiversity: 0.4} // .24+.08+.03+.02=.37

	open := Select([]Candidate{Z, Y, W}, Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.5, Weights: DefaultWeights})
	if len(open.Selected) != 1 || open.Selected[0].IntentID != "z" {
		t.Fatalf("open_floor 应仅选 Z（无正边际即停）：%+v", open.Selected)
	}
	rt := Select([]Candidate{Z, Y, W}, Policy{Mode: "roundtable", MaxSpeakers: 3, Lambda: 0.5, Weights: DefaultWeights})
	if len(rt.Selected) != 2 || rt.Selected[0].IntentID != "z" || rt.Selected[1].IntentID != "w" {
		t.Fatalf("roundtable 应 [Z, W]（challenge λ 减半翻转边际）：%+v", rt.Selected)
	}
}

// 同输入必须逐位一致（RFC-0003 3.4 确定性回归门禁的 UT 面）
func TestSelectionBitwiseDeterministic(t *testing.T) {
	A := cand("int_a", "par_alpha", "speak", "challenge", 0.9, 0.8, 0.7, 0.9, []string{"par_beta"})
	B := cand("int_b", "par_beta", "speak", "extend", 0.85, 0.5, 0.3, 0.6, []string{"par_beta"})
	C := cand("int_c", "par_gamma", "speak", "question", 0.7, 0.9, 0.5, 0.7, nil)
	policy := Policy{Mode: "open_floor", MaxSpeakers: 3, Lambda: 0.3, Weights: DefaultWeights}
	first := Select([]Candidate{A, B, C}, policy)
	for i := 0; i < 50; i++ {
		again := Select([]Candidate{A, B, C}, policy)
		if !resultsEqual(first, again) {
			t.Fatalf("第 %d 次选择结果漂移：%+v vs %+v", i, first, again)
		}
	}
}

// 平分决胜必须确定性：同分同类型 → participant_id 字典序
func TestTieBreakDeterministic(t *testing.T) {
	a := cand("i1", "par_zeta", "speak", "extend", 0.5, 0.5, 0.5, 0.5, nil)
	b := cand("i2", "par_alpha", "speak", "extend", 0.5, 0.5, 0.5, 0.5, nil)
	res := Select([]Candidate{a, b}, Policy{Mode: "open_floor", MaxSpeakers: 1, Lambda: 0.3, Weights: DefaultWeights})
	if res.Selected[0].ParticipantID != "par_alpha" {
		t.Fatalf("平分决胜应为 participant_id 字典序：%+v", res.Selected)
	}
}

func TestWeightValidation(t *testing.T) {
	if err := (Policy{Mode: "open_floor", Weights: DefaultWeights}).Validate(); err != nil {
		t.Fatalf("默认权重应合法：%v", err)
	}
	bad := DefaultWeights
	bad.Relevance = 0.7 // 单项 > 0.50
	if err := (Policy{Mode: "open_floor", Weights: bad}).Validate(); err == nil {
		t.Fatal("单项超 0.50 必须拒绝")
	}
	neg := DefaultWeights
	neg.Novelty = -0.1
	if err := (Policy{Mode: "open_floor", Weights: neg}).Validate(); err == nil {
		t.Fatal("负权重必须拒绝")
	}
	if err := (Policy{Weights: DefaultWeights}).Validate(); err == nil {
		t.Fatal("空模式必须拒绝（模式必填）")
	}
}

func TestPositiveWeightNormalizationOverOne(t *testing.T) {
	// 正值和 >1 时归一化（RFC：正值项之和归一化）；≤1 时保持字面值
	w := Weights{Relevance: 0.6, Novelty: 0.6, Diversity: 0.6, Urgency: 0.6, DirectAddress: 0.6}
	norm := NormalizeWeights(w)
	sum := norm.Relevance + norm.Novelty + norm.Diversity + norm.Urgency + norm.DirectAddress
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("归一化后正值和 = %v（期望 1）", sum)
	}
	keep := NormalizeWeights(DefaultWeights) // 和 0.90 ≤ 1 → 不动
	if keep != DefaultWeights {
		t.Fatalf("和 ≤1 不应重排：%+v", keep)
	}
}

func resultsEqual(a, b Result) bool {
	if len(a.Selected) != len(b.Selected) || a.Quiescent != b.Quiescent || a.SilentCount != b.SilentCount {
		return false
	}
	for i := range a.Selected {
		if a.Selected[i] != b.Selected[i] {
			return false
		}
	}
	return true
}
