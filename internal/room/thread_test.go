// UT 层：Thread 生命周期（B5——RFC-0004）。
package room

import (
	"context"
	"fmt"

	"encoding/json"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func threadEnv(t *testing.T) (*MemStore, *Engine, *Service, string, string) {
	t.Helper()
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	t.Cleanup(sup.Shutdown)
	sharedID := counterNewID()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_a", Profile: agent.Profile{ProfileID: "pa", Adapter: "echo"}}},
		Budget: contextx.Limits{},
		Clock:  testClock, Now: time.Now,
		NewID: sharedID, Tenant: "ten_local",
	})
	svc := NewService(Config{Store: store, Reader: store, Clock: testClock,
		NewID: sharedID, Tenant: "ten_local"})
	created, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{CommandKind: "create_room", ExpectedRoomVersion: 0,
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5ec0", IssuedAt: "2026-08-31T09:00:00.000Z",
			Payload: []byte(`{"display_name":"t"}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 根线程 id
	stored, _, _ := store.EventsAfter(context.Background(), created.RoomID, "", 10)
	rootThread := ""
	for _, ev := range stored {
		if ev.Envelope.Type == protocol.EventRoomCreated {
			var p struct {
				ThreadID string `json:"thread_id"`
			}
			_ = json.Unmarshal(ev.Envelope.Payload, &p)
			rootThread = p.ThreadID
		}
	}
	return store, eng, svc, created.RoomID, rootThread
}

var threadIDemSeq int

func idemKeyFor(seed string) string {
	threadIDemSeq++
	// 合法 UUIDv7 形态：版本 7 + 变体 9 + 递增序号尾巴（测试确定性）
	suffix := fmt.Sprintf("%012d", threadIDemSeq)
	return "018f6b2e-7c1a-7" + fmt.Sprintf("%03d", threadIDemSeq%1000) + "-9abc-" + suffix[:12]
}

func threadCmd(t *testing.T, svc *Service, roomID string, version int64, kind string, payload string) (*CommandResult, error) {
	t.Helper()
	return svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{RoomID: roomID, CommandKind: kind, ExpectedRoomVersion: version,
			IdempotencyKey: idemKeyFor(kind + payload),
			IssuedAt:       "2026-08-31T09:00:01.000Z", Payload: []byte(payload)})
}

func roomVersionOf(store *MemStore, roomID string) int64 {
	v := int64(0)
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Seq > v {
			v = ev.Seq
		}
	}
	return v
}

// TestThreadLifecycleStateMachine：fork → pause → resume → close → reopen → merge；
// 非法转移（active 直接 merge 到自身/双重 pause）拒绝。
func TestThreadLifecycleStateMachine(t *testing.T) {
	store, _, svc, roomID, root := threadEnv(t)

	// fork（源 = 根线程的消息锚：先发一条消息）
	_, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"},
		Command{RoomID: roomID, CommandKind: "post_message", ExpectedRoomVersion: roomVersionOf(store, roomID),
			IdempotencyKey: "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5ec1", IssuedAt: "2026-08-31T09:00:02.000Z",
			Payload: []byte(`{"body":"锚点","reply_to":null,"addressed_to":[],"relations":[]}`)})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	forked, err := threadCmd(t, svc, roomID, roomVersionOf(store, roomID), "fork_thread",
		`{"source_event_id":"`+stedEventID(store, roomID)+`","goal":"深挖锚点的性能分支"}`)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	_ = stedEventID
	// fork 返回的 event payload 载新线程
	var forkPayload protocol.ThreadLifecyclePayload
	for _, ev := range store.RoomEvents(roomID) {
		if ev.EventID == forked.EventID {
			_ = json.Unmarshal(ev.Payload, &forkPayload)
		}
	}
	if forkPayload.ThreadID == "" || forkPayload.ParentThreadID != root {
		t.Fatalf("fork 谱系不符：%+v（root=%s）", forkPayload, root)
	}
	v := roomVersionOf(store, roomID)

	// pause → resume
	if _, err := threadCmd(t, svc, roomID, v, "pause_thread", `{"thread_id":"`+forkPayload.ThreadID+`","reason":"挂起"}`); err != nil {
		t.Fatalf("pause: %v", err)
	}
	v = roomVersionOf(store, roomID)
	if _, err := threadCmd(t, svc, roomID, v, "pause_thread", `{"thread_id":"`+forkPayload.ThreadID+`"}`); err == nil {
		t.Fatal("双重 pause 应拒绝")
	}
	if _, err := threadCmd(t, svc, roomID, v, "resume_thread", `{"thread_id":"`+forkPayload.ThreadID+`"}`); err != nil {
		t.Fatalf("resume: %v", err)
	}
	v = roomVersionOf(store, roomID)

	// close → reopen → merge
	if _, err := threadCmd(t, svc, roomID, v, "close_thread", `{"thread_id":"`+forkPayload.ThreadID+`"}`); err != nil {
		t.Fatalf("close: %v", err)
	}
	v = roomVersionOf(store, roomID)
	if _, err := threadCmd(t, svc, roomID, v, "reopen_thread", `{"thread_id":"`+forkPayload.ThreadID+`"}`); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	v = roomVersionOf(store, roomID)
	if _, err := threadCmd(t, svc, roomID, v, "merge_thread", `{"thread_id":"`+forkPayload.ThreadID+`","merged_into":"`+root+`"}`); err != nil {
		t.Fatalf("merge: %v", err)
	}
	v = roomVersionOf(store, roomID)
	// merged 终态：任何转移拒绝
	if _, err := threadCmd(t, svc, roomID, v, "pause_thread", `{"thread_id":"`+forkPayload.ThreadID+`"}`); err == nil {
		t.Fatal("merged 终态应拒绝转移")
	}

	// 投影：threads + graph（forked_from + merged_into 显式边）
	envs := envelopesOfStore(store, roomID)
	threads, graph := RebuildThreads(envs)
	if th := threads[forkPayload.ThreadID]; th == nil || th.State != ThreadMerged || th.MergedInto != root {
		t.Fatalf("线程投影不符：%+v", threads[forkPayload.ThreadID])
	}
	kinds := map[string]int{}
	for _, e := range graph {
		kinds[e.Kind]++
		if e.Inferred {
			t.Fatal("当前所有边应为显式（推断边 M3）")
		}
	}
	if kinds["forked_from"] < 1 || kinds["merged_into"] < 1 {
		t.Fatalf("图边缺失：%v", kinds)
	}
}

// TestThreadGateEngine：暂停线程里的刺激不驱动自动轮；恢复后可驱动。
func TestThreadGateEngine(t *testing.T) {
	store, eng, svc, roomID, root := threadEnv(t)
	v := roomVersionOf(store, roomID)
	if _, err := threadCmd(t, svc, roomID, v, "pause_thread", `{"thread_id":"`+root+`"}`); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// 暂停线程的刺激：直接落库 + 投递（绕过命令面的线程校验以聚焦引擎门）
	tid := root
	payload, _ := json.Marshal(map[string]any{"body": "暂停线程里的消息", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}})
	env := protocol.Envelope{
		EventID: "evt_tg1", TenantID: "ten_local", RoomID: roomID, ThreadID: &tid,
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"}, Payload: payload, Metadata: map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, _ := json.Marshal(env)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: roomID, Envelope: raw})
	time.Sleep(200 * time.Millisecond)
	if hasType(store.RoomEvents(roomID), protocol.EventRoundOpened) {
		t.Fatal("暂停线程的刺激不应开轮")
	}
	// resume 后同刺激不重驱动（历史翻篇），新刺激可开轮
	if v2 := roomVersionOf(store, roomID); true {
		_ = v2
	}
	v = roomVersionOf(store, roomID)
	if _, err := threadCmd(t, svc, roomID, v, "resume_thread", `{"thread_id":"`+root+`"}`); err != nil {
		t.Fatalf("resume: %v", err)
	}
	env2 := env
	env2.EventID = "evt_tg2"
	payload2, _ := json.Marshal(map[string]any{"body": "恢复后的消息", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}})
	env2.Payload = payload2
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env2}); err != nil {
		t.Fatalf("append2: %v", err)
	}
	raw2, _ := json.Marshal(env2)
	eng.Deliver(context.Background(), outbox.Entry{RoomID: roomID, Envelope: raw2})
	waitRoundClosed(t, store, roomID)
	if !hasType(store.RoomEvents(roomID), protocol.EventRoundOpened) {
		t.Fatal("活跃线程的刺激应开轮")
	}
}

func stedEventID(store *MemStore, roomID string) string {
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Type == protocol.EventMessagePosted {
			return ev.EventID
		}
	}
	return ""
}

func envelopesOfStore(store *MemStore, roomID string) []protocol.Envelope {
	events := store.RoomEvents(roomID)
	envs := make([]protocol.Envelope, len(events))
	for i := range events {
		envs[i] = events[i]
	}
	return envs
}
