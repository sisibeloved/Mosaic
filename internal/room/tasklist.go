// tasklist（RFC-0012 OQ-A 修订 / v1.45 负责人裁定；v1.49 派生机制重做；v1.50
// 提出方/负责人分离）：带责任人的任务追踪——常见 Harness tasklist 在多 Agent
// 群聊形态上的必要增量是 owner 字段（每项任务归属具体负责人，否则"谁该交付"
// 无从判定）；v1.50 再分离 requester（提出方）——群聊里 A 指派 B 是常态
// （负责人指令：任务的提出方和负责人得分开显示）。
// 派生机制 v1.46 用自然语言宣言模式匹配（"我来/我会"），狗粮误报严重（假设句/
// 引用/疑问皆可命中）——负责人裁定：不能通过关键字匹配来做。改为显式申报协议：
// agent 在回复 body 末尾附 mosaic-todo 围栏块逐行维护任务（GFM 任务列表语法
// - [ ] / - [x]，可带 @负责人 行缀指派），围栏标记即协议边界——误报率结构性
// 归零，解析仍是确定性纯投影（零 LLM、零模型成本、完全可测）。
// 全量替换语义（同常见 Harness 的 todo write），定域=提出方：再次申报的块即
// 该提出方当前提出的完整任务集——打 x = delivered、消失 = dismissed、新行 =
// 新任务；负责人在自己块里对同文本任务打 x 可交叉结案。人工裁定
// （task.resolved）保留且先行终态有效。清单入评估语境供主动开口消费。
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
	TaskID string `json:"task_id"`
	Owner  string `json:"owner"`
	// Requester 提出方（v1.50：与负责人分离——A 指派 B 时 requester=A、owner=B；
	// 自领任务 requester=owner）。
	Requester     string `json:"requester"`
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
	Requester  string `json:"requester"`
	Text       string `json:"text"`
	WavesSince int    `json:"waves_since"`
	Overdue    bool   `json:"overdue"`
}

// TaskIDOf 派生任务 ID（确定性：源事件 + 负责人 + 归一化文本哈希——同消息
// 多事项、跨消息重申报、指派不同负责人均可稳定引用）。
func TaskIDOf(eventID, owner, text string) string {
	sum := sha256.Sum256([]byte(eventID + "\x1f" + owner + "\x1f" + text))
	return "tsk_" + hex.EncodeToString(sum[:])[:12]
}

// todoDecl 一条申报行（mention 解析前的原始形态；owner 由事件流参与者索引解析）。
type todoDecl struct {
	text    string // 归一化文本（@ 前缀已剥离）
	rawText string // 归一化文本（含 @ 前缀——解析回退自领时保留，信息不丢）
	done    bool
	mention string // @负责人（空 = 自领）
}

// participantIndex 事件流内已见参与者（id → kind），@指派解析与"人工/agent
// 终态可否重开"判定共用。构成：消息/意向/发授的 actor + participant.admitted
// 载荷 + room.created agents 名单（admitted/名单视为 agent）。
type participantIndex map[string]string

func (idx participantIndex) add(pid, kind string) {
	if pid == "" {
		return
	}
	if _, ok := idx[pid]; !ok {
		idx[pid] = kind
	}
}

// resolveMention @指派解析（确定性）：空 → 申报人自领；精确 participant_id；
// 否则 id 的下划线分段与 mention 不区分大小写全等且唯一命中（"@kimi" 命中
// par_kimi_…——agent 在语境里互见的正是这类 id）。负责人必须是 agent 座位
// （人类待办不派生）——解析到人类参与者一律回退自领。歧义/未命中 → 申报人
// 自领（mention 保留在任务文本里，信息不丢失）。
func resolveMention(mention string, idx participantIndex, declarer string) string {
	agentOnly := func(pid string) string {
		if kind, ok := idx[pid]; !ok || kind != "human" {
			return pid
		}
		return declarer
	}
	if mention == "" {
		return declarer
	}
	if _, ok := idx[mention]; ok {
		return agentOnly(mention)
	}
	lower := strings.ToLower(mention)
	hit := ""
	for pid := range idx {
		for _, seg := range strings.Split(pid, "_") {
			if seg != "" && strings.ToLower(seg) == lower {
				if hit != "" && hit != pid {
					return declarer // 歧义：同适配器多实例等
				}
				hit = pid
			}
		}
	}
	if hit != "" {
		return agentOnly(hit)
	}
	return declarer
}

