//go:build it

// M0 出口判据：进程内"命令 → 事件 → 游标续传"往返（echo 适配器参与）。
// 链路：post_message 命令（fixture 契约）→ message.posted 落库 → 订阅者游标读到 →
// echo 三段任务（observe/evaluate_intent/generate）→ agent 消息事件落库 →
// 同一订阅者从旧游标续传只收到新事件 → outbox 排空。
// HTTP/SSE 形态的同名闭环属 M1（ADR-0001）。
package sqlite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func TestCommandEventCursorRoundtrip_IT(t *testing.T) {
	ctx := context.Background()
	store, _ := openTempStore(t)

	// 1) 命令侧：按 command schema 契约构造的 post_message 命令 → 人类消息事件
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "room-protocol",
		"fixtures", "valid", "command-post-message.json"))
	if err != nil {
		t.Fatalf("read command fixture: %v", err)
	}
	var cmd struct {
		CommandKind string         `json:"command_kind"`
		Payload     map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(raw, &cmd); err != nil {
		t.Fatalf("parse command fixture: %v", err)
	}
	if cmd.CommandKind != "post_message" {
		t.Fatalf("fixture command_kind = %q", cmd.CommandKind)
	}
	payloadJSON, _ := json.Marshal(cmd.Payload)

	humanEnv := protocol.Envelope{
		EventID:       "evt_rt_human_msg",
		TenantID:      "ten_local",
		RoomID:        "room_rt",
		Type:          protocol.EventMessagePosted,
		SchemaVersion: 1,
		OccurredAt:    "2026-08-28T08:00:00.000Z",
		Actor:         protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       payloadJSON,
		Metadata:      map[string]any{},
	}
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{humanEnv}); err != nil {
		t.Fatalf("append human message: %v", err)
	}

	// 2) 订阅者：空游标首读（limit 1 走分页路径），收到人类消息与续传游标
	first, next, err := store.EventsAfter(ctx, "room_rt", "", 1)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first) != 1 || first[0].Envelope.EventID != humanEnv.EventID {
		t.Fatalf("首读 = %d 条（期望 1 条人类消息）", len(first))
	}
	if next == "" {
		t.Fatal("未追平时必须返回续传游标")
	}
	subscriberCursor := next

	// 3) echo 适配器：observe → evaluate_intent → generate（同 fixture 契约上的确定性链）
	sup := agent.NewSupervisor()
	if err := sup.Register(echo.Adapter{}); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	defer sup.Shutdown()
	profile := agent.Profile{ProfileID: "prof_echo_rt", Adapter: "echo"}

	runTask := func(kind agent.TaskKind) agent.Result {
		t.Helper()
		handle, err := sup.Submit(ctx, profile, agent.Task{
			TaskID:        "tsk_rt_" + string(kind),
			Kind:          kind,
			ParticipantID: "par_echo",
			RoomID:        "room_rt",
			ThreadID:      "thr_rt",
			Context:       agent.Context{Inline: map[string]any{"stimulus": string(payloadJSON)}},
		})
		if err != nil {
			t.Fatalf("submit %s: %v", kind, err)
		}
		result, err := handle.Result()
		if err != nil {
			t.Fatalf("result %s: %v", kind, err)
		}
		return result
	}

	if r := runTask(agent.KindObserve); r.Block != "attention_assessment" {
		t.Fatalf("observe block = %q", r.Block)
	}
	if r := runTask(agent.KindEvaluateIntent); r.Block != "turn_intent" {
		t.Fatalf("evaluate_intent block = %q", r.Block)
	}
	draft := runTask(agent.KindGenerate)
	if draft.Block != "public_draft" {
		t.Fatalf("generate block = %q", draft.Block)
	}
	draftJSON, _ := json.Marshal(draft.Data)

	// 4) agent 消息事件：causation 链指向人类消息（RFC-0003：agent 发言 causation 指向有效授权链）
	agentEnv := protocol.Envelope{
		EventID:       "evt_rt_agent_msg",
		TenantID:      "ten_local",
		RoomID:        "room_rt",
		Type:          protocol.EventMessagePosted,
		SchemaVersion: 1,
		OccurredAt:    "2026-08-28T08:00:05.000Z",
		Actor:         protocol.Actor{ParticipantID: "par_echo", Kind: "agent"},
		CausationID:   &humanEnv.EventID,
		Visibility:    protocol.Visibility{Kind: "public"},
		Payload:       draftJSON,
		Metadata:      map[string]any{},
	}
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{agentEnv}); err != nil {
		t.Fatalf("append agent message: %v", err)
	}

	// 5) 同一订阅者续传：只收到 agent 新事件，人类消息不重复投递
	events, next2, err := store.EventsAfter(ctx, "room_rt", subscriberCursor, 10)
	if err != nil {
		t.Fatalf("resume read: %v", err)
	}
	if len(events) != 1 || events[0].Envelope.EventID != agentEnv.EventID {
		t.Fatalf("续传 = %d 条（期望仅 1 条 agent 消息）", len(events))
	}
	got := events[0].Envelope
	if got.Actor.Kind != "agent" || got.CausationID == nil || *got.CausationID != humanEnv.EventID {
		t.Fatalf("agent 事件元数据不符：actor=%s causation=%v", got.Actor.Kind, got.CausationID)
	}
	if next2 != "" {
		t.Fatalf("已追平，next 应为空（got %q）", next2)
	}

	// 6) outbox 排空：两条消息按提交序分发完毕
	pending, err := store.Pending(ctx, 100)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 || pending[0].EventID != humanEnv.EventID || pending[1].EventID != agentEnv.EventID {
		t.Fatalf("outbox 顺序/条目不符：%v", pending)
	}
	ids := make([]int64, len(pending))
	for i, e := range pending {
		ids[i] = e.ID
	}
	if err := store.MarkDispatched(ctx, ids); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	if pending, err = store.Pending(ctx, 100); err != nil || len(pending) != 0 {
		t.Fatalf("排空后 pending = %d, err = %v", len(pending), err)
	}
}
