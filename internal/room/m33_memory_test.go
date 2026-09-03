// M3-3 tasklist / 记忆编辑 / 按需检索 / 恒常预算 UT（纯函数与命令链）。
// tasklist 语义锚点（RFC-0012 OQ-A 修订 / v1.45 负责人裁定）：带责任人 owner、
// 确定性派生（零 LLM）、人工门控（自动判定交付会伪装闭环）。
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func taskEvent(t *testing.T, seq int64, typ, actorKind, actor, body string) StoredEvent {
	t.Helper()
	e := protocol.Envelope{
		EventID:  "evt_t" + strings.Repeat("0", 3) + string(rune('a'+seq%26)) + string(rune('0'+seq%10)),
		TenantID: "ten_t", RoomID: "room_t", Seq: seq, Type: typ,
		SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:00Z",
		Actor:      protocol.Actor{ParticipantID: actor, Kind: actorKind},
		Visibility: protocol.Visibility{Kind: "public"},
		Metadata:   map[string]any{},
	}
	if typ == protocol.EventMessagePosted {
		e.Payload = mustJSON(map[string]any{"body": body})
	}
	return StoredEvent{Envelope: e, Cursor: "c" + string(rune('0'+seq%10))}
}

func TestTaskDerivationDeterministic(t *testing.T) {
	events := []StoredEvent{
		taskEvent(t, 1, protocol.EventRoomCreated, "human", "par_owner", ""),
		// agent 宣言（狗粮实录原句形态）→ 派生
		taskEvent(t, 2, protocol.EventMessagePosted, "agent", "par_codex", "好，我来拉数据，稍后给出结果。"),
		// human 的"我来"不派生（人类待办不是 agent 群聊语境的债）
		taskEvent(t, 3, protocol.EventMessagePosted, "human", "par_owner", "我来补充一下背景。"),
		// 疑问句不派生
		taskEvent(t, 4, protocol.EventMessagePosted, "agent", "par_kimi", "我来处理这个可以吗？"),
		// 无宣言模式不派生
		taskEvent(t, 5, protocol.EventMessagePosted, "agent", "par_kimi", "这个方案有风险。"),
	}
	tasks := TasksOf(events)
	if len(tasks) != 1 {
		t.Fatalf("派生数 = %d（%+v），期望 1（仅 agent 宣言句）", len(tasks), tasks)
	}
	got := tasks[0]
	if got.Owner != "par_codex" {
		t.Fatalf("owner = %s，期望 par_codex（责任人）", got.Owner)
	}
	if !strings.Contains(got.Text, "我来拉数据") {
		t.Fatalf("text = %q，期望宣言句", got.Text)
	}
	if got.Status != "pending" || got.Overdue {
		t.Fatalf("初始态应为 pending 且未逾期：%+v", got)
	}
	if got.WavesSince != 0 {
		t.Fatalf("无波时 waves_since 应为 0：%+v", got)
	}
	if got.TaskID != TaskIDOf(got.SourceEventID) {
		t.Fatal("task_id 须为源事件哈希派生（确定性）")
	}
}

func TestTaskWavesAndOverdue(t *testing.T) {
	events := []StoredEvent{
		taskEvent(t, 2, protocol.EventMessagePosted, "agent", "par_codex", "我去查证这个数据。"),
		taskEvent(t, 3, protocol.EventRoundOpened, "system", "par_system", ""),
		taskEvent(t, 4, protocol.EventRoundOpened, "system", "par_system", ""),
		taskEvent(t, 5, protocol.EventRoundOpened, "system", "par_system", ""),
	}
	tasks := TasksOf(events)
	if len(tasks) != 1 || tasks[0].WavesSince != 3 {
		t.Fatalf("waves_since = %d，期望 3（声明后每波计数）", tasks[0].WavesSince)
	}
	if !tasks[0].Overdue {
		t.Fatal("waves_since ≥ OverdueWaves(2) 应标 overdue")
	}
}

