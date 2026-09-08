// 每波记忆评审（v1.70，RFC-0007 Hermes 同构补编）：波结束后最后发布者回看本波
// 转录 + 当前记忆清单，自助产出策展操作。机制对照 Hermes background_review.py：
// "After every turn … ask 'should any memory be saved or updated?'"——写入直达
// 存储（免审批）、不触碰在途波（与波共用房间串行队列天然互斥）、失败只记日志
//（评审是例行情报任务，不是座位能力面——失败不构成 seat status）。
package room

import (
	"context"
	"encoding/json"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// runMemoryReview 评审执行链：能力门 → 波转录+清单组装（Receipt 落账）→
// memory_review 任务 → 逐 op 校验应用 → memory.curated 留痕。
func (e *Engine) runMemoryReview(ctx context.Context, roomID string, ref reviewRef) {
	history, err := e.roomHistory(ctx, roomID)
	if err != nil {
		e.warn(roomID, "记忆评审：历史读取失败", "round", ref.roundID, "err", err)
		return
	}
	if len(history) == 0 {
		return
	}
	profile := e.profileOf(ref.assignee)
	if profile.ProfileID == "" {
		e.debug(roomID, "记忆评审：执行者已不在席，跳过", "round", ref.roundID, "assignee", ref.assignee)
		return
	}
	if e.cfg.Agents == nil || !e.cfg.Agents.CapableOf(profile, func(c agent.Capabilities) bool { return c.MemoryCuration }) {
		e.debug(roomID, "记忆评审：执行者无策展能力，跳过", "round", ref.roundID, "assignee", ref.assignee)
		return
	}

	wave, anchor, ok := waveTranscriptOf(history, ref.roundID)
	if !ok || len(wave) == 0 {
		return // 波内无 agent 发言（收尾竞态）——无可沉淀
	}

	envs := make([]protocol.Envelope, len(history))
	for i := range history {
		envs[i] = history[i].Envelope
	}
	entries := CuratedEntriesOf(history)
	seatsMin := e.minimalSeats(history)
	asm := e.assembleChat(ctx, contextx.Config{
		RoomID: roomID, TaskID: ref.roundID + ":mrev", Mode: "memory_review",
		Seats: seatsMin, RecentWindow: len(wave), Budget: contextx.BudgetState{},
	}, envs, anchor, false)
	// 评审专用语境：波转录 + 完整记忆清单（含预算水位——容量门倒逼合并的
	// 前提是模型看得见现状）。
	inventory := make([]map[string]any, 0, len(entries))
	for _, en := range entries {
		inventory = append(inventory, map[string]any{"id": en.ID, "content": en.Content})
	}
	asm.Inline["review_wave"] = wave
	asm.Inline["memory_inventory"] = inventory
	asm.Inline["memory_budget"] = map[string]any{
		"used_runes": CuratedRunesOf(entries), "limit_runes": CapsuleBudgetRunes,
		"entry_max_runes": CuratedEntryRunesMax,
	}

	result, err := e.runTask(ctx, profile, ref.assignee, agent.Task{
		TaskID:        e.cfg.NewID("tsk"),
		Kind:          agent.KindReviewMemory,
		ParticipantID: ref.assignee,
		RoomID:        roomID,
		Epoch:         ref.roundID,
		Context:       agent.Context{Inline: asm.Inline, ReceiptRef: asm.Receipt.ReceiptID},
	})
	if err != nil {
		e.warn(roomID, "记忆评审任务失败（本轮跳过，不阻塞房间）", "round", ref.roundID, "err", err)
		return
	}
	if result.Block != agent.BlockMemoryOps {
		e.warn(roomID, "记忆评审产物块非法", "round", ref.roundID, "block", result.Block)
		return
	}
	ops := decodeMemoryOps(result.Data)
	if len(ops) == 0 {
		e.debug(roomID, "记忆评审：无可沉淀", "round", ref.roundID, "assignee", ref.assignee)
		return
	}
	_, results := ApplyCuratedOps(entries, ops, CapsuleBudgetRunes, ref.assignee, ref.roundID, e.cfg.Clock())
	payload := protocol.MemoryCuratedPayload{
		Author: ref.assignee, RoundID: ref.roundID,
		Note: noteOf(result.Data), Ops: results,
		OccurredAt: e.cfg.Clock(),
	}
	env := e.newEnv(roomID, protocol.EventMemoryCurated,
		protocol.Actor{ParticipantID: ref.assignee, Kind: "agent"}, anchor.EventID, ref.roundID, payload)
	if _, err := e.append(ctx, env); err != nil {
		e.warn(roomID, "记忆评审写入失败", "round", ref.roundID, "err", err)
		return
	}
	applied, rejected := 0, 0
	for _, r := range results {
		if r.Status == "applied" {
			applied++
		} else {
			rejected++
		}
	}
	e.debug(roomID, "记忆评审完成", "round", ref.roundID, "assignee", ref.assignee,
		"ops", len(results), "applied", applied, "rejected", rejected)
}

// waveTranscriptOf 波转录：round.opened(correlation=roundID) 定位锚（causation），
// 收集 correlation=roundID 的 message.posted（时序）。
func waveTranscriptOf(history []StoredEvent, roundID string) (wave []map[string]any, anchor protocol.Envelope, ok bool) {
	anchorID := ""
	for _, ev := range history {
		env := ev.Envelope
		if env.Type == protocol.EventRoundOpened && env.CorrelationID != nil && *env.CorrelationID == roundID {
			var p protocol.RoundOpenedPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				anchorID = p.StimulusEventID
			}
		}
	}
	if anchorID == "" {
		return nil, anchor, false
	}
	for _, ev := range history {
		if ev.Envelope.EventID == anchorID {
			anchor = ev.Envelope
			ok = true
			break
		}
	}
	if !ok {
		return nil, anchor, false
	}
	var p struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(anchor.Payload, &p)
	wave = append(wave, map[string]any{
		"actor": anchor.Actor.ParticipantID, "kind": anchor.Actor.Kind,
		"body": p.Body, "role": "stimulus",
	})
	for _, ev := range history {
		env := ev.Envelope
		if env.Type != protocol.EventMessagePosted || env.CorrelationID == nil || *env.CorrelationID != roundID {
			continue
		}
		var mp struct {
			Body string `json:"body"`
		}
		if json.Unmarshal(env.Payload, &mp) != nil {
			continue
		}
		wave = append(wave, map[string]any{
			"actor": env.Actor.ParticipantID, "kind": env.Actor.Kind,
			"body": mp.Body, "role": "reply",
		})
	}
	return wave, anchor, true
}

// decodeMemoryOps 结构化块 → 协议操作（端口校验已保证形状；此处防御性过滤）。
func decodeMemoryOps(data map[string]any) []protocol.MemoryOp {
	raw, _ := data["ops"].([]any)
	ops := make([]protocol.MemoryOp, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		if m == nil {
			continue
		}
		op := protocol.MemoryOp{}
		op.Action, _ = m["action"].(string)
		op.Content, _ = m["content"].(string)
		op.OldText, _ = m["old_text"].(string)
		ops = append(ops, op)
	}
	return ops
}

func noteOf(data map[string]any) string {
	n, _ := data["public_rationale"].(string)
	return n
}

// minimalSeats 座位最小投影（组装层 participants 需要）。
func (e *Engine) minimalSeats(history []StoredEvent) []contextx.Seat {
	seats := []contextx.Seat{}
	for _, s := range e.roomSeats(history) {
		seats = append(seats, contextx.Seat{ParticipantID: s.ParticipantID})
	}
	return seats
}
