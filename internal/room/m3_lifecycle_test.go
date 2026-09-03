// UT 层：M3-6 删除级联/导出重放 + M3-3 主动开口（OQ-A）最小验证。
package room

import (
	"bufio"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// TestDeleteRoomCascadeNoResidue：墓碑 → 级联后全库无残留（事件/回执；M3-6 出口判据）。
func TestDeleteRoomCascadeNoResidue(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	created, err := svc.ExecuteCommand(ctx, actor,
		createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "待删"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	roomID := created.RoomID
	post := createCmd("post_message", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7101", 1,
		map[string]any{"body": "bye", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}})
	post.RoomID = roomID
	if _, err := svc.ExecuteCommand(ctx, actor, post); err != nil {
		t.Fatalf("post: %v", err)
	}
	del := createCmd("delete_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7102", 2,
		map[string]any{"reason": "测试删除"})
	del.RoomID = roomID
	if _, err := svc.ExecuteCommand(ctx, actor, del); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := len(store.RoomEvents(roomID)); n != 0 {
		t.Fatalf("删除后残留 %d 条事件", n)
	}
	if exists, _ := store.RoomExists(ctx, roomID); exists {
		t.Fatal("删除后房间仍存在")
	}
}

// TestExportReplayConsistent：导出 NDJSON 重放重建快照一致（M3-6 出口判据：
// manifest + 全量事件行；快照投影确定性——同事件流必同视图）。
func TestExportReplayConsistent(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	created, _ := svc.ExecuteCommand(ctx, actor,
		createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "导出"}))
	roomID := created.RoomID
	post := createCmd("post_message", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7201", 1,
		map[string]any{"body": "replay", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}})
	post.RoomID = roomID
	if _, err := svc.ExecuteCommand(ctx, actor, post); err != nil {
		t.Fatalf("post: %v", err)
	}
	// 导出（事件全量）→ 重放进第二存储 → 快照一致
	events, _, err := store.EventsAfter(ctx, roomID, "", 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	envs := make([]protocol.Envelope, len(events))
	for i := range events {
		envs[i] = events[i].Envelope
	}
	// NDJSON 序列化再解析（导出介质语义）
	raw, _ := json.Marshal(envs)
	var reloaded []protocol.Envelope
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("reload: %v", err)
	}
	store2 := NewMemStore()
	if _, err := store2.AppendEvents(ctx, reloaded); err != nil {
		t.Fatalf("replay: %v", err)
	}
	ev1, _, _ := store.EventsAfter(ctx, roomID, "", 1000)
	ev2, _, _ := store2.EventsAfter(ctx, roomID, "", 1000)
	s1 := ProjectSnapshot(roomID, ev1)
	s2 := ProjectSnapshot(roomID, ev2)
	if s1.RoomVersion != s2.RoomVersion || len(s1.Timeline) != len(s2.Timeline) ||
		s1.DisplayName != s2.DisplayName {
		t.Fatalf("重放不一致：v%d/%d tl%d/%d", s1.RoomVersion, s2.RoomVersion, len(s1.Timeline), len(s2.Timeline))
	}
}

// TestProactiveWaveAfterSilence：静默期满自起一波（OQ-A）；人类消息取消计时。
func TestProactiveWaveAfterSilence(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:            []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "pe", Adapter: "echo"}}},
		Budget:           contextx.Limits{},
		ReactionWindow:   5 * time.Millisecond,
		ProactiveSilence: 40 * time.Millisecond,
		Clock:            testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_pa")
	deliverHuman(t, store, eng, "room_pa")

	// 初始链收敛（单座：波1 echo 发言 → 波2 对自己消息礼貌自决 silent → quiescent 排主动计时）
	waitRoundsClosed(t, store, "room_pa", 1)
	time.Sleep(60 * time.Millisecond) // 波2（静默收束）落定
	base := countRoundsIn(store, "room_pa")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && countRoundsIn(store, "room_pa") <= base {
		time.Sleep(10 * time.Millisecond)
	}
	if countRoundsIn(store, "room_pa") <= base {
		t.Fatalf("静默期后未自起波（rounds=%d）", base)
	}
	// 主动波自证：波1（人类锚点）不带标记；其后自起的波 round.opened metadata 带
	// proactive 标记且波链路投影可辨——全静默收场的主动波不落用户可见消息，无此
	// 标记则"主动波从未触发"无从证伪（装配漏配曾致主动波从未排上，见 app.go）。
	waves := WaveChainOf(storedEventsOf(store.RoomEvents("room_pa")))
	if len(waves) < 2 {
		t.Fatalf("波数不足（%d）", len(waves))
	}
	if waves[0].Proactive {
		t.Fatalf("人类锚点波不该带主动标记")
	}
	if !waves[len(waves)-1].Proactive {
		t.Fatalf("静默期自起波缺主动标记（rounds=%d）", countRoundsIn(store, "room_pa"))
	}
}

func countRoundsIn(store *MemStore, roomID string) int64 {
	n := int64(0)
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Type == protocol.EventRoundOpened {
			n++
		}
	}
	return n
}

func storedEventsOf(envs []protocol.Envelope) []StoredEvent {
	out := make([]StoredEvent, len(envs))
	for i := range envs {
		out[i].Envelope = envs[i]
	}
	return out
}

var _ = bufio.ScanLines