func TestTaskResolvedHumanGate(t *testing.T) {
	decl := taskEvent(t, 2, protocol.EventMessagePosted, "agent", "par_codex", "我来拉数据。")
	events := []StoredEvent{decl}
	taskID := TaskIDOf(decl.Envelope.EventID)
	resolved := StoredEvent{Envelope: protocol.Envelope{
		EventID: "evt_tr1", TenantID: "ten_t", RoomID: "room_t", Seq: 3,
		Type: protocol.EventTaskResolved, SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:01Z",
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload: mustJSON(protocol.TaskResolvedPayload{
			TaskID: taskID, Owner: "par_codex", Resolution: "delivered",
			Note: "数据已贴出", ResolvedBy: "par_owner",
		}),
		Metadata: map[string]any{},
	}}
	tasks := TasksOf(append(events, resolved))
	if len(tasks) != 1 || tasks[0].Status != "delivered" {
		t.Fatalf("裁定后 status = %+v，期望 delivered", tasks)
	}
	// 重复裁定（第二次 resolve 事件）不改变终态
	again := resolved
	again.Envelope.EventID = "evt_tr2"
	again.Envelope.Seq = 4
	tasks = TasksOf(append(events, resolved, again))
	if tasks[0].Status != "delivered" || !strings.Contains(tasks[0].Note, "数据已贴出") {
		t.Fatalf("重复裁定应保持首个终态：%+v", tasks[0])
	}
	// pending 投影：已裁定任务不再入语境
	if briefs := PendingTaskBriefsOf(envelopesOfStored(append(events, resolved))); len(briefs) != 0 {
		t.Fatalf("pending 投影应不含已裁定任务：%+v", briefs)
	}
}

// ---- resolve_task / edit_memory 命令链 ----

func newTaskTestService(t *testing.T) (*Service, *MemStore) {
	t.Helper()
	store := NewMemStore()
	svc := NewService(Config{
		Store: store, Reader: store, Clock: func() string { return "2026-09-03T00:00:00Z" },
		NewID: counterNewID(), Tenant: "ten_t",
	})
	return svc, store
}

// 合法 UUIDv7 且每命令唯一（尾字节自增）。
var m33IDemSeq int

func m33UUID() string {
	m33IDemSeq++
	return fmt.Sprintf("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e%02x", m33IDemSeq)
}

func TestServiceResolveTask(t *testing.T) {
	svc, store := newTaskTestService(t)
	ctx := context.Background()
	// 房间 + agent 宣言
	seed := []protocol.Envelope{
		{EventID: "evt_r1", TenantID: "ten_t", RoomID: "room_t", Seq: 1, Type: protocol.EventRoomCreated,
			SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:00Z", Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"}, Payload: mustJSON(map[string]any{"display_name": "T", "thread_id": "thr_1", "agents": []string{}}), Metadata: map[string]any{}},
		{EventID: "evt_m1", TenantID: "ten_t", RoomID: "room_t", Seq: 2, Type: protocol.EventMessagePosted,
			SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:00Z", Actor: protocol.Actor{ParticipantID: "par_codex", Kind: "agent"},
			Visibility: protocol.Visibility{Kind: "public"}, Payload: mustJSON(map[string]any{"body": "好，开拉数据，稍后给出结果。"}), Metadata: map[string]any{}},
	}
	if _, err := store.AppendEvents(ctx, seed); err != nil {
		t.Fatal(err)
	}
	taskID := TaskIDOf("evt_m1")
	v, _ := store.RoomVersion(ctx, "room_t")

	mk := func(payload string) Command {
		return Command{RoomID: "room_t", CommandKind: "resolve_task", ExpectedRoomVersion: v,
			IdempotencyKey: m33UUID(), IssuedAt: "2026-09-03T00:00:00Z", Payload: json.RawMessage(payload)}
	}
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}

	// 非法 resolution
	if _, err := svc.ExecuteCommand(ctx, actor, mk(`{"task_id":"`+taskID+`","resolution":"done"}`)); err == nil {
		t.Fatal("非法 resolution 应拒绝")
	}
	// 不存在的任务
	if _, err := svc.ExecuteCommand(ctx, actor, mk(`{"task_id":"tsk_nope","resolution":"delivered"}`)); err == nil {
		t.Fatal("不存在的任务应拒绝")
	}
	// 成功裁定
	res, err := svc.ExecuteCommand(ctx, actor, mk(`{"task_id":"`+taskID+`","resolution":"delivered","note":"已贴出"}`))
	if err != nil {
		t.Fatalf("resolve_task 失败: %v", err)
	}
	if res.Replayed {
		t.Fatal("首次执行非回放")
	}
	// 版本推进后重复裁定 → 拒绝
	v2, _ := store.RoomVersion(ctx, "room_t")
	cmd2 := Command{RoomID: "room_t", CommandKind: "resolve_task", ExpectedRoomVersion: v2,
		IdempotencyKey: m33UUID(), IssuedAt: "2026-09-03T00:00:00Z",
		Payload: json.RawMessage(`{"task_id":"` + taskID + `","resolution":"dismissed"}`)}
	if _, err := svc.ExecuteCommand(ctx, actor, cmd2); err == nil {
		t.Fatal("已裁定任务重复裁定应拒绝")
	}
}

