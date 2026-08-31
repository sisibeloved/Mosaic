// Package chaos：conformance 反例适配器（RFC-0002 §3.5.1 三件套之二——
// 迟到/畸形结构化块/崩溃/取消场景）。每个 Knobs 旋钮注入一类端口契约违规；
// 门禁（chaos_test.go）：每个旋钮必须被 conformance 套件抓获，零旋钮（clean）
// 必须通过——套件对"正确的 chaos"不得误伤。
package chaos

import (
	"context"
	"errors"
	"sync"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// Knobs 违规注入旋钮。
type Knobs struct {
	WrongBlock       bool // kind→block 张冠李戴（evaluate_intent 返回 public_draft 等）
	MalformedData    bool // 畸形结构化块（分数字符串化/空正文/缺字段/非法枚举）
	IgnoreCancel     bool // Cancel 不生效：照常返回结果
	CancelHangs      bool // Cancel 后 Result 永不返回（进程忽视终止信号的场景代理）
	BootFails        bool // Boot 直接失败（启动即崩溃的场景代理）
	StreamingLie     bool // 声明 Streaming=false 却发射 draft
	UsageFabrication bool // 声明 UsageReporting=false 却虚构 usage
	ObserveLie       bool // 声明 Observe=false 却接受 observe 任务
}

// Adapter 实现 agent.Adapter；能力声明恒为"最小诚实面"，违规靠旋钮注入。
type Adapter struct {
	Knobs Knobs
}

func (a Adapter) Name() string { return "chaos" }

func (a Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false,
		CancelMode:     "notify",
		HistoryChannel: "structured_request",
		Continuity:     true,
		UsageReporting: false,
		Observe:        false,
	}
}

func (a Adapter) Boot(_ context.Context, _ agent.Profile) (agent.Session, error) {
	if a.Knobs.BootFails {
		return nil, errors.New("chaos: boot 崩溃（BootFails 旋钮）")
	}
	return &session{knobs: a.Knobs}, nil
}

type session struct {
	knobs Knobs
}

func (s *session) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	if task.Kind == agent.KindObserve && !s.knobs.ObserveLie {
		return nil, errors.New("chaos: 不支持 observe（声明 Observe=false 的诚实路径）")
	}
	block := blockFor(task.Kind)
	data := validData(task.Kind)
	if s.knobs.WrongBlock {
		block = wrongBlockFor(task.Kind)
	}
	if s.knobs.MalformedData {
		data = malformedData(task.Kind)
	}
	res := agent.Result{Block: block, Data: data, StopReason: "stop"}
	if s.knobs.UsageFabrication {
		res.Usage = &agent.Usage{InputTokens: 5, OutputTokens: 3, Model: "chaos"}
	}
	return &handle{knobs: s.knobs, result: res}, nil
}

func (*session) Cancel(string) {}
func (*session) Close()        {}

type handle struct {
	knobs  Knobs
	result agent.Result

	mu        sync.Mutex
	cancelled bool
}

func (h *handle) Updates() <-chan agent.DraftUpdate {
	ch := make(chan agent.DraftUpdate, 1)
	if h.knobs.StreamingLie {
		ch <- agent.DraftUpdate{Kind: "stage", Stage: "generating"} // 声明外的 draft
	}
	close(ch)
	return ch
}

func (h *handle) Cancel() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelled = true
}

func (h *handle) Result() (agent.Result, error) {
	h.mu.Lock()
	cancelled := h.cancelled
	h.mu.Unlock()
	if cancelled && h.knobs.CancelHangs {
		select {} // 忽视取消：永不返回（conformance 看门狗抓获；测试进程退出时随进程回收）
	}
	if cancelled && !h.knobs.IgnoreCancel {
		return agent.Result{}, agent.ErrStale
	}
	return h.result, nil
}

// blockFor 正确的 kind→block 映射。
func blockFor(kind agent.TaskKind) string {
	switch kind {
	case agent.KindObserve:
		return agent.BlockAttentionAssessment
	case agent.KindEvaluateIntent:
		return agent.BlockTurnIntent
	case agent.KindGenerate:
		return agent.BlockPublicDraft
	case agent.KindSummarize:
		return agent.BlockGroundedSummary
	case agent.KindEvaluateClosure:
		return agent.BlockClosureIntent
	default:
		return "unsupported"
	}
}

// wrongBlockFor 张冠李戴映射（块名错位，数据保持原 kind 的合法形态）。
func wrongBlockFor(kind agent.TaskKind) string {
	if kind == agent.KindGenerate {
		return agent.BlockTurnIntent
	}
	return agent.BlockPublicDraft
}

// validData 各 kind 的合法数据（clean 形态）。
func validData(kind agent.TaskKind) map[string]any {
	switch kind {
	case agent.KindObserve:
		return map[string]any{"salience": 0.5, "disposition": "consider"}
	case agent.KindEvaluateIntent:
		return map[string]any{
			"action": "speak", "type": "extend", "public_rationale": "chaos 合法 intent",
			"scores": map[string]any{"relevance": 0.5, "novelty": 0.5, "urgency": 0.5, "confidence": 0.5},
		}
	case agent.KindGenerate:
		return map[string]any{"body": "[chaos] clean draft", "declared_relations": []any{}}
	case agent.KindSummarize:
		return map[string]any{"summary": "[chaos] clean summary", "cited_event_ids": []any{}}
	case agent.KindEvaluateClosure:
		return map[string]any{"action": "abstain"}
	default:
		return map[string]any{}
	}
}

// malformedData 各 kind 的畸形数据（结构校验必须抓获）。
func malformedData(kind agent.TaskKind) map[string]any {
	switch kind {
	case agent.KindEvaluateIntent:
		return map[string]any{
			"action": "speak", "type": "extend",
			"scores": map[string]any{"relevance": "0.5", "novelty": 0.5, "urgency": 0.5, "confidence": 0.5},
		} // 字符串分数（v1.6 审校 #8 形态）
	case agent.KindGenerate:
		return map[string]any{"body": "", "declared_relations": []any{}} // 空正文
	case agent.KindSummarize:
		return map[string]any{"summary": "[chaos] 无引用摘要"} // 缺 cited_event_ids
	case agent.KindEvaluateClosure:
		return map[string]any{"action": "merge"} // 非法枚举
	default:
		return map[string]any{"salience": "high"} // observe：salience 非数值
	}
}
