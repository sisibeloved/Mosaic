package room

import (
	"strconv"
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

// anchorTaskAssignees 锚点指派解析（豁免面补全 2026-09-18）：开口指派直通、
// 人类锚点/完成项/自领项不算、解析回退与 tasklist 归属同源。
func TestAnchorTaskAssignees(t *testing.T) {
	created := protocol.Envelope{
		EventID: "c1", Type: protocol.EventRoomCreated,
		Actor:   protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload: []byte(`{"agents":["par_aa","par_bb"]}`),
	}
	decl := func(eid, pid, body string) protocol.Envelope {
		return protocol.Envelope{
			EventID: eid, Type: protocol.EventMessagePosted,
			Actor:   protocol.Actor{ParticipantID: pid, Kind: "agent"},
			Payload: []byte(`{"body":` + strconv.Quote(body) + `}`),
		}
	}
	hist := []protocol.Envelope{created}

	// 开口指派 → 负责人直通；未提及的自领项不算定向。
	anchor := decl("m1", "par_aa", "安排一下\n```mosaic-todo\n- [ ] @par_bb 交付方案\n- [ ] 我自己跟进\n```")
	got := anchorTaskAssignees(anchor, append(hist, anchor))
	if !got["par_bb"] || len(got) != 1 {
		t.Fatalf("开口指派应解析出 par_bb（自领项不算），got %v", got)
	}

	// 完成项不算指派。
	done := decl("m2", "par_aa", "```mosaic-todo\n- [x] @par_bb 交付方案\n```")
	if got := anchorTaskAssignees(done, append(hist, done)); len(got) != 0 {
		t.Fatalf("完成项不应产生豁免，got %v", got)
	}

	// 人类锚点跳过（人类待办不派生，与 TasksOf 同纪律）。
	human := protocol.Envelope{
		EventID: "m3", Type: protocol.EventMessagePosted,
		Actor:   protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload: []byte(`{"body":` + strconv.Quote("```mosaic-todo\n- [ ] @par_bb 交付方案\n```") + `}`),
	}
	if got := anchorTaskAssignees(human, append(hist, human)); len(got) != 0 {
		t.Fatalf("人类锚点不应解析指派，got %v", got)
	}

	// @指派解析回退与 tasklist 同源：不可解析的 mention → 申报人自领。
	unresolved := decl("m4", "par_aa", "```mosaic-todo\n- [ ] @nobody 事项\n```")
	if got := anchorTaskAssignees(unresolved, append(hist, unresolved)); !got["par_aa"] {
		t.Fatalf("不可解析 mention 应回退申报人（与 resolveMention 同源），got %v", got)
	}

	// 分段匹配（@bb 命中 par_bb 下划线分段，agent 互见的正是这类 id）。
	seg := decl("m5", "par_aa", "```mosaic-todo\n- [ ] @bb 事项\n```")
	if got := anchorTaskAssignees(seg, append(hist, seg)); !got["par_bb"] {
		t.Fatalf("分段匹配应命中 par_bb，got %v", got)
	}
}
