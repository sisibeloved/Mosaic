package sheet

import "testing"

// CanonicalFormula 外部交换比对专用：解析→渲染→单格区间塌缩（云端引擎
// 规范化对齐——腾讯实测 B1:B1 读回 B1）。
func TestCanonicalFormula(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=SUM(B1:B1)", "=SUM(B1)"},                  // 单格区间塌缩
		{"SUM(B1:B1)", "=SUM(B1)"},                   // 无引导 = 同样收敛
		{"=SUM(B1)", "=SUM(B1)"},                     // 已规范不动
		{"=SUM(A1:B2)", "=SUM(A1:B2)"},               // 真区间保留
		{"=数据B!A1*2", "=数据B!A1*2"},                // 跨表保留
		{"= SUM( B1:B1 , A1 )", "=SUM(B1,A1)"},       // 空白归一
		{"=UNKNOWNFN(1,2)", "=UNKNOWNFN(1,2)"},       // 未知函数按调用保留
		{"=1 +2", "=1+2"},                            // 空白
		{"=", "="},                                   // 空公式原样
		{"=1)", "=1)"},                               // 不可解析 → 原文（裁空白）
	}
	for _, c := range cases {
		if got := CanonicalFormula(c.in); got != c.want {
			t.Fatalf("CanonicalFormula(%q) = %q（期 %q）", c.in, got, c.want)
		}
	}
	// 等价判定的实用面：B1:B1 与 B1 规范后相等
	if CanonicalFormula("=SUM(B1:B1)") != CanonicalFormula("=SUM(B1)") {
		t.Fatal("单格区间与单引用应规范为同一形态")
	}
}
