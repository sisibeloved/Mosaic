package room

import (
	"testing"

	"github.com/sisibeloved/Mosaic/internal/attention"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func TestSpeakGatePass(t *testing.T) {
	gate := SpeakGateSettings{Enabled: true, Threshold: 0.30, CooldownPenalty: 0.20}
	high := attention.Scores{Relevance: 0.9, Urgency: 0.8}  // 0.86
	mid := attention.Scores{Relevance: 0.35, Urgency: 0.25} // 0.31（贴线过）
	low := attention.Scores{Relevance: 0.1, Urgency: 0.1}   // 0.10

	cases := []struct {
		name      string
		gate      SpeakGateSettings
		scores    attention.Scores
		act       SeatActivity
		addressed bool
		wantPass  bool
		wantRsn   string
	}{
		{"高分放行", gate, high, SeatActivity{}, false, true, ""},
		{"低分被闸", gate, low, SeatActivity{}, false, false, reasonBelowThreshold},
		{"贴线过阈", gate, mid, SeatActivity{}, false, true, ""},
		{"上波发言者高分仍过", gate, high, SeatActivity{SpokeLastWave: true}, false, true, ""},
		// raw 0.31 过阈、扣 0.20 → 0.11 掉线：归因冷却。
		{"贴线者冷却掉线归因 recent_speaker", gate, mid, SeatActivity{SpokeLastWave: true}, false, false, reasonRecentSpeaker},
		// raw 0.10 本就不足：归因 below_threshold（冷却只是雪上加霜）。
		{"低分+冷却归因 below_threshold", gate, low, SeatActivity{SpokeLastWave: true}, false, false, reasonBelowThreshold},
		{"点名直通豁免", gate, low, SeatActivity{SpokeLastWave: true}, true, true, ""},
		{"闸关全放行", SpeakGateSettings{Enabled: false}, low, SeatActivity{}, false, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := speakGatePass(tc.gate, tc.scores, tc.act, tc.addressed)
			if got.Pass != tc.wantPass || got.Reason != tc.wantRsn {
				t.Fatalf("pass=%v reason=%q score=%.2f，期望 pass=%v reason=%q",
					got.Pass, got.Reason, got.Score, tc.wantPass, tc.wantRsn)
			}
		})
	}
}

func TestSpeakGateScoreNeutralDefault(t *testing.T) {
	// 畸形自报（零值）= 0 分被闸：闸门保守语义——不确定 = 不说。
	if s := speakGateScore(attention.Scores{}); s != 0 {
		t.Fatalf("零值 scores 应 0 分，got %.2f", s)
	}
	// 权重：relevance 0.6 / urgency 0.4。
	if s := speakGateScore(attention.Scores{Relevance: 1.0}); s < 0.599 || s > 0.601 {
		t.Fatalf("权重应为 0.6/0.4，got %.2f", s)
	}
}

// msgEnv 测试用消息事件（actor kind/pid 可控）。
func msgEnv(eid, pid, kind string) protocol.Envelope {
	return protocol.Envelope{
		EventID: eid, Type: protocol.EventMessagePosted,
		Actor: protocol.Actor{ParticipantID: pid, Kind: kind},
	}
}

func roundEnv(kind string) protocol.Envelope {
	return protocol.Envelope{EventID: kind, Type: kind}
}

func TestSeatActivityOf(t *testing.T) {
	// 场景：上波（opened..closed）B 发过言；本波前（closed 后）人类发 2 条，
	// 其中锚点为最新。近窗 10 条内 B 有 1 条（上波那条不在近窗？在——总消息
	// 数 < 10 全在近窗）。
	envs := []protocol.Envelope{
		roundEnv(protocol.EventRoundOpened),
		msgEnv("m1", "par_human", "human"),
		msgEnv("m2", "par_b", "agent"),
		roundEnv(protocol.EventRoundClosed),
		msgEnv("m3", "par_human", "human"),
		msgEnv("m4", "par_human", "human"),
	}

	b := seatActivityOf(envs, "par_b")
	if !b.SpokeLastWave {
		t.Fatal("par_b 上波发言，应 SpokeLastWave=true")
	}
	// 距上次发言：m4、m3 两条在他之后 → messages_since=2。
	if b.MessagesSince != 2 {
		t.Fatalf("messages_since 期望 2，got %d", b.MessagesSince)
	}
	if b.RecentUtterances != 1 {
		t.Fatalf("近窗发言数期望 1，got %d", b.RecentUtterances)
	}

	// 人类消息只计数、不算"该座位发言"——投影是 agent 席位活动面；
	// 人类 pid 的活动恒零（无消费场景，语义如实）。
	h := seatActivityOf(envs, "par_human")
	if h.SpokeLastWave || h.RecentUtterances != 0 {
		t.Fatalf("人类发言不计 agent 活动，got %+v", h)
	}

	// 最新消息即自己 → messages_since=0。
	self := []protocol.Envelope{
		msgEnv("m1", "par_human", "human"),
		msgEnv("m2", "par_b", "agent"),
	}
	if a := seatActivityOf(self, "par_b"); a.MessagesSince != 0 {
		t.Fatalf("锚点为自己消息时 messages_since=0，got %d", a.MessagesSince)
	}

	// 从未发言：MessagesSince = -1。
	c := seatActivityOf(envs, "par_c")
	if c.SpokeLastWave || c.MessagesSince != -1 || c.RecentUtterances != 0 {
		t.Fatalf("从未发言的座位应全零/-1，got %+v", c)
	}

	// 无已收波（首个反应波）：SpokeLastWave 恒 false。
	firstWave := []protocol.Envelope{msgEnv("m1", "par_human", "human")}
	if a := seatActivityOf(firstWave, "par_b"); a.SpokeLastWave {
		t.Fatal("无 round.closed 时 SpokeLastWave 应 false")
	}
}
