// UT 层：simultaneous 波内生成并行性（dogfood 反馈修复）——冻结水位下各获选者
// 独立生成，墙钟 ≈ 最慢者而非各座位延迟之和（串行双 400ms 座 ≥800ms，并行 <700ms）；
// 并行证据用"生成窗口重叠"原子标记断言，不单靠计时。
package room

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// slowAdapter：evaluate 即返 intent；generate 睡 400ms 后返正文。
type slowAdapter struct {
	genInFlight atomic.Int64
}

func (s *slowAdapter) Name() string                     { return "slow" }
func (s *slowAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (s *slowAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return &slowSession{adapter: s}, nil
}

type slowSession struct {
	adapter *slowAdapter
	mu      sync.Mutex
	cancels map[string]func()
}

func (s *slowSession) Run(ctx context.Context, task agent.Task) (agent.Handle, error) {
	taskCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.cancels == nil {
		s.cancels = map[string]func(){}
	}
	s.cancels[task.TaskID] = cancel
	s.mu.Unlock()
	return &slowHandle{adapter: s.adapter, task: task, ctx: taskCtx, cancel: cancel}, nil
}
func (s *slowSession) Cancel(taskID string) {
	s.mu.Lock()
	if c, ok := s.cancels[taskID]; ok {
		c()
		delete(s.cancels, taskID)
	}
	s.mu.Unlock()
}
func (s *slowSession) Close() {}

type slowHandle struct {
	adapter *slowAdapter
	task    agent.Task
	ctx     context.Context
	cancel  func()
}

func (h *slowHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (h *slowHandle) Cancel()                           { h.cancel() }

func (h *slowHandle) Result() (agent.Result, error) {
	if h.task.Kind == agent.KindGenerate {
		n := h.adapter.genInFlight.Add(1)
		if n >= 2 {
			slowParallelOverlap.Store(true) // 第二个生成开始时第一个仍在途
		}
		select {
		case <-time.After(400 * time.Millisecond):
		case <-h.ctx.Done():
			h.adapter.genInFlight.Add(-1)
			return agent.Result{}, agent.ErrStale
		}
		h.adapter.genInFlight.Add(-1)
		return agent.Result{Block: "public_draft",
			Data: map[string]any{"body": "slow " + h.task.ParticipantID, "declared_relations": []any{}}}, nil
	}
	return agent.Result{Block: "turn_intent",
		Data: map[string]any{"action": "speak", "type": "extend",
			"scores": map[string]any{"relevance": .8, "novelty": .5, "urgency": .5, "confidence": .5}}}, nil
}

var slowParallelOverlap atomic.Bool

// TestSimultaneousGenerationParallel：双 slow 座一轮（默认 open_floor=simultaneous）。
func TestSimultaneousGenerationParallel(t *testing.T) {
	store := NewMemStore()
	sup := agent.NewSupervisor()
	slow := &slowAdapter{}
	_ = sup.Register(slow)
	defer sup.Shutdown()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_s1", Profile: agent.Profile{ProfileID: "ps1", Adapter: "slow"}},
			{ParticipantID: "par_s2", Profile: agent.Profile{ProfileID: "ps2", Adapter: "slow"}},
		},
		Budget: contextx.Limits{},
		Clock:  testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "sp0", TenantID: "ten_local", RoomID: "room_sp", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	})
	start := time.Now()
	deliverHuman(t, store, eng, "room_sp")
	waitRoundClosed(t, store, "room_sp")
	elapsed := time.Since(start)

	events := store.RoomEvents("room_sp")
	if n := countAgentMsgsOf(events); n != 2 {
		t.Fatalf("双座应各发布一条，got %d：%v", n, typesOf(events))
	}
	if elapsed >= 700*time.Millisecond {
		t.Fatalf("波内生成应并行（≈最慢者 400ms），串行回归：elapsed=%v", elapsed)
	}
	if !slowParallelOverlap.Load() {
		t.Fatal("缺并行证据：两生成窗口无重叠")
	}
}

func countAgentMsgsOf(events []protocol.Envelope) int {
	n := 0
	for _, ev := range events {
		if ev.Type == protocol.EventMessagePosted && ev.Actor.Kind == "agent" {
			n++
		}
	}
	return n
}
