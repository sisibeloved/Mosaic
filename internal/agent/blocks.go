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
	// BlockSheetOps 表格写面（RFC-0017 §4.3）：agent 的表格资产写口——随
	// public_draft 同波携带（doc_ops 同纪律），编辑类 op 必须携带 base_version
	// 与显式坐标；引擎逐 op 代写落 doc.sheet_revision_committed。
	BlockSheetOps = "sheet_ops"
	// BlockSheetQuery 表格读面（RFC-0017 §4.3，doc_query 同款预留）：读走
	// referenced_docs 语境摘录（format 分派的有界表格摘要），块名仅锁词汇。
	BlockSheetQuery = "sheet_query"
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
	case BlockSheetOps:
		return validateSheetOps(data)
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

// checkDocOpBlocks op 的 blocks 字段：非空数组、≤64、逐块类型封闭、非 hr 块
// text 非空；level/lang 元数据形状（RFC-0016 块模型增补——与 doc 域
// ValidateBlockShape 同语义，端口边界先行拒绝）。
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
		if lvl, present := b["level"]; present {
			n, isNum := lvl.(float64)
			if !isNum || n != float64(int(n)) || typ != "heading" || n < 1 || n > 6 {
				return fmt.Errorf("agent: doc_ops[%d].blocks[%d].level 仅 heading 可携带且为 1..6 整数", i, j)
			}
		}
		if lang, present := b["lang"]; present {
			s, isStr := lang.(string)
			if !isStr || typ != "code" || len([]rune(s)) > 48 || strings.TrimSpace(s) == "" {
				return fmt.Errorf("agent: doc_ops[%d].blocks[%d].lang 仅 code 可携带且为 1..48 字符", i, j)
			}
		}
	}
	return nil
}

// MaxSheetOpsPerWave 单波 sheet_ops 批次上限（RFC-0017 §4.3，doc_ops 同量级）。
const MaxSheetOpsPerWave = 8

// maxSheetOpCells 单 op 携带单元格上限（端口先行拒形；sheet 域单批上限
// 1000 仍由服务端强制——agent 端口收紧到与编辑器防抖批同量级）。
const maxSheetOpCells = 200

var sheetIDPattern = regexp.MustCompile(`^sht_[0-9A-Za-z_-]+$`)

// validateSheetOps sheet_ops 块校验（载荷形 {"ops":[...]}）。编辑类 op 必须
// 携带 base_version（RFC-0017 §4.3："修改必须携带所依据的版本与明确坐标"）
// ——版本失配由引擎代写时按 409 逐 op 降级。
func validateSheetOps(data map[string]any) error {
	ops, ok := data["ops"].([]any)
	if !ok {
		return fmt.Errorf("agent: sheet_ops.ops 必须为数组")
	}
	return checkSheetOps(ops)
}

// checkSheetOps op 列表形状校验（public_draft 侧车与独立块共用）。
func checkSheetOps(ops []any) error {
	if len(ops) == 0 {
		return fmt.Errorf("agent: sheet_ops 至少 1 条 op")
	}
	if len(ops) > MaxSheetOpsPerWave {
		return fmt.Errorf("agent: sheet_ops 超单波上限 %d", MaxSheetOpsPerWave)
	}
	for i, raw := range ops {
		op, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("agent: sheet_ops[%d] 非对象", i)
		}
		kind, _ := op["op"].(string)
		switch kind {
		case "create":
			title, _ := op["title"].(string)
			if n := len([]rune(title)); n < 1 || n > 200 {
				return fmt.Errorf("agent: sheet_ops[%d] create 缺 title 或超 200 字", i)
			}
			if cells, present := op["cells"]; present {
				if err := checkSheetCells(i, cells, maxSheetOpCells); err != nil {
					return err
				}
			}
		case "add_sheet", "set_cells", "clear_range", "insert_rows", "insert_columns", "delete_rows", "delete_columns":
			if err := checkSheetEditOp(i, op, kind); err != nil {
				return err
			}
		default:
			return fmt.Errorf("agent: sheet_ops[%d].op 非法 %q（create|add_sheet|set_cells|clear_range|insert_rows|insert_columns|delete_rows|delete_columns）", i, kind)
		}
	}
	return nil
}