// TasksOf 自事件流重建任务清单（纯函数，回放一致）：agent 消息的 mosaic-todo
// 围栏块申报 → round.opened 计波龄 → task.resolved 人工裁定（先行终态有效）。
// 单遍扫描序即因果序。
func TasksOf(events []StoredEvent) []TaskItem {
	out := []TaskItem{}
	index := map[string]int{} // task_id → out 下标
	pidx := participantIndex{}
	for _, ev := range events {
		env := ev.Envelope
		if env.Actor.ParticipantID != "" {
			pidx.add(env.Actor.ParticipantID, env.Actor.Kind)
		}
		switch env.Type {
		case protocol.EventRoomCreated:
			var p struct {
				Agents []string `json:"agents"`
			}
			if json.Unmarshal(env.Payload, &p) == nil {
				for _, pid := range p.Agents {
					pidx.add(pid, "agent")
				}
			}
		case protocol.EventParticipantAdmitted:
			var p struct {
				ParticipantID string `json:"participant_id"`
			}
			if json.Unmarshal(env.Payload, &p) == nil {
				pidx.add(p.ParticipantID, "agent")
			}
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
				resolved := make([]todoDecl, len(decls))
				for i, d := range decls {
					d.mention = resolveMention(d.mention, pidx, env.Actor.ParticipantID)
					resolved[i] = d
				}
				out = applyTodoDeclaration(out, index, pidx, env, resolved)
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

// applyTodoDeclaration 全量替换语义，定域=提出方（块即该提出方当前提出的完整
// 任务集；自领任务 requester=owner，同属此集）：
//   - requester==申报人的 pending 任务：块内 (负责人,文本) 打 x → delivered；
//     块内消失 → dismissed（撤回指派/收束，未再申报）
//   - 负责人交叉结案：申报人自领行 [x] 可结案 owner==申报人、requester!=申报人
//     的同文本 pending（A 派给 B，B 在自己块里打 x 交差）
//   - 新 [ ] 行（无同 (owner,文本) 存量）→ 新 pending
//
// 已终态（人工裁定过）的任务不受影响；agent 申报/撤回结案的可重开（新任务新
// ID），人工终态不复活（人工门控是权威）。保留项 TaskID/债龄不重置。
// @前缀解析回退自领时（未命中/歧义/指向人类），@token 保留在任务文本里。
func applyTodoDeclaration(tasks []TaskItem, index map[string]int, pidx participantIndex, env protocol.Envelope, decls []todoDecl) []TaskItem {
	requester := env.Actor.ParticipantID
	// 有效文本：@前缀解析回退自领时（未命中/歧义/指向人类/自指）保留前缀，
	// 替换/结案/新建/TaskID 全链路统一用同一键，避免回退形态键漂移。
	type effDecl struct {
		owner string
		text  string
		done  bool
	}
	effs := make([]effDecl, len(decls))
	for i, d := range decls {
		text := d.text
		if d.mention == requester && d.rawText != "" {
			text = d.rawText
		}
		effs[i] = effDecl{owner: d.mention, text: text, done: d.done}
	}
	declByKey := map[string]bool{} // owner \x1f 有效文本 → done
	for _, e := range effs {
		declByKey[e.owner+"\x1f"+e.text] = e.done
	}

	// 1) 替换定域：requester==申报人
	for i := range tasks {
		t := &tasks[i]
		if t.Requester != requester || t.Status != "pending" {
			continue
		}
		done, present := declByKey[t.Owner+"\x1f"+t.Text]
		switch {
		case present && done:
			t.Status, t.Resolution = "delivered", "delivered"
			t.ResolvedBy, t.ResolvedAt = requester, env.OccurredAt
			t.Note = "申报完成（mosaic-todo [x]）"
		case !present:
			t.Status, t.Resolution = "dismissed", "dismissed"
			t.ResolvedBy, t.ResolvedAt = requester, env.OccurredAt
			t.Note = "全量替换后未再申报，自动收束"
		}
	}

	// 2) 负责人交叉结案：自领 [x] 行结案 owner==申报人、他方提出的同文本 pending
	for _, e := range effs {
		if !e.done || e.owner != requester {
			continue
		}
		for i := range tasks {
			t := &tasks[i]
			if t.Status == "pending" && t.Owner == requester && t.Requester != requester && t.Text == e.text {
				t.Status, t.Resolution = "delivered", "delivered"
				t.ResolvedBy, t.ResolvedAt = requester, env.OccurredAt
				t.Note = "负责人申报完成（交叉结案）"
			}
		}
	}

	// 3) 新建（[ ] 行，完成行只结案不新建——完成不存在的事是空账）
	for _, e := range effs {
		if e.done {
			continue
		}
		dup := false
		for i := range tasks {
			t := &tasks[i]
			if t.Owner != e.owner || t.Text != e.text {
				continue
			}
			// pending 保留原任务（TaskID/债龄不变）；agent 结案的终态可重开
			//（提出方撤回后重指派、负责人交差后重领均走新任务）；人工裁定
			// 的终态不复活（人工门控是权威——ResolvedBy 为人类参与者）。
			if t.Status == "pending" {
				dup = true
				break
			}
			if kind, ok := pidx[t.ResolvedBy]; ok && kind == "agent" {
				continue // agent 结案：可重开
			}
			dup = true // 人工终态或未知结案者：不复活
			break
		}
		if dup {
			continue
		}
		nt := TaskItem{
			TaskID:        TaskIDOf(env.EventID, e.owner, e.text),
			Owner:         e.owner,
			Requester:     requester,
			Text:          e.text,
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
// 文本前缀 @token = 负责人指派（token 不含空白）；text 剥离前缀、rawText 保留
// （@ 解析回退自领时以 rawText 入库，信息不丢）。
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
		rest = strings.TrimSpace(rest[3:])
	} else {
		rest = strings.TrimSpace(rest[3:])
	}
	// @负责人 指派前缀（@kimi / @par_kimi_… / @Mavis）
	if strings.HasPrefix(rest, "@") {
		token := rest[1:]
		var body string
		if sp := strings.IndexAny(token, " \t"); sp >= 0 {
			d.mention = strings.Trim(token[:sp], ":：,，")
			body = token[sp:]
		} else {
			d.mention = strings.Trim(token, ":：,，")
			body = ""
		}
		d.rawText = normalizeTaskText(rest)
		rest = body
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
			TaskID: t.TaskID, Owner: t.Owner, Requester: t.Requester, Text: t.Text,
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
