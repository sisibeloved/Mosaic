// M4-3 UT：资格判定七形（点名单人/PID/多提及歧义/不可解析/无提及/reply-to
// 任务命中/作者不符）+ 引擎三路径（speak 发布带路径 metadata / pass 沉默 /
// 失败不回退）+ 设置门（off 回退两阶段且指标入账）。
package room

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func ropEnv(actor protocol.Actor, payload string) protocol.Envelope {
	return protocol.Envelope{EventID: "evt_stim", RoomID: "room_rop", Type: protocol.EventMessagePosted, Actor: actor, Payload: []byte(payload)}
}

func ropSeats() []AgentSeat {
	return []AgentSeat{
		{ParticipantID: "par_codex_x", Profile: agent.Profile{ProfileID: "pc", Adapter: "codex", DisplayName: "Codex"}},
		{ParticipantID: "par_kimi_x", Profile: agent.Profile{ProfileID: "pk", Adapter: "kimi", DisplayName: "Kimi"}},
	}
}

func TestROPEligibilityShapes(t *testing.T) {
	seats := ropSeats()
	human := protocol.Actor{ParticipantID: "par_owner", Kind: "human"}

	// 1) 点名显示名 → 恰一座
	e := ropEnv(human, `{"body":"@Codex 这个问题你怎么看？"}`)
	if got := ROPEligibility(e, seats, nil); got["par_codex_x"] != ROPScenarioNamedSingle || len(got) != 1 {
		t.Fatalf("点名单人应命中: %+v", got)
	}
	// 2) 点名 PID → 同样命中
	e = ropEnv(human, `{"body":"@par_kimi_x 给我说说"}`)
	if got := ROPEligibility(e, seats, nil); got["par_kimi_x"] != ROPScenarioNamedSingle {
		t.Fatalf("PID 点名应命中: %+v", got)
	}
	// 3) 多提及（两个 agent）→ 歧义不进入
	e = ropEnv(human, `{"body":"@Codex @Kimi 你们都说说"}`)
	if got := ROPEligibility(e, seats, nil); len(got) != 0 {
		t.Fatalf("多提及不得进入: %+v", got)
	}
	// 4) 不可解析提及 → 不进入
	e = ropEnv(human, `{"body":"@Ghost 你在吗"}`)
	if got := ROPEligibility(e, seats, nil); len(got) != 0 {
		t.Fatalf("不可解析不得进入: %+v", got)
	}
	// 5) 正文中间出现名字不算点名（须首行 @ 形态）
	e = ropEnv(human, `{"body":"大家觉得 Codex 说得对吗"}`)
	if got := ROPEligibility(e, seats, nil); len(got) != 0 {
		t.Fatalf("正文裸名不得进入: %+v", got)
	}
	// 6) reply_to 锚定 pending 任务申报消息（agent 围栏申报）→ 负责人资格
	declBody := "收到，我来安排。\n```mosaic-todo\n- [ ] @par_kimi_x 整理文档\n```"
	declEnv := ropEnv(protocol.Actor{ParticipantID: "par_codex_x", Kind: "agent"}, "")
	declEnv.Payload = mustJSON(map[string]any{"body": declBody})
	declEnv.EventID = "evt_decl"
	roomEnv := protocol.Envelope{EventID: "evt_room", Type: protocol.EventRoomCreated,
		Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Payload: mustJSON(map[string]any{"agents": []string{"par_codex_x", "par_kimi_x"}})}
	history := []StoredEvent{{Envelope: roomEnv}, {Envelope: declEnv}}
	e = ropEnv(human, `{"body":"进度如何？","reply_to":"evt_decl"}`)
	if got := ROPEligibility(e, seats, history); got["par_kimi_x"] != ROPScenarioTaskOwner {
		t.Fatalf("任务交付场景应命中负责人: %+v", got)
	}
	// 7) 回复指向不存在的消息 → 不进入
	e = ropEnv(human, `{"body":"进度如何？","reply_to":"evt_nope"}`)
	if got := ROPEligibility(e, seats, history); len(got) != 0 {
		t.Fatalf("未定位任务不得进入: %+v", got)
	}
}

// ropAdapter 可探单次路径桩：ReplyOrPass 能力 + 按 action 出结果。
type ropAdapter struct {
	mu       sync.Mutex
	action   string
	ropCalls int
}

func (a *ropAdapter) Name() string                     { return "rop_ad" }
func (a *ropAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{ReplyOrPass: true} }
func (a *ropAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return ropSession{adapter: a}, nil
}

type ropSession struct{ adapter *ropAdapter }

