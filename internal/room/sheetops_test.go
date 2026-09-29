// UT 层：RFC-0017 §4.3 agent sheet_ops 写面——同波代写（sheet 修订先行、refs
// 随正文同波、actor 记为该 bot）、默认表 ID 解析、base_version 失配逐 op 降级、
// 写面指令不进消息载荷。
package room

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ---- sheet_ops 发言桩（docOpsAdapter 同构，侧车键换 sheet_ops）----

type sheetOpsAdapter struct{ ops []any }

func (sheetOpsAdapter) Name() string                     { return "sheet_ops_stub" }
func (sheetOpsAdapter) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (a sheetOpsAdapter) Boot(context.Context, agent.Profile) (agent.Session, error) {
	return sheetOpsSession{ops: a.ops}, nil
}

type sheetOpsSession struct{ ops []any }

func (s sheetOpsSession) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return sheetOpsHandle{task: task, ops: s.ops}, nil
}
func (sheetOpsSession) Cancel(string) {}
func (sheetOpsSession) Close()        {}

type sheetOpsHandle struct {
	task agent.Task
	ops  []any
}

func (sheetOpsHandle) Updates() <-chan agent.DraftUpdate { return nil }
func (sheetOpsHandle) Cancel()                           {}

func (h sheetOpsHandle) Result() (agent.Result, error) {
	switch h.task.Kind {
	case agent.KindEvaluateIntent:
		action := "speak"
		recent, _ := h.task.Context.Inline["recent"].([]map[string]any)
		if len(recent) > 0 {
			if kind, _ := recent[len(recent)-1]["kind"].(string); kind != "human" {
				action = "silent"
			}
		}
		return agent.Result{Block: "turn_intent", Data: map[string]any{
			"action": action, "type": "extend", "public_rationale": "sheet ops stub",
			"scores": map[string]any{"relevance": 0.5, "novelty": 0.5, "urgency": 0.5, "confidence": 0.5},
		}}, nil
	case agent.KindGenerate:
		return agent.Result{Block: "public_draft", Data: map[string]any{
			"body": "我把数据写进了表格", "declared_relations": []any{}, "sheet_ops": h.ops,
		}}, nil
	}
	return agent.Result{Block: "unsupported"}, nil
}

// seedSheetDoc 直落 doc.created(format=spreadsheet)——默认表 ID = sht_ + doc_id 后缀。
func seedSheetDoc(t *testing.T, docs *doc.MemStore, docID string) {
	t.Helper()
	if _, err := docs.AppendDocEvents(context.Background(), []protocol.DocEnvelope{{
		EventID: "evt_seedsc_" + docID, TenantID: "ten_local", DocID: docID,
		Type: protocol.EventDocCreated, SchemaVersion: 1, OccurredAt: testClock(),
		Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload: mustJSON(protocol.DocCreatedPayload{
			DocID: docID, Title: "seeded sheet " + docID, Format: "spreadsheet", CreatedBy: "par_owner"}),
		Metadata: map[string]any{},
	}}); err != nil {
		t.Fatalf("seed sheet doc: %v", err)
	}
}

// newSheetOpsWave 一波 sheet_ops 集成装置（newDocOpsWave 同构，Docs/Sheets 双面注入）。
func newSheetOpsWave(t *testing.T, ops []any, seed func(*doc.MemStore)) (*MemStore, *doc.MemStore, *doc.Service) {
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
		seed(docStore)
	}
	sup := agent.NewSupervisor()
	_ = sup.Register(sheetOpsAdapter{ops: ops})
	t.Cleanup(sup.Shutdown)
	eng := NewEngine(EngineConfig{
		Store: store, Reader: store, Agents: sup,
		Seats: []AgentSeat{
			{ParticipantID: "par_sheetbot", Profile: agent.Profile{ProfileID: "ps", Adapter: "sheet_ops_stub"}},
		},
		Budget:         contextx.Limits{},
		ReactionWindow: 5 * time.Millisecond,
		Docs:           docSvc,
		Sheets:         docSvc,
		Clock:          testClock, Now: time.Now,
		NewID: counterNewID(), Tenant: "ten_local",
	})
	t.Cleanup(eng.Close)

	seedRoomCreatedFor(t, store, "room_sheetops")
	deliverHuman(t, store, eng, "room_sheetops")
	waitRoundsClosed(t, store, "room_sheetops", 2)
	time.Sleep(100 * time.Millisecond)
	return store, docStore, docSvc
}

