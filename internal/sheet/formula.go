// 公式引擎（RFC-0017 M1，ADR-0015：自研封闭子集、服务端权威求值）。
// 语法：字面量（数/串/布尔/错误值）、单元格与区域引用（A1/$A$1/Sheet!A1/
// '中文名'!A1）、一元 ± 与后缀 %、^ * / + - & 与比较、函数封闭集
// SUM AVERAGE MIN MAX COUNT COUNTA IF ROUND。易变函数/宏/网络函数不支持。
// 语义要点（ADR-0015 记录）：空引用按数值 0；算术接纳布尔（TRUE=1）；文本
// 参与算术比较报 #VALUE!；比较仅同型（文本比较不区分大小写）；区域值只在
// 聚合函数里展开；循环引用 #CIRC!；求值节点预算耗尽 #BUDGET!。
package sheet

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ---- AST ----

type fnode interface {
	// prec 渲染优先级（越大越紧）；渲染时按需补最小括号。
	prec() int
}

type numLit struct{ v float64 }

func (numLit) prec() int { return 8 }

type strLit struct{ v string }

func (strLit) prec() int { return 8 }

type boolLit struct{ v bool }

func (boolLit) prec() int { return 8 }

type errLit struct{ code string }

func (errLit) prec() int { return 8 }

// refNode 单元格引用。sheet 为表名原文（空 = 公式所在表）；abs 为 $ 锚。
type refNode struct {
	sheet string
	r, c  int
	absR  bool
	absC  bool
}

func (refNode) prec() int { return 8 }

type rangeNode struct{ a, b refNode }

func (rangeNode) prec() int { return 8 }

type unaryNode struct {
	op string // + | -
	x  fnode
}

func (unaryNode) prec() int { return 6 }

type pctNode struct{ x fnode }

func (pctNode) prec() int { return 7 }

type binNode struct {
	op   string // + - * / ^ & = <> < > <= >=
	l, r fnode
}

func (*binNode) prec() int { return 0 } // 由 opPrec 决定

func opPrec(op string) int {
	switch op {
	case "+", "-":
		return 3
	case "*", "/":
		return 4
	case "^":
		return 5
	case "&":
		return 2
	default: // 比较族
		return 1
	}
}

func (b *binNode) renderPrec() int { return opPrec(b.op) }

type funcNode struct {
	name string
	args []fnode
}

func (funcNode) prec() int { return 8 }

// ---- 词法 ----

type ftokKind int

const (
	tkEOF ftokKind = iota
	tkNum
	tkStr
	tkBool
	tkIdent  // 函数名 / TRUE/FALSE / 表名（后随 !）
	tkRef    // $A$1 形态
	tkErr    // #REF! 等错误字面量
	tkOp     // + - * / ^ & % = <> < > <= >=
	tkBang   // !
	tkColon  // :
	tkLParen // (
	tkRParen // )
	tkComma  // ,
)

type ftok struct {
	kind ftokKind
	text string
	num  float64
	ref  refNode
}

type flexer struct {
	src string
	pos int
}

