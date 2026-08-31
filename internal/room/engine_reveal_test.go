// UT 层：揭示三策略执行面（B2——RFC-0003 §3.1.8）。
// simultaneous：全部获选者同一冻结水位发授，正文在生成完成后统一揭示；
// independent_then_cross：独立首轮（simultaneous）+ cross 子轮（复用完整
// Intent→Floor 路径，参与资格限本轮已发言者，round.closed 计 cross_subrounds）。
package room

import (
	"context"
	"encoding/json"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// twoSeatEcho 装配两个 echo 座位（不同 profile 键），确保一轮有双候选可揭示。
func revealTestEngine(t *testing.T) (*MemStore, *Engine, string) {
	t.Helper()
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	t.Cleanup(sup.Shutdown)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_a", Profile: agent.Profile{ProfileID: "pa", Adapter: "echo"}},
			{ParticipantID: "par_b", Profile: agent.Profile{ProfileID: "pb", Adapter: "echo"}},
		},
		Budget: contextx.Limits{},
		Clock:  testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "ev0", TenantID: "ten_local", RoomID: "room_rv", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	return store, eng, "room_rv"
}

func setRevealPolicy(t *testing.T, store *MemStore, reveal string, rebuttals int) {
	t.Helper()
	p := policyDefaults("open_floor")
	p.RevealStrategy = reveal
	p.Rebuttals = rebuttals
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "ev_pol_rv", TenantID: "ten_local", RoomID: "room_rv", Type: protocol.EventPolicyChanged,
			Actor:   protocol.Actor{ParticipantID: "o", Kind: "human"},
			Payload: mustMarshalForTest(p), Metadata: map[string]any{}},
	})
}

// subroundOf metadata 子轮标记（MemStore 内存 int / SQLite JSON float64 双形态）。
func subroundOf(md map[string]any) int {
	switch v := md["subround"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

func grantsAndMessages(events []protocol.Envelope) (grants []protocol.FloorGrantedPayload, agentMsgs int, crossGrants int) {
	for _, ev := range events {
		switch ev.Type {
		case protocol.EventFloorGranted:
			var g protocol.FloorGrantedPayload
			_ = json.Unmarshal(ev.Payload, &g)
			grants = append(grants, g)
			if sr := subroundOf(ev.Metadata); sr > 0 {
				crossGrants++
			}
		case protocol.EventMessagePosted:
			if ev.Actor.Kind == "agent" {
				agentMsgs++
			}
		}
	}
	return
}

// TestRevealSimultaneousFrozenWatermark：默认 open_floor（simultaneous）——
// 双获选者 grant 的 context_watermark 相同（冻结），且全部发授先于任何正文。
func TestRevealSimultaneousFrozenWatermark(t *testing.T) {
	store, eng, roomID := revealTestEngine(t)
	deliverHuman(t, store, eng, roomID)
	waitRoundClosed(t, store, roomID)

	events := store.RoomEvents(roomID)
	grants, agentMsgs, crossGrants := grantsAndMessages(events)
	if len(grants) != 2 || agentMsgs != 2 || crossGrants != 0 {
		t.Fatalf("双座 simultaneous 应 2 授 2 文 0 cross：%+v", typesOf(events))
	}
	if grants[0].ContextWatermark != grants[1].ContextWatermark {
		t.Fatalf("simultaneous 授予水位应冻结一致：%d vs %d", grants[0].ContextWatermark, grants[1].ContextWatermark)
	}
	// 发授全部先于正文（统一揭示）：最后一个 grant 的 seq < 第一条 agent 正文 seq
	lastGrantSeq, firstMsgSeq := int64(0), int64(1<<62)
	for _, ev := range events {
		switch {
		case ev.Type == protocol.EventFloorGranted:
			lastGrantSeq = ev.Seq
		case ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent":
			if ev.Seq < firstMsgSeq {
				firstMsgSeq = ev.Seq
			}
		}
	}
	if lastGrantSeq > firstMsgSeq {
		t.Fatalf("simultaneous 应先全部发授再揭示：lastGrant=%d firstMsg=%d", lastGrantSeq, firstMsgSeq)
	}
}

// TestRevealSequentialInterleaved：sequential——授予水位随前序发布推进（不冻结）。
func TestRevealSequentialInterleaved(t *testing.T) {
	store, eng, roomID := revealTestEngine(t)
	setRevealPolicy(t, store, "sequential", 0)
	deliverHuman(t, store, eng, roomID)
	waitRoundClosed(t, store, roomID)

	events := store.RoomEvents(roomID)
	grants, agentMsgs, _ := grantsAndMessages(events)
	if len(grants) != 2 || agentMsgs != 2 {
		t.Fatalf("双座 sequential 应 2 授 2 文：%v", typesOf(events))
	}
	if grants[0].ContextWatermark == grants[1].ContextWatermark {
		t.Fatalf("sequential 授予水位应随发布推进（rank2 晚于 rank1 的正文）")
	}
}

// TestRevealIndependentThenCross：ITC——独立首轮（冻结）后 cross 子轮（复用完整
// 路径），round.closed.cross_subrounds 计数，cross 授予带 subround 标记。
func TestRevealIndependentThenCross(t *testing.T) {
	store, eng, roomID := revealTestEngine(t)
	setRevealPolicy(t, store, "independent_then_cross", 1)
	deliverHuman(t, store, eng, roomID)
	waitRoundClosed(t, store, roomID)

	events := store.RoomEvents(roomID)
	grants, agentMsgs, crossGrants := grantsAndMessages(events)
	if agentMsgs < 3 { // 首轮 2 + cross ≥1（echo 恒发言）
		t.Fatalf("ITC 应有 cross 发言（首轮 2 + cross ≥1）：%v", typesOf(events))
	}
	if crossGrants == 0 {
		t.Fatal("cross 子轮应有带 subround 标记的授予")
	}
	var closed protocol.RoundClosedPayload
	for _, ev := range events {
		if ev.Type == protocol.EventRoundClosed {
			_ = json.Unmarshal(ev.Payload, &closed)
		}
	}
	if closed.CrossSubrounds != 1 {
		t.Fatalf("round.closed.cross_subrounds = %d（期望 1）", closed.CrossSubrounds)
	}
	// cross 参与资格限首轮已发言者：授予参与者 ⊆ {par_a, par_b}
	for _, g := range grants {
		if g.ParticipantID != "par_a" && g.ParticipantID != "par_b" {
			t.Fatalf("cross 授予了非本轮发言者：%s", g.ParticipantID)
		}
	}
}
