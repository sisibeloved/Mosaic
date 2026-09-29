// UT：sheet 域——坐标往返、公式解析/渲染/求值矩阵（算术/比较/拼接/区域/
// 跨表/函数封闭集/错误值/循环/预算）、结构改写（平移/$锚/跨表/#REF!）、
// 批校验与归一化（上限/坐标/重名/结构联动）、「逐步应用 == 事件回放」等价、
// 摘录有界。
package sheet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func newTestWorkbook() *Workbook {
	return NewWorkbook(func() string { return "sht_000000000001" })
}

func numCell(v float64) protocol.SheetCell {
	return protocol.SheetCell{R: 0, C: 0, Type: "number", Number: &v}
}

func setNum(t *testing.T, wb *Workbook, sheetID string, r, c int, v float64) {
	t.Helper()
	f := v
	mustApply(t, wb, protocol.SheetOp{Op: OpSetCells, SheetID: sheetID, Cells: []protocol.SheetCell{{R: r, C: c, Type: "number", Number: &f}}})
}

func setText(t *testing.T, wb *Workbook, sheetID string, r, c int, s string) {
	t.Helper()
	mustApply(t, wb, protocol.SheetOp{Op: OpSetCells, SheetID: sheetID, Cells: []protocol.SheetCell{{R: r, C: c, Type: "text", Text: s}}})
}

func setFormula(t *testing.T, wb *Workbook, sheetID string, r, c int, f string) {
	t.Helper()
	mustApply(t, wb, protocol.SheetOp{Op: OpSetCells, SheetID: sheetID, Cells: []protocol.SheetCell{{R: r, C: c, Type: "formula", Formula: f}}})
}

// mustApply 走完整校验+归一化+应用（等价真实提交路径）。
func mustApply(t *testing.T, wb *Workbook, ops ...protocol.SheetOp) {
	t.Helper()
	normalized, err := ValidateBatch(wb, ops, func() string { return "sht_000000000002" })
	if err != nil {
		t.Fatalf("ValidateBatch: %v", err)
	}
	if err := ApplySheetOps(wb, normalized); err != nil {
		t.Fatalf("ApplySheetOps: %v", err)
	}
}

func derivedOfCell(t *testing.T, wb *Workbook, sheetID string, r, c int) Derived {
	t.Helper()
	all := Evaluate(wb)
	for coord, d := range all[sheetID] {
		if coord.R == r && coord.C == c {
			return d
		}
	}
	t.Fatalf("无派生值 %s!%s", sheetID, A1Of(r, c))
	return Derived{}
}

func wantNum(t *testing.T, d Derived, want float64) {
	t.Helper()
	if d.Kind != "value" || d.Value == nil || d.Value.Type != "number" || d.Value.Number == nil || *d.Value.Number != want {
		t.Fatalf("期望数值 %v，实得 %+v", want, d)
	}
}

func wantErr(t *testing.T, d Derived, code string) {
	t.Helper()
	if d.Kind != "error" || d.Err != code {
		t.Fatalf("期望错误 %s，实得 %+v", code, d)
	}
}

// ---- 坐标 ----

func TestCoordsRoundTrip(t *testing.T) {
	for _, c := range []struct {
		col  int
		name string
	}{
		{1, "A"}, {26, "Z"}, {27, "AA"}, {52, "AZ"}, {200, "GR"},
	} {
		if got := ColName(c.col); got != c.name {
			t.Fatalf("ColName(%d) = %q（期望 %q）", c.col, got, c.name)
		}
		if got := ColIndex(c.name); got != c.col {
			t.Fatalf("ColIndex(%q) = %d（期望 %d）", c.name, got, c.col)
		}
	}
	if ColIndex("a") != 1 || ColIndex("GS") != 0 || ColIndex("") != 0 || ColIndex("1A") != 0 {
		t.Fatal("ColIndex 边界")
	}
	for _, s := range []string{"A1", "B7", "GR5000", "AA100"} {
		co, ok := ParseA1(s)
		if !ok || A1Of(co.R, co.C) != s {
			t.Fatalf("ParseA1(%q) 往返失败 = %+v", s, co)
		}
	}
	for _, s := range []string{"A0", "A5001", "GSA1", "1A", "A", "A1x", "GS1"} {
		if _, ok := ParseA1(s); ok {
			t.Fatalf("ParseA1(%q) 应失败", s)
		}
	}
	r1, c1, r2, c2, ok := ParseRange("C5:A1")
	if !ok || r1 != 1 || c1 != 1 || r2 != 5 || c2 != 3 {
		t.Fatalf("ParseRange 规整失败 %d %d %d %d", r1, c1, r2, c2)
	}
}