func isASCIILetter(ch byte) bool { return ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' }
func isDigit(ch byte) bool       { return ch >= '0' && ch <= '9' }

func (lx *flexer) next() (ftok, error) {
	for lx.pos < len(lx.src) && (lx.src[lx.pos] == ' ' || lx.src[lx.pos] == '\t') {
		lx.pos++
	}
	if lx.pos >= len(lx.src) {
		return ftok{kind: tkEOF}, nil
	}
	s := lx.src
	p := lx.pos
	ch := s[p]
	switch {
	case isDigit(ch):
		start := p
		for lx.pos < len(s) && (isDigit(s[lx.pos]) || s[lx.pos] == '.') {
			lx.pos++
		}
		if lx.pos < len(s) && (s[lx.pos] == 'e' || s[lx.pos] == 'E') { // 科学计数
			lx.pos++
			if lx.pos < len(s) && (s[lx.pos] == '+' || s[lx.pos] == '-') {
				lx.pos++
			}
			for lx.pos < len(s) && isDigit(s[lx.pos]) {
				lx.pos++
			}
		}
		v, err := strconv.ParseFloat(s[start:lx.pos], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return ftok{}, fmt.Errorf("非法数值 %q", s[start:lx.pos])
		}
		return ftok{kind: tkNum, num: v, text: s[start:lx.pos]}, nil
	case ch == '"':
		var b strings.Builder
		lx.pos++
		for lx.pos < len(s) {
			if s[lx.pos] == '"' {
				if lx.pos+1 < len(s) && s[lx.pos+1] == '"' {
					b.WriteByte('"')
					lx.pos += 2
					continue
				}
				lx.pos++
				return ftok{kind: tkStr, text: b.String()}, nil
			}
			b.WriteByte(s[lx.pos])
			lx.pos++
		}
		return ftok{}, fmt.Errorf("字符串未闭合")
	case ch == '#':
		// 错误字面量：吞到 ! 或 ? 终止符（#DIV/0! / #NAME?）
		start := p
		lx.pos++
		for lx.pos < len(s) && s[lx.pos] != '!' && s[lx.pos] != '?' {
			lx.pos++
		}
		if lx.pos < len(s) {
			lx.pos++ // 吞终止符
		}
		return ftok{kind: tkErr, text: strings.ToUpper(s[start:lx.pos])}, nil
	case ch == '$' || isASCIILetter(ch):
		// 连读 $ 字母 数字 run：后随 ! = 表名（! 留给语法器）；否则形如
		// ref / TRUE/FALSE / 函数名
		start := p
		for lx.pos < len(s) && (isASCIILetter(s[lx.pos]) || isDigit(s[lx.pos]) || s[lx.pos] == '$' || s[lx.pos] == '_') {
			lx.pos++
		}
		run := s[start:lx.pos]
		if lx.pos < len(s) && s[lx.pos] == '!' {
			return ftok{kind: tkIdent, text: run}, nil
		}
		if r, ok := parseRefText(run); ok {
			return ftok{kind: tkRef, ref: r, text: run}, nil
		}
		up := strings.ToUpper(run)
		if up == "TRUE" || up == "FALSE" {
			return ftok{kind: tkBool, text: up}, nil
		}
		return ftok{kind: tkIdent, text: run}, nil
	case ch == '\'':
		var b strings.Builder
		lx.pos++
		for lx.pos < len(s) {
			if s[lx.pos] == '\'' {
				if lx.pos+1 < len(s) && s[lx.pos+1] == '\'' {
					b.WriteByte('\'')
					lx.pos += 2
					continue
				}
				lx.pos++
				break
			}
			b.WriteByte(s[lx.pos])
			lx.pos++
		}
		if lx.pos > len(s) || lx.pos <= p+1 {
			return ftok{}, fmt.Errorf("表名引号未闭合")
		}
		return ftok{kind: tkIdent, text: b.String()}, nil
	case ch == '<':
		if lx.pos+1 < len(s) && (s[lx.pos+1] == '>' || s[lx.pos+1] == '=') {
			lx.pos += 2
			return ftok{kind: tkOp, text: s[p:lx.pos]}, nil
		}
		lx.pos++
		return ftok{kind: tkOp, text: "<"}, nil
	case ch == '>':
		if lx.pos+1 < len(s) && s[lx.pos+1] == '=' {
			lx.pos += 2
			return ftok{kind: tkOp, text: ">="}, nil
		}
		lx.pos++
		return ftok{kind: tkOp, text: ">"}, nil
	case strings.IndexByte("+-*/^&%=(),:!$", ch) >= 0 && ch != '$':
		lx.pos++
		switch ch {
		case '(':
			return ftok{kind: tkLParen, text: "("}, nil
		case ')':
			return ftok{kind: tkRParen, text: ")"}, nil
		case ',':
			return ftok{kind: tkComma, text: ","}, nil
		case '!':
			return ftok{kind: tkBang, text: "!"}, nil
		case ':':
			return ftok{kind: tkColon, text: ":"}, nil
		default:
			return ftok{kind: tkOp, text: string(ch)}, nil
		}
	default:
		return ftok{}, fmt.Errorf("非法字符 %q", string(ch))
	}
}

// parseRefText "$A$1" → refNode（列号超界或语法不符返回 false）。
func parseRefText(s string) (refNode, bool) {
	i := 0
	absC := false
	if i < len(s) && s[i] == '$' {
		absC = true
		i++
	}
	cs := i
	for i < len(s) && isASCIILetter(s[i]) {
		i++
	}
	if i == cs {
		return refNode{}, false
	}
	col := ColIndex(s[cs:i])
	absR := false
	if i < len(s) && s[i] == '$' {
		absR = true
		i++
	}
	rs := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == rs || i != len(s) || col == 0 {
		return refNode{}, false
	}
	row, _ := strconv.Atoi(s[rs:i])
	if row < 1 || row > MaxRows {
		return refNode{}, false
	}
	return refNode{r: row, c: col, absR: absR, absC: absC}, true
}

// ---- 语法（递归下降；优先级 低→高：比较 < & < 加减 < 乘除 < ^ < 一元 < %）----

type fparser struct {
	toks []ftok
	pos  int
}

// ParseFormula 表达式原文（不含引导 =）→ AST；语法错误返回 err（公式保留
// 原文标注不可计算，不拒绝整个提交）。
func ParseFormula(src string) (fnode, error) {
	lx := &flexer{src: src}
	var toks []ftok
	for {
		t, err := lx.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.kind == tkEOF {
			break
		}
		if len(toks) > 4*MaxFormulaRunes {
			return nil, fmt.Errorf("公式过长")
		}
	}
	p := &fparser{toks: toks}
	n, err := p.parseCompare()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tkEOF {
		return nil, fmt.Errorf("多余记号 %q", p.cur().text)
	}
	return n, nil
}

func (p *fparser) cur() ftok { return p.toks[p.pos] }

func (p *fparser) eat() ftok {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *fparser) parseCompare() (fnode, error) {
	l, err := p.parseConcat()
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		if t.kind == tkOp && isCmpOp(t.text) {
			p.eat()
			r, err := p.parseConcat()
			if err != nil {
				return nil, err
			}
			l = &binNode{op: t.text, l: l, r: r}
			continue
		}
		return l, nil
	}
}

