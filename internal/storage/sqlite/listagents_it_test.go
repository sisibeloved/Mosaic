// IT 层：ListRooms 的 agents roster 聚合（v1.72）——SQL 分层与 room.RosterOf 同语义：
// 显式（created 物化快照 + admitted 链）优先；旧房间历史推导（agent actor +
// intent/grant 载荷）；两者皆无 → NULL = 全席。与 MemStore 投影用例同构对齐。
//go:build it

package sqlite

import (
	"context"
	"reflect"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func TestListRoomsAgents_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	human := protocol.Actor{ParticipantID: "par_owner", Kind: "human"}
	agentKimi := protocol.Actor{ParticipantID: "par_kimi_x", Kind: "agent"}
	mk := func(id, roomID, typ string, actor protocol.Actor, payload string) protocol.Envelope {
		return protocol.Envelope{
			EventID: id, TenantID: "ten_local", RoomID: roomID, Type: typ, SchemaVersion: 1,
			OccurredAt: "2026-09-09T10:00:00.000Z",
			Actor:      actor,
			Visibility: protocol.Visibility{Kind: "public"},
			Payload:    []byte(payload), Metadata: map[string]any{},
		}
	}
	seed := []protocol.Envelope{
		mk("evt_a1", "room_exp", protocol.EventRoomCreated, human, `{"display_name":"双员房","agents":["par_kimi_x","par_codex_x"]}`),
		mk("evt_d1", "room_dm", protocol.EventRoomCreated, human, `{"display_name":"Kimi","agents":["par_kimi_x"]}`),
		mk("evt_i1", "room_inv", protocol.EventRoomCreated, human, `{"display_name":"邀请房","agents":["par_kimi_x"]}`),
		mk("evt_i2", "room_inv", protocol.EventParticipantAdmitted, human, `{"participant_id":"par_codex_x","invited_by":"par_owner"}`),
		// 旧房间：无 agents 载荷 → 历史推导（agent actor + intent 载荷目标）
		mk("evt_o1", "room_old", protocol.EventRoomCreated, human, `{"display_name":"旧群聊"}`),
		mk("evt_o2", "room_old", protocol.EventMessagePosted, agentKimi, `{"body":"老消息"}`),
		mk("evt_o3", "room_old", protocol.EventIntentRecorded, human, `{"participant_id":"par_codex_x"}`),
		// 显式优先验证：room_pref 既有物化快照又有历史噪音——快照为准（par_noise 不入列）
		mk("evt_p1", "room_pref", protocol.EventRoomCreated, human, `{"display_name":"显式优先","agents":["par_kimi_x"]}`),
		mk("evt_p2", "room_pref", protocol.EventMessagePosted,
			protocol.Actor{ParticipantID: "par_noise", Kind: "agent"}, `{"body":"历史噪音"}`),
		// 旧空转房：无 agent 历史 → nil = 全席
		mk("evt_l1", "room_legacy", protocol.EventRoomCreated, human, `{"display_name":"旧空房"}`),
		mk("evt_l2", "room_legacy", protocol.EventMessagePosted, human, `{"body":"只有人类发言"}`),
	}
	for _, env := range seed {
		if _, err := store.AppendEvents(ctx, []protocol.Envelope{env}); err != nil {
			t.Fatalf("append %s: %v", env.EventID, err)
		}
	}

	rooms, err := store.ListRooms(ctx)
	if err != nil || len(rooms) != 6 {
		t.Fatalf("六房间：%v %+v", err, rooms)
	}
	byID := map[string]room.RoomSummary{}
	for _, r := range rooms {
		byID[r.RoomID] = r
	}
	want := map[string][]string{
		"room_exp":  {"par_codex_x", "par_kimi_x"}, // GROUP_CONCAT 拆分后排序确定性
		"room_dm":   {"par_kimi_x"},
		"room_inv":  {"par_codex_x", "par_kimi_x"},
		"room_old":  {"par_codex_x", "par_kimi_x"},
		"room_pref": {"par_kimi_x"},
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