// ---- 公式解析/渲染往返 ----

func TestFormulaParseRenderRoundTrip(t *testing.T) {
	cases := []string{
		"1+2*3", "(1+2)*3", "-2^2", "2^-1", "1+2&\"x\"", "50%", "-50%",
		"SUM(A1:B3)", "SUM(A1:B3,C1,5)", "AVERAGE(Sheet1!A1:B2)",
		"IF(A1>=3,\"是\",\"否\")", "ROUND(1.234,2)", "COUNT(A1:B5)",
		"$A$1+A$1+$A1", "#REF!+0", "#DIV/0!", "TRUE", "FALSE=TRUE",
		"'数据 表'!A1+B1", "\"a\"\"b\"&\"c\"", "MIN(1,2,3)+MAX(A1:A2)*2",
		"A1<>B1", "1<=2", "-(1+2)", "SUM(A1:A3)/COUNTA(B1:B4)%",
	}
	for _, src := range cases {
		ast, err := ParseFormula(src)
		if err != nil {
			t.Fatalf("ParseFormula(%q): %v", src, err)
		}
		r1 := RenderFormula(ast)
		ast2, err := ParseFormula(r1)
		if err != nil {
			t.Fatalf("再解析(%q ← %q): %v", r1, src, err)
		}
		if r2 := RenderFormula(ast2); r1 != r2 {
			t.Fatalf("渲染不稳定：%q → %q → %q", src, r1, r2)
		}
	}
}

func TestFormulaParseErrors(t *testing.T) {
	// 未知函数（Foo(1)）不是语法错误——求值期报 #NAME?（见求值矩阵）。
	for _, src := range []string{"1+", "SUM(", "A1:", "B1!", "'未闭合", "\"abc", "1 2", "Sheet1!SUM", "@"} {
		if _, err := ParseFormula(src); err == nil {
			t.Fatalf("ParseFormula(%q) 应报错", src)
		}
	}
}

// ---- 求值矩阵 ----

func TestEvalArithmeticAndComparison(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	setNum(t, wb, sid, 1, 1, 10)     // A1
	setNum(t, wb, sid, 1, 2, 4)      // B1
	setText(t, wb, sid, 2, 1, "abc") // A2
	cases := []struct {
		f    string
		want float64
	}{
		{"A1+B1*2", 18}, {"(A1+B1)*2", 28}, {"-A1^2", 100}, {"A1/B1", 2.5},
		{"A1%", 0.1}, {"TRUE+1", 2}, {"B2+0", 0}, // 空格按 0
	}
	for i, c := range cases {
		setFormula(t, wb, sid, 10+i, 1, c.f)
		wantNum(t, derivedOfCell(t, wb, sid, 10+i, 1), c.want)
	}
	setFormula(t, wb, sid, 20, 1, "A2+1") // 文本参与算术
	wantErr(t, derivedOfCell(t, wb, sid, 20, 1), ErrValue)
	setFormula(t, wb, sid, 21, 1, "A1/0")
	wantErr(t, derivedOfCell(t, wb, sid, 21, 1), ErrDiv0)
	setFormula(t, wb, sid, 22, 1, "UNKNOWNFN(1)")
	wantErr(t, derivedOfCell(t, wb, sid, 22, 1), ErrName)
	setFormula(t, wb, sid, 23, 1, "SUM((")
	if d := derivedOfCell(t, wb, sid, 23, 1); d.Kind != "unsupported" {
		t.Fatalf("语法坏公式应 unsupported，实得 %+v", d)
	}
}

