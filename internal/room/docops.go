// doc_ops 引擎代写（RFC-0014 §2.7）：agent 唯一文档写面——结构化 doc_ops 随
// public_draft 同波到达（透明纪律：公开说明消息 + doc_ref 卡片），引擎逐 op
// 校验、翻译为协议 DocOp 代写事件，actor 记为该 bot（memory_ops 代写先例平移）。
// op 失败不翻波：丢弃该 op 记日志，正文叙事照常发布（逐 op 诚实——记分卡透明
// 原则）；文档事件先于 message.posted 落库，卡片渲染时 refs 可解析。
package room

import (
	"context"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// DocProxy 引擎的文档代写/读取面（*doc.Service 结构满足；装配注入，nil = 无
// 文档面——doc_ops 一律降级为空 refs，房间照常运转）。
type DocProxy interface {
	CreateDoc(ctx context.Context, actor doc.Actor, title string, blocks []protocol.DocBlock, source, note string) (*doc.CommandResult, error)
	CommitRevision(ctx context.Context, actor doc.Actor, docID string, baseVersion int64, ops []protocol.DocOp, source, note string) (*doc.CommandResult, error)
	GetDoc(ctx context.Context, docID string) (doc.DocState, error)
}

// applyWaveDocOps 同波 doc_ops 处理：端口形状校验（防御适配器边界失守）→ 逐 op
// 翻译代写 → 收集 refs（去重，首触序）。返回注入消息载荷的 refs 列表（可为空）。
func (e *Engine) applyWaveDocOps(ctx context.Context, roomID, roundID, participantID string, raw any) []map[string]any {
	fail := func(msg string, args ...any) {
		e.warn(roomID, "doc_ops 代写降级："+msg, args...)
	}
	if e.cfg.Docs == nil {
		e.debug(roomID, "doc_ops 到达但本装配无文档面，全部丢弃", "round", roundID, "participant", participantID)
		return nil
	}
	if err := agent.ValidateBlock(agent.BlockDocOps, map[string]any{"ops": raw}); err != nil {
		fail("批次形状非法，整批丢弃", "round", roundID, "participant", participantID, "err", err)
		return nil
	}
	rawOps, _ := raw.([]any)
	actor := doc.Actor{ParticipantID: participantID, Kind: "agent"}
	var refs []map[string]any
	seen := map[string]bool{}
	addRef := func(docID, anchor string) {
		if seen[docID] {
			return
		}
		seen[docID] = true
		ref := map[string]any{"kind": "doc", "doc_id": docID}
		if anchor != "" {
			ref["anchor_block_id"] = anchor
		}
		refs = append(refs, ref)
	}
	for i, rawOp := range rawOps {
		op, _ := rawOp.(map[string]any)
		kind, _ := op["op"].(string)
		if kind == "create" {
			title, _ := op["title"].(string)
			res, err := e.cfg.Docs.CreateDoc(ctx, actor, title, waveBlocksOf(op), "agent", "")
			if err != nil {
				fail("create 失败", "round", roundID, "op", i, "err", err)
				continue
			}
			addRef(res.DocID, "")
			e.debug(roomID, "doc_ops create 已代写", "round", roundID, "participant", participantID, "doc", res.DocID)
			continue
		}
		docID, _ := op["doc_id"].(string)
		state, err := e.cfg.Docs.GetDoc(ctx, docID)
		if err != nil {
			fail("目标文档不可读", "round", roundID, "op", i, "doc", docID, "err", err)
			continue
		}
		translated, anchor, terr := e.translateDocOp(state, op)
		if terr != nil {
			fail("op 翻译失败", "round", roundID, "op", i, "doc", docID, "err", terr)
			continue
		}
		if _, err := e.cfg.Docs.CommitRevision(ctx, actor, docID, state.Version, translated, "agent", ""); err != nil {
			fail("revision 提交失败", "round", roundID, "op", i, "doc", docID, "err", err)
			continue
		}
		addRef(docID, anchor)
		e.debug(roomID, "doc_ops 已代写", "round", roundID, "participant", participantID, "doc", docID, "op", kind)
	}
	return refs
}

// waveBlocksOf agent 块 JSON → 协议块（block_id 缺省——服务端规范化分配）。
func waveBlocksOf(op map[string]any) []protocol.DocBlock {
	rawBlocks, _ := op["blocks"].([]any)
	out := make([]protocol.DocBlock, 0, len(rawBlocks))
	for _, rb := range rawBlocks {
		b, _ := rb.(map[string]any)
		typ, _ := b["type"].(string)
		text, _ := b["text"].(string)
		out = append(out, protocol.DocBlock{Type: typ, Text: text})
	}
	return out
}

// translateDocOp 编辑类 op 翻译（agents 想小节、存储想块——小节 = heading 块
// （文本精确匹配，首个命中）+ 其后至下一 heading 前的全部块）。返回协议 ops
// 与 refs 锚点块（insert_after 的锚 / section 的 heading 块）。
func (e *Engine) translateDocOp(state doc.DocState, op map[string]any) ([]protocol.DocOp, string, error) {
	kind, _ := op["op"].(string)
	switch kind {
	case "append":
		var ops []protocol.DocOp
		for _, b := range waveBlocksOf(op) {
			block := b
			ops = append(ops, protocol.DocOp{Op: "append", Block: &block})
		}
		return ops, "", nil
	case "insert_after":
		anchorID, _ := op["block_id"].(string)
		if blockIndexOf(state.Blocks, anchorID) < 0 {
			return nil, "", fmt.Errorf("锚点块不存在 %q", anchorID)
		}
		return e.chainedInserts(anchorID, waveBlocksOf(op)), anchorID, nil
	case "replace_section":
		heading, _ := op["heading"].(string)
		head, contents := sectionOf(state.Blocks, heading)
		if head < 0 {
			return nil, "", fmt.Errorf("小节不存在 %q（heading 文本锚未命中）", heading)
		}
		ops := make([]protocol.DocOp, 0, len(contents)+1)
		for _, idx := range contents {
			id := state.Blocks[idx].BlockID
			ops = append(ops, protocol.DocOp{Op: "delete", BlockID: &id})
		}
		ops = append(ops, e.chainedInserts(state.Blocks[head].BlockID, waveBlocksOf(op))...)
		return ops, state.Blocks[head].BlockID, nil
	case "delete_section":
		heading, _ := op["heading"].(string)
		head, contents := sectionOf(state.Blocks, heading)
		if head < 0 {
			return nil, "", fmt.Errorf("小节不存在 %q（heading 文本锚未命中）", heading)
		}
		ops := make([]protocol.DocOp, 0, len(contents)+1)
		headID := state.Blocks[head].BlockID
		ops = append(ops, protocol.DocOp{Op: "delete", BlockID: &headID})
		for _, idx := range contents {
			id := state.Blocks[idx].BlockID
			ops = append(ops, protocol.DocOp{Op: "delete", BlockID: &id})
		}
		return ops, headID, nil
	}
	return nil, "", fmt.Errorf("未知 op %q", kind)
}

// chainedInserts 多块顺序插入：引擎预分配块 ID 并逐块链锚（同锚重复
// insert_after 会逆序——链式锚定保序）。
func (e *Engine) chainedInserts(anchorID string, blocks []protocol.DocBlock) []protocol.DocOp {
	ops := make([]protocol.DocOp, 0, len(blocks))
	for _, b := range blocks {
		block := b
		block.BlockID = e.cfg.NewID("blk")
		anchor := anchorID
		ops = append(ops, protocol.DocOp{Op: "insert_after", BlockID: &anchor, Block: &block})
		anchorID = block.BlockID
	}
	return ops
}

// blockIndexOf 块 ID 定位（-1 = 不存在）。
func blockIndexOf(blocks []protocol.DocBlock, blockID string) int {
	for i := range blocks {
		if blocks[i].BlockID == blockID {
			return i
		}
	}
	return -1
}

// sectionOf 小节定位：heading 文本精确匹配的首个 heading 块索引 + 小节内容块
// 索引集（其后至下一 heading 前）。未命中 head=-1。
func sectionOf(blocks []protocol.DocBlock, heading string) (head int, contents []int) {
	head = -1
	for i, b := range blocks {
		if b.Type == "heading" && b.Text == heading {
			head = i
			break
		}
	}
	if head < 0 {
		return -1, nil
	}
	for i := head + 1; i < len(blocks); i++ {
		if blocks[i].Type == "heading" {
			break
		}
		contents = append(contents, i)
	}
	return head, contents
}

// sanitizeDocRefs 模型自报 refs 的白名单化（ROP/run 旁路发布面只透传形状合法
// 的引用——kind 封闭、doc_id 形态、≤8；不合法的项丢弃不声张，与
// SanitizeRelations 同纪律）。返回可入消息载荷的 []any。
func sanitizeDocRefs(raw any) []any {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := make([]any, 0, len(items))
	for _, r := range items {
		if len(out) >= 8 {
			break
		}
		m, _ := r.(map[string]any)
		if m == nil {
			continue
		}
		kind, _ := m["kind"].(string)
		docID, _ := m["doc_id"].(string)
		if kind != "doc" || !docIDPattern.MatchString(docID) {
			continue
		}
		ref := map[string]any{"kind": "doc", "doc_id": docID}
		if anchor, _ := m["anchor_block_id"].(string); strings.TrimSpace(anchor) != "" {
			ref["anchor_block_id"] = anchor
		}
		out = append(out, ref)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
