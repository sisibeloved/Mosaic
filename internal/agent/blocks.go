package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// 结构化块名（RFC-0002 §3.1 端口级规范；Result.Block 的合法取值）。
const (
	BlockTurnIntent          = "turn_intent"
	BlockAttentionAssessment = "attention_assessment"
	BlockPublicDraft         = "public_draft"
	BlockGroundedSummary     = "grounded_summary"
	BlockClosureIntent       = "closure_intent"
	// BlockMemoryOps 记忆评审产物（v1.70）：策展操作批次（含空批 = 无可沉淀）。
	BlockMemoryOps = "memory_ops"
	// BlockHistoryRequest 生成位历史查询（v1.70）：模型自报"需要更早语境"——
	// 引擎执行检索后携结果重发一次生成（两段式，agent 侧的 session_search 面）。
	BlockHistoryRequest = "history_request"
	// BlockDocOps 文档写面（RFC-0014 §2.7）：agent 唯一文档写口——随 public_draft
	// 同波携带（透明纪律：公开说明消息 + doc_ref 卡片），引擎逐 op 校验代写，
	// actor 记为该 bot（memory_ops 代写先例平移）。
	BlockDocOps = "doc_ops"
	// BlockDocQuery 文档检索两段式（§2.7 读面②，M2 预留——history_request 先例
	// 扩展）：首版仅锁定块名词汇，不接线校验、无适配器支持。
	BlockDocQuery = "doc_query"
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
	case BlockMemoryOps:
		return validateMemoryOps(data)
	case BlockHistoryRequest:
		return validateHistoryRequest(data)
	case BlockDocOps:
		return validateDocOps(data)
	default:
		return fmt.Errorf("agent: 未知结构化块 %q", block)
	}
}

// maxMemoryOpsPerReview 单次评审的操作上限（批次有界——评审是例行轻任务，
// 不是批量重写入口；Hermes 单 turn 评审同量级）。
const maxMemoryOpsPerReview = 5

func validateMemoryOps(data map[string]any) error {
	ops, ok := data["ops"].([]any)
	if !ok {
		return fmt.Errorf("agent: memory_ops.ops 必须为数组（空数组 = 无可沉淀）")
	}
	if len(ops) > maxMemoryOpsPerReview {
		return fmt.Errorf("agent: memory_ops.ops 超单批上限 %d", maxMemoryOpsPerReview)
	}
	for i, raw := range ops {
		op, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("agent: memory_ops.ops[%d] 非对象", i)
		}
		switch action, _ := op["action"].(string); action {
		case "add":
			if c, _ := op["content"].(string); strings.TrimSpace(c) == "" {
				return fmt.Errorf("agent: memory_ops.ops[%d] add 缺 content", i)
			}
		case "replace":
			if c, _ := op["content"].(string); strings.TrimSpace(c) == "" {
				return fmt.Errorf("agent: memory_ops.ops[%d] replace 缺 content", i)
			}
			if o, _ := op["old_text"].(string); strings.TrimSpace(o) == "" {
				return fmt.Errorf("agent: memory_ops.ops[%d] replace 缺 old_text（现有条目定位子串）", i)
			}
		case "remove":
			if o, _ := op["old_text"].(string); strings.TrimSpace(o) == "" {
				return fmt.Errorf("agent: memory_ops.ops[%d] remove 缺 old_text", i)
			}
		default:
			return fmt.Errorf("agent: memory_ops.ops[%d].action 非法 %q", i, action)
		}
	}
	if r, ok := data["public_rationale"]; ok {
		if _, isStr := r.(string); !isStr {
			return fmt.Errorf("agent: memory_ops.public_rationale 必须为字符串")
		}
	}
	return nil
}

func validateHistoryRequest(data map[string]any) error {
	q, _ := data["history_query"].(string)
	if strings.TrimSpace(q) == "" {
		return fmt.Errorf("agent: history_request.history_query 缺失或为空")
	}
	if len([]rune(q)) > 200 {
		return fmt.Errorf("agent: history_request.history_query 超 200 字上限")
	}
	return nil
}

// MaxDocOpsPerWave 单波 doc_ops 批次上限（RFC-0014 §2.7：写面是叙事性增量，
// 不是批量迁移入口；量级对齐 memory_ops 单批上限）。
const MaxDocOpsPerWave = 8

// maxDocOpBlocks 单 op 携带块数上限（与 doc.MaxOpsPerBatch 同值——端口层先行
// 拒形，域层上限仍以 doc 服务为准；agent 端口不反向依赖 doc 域包）。
const maxDocOpBlocks = 64

var docIDPattern = regexp.MustCompile(`^doc_[0-9a-f]{12}$`)

// docBlockTypes 块类型封闭集（与 doc 域 validBlockTypes 同源——RFC-0014 §2.2 首版）。
var docBlockTypes = map[string]bool{
	"heading": true, "paragraph": true, "list": true,
	"code": true, "quote": true, "hr": true,
}

// validateDocOps doc_ops 块校验（块载荷形 {"ops":[...]}，与 memory_ops 同构）。
// 端口只管形状：op 枚举/必填字段/ID 形态/批次上限；锚点存在性与正文上限由
// 引擎代写时按当前文档态判定（逐 op 降级，不翻波）。
func validateDocOps(data map[string]any) error {
	ops, ok := data["ops"].([]any)
	if !ok {
		return fmt.Errorf("agent: doc_ops.ops 必须为数组")
	}
	return checkDocOps(ops)
}

