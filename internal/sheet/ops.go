// 修订批校验与归一化（RFC-0017 §3.3）：任何 op 校验失败整批拒绝（工作簿级
// CAS + 整批原子）。归一化产物：add_sheet 回填分配 ID；结构操作（插删行列）
// 后自动追加 set_cells 携带改写后的公式原文——事件回放因此是纯机械应用，
// 不依赖解析器行为。校验在副本上试算，拒绝时原件无损。
package sheet

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// SheetOp 种类（与 protocol.SheetOp.Op 逐一对应）。
const (
	OpSetCells      = "set_cells"
	OpClearRange    = "clear_range"
	OpSetStyle      = "set_style"
	OpAddSheet      = "add_sheet"
	OpRenameSheet   = "rename_sheet"
	OpDeleteSheet   = "delete_sheet"
	OpInsertRows    = "insert_rows"
	OpDeleteRows    = "delete_rows"
	OpInsertColumns = "insert_columns"
	OpDeleteColumns = "delete_columns"
	OpSetColWidth   = "set_col_width"
	OpSetRowHeight  = "set_row_height"
)

// ValidateBatch 校验并归一化。state 为提交前工作簿（只读），newID 为 add_sheet
// 的 ID 分配器。归一化批 op 数受 MaxOpsPerBatch 约束（含自动追加的公式改写
// set_cells——超出即拒绝，提示拆批）。
func ValidateBatch(state *Workbook, ops []protocol.SheetOp, newID NewSheetID) ([]protocol.SheetOp, error) {
	if len(ops) == 0 {
		return nil, errors.New("表格操作批不能为空")
	}
	if len(ops) > MaxOpsPerBatch {
		return nil, fmt.Errorf("单批操作数超限（≤%d）", MaxOpsPerBatch)
	}
	wb := state.Clone()
	out := make([]protocol.SheetOp, 0, len(ops)+8)
	cellsInBatch := 0
	for i, op := range ops {
		normalized, extra, err := validateOp(wb, op, newID, &cellsInBatch)
		if err != nil {
			return nil, fmt.Errorf("op[%d] %s: %w", i, op.Op, err)
		}
		if err := ApplySheetOps(wb, append([]protocol.SheetOp{normalized}, extra...)); err != nil {
			return nil, fmt.Errorf("op[%d] %s: %w", i, op.Op, err)
		}
		out = append(out, normalized)
		out = append(out, extra...)
		if len(out) > MaxOpsPerBatch {
			return nil, fmt.Errorf("归一化后批操作数超限（≤%d，含公式改写）——请拆批", MaxOpsPerBatch)
		}
	}
	return out, nil
}