func TestEvalStringAndBool(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	setFormula(t, wb, sid, 1, 1, `"a"&"b"&1`)
	d := derivedOfCell(t, wb, sid, 1, 1)
	if d.Kind != "value" || d.Value.Type != "text" || d.Value.Text != "ab1" {
		t.Fatalf("拼接 = %+v", d)
	}
	setFormula(t, wb, sid, 2, 1, `"ABC"="abc"`)
	d = derivedOfCell(t, wb, sid, 2, 1)
	if d.Kind != "value" || d.Value.Type != "bool" || !*d.Value.Bool {
		t.Fatalf("大小写不敏感比较 = %+v", d)
	}
	setFormula(t, wb, sid, 3, 1, `"a"<"b"`)
	d = derivedOfCell(t, wb, sid, 3, 1)
	if d.Kind != "value" || !*d.Value.Bool {
		t.Fatalf("文本序 = %+v", d)
	}
	setFormula(t, wb, sid, 4, 1, `1="1"`)
	wantErr(t, derivedOfCell(t, wb, sid, 4, 1), ErrValue)
}

func TestEvalFunctionsAndRanges(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	// A1:A4 = 1,2,3,文本" x "
	setNum(t, wb, sid, 1, 1, 1)
	setNum(t, wb, sid, 2, 1, 2)
	setNum(t, wb, sid, 3, 1, 3)
	setText(t, wb, sid, 4, 1, "x")
	cases := []struct {
		f    string
		want float64
	}{
		{"SUM(A1:A4)", 6}, // 区域内文本忽略
		{"AVERAGE(A1:A3)", 2},
		{"MIN(A1:A3)", 1},
		{"MAX(A1:A3)", 3},
		{"COUNT(A1:A4)", 3},
		{"COUNTA(A1:A4)", 4},
		{"SUM(A1:A3,10)", 16},
		{"ROUND(2.5,0)", 3},
		{"ROUND(-2.5,0)", -3},
		{"ROUND(1.234,2)", 1.23},
		{"IF(A1=1,SUM(A1:A3),0)", 6},
		{"MIN(C1:C9)", 0}, // 空区域
	}
	for i, c := range cases {
		setFormula(t, wb, sid, 10+i, 1, c.f)
		wantNum(t, derivedOfCell(t, wb, sid, 10+i, 1), c.want)
	}
	// IF 惰性：假分支含除零不受影响
	setFormula(t, wb, sid, 30, 1, "IF(TRUE,1,1/0)")
	wantNum(t, derivedOfCell(t, wb, sid, 30, 1), 1)
	// 区域内文本作显式参数 → #VALUE!
	setFormula(t, wb, sid, 31, 1, "SUM(A4)")
	wantErr(t, derivedOfCell(t, wb, sid, 31, 1), ErrValue)
	// AVERAGE 空数值 → #DIV/0!
	setFormula(t, wb, sid, 32, 1, "AVERAGE(C1:C9)")
	wantErr(t, derivedOfCell(t, wb, sid, 32, 1), ErrDiv0)
}

func TestEvalCrossSheetAndChained(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	mustApply(t, wb, protocol.SheetOp{Op: OpAddSheet, Name: "数据表"})
	second := wb.Sheets[1].SheetID
	if wb.Sheets[1].Name != "数据表" {
		t.Fatal("add_sheet")
	}
	setNum(t, wb, second, 2, 2, 7) // B2
	setFormula(t, wb, sid, 1, 1, "'数据表'!B2*2")
	wantNum(t, derivedOfCell(t, wb, sid, 1, 1), 14)
	setFormula(t, wb, sid, 2, 1, "数据表!B2+1") // 裸名不可（中文名须引号）→ 解析失败
	if d := derivedOfCell(t, wb, sid, 2, 1); d.Kind != "unsupported" {
		t.Fatalf("中文裸表名应不可解析 = %+v", d)
	}
	// 链式公式
	setFormula(t, wb, sid, 3, 1, "A1+1")
	setFormula(t, wb, sid, 4, 1, "A3+1")
	wantNum(t, derivedOfCell(t, wb, sid, 4, 1), 16)
	// 跨表区域
	setNum(t, wb, second, 3, 2, 3)
	setFormula(t, wb, sid, 5, 1, "SUM('数据表'!B2:B3)")
	wantNum(t, derivedOfCell(t, wb, sid, 5, 1), 10)
}

