package agent

import (
	"encoding/json"
	"testing"
)

// TestNormalizeGenerateJSON 生成位宽松提取（2026-09-17 真机实证缺陷）：
// body 值内未转义英文双引号致整体解析失败、JSON 原文冒充发言发布。
func TestNormalizeGenerateJSON(t *testing.T) {
	// 实证形态：值内裸引号（"想摸鱼" 未转义）——修复后可解析且 body 提取正确。
	raw := `{"body":"你们那边天气是真不错还是"想摸鱼"的那种不错？😄","declared_relations":[]}`
	fixed := NormalizeGenerateJSON(raw)
	var obj map[string]any
	if err := json.Unmarshal([]byte(fixed), &obj); err != nil {
		t.Fatalf("裸引号修复后应可解析：%v（修复结果 %s）", err, fixed)
	}
	if body, _ := obj["body"].(string); body != `你们那边天气是真不错还是"想摸鱼"的那种不错？😄` {
		t.Fatalf("body 提取不符：%q", body)
	}
	// 合法 JSON 幂等无伤。
	legal := `{"body":"hello","declared_relations":[]}`
	if got := NormalizeGenerateJSON(legal); got != legal {
		t.Fatalf("合法 JSON 不应被改写：%s", got)
	}
	// 已转义序列保留。
	escaped := `{"body":"he said \"hi\" ok","declared_relations":[]}`
	if got := NormalizeGenerateJSON(escaped); got != escaped {
		t.Fatalf("已转义输入不应被改写：%s", got)
	}
	// 非 JSON 形状（散文）原样返回——回退路径不受影响。
	prose := "大家好，这只是一句普通发言"
	if got := NormalizeGenerateJSON(prose); got != prose {
		t.Fatalf("散文不应被改写：%s", got)
	}
	// 前后空白 + JSON 形状：Trim 后处理。
	if got := NormalizeGenerateJSON("  " + legal + "\n"); got != legal {
		t.Fatalf("JSON 形状应 Trim 后规范化：%s", got)
	}
}

func TestRepairLooseQuotes(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"字符串边界-尾}", `{"a":"x"}`, `{"a":"x"}`},
		{"字符串边界-逗号", `{"a":"x","b":1}`, `{"a":"x","b":1}`},
		{"内嵌引号转义", `{"a":"说"好"了"}`, `{"a":"说\"好\"了"}`},
		{"边界前瞻跳空白", `{"a":"x" , "b":1}`, `{"a":"x" , "b":1}`},
		{"冒号边界-键值", `{"a" :"x"}`, `{"a" :"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepairLooseQuotes(tc.in); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestLooksLikeJSON(t *testing.T) {
	if !LooksLikeJSON(`{"a":1}`) || !LooksLikeJSON("  {\"a\":1}  ") {
		t.Fatal("JSON 对象形状应判定为 true")
	}
	if LooksLikeJSON("hello world") || LooksLikeJSON(`{"a":1} extra`) || LooksLikeJSON("") {
		t.Fatal("非首{尾}形状应判定为 false")
	}
}
