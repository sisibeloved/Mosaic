// UT 层：策略投影与参数束（B1——RFC-0003 §3.1.7 / R-10）。
package room

import (
	"testing"

	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func validParams(mode string) protocol.PolicyParams {
	return protocol.PolicyParams{Mode: mode, MaxSpeakers: 2, Lambda: 0.3,
		Weights:        protocol.PolicyWeights(attention.DefaultWeights),
		IntentWindow:   "30s",
		ResponseCap:    600,
		RevealStrategy: "sequential"}
}

func policyEnvelope(eventID string, p protocol.PolicyParams) protocol.Envelope {
	return protocol.Envelope{EventID: eventID, TenantID: "ten_local", RoomID: "r",
		Type: protocol.EventPolicyChanged, Payload: mustJSON(p),
		Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Metadata: map[string]any{}}
}

func TestPolicyDefaultsPerMode(t *testing.T) {
	cases := map[string]struct {
		max   int
		win   string
		cap   int64
		reval string
		rebut int
		auto  int
	}{
		"open_floor": {3, "20s", 500, "simultaneous", 0, 3},
		"roundtable": {8, "30s", 600, "independent_then_cross", 1, 0},
		"deep_dive":  {2, "15s", 900, "sequential", 0, 2},
		"review":     {3, "30s", 500, "sequential", 0, 0},
		"decision":   {2, "45s", 400, "sequential", 0, 0},
	}
	def := DefaultPolicy()
	for mode, want := range cases {
		got := policyDefaults(mode)
		if got.MaxSpeakers != want.max || got.IntentWindow != want.win ||
			got.ResponseCap != want.cap || got.RevealStrategy != want.reval || got.Rebuttals != want.rebut ||
			got.AutoRounds != want.auto {
			t.Fatalf("%s 参数束不符：%+v（want %+v）", mode, got, want)
		}
	}
	if def.Params.AutoRounds != 3 {
		t.Fatalf("默认束自动续聊应为 3（RFC §3.1.7 Open Floor）：%+v", def.Params)
	}
	if def.PolicyVersion != "pol_1" || def.Params.Mode != "open_floor" {
		t.Fatalf("默认策略应为 open_floor/pol_1：%+v", def)
	}
}

func TestRebuildPolicyChainAndVersion(t *testing.T) {
	p2 := validParams("roundtable")
	p2.MaxSpeakers = 6
	p3 := validParams("deep_dive")
	got := RebuildPolicy([]protocol.Envelope{
		policyEnvelope("e1", validParams("open_floor")),
		policyEnvelope("e2", p2),
		policyEnvelope("e3", p3),
	})
	if got.Params.Mode != "deep_dive" || got.Params.MaxSpeakers != 2 || got.PolicyVersion != "pol_4" {
		t.Fatalf("链式投影失败：%+v", got)
	}

	// 损坏 payload 跳过（不致命），取最后有效值
	broken := policyEnvelope("e4", validParams("deep_dive"))
	broken.Payload = []byte(`{"mode":"deep_dive"`) // 非法 JSON
	got2 := RebuildPolicy([]protocol.Envelope{policyEnvelope("e1", validParams("roundtable")), broken})
	if got2.Params.Mode != "roundtable" || got2.PolicyVersion != "pol_2" {
		t.Fatalf("损坏 policy.changed 应跳过：%+v", got2)
	}

	// 越界 payload（max_speakers=99）同样跳过
	bad := validParams("deep_dive")
	bad.MaxSpeakers = 99
	got3 := RebuildPolicy([]protocol.Envelope{policyEnvelope("e1", validParams("roundtable")), policyEnvelope("e2", bad)})
	if got3.Params.Mode != "roundtable" {
		t.Fatalf("越界 policy.changed 应跳过：%+v", got3)
	}
}

func TestValidatePolicyParamsBounds(t *testing.T) {
	if err := ValidatePolicyParams(validParams("open_floor")); err != nil {
		t.Fatalf("合法参数被拒：%v", err)
	}
	cases := []struct {
		name   string
		mutate func(p *protocol.PolicyParams)
	}{
		{"非法模式", func(p *protocol.PolicyParams) { p.Mode = "sandbox" }},
		{"人数下界", func(p *protocol.PolicyParams) { p.MaxSpeakers = 0 }},
		{"人数上界", func(p *protocol.PolicyParams) { p.MaxSpeakers = 9 }},
		{"λ 上界", func(p *protocol.PolicyParams) { p.Lambda = 1.5 }},
		{"窗口零", func(p *protocol.PolicyParams) { p.IntentWindow = "0s" }},
		{"窗口格式", func(p *protocol.PolicyParams) { p.IntentWindow = "soon" }},
		{"cap 下界", func(p *protocol.PolicyParams) { p.ResponseCap = 50 }},
		{"reveal 非法值", func(p *protocol.PolicyParams) { p.RevealStrategy = "random" }},
		{"rebuttals 越界", func(p *protocol.PolicyParams) { p.Rebuttals = 3 }},
		{"权重越界", func(p *protocol.PolicyParams) { p.Weights.Relevance = 0.7 }},
	}
	for _, tc := range cases {
		p := validParams("open_floor")
		tc.mutate(&p)
		if err := ValidatePolicyParams(p); err == nil {
			t.Fatalf("%s 应被拒绝", tc.name)
		}
	}
}

func TestEffectiveMaxSpeakersRoundtable(t *testing.T) {
	pol := RebuildPolicy([]protocol.Envelope{policyEnvelope("e1", policyDefaults("roundtable"))})
	if got := pol.EffectiveMaxSpeakers(3); got != 3 {
		t.Fatalf("Roundtable 全员各 1：3 座位应取 3，got %d", got)
	}
	if got := pol.EffectiveMaxSpeakers(20); got != 8 {
		t.Fatalf("Roundtable 上限 8，got %d", got)
	}
	openFloor := RebuildPolicy(nil)
	if got := openFloor.EffectiveMaxSpeakers(1); got != 3 {
		t.Fatalf("Open Floor 不随座位收缩，got %d", got)
	}
}