// checkDocOps op 列表形状校验（public_draft 侧车字段与独立块共用）。
func checkDocOps(ops []any) error {
	if len(ops) == 0 {
		return fmt.Errorf("agent: doc_ops 至少 1 条 op")
	}
	if len(ops) > MaxDocOpsPerWave {
		return fmt.Errorf("agent: doc_ops 超单波上限 %d", MaxDocOpsPerWave)
	}
	for i, raw := range ops {
		op, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("agent: doc_ops[%d] 非对象", i)
		}
		// 编辑类 op 必须指名目标文档
		switch kind, _ := op["op"].(string); kind {
		case "create":
			title, _ := op["title"].(string)
			if n := len([]rune(title)); n < 1 || n > 200 {
				return fmt.Errorf("agent: doc_ops[%d] create 缺 title 或超 200 字", i)
			}
			if err := checkDocOpBlocks(i, op); err != nil {
				return err
			}
		case "append", "replace_section", "insert_after", "delete_section":
			docID, _ := op["doc_id"].(string)
			if !docIDPattern.MatchString(docID) {
				return fmt.Errorf("agent: doc_ops[%d].doc_id 形如 doc_<12hex>", i)
			}
			switch kind {
			case "append":
				if err := checkDocOpBlocks(i, op); err != nil {
					return err
				}
			case "replace_section":
				if h, _ := op["heading"].(string); strings.TrimSpace(h) == "" {
					return fmt.Errorf("agent: doc_ops[%d] replace_section 缺 heading（小节标题文本锚）", i)
				}
				if err := checkDocOpBlocks(i, op); err != nil {
					return err
				}
			case "insert_after":
				if b, _ := op["block_id"].(string); strings.TrimSpace(b) == "" {
					return fmt.Errorf("agent: doc_ops[%d] insert_after 缺 block_id 锚点", i)
				}
				if err := checkDocOpBlocks(i, op); err != nil {
					return err
				}
			case "delete_section":
				if h, _ := op["heading"].(string); strings.TrimSpace(h) == "" {
					return fmt.Errorf("agent: doc_ops[%d] delete_section 缺 heading（小节标题文本锚）", i)
				}
			}
		default:
			return fmt.Errorf("agent: doc_ops[%d].op 非法 %q（create|append|replace_section|insert_after|delete_section）", i, kind)
		}
	}
	return nil
}

// checkDocOpBlocks op 的 blocks 字段：非空数组、≤64、逐块类型封闭、非 hr 块 text 非空。
func checkDocOpBlocks(i int, op map[string]any) error {
	blocks, ok := op["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		return fmt.Errorf("agent: doc_ops[%d].blocks 缺失或为空", i)
	}
	if len(blocks) > maxDocOpBlocks {
		return fmt.Errorf("agent: doc_ops[%d].blocks 超单 op 上限 %d", i, maxDocOpBlocks)
	}
	for j, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("agent: doc_ops[%d].blocks[%d] 非对象", i, j)
		}
		typ, _ := b["type"].(string)
		if !docBlockTypes[typ] {
			return fmt.Errorf("agent: doc_ops[%d].blocks[%d].type 非法 %q", i, j, typ)
		}
		text, isStr := b["text"].(string)
		if !isStr {
			return fmt.Errorf("agent: doc_ops[%d].blocks[%d].text 必须为字符串", i, j)
		}
		if typ != "hr" && strings.TrimSpace(text) == "" {
			return fmt.Errorf("agent: doc_ops[%d].blocks[%d].text 为空（hr 例外）", i, j)
		}
	}
	return nil
}

// turnIntentActions / turnIntentTypes 与 intent.recorded payload schema 枚举对齐。
var turnIntentActions = map[string]bool{
	"speak": true, "react": true, "fork": true, "summarize": true, "silent": true,
}

var turnIntentTypes = map[string]bool{
	"answer": true, "extend": true, "challenge": true, "support": true,
	"question": true, "redirect": true, "synthesize": true,
}

// RationaleMaxRunes turn_intent.public_rationale 上限（与 intent.recorded
// payload schema 同源）。
const RationaleMaxRunes = 280

// CapIntentRationale 防御性规范化：模型偶发超写 public_rationale（2026-09-08
// IT 实证：仲裁指令语义被复述进 rationale 撑爆上限）——决策本体
// （action/type/scores）仍有效，截断展示文本保留决策，不让整座意图评估因
// rationale 超长而失败。CLI 适配器在返回 turn_intent 块前调用。
func CapIntentRationale(data map[string]any) {
	if r, ok := data["public_rationale"].(string); ok {
		if runes := []rune(r); len(runes) > RationaleMaxRunes {
			data["public_rationale"] = string(runes[:RationaleMaxRunes])
		}
	}
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
		if len([]rune(s)) > RationaleMaxRunes {
			return fmt.Errorf("agent: turn_intent.public_rationale 超 %d 字符上限", RationaleMaxRunes)
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
	// RFC-0014 §2.7 侧车写面：携带 doc_ops 时同标准校验（透明纪律——
	// 写文档必须与公开说明消息同波；引擎代写前端口先钉形）。
	if raw, ok := data["doc_ops"]; ok {
		ops, isArr := raw.([]any)
		if !isArr {
			return fmt.Errorf("agent: public_draft.doc_ops 必须为数组")
		}
		if err := checkDocOps(ops); err != nil {
			return err
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
