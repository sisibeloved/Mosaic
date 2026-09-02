// UT 层：波链路投影（M3-1 开发者模式持久化）——重启后自事件流完整复盘波全貌：
// 意图全记录（含弃权/silent/未选理由）、发授终态（发布/撤销归账）、收波结局、
// 开波前事件忽略、未收波（崩溃在途）可辨。
package room

import (
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func waveEvent(seq int64, id, typ, actorKind, actor, payload string) StoredEvent {
	return StoredEvent{Cursor: "cur", Envelope: protocol.Envelope{
		EventID: id, RoomID: "r", Seq: seq, Type: typ, OccurredAt: "t",
		Actor:   protocol.Actor{ParticipantID: actor, Kind: actorKind},
		Payload: []byte(payload),
	}}
}

// TestWaveChainRebuildsFullWave 两波完整链路：波1 published（双意图/发授/发布/撤销），
// 波2 quiescent（silent 意图）。开波前事件忽略。
func TestWaveChainRebuildsFullWave(t *testing.T) {
	events := []StoredEvent{
		waveEvent(1, "e1", protocol.EventRoomCreated, "human", "o", `{}`),
		waveEvent(2, "e2", protocol.EventMessagePosted, "human", "o", `{"body":"哈喽"}`),
		// 波1：双意图（codex 意愿 + kimi silent）→ 双发授（kimi 撤销 / codex 发布）→ 收波
		waveEvent(3, "e3", protocol.EventRoundOpened, "system", "par_system", `{"round_id":"rnd_1","stimulus_event_id":"e2"}`),
		waveEvent(4, "e4", protocol.EventIntentRecorded, "agent", "par_codex",
			`{"intent_id":"int_1","participant_id":"par_codex","action":"speak","type":"answer","public_rationale":"补充上下文","score_band":"medium","selected":true}`),
		waveEvent(5, "e5", protocol.EventIntentRecorded, "agent", "par_kimi",
			`{"intent_id":"int_2","participant_id":"par_kimi","action":"silent","public_rationale":"已有人说","score_band":"unranked","selected":false,"unselected_reason":""}`),
		waveEvent(6, "e6", protocol.EventFloorGranted, "system", "par_system",
			`{"grant_id":"g1","round_id":"rnd_1","participant_id":"par_codex","rank":1}`),
		waveEvent(7, "e7", protocol.EventFloorGranted, "system", "par_system",
			`{"grant_id":"g2","round_id":"rnd_1","participant_id":"par_kimi","rank":2}`),
		waveEvent(8, "e8", protocol.EventFloorRevoked, "system", "par_system",
			`{"grant_id":"g2","reason":"generation_failed"}`),
		waveEvent(9, "e9", protocol.EventMessagePosted, "agent", "par_codex", `{"body":"答"}`),
		waveEvent(10, "e10", protocol.EventRoundClosed, "system", "par_system",
			`{"round_id":"rnd_1","outcome":"published","selected_count":1,"silent_count":1}`),
		// 波2：quiescent（单 silent 意图）
		waveEvent(11, "e11", protocol.EventRoundOpened, "system", "par_system", `{"round_id":"rnd_2","stimulus_event_id":"e9"}`),
		waveEvent(12, "e12", protocol.EventIntentRecorded, "agent", "par_kimi",
			`{"intent_id":"int_3","participant_id":"par_kimi","action":"silent","score_band":"unranked","selected":false,"unselected_reason":"budget"}`),
		waveEvent(13, "e13", protocol.EventRoundClosed, "system", "par_system",
			`{"round_id":"rnd_2","outcome":"quiescent","selected_count":0,"silent_count":1}`),
	}

	waves := WaveChainOf(events)
	if len(waves) != 2 {
		t.Fatalf("waves = %d, want 2（开波前事件不计波）", len(waves))
	}

	w1 := waves[0]
	if w1.RoundID != "rnd_1" || w1.StimulusEventID != "e2" || w1.OpenedSeq != 3 ||
		w1.ClosedSeq != 10 || w1.Outcome != "published" || w1.Published != 1 || w1.SilentCount != 1 {
		t.Fatalf("波1 骨架 = %+v", w1)
	}
	if len(w1.Intents) != 2 {
		t.Fatalf("波1 意图数 = %d, want 2（R-01 全记录）", len(w1.Intents))
	}
	if in := w1.Intents[0]; in.ParticipantID != "par_codex" || !in.Selected || in.ScoreBand != "medium" {
		t.Fatalf("波1 意图1 = %+v", in)
	}
	if in := w1.Intents[1]; in.Action != "silent" || in.Selected {
		t.Fatalf("波1 意图2 = %+v", in)
	}
	if len(w1.Grants) != 2 {
		t.Fatalf("波1 发授数 = %d, want 2", len(w1.Grants))
	}
	if g := w1.Grants[0]; g.GrantID != "g1" || !g.Published || g.Revoked {
		t.Fatalf("发授 g1 终态 = %+v（应已发布）", g)
	}
	if g := w1.Grants[1]; g.GrantID != "g2" || g.Published || !g.Revoked || g.RevokeReason != "generation_failed" {
		t.Fatalf("发授 g2 终态 = %+v（应已撤销）", g)
	}

	w2 := waves[1]
	if w2.RoundID != "rnd_2" || w2.Outcome != "quiescent" || w2.Published != 0 {
		t.Fatalf("波2 骨架 = %+v", w2)
	}
	if len(w2.Intents) != 1 || w2.Intents[0].UnselectedReason != "budget" {
		t.Fatalf("波2 意图 = %+v", w2.Intents)
	}
}

// TestWaveChainUnclosedWave 崩溃/在途波：round.opened 后无 round.closed——
// ClosedSeq=0、Outcome 空（复盘时可辨"未收波"而非数据丢失）。
func TestWaveChainUnclosedWave(t *testing.T) {
	events := []StoredEvent{
		waveEvent(1, "e1", protocol.EventRoundOpened, "system", "par_system", `{"round_id":"rnd_x","stimulus_event_id":"e0"}`),
		waveEvent(2, "e2", protocol.EventIntentRecorded, "agent", "par_codex",
			`{"intent_id":"int_1","participant_id":"par_codex","action":"speak","score_band":"high","selected":true}`),
	}
	waves := WaveChainOf(events)
	if len(waves) != 1 {
		t.Fatalf("waves = %d, want 1", len(waves))
	}
	if waves[0].ClosedSeq != 0 || waves[0].Outcome != "" {
		t.Fatalf("未收波应 ClosedSeq=0/Outcome 空：%+v", waves[0])
	}
	if len(waves[0].Intents) != 1 {
		t.Fatalf("未收波意图也应投影：%+v", waves[0])
	}
}

// TestWaveChainEmpty 无波历史（纯人类消息）→ 空链不 panic。
func TestWaveChainEmpty(t *testing.T) {
	events := []StoredEvent{
		waveEvent(1, "e1", protocol.EventRoomCreated, "human", "o", `{}`),
		waveEvent(2, "e2", protocol.EventMessagePosted, "human", "o", `{"body":"hi"}`),
	}
	if waves := WaveChainOf(events); len(waves) != 0 {
		t.Fatalf("waves = %d, want 0", len(waves))
	}
}