func isCmpOp(s string) bool {
	switch s {
	case "=", "<>", "<", ">", "<=", ">=":
		return true
	}
	return false
}

func (p *fparser) parseConcat() (fnode, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkOp && p.cur().text == "&" {
		p.eat()
		r, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: "&", l: l, r: r}
	}
	return l, nil
}

func (p *fparser) parseAdd() (fnode, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkOp && (p.cur().text == "+" || p.cur().text == "-") {
		op := p.eat().text
		r, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *fparser) parseMul() (fnode, error) {
	l, err := p.parsePow()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkOp && (p.cur().text == "*" || p.cur().text == "/") {
		op := p.eat().text
		r, err := p.parsePow()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *fparser) parsePow() (fnode, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == tkOp && p.cur().text == "^" {
		p.eat()
		r, err := p.parsePow() // 右结合
		if err != nil {
			return nil, err
		}
		return &binNode{op: "^", l: l, r: r}, nil
	}
	return l, nil
}

func (p *fparser) parseUnary() (fnode, error) {
	if p.cur().kind == tkOp && (p.cur().text == "-" || p.cur().text == "+") {
		op := p.eat().text
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: op, x: x}, nil
	}
	return p.parsePostfix()
}

func (p *fparser) parsePostfix() (fnode, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkOp && p.cur().text == "%" {
		p.eat()
		x = &pctNode{x: x}
	}
	return x, nil
}

func (p *fparser) parsePrimary() (fnode, error) {
	t := p.cur()
	switch t.kind {
	case tkNum:
		p.eat()
		return &numLit{v: t.num}, nil
	case tkStr:
		p.eat()
		return &strLit{v: t.text}, nil
	case tkErr:
		p.eat()
		return &errLit{code: t.text}, nil
	case tkBool:
		p.eat()
		return &boolLit{v: t.text == "TRUE"}, nil
	case tkOp:
		if t.text == "-" || t.text == "+" {
			return p.parseUnary()
		}
	case tkLParen:
		p.eat()
		x, err := p.parseCompare()
		if err != nil {
			return nil, err
		}
		if p.cur().kind != tkRParen {
			return nil, fmt.Errorf("缺右括号")
		}
		p.eat()
		return x, nil
	case tkRef:
		p.eat()
		ref := t.ref
		if p.cur().kind == tkColon && p.toks[p.pos+1].kind == tkRef {
			p.eat()
			b := p.eat().ref
			// 规整端点为左上/右下
			if b.r < ref.r || b.c < ref.c {
				ref, b = b, ref
			}
			return &rangeNode{a: ref, b: b}, nil
		}
		return &ref, nil
	case tkIdent:
		// 表名!引用 / 函数调用
		name := t.text
		p.eat()
		if p.cur().kind == tkBang {
			p.eat()
			if p.cur().kind != tkRef {
				return nil, fmt.Errorf("表名 %q 后须为单元格引用", name)
			}
			r := p.eat().ref
			r.sheet = name
			if p.cur().kind == tkColon && p.toks[p.pos+1].kind == tkRef {
				p.eat()
				b := p.eat().ref
				b.sheet = name
				if b.r < r.r || b.c < r.c {
					r, b = b, r
				}
				return &rangeNode{a: r, b: b}, nil
			}
			return &r, nil
		}
		if p.cur().kind == tkLParen {
			p.eat()
			fn := &funcNode{name: strings.ToUpper(name)}
			if p.cur().kind == tkRParen {
				p.eat()
				return fn, nil
			}
			for {
				arg, err := p.parseCompare()
				if err != nil {
					return nil, err
				}
				fn.args = append(fn.args, arg)
				if p.cur().kind == tkComma {
					p.eat()
					continue
				}
				if p.cur().kind == tkRParen {
					p.eat()
					return fn, nil
				}
				return nil, fmt.Errorf("函数 %q 参数表不合法", fn.name)
			}
		}
		return nil, fmt.Errorf("未知名称 %q", name)
	}
	return nil, fmt.Errorf("意外的记号 %q", t.text)
}

// ---- 渲染（AST → 规范文本；parse(render(ast)) 稳定往返）----

// RenderFormula AST → 表达式原文（幂等：同 AST 恒同文本）。
func RenderFormula(n fnode) string {
	var b strings.Builder
	renderNode(&b, n, 0)
	return b.String()
}

func renderNode(b *strings.Builder, n fnode, parentPrec int) {
	switch t := n.(type) {
	case *numLit:
		b.WriteString(formatNum(t.v))
	case *strLit:
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(t.v, `"`, `""`))
		b.WriteByte('"')
	case *boolLit:
		if t.v {
			b.WriteString("TRUE")
		} else {
			b.WriteString("FALSE")
		}
	case *errLit:
		b.WriteString(t.code)
	case *refNode:
		writeRef(b, *t)
	case *rangeNode:
		writeRef(b, t.a)
		b.WriteByte(':')
		writeRefBare(b, t.b) // 区域两端同表：后端点不重复表名
	case *unaryNode:
		pr := 6
		if pr < parentPrec {
			b.WriteByte('(')
		}
		b.WriteString(t.op)
		renderNode(b, t.x, pr)
		if pr < parentPrec {
			b.WriteByte(')')
		}
	case *pctNode:
		pr := 7
		if pr < parentPrec {
			b.WriteByte('(')
		}
		renderNode(b, t.x, pr)
		b.WriteByte('%')
		if pr < parentPrec {
			b.WriteByte(')')
		}
	case *binNode:
		pr := opPrec(t.op)
		if pr < parentPrec {
			b.WriteByte('(')
		}
		renderNode(b, t.l, pr)
		b.WriteString(t.op)
		if t.op == "^" {
			renderNode(b, t.r, pr) // 右结合
		} else {
			renderNode(b, t.r, pr+1)
		}
		if pr < parentPrec {
			b.WriteByte(')')
		}
	case *funcNode:
		b.WriteString(t.name)
		b.WriteByte('(')
		for i, a := range t.args {
			if i > 0 {
				b.WriteByte(',')
			}
			renderNode(b, a, 0)
		}
		b.WriteByte(')')
	default:
		b.WriteString("#VALUE!")
	}
}