// 同波代写主链：create（初始格随 created+revision 同批）+ set_cells（默认表 ID
// 解析 + base_version 生效）→ 修订落库（actor=该 bot、source=agent）、refs 随
// 正文同波、sheet_ops 键不进载荷。
func TestWaveSheetOpsCreateAndEdit(t *testing.T) {
	ops := []any{
		map[string]any{"op": "create", "title": "波内新表格", "cells": []any{
			map[string]any{"r": 1, "c": 1, "type": "text", "text": "项目"},
			map[string]any{"r": 2, "c": 1, "type": "formula", "formula": "SUM(B1:B3)"},
		}},
		map[string]any{"op": "set_cells", "doc_id": "doc_0000000000bb", "base_version": float64(1), "cells": []any{
			map[string]any{"r": 1, "c": 1, "type": "number", "number": float64(5)},
		}},
	}
	store, docStore, docSvc := newSheetOpsWave(t, ops, func(docs *doc.MemStore) {
		seedSheetDoc(t, docs, "doc_0000000000bb")
	})

	// create：created + 首条 sheet revision（初始格）同批落库
	created := docStore.DocEvents("doc_0000000000aa")
	if len(created) != 2 || created[1].Type != protocol.EventDocSheetRevisionCommitted {
		t.Fatalf("新表格应落 created+sheet revision 两事件，实际 %d", len(created))
	}
	for _, ev := range created {
		if ev.Actor.ParticipantID != "par_sheetbot" || ev.Actor.Kind != "agent" {
			t.Fatalf("表格事件 actor 应为代写 bot，实际 %+v", ev.Actor)
		}
	}
	state, err := docSvc.GetDoc(context.Background(), "doc_0000000000aa")
	if err != nil {
		t.Fatal(err)
	}
	if state.Format != "spreadsheet" || state.Workbook == nil || state.Workbook.Sheets[0].CellAt(2, 1) == nil {
		t.Fatal("agent 建表初始格未落")
	}

	// set_cells：seed 表格默认表（sht_ + doc_id 后缀）写入 + actor/source 断言
	appended := docStore.DocEvents("doc_0000000000bb")
	last := appended[len(appended)-1]
	if last.Type != protocol.EventDocSheetRevisionCommitted || last.Actor.ParticipantID != "par_sheetbot" {
		t.Fatalf("set_cells 应为 agent 代写 sheet revision，实际 %+v", last)
	}
	var p protocol.DocSheetRevisionCommittedPayload
	if err := json.Unmarshal(last.Payload, &p); err != nil || len(p.Ops) != 1 {
		t.Fatalf("sheet revision 载荷 = %v", err)
	}
	if p.Ops[0].SheetID != "sht_0000000000bb" {
		t.Fatalf("默认表 ID 应由 doc_id 派生，实际 %q", p.Ops[0].SheetID)
	}
	bb, err := docSvc.GetDoc(context.Background(), "doc_0000000000bb")
	if err != nil {
		t.Fatal(err)
	}
	if cell := bb.Workbook.Sheets[0].CellAt(1, 1); cell == nil || cell.Value == nil || *cell.Value.Number != 5 {
		t.Fatalf("写入格 = %+v", cell)
	}

	// refs 随正文同波；写面指令不进载荷
	msg := waveAgentMessage(t, store, "room_sheetops")
	if _, leaked := msg["sheet_ops"]; leaked {
		t.Fatal("sheet_ops 写面指令不应进消息载荷")
	}
	refs, _ := msg["refs"].([]any)
	if len(refs) != 2 {
		t.Fatalf("refs 应两条（create + set_cells），实际 %v", msg["refs"])
	}
}

// base_version 失配（409）逐 op 降级：op 丢弃、正文照常发布、无该 ref。
func TestWaveSheetOpsVersionConflictDegrades(t *testing.T) {
	ops := []any{
		map[string]any{"op": "add_sheet", "doc_id": "doc_0000000000bb", "base_version": float64(99), "name": "二月"},
	}
	store, docStore, _ := newSheetOpsWave(t, ops, func(docs *doc.MemStore) {
		seedSheetDoc(t, docs, "doc_0000000000bb") // version = 1，op 声称 99
	})
	events := docStore.DocEvents("doc_0000000000bb")
	if len(events) != 1 {
		t.Fatalf("失配 op 不应落库修订，实际 %d 事件", len(events))
	}
	msg := waveAgentMessage(t, store, "room_sheetops")
	if _, has := msg["refs"]; has {
		t.Fatalf("失配 op 不应产生 ref，实际 %v", msg["refs"])
	}
}
