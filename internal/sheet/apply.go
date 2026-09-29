// 机械应用（事件回放路径）：归一化 ops 直接落工作簿，不做业务校验——校验
// 已在提交时完成。防御性错误（未知表/未知 op）返回 err，由投影层跳过该 op
// （与 ApplyDocOps 的容错语义一致）。
package sheet

import (
	"fmt"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ApplySheetOps 逐条机械应用（投影回放）。
func ApplySheetOps(wb *Workbook, ops []protocol.SheetOp) error {
	for _, op := range ops {
		if err := ApplySheetOp(wb, op); err != nil {
			return err
		}
	}
	return nil
}

// ApplySheetOp 单条机械应用。
func ApplySheetOp(wb *Workbook, op protocol.SheetOp) error {
	switch op.Op {
	case OpSetCells:
		sh := wb.SheetByID(op.SheetID)
		if sh == nil {
			return fmt.Errorf("sheet %s 不存在", op.SheetID)
		}
		for _, c := range op.Cells {
			coord := Coord{R: c.R, C: c.C}
			cur := sh.Cells[coord]
			var next *Cell
			switch c.Type {
			case "formula":
				next = &Cell{Formula: c.Formula}
			case "text":
				v := c.Text
				next = &Cell{Value: &protocol.SheetCellValue{Type: "text", Text: v}}
			case "datetime":
				next = &Cell{Value: &protocol.SheetCellValue{Type: "datetime", Text: c.Text}}
			case "number":
				n := *c.Number
				next = &Cell{Value: &protocol.SheetCellValue{Type: "number", Number: &n}}
			case "bool":
				b := *c.Bool
				next = &Cell{Value: &protocol.SheetCellValue{Type: "bool", Bool: &b}}
			case "null":
				next = &Cell{}
			default:
				return fmt.Errorf("未知单元格类型 %q", c.Type)
			}
			// 样式：未携带保留既有；携带则按字段补丁合并
			base := cur
			if c.Style != nil {
				st := mergeStyle(base, c.Style)
				next.Style = st
			} else if base != nil {
				next.Style = base.Style
			}
			if next.IsEmpty() {
				delete(sh.Cells, coord)
			} else {
				sh.Cells[coord] = next
			}
			sh.growDim(c.R, c.C)
		}
		return nil

	case OpClearRange:
		sh := wb.SheetByID(op.SheetID)
		if sh == nil || op.Range == nil {
			return fmt.Errorf("clear_range 参数缺失")
		}
		for r := op.Range.R1; r <= op.Range.R2; r++ {
			for c := op.Range.C1; c <= op.Range.C2; c++ {
				coord := Coord{R: r, C: c}
				cur := sh.Cells[coord]
				if cur == nil {
					continue
				}
				next := &Cell{}
				if op.KeepStyle {
					next.Style = cur.Style
				}
				if next.IsEmpty() {
					delete(sh.Cells, coord)
				} else {
					sh.Cells[coord] = next
				}
			}
		}
		return nil

	case OpSetStyle:
		sh := wb.SheetByID(op.SheetID)
		if sh == nil || op.Range == nil || op.Style == nil {
			return fmt.Errorf("set_style 参数缺失")
		}
		for r := op.Range.R1; r <= op.Range.R2; r++ {
			for c := op.Range.C1; c <= op.Range.C2; c++ {
				coord := Coord{R: r, C: c}
				cur := sh.Cells[coord]
				next := &Cell{}
				if cur != nil {
					next.Formula = cur.Formula
					next.Value = cur.Value
				}
				next.Style = mergeStyle(cur, op.Style)
				sh.Cells[coord] = next
			}
		}
		return nil

	case OpAddSheet:
		wb.Sheets = append(wb.Sheets, &Sheet{
			SheetID: op.SheetID, Name: op.Name, Rows: 32, Cols: 8, Cells: map[Coord]*Cell{},
		})
		return nil

	case OpRenameSheet:
		sh := wb.SheetByID(op.SheetID)
		if sh == nil {
			return fmt.Errorf("sheet %s 不存在", op.SheetID)
		}
		sh.Name = op.Name
		return nil

	case OpDeleteSheet:
		for i, s := range wb.Sheets {
			if s.SheetID == op.SheetID {
				wb.Sheets = append(wb.Sheets[:i], wb.Sheets[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("sheet %s 不存在", op.SheetID)

	case OpInsertRows, OpDeleteRows, OpInsertColumns, OpDeleteColumns:
		return applyStructural(wb, op)

	case OpSetColWidth:
		sh := wb.SheetByID(op.SheetID)
		if sh == nil {
			return fmt.Errorf("sheet %s 不存在", op.SheetID)
		}
		if sh.ColWidths == nil {
			sh.ColWidths = map[int]int{}
		}
		sh.ColWidths[op.Col] = op.Width
		return nil

	case OpSetRowHeight:
		sh := wb.SheetByID(op.SheetID)
		if sh == nil {
			return fmt.Errorf("sheet %s 不存在", op.SheetID)
		}
		if sh.RowHeights == nil {
			sh.RowHeights = map[int]int{}
		}
		sh.RowHeights[op.Row] = op.Height
		return nil
	}
	return fmt.Errorf("未知操作 %q", op.Op)
}

func mergeStyle(base *Cell, patch *protocol.SheetCellStyle) *protocol.SheetCellStyle {
	var st protocol.SheetCellStyle
	if base != nil && base.Style != nil {
		st = *base.Style
	}
	if patch.Bold != nil {
		b := *patch.Bold
		st.Bold = &b
	}
	if patch.Italic != nil {
		i := *patch.Italic
		st.Italic = &i
	}
	return &st
}

// applyStructural 机械位移：格子数据、行高/列宽与尺寸联动。
func applyStructural(wb *Workbook, op protocol.SheetOp) error {
	sh := wb.SheetByID(op.SheetID)
	if sh == nil {
		return fmt.Errorf("sheet %s 不存在", op.SheetID)
	}
	axis := AxisRow
	if op.Op == OpInsertColumns || op.Op == OpDeleteColumns {
		axis = AxisCol
	}
	insert := op.Op == OpInsertRows || op.Op == OpInsertColumns
	shift := func(pos int) (int, bool) {
		if insert {
			if pos >= op.At {
				return pos + op.Count, true
			}
			return pos, true
		}
		if pos >= op.At && pos < op.At+op.Count {
			return 0, false
		}
		if pos >= op.At+op.Count {
			return pos - op.Count, true
		}
		return pos, true
	}
	newCells := make(map[Coord]*Cell, len(sh.Cells))
	for coord, cell := range sh.Cells {
		pos := coord.R
		if axis == AxisCol {
			pos = coord.C
		}
		np, alive := shift(pos)
		if !alive {
			continue
		}
		nc := coord
		if axis == AxisCol {
			nc.C = np
		} else {
			nc.R = np
		}
		newCells[nc] = cell
	}
	sh.Cells = newCells
	if axis == AxisRow {
		sh.RowHeights = shiftMap(sh.RowHeights, shift)
		if insert {
			sh.Rows += op.Count
		} else {
			sh.Rows = maxInt(1, sh.Rows-op.Count)
		}
	} else {
		sh.ColWidths = shiftMap(sh.ColWidths, shift)
		if insert {
			sh.Cols += op.Count
		} else {
			sh.Cols = maxInt(1, sh.Cols-op.Count)
		}
	}
	return nil
}

func shiftMap(m map[int]int, shift func(int) (int, bool)) map[int]int {
	if m == nil {
		return nil
	}
	out := make(map[int]int, len(m))
	for k, v := range m {
		nk, alive := shift(k)
		if alive {
			out[nk] = v
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