func writeRef(b *strings.Builder, r refNode) {
	if r.sheet != "" {
		if isBareSheetName(r.sheet) {
			b.WriteString(r.sheet)
		} else {
			b.WriteString("'" + strings.ReplaceAll(r.sheet, "'", "''") + "'")
		}
		b.WriteByte('!')
	}
	writeRefBare(b, r)
}

func writeRefBare(b *strings.Builder, r refNode) {
	if r.absC {
		b.WriteByte('$')
	}
	b.WriteString(ColName(r.c))
	if r.absR {
		b.WriteByte('$')
	}
	fmt.Fprintf(b, "%d", r.r)
}

func isBareSheetName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !isASCIILetter(ch) && !isDigit(ch) && ch != '_' {
			return false
		}
	}
	return true
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ---- 求值 ----

// 错误码（六值封闭）。
const (
	ErrRef    = "#REF!"
	ErrDiv0   = "#DIV/0!"
	ErrValue  = "#VALUE!"
	ErrName   = "#NAME?"
	ErrCirc   = "#CIRC!"
	ErrBudget = "#BUDGET!"
)

type fval struct {
	kind string // num | str | bool | err | range
	num  float64
	str  string
	b    bool
	err  string
	// range 视图（仅聚合函数消费）
	rgSheet string
	rgR1    int
	rgC1    int
	rgR2    int
	rgC2    int
}

func errVal(code string) fval { return fval{kind: "err", err: code} }

// Derived 公式格派生结果。
type Derived struct {
	Kind  string                   `json:"kind"`            // value | error | unsupported
	Value *protocol.SheetCellValue `json:"value,omitempty"` // kind=value 的计算值
	Err   string                   `json:"err,omitempty"`   // kind=error 的错误码；unsupported 携带原因
}

