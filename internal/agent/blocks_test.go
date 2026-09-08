// UT：turn_intent 防御性规范化——超长 public_rationale 截断保决策。
// 实证（2026-09-08 minimax IT）：仲裁指令语义被模型复述进 rationale 撑爆 280
// 上限，端口校验硬失败——决策本体仍有效，截断展示文本而非弃整个意图。
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
