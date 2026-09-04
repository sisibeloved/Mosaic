// M3 完成度补齐 UT（v1.54）：M3-5 认领命令链（evidence_request.claimed）+
// M3-4 Claim 认知账本投影（ClaimsOf）。
package room

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ---- M3-5 认领 ----

func TestEvidenceClaimChain(t *testing.T) {
	svc, store := newTaskTestService(t)
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	seed := []protocol.Envelope{
		{EventID: "evt_ec_r1", TenantID: "ten_t", RoomID: "room_t", Seq: 1, Type: protocol.EventRoomCreated,
			SchemaVersion: 1, OccurredAt: "2026-09-04T00:00:00Z", Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"}, Payload: mustJSON(map[string]any{"display_name": "T", "thread_id": "thr_1", "agents": []string{}}), Metadata: map[string]any{}},
		{EventID: "evt_ec_c1", TenantID: "ten_t", RoomID: "room_t", Seq: 2, Type: protocol.EventEvidenceRequestCreated,
			SchemaVersion: 1, OccurredAt: "2026-09-04T00:00:00Z", Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"},
			Payload: mustJSON(protocol.EvidenceRequestCreatedPayload{
				RequestID: "ereq_ec1", Question: "GPT-5 的 GA 日期是哪天？", Owners: []string{},
			}), Metadata: map[string]any{}},
	}
	if _, err := store.AppendEvents(ctx, seed); err != nil {
		t.Fatal(err)
	}
	v, _ := store.RoomVersion(ctx, "room_t")
	mk := func(kind, payload string) Command {
		return Command{RoomID: "room_t", CommandKind: kind, ExpectedRoomVersion: v,
			IdempotencyKey: m33UUID(), IssuedAt: "2026-09-04T00:00:00Z", Payload: json.RawMessage(payload)}
	}

	// 不存在的需求单 → 拒绝
	if _, err := svc.ExecuteCommand(ctx, actor, mk("claim_evidence_request", `{"request_id":"ereq_nope"}`)); err == nil {
		t.Fatal("不存在需求单应拒绝")
	}

	// 认领成功（owner 追加；命令面仅 human/system——agent 自主认领随工具面分期）
	res, err := svc.ExecuteCommand(ctx, actor, mk("claim_evidence_request", `{"request_id":"ereq_ec1","note":"让 kimi 抓官方页"}`))
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	if res.Replayed {
		t.Fatal("首次认领非回放")
	}
	reqs := EvidenceRequestsOf(eventsOfStore(t, store))
	if len(reqs) != 1 || len(reqs[0].Owners) != 1 || reqs[0].Owners[0] != "par_owner" {
		t.Fatalf("认领后 owners 应含认领人：%+v", reqs)
	}

	// 同人重复认领 → 拒绝
	v2, _ := store.RoomVersion(ctx, "room_t")
	dup := Command{RoomID: "room_t", CommandKind: "claim_evidence_request", ExpectedRoomVersion: v2,
		IdempotencyKey: m33UUID(), IssuedAt: "2026-09-04T00:00:00Z", Payload: json.RawMessage(`{"request_id":"ereq_ec1"}`)}
	if _, err := svc.ExecuteCommand(ctx, actor, dup); err == nil {
		t.Fatal("重复认领应拒绝")
	}

	// 终态后认领 → 拒绝
	v3, _ := store.RoomVersion(ctx, "room_t")
	if _, err := svc.ExecuteCommand(ctx, actor, Command{RoomID: "room_t", CommandKind: "resolve_evidence_request",
		ExpectedRoomVersion: v3, IdempotencyKey: m33UUID(), IssuedAt: "2026-09-04T00:00:00Z",
		Payload: json.RawMessage(`{"request_id":"ereq_ec1","resolution":"resolved","evidence_refs":["evt_x1"]}`)}); err != nil {
		t.Fatalf("解决失败: %v", err)
	}
	v4, _ := store.RoomVersion(ctx, "room_t")
	late := Command{RoomID: "room_t", CommandKind: "claim_evidence_request", ExpectedRoomVersion: v4,
		IdempotencyKey: m33UUID(), IssuedAt: "2026-09-04T00:00:00Z", Payload: json.RawMessage(`{"request_id":"ereq_ec1"}`)}
	if _, err := svc.ExecuteCommand(ctx, actor, late); err == nil {
		t.Fatal("终态后认领应拒绝")
	}

	// 投影幂等：同一 claimed 事件重放不重复追加（纯函数重放一致性）
	reqs2 := EvidenceRequestsOf(eventsOfStore(t, store))
	if len(reqs2[0].Owners) != 1 {
		t.Fatalf("重放投影 owners 应稳定：%+v", reqs2[0].Owners)
	}
}

// ---- M3-4 Claim 认知账本 ----

func capsuleAcceptEvent(t *testing.T, seq int64, closureID, threadID string, conclusions, assumptions []string) StoredEvent {
	t.Helper()
	capsule := protocol.ClosureCapsule{
		ClosureID: closureID, ClosureType: "consensus", ThreadID: threadID,
		Conclusions: conclusions, Assumptions: assumptions,
		Evidence:       protocol.CapsuleEvidence{Support: []string{}, Oppose: []string{}},
		Participation:  protocol.CapsuleParticipation{Concluded: []string{}, Objected: []string{}, Abstained: []string{}, Timeout: []string{}, Unavailable: []string{}},
		ReopenTriggers: []string{"新证据"},
	}
	return StoredEvent{Envelope: protocol.Envelope{
		EventID: "evt_clm_" + closureID, TenantID: "ten_t", RoomID: "room_t", Seq: seq,
		Type: protocol.EventClosureAccepted, SchemaVersion: 1, OccurredAt: "2026-09-04T00:00:00Z",
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    mustJSON(protocol.ClosureAcceptedPayload{ClosureID: closureID, ClosureType: "consensus", ThreadID: threadID, Capsule: capsule}),
		Metadata:   map[string]any{},
	}}
}

func TestClaimsOf(t *testing.T) {
	events := []StoredEvent{
		{Envelope: protocol.Envelope{EventID: "evt_cl_r", TenantID: "ten_t", RoomID: "room_t", Seq: 1,
			Type: protocol.EventRoomCreated, SchemaVersion: 1, OccurredAt: "2026-09-04T00:00:00Z",
			Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Visibility: protocol.Visibility{Kind: "public"},
			Payload: mustJSON(map[string]any{"display_name": "T", "thread_id": "thr_1", "agents": []string{}}), Metadata: map[string]any{}}},
		capsuleAcceptEvent(t, 2, "clo_1", "thr_1", []string{"结论A1", "结论A2"}, []string{"假设X"}),
		capsuleAcceptEvent(t, 3, "clo_2", "thr_1", []string{"结论B1"}, []string{}), // 同 thread 再收束 → A1/A2 superseded
		capsuleAcceptEvent(t, 4, "clo_3", "thr_2", []string{"结论C1"}, []string{}), // 异 thread 独立
		{Envelope: protocol.Envelope{EventID: "evt_cl_e1", TenantID: "ten_t", RoomID: "room_t", Seq: 5,
			Type: protocol.EventEvidenceRequestCreated, SchemaVersion: 1, OccurredAt: "2026-09-04T00:00:00Z",
			Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Visibility: protocol.Visibility{Kind: "public"},
			Payload: mustJSON(protocol.EvidenceRequestCreatedPayload{RequestID: "ereq_q1", Question: "GA 日期？", Owners: []string{}}), Metadata: map[string]any{}}},
		{Envelope: protocol.Envelope{EventID: "evt_cl_e2", TenantID: "ten_t", RoomID: "room_t", Seq: 6,
			Type: protocol.EventEvidenceRequestResolved, SchemaVersion: 1, OccurredAt: "2026-09-04T00:00:00Z",
			Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Visibility: protocol.Visibility{Kind: "public"},
			Payload: mustJSON(protocol.EvidenceRequestResolvedPayload{RequestID: "ereq_q1", Resolution: "resolved", EvidenceRefs: []string{"evt_src"}}), Metadata: map[string]any{}}},
	}
	claims := ClaimsOf(events)
	byStmt := map[string]ClaimView{}
	for _, c := range claims {
		byStmt[c.Statement] = c
	}
	if c := byStmt["结论A1"]; c.Status != "superseded" || c.Kind != "conclusion" || c.OriginID != "clo_1" {
		t.Fatalf("同 thread 后到胶囊应取代早者结论：%+v", c)
	}
	if c := byStmt["结论A2"]; c.Status != "superseded" {
		t.Fatalf("A2 应 superseded：%+v", c)
	}
	if c := byStmt["结论B1"]; c.Status != "strengthened" {
		t.Fatalf("最新结论应 strengthened：%+v", c)
	}
	if c := byStmt["结论C1"]; c.Status != "strengthened" || c.ThreadID != "thr_2" {
		t.Fatalf("异 thread 独立：%+v", c)
	}
	if c := byStmt["假设X"]; c.Kind != "assumption" || c.Status != "open" {
		t.Fatalf("假设应 open：%+v", c)
	}
	if c := byStmt["GA 日期？"]; c.Kind != "open_question" || c.Status != "strengthened" {
		t.Fatalf("已解决需求单应 strengthened：%+v", c)
	}
	// 纯函数回放一致
	if again := ClaimsOf(events); len(again) != len(claims) {
		t.Fatalf("重放不一致：%d vs %d", len(again), len(claims))
	}
}

// eventsOfStore 测试辅助：读全量事件（MemStore Reader 面）。
func eventsOfStore(t *testing.T, store *MemStore) []StoredEvent {
	t.Helper()
	envs, _, err := store.EventsAfter(context.Background(), "room_t", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return envs
}