// CalcBudget 单次求值预算（var 供 UT 调低触发 #BUDGET!；生产恒 CalcBudgetNodes）。
var CalcBudget = CalcBudgetNodes

type evalPass struct {
	wb      *Workbook
	budget  int
	memo    map[string]fval // coordKey → 已定稿公式值
	stack   map[string]bool // 求值中坐标（循环检测）
	astMemo map[string]any  // 公式文本 → AST / error
}

func coordKey(sheetID string, c Coord) string {
	return sheetID + "|" + strconv.Itoa(c.R) + ":" + strconv.Itoa(c.C)
}

// Evaluate 全簿求值：每个公式格 → 派生结果。纯函数（同工作簿同结果），
// 语法错误 → unsupported、未知函数 → #NAME?、循环 → #CIRC!、预算耗尽 →
// #BUDGET!。
func Evaluate(wb *Workbook) map[string]map[Coord]Derived {
	out := map[string]map[Coord]Derived{}
	if wb == nil {
		return out
	}
	ep := &evalPass{
		wb:      wb,
		budget:  CalcBudget,
		memo:    map[string]fval{},
		stack:   map[string]bool{},
		astMemo: map[string]any{},
	}
	for _, sh := range wb.Sheets {
		for coord, cell := range sh.Cells {
			if cell == nil || cell.Formula == "" {
				continue
			}
			if out[sh.SheetID] == nil {
				out[sh.SheetID] = map[Coord]Derived{}
			}
			out[sh.SheetID][coord] = ep.derive(sh, coord, cell.Formula)
		}
	}
	return out
}

// derive 单格派生（memo 命中直接返回；栈递归检测循环）。
func (ep *evalPass) derive(sh *Sheet, coord Coord, formula string) Derived {
	key := coordKey(sh.SheetID, coord)
	if v, ok := ep.memo[key]; ok {
		return derivedOf(v)
	}
	if ep.stack[key] {
		return Derived{Kind: "error", Err: ErrCirc}
	}
	ast, perr := ep.parseOf(formula)
	if perr != nil {
		return Derived{Kind: "unsupported", Err: truncateRunes(perr.Error(), 120)}
	}
	ep.stack[key] = true
	v := ep.eval(ast, sh)
	delete(ep.stack, key)
	if v.kind == "err" && v.err == ErrCirc {
		// 环上每格都应报 #CIRC!，不落 memo 让其余环成员同样命中栈检测
		return Derived{Kind: "error", Err: ErrCirc}
	}
	ep.memo[key] = v
	return derivedOf(v)
}

func derivedOf(v fval) Derived {
	switch v.kind {
	case "num":
		n := v.num
		return Derived{Kind: "value", Value: &protocol.SheetCellValue{Type: "number", Number: &n}}
	case "str":
		return Derived{Kind: "value", Value: &protocol.SheetCellValue{Type: "text", Text: v.str}}
	case "bool":
		b := v.b
		return Derived{Kind: "value", Value: &protocol.SheetCellValue{Type: "bool", Bool: &b}}
	case "err":
		return Derived{Kind: "error", Err: v.err}
	default: // range 到达标量语境
		return Derived{Kind: "error", Err: ErrValue}
	}
}

func (ep *evalPass) parseOf(formula string) (fnode, error) {
	if cached, ok := ep.astMemo[formula]; ok {
		switch c := cached.(type) {
		case fnode:
			return c, nil
		case error:
			return nil, c
		}
	}
	ast, err := ParseFormula(formula)
	if err != nil {
		ep.astMemo[formula] = err
		return nil, err
	}
	ep.astMemo[formula] = ast
	return ast, nil
}

func (ep *evalPass) spend() bool {
	ep.budget--
	return ep.budget > 0
}

// eval 节点求值（AST 节点恒为指针形态）。
func (ep *evalPass) eval(n fnode, sh *Sheet) fval {
	if !ep.spend() {
		return errVal(ErrBudget)
	}
	switch t := n.(type) {
	case *numLit:
		return fval{kind: "num", num: t.v}
	case *strLit:
		return fval{kind: "str", str: t.v}
	case *boolLit:
		return fval{kind: "bool", b: t.v}
	case *errLit:
		return errVal(t.code)
	case *refNode:
		return ep.evalRef(*t, sh)
	case *rangeNode:
		return ep.rangeVal(t, sh)
	case *unaryNode:
		v := ep.eval(t.x, sh)
		if t.op == "+" {
			return v
		}
		x, ok := toNum(v)
		if !ok {
			return propagate(v)
		}
		return fval{kind: "num", num: -x}
	case *pctNode:
		v := ep.eval(t.x, sh)
		x, ok := toNum(v)
		if !ok {
			return propagate(v)
		}
		return fval{kind: "num", num: x / 100}
	case *binNode:
		return ep.evalBin(t, sh)
	case *funcNode:
		return ep.evalFunc(t, sh)
	}
	return errVal(ErrValue)
}

