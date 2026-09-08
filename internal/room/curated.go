// 策展记忆（v1.70，RFC-0007 Hermes 同构补编）：agent 每波记忆评审的自助写入面。
// 机制对照（Hermes memory_tool_store.py / background_review.py）：
//   - 写入 = 策展条目（add/replace/remove，old_text 子串定位），不是叙述摘要；
//   - 免人工审批（负责人裁定 2026-09-08；Hermes write_approval 默认关闭同构）；
//   - 容量硬门：超限写入拒绝并附现有清单 + 用量（倒逼 agent 下轮合并，不静默截断）；
//   - 安全扫描：不可见 Unicode / 控制字符 / 提示注入套语拒绝（_scan_memory_content 同构）；
//   - 完全重复拒绝（hoarding 防线）；
//   - 人工纠错走既有 memory.edited 事后编辑面（生效于下次组装）。
// 事件溯源：memory.curated 逐 op 留痕（含拒绝原因）；条目 ID 按折叠序确定性分配。
package room

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// CuratedEntryRunesMax 单条策展条目字符上限（条目是"一句话可复用的事实/偏好"，
// 不是便签簿；Hermes 条目同量级）。
const CuratedEntryRunesMax = 200

// CuratedEntry 策展条目投影（最新在后——时间序即追加序；注入时倒序取新）。
// 编辑后视图：memory.edited(curated_content) 覆盖 Content 并记 UpdatedAt。
type CuratedEntry struct {
	ID         string   `json:"id"`
	Author     string   `json:"author"`
	Content    string   `json:"content"`
	SourceRefs []string `json:"source_refs"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	Edited     bool     `json:"edited"`
}

// CuratedBudgetStat 策展面容量水位（与胶囊共单预算——恒常平面一个池）。
type CuratedBudgetStat struct {
	BudgetRunes     int `json:"budget_runes"`
	CuratedRunes    int `json:"curated_runes"`
	CuratedCount    int `json:"curated_count"`
	CapsuleRunes    int `json:"capsule_runes"`
	CapsuleCount    int `json:"capsule_count"`
	OverBudget      int `json:"over_budget_runes"` // 策展+胶囊超出预算的部分（注入裁剪依据）
}

// CuratedEntriesOf 事件流折叠 → 策展条目清单（时间序）。applied 操作依序应用；
// rejected 操作跳过（拒绝原因已在事件里留痕）；memory.edited(curated_content)
// 按条目 ID 覆盖正文（人工纠错生效于下次组装）。
func CuratedEntriesOf(events []StoredEvent) []CuratedEntry {
	entries := []CuratedEntry{}
	nextID := 1
	edits := map[string][]memoryEdit{}
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventMemoryCurated:
			var p protocol.MemoryCuratedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			for _, op := range p.Ops {
				if op.Status != "applied" {
					continue
				}
				switch op.Action {
				case "add":
					e := CuratedEntry{
						ID:         fmt.Sprintf("mem_%d", nextID),
						Author:     p.Author,
						Content:    op.Content,
						SourceRefs: []string{p.RoundID},
						CreatedAt:  p.OccurredAt,
						UpdatedAt:  p.OccurredAt,
					}
					nextID++
					entries = append(entries, e)
				case "replace":
					if i := locateEntry(entries, op.OldText); i >= 0 {
						entries[i].Content = op.Content
						entries[i].UpdatedAt = p.OccurredAt
					}
				case "remove":
					if i := locateEntry(entries, op.OldText); i >= 0 {
						entries = append(entries[:i], entries[i+1:]...)
					}
				}
			}
		case protocol.EventMemoryEdited:
			var p protocol.MemoryEditedPayload
			if json.Unmarshal(env.Payload, &p) == nil && p.MemoryID != "" {
				edits[p.MemoryID] = append(edits[p.MemoryID], memoryEdit{
					MemoryEditedPayload: p, EventID: env.EventID, OccurredAt: env.OccurredAt,
				})
			}
		}
	}
	for i := range entries {
		for _, e := range edits[entries[i].ID] {
			if e.CuratedContent != nil {
				entries[i].Content = *e.CuratedContent
				entries[i].Edited = true
				entries[i].UpdatedAt = e.OccurredAt
			}
		}
	}
	return entries
}

// locateEntry old_text 子串定位：恰一命中返回下标；零命中 -1；多命中 -2（歧义拒绝）。
func locateEntry(entries []CuratedEntry, oldText string) int {
	idx, hits := -1, 0
	for i := range entries {
		if strings.Contains(entries[i].Content, oldText) {
			idx, hits = i, hits+1
			if hits > 1 {
				return -2
			}
		}
	}
	return idx
}

// curatedRunes 条目字符量。
func curatedRunes(e CuratedEntry) int { return len([]rune(e.Content)) }

// CuratedRunesOf 策展面当前用量（查看面与注入面同源）。
func CuratedRunesOf(entries []CuratedEntry) int {
	n := 0
	for _, e := range entries {
		n += curatedRunes(e)
	}
	return n
}

// ApplyCuratedOps Hermes 同构校验门：逐 op 校验并应用，返回新清单与逐 op 结果
// （拒绝原因可审计：容量超限附清单、重复、安全扫描、定位失败/歧义）。
// 免审批——门只拦结构性危害，不拦内容判断（内容判断归评审提示词与人工纠错）。
func ApplyCuratedOps(entries []CuratedEntry, ops []protocol.MemoryOp, budget int, author, roundID, occurredAt string) ([]CuratedEntry, []protocol.MemoryOp) {
	next := append([]CuratedEntry(nil), entries...)
	nextID := len(entries) + 1
	results := make([]protocol.MemoryOp, 0, len(ops))
	inventory := func() string {
		parts := make([]string, 0, len(next))
		for _, e := range next {
			parts = append(parts, fmt.Sprintf("- %s", e.Content))
		}
		return fmt.Sprintf("[%d/%d chars] %s", CuratedRunesOf(next), budget, strings.Join(parts, "\n"))
	}
	for _, op := range ops {
		res := op
		res.Status, res.Reason, res.EntryID = "", "", ""
		switch op.Action {
		case "add":
			content := strings.TrimSpace(op.Content)
			switch {
			case content == "":
				res.Status, res.Reason = "rejected", "add 缺正文"
			case len([]rune(content)) > CuratedEntryRunesMax:
				res.Status, res.Reason = "rejected", fmt.Sprintf("单条上限 %d 字（当前 %d）——拆分或精炼", CuratedEntryRunesMax, len([]rune(content)))
			case scanCuratedContent(content) != "":
				res.Status, res.Reason = "rejected", scanCuratedContent(content)
			case containsExactEntry(next, content):
				res.Status, res.Reason = "rejected", "完全重复（先 replace/remove 既有条目再考虑新增）"
			default:
				// 容量门：加入后超预算即拒绝并附清单（倒逼合并，不静默截断）。
				if CuratedRunesOf(next)+len([]rune(content)) > budget {
					res.Status, res.Reason = "rejected", "超出恒常平面容量——先合并/删除既有条目。当前清单："+inventory()
					break
				}
				e := CuratedEntry{
					ID: fmt.Sprintf("mem_%d", nextID), Author: author, Content: content,
					SourceRefs: []string{roundID}, CreatedAt: occurredAt, UpdatedAt: occurredAt,
				}
				nextID++
				next = append(next, e)
				res.Status, res.EntryID = "applied", e.ID
			}
		case "replace":
			content := strings.TrimSpace(op.Content)
			i := locateEntry(next, strings.TrimSpace(op.OldText))
			switch {
			case content == "":
				res.Status, res.Reason = "rejected", "replace 缺正文"
			case len([]rune(content)) > CuratedEntryRunesMax:
				res.Status, res.Reason = "rejected", fmt.Sprintf("单条上限 %d 字", CuratedEntryRunesMax)
			case scanCuratedContent(content) != "":
				res.Status, res.Reason = "rejected", scanCuratedContent(content)
			case i == -1:
				res.Status, res.Reason = "rejected", "old_text 未命中任何条目。当前清单："+inventory()
			case i == -2:
				res.Status, res.Reason = "rejected", "old_text 命中多条（歧义）——用更长的唯一子串。当前清单："+inventory()
			default:
				delta := len([]rune(content)) - curatedRunes(next[i])
				if CuratedRunesOf(next)+delta > budget {
					res.Status, res.Reason = "rejected", "替换后超出容量。当前清单："+inventory()
					break
				}
				next[i].Content = content
				next[i].UpdatedAt = occurredAt
				res.Status, res.EntryID = "applied", next[i].ID
			}
		case "remove":
			i := locateEntry(next, strings.TrimSpace(op.OldText))
			switch {
			case i == -1:
				res.Status, res.Reason = "rejected", "old_text 未命中任何条目。当前清单："+inventory()
			case i == -2:
				res.Status, res.Reason = "rejected", "old_text 命中多条（歧义）。当前清单："+inventory()
			default:
				res.Status, res.EntryID = "applied", next[i].ID
				next = append(next[:i], next[i+1:]...)
			}
		default:
			res.Status, res.Reason = "rejected", fmt.Sprintf("未知 action %q", op.Action)
		}
		results = append(results, res)
	}
	return next, results
}

func containsExactEntry(entries []CuratedEntry, content string) bool {
	for _, e := range entries {
		if e.Content == content {
			return true
		}
	}
	return false
}

// scanCuratedContent 安全扫描（Hermes _scan_memory_content 同构，保守最小面）：
// 不可见/控制字符（零宽、双向覆盖、控制符——注入与视觉欺骗载体）与提示注入套语。
// 返回拒绝原因；空串 = 通过。秘密形状检测复用发布门口径——条目经注入面回流
// 全体座位，等价于发布面。
func scanCuratedContent(content string) string {
	for _, r := range content {
		if r == '\n' || r == '\t' {
			continue // 条目内允许换行/制表（多行规则）
		}
		if !unicode.IsPrint(r) && r != ' ' {
			return "含不可见/控制字符（拒绝：视觉欺骗与注入载体）"
		}
	}
	lower := strings.ToLower(content)
	for _, pat := range []string{"ignore previous", "ignore all previous", "disregard previous", "system prompt:", "忽略之前", "忽略以上"} {
		if strings.Contains(lower, pat) {
			return "含提示注入套语（拒绝）：" + pat
		}
	}
	return ""
}

// CuratedPlaneView 注入面/查看面共用（不双轨）：最新在前、装满预算即停。
type CuratedPlaneView struct {
	Entries  []CuratedEntry
	Capsules []protocol.ClosureCapsule
	Stat     CuratedBudgetStat
}

// ConstantPlaneOf 恒常平面合并视图（胶囊 + 策展条目，单预算 CapsuleBudgetRunes）：
// 策展条目优先（小而高频的偏好/事实），余量给胶囊（共识结论体积大）。注入裁剪
// 只在平面边界发生（dropped 计数透出）；条目本身从不截断。
func ConstantPlaneOf(envs []protocol.Envelope) CuratedPlaneView {
	entries := CuratedEntriesOf(storedOfEnvelopes(envs))
	capsules, _ := capsuleMemoriesOf(envs)
	stat := CuratedBudgetStat{BudgetRunes: CapsuleBudgetRunes}
	view := CuratedPlaneView{Stat: stat}
	used := 0
	for i := len(entries) - 1; i >= 0; i-- { // 最新在前
		n := curatedRunes(entries[i])
		if used+n > CapsuleBudgetRunes {
			continue // 平面装不下：计超限，不截断条目
		}
		view.Entries = append(view.Entries, entries[i])
		used += n
	}
	view.Stat.CuratedRunes = used
	view.Stat.CuratedCount = len(view.Entries)
	view.Stat.CapsuleCount = len(capsules)
	for _, c := range capsules {
		view.Stat.CapsuleRunes += capsuleRunes(c)
	}
	view.Stat.OverBudget = maxInt(0, CuratedRunesOf(entries)+view.Stat.CapsuleRunes-CapsuleBudgetRunes)
	view.Capsules = capsules
	return view
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
