// 结构改写（RFC-0017 §3.3）：插删行列后 AST 级平移受影响引用、指向被删
// 区域的引用改写为 #REF!，再渲染回规范公式文本。改写只认本引擎语法——本地
// 公式恒可安全改写；M2 导入的"不可改写不支持公式"在交换层另行处理。
package sheet

import (
	"fmt"
)

// Axis 结构维度。
type Axis int

const (
	AxisRow Axis = iota
	AxisCol
)

// StructuralChange 一次结构修改的描述（归一化时驱动全簿公式改写）。
type StructuralChange struct {
	Axis    Axis
	At      int // 1-based 起始行/列
	Count   int
	Insert  bool   // true=插入；false=删除
	SheetID string // 被改结构的表
}

// RewriteFormula 对单条公式执行结构改写。公式所在表（curSheetID）用于解析
// 空表名引用（跨表引用按名比对目标表）。返回是否发生改写。
func (wb *Workbook) RewriteFormula(formula, curSheetID string, ch StructuralChange) (string, bool, error) {
	ast, err := ParseFormula(formula)
	if err != nil {
		return formula, false, err // 语法坏公式不动原文
	}
	targetName := ""
	if s := wb.SheetByID(ch.SheetID); s != nil {
		targetName = s.Name
	} else {
		return formula, false, fmt.Errorf("结构改写目标表不存在")
	}
	curName := ""
	if s := wb.SheetByID(curSheetID); s != nil {
		curName = s.Name
	}
	changed := false
	newAST := rewriteNode(ast, curName, targetName, ch, &changed)
	if !changed {
		return formula, false, nil
	}
	return RenderFormula(newAST), true, nil
}

// rewriteNode 深遍历改写。命中目标表的引用按 ch 平移；越出维度上限或落入
// 被删区域的引用整点替换为 #REF!（区域任一端命中 → 整区域 #REF!）。
func rewriteNode(n fnode, curName, targetName string, ch StructuralChange, changed *bool) fnode {
	switch t := n.(type) {
	case *refNode:
		return rewriteRef(*t, curName, targetName, ch, changed)
	case *rangeNode:
		a, aHit := rewriteRefRaw(t.a, curName, targetName, ch)
		b, bHit := rewriteRefRaw(t.b, curName, targetName, ch)
		if !aHit && !bHit {
			return t
		}
		*changed = true
		if a.invalid || b.invalid {
			return &errLit{code: ErrRef}
		}
		return &rangeNode{a: a.ref, b: b.ref}
	case *unaryNode:
		return &unaryNode{op: t.op, x: rewriteNode(t.x, curName, targetName, ch, changed)}
	case *pctNode:
		return &pctNode{x: rewriteNode(t.x, curName, targetName, ch, changed)}
	case *binNode:
		return &binNode{op: t.op,
			l: rewriteNode(t.l, curName, targetName, ch, changed),
			r: rewriteNode(t.r, curName, targetName, ch, changed)}
	case *funcNode:
		args := make([]fnode, len(t.args))
		for i, a := range t.args {
			args[i] = rewriteNode(a, curName, targetName, ch, changed)
		}
		return &funcNode{name: t.name, args: args}
	}
	return n
}

func rewriteRef(r refNode, curName, targetName string, ch StructuralChange, changed *bool) fnode {
	out, hit := rewriteRefRaw(r, curName, targetName, ch)
	if !hit {
		return &r
	}
	*changed = true
	if out.invalid {
		return &errLit{code: ErrRef}
	}
	return &out.ref
}

type refRewrite struct {
	ref     refNode
	invalid bool // 指向被删区域 / 平移越界 → #REF!
}

// rewriteRefRaw 单点引用平移；hit=false 表示该引用不受本次结构修改影响。
func rewriteRefRaw(r refNode, curName, targetName string, ch StructuralChange) (refRewrite, bool) {
	name := r.sheet
	if name == "" {
		name = curName
	}
	if name != targetName {
		return refRewrite{ref: r}, false
	}
	out := r
	pos, limit := 0, 0
	if ch.Axis == AxisRow {
		pos, limit = out.r, MaxRows
	} else {
		pos, limit = out.c, MaxCols
	}
	if ch.Insert {
		if pos < ch.At {
			return refRewrite{ref: r}, false
		}
		pos += ch.Count
		if pos > limit {
			return refRewrite{invalid: true}, true
		}
	} else {
		if pos < ch.At {
			return refRewrite{ref: r}, false
		}
		if pos < ch.At+ch.Count {
			return refRewrite{invalid: true}, true // 落入被删区域
		}
		pos -= ch.Count
	}
	if ch.Axis == AxisRow {
		out.r = pos
	} else {
		out.c = pos
	}
	return refRewrite{ref: out}, true
}

// RewriteWorkbookFor 全簿公式改写：返回受影响格的（表 → 坐标 → 新公式文本）
// 清单。坐标已映射为**结构操作生效后**的位置（被删区域内的公式格自身随
// 删除消失，不出现在清单里）。坏公式（语法错误）原样保留——机械回放不碰
// 它，读侧标 unsupported。
func (wb *Workbook) RewriteWorkbookFor(ch StructuralChange) (map[string]map[Coord]string, error) {
	out := map[string]map[Coord]string{}
	for _, sh := range wb.Sheets {
		for coord, cell := range sh.Cells {
			if cell == nil || cell.Formula == "" {
				continue
			}
			newText, changed, err := wb.RewriteFormula(cell.Formula, sh.SheetID, ch)
			if err != nil || !changed {
				continue
			}
			at := coord
			if sh.SheetID == ch.SheetID {
				mapped, alive := mapCoordThrough(coord, ch)
				if !alive {
					continue // 公式格自身落在被删区域
				}
				at = mapped
			}
			if out[sh.SheetID] == nil {
				out[sh.SheetID] = map[Coord]string{}
			}
			out[sh.SheetID][at] = newText
		}
	}
	return out, nil
}

// mapCoordThrough 结构操作后该格的新位置；alive=false 表示落入被删区域。
func mapCoordThrough(coord Coord, ch StructuralChange) (Coord, bool) {
	pos := coord.R
	if ch.Axis == AxisCol {
		pos = coord.C
	}
	if ch.Insert {
		if pos >= ch.At {
			pos += ch.Count
		}
	} else {
		if pos >= ch.At && pos < ch.At+ch.Count {
			return coord, false
		}
		if pos >= ch.At+ch.Count {
			pos -= ch.Count
		}
	}
	if ch.Axis == AxisCol {
		coord.C = pos
	} else {
		coord.R = pos
	}
	return coord, true
}
