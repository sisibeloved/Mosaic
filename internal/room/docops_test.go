// UT 层：RFC-0014 §2.7 agent doc_ops 写面——同波代写（文档事件先行、refs 随
// 正文同波发布、actor 记为该 bot）、逐 op 失败降级、批次形状门、小节翻译
// （agents 想小节、存储想块）。
package room

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ---- doc_ops 发言桩：意愿同 recap 桩（人类锚发言、agent 锚静默），生成恒带
// 配置的 doc_ops 侧车 ----

type docOpsAdapter struct{ ops []any }

func (docOpsAdapter) Name() string                     { return "doc_ops_stub" }
func (docOpsAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (a docOpsAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return docOpsSession{ops: a.ops}, nil
}

type docOpsSession struct{ ops []any }

func (s docOpsSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return docOpsHandle{task: task, ops: s.ops}, nil
}
func (docOpsSession) Cancel(string) {}
func (docOpsSession) Close()        {}

type docOpsHandle struct {
	task agent.Task
	ops  []any
}

func (docOpsHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (docOpsHandle) Cancel()                           {}

func (h docOpsHandle) Result() (agent.Result, error) {
	switch h.task.Kind {
	case agent.KindEvaluateIntent:
		action := "speak"
		recent, _ := h.task.Context.Inline["recent"].([]map[string]any)
		if len(recent) > 0 {
			if kind, _ := recent[len(recent)-1]["kind"].(string); kind != "human" {
				action = "silent" // 锚=agent 消息自决静默，链收敛
			}
		}
		return agent.Result{Block: "turn_intent", Data: map[string]any{
			"action": action, "type": "extend", "public_rationale": "doc ops stub",
			"scores": map[string]any{"relevance": 0.5, "novelty": 0.5, "urgency": 0.5, "confidence": 0.5},
		}}, nil
	case agent.KindGenerate:
		return agent.Result{Block: "public_draft", Data: map[string]any{
			"body": "我把结论写成了文档", "declared_relations": []any{}, "doc_ops": h.ops,
		}}, nil
	}
	return agent.Result{Block: "unsupported"}, nil
}

// seedDocWithBlocks 直落 doc.created + 首条 revision（带显式块 ID——小节/锚点测试用）。
func seedDocWithBlocks(t *testing.T, docs *doc.MemStore, docID string, blocks ...protocol.DocBlock) {
	t.Helper()
	envs := []protocol.DocEnvelope{{
		EventID: "evt_seedc_" + docID, TenantID: "ten_local", DocID: docID,
		Type: protocol.EventDocCreated, SchemaVersion: 1, OccurredAt: testClock(),
		Actor:   protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload: mustJSON(protocol.DocCreatedPayload{
			DocID: docID, Title: "seeded " + docID, Format: "markdown", CreatedBy: "par_owner"}),
		Metadata: map[string]any{},
	}}
	if len(blocks) > 0 {
		ops := make([]protocol.DocOp, 0, len(blocks))
		for _, b := range blocks {
			block := b
			ops = append(ops, protocol.DocOp{Op: "append", Block: &block})
		}
		envs = append(envs, protocol.DocEnvelope{
			EventID: "evt_seedr_" + docID, TenantID: "ten_local", DocID: docID,
			Type: protocol.EventDocRevisionCommitted, SchemaVersion: 1, OccurredAt: testClock(),
			Actor:   protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Payload: mustJSON(protocol.DocRevisionCommittedPayload{
				DocID: docID, BaseVersion: 1, Version: 2, Ops: ops, Actor: "par_owner", Source: "human_editor"}),
			Metadata: map[string]any{},
		})
	}
	if _, err := docs.AppendDocEvents(context.Background(), envs); err != nil {
		t.Fatalf("seed doc with blocks: %v", err)
	}
}

// newDocOpsWave 一波 doc_ops 集成装置：单座引擎（Docs 面接真实 doc.Service over
// MemStore）+ 建房 + seed（波前注入既有文档）+ 人类刺激 → 等两波收束（波1 发言、
// 波2 静默）。返回房间存储、文档存储与文档服务（断言投影用）。
func newDocOpsWave(t *testing.T, ops []any, seed func(*doc.MemStore)) (*MemStore, *doc.MemStore, *doc.Service) {
	t.Helper()
	store := NewMemStore()
	docStore := doc.NewMemStore()
	docSvc := doc.NewService(doc.Config{
		Store: docStore, Clock: testClock,
		NewID:    counterNewID(),
		NewDocID: func() string { return "doc_0000000000aa" },
		Tenant:   "ten_local",
	})
	if seed != nil {
		seed(docStore) // 波前注入：doc_ops 编辑目标须先于波存在
	}
	sup := agent.NewSupervisor()
	_ = sup.Register(docOpsAdapter{ops: ops})
	t.Cleanup(sup.Shutdown)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_docbot", Profile: agent.Profile{ProfileID: "pd", Adapter: "doc_ops_stub"}},
		},
		Budget:         contextx.Limits{},
		ReactionWindow: 5 * time.Millisecond,
		Docs:           docSvc,
		Clock:          testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	t.Cleanup(eng.Close)

	seedRoomCreatedFor(t, store, "room_docops")
	deliverHuman(t, store, eng, "room_docops")
	waitRoundsClosed(t, store, "room_docops", 2) // 波1 published → 波2 自决静默收束
	time.Sleep(100 * time.Millisecond)
	return store, docStore, docSvc
}

// waveAgentMessage 取本波 agent 发言的 message.posted 载荷。
func waveAgentMessage(t *testing.T, store *MemStore, roomID string) map[string]any {
	t.Helper()
	for _, ev := range store.RoomEvents(roomID) {
		if ev.Type != protocol.EventMessagePosted || ev.Actor.Kind != "agent" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		return p
	}
	t.Fatal("波内应有 agent 发言")
	return nil
}

// 同波代写主链：create + append → 文档落库（actor=该 bot、source=agent）、
// 正文 refs 首触序、写面指令不进消息载荷、refs 可解析（文档事件先行语义）。
func TestWaveDocOpsCreateAndAppend(t *testing.T) {
	ops := []any{
		map[string]any{"op": "create", "title": "波内新文档", "blocks": []any{
			map[string]any{"type": "heading", "text": "结论"},
			map[string]any{"type": "paragraph", "text": "本期共识两条。"},
		}},
		map[string]any{"op": "append", "doc_id": "doc_0000000000bb", "blocks": []any{
			map[string]any{"type": "paragraph", "text": "追加的一段。"},
		}},
	}
	store, docStore, docSvc := newDocOpsWave(t, ops, func(docs *doc.MemStore) {
		seedDocWithBlocks(t, docs, "doc_0000000000bb",
			protocol.DocBlock{BlockID: "b_seed", Type: "paragraph", Text: "原有内容"})
	})

	// create：created + 首 revision 同批落库，actor 记为代写 bot
	created := docStore.DocEvents("doc_0000000000aa")
	if len(created) != 2 {
		t.Fatalf("新文档应落 created+revision 两事件，实际 %d", len(created))
	}
	for _, ev := range created {
		if ev.Actor.ParticipantID != "par_docbot" || ev.Actor.Kind != "agent" {
			t.Fatalf("文档事件 actor 应为代写 bot，实际 %+v", ev.Actor)
		}
	}
	state, err := docSvc.GetDoc(context.Background(), "doc_0000000000aa")
	if err != nil {
		t.Fatalf("get created doc: %v", err)
	}
	if state.Title != "波内新文档" || len(state.Blocks) != 2 {
		t.Fatalf("新文档投影不符：title=%q blocks=%d", state.Title, len(state.Blocks))
	}

	// append：seed 文档末块为追加内容，末事件为该 bot 代写的 revision
	appended := docStore.DocEvents("doc_0000000000bb")
	last := appended[len(appended)-1]
	if last.Type != protocol.EventDocRevisionCommitted || last.Actor.Kind != "agent" ||
		last.Actor.ParticipantID != "par_docbot" {
		t.Fatalf("append 应为 agent 代写的 revision，实际 %+v", last)
	}
	bb, err := docSvc.GetDoc(context.Background(), "doc_0000000000bb")
	if err != nil {
		t.Fatalf("get appended doc: %v", err)
	}
	if got := bb.Blocks[len(bb.Blocks)-1].Text; got != "追加的一段。" {
		t.Fatalf("末块应为追加内容，实际 %q", got)
	}

	// refs 首触序随正文同波发布；写面指令本身不进消息载荷
	p := waveAgentMessage(t, store, "room_docops")
	if _, leaked := p["doc_ops"]; leaked {
		t.Fatal("doc_ops 写面指令不应进消息载荷")
	}
	refs, _ := p["refs"].([]any)
	if len(refs) != 2 {
		t.Fatalf("refs 应两条（create + append），实际 %v", p["refs"])
	}
	ref0, _ := refs[0].(map[string]any)
	ref1, _ := refs[1].(map[string]any)
	if ref0["doc_id"] != "doc_0000000000aa" || ref0["kind"] != "doc" ||
		ref1["doc_id"] != "doc_0000000000bb" {
		t.Fatalf("refs 首触序不符：%v / %v", refs[0], refs[1])
	}
}

// 逐 op 失败降级：目标文档不存在 → 丢该 op 记日志，波不翻、正文照常发布、
// 无 refs、不存在的文档不落任何事件。
func TestWaveDocOpsFailureDegrades(t *testing.T) {
	ops := []any{
		map[string]any{"op": "append", "doc_id": "doc_000000000099", "blocks": []any{
			map[string]any{"type": "paragraph", "text": "写到不存在的文档。"},
		}},
	}
	store, docStore, _ := newDocOpsWave(t, ops, nil)

	p := waveAgentMessage(t, store, "room_docops") // 波不翻：正文照常发布
	if _, ok := p["refs"]; ok {
		t.Fatalf("全部 op 降级后不应有 refs，实际 %v", p["refs"])
	}
	if evs := docStore.DocEvents("doc_000000000099"); len(evs) != 0 {
		t.Fatalf("不存在的文档不应落事件，实际 %d", len(evs))
	}
}

// 批次形状门：超 MaxDocOpsPerWave 的批次整批丢弃（端口形状校验防御适配器边界
// 失守）——不逐条执行、不动目标文档、正文照常发布且无 refs。
func TestWaveDocOpsInvalidBatchDropped(t *testing.T) {
	ops := make([]any, 0, agent.MaxDocOpsPerWave+1)
	for i := 0; i <= agent.MaxDocOpsPerWave; i++ {
		ops = append(ops, map[string]any{
			"op": "delete_section", "doc_id": "doc_0000000000bb", "heading": "目标",
		})
	}
	store, docStore, _ := newDocOpsWave(t, ops, func(docs *doc.MemStore) {
		seedDocWithBlocks(t, docs, "doc_0000000000bb",
			protocol.DocBlock{BlockID: "b_h", Type: "heading", Text: "目标"},
			protocol.DocBlock{BlockID: "b_p", Type: "paragraph", Text: "内容"})
	})

	p := waveAgentMessage(t, store, "room_docops")
	if _, ok := p["refs"]; ok {
		t.Fatalf("非法批次整批丢弃后不应有 refs，实际 %v", p["refs"])
	}
	if evs := docStore.DocEvents("doc_0000000000bb"); len(evs) != 2 {
		t.Fatalf("整批丢弃不应动目标文档（仍 seed 的 2 事件），实际 %d", len(evs))
	}
}

// 小节翻译单测：replace_section 保留 heading、删内容块、链式插入保序；
// delete_section 连 heading 一起删；heading 文本锚/块锚未命中 → 报错（上层丢
// op 降级）。
func TestTranslateDocOpSections(t *testing.T) {
	eng := &Engine{cfg: EngineConfig{NewID: counterNewID()}}
	state := doc.DocState{Blocks: []protocol.DocBlock{
		{BlockID: "b_h1", Type: "heading", Text: "目标"},
		{BlockID: "b_p1", Type: "paragraph", Text: "旧一"},
		{BlockID: "b_p2", Type: "paragraph", Text: "旧二"},
		{BlockID: "b_h2", Type: "heading", Text: "计划"},
		{BlockID: "b_p3", Type: "paragraph", Text: "保留"},
	}}

	// replace_section：删 b_p1/b_p2，新块链式锚定 b_h1 → blk_c1
	ops, anchor, err := eng.translateDocOp(state, map[string]any{
		"op": "replace_section", "doc_id": "doc_0000000000bb", "heading": "目标",
		"blocks": []any{
			map[string]any{"type": "paragraph", "text": "新一"},
			map[string]any{"type": "paragraph", "text": "新二"},
		},
	})
	if err != nil {
		t.Fatalf("translate replace_section: %v", err)
	}
	if anchor != "b_h1" {
		t.Fatalf("refs 锚点应为小节 heading 块，实际 %q", anchor)
	}
	if len(ops) != 4 || ops[0].Op != "delete" || *ops[0].BlockID != "b_p1" ||
		ops[1].Op != "delete" || *ops[1].BlockID != "b_p2" ||
		ops[2].Op != "insert_after" || *ops[2].BlockID != "b_h1" ||
		ops[3].Op != "insert_after" || *ops[3].BlockID != "blk_c1" {
		t.Fatalf("replace_section 翻译不符：%+v", ops)
	}
	next, statuses := doc.ApplyDocOps(state.Blocks, ops, nil)
	for _, st := range statuses {
		if st.Status != "applied" {
			t.Fatalf("op %d 未应用：%s", st.Index, st.Reason)
		}
	}
	ids := make([]string, 0, len(next))
	for _, b := range next {
		ids = append(ids, b.BlockID)
	}
	if strings.Join(ids, ",") != "b_h1,blk_c1,blk_c2,b_h2,b_p3" {
		t.Fatalf("替换后块序不符：%v", ids)
	}
	if next[1].Text != "新一" || next[2].Text != "新二" {
		t.Fatalf("新块文本不符：%q / %q", next[1].Text, next[2].Text)
	}

	// delete_section：heading 连同小节内容一起删
	ops, anchor, err = eng.translateDocOp(state, map[string]any{
		"op": "delete_section", "doc_id": "doc_0000000000bb", "heading": "目标",
	})
	if err != nil {
		t.Fatalf("translate delete_section: %v", err)
	}
	if anchor != "b_h1" || len(ops) != 3 {
		t.Fatalf("delete_section 翻译不符：anchor=%q ops=%+v", anchor, ops)
	}
	next, _ = doc.ApplyDocOps(state.Blocks, ops, nil)
	ids = ids[:0]
	for _, b := range next {
		ids = append(ids, b.BlockID)
	}
	if strings.Join(ids, ",") != "b_h2,b_p3" {
		t.Fatalf("删除后余块不符：%v", ids)
	}

	// heading 文本锚未命中 / insert_after 锚点块不存在 → 报错
	if _, _, err = eng.translateDocOp(state, map[string]any{
		"op": "replace_section", "doc_id": "doc_0000000000bb", "heading": "不存在",
		"blocks": []any{map[string]any{"type": "paragraph", "text": "x"}},
	}); err == nil {
		t.Fatal("heading 未命中应报错")
	}
	if _, _, err = eng.translateDocOp(state, map[string]any{
		"op": "insert_after", "doc_id": "doc_0000000000bb", "block_id": "b_none",
		"blocks": []any{map[string]any{"type": "paragraph", "text": "x"}},
	}); err == nil {
		t.Fatal("锚点块不存在应报错")
	}
}