func TestEvalCycleAndBudget(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	setFormula(t, wb, sid, 1, 1, "B1+1")
	setFormula(t, wb, sid, 1, 2, "A1+1")
	wantErr(t, derivedOfCell(t, wb, sid, 1, 1), ErrCirc)
	wantErr(t, derivedOfCell(t, wb, sid, 1, 2), ErrCirc)
	setFormula(t, wb, sid, 2, 1, "A2+1") // 自环
	wantErr(t, derivedOfCell(t, wb, sid, 2, 1), ErrCirc)

	orig := CalcBudget
	CalcBudget = 5
	defer func() { CalcBudget = orig }()
	wb2 := newTestWorkbook()
	s2 := wb2.Sheets[0].SheetID
	setFormula(t, wb2, s2, 1, 1, "SUM(A2:A200)+SUM(B2:B200)+1")
	wantErr(t, derivedOfCell(t, wb2, s2, 1, 1), ErrBudget)
}

// ---- 结构改写 ----

func TestRewriteInsertDeleteRows(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	for i, v := range []float64{1, 2, 3, 4, 5} {
		setNum(t, wb, sid, i+1, 1, v)
	}
	setFormula(t, wb, sid, 6, 1, "SUM(A1:A5)+$A$2+A3") // 15+2+3=20
	wantNum(t, derivedOfCell(t, wb, sid, 6, 1), 20)

	// 在第 2 行前插入 1 行：区域扩为 A1:A6，A3 引用平移为 A4，$A$2 → $A$3
	mustApply(t, wb, protocol.SheetOp{Op: OpInsertRows, SheetID: sid, At: 2, Count: 1})
	if got := wb.Sheets[0].CellAt(7, 1).Formula; got != "SUM(A1:A6)+$A$3+A4" {
		t.Fatalf("插入后公式 = %q", got)
	}
	// 原数据平移：A2 为新空行，原 2 位于 A3
	if v := wb.Sheets[0].CellAt(3, 1); v == nil || v.Value == nil || *v.Value.Number != 2 {
		t.Fatal("插入行数据平移")
	}

	// 删除第 3 行（原数据 2 所在行）：$A$3 命中被删区 → #REF!；A4 → A3；
	// 公式格自身 7→6、8→7、9→8
	setFormula(t, wb, sid, 8, 1, "A4*10")
	setFormula(t, wb, sid, 9, 1, "A3*10")
	mustApply(t, wb, protocol.SheetOp{Op: OpDeleteRows, SheetID: sid, At: 3, Count: 1})
	if got := wb.Sheets[0].CellAt(6, 1).Formula; got != "SUM(A1:A5)+#REF!+A3" {
		t.Fatalf("删除后公式 = %q", got)
	}
	if got := wb.Sheets[0].CellAt(7, 1).Formula; got != "A3*10" {
		t.Fatalf("平移引用 = %q", got)
	}
	if got := wb.Sheets[0].CellAt(8, 1); got == nil || got.Formula != "#REF!*10" {
		t.Fatalf("命中被删区引用应改写 #REF! = %+v", got)
	}
	wantErr(t, derivedOfCell(t, wb, sid, 8, 1), ErrRef)
	wantErr(t, derivedOfCell(t, wb, sid, 6, 1), ErrRef)
}

func TestRewriteColumnsAndCrossSheet(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	mustApply(t, wb, protocol.SheetOp{Op: OpAddSheet, Name: "S2"})
	s2 := wb.Sheets[1].SheetID
	setFormula(t, wb, sid, 1, 1, "SUM(B1:B3)*S2!B2")
	// 目标表 S2 插列：跨表引用 B2→C2，区域 B1:B3→C1:C3
	mustApply(t, wb, protocol.SheetOp{Op: OpInsertColumns, SheetID: s2, At: 2, Count: 1})
	if got := wb.Sheets[0].CellAt(1, 1).Formula; got != "SUM(B1:B3)*S2!C2" {
		t.Fatalf("跨表插列改写 = %q", got)
	}
	// 本表第 2 列前插 2 列：B1:B3 两端同列平移 B→D；跨表引用 S2!C2 不动
	mustApply(t, wb, protocol.SheetOp{Op: OpInsertColumns, SheetID: sid, At: 2, Count: 2})
	if got := wb.Sheets[0].CellAt(1, 1).Formula; got != "SUM(D1:D3)*S2!C2" {
		t.Fatalf("本表插列改写 = %q", got)
	}
}

// ---- 批校验 ----

