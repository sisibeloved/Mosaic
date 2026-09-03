// tasklist（RFC-0012 OQ-A 修订 / v1.45 负责人裁定）：带责任人的承诺追踪——
// 常见 Harness tasklist 在多 Agent 群聊形态上的必要增量是 owner 字段（每项
// 任务归属具体承诺者，否则"谁该交付"无从判定）。派生是确定性纯投影（宣言
// 模式匹配，零 LLM、零模型成本、完全可测）；生命周期人工门控（delivered/
// dismissed 由人类裁定——自动判定交付会把"说了等于做了"伪装成闭环）。
// 清单入评估语境供主动开口消费：狗粮实证（v1.44）"我曾承诺未交付"是静默期
// agent 自起话的触发源，此前无承载物导致"开拉数据"空转。
package room

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// OverdueWaves 逾期阈值：声明后经过 ≥N 个波仍未 resolved 即 overdue（语境与
// UI 高亮；狗粮实录为宣言后下一波即全静默，2 波是保守起点）。
const OverdueWaves = 2

// commitmentPatterns 宣言模式（curated；狗粮语言为中文）：句子级子串匹配，
// 每条 agent 消息至多派生一项（首个命中句）。误报由人工 dismiss 兜底——
// 模式表宁可漏报不可过泛（"我来/我会"是承诺信号词的真实分布）。
var commitmentPatterns = []string{
	"我来", "我去", "我会", "我将", "我马上", "稍后我", "回头我", "待我",
	"接下来我", "交给我", "让我来", "开拉", "稍后给", "稍后补", "稍后回",
	"稍后处理", "马上处理",
}

// TaskItem 任务清单项（快照投影；语境注入走 TaskBriefsOf 最小投影）。
type TaskItem struct {
	TaskID        string `json:"task_id"`
	Owner         string `json:"owner"`
	Text          string `json:"text"`
	SourceEventID string `json:"source_event_id"`
	DeclaredAt    string `json:"declared_at"`
	DeclaredSeq   int64  `json:"declared_seq"`
	WavesSince    int    `json:"waves_since"`
	Overdue       bool   `json:"overdue"`
	Status        string `json:"status"` // pending | delivered | dismissed
	Resolution    string `json:"resolution,omitempty"`
	Note          string `json:"note,omitempty"`
	ResolvedBy    string `json:"resolved_by,omitempty"`
	ResolvedAt    string `json:"resolved_at,omitempty"`
}

// TaskBrief 语境注入最小投影（contextx tasklist 层；波龄与 overdue 供"何时
// 值得说"消费——OQ-A 触发源）。
type TaskBrief struct {
	TaskID     string `json:"task_id"`
	Owner      string `json:"owner"`
	Text       string `json:"text"`
	WavesSince int    `json:"waves_since"`
	Overdue    bool   `json:"overdue"`
}

// TaskIDOf 派生任务 ID（确定性：源事件哈希——resolve_task 命令引用它）。
func TaskIDOf(eventID string) string {
	sum := sha256.Sum256([]byte(eventID))
	return "tsk_" + hex.EncodeToString(sum[:])[:12]
}

// TasksOf 自事件流重建任务清单（纯函数，回放一致）：agent 消息宣言句派生 →
// round.opened 计波龄 → task.resolved 人工裁定。单遍扫描序即因果序。
func TasksOf(events []StoredEvent) []TaskItem {
	out := []TaskItem{}
	index := map[string]int{} // task_id → out 下标
	appendTask := func(t TaskItem) {
		index[t.TaskID] = len(out)
		out = append(out, t)
	}
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventMessagePosted:
			if env.Actor.Kind != "agent" {
				continue // 人类承诺不走派生（owner 的待办不是 agent 群聊语境的债）
			}
			var p struct {
				Body string `json:"body"`
			}
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			if text, ok := commitmentSentenceOf(p.Body); ok {
				appendTask(TaskItem{
					TaskID:        TaskIDOf(env.EventID),
					Owner:         env.Actor.ParticipantID,
					Text:          text,
					SourceEventID: env.EventID,
					DeclaredAt:    env.OccurredAt,
					DeclaredSeq:   env.Seq,
					Status:        "pending",
				})
			}
		case protocol.EventRoundOpened:
			for i := range out {
				out[i].WavesSince++
			}
		case protocol.EventTaskResolved:
			var p protocol.TaskResolvedPayload
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			if i, ok := index[p.TaskID]; ok && out[i].Status == "pending" {
				out[i].Status = p.Resolution
				out[i].Resolution = p.Resolution
				out[i].Note = p.Note
				out[i].ResolvedBy = p.ResolvedBy
				out[i].ResolvedAt = env.OccurredAt
			}
		}
	}
	for i := range out {
		out[i].Overdue = out[i].Status == "pending" && out[i].WavesSince >= OverdueWaves
	}
	return out
}

// PendingTaskBriefsOf 语境注入位：pending 任务的最小投影（声明序，旧债在前——
// 最老的最该被想起）。
func PendingTaskBriefsOf(envs []protocol.Envelope) []TaskBrief {
	events := storedOfEnvelopes(envs)
	briefs := []TaskBrief{}
	for _, t := range TasksOf(events) {
		if t.Status != "pending" {
			continue
		}
		briefs = append(briefs, TaskBrief{
			TaskID: t.TaskID, Owner: t.Owner, Text: t.Text,
			WavesSince: t.WavesSince, Overdue: t.Overdue,
		})
	}
	return briefs
}

// storedOfEnvelopes 轻量视图转换（投影函数用不到 Cursor）。
func storedOfEnvelopes(envs []protocol.Envelope) []StoredEvent {
	out := make([]StoredEvent, len(envs))
	for i := range envs {
		out[i] = StoredEvent{Envelope: envs[i]}
	}
	return out
}

// commitmentSentenceOf 宣言句提取：按句读切分，返回首个命中宣言模式且非疑问
// 的句子（截断 120 runes——语境注入与 UI 列表的阅读单元）。
func commitmentSentenceOf(body string) (string, bool) {
	for _, s := range splitSentences(body) {
		if isQuestion(s) {
			continue
		}
		for _, pat := range commitmentPatterns {
			if strings.Contains(s, pat) {
				return truncate(s, 120), true
			}
		}
	}
	return "", false
}

// splitSentences 句读切分（中英文句终符 + 换行；保留非空句）。
func splitSentences(body string) []string {
	return strings.FieldsFunc(body, func(r rune) bool {
		switch r {
		case '。', '！', '？', '!', '?', ';', '；', '\n', '\r':
			return true
		}
		return false
	})
}

// isQuestion 疑问句排除（"我来处理好吗？"不是承诺）。
func isQuestion(s string) bool {
	s = strings.TrimSpace(s)
	return strings.ContainsAny(s, "？?") || strings.HasSuffix(s, "吗") || strings.HasSuffix(s, "么")
}

// 含 CJK 判定（关键词提取用）。
func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