// validateOp 单 op 校验；extra 为结构操作自动追加的公式改写 set_cells。
func validateOp(wb *Workbook, op protocol.SheetOp, newID NewSheetID, cellsInBatch *int) (protocol.SheetOp, []protocol.SheetOp, error) {
	switch op.Op {
	case OpSetCells:
		if op.SheetID == "" {
			return op, nil, errors.New("缺少 sheet_id")
		}
		sh := wb.SheetByID(op.SheetID)
		if sh == nil {
			return op, nil, errors.New("目标工作表不存在")
		}
		if len(op.Cells) == 0 {
			return op, nil, errors.New("cells 不能为空")
		}
		if len(op.Cells) > MaxCellsPerBatch || *cellsInBatch+len(op.Cells) > MaxCellsPerBatch {
			return op, nil, fmt.Errorf("单批写入单元格数超限（≤%d）", MaxCellsPerBatch)
		}
		formulaDelta := 0
		seen := make(map[Coord]bool, len(op.Cells))
		for _, c := range op.Cells {
			if c.R < 1 || c.R > MaxRows || c.C < 1 || c.C > MaxCols {
				return op, nil, fmt.Errorf("坐标越界 r=%d c=%d（上限 %d 行 × %d 列）", c.R, c.C, MaxRows, MaxCols)
			}
			coord := Coord{R: c.R, C: c.C}
			if seen[coord] {
				return op, nil, fmt.Errorf("批内坐标重复 %s", A1Of(c.R, c.C))
			}
			seen[coord] = true
			if err := validateCellShape(c); err != nil {
				return op, nil, fmt.Errorf("%s: %w", A1Of(c.R, c.C), err)
			}
			if c.Type == "formula" {
				formulaDelta++
			} else if cur := sh.CellAt(c.R, c.C); cur != nil && cur.Formula != "" {
				formulaDelta-- // 公式格被字面覆盖
			}
		}
		if wb.FormulaCount()+formulaDelta > MaxFormulasPerBook {
			return op, nil, fmt.Errorf("工作簿公式格数超限（≤%d）", MaxFormulasPerBook)
		}
		*cellsInBatch += len(op.Cells)
		return op, nil, nil

	case OpClearRange, OpSetStyle:
		if op.SheetID == "" {
			return op, nil, errors.New("缺少 sheet_id")
		}
		if wb.SheetByID(op.SheetID) == nil {
			return op, nil, errors.New("目标工作表不存在")
		}
		if op.Range == nil {
			return op, nil, errors.New("缺少 range")
		}
		if err := validateRange(*op.Range); err != nil {
			return op, nil, err
		}
		if op.Op == OpSetStyle && op.Style == nil {
			return op, nil, errors.New("缺少 style")
		}
		return op, nil, nil

	case OpAddSheet:
		name := strings.TrimSpace(op.Name)
		if name == "" || utf8.RuneCountInString(name) > MaxSheetNameRunes {
			return op, nil, fmt.Errorf("工作表名须 1..%d 字符", MaxSheetNameRunes)
		}
		if len(wb.Sheets)+1 > MaxSheets {
			return op, nil, fmt.Errorf("工作表数超限（≤%d）", MaxSheets)
		}
		if wb.SheetByName(name) != nil {
			return op, nil, fmt.Errorf("工作表名 %q 已存在", name)
		}
		normalized := op
		normalized.Name = name
		if normalized.SheetID == "" {
			if newID == nil {
				return op, nil, errors.New("缺少工作表 ID 分配器")
			}
			normalized.SheetID = newID()
		}
		return normalized, nil, nil

	case OpRenameSheet:
		name := strings.TrimSpace(op.Name)
		if name == "" || utf8.RuneCountInString(name) > MaxSheetNameRunes {
			return op, nil, fmt.Errorf("工作表名须 1..%d 字符", MaxSheetNameRunes)
		}
		sh := wb.SheetByID(op.SheetID)
		if sh == nil {
			return op, nil, errors.New("目标工作表不存在")
		}
		if other := wb.SheetByName(name); other != nil && other.SheetID != sh.SheetID {
			return op, nil, fmt.Errorf("工作表名 %q 已存在", name)
		}
		normalized := op
		normalized.Name = name
		return normalized, nil, nil

	case OpDeleteSheet:
		if wb.SheetByID(op.SheetID) == nil {
			return op, nil, errors.New("目标工作表不存在")
		}
		if len(wb.Sheets) <= 1 {
			return op, nil, errors.New("至少保留一张工作表")
		}
		// 删表后既有公式中指向该表的引用求值报 #REF!（机械语义，不阻止删除）
		return op, nil, nil

	case OpInsertRows, OpInsertColumns, OpDeleteRows, OpDeleteColumns:
		return validateStructural(wb, op)

	case OpSetColWidth:
		if wb.SheetByID(op.SheetID) == nil {
			return op, nil, errors.New("目标工作表不存在")
		}
		if op.Col < 1 || op.Col > MaxCols {
			return op, nil, fmt.Errorf("列号越界（1..%d）", MaxCols)
		}
		if op.Width < MinColWidth || op.Width > MaxColWidth {
			return op, nil, fmt.Errorf("列宽须 %d..%d 像素", MinColWidth, MaxColWidth)
		}
		return op, nil, nil

	case OpSetRowHeight:
		if wb.SheetByID(op.SheetID) == nil {
			return op, nil, errors.New("目标工作表不存在")
		}
		if op.Row < 1 || op.Row > MaxRows {
			return op, nil, fmt.Errorf("行号越界（1..%d）", MaxRows)
		}
		if op.Height < MinRowHeight || op.Height > MaxRowHeight {
			return op, nil, fmt.Errorf("行高须 %d..%d 像素", MinRowHeight, MaxRowHeight)
		}
		return op, nil, nil
	}
	return op, nil, fmt.Errorf("未知操作 %q", op.Op)
}