func TestValidateBatchRejects(t *testing.T) {
	sid0 := func() string { return "sht_new00000001" }
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID

	if _, err := ValidateBatch(wb, nil, sid0); err == nil {
		t.Fatal("空批应拒绝")
	}
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpSetCells, SheetID: "sht_missing", Cells: []protocol.SheetCell{numCell(1)}}}, sid0); err == nil {
		t.Fatal("未知表应拒绝")
	}
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpSetCells, SheetID: sid, Cells: []protocol.SheetCell{{R: 0, C: 1, Type: "number"}}}}, sid0); err == nil {
		t.Fatal("越界坐标应拒绝")
	}
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpSetCells, SheetID: sid, Cells: []protocol.SheetCell{
		{R: 1, C: 1, Type: "number", Number: ptrFloat(1)}, {R: 1, C: 1, Type: "number", Number: ptrFloat(2)},
	}}}, sid0); err == nil {
		t.Fatal("批内重复坐标应拒绝")
	}
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpAddSheet, Name: "Sheet1"}}, sid0); err == nil {
		t.Fatal("重名表应拒绝")
	}
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpDeleteSheet, SheetID: sid}}, sid0); err == nil {
		t.Fatal("删最后一张表应拒绝")
	}
	// 未解析公式阻止结构修改
	mustApply(t, wb, protocol.SheetOp{Op: OpSetCells, SheetID: sid, Cells: []protocol.SheetCell{{R: 1, C: 1, Type: "formula", Formula: "SUM(("}}})
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpInsertRows, SheetID: sid, At: 1, Count: 1}}, sid0); err == nil {
		t.Fatal("坏公式应阻止结构修改")
	}
	// datetime 形状
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpSetCells, SheetID: sid, Cells: []protocol.SheetCell{{R: 2, C: 2, Type: "datetime", Text: "not-a-date"}}}}, sid0); err == nil {
		t.Fatal("非法 datetime 应拒绝")
	}
	// 插入推出上限（5000 行表尾插 1 行 + 已填格）
	mustApply(t, wb, protocol.SheetOp{Op: OpSetCells, SheetID: sid, Cells: []protocol.SheetCell{{R: 4999, C: 1, Type: "number", Number: ptrFloat(9)}}})
	if _, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpInsertRows, SheetID: sid, At: 4998, Count: 5}}, sid0); err == nil {
		t.Fatal("推格出上限应拒绝")
	}
}

func ptrFloat(v float64) *float64 { return &v }

func ptrBool(b bool) *bool { return &b }

func TestValidateBatchNormalizeAddSheetID(t *testing.T) {
	wb := newTestWorkbook()
	out, err := ValidateBatch(wb, []protocol.SheetOp{{Op: OpAddSheet, Name: "月度"}}, func() string { return "sht_aabbccddeeff" })
	if err != nil {
		t.Fatal(err)
	}
	if out[0].SheetID != "sht_aabbccddeeff" || out[0].Name != "月度" {
		t.Fatalf("add_sheet 归一化 = %+v", out[0])
	}
}

// ---- 回放等价：逐步应用 == 事件回放 ----

func TestReplayEquivalence(t *testing.T) {
	// 固定序列：每步走完整校验+归一化+应用，同时录制归一化产物
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	newID := func() string { return "sht_000000000002" }
	batches := [][]protocol.SheetOp{
		{{Op: OpSetCells, SheetID: sid, Cells: []protocol.SheetCell{
			{R: 1, C: 1, Type: "number", Number: ptrFloat(1)},
			{R: 2, C: 1, Type: "number", Number: ptrFloat(2)},
			{R: 3, C: 1, Type: "text", Text: "备注"},
			{R: 1, C: 2, Type: "bool", Bool: ptrBool(true)},
		}}},
		{{Op: OpAddSheet, Name: "二号"}},
		{{Op: OpSetCells, SheetID: "sht_000000000002", Cells: []protocol.SheetCell{{R: 1, C: 1, Type: "number", Number: ptrFloat(10)}}}},
		{{Op: OpSetCells, SheetID: sid, Cells: []protocol.SheetCell{{R: 4, C: 1, Type: "formula", Formula: "SUM(A1:A3)+'二号'!A1"}}},
			{Op: OpInsertRows, SheetID: sid, At: 2, Count: 2}},
		{{Op: OpDeleteRows, SheetID: sid, At: 4, Count: 1}},
		{{Op: OpSetStyle, SheetID: sid, Range: &protocol.SheetRange{R1: 1, C1: 1, R2: 3, C2: 3}, Style: &protocol.SheetCellStyle{Bold: ptrBool(true)}}},
		{{Op: OpSetColWidth, SheetID: sid, Col: 2, Width: 120}},
		{{Op: OpRenameSheet, SheetID: "sht_000000000002", Name: "附表"}},
		{{Op: OpClearRange, SheetID: sid, Range: &protocol.SheetRange{R1: 1, C1: 3, R2: 5, C2: 3}}},
	}
	var tape [][]protocol.SheetOp
	for _, batch := range batches {
		normalized, err := ValidateBatch(wb, batch, newID)
		if err != nil {
			t.Fatalf("批校验: %v", err)
		}
		tape = append(tape, normalized)
		if err := ApplySheetOps(wb, normalized); err != nil {
			t.Fatal(err)
		}
	}
	// 纯回放：全新工作簿重放归一化 tape（无校验、无解析器——事件回放路径）
	fresh := newTestWorkbook()
	for _, batch := range tape {
		if err := ApplySheetOps(fresh, batch); err != nil {
			t.Fatalf("回放: %v", err)
		}
	}
	if !reflect.DeepEqual(wb.Sheets, fresh.Sheets) {
		t.Fatal("逐步应用与归一化回放不一致")
	}
	if !reflect.DeepEqual(Evaluate(wb), Evaluate(fresh)) {
		t.Fatal("派生面不一致")
	}
}

