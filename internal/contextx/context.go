// Package contextx：Context 组装（RFC-0007 七层最小版）与预算账本（RFC-0003 §3.1.4）。
// 纯函数、确定性：层摘要（sha256）使 Context Receipt 可验证——同事件流必同摘要，
// 回放重建与审计据此成立。M1 裁剪：无 embedding 检索层（M3 记忆层接入）。
package contextx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// Layer 组装层：名称 + 内容摘要（内容本身不落 Receipt，摘要可复算验证）。
type Layer struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// BudgetState 预算水位快照（入 L6 层）。
type BudgetState struct {
	RemainingTokens int64 `json:"remaining_tokens"`
	Level           int   `json:"level"` // 0 / 70 / 90 / 100（熔断梯度）
}

// taskDirectiveOf 任务指令层（cross 子轮标注：回应本轮已揭示发言，RFC-0003 §3.1.8）。
func taskDirectiveOf(cfg Config) map[string]any {
	d := map[string]any{"task_id": cfg.TaskID, "note": "任务指令层由适配器提示词承载（M1）"}
	if cfg.Subround > 0 {
		d["subround"] = cfg.Subround
		d["directive"] = "cross-response 子轮：回应本轮已揭示的发言（挑战/补充/收敛），不是新话题"
	}
	return d
}

// Config 组装输入。
type Config struct {
	RoomID       string
	TaskID       string
	Mode         string
	Seats        []Seat
	RecentWindow int // 近期消息窗口（默认 10）
	Budget       BudgetState
	Subround     int // >0 = cross 子轮（任务指令层标注：回应本轮已揭示发言）
}

// Receipt 上下文回执（落库可查：给了什么水位、哪些层、摘要为何）。
type Receipt struct {
	ReceiptID    string   `json:"receipt_id"`
	RoomID       string   `json:"room_id"`
	TaskID       string   `json:"task_id"`
	Watermark    int64    `json:"watermark"` // 组装时的房间版本（seq）
	LayerDigests []string `json:"layer_digests"`
	CreatedAt    string   `json:"created_at"`
}

// Assembled 组装产物：层清单 + 适配器载荷 + 回执。
type Assembled struct {
	Layers  []Layer
	Inline  map[string]any
	Receipt Receipt
}

// Assemble 从房间历史组装七层上下文。
// Seat 参与者座位（引擎侧 AgentSeat 的最小投影，避免反向依赖）。
type Seat struct{ ParticipantID string }

func Assemble(cfg Config, history []protocol.Envelope, stimulus protocol.Envelope) Assembled {
	if cfg.RecentWindow <= 0 {
		cfg.RecentWindow = 10
	}
	watermark := int64(0)
	var messages []protocol.Envelope
	for _, e := range history {
		if e.Seq > watermark {
			watermark = e.Seq
		}
		if e.Type == protocol.EventMessagePosted {
			messages = append(messages, e)
		}
	}
	// 近期窗口（含刺激，按序）
	var recent []map[string]any
	start := len(messages) - cfg.RecentWindow
	if start < 0 {
		start = 0
	}
	for _, m := range messages[start:] {
		var body struct {
			Body        string   `json:"body"`
			AddressedTo []string `json:"addressed_to"`
			ReplyTo     *string  `json:"reply_to"`
		}
		_ = json.Unmarshal(m.Payload, &body)
		recent = append(recent, map[string]any{
			"event_id": m.EventID, "actor": m.Actor.ParticipantID, "kind": m.Actor.Kind,
			"body": body.Body, "addressed_to": body.AddressedTo, "reply_to": body.ReplyTo,
		})
	}
	participants := make([]string, 0, len(cfg.Seats))
	for _, s := range cfg.Seats {
		participants = append(participants, s.ParticipantID)
	}

	stimulusBody := ""
	{
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(stimulus.Payload, &p)
		stimulusBody = p.Body
	}

	relations := map[string]any{"reply_edges": countReplies(messages), "addressed_edges": countAddressed(messages)}
	inline := map[string]any{
		"room_id":                 cfg.RoomID,
		"mode":                    cfg.Mode,
		"participants":            participants,
		"stimulus_body":           stimulusBody,
		"stimulus_id":             stimulus.EventID,
		"recent":                  recent,
		"watermark":               watermark,
		"budget_level":            cfg.Budget.Level,
		"budget_remaining_tokens": cfg.Budget.RemainingTokens,
	}

	// 七层（顺序即组装序；摘要对层内容规范化 JSON 计算）
	layerDefs := []struct {
		name    string
		content any
	}{
		{"charter", map[string]any{"mode": cfg.Mode, "rules": "attention: deterministic; no hidden reasoning"}},
		{"participants", participants},
		{"stimulus", map[string]any{"id": stimulus.EventID, "body": stimulusBody}},
		{"recent_messages", recent},
		{"relations", relations},
		{"budget_watermark", map[string]any{"watermark": watermark, "budget": cfg.Budget}},
		{"task_directive", taskDirectiveOf(cfg)},
	}
	layers := make([]Layer, 0, len(layerDefs))
	digests := make([]string, 0, len(layerDefs))
	for _, def := range layerDefs {
		d := digest(def.content)
		layers = append(layers, Layer{Name: def.name, Digest: d})
		digests = append(digests, d)
	}
	return Assembled{
		Layers: layers,
		Inline: inline,
		Receipt: Receipt{
			ReceiptID:    "rcpt_" + digest([]any{cfg.RoomID, cfg.TaskID, watermark, digests})[:24],
			RoomID:       cfg.RoomID,
			TaskID:       cfg.TaskID,
			Watermark:    watermark,
			LayerDigests: digests,
		},
	}
}

