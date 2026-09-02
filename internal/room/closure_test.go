// UT 层：M3-2 收束协议（群聊制裁剪）——三态评估、确定性合格异议、胶囊组装、
// 接受关线程、预算暂停胶囊（预算路径随 TestEngineBudgetHardStop 覆盖）。
package room

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/echo"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// closureStubAdapter 可编程收束表态桩（conclude/object/abstain + object 增量字段）。
type closureStubAdapter struct {
	action     string
	evidence   []string
	assumption []string
	impact     string
}

func (a closureStubAdapter) Name() string { return "closure_stub" }
func (a closureStubAdapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{CancelMode: "interrupt", HistoryChannel: "structured_request"}
}
func (a closureStubAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return &closureStubSession{adapter: a}, nil
}

type closureStubSession struct{ adapter closureStubAdapter }

func (s *closureStubSession) Run(ctx context.Context, task agent.Task) (agent.Handle, error) {
	h := &closureHandle{done: make(chan struct{})}
	go func() {
		defer close(h.done)
		data := map[string]any{"action": s.adapter.action, "rationale": s.adapter.action + " 理由"}
		if s.adapter.evidence != nil {
			data["new_evidence"] = toAny(s.adapter.evidence)
		}
		if s.adapter.assumption != nil {
			data["new_assumptions"] = toAny(s.adapter.assumption)
		}
		if s.adapter.impact != "" {
			data["expected_impact"] = s.adapter.impact
		}
		h.result = agent.Result{Block: agent.BlockClosureIntent, Data: data}
	}()
	return h, nil
}
func (s *closureStubSession) Cancel(string) {}
func (s *closureStubSession) Close()        {}

type closureHandle struct {
	done   chan struct{}
	result agent.Result
}

func (h *closureHandle) Updates() <-chan agent.DraftUpdate {
	ch := make(chan agent.DraftUpdate)
	close(ch)
	return ch
}
func (h *closureHandle) Cancel()                       {}
func (h *closureHandle) Result() (agent.Result, error) { <-h.done; return h.result, nil }

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// newClosureEngine 双座收束引擎（echo 不参与：roster 圈定 stub 座）。
func newClosureEngine(t *testing.T, seats ...closureStubAdapter) (*Engine, *MemStore, *agent.Supervisor) {
	t.Helper()
	store := NewMemStore()
	sup := agent.NewSupervisor()
	_ = sup.Register(echo.Adapter{})
	roomSeats := []AgentSeat{}
	for i, a := range seats {
		profileID := "prof_cs_" + string(rune('a'+i))
		_ = sup.RegisterFor(profileID, a)
		roomSeats = append(roomSeats, AgentSeat{
			ParticipantID: "par_cs_" + string(rune('a'+i)),
			Profile:       agent.Profile{ProfileID: profileID, Adapter: "closure_stub"},
		})
	}
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup, Seats: roomSeats,
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now, NewID: counterNewID(), Tenant: "ten_local",
	})
	return eng, store, sup
}

