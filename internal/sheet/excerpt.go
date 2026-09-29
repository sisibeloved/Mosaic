// 表格摘录：agent 语境注入与卡片摘要的有界投影（默认不把整个工作簿灌进
// 上下文——每表首个非空 ≤10 行 ×8 列，公式格显派生值）。
package sheet

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ExcerptSheetRows / ExcerptSheetCols 摘录视窗。
const (
	ExcerptSheetRows = 10
	ExcerptSheetCols = 8
)

// RenderSheetExcerpt 工作簿摘要（TSV 行）；maxRunes 为总预算（到量即止）。
// derived 可为 nil（无派生面时公式格显 "不可计算"）。
func RenderSheetExcerpt(wb *Workbook, derived map[string]map[Coord]Derived, maxRunes int) string {
	if wb == nil || maxRunes <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	writeLine := func(s string) bool {
		n := len([]rune(s))
		if used+n > maxRunes {
			return false
		}
		b.WriteString(s)
		used += n
		return true
	}
	for _, sh := range wb.Sheets {
		lines := sheetExcerptLines(sh, derivedMap(derived, sh.SheetID))
		if len(lines) == 0 {
			continue
		}
		if !writeLine(fmt.Sprintf("[%s] %d 行 ×%d 列，已填 %d 格\n", sh.Name, sh.Rows, sh.Cols, sh.FilledCells())) {
			break
		}
		for _, ln := range lines {
			if !writeLine(ln + "\n") {
				return b.String()
			}
		}
	}
	return b.String()
}

func derivedMap(m map[string]map[Coord]Derived, sheetID string) map[Coord]Derived {
	if m == nil {
		return nil
	}
	return m[sheetID]
}

// sheetExcerptLines 单表摘例行：首个非空 ≤10 行、前 ≤8 个非空列。
func sheetExcerptLines(sh *Sheet, derived map[Coord]Derived) []string {
	if sh.FilledCells() == 0 {
		return nil
	}
	rows := map[int]bool{}
	cols := map[int]bool{}
	for coord, cell := range sh.Cells {
		if cell == nil || (cell.Formula == "" && cell.Value == nil) {
			continue
		}
		rows[coord.R] = true
		cols[coord.C] = true
	}
	rowList := sortedKeysLimit(rows, ExcerptSheetRows)
	colList := sortedKeysLimit(cols, ExcerptSheetCols)
	if len(rowList) == 0 {
		return nil
	}
	lines := make([]string, 0, len(rowList))
	for _, r := range rowList {
		parts := make([]string, 0, len(colList))
		for _, c := range colList {
			parts = append(parts, CellDisplay(sh.CellAt(r, c), derived[Coord{R: r, C: c}]))
		}
		for len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		if len(parts) > 0 {
			lines = append(lines, strings.Join(parts, "\t"))
		}
	}
	return lines
}

func sortedKeysLimit(m map[int]bool, limit int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// CellDisplay 单格显示文本（公式格显派生值/错误码）。
func CellDisplay(cell *Cell, d Derived) string {
	if cell == nil {
		return ""
	}
	if cell.Formula != "" {
		switch d.Kind {
		case "value":
			return DisplayValue(d.Value)
		case "error":
			return d.Err
		default:
			return "不可计算"
		}
	}
	return DisplayValue(cell.Value)
}

// DisplayValue wire 值 → 显示文本。
func DisplayValue(v *protocol.SheetCellValue) string {
	if v == nil {
		return ""
	}
	switch v.Type {
	case "number":
		if v.Number != nil {
			return formatNum(*v.Number)
		}
	case "bool":
		if v.Bool != nil {
			if *v.Bool {
				return "TRUE"
			}
			return "FALSE"
		}
	case "text", "datetime":
		return v.Text
	}
	return ""
}