// ---- 摘录 ----

func TestRenderSheetExcerpt(t *testing.T) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	setNum(t, wb, sid, 1, 1, 1)
	setNum(t, wb, sid, 1, 2, 2)
	setFormula(t, wb, sid, 2, 1, "A1+B1")
	out := RenderSheetExcerpt(wb, Evaluate(wb), 8000)
	if !strings.Contains(out, "[Sheet1]") || !strings.Contains(out, "1\t2") || !strings.Contains(out, "3") {
		t.Fatalf("摘录 = %q", out)
	}
	// 预算截断
	if len([]rune(RenderSheetExcerpt(wb, nil, 5))) > 5 {
		t.Fatal("预算截断失效")
	}
	// 空簿
	if RenderSheetExcerpt(newTestWorkbook(), nil, 100) != "" {
		t.Fatal("空簿摘录应为空")
	}
}

// ---- 基准（RFC-0017 §3.4：万格折叠/千公式重算/五百修订回放） ----

func BenchmarkEval10kCells1kFormulas(b *testing.B) {
	wb := newTestWorkbook()
	sid := wb.Sheets[0].SheetID
	var cells []protocol.SheetCell
	for i := 1; i <= 10000; i++ {
		v := float64(i % 97)
		cells = append(cells, protocol.SheetCell{R: (i-1)/100 + 1, C: (i-1)%100 + 1, Type: "number", Number: &v})
	}
	normalized, _ := ValidateBatch(wb, []protocol.SheetOp{{Op: OpSetCells, SheetID: sid, Cells: cells}}, func() string { return "sht_bench000001" })
	_ = ApplySheetOps(wb, normalized)
	// 1000 条公式（第 101-110 行，每行引用同列前 100 格 +1）
	var fcells []protocol.SheetCell
	for r := 101; r <= 110; r++ {
		for c := 1; c <= 100; c++ {
			fcells = append(fcells, protocol.SheetCell{R: r, C: c, Type: "formula", Formula: ColName(c) + "1+1"})
		}
	}
	normalized, _ = ValidateBatch(wb, []protocol.SheetOp{{Op: OpSetCells, SheetID: sid, Cells: fcells}}, func() string { return "sht_bench000001" })
	_ = ApplySheetOps(wb, normalized)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Evaluate(wb)
	}
}

func BenchmarkReplay500Revisions(b *testing.B) {
	build := func() *Workbook {
		wb := newTestWorkbook()
		sid := wb.Sheets[0].SheetID
		for rev := 0; rev < 500; rev++ {
			var cells []protocol.SheetCell
			v := float64(rev)
			for k := 0; k < 20; k++ {
				cells = append(cells, protocol.SheetCell{R: (rev%50)*20 + k + 1, C: 1, Type: "number", Number: &v})
			}
			normalized, _ := ValidateBatch(wb, []protocol.SheetOp{{Op: OpSetCells, SheetID: sid, Cells: cells}}, func() string { return "sht_bench000001" })
			_ = ApplySheetOps(wb, normalized)
		}
		return wb
	}
	build() // 预热验证
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = build()
	}
}