func TestServiceEditMemory(t *testing.T) {
	svc, store := newTaskTestService(t)
	ctx := context.Background()
	capsule := protocol.ClosureCapsule{
		ClosureID: "clo_1", ClosureType: "consensus", ThreadID: "thr_1", Watermark: 3,
		Conclusions: []string{"原始结论"}, Assumptions: []string{"原始假设"},
		ReopenTriggers: []string{"x"}, Evidence: protocol.CapsuleEvidence{Support: []string{}, Oppose: []string{}},
		Participation: protocol.CapsuleParticipation{Concluded: []string{"par_codex"}, Objected: []string{}, Abstained: []string{}, Timeout: []string{}, Unavailable: []string{}},
	}
	seed := []protocol.Envelope{
		{EventID: "evt_r1", TenantID: "ten_t", RoomID: "room_t", Seq: 1, Type: protocol.EventRoomCreated,
			SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:00Z", Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"}, Payload: mustJSON(map[string]any{"display_name": "T", "thread_id": "thr_1", "agents": []string{}}), Metadata: map[string]any{}},
		{EventID: "evt_ca1", TenantID: "ten_t", RoomID: "room_t", Seq: 2, Type: protocol.EventClosureAccepted,
			SchemaVersion: 1, OccurredAt: "2026-09-03T00:00:00Z", Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"},
			Payload:    mustJSON(protocol.ClosureAcceptedPayload{ClosureID: "clo_1", ClosureType: "consensus", ThreadID: "thr_1", Capsule: capsule, AcceptedBy: "par_owner"}),
			Metadata:   map[string]any{}},
	}
	if _, err := store.AppendEvents(ctx, seed); err != nil {
		t.Fatal(err)
	}
	v, _ := store.RoomVersion(ctx, "room_t")
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	mk := func(idem int, payload string) Command {
		return Command{RoomID: "room_t", CommandKind: "edit_memory", ExpectedRoomVersion: v,
			IdempotencyKey: m33UUID(), IssuedAt: "2026-09-03T00:00:00Z", Payload: json.RawMessage(payload)}
	}

	// 不存在的胶囊
	if _, err := svc.ExecuteCommand(ctx, actor, mk(1, `{"memory_id":"clo_nope","conclusions":["x"]}`)); err == nil {
		t.Fatal("不存在胶囊应拒绝")
	}
	// 全空编辑
	if _, err := svc.ExecuteCommand(ctx, actor, mk(2, `{"memory_id":"clo_1","note":"x"}`)); err == nil {
		t.Fatal("conclusions/assumptions 全空应拒绝")
	}
	// 成功编辑（只换 conclusions；assumptions 不动）
	if _, err := svc.ExecuteCommand(ctx, actor, mk(3, `{"memory_id":"clo_1","conclusions":["修正后结论"],"note":"表述有误"}`)); err != nil {
		t.Fatalf("edit_memory 失败: %v", err)
	}
	events, _, _ := store.EventsAfter(ctx, "room_t", "", 100)
	views := MemoryCapsulesOf(events)
	if len(views) != 1 {
		t.Fatalf("胶囊视图数 = %d", len(views))
	}
	if len(views[0].Conclusions) != 1 || views[0].Conclusions[0] != "修正后结论" {
		t.Fatalf("conclusions 未替换: %v", views[0].Conclusions)
	}
	if len(views[0].Assumptions) != 1 || views[0].Assumptions[0] != "原始假设" {
		t.Fatalf("未提供的 assumptions 应保持: %v", views[0].Assumptions)
	}
	if len(views[0].EditHistory) != 1 || views[0].EditHistory[0].EditVersion != 1 {
		t.Fatalf("edit_history 应记 v1: %+v", views[0].EditHistory)
	}
	// 第二次编辑 → v2
	v2, _ := store.RoomVersion(ctx, "room_t")
	cmd2 := Command{RoomID: "room_t", CommandKind: "edit_memory", ExpectedRoomVersion: v2,
		IdempotencyKey: m33UUID(), IssuedAt: "2026-09-03T00:00:00Z",
		Payload: json.RawMessage(`{"memory_id":"clo_1","assumptions":["新假设"],"note":"二改"}`)}
	if _, err := svc.ExecuteCommand(ctx, actor, cmd2); err != nil {
		t.Fatalf("第二次编辑失败: %v", err)
	}
	events, _, _ = store.EventsAfter(ctx, "room_t", "", 100)
	views = MemoryCapsulesOf(events)
	if len(views[0].EditHistory) != 2 || views[0].EditHistory[1].EditVersion != 2 {
		t.Fatalf("edit_history 应记 v2: %+v", views[0].EditHistory)
	}
	if views[0].Conclusions[0] != "修正后结论" || views[0].Assumptions[0] != "新假设" {
		t.Fatalf("编辑链应用错: %+v", views[0])
	}
}

