// UT 层：上下文组装（RFC-0007 七层最小）与预算账本（RFC-0003 §3.1.4）。
// 纯函数：同输入同输出（层摘要确定性是 Receipt 可验证性的前提）。
package contextx

import (
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func ev(typ string, seq int64, actorKind, body string, md map[string]any) protocol.Envelope {
	e := protocol.Envelope{
		EventID: typ, TenantID: "t", RoomID: "room_c", Seq: seq, Type: typ,
		SchemaVersion: 1, OccurredAt: "2026-08-29T00:00:00Z",
		Actor:      protocol.Actor{ParticipantID: "par_x", Kind: actorKind},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"body":"` + body + `"}`),
	}
	if md != nil {
		e.Metadata = md
	} else {
		e.Metadata = map[string]any{}
	}
	return e
}

func stored(events ...protocol.Envelope) []protocol.Envelope { return events }

// 七层最小：章程/参与者/刺激/近期窗口/关系/预算水位/任务指令占位——全部出现且有序
func TestAssembleSevenLayers(t *testing.T) {
	history := stored(
		ev(protocol.EventRoomCreated, 1, "human", "room", nil),
		ev(protocol.EventMessagePosted, 2, "human", "stimulus-body", nil),
	)
	cfg := Config{
		RoomID: "room_c", TaskID: "tsk_1", Mode: "open_floor",
		Seats:        []Seat{{ParticipantID: "par_a"}, {ParticipantID: "par_b"}},
		RecentWindow: 10, Budget: BudgetState{RemainingTokens: 1200, Level: 0},
	}
	assembled := Assemble(cfg, history, history[1])
	names := layerNames(assembled.Layers)
	want := []string{"charter", "participants", "stimulus", "recent_messages", "relations", "budget_watermark", "task_directive"}
	if len(names) != len(want) {
		t.Fatalf("层数 = %d（%v）", len(names), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("层 %d = %s（期望 %s）：%v", i, names[i], want[i], names)
		}
		if assembled.Layers[i].Digest == "" {
			t.Fatalf("层 %s 缺摘要", want[i])
		}
	}
	// 适配器载荷含关键内容
	if s, _ := assembled.Inline["stimulus_body"].(string); s != "stimulus-body" {
		t.Fatalf("载荷缺刺激：%v", assembled.Inline)
	}
	if _, ok := assembled.Inline["recent"]; !ok {
		t.Fatal("载荷缺近期窗口")
	}
	if assembled.Receipt.ReceiptID == "" || assembled.Receipt.Watermark != 2 {
		t.Fatalf("Receipt 不符：%+v", assembled.Receipt)
	}
}

// 层摘要确定性：同输入两次组装逐位一致（回放可验证）
func TestAssembleDeterministic(t *testing.T) {
	history := stored(
		ev(protocol.EventRoomCreated, 1, "human", "room", nil),
		ev(protocol.EventMessagePosted, 2, "agent", "a1", map[string]any{"usage": map[string]any{"output_tokens": 64}}),
	)
	cfg := Config{RoomID: "r", TaskID: "tsk", Mode: "open_floor",
		Seats: []Seat{{ParticipantID: "p"}}, RecentWindow: 5, Budget: BudgetState{RemainingTokens: 1, Level: 70}}
	a := Assemble(cfg, history, history[1])
	b := Assemble(cfg, history, history[1])
	if len(a.Layers) != len(b.Layers) {
		t.Fatal("层数漂移")
	}
	for i := range a.Layers {
		if a.Layers[i] != b.Layers[i] {
			t.Fatalf("层 %d 摘要漂移：%+v vs %+v", i, a.Layers[i], b.Layers[i])
		}
	}
}

// 近期窗口有界：只取最近 N 条消息
func TestRecentWindowBounded(t *testing.T) {
	var events []protocol.Envelope
	events = append(events, ev(protocol.EventRoomCreated, 1, "human", "room", nil))
	for i := int64(2); i <= 12; i++ {
		events = append(events, ev(protocol.EventMessagePosted, i, "agent", "m", nil))
	}
	h := stored(events...)
	cfg := Config{RoomID: "r", TaskID: "t", Mode: "open_floor", Seats: nil, RecentWindow: 3, Budget: BudgetState{}}
	assembled := Assemble(cfg, h, h[len(h)-1])
	recent, _ := assembled.Inline["recent"].([]map[string]any)
	if len(recent) != 3 {
		t.Fatalf("窗口 = %d（期望 3）", len(recent))
	}
}

// ---- 预算账本 ----

func TestBudgetRebuildFromEvents(t *testing.T) {
	events := []protocol.Envelope{
		ev(protocol.EventRoomCreated, 1, "human", "r", nil),
		ev(protocol.EventRoundOpened, 2, "system", "", nil),
		ev(protocol.EventMessagePosted, 3, "agent", "m1", map[string]any{"usage": map[string]any{"output_tokens": 100, "input_tokens": 400}}),
		ev(protocol.EventRoundOpened, 4, "system", "", nil),
		ev(protocol.EventMessagePosted, 5, "agent", "m2", map[string]any{"usage": map[string]any{"output_tokens": 50}}),
		ev(protocol.EventMessagePosted, 6, "human", "h1", nil), // 人类发言不计 agent 预算
	}
	ledger := RebuildBudget(events)
	if ledger.Rounds != 2 || ledger.Utterances != 2 {
		t.Fatalf("轮/发言数 = %d/%d（期望 2/2）", ledger.Rounds, ledger.Utterances)
	}
	if ledger.Tokens != 550 {
		t.Fatalf("tokens = %d（期望 550：input+output 之和按 output 计）", ledger.Tokens)
	}
}

func TestBudgetGradientsAndAdmission(t *testing.T) {
	limits := Limits{MaxRounds: 10, MaxUtterances: 10, MaxTokens: 1000}
	cases := []struct {
		ledger Ledger
		level  int  // 0 / 70 / 90 / 100
		admit  bool // 硬停后不再开轮
		spk    int  // 90% 降级后的建议 speaker 数
	}{
		{Ledger{Rounds: 7, Utterances: 7, Tokens: 700}, 70, true, 3},
		{Ledger{Rounds: 9, Utterances: 9, Tokens: 920}, 90, true, 2},
		{Ledger{Rounds: 10, Utterances: 10, Tokens: 1000}, 100, false, 0},
		{Ledger{Rounds: 0, Utterances: 0, Tokens: 0}, 0, true, 3},
	}
	for _, tc := range cases {
		level := tc.ledger.Level(limits)
		if level != tc.level {
			t.Fatalf("level = %d（期望 %d）：ledger %+v", level, tc.level, tc.ledger)
		}
		if got := tc.ledger.Admit(limits); got != tc.admit {
			t.Fatalf("admit = %v（期望 %v）", got, tc.admit)
		}
		if got := tc.ledger.ReducedSpeakers(limits, 3); got != tc.spk {
			t.Fatalf("降级 speakers = %d（期望 %d）", got, tc.spk)
		}
	}
}

// 对称预留：按"本轮最大发言者 × cap"预留后再判定（RFC-0003 §3.1.4）
func TestBudgetReservation(t *testing.T) {
	limits := Limits{MaxTokens: 1000}
	ledger := Ledger{Tokens: 300}
	if !ledger.ReserveOK(limits, 3, 200) { // 300+600 ≤ 1000
		t.Fatal("应可预留")
	}
	if ledger.ReserveOK(limits, 3, 250) { // 300+750 > 1000
		t.Fatal("预留超限应拒绝")
	}
}

func layerNames(layers []Layer) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = l.Name
	}
	return out
}
