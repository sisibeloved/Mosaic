// UT：结构化块端口校验与防御性规范化——turn_intent 超长 rationale 截断保决策
// （2026-09-08 minimax IT 实证）、doc_ops 写面形状门（RFC-0014 §2.7）。
package agent

import (
	"strings"
	"testing"
)

func TestCapIntentRationale(t *testing.T) {
	long := strings.Repeat("长", RationaleMaxRunes+50)
	data := map[string]any{
		"action": "speak", "type": "answer", "public_rationale": long,
		"scores": map[string]any{"relevance": 0.5, "novelty": 0.5, "urgency": 0.5, "confidence": 0.5},
	}
	CapIntentRationale(data)
	if got := len([]rune(data["public_rationale"].(string))); got != RationaleMaxRunes {
		t.Fatalf("截断后应为 %d 字，got %d", RationaleMaxRunes, got)
	}
	if err := ValidateBlock(BlockTurnIntent, data); err != nil {
		t.Fatalf("截断后应过端口校验：%v", err)
	}

	// 未超长/缺省不动写
	data2 := map[string]any{"action": "silent", "public_rationale": "短"}
	CapIntentRationale(data2)
	if data2["public_rationale"] != "短" {
		t.Fatal("未超长不得改写")
	}
	data3 := map[string]any{"action": "silent"}
	CapIntentRationale(data3)
	if _, ok := data3["public_rationale"]; ok {
		t.Fatal("缺省不得凭空补写")
	}
}

// TestValidateDocOps doc_ops 块校验（RFC-0014 §2.7 写面形状门）：op 枚举、
// 必填字段、ID 形态、块类型封闭集、批次上限；锚点存在性不在端口层（引擎
// 代写时按当前文档态逐 op 判定）。
func TestValidateDocOps(t *testing.T) {
	par := func(text string) map[string]any { return map[string]any{"type": "paragraph", "text": text} }
	valid := []struct {
		name string
		ops  []any
	}{
		{"create", []any{map[string]any{"op": "create", "title": "纪要", "blocks": []any{map[string]any{"type": "heading", "text": "结论"}, par("正文")}}}},
		{"append", []any{map[string]any{"op": "append", "doc_id": "doc_0123456789ab", "blocks": []any{par("x")}}}},
		{"replace_section", []any{map[string]any{"op": "replace_section", "doc_id": "doc_0123456789ab", "heading": "结论", "blocks": []any{par("新")}}}},
		{"insert_after", []any{map[string]any{"op": "insert_after", "doc_id": "doc_0123456789ab", "block_id": "blk_ab12cd34", "blocks": []any{par("y")}}}},
		{"delete_section", []any{map[string]any{"op": "delete_section", "doc_id": "doc_0123456789ab", "heading": "旧"}}},
		{"hr 块空 text 合法", []any{map[string]any{"op": "create", "title": "t", "blocks": []any{map[string]any{"type": "hr", "text": ""}}}}},
	}
	for _, tc := range valid {
		if err := ValidateBlock(BlockDocOps, map[string]any{"ops": tc.ops}); err != nil {
			t.Errorf("%s：应过校验，got %v", tc.name, err)
		}
	}

	overCap := make([]any, MaxDocOpsPerWave+1)
	for i := range overCap {
		overCap[i] = map[string]any{"op": "delete_section", "doc_id": "doc_0123456789ab", "heading": "h"}
	}
	invalid := []struct {
		name string
		data map[string]any
	}{
		{"ops 非数组", map[string]any{"ops": "x"}},
		{"ops 空批", map[string]any{"ops": []any{}}},
		{"超单波上限", map[string]any{"ops": overCap}},
		{"非法 op", map[string]any{"ops": []any{map[string]any{"op": "rewrite", "doc_id": "doc_0123456789ab"}}}},
		{"create 缺 title", map[string]any{"ops": []any{map[string]any{"op": "create", "blocks": []any{par("x")}}}}},
		{"create 缺 blocks", map[string]any{"ops": []any{map[string]any{"op": "create", "title": "t"}}}},
		{"doc_id 形态非法", map[string]any{"ops": []any{map[string]any{"op": "append", "doc_id": "docXYZ", "blocks": []any{par("x")}}}}},
		{"replace_section 缺 heading", map[string]any{"ops": []any{map[string]any{"op": "replace_section", "doc_id": "doc_0123456789ab", "blocks": []any{par("x")}}}}},
		{"insert_after 缺 block_id", map[string]any{"ops": []any{map[string]any{"op": "insert_after", "doc_id": "doc_0123456789ab", "blocks": []any{par("x")}}}}},
		{"delete_section 缺 heading", map[string]any{"ops": []any{map[string]any{"op": "delete_section", "doc_id": "doc_0123456789ab"}}}},
		{"非法块类型", map[string]any{"ops": []any{map[string]any{"op": "create", "title": "t", "blocks": []any{map[string]any{"type": "table", "text": "x"}}}}}},
		{"块 text 为空", map[string]any{"ops": []any{map[string]any{"op": "append", "doc_id": "doc_0123456789ab", "blocks": []any{map[string]any{"type": "paragraph", "text": "  "}}}}}},
	}
	for _, tc := range invalid {
		if err := ValidateBlock(BlockDocOps, tc.data); err == nil {
			t.Errorf("%s：应被拒", tc.name)
		}
	}

	// public_draft 侧车：携带 doc_ops 同标准校验；不携带不受影响
	draft := map[string]any{"body": "我写好了文档", "declared_relations": []any{},
		"doc_ops": []any{map[string]any{"op": "create", "title": "t", "blocks": []any{par("x")}}}}
	if err := ValidateBlock(BlockPublicDraft, draft); err != nil {
		t.Fatalf("public_draft 携带合法 doc_ops 应过校验：%v", err)
	}
	draft["doc_ops"] = []any{map[string]any{"op": "append", "doc_id": "bad"}}
	if err := ValidateBlock(BlockPublicDraft, draft); err == nil {
		t.Fatal("public_draft 携带非法 doc_ops 应被拒")
	}
	delete(draft, "doc_ops")
	if err := ValidateBlock(BlockPublicDraft, draft); err != nil {
		t.Fatalf("不带 doc_ops 的 public_draft 不受影响：%v", err)
	}
}
