// sheet_ops 引擎代写（RFC-0017 §4.3，docops.go 同纪律平移）：agent 表格写面
// ——结构化 sheet_ops 随 public_draft 同波到达，引擎逐 op 翻译为协议 SheetOp
// 代写 doc.sheet_revision_committed，actor 记为该 bot。编辑类 op 携带
// base_version（RFC 原文"修改必须携带所依据的版本"）——失配即 409 逐 op
// 降级（不翻波、正文照常发布）。
package room

import (
	"context"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// SheetProxy 引擎的表格代写/读取面（*doc.Service 结构满足；装配注入，nil =
// 无表格面——sheet_ops 一律降级为空 refs，房间照常运转）。
type SheetProxy interface {
	CreateSheetDoc(ctx context.Context, actor doc.Actor, title string, initialCells []protocol.SheetCell, source, note string) (*doc.CommandResult, error)
	CommitSheetRevision(ctx context.Context, actor doc.Actor, docID string, baseVersion int64, ops []protocol.SheetOp, source, note string) (*doc.CommandResult, error)
	GetDoc(ctx context.Context, docID string) (doc.DocState, error)
}

// applyWaveSheetOps 同波 sheet_ops 处理：端口形状校验（防御适配器边界失守）→
// 逐 op 翻译代写 → 收集 refs（kind=doc 与 docops 同形）。
func (e *Engine) applyWaveSheetOps(ctx context.Context, roomID, roundID, participantID string, raw any) []map[string]any {
	fail := func(msg string, args ...any) {
		e.warn(roomID, "sheet_ops 代写降级："+msg, args...)
	}
	if e.cfg.Sheets == nil {
		e.debug(roomID, "sheet_ops 到达但本装配无表格面，全部丢弃", "round", roundID, "participant", participantID)
		return nil
	}
	if err := agent.ValidateBlock(agent.BlockSheetOps, map[string]any{"ops": raw}); err != nil {
		fail("批次形状非法，整批丢弃", "round", roundID, "participant", participantID, "err", err)
		return nil
	}
	rawOps, _ := raw.([]any)
	actor := doc.Actor{ParticipantID: participantID, Kind: "agent"}
	var refs []map[string]any
	seen := map[string]bool{}
	addRef := func(docID string) {
		if seen[docID] {
			return
		}
		seen[docID] = true
		refs = append(refs, map[string]any{"kind": "doc", "doc_id": docID})
	}
	for i, rawOp := range rawOps {
		op, _ := rawOp.(map[string]any)
		kind, _ := op["op"].(string)
		if kind == "create" {
			title, _ := op["title"].(string)
			res, err := e.cfg.Sheets.CreateSheetDoc(ctx, actor, title, waveSheetCellsOf(op), "agent", "")
			if err != nil {
				fail("create 失败", "round", roundID, "op", i, "err", err)
				continue
			}
			addRef(res.DocID)
			e.debug(roomID, "sheet_ops create 已代写", "round", roundID, "participant", participantID, "doc", res.DocID)
			continue
		}
		docID, _ := op["doc_id"].(string)
		translated, terr := translateSheetOp(op)
		if terr != nil {
			fail("op 翻译失败", "round", roundID, "op", i, "doc", docID, "err", terr)
			continue
		}
		if translated.SheetID == "" {
			// 默认表：由 doc_id 派生（与服务端投影 NewWorkbookForDoc 同规则）
			translated.SheetID = "sht_" + strings.TrimPrefix(docID, "doc_")
		}
		bv, _ := agent.WaveNum(op["base_version"])
		if _, err := e.cfg.Sheets.CommitSheetRevision(ctx, actor, docID, int64(bv), []protocol.SheetOp{translated}, "agent", ""); err != nil {
			fail("sheet revision 提交失败（含 base_version 失配 409）", "round", roundID, "op", i, "doc", docID, "err", err)
			continue
		}
		addRef(docID)
		e.debug(roomID, "sheet_ops 已代写", "round", roundID, "participant", participantID, "doc", docID, "op", kind)
	}
	return refs
}

// waveSheetCellsOf agent create op 的 cells JSON → 协议格（端口已校验形状）。
func waveSheetCellsOf(op map[string]any) []protocol.SheetCell {
	rawCells, _ := op["cells"].([]any)
	out := make([]protocol.SheetCell, 0, len(rawCells))
	for _, rc := range rawCells {
		c, _ := rc.(map[string]any)
		cell, ok := waveSheetCell(c)
		if !ok {
			continue
		}
		out = append(out, cell)
	}
	return out
}

// waveSheetCell 单格 JSON → 协议格。
func waveSheetCell(c map[string]any) (protocol.SheetCell, bool) {
	r, _ := agent.WaveNum(c["r"])
	cc, _ := agent.WaveNum(c["c"])
	cell := protocol.SheetCell{R: int(r), C: int(cc)}
	switch typ, _ := c["type"].(string); typ {
	case "text":
		cell.Type = "text"
		cell.Text, _ = c["text"].(string)
	case "number":
		cell.Type = "number"
		v, _ := c["number"].(float64)
		cell.Number = &v
	case "bool":
		cell.Type = "bool"
		v, _ := c["bool"].(bool)
		cell.Bool = &v
	case "formula":
		cell.Type = "formula"
		cell.Formula, _ = c["formula"].(string)
	case "null":
		cell.Type = "null"
	default:
		return protocol.SheetCell{}, false
	}
	return cell, true
}

// translateSheetOp 编辑类 agent op → 协议 SheetOp。sheet_id 空 = 默认表
// （由 doc_id 派生，与服务端投影同规则）。
func translateSheetOp(op map[string]any) (protocol.SheetOp, error) {
	kind, _ := op["op"].(string)
	sheetID, _ := op["sheet_id"].(string)
	out := protocol.SheetOp{Op: mapAgentSheetOp(kind), SheetID: sheetID}
	switch kind {
	case "add_sheet":
		out.Name, _ = op["name"].(string)
	case "set_cells":
		rawCells, _ := op["cells"].([]any)
		out.Cells = make([]protocol.SheetCell, 0, len(rawCells))
		for _, rc := range rawCells {
			c, _ := rc.(map[string]any)
			if cell, ok := waveSheetCell(c); ok {
				out.Cells = append(out.Cells, cell)
			}
		}
	case "clear_range":
		if r, ok := op["range"].(map[string]any); ok {
			out.Range = waveSheetRange(r)
			out.KeepStyle = false
		}
	case "insert_rows", "insert_columns", "delete_rows", "delete_columns":
		at, _ := agent.WaveNum(op["at"])
		cnt, _ := agent.WaveNum(op["count"])
		out.At = int(at)
		out.Count = int(cnt)
	default:
		return protocol.SheetOp{}, errSheetTranslate
	}
	return out, nil
}

var errSheetTranslate = &sheetTranslateError{}

type sheetTranslateError struct{}

func (*sheetTranslateError) Error() string { return "未知 sheet op" }

// mapAgentSheetOp agent op 名 → 协议 op 名（insert_rows 等两侧同名，显式表
// 以防将来分叉）。
func mapAgentSheetOp(kind string) string {
	switch kind {
	case "add_sheet":
		return "add_sheet"
	case "set_cells":
		return "set_cells"
	case "clear_range":
		return "clear_range"
	case "insert_rows":
		return "insert_rows"
	case "insert_columns":
		return "insert_columns"
	case "delete_rows":
		return "delete_rows"
	case "delete_columns":
		return "delete_columns"
	}
	return kind
}

func waveSheetRange(r map[string]any) *protocol.SheetRange {
	r1, _ := agent.WaveNum(r["r1"])
	c1, _ := agent.WaveNum(r["c1"])
	r2, _ := agent.WaveNum(r["r2"])
	c2, _ := agent.WaveNum(r["c2"])
	return &protocol.SheetRange{R1: int(r1), C1: int(c1), R2: int(r2), C2: int(c2)}
}
