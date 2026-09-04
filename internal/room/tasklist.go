// tasklist（RFC-0012 OQ-A 修订 / v1.45 负责人裁定；v1.49 派生机制重做）：带责任人
// 的承诺追踪——常见 Harness tasklist 在多 Agent 群聊形态上的必要增量是 owner 字段
// （每项任务归属具体承诺者，否则"谁该交付"无从判定）。
// 派生机制 v1.46 用自然语言宣言模式匹配（"我来/我会"），狗粮误报严重（假设句/
// 引用/疑问皆可命中）——负责人裁定：不能通过关键字匹配来做。改为显式申报协议：
// agent 在回复 body 末尾附 mosaic-todo 围栏块逐行维护自己名下的全量在办事项
// （GFM 任务列表语法 - [ ] / - [x]），围栏标记即协议边界——误报率结构性归零，
// 解析仍是确定性纯投影（零 LLM、零模型成本、完全可测）。
// 全量替换语义（同常见 Harness 的 todo write）：再次申报的块即该 owner 当前
// 完整在办清单——打 x = delivered、消失 = dismissed、新行 = 新任务。人工裁定
// （task.resolved）保留且先行终态有效（自动申报不伪装人工闭环，反之亦然）。
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

// OverdueWaves 逾期阈值：申报后经过 ≥N 个波仍未 resolved 即 overdue（语境与
// UI 高亮；狗粮实录为申报后下一波即全静默，2 波是保守起点）。
const OverdueWaves = 2

// todoFence 申报围栏标记（协议边界：正文里的自然语言永不派生）。
const todoFence = "```mosaic-todo"

// maxTodoItems 单块申报上限（与记忆编辑 ≤12 同口径——申报面也要有容量纪律）。
const maxTodoItems = 12

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

// TaskIDOf 派生任务 ID（确定性：源事件 + 归一化事项文本哈希——同消息多事项、
// 跨消息重申报同文本均可稳定引用）。
func TaskIDOf(eventID, text string) string {
	sum := sha256.Sum256([]byte(eventID + "\x1f" + text))
	return "tsk_" + hex.EncodeToString(sum[:])[:12]
}

// todoDecl 一条申报行。
type todoDecl struct {
	text string
	done bool
}

// TasksOf 自事件流重建任务清单（纯函数，回放一致）：agent 消息的 mosaic-todo
// 围栏块申报 → round.opened 计波龄 → task.resolved 人工裁定（先行终态有效）。
// 单遍扫描序即因果序。
func TasksOf(events []StoredEvent) []TaskItem {
	out := []TaskItem{}
	index := map[string]int{} // task_id → out 下标
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventMessagePosted:
			if env.Actor.Kind != "agent" {
				continue // 人类待办不是 agent 群聊语境的债（owner 须是 agent 座位）
			}
			var p struct {
				Body string `json:"body"`
			}
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			if decls, ok := todoDeclarations(p.Body); ok {
				out = applyTodoDeclaration(out, index, env, decls)
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

// applyTodoDeclaration 全量替换语义：块即该 owner 当前完整在办清单。
//   - 原 pending 且块内打 x → delivered（agent 申报完成）
//   - 原 pending 且块内消失 → dismissed（未再申报，自动收束）
//   - 原 pending 且块内保留 [ ] → 维持（TaskID/波龄/申报时间不变——债龄自首次申报起算）
//   - 块内新 [ ] 行（无同 owner 同文本任务）→ 新 pending
//
// 已终态（人工裁定过）的任务不受影响；delivered 后再次申报同文本视为新债（新 TaskID）。
func applyTodoDeclaration(tasks []TaskItem, index map[string]int, env protocol.Envelope, decls []todoDecl) []TaskItem {
	owner := env.Actor.ParticipantID
	declared := map[string]bool{} // 归一化文本 → 是否 [x]
	for _, d := range decls {
		declared[d.text] = d.done
	}
	for i := range tasks {
		t := &tasks[i]
		if t.Owner != owner || t.Status != "pending" {
			continue
		}
		done, present := declared[t.Text]
		switch {
		case present && done:
			t.Status, t.Resolution = "delivered", "delivered"
			t.ResolvedBy, t.ResolvedAt = owner, env.OccurredAt
			t.Note = "agent 申报完成（mosaic-todo [x]）"
		case !present:
			t.Status, t.Resolution = "dismissed", "dismissed"
			t.ResolvedBy, t.ResolvedAt = owner, env.OccurredAt
			t.Note = "全量替换后未再申报，自动收束"
		}
	}
	for _, d := range decls {
		if d.done {
			continue // 完成行只用于结案，不新建（完成不存在的事是空账）
		}
		dup := false
		for i := range tasks {
			if tasks[i].Owner != owner || tasks[i].Text != d.text {
				continue
			}
			// pending 保留原任务（TaskID/债龄不变）；agent 自报结案的可重开
			// （新任务新 ID）；人工裁定的终态不复活（人工门控是权威）。
			if tasks[i].Status == "pending" || tasks[i].ResolvedBy != owner {
				dup = true
			}
			break
		}
		if dup {
			continue
		}
		nt := TaskItem{
			TaskID:        TaskIDOf(env.EventID, d.text),
			Owner:         owner,
			Text:          d.text,
			SourceEventID: env.EventID,
			DeclaredAt:    env.OccurredAt,
			DeclaredSeq:   env.Seq,
			Status:        "pending",
		}
		index[nt.TaskID] = len(tasks)
		tasks = append(tasks, nt)
	}
	return tasks
}

// todoDeclarations 提取 body 中最后一个 mosaic-todo 围栏块的任务行。
// 多块时最后一块生效（终态语义）；未闭合块容忍（模型截断兜底）——但零有效行
// 的块视为无申报（防误触全量清空）。返回 ok=false 表示本次消息不构成申报。
func todoDeclarations(body string) ([]todoDecl, bool) {
	var decls []todoDecl
	inBlock := false
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !inBlock {
			if strings.HasPrefix(trimmed, todoFence) {
				inBlock = true
				decls = nil // 新块重置：最后一块生效
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			inBlock = false
			continue
		}
		if d, ok := parseTodoLine(trimmed); ok && len(decls) < maxTodoItems {
			decls = append(decls, d)
		}
	}
	return decls, len(decls) > 0
}

// parseTodoLine GFM 任务列表行：- [ ] 文本 / - [x] 文本（子弹符 - * + 均收）。
func parseTodoLine(s string) (todoDecl, bool) {
	var d todoDecl
	rest := strings.TrimLeft(s, "-*+")
	if rest == s {
		return d, false
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "[ ]") {
		if len(rest) < 3 || rest[0] != '[' || (rest[1] != 'x' && rest[1] != 'X') || rest[2] != ']' {
			return d, false
		}
		d.done = true
		rest = rest[3:]
	} else {
		rest = rest[3:]
	}
	d.text = normalizeTaskText(rest)
	return d, d.text != ""
}

// normalizeTaskText 归一化：空白折叠、去尾部标点、截断 120 runes（语境注入与
// UI 的阅读单元；跨申报匹配以归一化文本为键）。
func normalizeTaskText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, "。．.!！?？;；,，~～")
	return truncate(s, 120)
}

// PendingTaskBriefsOf 语境注入位：pending 任务的最小投影（申报序，旧债在前——
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

// 含 CJK 判定（关键词提取用）。
func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