func TestCapsuleBudgetDiscipline(t *testing.T) {
	mkCapsule := func(id string, n int) protocol.ClosureCapsule {
		c := protocol.ClosureCapsule{ClosureID: id, ClosureType: "consensus", ThreadID: "thr_1"}
		for i := 0; i < n; i++ {
			c.Conclusions = append(c.Conclusions, strings.Repeat("结", 100))
		}
		return c
	}
	// envs 数组序即事件序（尾部最新）——AcceptedCapsulesOf 倒序扫描取最新在前。
	mkEnvs := func(cs ...protocol.ClosureCapsule) []protocol.Envelope {
		out := make([]protocol.Envelope, len(cs))
		for i, c := range cs {
			out[i] = protocol.Envelope{Type: protocol.EventClosureAccepted, Payload: mustJSON(protocol.ClosureAcceptedPayload{ClosureID: c.ClosureID, Capsule: c})}
		}
		return out
	}
	// 3 条各 ~1000 字：合计 3000 恰满预算 → 全注入
	envs := mkEnvs(mkCapsule("clo_a", 10), mkCapsule("clo_b", 10), mkCapsule("clo_c", 10))
	got, stat := capsuleMemoriesOf(envs)
	if len(got) != 3 || stat.DroppedCount != 0 {
		t.Fatalf("预算内应全注入: got=%d stat=%+v", len(got), stat)
	}
	// clo_new 最旧（数组首位）→ 超预算时被挤出，最新三条注入
	envs = mkEnvs(mkCapsule("clo_new", 10), mkCapsule("clo_a", 10), mkCapsule("clo_b", 10), mkCapsule("clo_c", 10))
	got, stat = capsuleMemoriesOf(envs)
	if len(got) != 3 || stat.DroppedCount != 1 {
		t.Fatalf("超预算应挤最旧: got=%d stat=%+v", len(got), stat)
	}
	if got[0].ClosureID != "clo_c" {
		t.Fatalf("最新胶囊应最先注入（最新语境优先）: %v", got[0].ClosureID)
	}
}

// ---- 按需检索（线性语义基准）----

func TestSearchMessagesLinear(t *testing.T) {
	events := []StoredEvent{
		taskEvent(t, 1, protocol.EventMessagePosted, "human", "par_owner", "预算超限的问题需要先排查"),
		taskEvent(t, 2, protocol.EventMessagePosted, "agent", "par_codex", "我去拉数据"),
		taskEvent(t, 3, protocol.EventMessagePosted, "agent", "par_kimi", "数据已拉到，Budget exceeded"),
	}
	hits := SearchMessages(events, "拉数据", "", "", 20)
	if len(hits) != 1 || hits[0].Actor != "par_codex" {
		t.Fatalf("CJK 子串命中 = %+v", hits)
	}
	hits = SearchMessages(events, "budget", "", "", 20)
	if len(hits) != 1 || hits[0].Actor != "par_kimi" {
		t.Fatalf("大小写不敏感 ASCII 命中 = %+v", hits)
	}
	hits = SearchMessages(events, "预算", "par_owner", "", 20)
	if len(hits) != 1 {
		t.Fatalf("actor 过滤命中 = %+v", hits)
	}
	hits = SearchMessages(events, "数据", "", "", 1)
	if len(hits) != 1 {
		t.Fatalf("limit 生效 = %+v", hits)
	}
	if hits := SearchMessages(events, "", "", "", 20); len(hits) != 0 {
		t.Fatal("空查询应返回空")
	}
	// 最新在前
	hits = SearchMessages(events, "数据", "", "", 20)
	if len(hits) < 2 || hits[0].Actor != "par_kimi" {
		t.Fatalf("应最新在前: %+v", hits)
	}
}

