package agent

import "fmt"

// 结构化块名（RFC-0002 §3.1 端口级规范；Result.Block 的合法取值）。
const (
	BlockTurnIntent          = "turn_intent"
	BlockAttentionAssessment = "attention_assessment"
	BlockPublicDraft         = "public_draft"
	BlockGroundedSummary     = "grounded_summary"
	BlockClosureIntent       = "closure_intent"
)

// ValidateBlock 端口级结构化块校验（RFC-0002 §3.5.1"结构化输出必须过 Schema 校验"的
// 端口执行面）：conformance 套件与适配器共用同一校验器——畸形输出必须在适配器边界
// 被拒，而不是被映射成合法零值结果混进域层（v1.6 审校 #8 教训：字符串分数曾被转成
// 合法零分进入选择引擎）。
//
// 校验口径与域层消费方（room 引擎 intentFromData / attention 硬资格）严格对齐：
// 已知字段强校验（枚举/数值/边界），未知附加键忽略（端口为投影前面，域层只取已知字段）。
func ValidateBlock(block string, data map[string]any) error {
	if data == nil {
		return fmt.Errorf("agent: 块 %q 的 data 为空", block)
	}
	switch block {
	case BlockTurnIntent:
		return validateTurnIntent(data)
	case BlockAttentionAssessment:
		return validateAttentionAssessment(data)
	case BlockPublicDraft:
		return validatePublicDraft(data)
	case BlockGroundedSummary:
		return validateGroundedSummary(data)
	case BlockClosureIntent:
		return validateClosureIntent(data)
	default:
		return fmt.Errorf("agent: 未知结构化块 %q", block)
	}
}

// turnIntentActions / turnIntentTypes 与 intent.recorded payload schema 枚举对齐。
var turnIntentActions = map[string]bool{
	"speak": true, "react": true, "fork": true, "summarize": true, "silent": true,
}

var turnIntentTypes = map[string]bool{
	"answer": true, "extend": true, "challenge": true, "support": true,
	"question": true, "redirect": true, "synthesize": true,
}

func validateTurnIntent(data map[string]any) error {
	action, _ := data["action"].(string)
	if !turnIntentActions[action] {
		return fmt.Errorf("agent: turn_intent.action 非法 %q", action)
	}
	if r, ok := data["public_rationale"]; ok {
		s, isStr := r.(string)
		if !isStr {
			return fmt.Errorf("agent: turn_intent.public_rationale 必须为字符串")
		}
		if len([]rune(s)) > 280 {
			return fmt.Errorf("agent: turn_intent.public_rationale 超 280 字符上限")
		}
	}
	if action == "silent" {
		return nil // RFC-0003 §3.1.2：silent 意图其余字段可省略
	}
	typ, _ := data["type"].(string)
	if !turnIntentTypes[typ] {
		return fmt.Errorf("agent: turn_intent.type 非法 %q", typ)
	}
	scores, ok := data["scores"].(map[string]any)
	if !ok {
		return fmt.Errorf("agent: turn_intent 非 silent 缺 scores")
	}
	for _, dim := range []string{"relevance", "novelty", "urgency", "confidence"} {
		v, ok := scores[dim].(float64) // JSON 数值；字符串数字/布尔一律拒收
		if !ok {
			return fmt.Errorf("agent: turn_intent.scores.%s 缺失或非数值", dim)
		}
		if v < 0 || v > 1 {
			return fmt.Errorf("agent: turn_intent.scores.%s 越界 [0,1]：%v", dim, v)
		}
	}
	return nil
}

func validateAttentionAssessment(data map[string]any) error {
	salience, ok := data["salience"].(float64)
	if !ok || salience < 0 || salience > 1 {
		return fmt.Errorf("agent: attention_assessment.salience 须为 [0,1] 数值，got %v", data["salience"])
	}
	switch d, _ := data["disposition"].(string); d {
	case "observe", "consider", "ignore":
	default:
		return fmt.Errorf("agent: attention_assessment.disposition 非法 %q", d)
	}
	if n, ok := data["note"]; ok {
		if _, isStr := n.(string); !isStr {
			return fmt.Errorf("agent: attention_assessment.note 必须为字符串")
		}
	}
	return nil
}

func validatePublicDraft(data map[string]any) error {
	body, _ := data["body"].(string)
	if body == "" {
		return fmt.Errorf("agent: public_draft.body 缺失或为空")
	}
	rels, ok := data["declared_relations"].([]any)
	if !ok {
		return fmt.Errorf("agent: public_draft.declared_relations 缺失或非数组")
	}
	for i, r := range rels {
		if _, isStr := r.(string); !isStr {
			return fmt.Errorf("agent: public_draft.declared_relations[%d] 非字符串", i)
		}
	}
	return nil
}

func validateGroundedSummary(data map[string]any) error {
	summary, _ := data["summary"].(string)
	if summary == "" {
		return fmt.Errorf("agent: grounded_summary.summary 缺失或为空")
	}
	ids, ok := data["cited_event_ids"].([]any)
	if !ok {
		return fmt.Errorf("agent: grounded_summary.cited_event_ids 缺失或非数组")
	}
	for i, id := range ids {
		if _, isStr := id.(string); !isStr {
			return fmt.Errorf("agent: grounded_summary.cited_event_ids[%d] 非字符串", i)
		}
	}
	return nil
}

func validateClosureIntent(data map[string]any) error {
	switch a, _ := data["action"].(string); a {
	case "conclude", "object", "abstain":
	default:
		return fmt.Errorf("agent: closure_intent.action 非法 %q", a)
	}
	if r, ok := data["rationale"]; ok {
		if _, isStr := r.(string); !isStr {
			return fmt.Errorf("agent: closure_intent.rationale 必须为字符串")
		}
	}
	return nil
}
