// UT 层：M3-5 Evidence Request——创建/解决/驳回/重复解决拒绝/投影重建。
package room

import (
	"context"
	"encoding/json"
	"testing"
)

var roomID = "room_er"

func evCmd(t *testing.T, kind string, version int64, idem string, payload string) Command {
	t.Helper()
	cmd := createCmd(kind, idem, version, json.RawMessage(payload))
	cmd.RoomID = roomID
	cmd.IssuedAt = "2026-09-02T03:00:00.000Z"
	return cmd
}

func TestEvidenceRequestLifecycle(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	// 种子房间（created v1；拿实际 room id）
	created, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", validUUIDv7, 0, map[string]any{"display_name": "er"}))
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	roomID = created.RoomID

	// 创建
	res, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		evCmd(t, "create_evidence_request", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7001",
			`{"question":"需要并发 A/B 三轮数据","required_evidence":["benchmark"],"acceptance_criteria":"相同环境三轮","owners":[],"reopen_thread_on_resolution":true}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = res
	reqs := EvidenceRequestsOf(storedOf(store.RoomEvents(roomID)))
	if len(reqs) != 1 || reqs[0].Status != "open" || !reqs[0].ReopenOnResolution {
		t.Fatalf("创建后投影 = %+v", reqs)
	}
	reqID := reqs[0].RequestID

	// resolved 必须带 refs
	if _, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		evCmd(t, "resolve_evidence_request", 2, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7002",
			`{"request_id":"`+reqID+`","resolution":"resolved"}`)); err == nil {
		t.Fatal("resolved 无 refs 应拒绝")
	}

	// 正常解决
	if _, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		evCmd(t, "resolve_evidence_request", 2, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7002",
			`{"request_id":"`+reqID+`","resolution":"resolved","evidence_refs":["evt_ab1","evt_ab2"]}`)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reqs = EvidenceRequestsOf(storedOf(store.RoomEvents(roomID)))
	if reqs[0].Status != "resolved" || len(reqs[0].EvidenceRefs) != 2 {
		t.Fatalf("解决后投影 = %+v", reqs)
	}

	// 重复解决拒绝
	if _, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		evCmd(t, "resolve_evidence_request", 3, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7003",
			`{"request_id":"`+reqID+`","resolution":"dismissed"}`)); err == nil {
		t.Fatal("已终态不得重复解决")
	}
}