func (s ropSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	if task.Kind == agent.KindReplyOrPass {
		s.adapter.mu.Lock()
		s.adapter.ropCalls++
		action := s.adapter.action
		s.adapter.mu.Unlock()
		data := map[string]any{"action": action, "public_rationale": "rop 测试"}
		if action == "speak" {
			data["body"] = "单次路径的公开回复。"
		}
		return ropHandle{result: agent.Result{Block: "reply_or_pass_decision", Data: data}}, nil
	}
	// 两阶段面：恒静默
	return ropHandle{result: agent.Result{Block: "turn_intent", Data: map[string]any{"action": "silent", "public_rationale": "two-phase"}}}, nil
}
func (s ropSession) Cancel(string) {}
func (s ropSession) Close()        {}

type ropHandle struct{ result agent.Result }

func (h ropHandle) Updates() <-chan agent.DraftUpdate {
	ch := make(chan agent.DraftUpdate)
	close(ch)
	return ch
}
func (h ropHandle) Cancel() {}
func (h ropHandle) Result() (agent.Result, error) {
	return h.result, nil
}

func ropTestEngine(t *testing.T, store *MemStore, ad *ropAdapter, gate func() bool, metric func(string, string, string, string, string, int64)) *Engine {
	t.Helper()
	sup := agent.NewSupervisor()
	t.Cleanup(sup.Shutdown)
	_ = sup.Register(ad)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_rop", Profile: agent.Profile{ProfileID: "p", Adapter: "rop_ad", DisplayName: "Roper"}}},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now,
		NewID:              func(p string) string { return p + "_rop_" + m41Seq() },
		ReplyOrPassEnabled: gate, OnPathMetric: metric,
		Tenant: "ten_local",
	})
	t.Cleanup(eng.Close)
	return eng
}

func ropStimulus(t *testing.T, store *MemStore, body string, replyTo *string) protocol.Envelope {
	t.Helper()
	env := protocol.Envelope{
		EventID: "evt_stim_" + m41Seq(), TenantID: "ten_local", RoomID: "room_rop",
		Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:   protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload: mustJSON(map[string]any{"body": body, "reply_to": replyTo, "addressed_to": []string{}, "relations": []any{}}),
	}
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{env}); err != nil {
		t.Fatalf("stimulus: %v", err)
	}
	return env
}

func seedRoom(t *testing.T, store *MemStore, roomID string) {
	t.Helper()
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{{
		EventID: "evt_room_" + m41Seq(), TenantID: "ten_local", RoomID: roomID,
		Type: protocol.EventRoomCreated, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:   protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload: mustJSON(map[string]any{"display_name": "rop"}), Metadata: map[string]any{},
	}}); err != nil {
		t.Fatalf("seed room: %v", err)
	}
}

func ropWait(t *testing.T, store *MemStore, want int, timeout time.Duration) []StoredEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, _, _ := store.EventsAfter(context.Background(), "room_rop", "", 1000)
		n := 0
		for _, ev := range events {
			if ev.Envelope.Type == protocol.EventMessagePosted && ev.Envelope.Actor.Kind == "agent" {
				n++
			}
		}
		if n >= want {
			return events
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%v 内未见 %d 条 agent 消息", timeout, want)
	return nil
}

func TestROPEngineSpeakAndPass(t *testing.T) {
	store := NewMemStore()
	seedRoom(t, store, "room_rop")
	var mu sync.Mutex
	metrics := []string{}
	ad := &ropAdapter{action: "speak"}
	eng := ropTestEngine(t, store, ad, nil, func(_, _, scenario, path, outcome string, _ int64) {
		mu.Lock()
		metrics = append(metrics, scenario+"/"+path+"/"+outcome)
		mu.Unlock()
	})
	stim := ropStimulus(t, store, "@Roper 单值这个问题", nil)
	_ = eng.Deliver(context.Background(), outboxEntryOf(stim))
	events := ropWait(t, store, 1, 3*time.Second)
	foundPath := false
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventMessagePosted && ev.Envelope.Actor.Kind == "agent" {
			if ev.Envelope.Metadata["path"] != "reply_or_pass" {
				t.Fatalf("ROP 发布应携路径 metadata: %+v", ev.Envelope.Metadata)
			}
			foundPath = true
		}
		if ev.Envelope.Type == protocol.EventIntentRecorded && ev.Envelope.Metadata["path"] == "reply_or_pass" {
			var p protocol.IntentRecordedPayload
			_ = jsonUnmarshal(ev.Envelope.Payload, &p)
			if p.Action != "speak" || p.ScoreBand != "unranked" {
				t.Fatalf("ROP 意愿记录不伪造评分: %+v", p)
			}
		}
	}
	if !foundPath {
		t.Fatal("未见 ROP 发布")
	}
	if ad.ropCalls != 1 {
		t.Fatalf("单次路径应恰一次调用, got %d", ad.ropCalls)
	}
	mu.Lock()
	metricOK := len(metrics) > 0 && strings.Contains(metrics[0], "named_single/reply_or_pass/published")
	mu.Unlock()
	if !metricOK {
		t.Fatalf("指标应入账")
	}

	// pass 路径：沉默入账、无发布
	ad2 := &ropAdapter{action: "pass"}
	store2 := NewMemStore()
	seedRoom(t, store2, "room_rop")
	eng2 := ropTestEngine(t, store2, ad2, nil, nil)
	stim2 := ropStimulus(t, store2, "@Roper 这题让给你", nil)
	_ = eng2.Deliver(context.Background(), outboxEntryOf(stim2))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events, _, _ = store2.EventsAfter(context.Background(), "room_rop", "", 1000)
		done := false
		for _, ev := range events {
			if ev.Envelope.Type == protocol.EventIntentRecorded && ev.Envelope.Metadata["path"] == "reply_or_pass" {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventMessagePosted && ev.Envelope.Actor.Kind == "agent" {
			t.Fatal("pass 不得发布")
		}
	}
}