// WaveNum 数值字段兼容读取（JSON 解码恒 float64；Go 原生构造可能是 int——
// room 引擎二次校验与 UT 构造共用）。
func WaveNum(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// checkSheetEditOp 编辑类 op 公共面：doc_id 形态 + base_version ≥1 + sheet_id
// 可选（空 = 默认表）+ 各 op 专属字段。
func checkSheetEditOp(i int, op map[string]any, kind string) error {
	docID, _ := op["doc_id"].(string)
	if !docIDPattern.MatchString(docID) {
		return fmt.Errorf("agent: sheet_ops[%d].doc_id 形如 doc_<12hex>", i)
	}
	if sid, present := op["sheet_id"]; present {
		s, isStr := sid.(string)
		if !isStr || (s != "" && !sheetIDPattern.MatchString(s)) {
			return fmt.Errorf("agent: sheet_ops[%d].sheet_id 形如 sht_*（空 = 默认表）", i)
		}
	}
	bv, ok := WaveNum(op["base_version"])
	if !ok || bv < 1 || bv != float64(int(bv)) {
		return fmt.Errorf("agent: sheet_ops[%d].base_version 必须为 ≥1 整数（所依据的文档版本）", i)
	}
	switch kind {
	case "add_sheet":
		name, _ := op["name"].(string)
		if n := len([]rune(name)); n < 1 || n > 60 {
			return fmt.Errorf("agent: sheet_ops[%d] add_sheet 缺 name 或超 60 字", i)
		}
	case "set_cells":
		if err := checkSheetCells(i, op["cells"], maxSheetOpCells); err != nil {
			return err
		}
	case "clear_range":
		if err := checkSheetRange(i, op["range"]); err != nil {
			return err
		}
	case "insert_rows", "insert_columns", "delete_rows", "delete_columns":
		at, atOK := WaveNum(op["at"])
		cnt, cntOK := WaveNum(op["count"])
		if !atOK || at < 1 || at != float64(int(at)) {
			return fmt.Errorf("agent: sheet_ops[%d].at 必须为 ≥1 整数", i)
		}
		if !cntOK || cnt < 1 || cnt != float64(int(cnt)) || cnt > 1000 {
			return fmt.Errorf("agent: sheet_ops[%d].count 必须为 1..1000 整数", i)
		}
	}
	return nil
}

// checkSheetCells 单元格批形状：数组非空、≤ cap、坐标 1..5000/1..200、
// 类型-值严格配对（datetime 不对 agent 面开放——导入面专属）。
func checkSheetCells(i int, raw any, cap int) error {
	cells, ok := raw.([]any)
	if !ok || len(cells) == 0 {
		return fmt.Errorf("agent: sheet_ops[%d].cells 缺失或为空", i)
	}
	if len(cells) > cap {
		return fmt.Errorf("agent: sheet_ops[%d].cells 超单 op 上限 %d", i, cap)
	}
	for j, rc := range cells {
		c, ok := rc.(map[string]any)
		if !ok {
			return fmt.Errorf("agent: sheet_ops[%d].cells[%d] 非对象", i, j)
		}
		for _, dim := range []struct {
			key string
			max int
		}{{"r", 5000}, {"c", 200}} {
			v, isNum := WaveNum(c[dim.key])
			if !isNum || v < 1 || v > float64(dim.max) || v != float64(int(v)) {
				return fmt.Errorf("agent: sheet_ops[%d].cells[%d].%s 必须为 1..%d 整数", i, j, dim.key, dim.max)
			}
		}
		typ, _ := c["type"].(string)
		switch typ {
		case "text":
			text, isStr := c["text"].(string)
			if !isStr || len(text) > 8192 {
				return fmt.Errorf("agent: sheet_ops[%d].cells[%d] text 须为 ≤8192 字节字符串", i, j)
			}
		case "number":
			if _, isNum := c["number"].(float64); !isNum {
				return fmt.Errorf("agent: sheet_ops[%d].cells[%d] number 缺数值", i, j)
			}
		case "bool":
			if _, isBool := c["bool"].(bool); !isBool {
				return fmt.Errorf("agent: sheet_ops[%d].cells[%d] bool 缺布尔", i, j)
			}
		case "formula":
			f, isStr := c["formula"].(string)
			if !isStr || f == "" || len([]rune(f)) > 512 {
				return fmt.Errorf("agent: sheet_ops[%d].cells[%d] formula 须为 1..512 字符", i, j)
			}
		case "null":
			// 清空
		default:
			return fmt.Errorf("agent: sheet_ops[%d].cells[%d].type 非法 %q（text|number|bool|formula|null）", i, j, typ)
		}
	}
	return nil
}

// checkSheetRange range 字段形状（闭区间四角）。
func checkSheetRange(i int, raw any) error {
	r, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("agent: sheet_ops[%d].range 缺失", i)
	}
	for _, key := range []string{"r1", "c1", "r2", "c2"} {
		v, isNum := WaveNum(r[key])
		if !isNum || v < 1 || v != float64(int(v)) {
			return fmt.Errorf("agent: sheet_ops[%d].range.%s 必须为 ≥1 整数", i, key)
		}
	}
	r1, _ := WaveNum(r["r1"])
	r2, _ := WaveNum(r["r2"])
	c1, _ := WaveNum(r["c1"])
	c2, _ := WaveNum(r["c2"])
	if r2 < r1 || c2 < c1 || r2 > 5000 || c2 > 200 {
		return fmt.Errorf("agent: sheet_ops[%d].range 越界（≤5000 行 ×200 列，r1≤r2、c1≤c2）", i)
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
	// RFC-0017 §4.3 侧车写面：sheet_ops 同纪律。
	if raw, ok := data["sheet_ops"]; ok {
		ops, isArr := raw.([]any)
		if !isArr {
			return fmt.Errorf("agent: public_draft.sheet_ops 必须为数组")
		}
		if err := checkSheetOps(ops); err != nil {
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
