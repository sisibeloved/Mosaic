// 表格领域模型（RFC-0017 §3.2）：工作簿挂 DocState（format=spreadsheet），
// 工作表稳定 ID，稀疏单元格；字面类型封闭（text/number/bool/datetime/null），
// 公式保存表达式原文、计算值为派生结果（Evaluate 重算，不落模型）。复用
// protocol 的 wire 类型（SheetCellValue/SheetCellStyle），避免双份词表。
package sheet

import (
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// FormulaRulesVersion 公式规则版本（同一工作簿版本同一规则集；v1 = RFC-0017
// M1 封闭函数集，ADR-0015 定稿）。
const FormulaRulesVersion = 1

// Cell 单元格（稀疏存储：map 中不存在 = 空格）。Value 与 Formula 二选一——
// Formula 非空即公式格（Value 恒 nil）；空格三种成分（值/公式/样式）可独立
// 存在：清值保留样式即只有 Style 的格。
type Cell struct {
	Formula string                   `json:"formula,omitempty"` // 表达式原文（不含引导 =）
	Value   *protocol.SheetCellValue `json:"value,omitempty"`
	Style   *protocol.SheetCellStyle `json:"style,omitempty"`
}

// IsEmpty 该格是否完全无内容（无值、无公式、无样式）。
func (c *Cell) IsEmpty() bool {
	return c == nil || (c.Formula == "" && c.Value == nil && c.Style == nil)
}

// Sheet 单张工作表。Rows/Cols 为显式尺寸（内容可稀疏分布于其内）；尺寸随
// 写入扩张（受 MaxRows/MaxCols 约束）、随删行列收缩，保存布局意图。
type Sheet struct {
	SheetID    string          `json:"sheet_id"`
	Name       string          `json:"name"`
	Rows       int             `json:"rows"`
	Cols       int             `json:"cols"`
	Cells      map[Coord]*Cell `json:"cells,omitempty"`
	ColWidths  map[int]int     `json:"col_widths,omitempty"`
	RowHeights map[int]int     `json:"row_heights,omitempty"`
}

// Workbook 工作簿（= 一篇 format=spreadsheet 文档的正文投影）。
type Workbook struct {
	FormulaRules int      `json:"formula_rules"`
	Sheets       []*Sheet `json:"sheets"`
}

// NewSheetID 由 doc 服务注入的 ID 生成器产出（cfg.NewID("sht")——sht_ 前缀
// 时间有序形态，与 evt_/blk_ 同源）。
type NewSheetID func() string

// NewWorkbook 新建工作簿：单表 "Sheet1"（尺寸 32×8 初始画布）。
func NewWorkbook(newID NewSheetID) *Workbook {
	if newID == nil {
		newID = func() string { return "sht_000000000000" } // 仅测试缺省
	}
	return &Workbook{
		FormulaRules: FormulaRulesVersion,
		Sheets: []*Sheet{{
			SheetID: newID(),
			Name:    "Sheet1",
			Rows:    32,
			Cols:    8,
			Cells:   map[Coord]*Cell{},
		}},
	}
}

// NewWorkbookForDoc 投影路径的确定性构造（回放可重建）：doc.created(format=
// spreadsheet) 时由 doc_id 派生默认表 ID——sht_ + doc_id 的 12 hex 后缀。
// 随后的 add_sheet ID 由提交路径经 cfg.NewID("sht") 分配并定稿进事件。
func NewWorkbookForDoc(docID string) *Workbook {
	suffix := strings.TrimPrefix(docID, "doc_")
	if suffix == docID || suffix == "" {
		suffix = "000000000000"
	}
	return NewWorkbook(func() string { return "sht_" + suffix })
}

// SheetByID 按 ID 取表。
func (w *Workbook) SheetByID(id string) *Sheet {
	for _, s := range w.Sheets {
		if s.SheetID == id {
			return s
		}
	}
	return nil
}

// SheetByName 按名取表（工作簿内名唯一）。
func (w *Workbook) SheetByName(name string) *Sheet {
	for _, s := range w.Sheets {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// FilledCells 已填格数（值或公式非空；样式不算内容）。
func (s *Sheet) FilledCells() int {
	n := 0
	for _, c := range s.Cells {
		if c != nil && (c.Formula != "" || c.Value != nil) {
			n++
		}
	}
	return n
}

// FormulaCount 全簿公式格数。
func (w *Workbook) FormulaCount() int {
	n := 0
	for _, s := range w.Sheets {
		for _, c := range s.Cells {
			if c != nil && c.Formula != "" {
				n++
			}
		}
	}
	return n
}

// Clone 深拷贝（修订校验在副本上试算，拒绝时原件无损）。
func (w *Workbook) Clone() *Workbook {
	if w == nil {
		return nil
	}
	out := &Workbook{FormulaRules: w.FormulaRules, Sheets: make([]*Sheet, 0, len(w.Sheets))}
	for _, s := range w.Sheets {
		ns := &Sheet{SheetID: s.SheetID, Name: s.Name, Rows: s.Rows, Cols: s.Cols,
			Cells: make(map[Coord]*Cell, len(s.Cells))}
		for k, c := range s.Cells {
			if c == nil {
				continue
			}
			nc := Cell{Formula: c.Formula}
			if c.Value != nil {
				v := *c.Value
				nc.Value = &v
			}
			if c.Style != nil {
				st := *c.Style
				nc.Style = &st
			}
			ns.Cells[k] = &nc
		}
		if s.ColWidths != nil {
			ns.ColWidths = make(map[int]int, len(s.ColWidths))
			for k, v := range s.ColWidths {
				ns.ColWidths[k] = v
			}
		}
		if s.RowHeights != nil {
			ns.RowHeights = make(map[int]int, len(s.RowHeights))
			for k, v := range s.RowHeights {
				ns.RowHeights[k] = v
			}
		}
		out.Sheets = append(out.Sheets, ns)
	}
	return out
}

// CellAt 读取格（nil = 空）。
func (s *Sheet) CellAt(r, c int) *Cell {
	return s.Cells[Coord{R: r, C: c}]
}

// growDim 写入越界扩张（上限内）。
func (s *Sheet) growDim(r, c int) {
	if r > s.Rows {
		s.Rows = min(r, MaxRows)
	}
	if c > s.Cols {
		s.Cols = min(c, MaxCols)
	}
}

// LiteralOf 字面格 → wire 值（公式格返回 nil——计算值走 Evaluate 派生面）。
func LiteralOf(c *Cell) *protocol.SheetCellValue {
	if c == nil || c.Formula != "" {
		return nil
	}
	return c.Value
}