func TestExtractKeywords(t *testing.T) {
	kws := ExtractKeywords("预算超限的问题，需要先排查 migration script")
	if len(kws) != 3 {
		t.Fatalf("关键词数 = %d（%v），期望 3（取前 3）", len(kws), kws)
	}
	// 长词在前：中文连续段（预算超限的问题 = 7 字）与 migration（10 字母）
	if kws[0] != "migration" && kws[0] != "预算超限的问题" {
		t.Fatalf("最长词应在前: %v", kws)
	}
	for _, k := range kws {
		if len([]rune(k)) < 2 {
			t.Fatalf("过短词不应入选: %v", kws)
		}
	}
	if kws := ExtractKeywords("。！？；"); len(kws) != 0 {
		t.Fatalf("纯标点应无关键词: %v", kws)
	}
}

func TestRetrieveRelated(t *testing.T) {
	envs := []protocol.Envelope{
		{EventID: "evt_old1", Type: protocol.EventMessagePosted, Payload: mustJSON(map[string]any{"body": "预算口径上次讨论过，是 5 万"}), Actor: protocol.Actor{ParticipantID: "par_codex", Kind: "agent"}},
		{EventID: "evt_new1", Type: protocol.EventMessagePosted, Payload: mustJSON(map[string]any{"body": "这次预算超了"}), Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}},
	}
	stimulus := envs[1]
	exclude := map[string]bool{"evt_new1": true} // recent 窗口（仅 1 条消息）
	hits := RetrieveRelated(envs, []string{"预算"}, exclude, 5)
	if len(hits) != 1 || hits[0].EventID != "evt_old1" {
		t.Fatalf("近窗外命中 = %+v", hits)
	}
	// 无关键词命中 → 空
	if hits := RetrieveRelated(envs, []string{"不存在词"}, exclude, 5); len(hits) != 0 {
		t.Fatalf("无命中应空: %+v", hits)
	}
	_ = stimulus
}

// ---- 快照投影 ----

func TestSnapshotTasksProjection(t *testing.T) {
	decl := taskEvent(t, 2, protocol.EventMessagePosted, "agent", "par_codex", "我来拉数据，稍后给出结果。")
	events := []StoredEvent{
		taskEvent(t, 1, protocol.EventRoomCreated, "human", "par_owner", ""), decl,
		taskEvent(t, 3, protocol.EventRoundOpened, "system", "par_system", ""),
		taskEvent(t, 4, protocol.EventRoundOpened, "system", "par_system", ""),
	}
	snap := ProjectSnapshot("room_t", events)
	if len(snap.Tasks) != 1 {
		t.Fatalf("快照 tasks = %+v", snap.Tasks)
	}
	if !snap.Tasks[0].Overdue || snap.Tasks[0].WavesSince != 2 {
		t.Fatalf("波龄投影错: %+v", snap.Tasks[0])
	}
}

// ---- assembleChat 接线（v1.46 修复 v1.36 注入失实：胶囊/任务/检索三面进语境）----

