// M4-2 协作状态：座位级失败瞬态帧——评估失败/生成失败携真实原因发
// OnSeatStatus（此前只进日志，房间内零感知——v1.50/58 实证）。
package room

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// failAdapter 任务恒失败（Result 返回错误——模拟配额耗尽/网络不可达的真实错误串）。
type failAdapter struct{}

func (failAdapter) Name() string                     { return "fail_ad" }
func (failAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (failAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return failSession{}, nil
}

type failSession struct{}

func (failSession) Run(context.Context, agent.Task) (agent.Handle, error) { return failHandle{}, nil }
func (failSession) Cancel(string)                                         {}
func (failSession) Close()                                                {}

type failHandle struct{}

func (failHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (failHandle) Cancel()                           {}
func (failHandle) Result() (agent.Result, error) {
	return agent.Result{}, errM42Boom
}

var errM42Boom = &m42Error{}

type m42Error struct{}

func (*m42Error) Error() string { return "provider.auth_error: 403 usage limit（测试注入）" }

// m42Engine 双座：stub（评估 speak + 生成成功）与 fail（评估/生成均失败）。
// 失败帧经 channel 收集（闭包并发安全）。
func m42Engine(t *testing.T, store *MemStore) (*Engine, chan [2]string) {
	t.Helper()
	sup := agent.NewSupervisor()
	t.Cleanup(sup.Shutdown)
	stubIntentData = map[string]any{"action": "speak", "type": "answer",
		"scores": map[string]any{"relevance": .8, "novelty": .5, "urgency": .5, "confidence": .8}}
	t.Cleanup(func() { stubIntentData = nil })
	_ = sup.Register(intentStubAdapter{})
	_ = sup.Register(failAdapter{})
	ch := make(chan [2]string, 16)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_stub", Profile: agent.Profile{ProfileID: "ps", Adapter: "stub_intent"}},
			{ParticipantID: "par_fail", Profile: agent.Profile{ProfileID: "pf", Adapter: "fail_ad"}},
		},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		OnSeatStatus: func(roomID, participantID, status, detail string) {
			ch <- [2]string{participantID + "|" + status, detail}
		},
		Clock: testClock, Now: time.Now,
		NewID:  func(p string) string { return p + "_m42_" + pcount() },
		Tenant: "ten_local",
	})
	t.Cleanup(eng.Close)
	return eng, ch
}

var m42n atomic.Int64

func pcount() string {
	return strconv.FormatInt(m42n.Add(1), 36)
}

func TestSeatStatusOnFailures(t *testing.T) {
	store := NewMemStore()
	eng, ch := m42Engine(t, store)
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m42c", TenantID: "ten_local", RoomID: "room_m42", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	deliverHuman(t, store, eng, "room_m42")

	var got [][2]string
	deadline := time.After(3 * time.Second)
collect:
	for {
		select {
		case f := <-ch:
			got = append(got, f)
		case <-deadline:
			break collect
		}
	}
	if len(got) == 0 {
		t.Fatal("失败座应发 seatStatus")
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f[0]] = true
		if f[1] == "" {
			t.Fatal("失败帧必须携带原因（detail）")
		}
	}
	if !seen["par_fail|eval_failed"] {
		t.Fatalf("应有 par_fail|eval_failed: %+v", got)
	}
}