func countReplies(messages []protocol.Envelope) int {
	n := 0
	for _, m := range messages {
		var p struct {
			ReplyTo *string `json:"reply_to"`
		}
		if json.Unmarshal(m.Payload, &p) == nil && p.ReplyTo != nil {
			n++
		}
	}
	return n
}

func countAddressed(messages []protocol.Envelope) int {
	n := 0
	for _, m := range messages {
		var p struct {
			AddressedTo []string `json:"addressed_to"`
		}
		if json.Unmarshal(m.Payload, &p) == nil && len(p.AddressedTo) > 0 {
			n++
		}
	}
	return n
}

func digest(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(fmt.Sprintf("%v", v))
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---- 预算账本（RFC-0003 §3.1.4：预算只作 admission，不进排序）----

// Limits 预算上限（0 = 该维度不限）。
type Limits struct {
	MaxRounds     int64
	MaxUtterances int64
	MaxTokens     int64
}

// Ledger 从事件重建的用量账本。
type Ledger struct {
	Rounds     int64
	Utterances int64
	Tokens     int64 // usage output+input 之和（自报缺失记 0，不虚构）
}

// RebuildBudget 从房间事件重建账本：round.opened 计轮、agent message 计发言、
// metadata.usage 计 token——评估（intent.recorded）、生成（agent message）与
// 撤销收尾（floor.revoked：被 pause/失败撤销的生成也已消耗 token，四轮复审 #13——
// 不入账则账本永久漏计）三侧都入账，三维账本缺任何一侧都会系统性低估
// （人类发言不计入 agent 预算——熔断只停自动续聊）。
func RebuildBudget(events []protocol.Envelope) Ledger {
	var l Ledger
	for _, e := range events {
		switch e.Type {
		case protocol.EventRoundOpened:
			l.Rounds++
		case protocol.EventIntentRecorded:
			if usage, ok := e.Metadata["usage"].(map[string]any); ok {
				l.Tokens += int64Of(usage["output_tokens"]) + int64Of(usage["input_tokens"])
			}
		case protocol.EventMessagePosted:
			if e.Actor.Kind != "agent" {
				continue
			}
			l.Utterances++
			if usage, ok := e.Metadata["usage"].(map[string]any); ok {
				l.Tokens += int64Of(usage["output_tokens"]) + int64Of(usage["input_tokens"])
			}
		case protocol.EventFloorRevoked:
			if usage, ok := e.Metadata["usage"].(map[string]any); ok {
				l.Tokens += int64Of(usage["output_tokens"]) + int64Of(usage["input_tokens"])
			}
		}
	}
	return l
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	default:
		return 0
	}
}

// Level 熔断梯度：任一维度到达 100% 即 100；≥90% 即 90；≥70% 即 70；否则 0。
func (l Ledger) Level(limits Limits) int {
	level := 0
	for _, ratio := range []float64{
		ratioOf(l.Rounds, limits.MaxRounds),
		ratioOf(l.Utterances, limits.MaxUtterances),
		ratioOf(l.Tokens, limits.MaxTokens),
	} {
		switch {
		case ratio >= 1.0 && level < 100:
			level = 100
		case ratio >= 0.9 && level < 90:
			level = 90
		case ratio >= 0.7 && level < 70:
			level = 70
		}
	}
	return level
}

func ratioOf(used, max int64) float64 {
	if max <= 0 {
		return 0
	}
	return float64(used) / float64(max)
}

// Admit 100% 硬停：不再开自动轮（人类消息不受限，RFC-0003 §3.1.4）。
func (l Ledger) Admit(limits Limits) bool { return l.Level(limits) < 100 }

// ReducedSpeakers 90% 降级：speaker 上限减一（下限 1；100% 时无轮可开返回 0）。
func (l Ledger) ReducedSpeakers(limits Limits, current int) int {
	switch l.Level(limits) {
	case 100:
		return 0
	case 90:
		if current > 1 {
			return current - 1
		}
		return 1
	default:
		return current
	}
}

// ReserveOK 对称预留：按"本轮 speaker × responseCap"预估计 token 后仍需在限内。
func (l Ledger) ReserveOK(limits Limits, speakers int, responseCap int64) bool {
	if limits.MaxTokens <= 0 {
		return true
	}
	return l.Tokens+int64(speakers)*responseCap <= limits.MaxTokens
}