func TestAssembleChatMemoryFaces(t *testing.T) {
	store := NewMemStore()
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: nil,
		Seats: []AgentSeat{{ParticipantID: "par_codex", Profile: agent.Profile{ProfileID: "p", Adapter: "echo"}}},
		Clock: func() string { return "2026-09-03T00:00:00Z" }, Now: func() time.Time { return time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC) },
		NewID: counterNewID(), Tenant: "ten_t", ReactionWindow: time.Millisecond,
	})
	defer eng.Close()

	capsule := protocol.ClosureCapsule{
		ClosureID: "clo_1", ClosureType: "consensus", ThreadID: "thr_1",
		Conclusions: []string{"结论A"}, Assumptions: []string{},
		Evidence:       protocol.CapsuleEvidence{Support: []string{}, Oppose: []string{}},
		Participation:  protocol.CapsuleParticipation{Concluded: []string{}, Objected: []string{}, Abstained: []string{}, Timeout: []string{}, Unavailable: []string{}},
		ReopenTriggers: []string{"x"},
	}
	envs := []protocol.Envelope{
		{EventID: "evt_r1", Type: protocol.EventRoomCreated, Payload: mustJSON(map[string]any{"display_name": "T", "thread_id": "thr_1", "agents": []string{}}), Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Visibility: protocol.Visibility{Kind: "public"}, Metadata: map[string]any{}},
		{EventID: "evt_cap1", Type: protocol.EventClosureAccepted, Payload: mustJSON(protocol.ClosureAcceptedPayload{ClosureID: "clo_1", ClosureType: "consensus", ThreadID: "thr_1", Capsule: capsule}), Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Visibility: protocol.Visibility{Kind: "public"}, Metadata: map[string]any{}},
		// 旧消息含关键词（按需召回面）
		{EventID: "evt_old1", Type: protocol.EventMessagePosted, Payload: mustJSON(map[string]any{"body": "预算口径是 5 万"}), Actor: protocol.Actor{ParticipantID: "par_codex", Kind: "agent"}, Visibility: protocol.Visibility{Kind: "public"}, Metadata: map[string]any{}},
		// agent 宣言（tasklist 面）
		{EventID: "evt_decl1", Type: protocol.EventMessagePosted, Payload: mustJSON(map[string]any{"body": "好，我来拉数据。"}), Actor: protocol.Actor{ParticipantID: "par_codex", Kind: "agent"}, Visibility: protocol.Visibility{Kind: "public"}, Metadata: map[string]any{}},
		// 刺激（关键词：预算）
		{EventID: "evt_stim1", Type: protocol.EventMessagePosted, Payload: mustJSON(map[string]any{"body": "预算是不是超了"}), Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"}, Visibility: protocol.Visibility{Kind: "public"}, Metadata: map[string]any{}},
	}
	asm := eng.assembleChat(context.Background(), contextx.Config{
		RoomID: "room_t", TaskID: "rnd_1:eval", Mode: "chat",
		RecentWindow: 2,
	}, envs, envs[len(envs)-1], true /* proactive：带承诺指令 */)

	// 恒常平面：编辑后胶囊注入（v1.36 失实的接线修复）
	caps, _ := asm.Inline["capsules"].([]map[string]any)
	if len(caps) != 1 {
		t.Fatalf("capsule 注入 = %v（期望 1 条——v1.36 曾为死代码）", asm.Inline["capsules"])
	}
	// tasklist 面：pending 承诺带 owner
	tasks, _ := asm.Inline["tasklist"].([]contextx.TaskBrief)
	if len(tasks) != 1 || tasks[0].Owner != "par_codex" || tasks[0].Text != "好，我来拉数据" {
		t.Fatalf("tasklist 注入 = %+v", tasks)
	}
	// 按需平面：近窗外的旧消息按关键词召回（带 provenance）
	retrieved, _ := asm.Inline["retrieved"].([]contextx.RetrievedItem)
	if len(retrieved) != 1 || retrieved[0].EventID != "evt_old1" {
		t.Fatalf("retrieved 注入 = %+v", retrieved)
	}
	// proactive 波：OQ-A 承诺指令在场
	if _, ok := asm.Inline["tasklist_note"]; !ok {
		t.Fatal("proactive 波应注入承诺指令（tasklist_note）")
	}
	if _, ok := asm.Inline["proactive"]; !ok {
		t.Fatal("proactive 标记缺失")
	}
	// 层清单含三面
	names := map[string]bool{}
	for _, l := range asm.Layers {
		names[l.Name] = true
	}
	for _, want := range []string{"capsule_memory", "retrieved_memory", "tasklist"} {
		if !names[want] {
			t.Fatalf("层 %s 缺失：%v", want, asm.Layers)
		}
	}
}