// validateStructural 结构操作：维度上限、被推移数据的存活检查、既有公式可
// 解析性（无法安全改写的不支持公式阻止结构修改——RFC §3.3），并产出改写
// set_cells。
func validateStructural(wb *Workbook, op protocol.SheetOp) (protocol.SheetOp, []protocol.SheetOp, error) {
	sh := wb.SheetByID(op.SheetID)
	if sh == nil {
		return op, nil, errors.New("目标工作表不存在")
	}
	if op.Count < 1 {
		return op, nil, errors.New("count 须 ≥1")
	}
	axis := AxisRow
	dim := sh.Rows
	limit := MaxRows
	if op.Op == OpInsertColumns || op.Op == OpDeleteColumns {
		axis = AxisCol
		dim = sh.Cols
		limit = MaxCols
	}
	insert := op.Op == OpInsertRows || op.Op == OpInsertColumns
	if insert {
		if op.At < 1 || op.At > dim+1 {
			return op, nil, fmt.Errorf("插入位置须 1..%d", dim+1)
		}
		if dim+op.Count > limit {
			return op, nil, fmt.Errorf("插入后尺寸超限（%d+%d > %d）", dim, op.Count, limit)
		}
		// 被推移的已填格不得越界（数据无声丢失不可接受）
		for coord, cell := range sh.Cells {
			if cell.IsEmpty() {
				continue
			}
			pos := coord.R
			if axis == AxisCol {
				pos = coord.C
			}
			if pos >= op.At && pos+op.Count > limit {
				posName := A1Of(coord.R, coord.C)
				return op, nil, fmt.Errorf("插入会把已填格 %s 推出上限——先清理或拆批", posName)
			}
		}
	} else {
		if op.At < 1 || op.At+op.Count-1 > dim {
			return op, nil, fmt.Errorf("删除范围越界（%d..%d 超 1..%d）", op.At, op.At+op.Count-1, dim)
		}
	}
	// 全簿公式须可解析（跨表引用无从预判指向，保守全查）
	for _, s := range wb.Sheets {
		for coord, cell := range s.Cells {
			if cell == nil || cell.Formula == "" {
				continue
			}
			if _, err := ParseFormula(cell.Formula); err != nil {
				return op, nil, fmt.Errorf("存在不可改写的公式 %s!%s（%v）——先修正或转值后再做结构修改", s.Name, A1Of(coord.R, coord.C), err)
			}
		}
	}
	ch := StructuralChange{Axis: axis, At: op.At, Count: op.Count, Insert: insert, SheetID: op.SheetID}
	rewrites, err := wb.RewriteWorkbookFor(ch)
	if err != nil {
		return op, nil, err
	}
	extra := chunkFormulaRewrites(rewrites)
	return op, extra, nil
}

// chunkFormulaRewrites 改写清单 → set_cells 序列（每条 ≤MaxCellsPerBatch）。
func chunkFormulaRewrites(rewrites map[string]map[Coord]string) []protocol.SheetOp {
	var out []protocol.SheetOp
	for sheetID, cells := range rewrites {
		if len(cells) == 0 {
			continue
		}
		coords := make([]Coord, 0, len(cells))
		for c := range cells {
			coords = append(coords, c)
		}
		sortCoords(coords)
		for start := 0; start < len(coords); start += MaxCellsPerBatch {
			end := min(start+MaxCellsPerBatch, len(coords))
			batch := protocol.SheetOp{Op: OpSetCells, SheetID: sheetID, Cells: make([]protocol.SheetCell, 0, end-start)}
			for _, c := range coords[start:end] {
				batch.Cells = append(batch.Cells, protocol.SheetCell{R: c.R, C: c.C, Type: "formula", Formula: cells[c]})
			}
			out = append(out, batch)
		}
	}
	// 按 sheet 顺序稳定输出
	return out
}

func sortCoords(coords []Coord) {
	for i := 1; i < len(coords); i++ {
		for j := i; j > 0; j-- {
			a, b := coords[j-1], coords[j]
			if a.R > b.R || (a.R == b.R && a.C > b.C) {
				coords[j-1], coords[j] = b, a
				continue
			}
			break
		}
	}
}

// validateCellShape 单格内容形状（类型-字段严格配对；值类型封闭）。
func validateCellShape(c protocol.SheetCell) error {
	switch c.Type {
	case "text":
		if c.Number != nil || c.Bool != nil || c.Formula != "" {
			return errors.New("type=text 只能携带 text")
		}
		if len(c.Text) > MaxTextBytes {
			return fmt.Errorf("文本超限（≤%d 字节）", MaxTextBytes)
		}
	case "datetime":
		if _, err := time.Parse(time.RFC3339, c.Text); err != nil {
			return errors.New("datetime 须为 RFC3339 文本")
		}
	case "number":
		if c.Number == nil {
			return errors.New("type=number 缺少 number 值")
		}
	case "bool":
		if c.Bool == nil {
			return errors.New("type=bool 缺少 bool 值")
		}
	case "formula":
		if c.Formula == "" {
			return errors.New("type=formula 缺少公式原文")
		}
		if utf8.RuneCountInString(c.Formula) > MaxFormulaRunes {
			return fmt.Errorf("公式超限（≤%d runes）", MaxFormulaRunes)
		}
	case "null":
		// 清空：不得携带值字段
	default:
		return fmt.Errorf("未知单元格类型 %q", c.Type)
	}
	return nil
}

func validateRange(r protocol.SheetRange) error {
	if r.R1 < 1 || r.C1 < 1 || r.R2 < r.R1 || r.C2 < r.C1 || r.R2 > MaxRows || r.C2 > MaxCols {
		return fmt.Errorf("范围非法 r1=%d c1=%d r2=%d c2=%d（上限 %d 行 × %d 列）", r.R1, r.C1, r.R2, r.C2, MaxRows, MaxCols)
	}
	return nil
}
