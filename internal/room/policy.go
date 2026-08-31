// Package room 的策略投影与参数束（RFC-0003 §3.1.7 / R-10）。
// Policy 是事件溯源的：默认束起步，set_policy → policy.changed 链重建当前值；
// 变更只在 round 边界生效（引擎开轮时自历史重建，不做热调）。
package room

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// 模式默认参数束（RFC-0003 §3.1.7 表；reveal 默认随 B2 执行面落地对齐：
// Open Floor=simultaneous、Roundtable=independent_then_cross（rebuttals=1）、
// Deep Dive/Review/Decision=sequential）。Roundtable"全员各 1"以数值上限（8）
// 近似，座位数取 min 在引擎内生效；自动续聊参数不随 B2 落地（轮次自驱动
// 语义，登记至后续切片）。
func policyDefaults(mode string) protocol.PolicyParams {
	p := protocol.PolicyParams{
		Mode:           "open_floor",
		MaxSpeakers:    3,
		Lambda:         0.30,
		Weights:        policyWeightsOf(attention.DefaultWeights),
		IntentWindow:   "20s",
		ResponseCap:    500,
		RevealStrategy: "simultaneous",
		Rebuttals:      0,
	}
	switch mode {
	case "roundtable":
		p.Mode = "roundtable"
		p.MaxSpeakers = 8
		p.IntentWindow = "30s"
		p.ResponseCap = 600
		p.RevealStrategy = "independent_then_cross"
		p.Rebuttals = 1
	case "deep_dive":
		p.Mode = "deep_dive"
		p.MaxSpeakers = 2
		p.IntentWindow = "15s"
		p.ResponseCap = 900
		p.RevealStrategy = "sequential"
	case "review":
		p.Mode = "review"
		p.IntentWindow = "30s"
		p.ResponseCap = 500
		p.RevealStrategy = "sequential"
	case "decision":
		p.Mode = "decision"
		p.MaxSpeakers = 2
		p.IntentWindow = "45s"
		p.ResponseCap = 400
		p.RevealStrategy = "sequential"
	case "open_floor":
	default:
		p.Mode = mode // 非法模式由 ValidatePolicyParams 拒
	}
	return p
}

func policyWeightsOf(w attention.Weights) protocol.PolicyWeights {
	return protocol.PolicyWeights(w)
}

// DefaultPolicy 房间起步策略（open_floor 默认束，pol_1）。
func DefaultPolicy() RoundPolicy {
	return roundPolicyOf(policyDefaults("open_floor"), 0)
}

// RoundPolicy 轮次边界生效的策略快照（含派生量）。
type RoundPolicy struct {
	Params        protocol.PolicyParams
	IntentWindow  time.Duration
	PolicyVersion string // pol_1 起；每次 policy.changed 递增
}

// ValidatePolicyParams 配置边界（模式枚举沿用 attention；窗口 1s..10m；
// response cap 100..4000；reveal 本切片只放行 sequential——可执行面之外不收配置，
// 防"参数被接受但从未生效"）。
func ValidatePolicyParams(p protocol.PolicyParams) error {
	w := attention.NormalizeWeights(attention.Weights(p.Weights))
	if err := (attention.Policy{
		Mode: p.Mode, MaxSpeakers: p.MaxSpeakers, Lambda: p.Lambda, Weights: w,
	}).Validate(); err != nil {
		return err
	}
	if p.MaxSpeakers < 1 || p.MaxSpeakers > 8 {
		return fmt.Errorf("max_speakers 越界 [1,8]")
	}
	d, err := time.ParseDuration(p.IntentWindow)
	if err != nil || d < time.Second || d > 10*time.Minute {
		return fmt.Errorf("intent_window 须为 1s..10m")
	}
	if p.ResponseCap < 100 || p.ResponseCap > 4000 {
		return fmt.Errorf("response_cap 越界 [100,4000]")
	}
	switch p.RevealStrategy {
	case "sequential", "simultaneous", "independent_then_cross":
	default:
		return fmt.Errorf("reveal_strategy %q 非法", p.RevealStrategy)
	}
	if p.Rebuttals < 0 || p.Rebuttals > 2 {
		return fmt.Errorf("rebuttals 越界 [0,2]（RFC §3.1.7：Roundtable 可配 0-2）")
	}
	return nil
}

func roundPolicyOf(p protocol.PolicyParams, changes int) RoundPolicy {
	p.Weights = policyWeightsOf(attention.NormalizeWeights(attention.Weights(p.Weights)))
	d, _ := time.ParseDuration(p.IntentWindow)
	return RoundPolicy{
		Params:        p,
		IntentWindow:  d,
		PolicyVersion: fmt.Sprintf("pol_%d", changes+1),
	}
}

// RebuildPolicy 自事件链重建当前策略（默认束 + 逐次 policy.changed 覆盖）。
// 损坏的 policy.changed payload 不致命：该次变更跳过、取最后有效值
// （事件已入库，投影端容错优于整房间不可用）。
func RebuildPolicy(envs []protocol.Envelope) RoundPolicy {
	changes := 0
	policy := policyDefaults("open_floor")
	for _, env := range envs {
		if env.Type != protocol.EventPolicyChanged {
			continue
		}
		var params protocol.PolicyParams // payload 平铺（policy_version 由本投影按计数派生）
		if err := json.Unmarshal(env.Payload, &params); err != nil {
			continue
		}
		if err := ValidatePolicyParams(params); err != nil {
			continue
		}
		policy = params
		changes++
	}
	return roundPolicyOf(policy, changes)
}

// EffectiveMaxSpeakers Roundtable"全员各 1"的座位数近似（数值上限内取 min）。
func (p RoundPolicy) EffectiveMaxSpeakers(seats int) int {
	if p.Params.Mode == "roundtable" && seats > 0 && seats < p.Params.MaxSpeakers {
		return seats
	}
	return p.Params.MaxSpeakers
}

// ToAttention 引擎选择器视角的策略。
func (p RoundPolicy) ToAttention() attention.Policy {
	return attention.Policy{
		Mode:        p.Params.Mode,
		MaxSpeakers: p.Params.MaxSpeakers,
		Lambda:      p.Params.Lambda,
		Weights:     attention.Weights(p.Params.Weights),
	}
}