// propagate 错误优先透传；非数值给算术语境 → #VALUE!。
func propagate(v fval) fval {
	if v.kind == "err" {
		return v
	}
	return errVal(ErrValue)
}

// resolveSheet 表名 → 表（空名 = 所在表；未知名 → nil）。
func (ep *evalPass) resolveSheet(name string, cur *Sheet) *Sheet {
	if name == "" {
		return cur
	}
	return ep.wb.SheetByName(name)
}

func (ep *evalPass) evalRef(r refNode, cur *Sheet) fval {
	target := ep.resolveSheet(r.sheet, cur)
	if target == nil {
		return errVal(ErrRef)
	}
	cell := target.CellAt(r.r, r.c)
	if cell == nil {
		return fval{kind: "num", num: 0} // 空格按 0（ADR-0015）
	}
	if cell.Formula != "" {
		return ep.deriveVal(target, Coord{R: r.r, C: r.c}, cell.Formula)
	}
	return literalVal(cell.Value)
}

// deriveVal fval 版 derive（memo 复用）。
func (ep *evalPass) deriveVal(sh *Sheet, coord Coord, formula string) fval {
	key := coordKey(sh.SheetID, coord)
	if v, ok := ep.memo[key]; ok {
		return v
	}
	if ep.stack[key] {
		return errVal(ErrCirc)
	}
	ast, perr := ep.parseOf(formula)
	if perr != nil {
		return errVal(ErrValue) // 语法坏公式按 #VALUE! 参与运算（derive 面已标 unsupported）
	}
	ep.stack[key] = true
	v := ep.eval(ast, sh)
	delete(ep.stack, key)
	if v.kind != "err" || v.err != ErrCirc {
		ep.memo[key] = v
	}
	return v
}

func literalVal(v *protocol.SheetCellValue) fval {
	if v == nil {
		return fval{kind: "num", num: 0}
	}
	switch v.Type {
	case "number":
		if v.Number != nil {
			return fval{kind: "num", num: *v.Number}
		}
	case "text", "datetime":
		return fval{kind: "str", str: v.Text}
	case "bool":
		if v.Bool != nil {
			return fval{kind: "bool", b: *v.Bool}
		}
	}
	return fval{kind: "num", num: 0}
}

func (ep *evalPass) rangeVal(rn *rangeNode, cur *Sheet) fval {
	target := ep.resolveSheet(rn.a.sheet, cur)
	if target == nil {
		return errVal(ErrRef)
	}
	return fval{kind: "range", rgSheet: target.Name, rgR1: rn.a.r, rgC1: rn.a.c, rgR2: rn.b.r, rgC2: rn.b.c}
}

func (ep *evalPass) evalBin(t *binNode, sh *Sheet) fval {
	// IF 短路族不经此处；比较允许两侧惰性无需求值顺序差异，直接求两侧
	l := ep.eval(t.l, sh)
	r := ep.eval(t.r, sh)
	if l.kind == "err" {
		return l
	}
	if r.kind == "err" {
		return r
	}
	if t.op == "&" {
		return fval{kind: "str", str: toStr(l) + toStr(r)}
	}
	if isCmpOp(t.op) {
		return cmpVal(t.op, l, r)
	}
	a, ok1 := toNum(l)
	b, ok2 := toNum(r)
	if !ok1 {
		return errVal(ErrValue)
	}
	if !ok2 {
		return errVal(ErrValue)
	}
	switch t.op {
	case "+":
		return fval{kind: "num", num: a + b}
	case "-":
		return fval{kind: "num", num: a - b}
	case "*":
		return fval{kind: "num", num: a * b}
	case "/":
		if b == 0 {
			return errVal(ErrDiv0)
		}
		return fval{kind: "num", num: a / b}
	case "^":
		return fval{kind: "num", num: pow(a, b)}
	}
	return errVal(ErrValue)
}

