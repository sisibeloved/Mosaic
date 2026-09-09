// UT 层：RoomSummary.Agents roster 投影摘要（v1.72 侧栏私聊/群聊分型依据）——
// 四形态：显式多员（建房间物化快照）/邀请追加/单员（私聊判定基础）/旧房间历史
// 推导/无 agent 历史回退 nil（全席，归群聊组）。MemStore 经 RosterOf 同源投影。
package room

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func mkEnvelope(id, roomID string, typ string, actor protocol.Actor, payload string) protocol.Envelope {
	return protocol.Envelope{
		EventID: id, TenantID: "ten_local", RoomID: roomID, Type: typ, SchemaVersion: 1,
		OccurredAt: "2026-09-09T10:00:00.000Z",
		Actor:      actor,
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    json.RawMessage(payload), Metadata: map[string]any{},
	}
}

func TestListRoomsAgentsRosterProjection(t *testing.T) {
	store := NewMemStore()
	human := protocol.Actor{ParticipantID: "par_owner", Kind: "human"}
	agentKimi := protocol.Actor{ParticipantID: "par_kimi_x", Kind: "agent"}
	ctx := context.Background()

	seed := []protocol.Envelope{
		// room_exp：显式两名（建房物化快照）
		mkEnvelope("evt_e1", "room_exp", protocol.EventRoomCreated, human, `{"display_name":"双员房","agents":["par_kimi_x","par_codex_x"]}`),
		// room_dm：单名（私聊判定）
		mkEnvelope("evt_d1", "room_dm", protocol.EventRoomCreated, human, `{"display_name":"Kimi","agents":["par_kimi_x"]}`),
		// room_inv：建房间单名 + 邀请第二名（admitted 链）
		mkEnvelope("evt_i1", "room_inv", protocol.EventRoomCreated, human, `{"display_name":"邀请房","agents":["par_kimi_x"]}`),
		mkEnvelope("evt_i2", "room_inv", protocol.EventParticipantAdmitted, human, `{"participant_id":"par_codex_x","invited_by":"par_owner"}`),
		// room_old：旧房间（created 无 agents 载荷）→ 历史推导（agent actor + 意向载荷）
		mkEnvelope("evt_o1", "room_old", protocol.EventRoomCreated, human, `{"display_name":"旧群聊"}`),
		mkEnvelope("evt_o2", "room_old", protocol.EventMessagePosted, agentKimi, `{"body":"老消息"}`),
		mkEnvelope("evt_o3", "room_old", protocol.EventIntentRecorded, human, `{"participant_id":"par_codex_x"}`),
		// room_legacy：旧空转房（无 agents 载荷、无 agent 历史）→ nil = 全席
		mkEnvelope("evt_l1", "room_legacy", protocol.EventRoomCreated, human, `{"display_name":"旧空房"}`),
		mkEnvelope("evt_l2", "room_legacy", protocol.EventMessagePosted, human, `{"body":"只有人类发言"}`),
	}
	if _, err := store.AppendEvents(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rooms, err := store.ListRooms(ctx)
	if err != nil || len(rooms) != 5 {
		t.Fatalf("五房间：%v %+v", err, rooms)
	}
	byID := map[string]RoomSummary{}
	for _, r := range rooms {
		byID[r.RoomID] = r
	}
	want := map[string][]string{
		"room_exp": {"par_codex_x", "par_kimi_x"}, // 排序确定性
		"room_dm":  {"par_kimi_x"},
		"room_inv": {"par_codex_x", "par_kimi_x"},
		"room_old": {"par_codex_x", "par_kimi_x"},
	}
	for roomID, agents := range want {
		got := byID[roomID].Agents
		if !reflect.DeepEqual(got, agents) {
			t.Fatalf("%s agents 应 %v，got %v", roomID, agents, got)
		}
	}
	if byID["room_legacy"].Agents != nil {
		t.Fatalf("无 agent 历史的旧房应 nil（全席），got %v", byID["room_legacy"].Agents)
	}
}