func TestROPEngineOffFallsBackWithMetric(t *testing.T) {
	store := NewMemStore()
	seedRoom(t, store, "room_rop")
	var dmu sync.Mutex
	deferred := 0
	ad := &ropAdapter{action: "speak"}
	eng := ropTestEngine(t, store, ad, func() bool { return false }, // off = 控制组
		func(_, _, _, path, outcome string, _ int64) {
			if path == "two_phase" && outcome == "deferred" {
				dmu.Lock()
				deferred++
				dmu.Unlock()
			}
		})
	stim := ropStimulus(t, store, "@Roper 控制组走两阶段", nil)
	_ = eng.Deliver(context.Background(), outboxEntryOf(stim))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ad.mu.Lock()
		calls := ad.ropCalls
		ad.mu.Unlock()
		dmu.Lock()
		d := deferred
		dmu.Unlock()
		if calls == 0 && d > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	ad.mu.Lock()
	calls := ad.ropCalls
	ad.mu.Unlock()
	dmu.Lock()
	d := deferred
	dmu.Unlock()
	if calls != 0 {
		t.Fatalf("off 模式不得走单次路径: %d", calls)
	}
	if d == 0 {
		t.Fatal("控制组指标应入账（A/B 面）")
	}
	// 两阶段照常出意图记录（silent）
	events, _, _ := store.EventsAfter(context.Background(), "room_rop", "", 1000)
	twoPhase := false
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventIntentRecorded && ev.Envelope.Metadata["path"] == nil {
			twoPhase = true
		}
	}
	if !twoPhase {
		t.Fatal("两阶段意图记录应存在")
	}
}

func TestROPEngineFailureNoFallback(t *testing.T) {
	store := NewMemStore()
	seedRoom(t, store, "room_rop")
	sup := agent.NewSupervisor()
	t.Cleanup(sup.Shutdown)
	_ = sup.Register(failAdapter{}) // ROP 调用失败（ReplyOrPass 未声明 → 走两阶段？不——能力门拒绝）
	// 直接构造能力支持但调用必败的桩
	_ = sup.RegisterFor("p", failingROPAdapter{})
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_rop", Profile: agent.Profile{ProfileID: "p", Adapter: "fail_ad"}}},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now,
		NewID:  func(p string) string { return p + "_ropf_" + m41Seq() },
		Tenant: "ten_local",
	})
	t.Cleanup(eng.Close)
	stim := ropStimulus(t, store, "@par_rop 来吧", nil)
	_ = eng.Deliver(context.Background(), outboxEntryOf(stim))
	deadline := time.Now().Add(3 * time.Second)
	events, _, _ := store.EventsAfter(context.Background(), "room_rop", "", 1000)
	for time.Now().Before(deadline) {
		events, _, _ = store.EventsAfter(context.Background(), "room_rop", "", 1000)
		done := false
		for _, ev := range events {
			if ev.Envelope.Type == protocol.EventIntentRecorded && ev.Envelope.Metadata["path"] == "reply_or_pass" {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	sawROP := false
	for _, ev := range events {
		if ev.Envelope.Type == protocol.EventIntentRecorded && ev.Envelope.Metadata["path"] == "reply_or_pass" {
			sawROP = true
		}
		if ev.Envelope.Type == protocol.EventMessagePosted && ev.Envelope.Actor.Kind == "agent" {
			t.Fatal("失败路径不得发布，也不得回退重跑（执行状态收口）")
		}
	}
	if !sawROP {
		t.Fatal("失败应记录弃权意图（含路径）")
	}
}

type failingROPAdapter struct{}

func (failingROPAdapter) Name() string { return "fail_ad" }
func (failingROPAdapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{ReplyOrPass: true}
}
func (failingROPAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return failingROPSession{}, nil
}

type failingROPSession struct{}

func (failingROPSession) Run(context.Context, agent.Task) (agent.Handle, error) {
	return nil, context.DeadlineExceeded
}
func (failingROPSession) Cancel(string) {}
func (failingROPSession) Close()        {}
