// M4-1 任务执行通道：投影折叠（含迟到审计不改终态）、生命周期（started→
// 发布→completed / failed）、重启 unknown 恢复、命令校验与能力门。
package room

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func runEvents(t *testing.T, store *MemStore, roomID string) []StoredEvent {
	t.Helper()
	events, _, err := store.EventsAfter(context.Background(), roomID, "", 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return events
}

func runViewOf(t *testing.T, store *MemStore, roomID, runID string) RunView {
	t.Helper()
	v, ok := runViewMaybe(store, roomID, runID)
	if !ok {
		t.Fatalf("run %s 不在投影中", runID)
	}
	return v
}

func runViewMaybe(store *MemStore, roomID, runID string) (RunView, bool) {
	events, _, err := store.EventsAfter(context.Background(), roomID, "", 1000)
	if err != nil {
		return RunView{}, false
	}
	for _, v := range RunsOf(events) {
		if v.RunID == runID {
			return v, true
		}
	}
	return RunView{}, false
}

func TestRunsProjectionFold(t *testing.T) {
	events := []StoredEvent{
		{Envelope: protocol.Envelope{Type: protocol.EventRunRequested, Payload: []byte(`{"run_id":"run_a","assignee":"par_x","instruction":"i","requester":"par_owner"}`)}},
		{Envelope: protocol.Envelope{Type: protocol.EventRunStarted, Payload: []byte(`{"run_id":"run_a","assignee":"par_x"}`)}},
		{Envelope: protocol.Envelope{Type: protocol.EventRunCanceled, Payload: []byte(`{"run_id":"run_a","reason":"方向调整"}`)}},
		// 迟到完成审计：late=true 不翻转 canceled 终态
		{Envelope: protocol.Envelope{Type: protocol.EventRunCompleted, Payload: []byte(`{"run_id":"run_a","assignee":"par_x","late":true,"body":"迟到结果"}`)}},
	}
	runs := RunsOf(events)
	if len(runs) != 1 || runs[0].Status != "canceled" || !runs[0].Late {
		t.Fatalf("迟到审计不得翻转终态: %+v", runs)
	}
	// 切片 B：迟到正文随投影带出（未发布结果的唯一留存面——人类可复制救济）
	if runs[0].ResultBody != "迟到结果" {
		t.Fatalf("迟到审计应携带正文: %+v", runs[0])
	}
}

func m41Engine(t *testing.T, store *MemStore) *Engine {
	t.Helper()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: agent.NewSupervisor(),
		Seats:  []AgentSeat{{ParticipantID: "par_echo", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now,
		NewID:  func(p string) string { return p + "_m41_" + m41Seq() },
		Tenant: "ten_local",
	})
	_ = eng.cfg.Agents.Register(echoAdapterForRuns())
	t.Cleanup(eng.Close)
	return eng
}

func TestRunLifecycleHappy(t *testing.T) {
	store := NewMemStore()
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41c", TenantID: "ten_local", RoomID: "room_m41", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 生产时序：service 先落 run.requested（命令），引擎经分发器拉起
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41req", TenantID: "ten_local", RoomID: "room_m41", Type: protocol.EventRunRequested,
			Actor:    protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Payload:  mustJSON(protocol.RunRequestedPayload{RunID: "run_h1", Assignee: "par_echo", Instruction: "做点事", Requester: "par_owner"}),
			Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("requested: %v", err)
	}
	eng := m41Engine(t, store)
	eng.LaunchRun("room_m41", protocol.RunRequestedPayload{RunID: "run_h1", Assignee: "par_echo", Instruction: "做点事", Requester: "par_owner"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := runViewMaybe(store, "room_m41", "run_h1")
		if ok && v.Status == "completed" {
			if v.ResultEventID == "" {
				t.Fatalf("完成应携带结果消息: %+v", v)
			}
			// 结果消息：agent 名义 + metadata.run_id
			for _, ev := range runEvents(t, store, "room_m41") {
				if ev.Envelope.EventID == v.ResultEventID {
					if ev.Envelope.Actor.Kind != "agent" || ev.Envelope.Actor.ParticipantID != "par_echo" {
						t.Fatalf("结果消息应以负责人名义发布: %+v", ev.Envelope.Actor)
					}
					if ev.Envelope.Metadata["run_id"] != "run_h1" {
						t.Fatalf("结果消息 metadata 应携 run_id: %+v", ev.Envelope.Metadata)
					}
					return
				}
			}
			t.Fatal("结果消息事件不存在")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("3s 内未完成")
}

func TestRunLifecycleFailed(t *testing.T) {
	store := NewMemStore()
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41fc", TenantID: "ten_local", RoomID: "room_m41f", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sup := agent.NewSupervisor()
	t.Cleanup(sup.Shutdown)
	_ = sup.Register(failAdapter{})
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_fail", Profile: agent.Profile{ProfileID: "pf", Adapter: "fail_ad"}}},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now,
		NewID:  func(p string) string { return p + "_m41f_" + m41Seq() },
		Tenant: "ten_local",
	})
	t.Cleanup(eng.Close)
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41freq", TenantID: "ten_local", RoomID: "room_m41f", Type: protocol.EventRunRequested,
			Actor:    protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Payload:  mustJSON(protocol.RunRequestedPayload{RunID: "run_f1", Assignee: "par_fail", Instruction: "x", Requester: "par_owner"}),
			Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("requested: %v", err)
	}
	eng.LaunchRun("room_m41f", protocol.RunRequestedPayload{RunID: "run_f1", Assignee: "par_fail", Instruction: "x", Requester: "par_owner"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := runViewMaybe(store, "room_m41f", "run_f1"); ok && v.Status == "failed" {
			if !strings.Contains(v.Error, "usage limit") {
				t.Fatalf("失败原因应携真实错误串: %+v", v)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("3s 内未失败")
}

func TestRunRecoverUnknown(t *testing.T) {
	store := NewMemStore()
	// 上一进程遗留：requested + started（running 态、无活句柄）
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41uc", TenantID: "ten_local", RoomID: "room_m41u", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
		{EventID: "m41ur", TenantID: "ten_local", RoomID: "room_m41u", Type: protocol.EventRunRequested,
			Actor:   protocol.Actor{ParticipantID: "o", Kind: "human"},
			Payload: []byte(`{"run_id":"run_z1","assignee":"par_echo","instruction":"x","requester":"par_owner"}`), Metadata: map[string]any{}},
		{EventID: "m41us", TenantID: "ten_local", RoomID: "room_m41u", Type: protocol.EventRunStarted,
			Actor:   protocol.Actor{ParticipantID: "system", Kind: "system"},
			Payload: []byte(`{"run_id":"run_z1","assignee":"par_echo"}`), Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng := m41Engine(t, store)
	eng.RecoverRuns()
	v := runViewOf(t, store, "room_m41u", "run_z1")
	if v.Status != "unknown" {
		t.Fatalf("重启后 running 态应为 unknown: %+v", v)
	}
}

func TestRunTaskCommandValidation(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	svcRun := NewService(Config{Store: store, Clock: testClock,
		NewID: svc.cfg.NewID, Tenant: "ten_local",
		RunCapable: func(assignee string) bool { return assignee == "par_capable" }})
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	created, err := svcRun.ExecuteCommand(ctx, actor, createCmd("create_room", "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e01", 0, map[string]any{"display_name": "r"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bad := []map[string]any{
		{"assignee": "not-par", "instruction": "x"},
		{"assignee": "par_capable", "instruction": ""},
		{"assignee": "par_capable", "instruction": strings.Repeat("字", 4001)},
		{"assignee": "par_capable", "instruction": "x", "task_id": "bad"},
		{"assignee": "par_uncapable", "instruction": "x"}, // 能力门（echo 等）
	}
	for i, p := range bad {
		if _, err := svcRun.ExecuteCommand(ctx, actor, Command{RoomID: created.RoomID, CommandKind: "run_task",
			ExpectedRoomVersion: 1, IdempotencyKey: validUUIDv7Suffix(i), IssuedAt: "2026-09-07T00:00:01.000Z",
			Payload: mustJSON(p)}); err == nil {
			t.Errorf("case %d 应拒绝: %+v", i, p)
		}
	}
	// 合法发起 → run.requested
	res, err := svcRun.ExecuteCommand(ctx, actor, Command{RoomID: created.RoomID, CommandKind: "run_task",
		ExpectedRoomVersion: 1, IdempotencyKey: validUUIDv7Suffix(90), IssuedAt: "2026-09-07T00:00:02.000Z",
		Payload: mustJSON(map[string]any{"assignee": "par_capable", "instruction": "抓取并总结"})})
	if err != nil {
		t.Fatalf("run_task: %v", err)
	}
	_ = res
	// cancel：不存在 / 终态拒
	if _, err := svcRun.ExecuteCommand(ctx, actor, Command{RoomID: created.RoomID, CommandKind: "cancel_run",
		ExpectedRoomVersion: 2, IdempotencyKey: validUUIDv7Suffix(91), IssuedAt: "2026-09-07T00:00:03.000Z",
		Payload: mustJSON(map[string]any{"run_id": "run_nope", "reason": "x"})}); err == nil {
		t.Fatal("未知 run 取消应拒绝")
	}
}

// TestRunTimeoutLimitPrecedence OQ-B 设置族活读面：Func 优先于静态 RunTimeout，
// 两者缺省回 defaultRunTimeout（设置页变更无须重建引擎的机制面）。
func TestRunTimeoutLimitPrecedence(t *testing.T) {
	eng := m41Engine(t, NewMemStore())
	if got := eng.runTimeoutLimit(); got != defaultRunTimeout {
		t.Fatalf("无配置 = %v, want 缺省 %v", got, defaultRunTimeout)
	}
	eng.cfg.RunTimeout = 5 * time.Minute
	if got := eng.runTimeoutLimit(); got != 5*time.Minute {
		t.Fatalf("静态配置 = %v, want 5m", got)
	}
	eng.cfg.RunTimeoutFunc = func() time.Duration { return 90 * time.Second }
	if got := eng.runTimeoutLimit(); got != 90*time.Second {
		t.Fatalf("活读面应优先: %v, want 90s", got)
	}
	eng.cfg.RunTimeoutFunc = func() time.Duration { return 0 } // 非正值让位
	if got := eng.runTimeoutLimit(); got != 5*time.Minute {
		t.Fatalf("活读面非正值应回静态配置: %v, want 5m", got)
	}
}

// TestRunTimeoutFailed 超时收口：活读面上限到点 → run.failed 携"执行超时"
// （区分于适配器自身错误；在途 run 被超时击杀不产生结果消息）。
func TestRunTimeoutFailed(t *testing.T) {
	store := NewMemStore()
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41tc", TenantID: "ten_local", RoomID: "room_m41t", Type: protocol.EventRoomCreated,
			Actor: protocol.Actor{ParticipantID: "o", Kind: "human"}, Payload: []byte(`{}`), Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sup := agent.NewSupervisor()
	t.Cleanup(sup.Shutdown)
	_ = sup.Register(blockingRunAdapter{})
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats:  []AgentSeat{{ParticipantID: "par_block", Profile: agent.Profile{ProfileID: "pb", Adapter: "block_ad"}}},
		Budget: contextx.Limits{}, ReactionWindow: 5 * time.Millisecond,
		Clock: testClock, Now: time.Now,
		NewID: func(p string) string { return p + "_m41t_" + m41Seq() },
		RunTimeoutFunc: func() time.Duration {
			return 150 * time.Millisecond // OQ-B 活读面（测试注入缩距）
		},
		Tenant: "ten_local",
	})
	t.Cleanup(eng.Close)
	if _, err := store.AppendEvents(context.Background(), []protocol.Envelope{
		{EventID: "m41treq", TenantID: "ten_local", RoomID: "room_m41t", Type: protocol.EventRunRequested,
			Actor:    protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Payload:  mustJSON(protocol.RunRequestedPayload{RunID: "run_t1", Assignee: "par_block", Instruction: "慢活", Requester: "par_owner"}),
			Metadata: map[string]any{}},
	}); err != nil {
		t.Fatalf("requested: %v", err)
	}
	eng.LaunchRun("room_m41t", protocol.RunRequestedPayload{RunID: "run_t1", Assignee: "par_block", Instruction: "慢活", Requester: "par_owner"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := runViewMaybe(store, "room_m41t", "run_t1"); ok && v.Status == "failed" {
			if !strings.Contains(v.Error, "执行超时") {
				t.Fatalf("超时失败应明示超时语义: %+v", v)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("3s 内未按超时收口")
}

// m41Seq 测试事件 ID 序号（CI 快机上同纳秒时间戳会撞 EventID 唯一约束——
// 原子计数保证互异）。
var m41n atomic.Int64

func m41Seq() string {
	return strconv.FormatInt(m41n.Add(1), 36) + time.Now().Format("150405.000000000")
}

func validUUIDv7Suffix(i int) string {
	return fmt.Sprintf("018f6b2e-7c1a-7b3d-9e4f-9a2b3c4d5e%02x", i)
}

// echoAdapterForRuns echo 适配器的本文件别名（避免依赖 echo 包导入环）。
func echoAdapterForRuns() agent.Adapter { return echoRunAdapter{} }

type echoRunAdapter struct{}

func (echoRunAdapter) Name() string                     { return "echo" }
func (echoRunAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (echoRunAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return echoRunSession{}, nil
}

type echoRunSession struct{}

func (echoRunSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return echoRunHandle{body: "执行结果：任务已完成（echo）"}, nil
}
func (echoRunSession) Cancel(string) {}
func (echoRunSession) Close()        {}

type echoRunHandle struct{ body string }

func (echoRunHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (echoRunHandle) Cancel()                           {}
func (h echoRunHandle) Result() (agent.Result, error) {
	return agent.Result{Block: "public_draft", Data: map[string]any{
		"body": h.body, "declared_relations": []any{},
	}}, nil
}

// blockingRunAdapter 永不主动完成的适配器（超时/取消路径测试）：Result 阻塞到
// 提交 ctx 结束——引擎的超时击杀与取消代次由此可观察。
type blockingRunAdapter struct{}

func (blockingRunAdapter) Name() string { return "block_ad" }
func (blockingRunAdapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{TaskRuns: true}
}
func (blockingRunAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return blockingRunSession{}, nil
}

type blockingRunSession struct{}

func (blockingRunSession) Run(ctx context.Context, _ agent.Task) (agent.Handle, error) {
	return blockingRunHandle{ctx: ctx}, nil
}
func (blockingRunSession) Cancel(string) {}
func (blockingRunSession) Close()        {}

type blockingRunHandle struct{ ctx context.Context }

func (blockingRunHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (blockingRunHandle) Cancel()                           {}
func (h blockingRunHandle) Result() (agent.Result, error) {
	<-h.ctx.Done()
	return agent.Result{}, h.ctx.Err()
}

var hBody = "执行结果：任务已完成（echo）"