func pow(a, b float64) float64 {
	v := math.Pow(a, b)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// toNum 算术强转：number 直取、bool→1/0；str/err 不转（调用方自行 propagate）。
func toNum(v fval) (float64, bool) {
	switch v.kind {
	case "num":
		return v.num, true
	case "bool":
		if v.b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func toStr(v fval) string {
	switch v.kind {
	case "num":
		return formatNum(v.num)
	case "str":
		return v.str
	case "bool":
		if v.b {
			return "TRUE"
		}
		return "FALSE"
	}
	return ""
}

// cmpVal 同型比较（文本不区分大小写）；异型 → #VALUE!。
func cmpVal(op string, l, r fval) fval {
	if l.kind == "str" && r.kind == "str" {
		a, b := strings.ToUpper(l.str), strings.ToUpper(r.str)
		return boolCmp(op, cmpInt(strings.Compare(a, b)))
	}
	if l.kind == "bool" && r.kind == "bool" {
		a, b := 0, 0
		if l.b {
			a = 1
		}
		if r.b {
			b = 1
		}
		return boolCmp(op, cmpInt(a-b))
	}
	a, ok1 := toNum(l)
	b, ok2 := toNum(r)
	if !ok1 || !ok2 {
		return errVal(ErrValue)
	}
	if a < b {
		return boolCmp(op, -1)
	}
	if a > b {
		return boolCmp(op, 1)
	}
	return boolCmp(op, 0)
}

func cmpInt(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

func boolCmp(op string, ord int) fval {
	var res bool
	switch op {
	case "=":
		res = ord == 0
	case "<>":
		res = ord != 0
	case "<":
		res = ord < 0
	case ">":
		res = ord > 0
	case "<=":
		res = ord <= 0
	case ">=":
		res = ord >= 0
	}
	return fval{kind: "bool", b: res}
}

// ---- 函数（封闭集；IF 惰性）----

var knownFuncs = map[string]int{ // 名 → 最大参数个数（-1 = 变长）
	"SUM": -1, "AVERAGE": -1, "MIN": -1, "MAX": -1,
	"COUNT": -1, "COUNTA": -1, "IF": 3, "ROUND": 2,
}

func (ep *evalPass) evalFunc(t *funcNode, sh *Sheet) fval {
	maxArgs, ok := knownFuncs[t.name]
	if !ok {
		return errVal(ErrName)
	}
	if maxArgs >= 0 && len(t.args) > maxArgs {
		return errVal(ErrValue)
	}
	switch t.name {
	case "IF":
		if len(t.args) < 2 {
			return errVal(ErrValue)
		}
		cond := ep.eval(t.args[0], sh)
		b, ok := toBool(cond)
		if !ok {
			return propagate(cond)
		}
		if b {
			return ep.eval(t.args[1], sh)
		}
		if len(t.args) >= 3 {
			return ep.eval(t.args[2], sh)
		}
		return fval{kind: "bool", b: false}
	case "ROUND":
		if len(t.args) != 2 {
			return errVal(ErrValue)
		}
		nv := ep.eval(t.args[0], sh)
		dv := ep.eval(t.args[1], sh)
		n, ok1 := toNum(nv)
		d, ok2 := toNum(dv)
		if !ok1 {
			return propagate(nv)
		}
		if !ok2 {
			return propagate(dv)
		}
		return fval{kind: "num", num: roundHalfAway(n, int(d))}
	}
	// 聚合族：先求参数值，再数值收集
	vals := make([]fval, 0, len(t.args))
	for _, a := range t.args {
		vals = append(vals, ep.eval(a, sh))
	}
	switch t.name {
	case "SUM", "AVERAGE", "MIN", "MAX":
		var nums []float64
		for _, v := range vals {
			if v.kind == "err" {
				return v
			}
			if err := ep.collectRangeNums(v, &nums); err != nil {
				return *err
			}
		}
		switch t.name {
		case "SUM":
			s := 0.0
			for _, x := range nums {
				s += x
			}
			return fval{kind: "num", num: s}
		case "AVERAGE":
			if len(nums) == 0 {
				return errVal(ErrDiv0)
			}
			s := 0.0
			for _, x := range nums {
				s += x
			}
			return fval{kind: "num", num: s / float64(len(nums))}
		case "MIN":
			if len(nums) == 0 {
				return fval{kind: "num", num: 0}
			}
			m := nums[0]
			for _, x := range nums {
				if x < m {
					m = x
				}
			}
			return fval{kind: "num", num: m}
		case "MAX":
			if len(nums) == 0 {
				return fval{kind: "num", num: 0}
			}
			m := nums[0]
			for _, x := range nums {
				if x > m {
					m = x
				}
			}
			return fval{kind: "num", num: m}
		}
	case "COUNT", "COUNTA":
		n := 0
		for _, v := range vals {
			if v.kind == "err" {
				return v
			}
			cnt, err := ep.countRange(v, t.name == "COUNTA")
			if err != nil {
				return *err
			}
			n += cnt
		}
		return fval{kind: "num", num: float64(n)}
	}
	return errVal(ErrValue)
}

func toBool(v fval) (bool, bool) {
	switch v.kind {
	case "bool":
		return v.b, true
	case "num":
		return v.num != 0, true
	}
	return false, false
}

func roundHalfAway(n float64, d int) float64 {
	if d < 0 {
		d = 0
	}
	shift := math.Pow(10, float64(d))
	x := n * shift
	if x < 0 {
		return math.Ceil(x-0.5) / shift
	}
	return math.Floor(x+0.5) / shift
}

// collectRangeNums 标量参与聚合：数值/布尔计入，文本拒收（#VALUE!——显式
// 文本参数是调用错误，与区域内的文本忽略语义相反）；区域仅取数值字面/数值
// 公式结果，文本与空格忽略（Excel 语义）。
func (ep *evalPass) collectRangeNums(v fval, nums *[]float64) *fval {
	if v.kind == "range" {
		sh := ep.wb.SheetByName(v.rgSheet)
		if sh == nil {
			e := errVal(ErrRef)
			return &e
		}
		for r := v.rgR1; r <= v.rgR2; r++ {
			for c := v.rgC1; c <= v.rgC2; c++ {
				if !ep.spend() {
					e := errVal(ErrBudget)
					return &e
				}
				cell := sh.CellAt(r, c)
				if cell == nil {
					continue
				}
				cv := literalVal(cell.Value)
				if cell.Formula != "" {
					cv = ep.deriveVal(sh, Coord{R: r, C: c}, cell.Formula)
				}
				if cv.kind == "num" {
					*nums = append(*nums, cv.num)
				}
			}
		}
		return nil
	}
	switch v.kind {
	case "num":
		*nums = append(*nums, v.num)
		return nil
	case "bool":
		if v.b {
			*nums = append(*nums, 1)
		} else {
			*nums = append(*nums, 0)
		}
		return nil
	}
	e := errVal(ErrValue)
	return &e
}

func (ep *evalPass) countRange(v fval, counta bool) (int, *fval) {
	if v.kind != "range" {
		if counta || v.kind == "num" || v.kind == "bool" {
			return 1, nil
		}
		return 0, nil
	}
	sh := ep.wb.SheetByName(v.rgSheet)
	if sh == nil {
		return 0, &[]fval{errVal(ErrRef)}[0]
	}
	n := 0
	for r := v.rgR1; r <= v.rgR2; r++ {
		for c := v.rgC1; c <= v.rgC2; c++ {
			if !ep.spend() {
				return 0, &[]fval{errVal(ErrBudget)}[0]
			}
			cell := sh.CellAt(r, c)
			if cell == nil {
				continue
			}
			if counta {
				n++
				continue
			}
			if cell.Formula != "" {
				dv := ep.deriveVal(sh, Coord{R: r, C: c}, cell.Formula)
				if dv.kind == "num" {
					n++
				}
				continue
			}
			if cell.Value != nil && cell.Value.Type == "number" {
				n++
			}
		}
	}
	return n, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// cellRangePattern 形如 A1:B2 的区间（含 $ 绝对形）；RE2 无反引用，端点
// 相同性在回调里比对。
var cellRangePattern = regexp.MustCompile(`\$?[A-Za-z]{1,3}\$?\d+:\$?[A-Za-z]{1,3}\$?\d+`)

// CanonicalFormula 公式文本规范化（外部交换比对专用——不影响事件归一化）：
// 解析 → 标准渲染 → 塌缩单格区间；不可解析返回裁过空白的原文。带不带引导
// = 均可，产物恒带 =。
func CanonicalFormula(src string) string {
	f := strings.TrimPrefix(strings.TrimSpace(src), "=")
	if f == "" {
		return strings.TrimSpace(src)
	}
	n, err := ParseFormula(f)
	if err != nil {
		return strings.TrimSpace(src)
	}
	rendered := RenderFormula(n)
	rendered = collapseSingleCellRanges(rendered)
	return "=" + rendered
}

// collapseSingleCellRanges 单格区间塌缩：B1:B1 → B1（云端引擎——腾讯实测
// ——会把单格区间规范化为单引用，文本比对须先对齐）。
func collapseSingleCellRanges(s string) string {
	return cellRangePattern.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.IndexByte(m, ':')
		if i > 0 && m[:i] == m[i+1:] {
			return m[i+1:]
		}
		return m
	})
}