func proposeClosureDirect(t *testing.T, store *MemStore, eng *Engine, roomID string) string {
	t.Helper()
	closureID := "clo_ut"
	env := protocol.Envelope{
		EventID: "evt_clo_p", TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventClosureProposed, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"closure_id":"` + closureID + `","thread_id":"thr_x","trigger":"human","watermark":1}`),
		Metadata:   map[string]any{},
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
		t.Fatalf("propose seed: %v", err)
	}
	if err := eng.Deliver(context.Background(), outboxEntryOf(env)); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	return closureID
}

// TestClosureHappyPathAccept：全 conclude → 待决可接受 → 接受 → 线程关闭。
func TestClosureHappyPathAccept(t *testing.T) {
	eng, store, sup := newClosureEngine(t,
		closureStubAdapter{action: "conclude"},
		closureStubAdapter{action: "abstain"},
	)
	defer sup.Shutdown()
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_clo1")
	proposeClosureDirect(t, store, eng, "room_clo1")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events := store.RoomEvents("room_clo1")
		if countType(events, protocol.EventClosureEvaluated) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pending, ok := PendingClosureOf(storedOf(store.RoomEvents("room_clo1")))
	if !ok || !pending.Ready || pending.ConcludedCountFor() != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	capsule, ok := BuildCapsule(storedOf(store.RoomEvents("room_clo1")), pending.ClosureID)
	if !ok {
		t.Fatal("胶囊组装失败")
	}
	if capsule.ClosureType != "bounded_disagreement" { // 有 abstain → 开放问题 → 分歧边界
		t.Fatalf("closure_type = %s", capsule.ClosureType)
	}
	if len(capsule.Participation.Concluded) != 1 || len(capsule.Participation.Abstained) != 1 {
		t.Fatalf("participation = %+v", capsule.Participation)
	}
	if len(capsule.ReopenTriggers) == 0 {
		t.Fatal("reopen_triggers 强制非空")
	}
}

// TestClosureQualifiedObjectionRejects：合格 object（新假设+预期影响）→ 收束中止。
func TestClosureQualifiedObjectionRejects(t *testing.T) {
	eng, store, sup := newClosureEngine(t,
		closureStubAdapter{action: "conclude"},
		closureStubAdapter{action: "object", assumption: []string{"成本假设不成立"}, impact: "推翻成本边界"},
	)
	defer sup.Shutdown()
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_clo2")
	proposeClosureDirect(t, store, eng, "room_clo2")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countType(store.RoomEvents("room_clo2"), protocol.EventClosureRejected) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	events := store.RoomEvents("room_clo2")
	if countType(events, protocol.EventClosureRejected) != 1 {
		t.Fatalf("合格异议应中止收束：%v", typesOf(events))
	}
	if _, pending := PendingClosureOf(storedOf(events)); pending {
		t.Fatal("中止后不得有待决收束")
	}
}

// TestClosureUnqualifiedObjectionParked：无增量 object → 具名异议不阻塞（待决可接受）。
func TestClosureUnqualifiedObjectionParked(t *testing.T) {
	eng, store, sup := newClosureEngine(t,
		closureStubAdapter{action: "object"}, // 无新证据/假设 → 不合格
	)
	defer sup.Shutdown()
	defer eng.Close()
	seedRoomCreatedFor(t, store, "room_clo3")
	proposeClosureDirect(t, store, eng, "room_clo3")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countType(store.RoomEvents("room_clo3"), protocol.EventClosureEvaluated) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	events := store.RoomEvents("room_clo3")
	var eval protocol.ClosureEvaluatedPayload
	for _, ev := range events {
		if ev.Type == protocol.EventClosureEvaluated {
			_ = json.Unmarshal(ev.Payload, &eval)
		}
	}
	if eval.Qualified || eval.ParkedReason != "no_new_evidence_or_assumptions" {
		t.Fatalf("不合格 object 应停放：%+v", eval)
	}
	if countType(events, protocol.EventClosureRejected) != 0 {
		t.Fatal("不合格异议不得中止收束")
	}
	capsule, ok := BuildCapsule(storedOf(events), "clo_ut")
	if !ok || len(capsule.NamedDissent) != 1 {
		t.Fatalf("具名异议应入胶囊：%+v", capsule)
	}
	if capsule.ClosureType != "bounded_disagreement" {
		t.Fatalf("有异议 → bounded_disagreement，got %s", capsule.ClosureType)
	}
}

// storedOf Envelope 列表转 StoredEvent（纯 UT 桥）。
func storedOf(envs []protocol.Envelope) []StoredEvent {
	out := make([]StoredEvent, len(envs))
	for i, e := range envs {
		out[i] = StoredEvent{Envelope: e}
	}
	return out
}

func (p PendingClosure) ConcludedCountFor() int { return p.ConcludedCount }
