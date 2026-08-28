// Package echo 提供 conformance 用 echo 适配器（RFC-0002：常备测试适配器）。
// 输出为确定性结构化块：同输入必同输出，供回放与门禁断言。
package echo

import (
	"context"
	"sync"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// Adapter 实现 agent.Adapter。
type Adapter struct{}

func (Adapter) Name() string { return "echo" }

func (Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false,
		CancelMode:     "none",
		HistoryChannel: "structured_request",
		Continuity:     true,
		UsageReporting: false, // echo 不自报 usage：显式 unknown
		Observe:        true,
	}
}

func (Adapter) Boot(_ context.Context, _ agent.Profile) (agent.Session, error) {
	return &session{}, nil
}

type session struct{}

func (*session) Run(_ context.Context, task agent.Task) (agent.Handle, error) {
	return &handle{task: task}, nil
}

func (*session) Cancel(string) {}
func (*session) Close()        {}

type handle struct {
	mu        sync.Mutex
	task      agent.Task
	cancelled bool
	done      bool
	result    agent.Result
}

func (h *handle) Updates() <-chan agent.DraftUpdate {
	ch := make(chan agent.DraftUpdate)
	close(ch) // 无草稿流能力：立即关闭（端口按能力声明降级）
	return ch
}

func (h *handle) Cancel() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelled = true
}

func (h *handle) Result() (agent.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancelled {
		return agent.Result{}, agent.ErrStale
	}
	if !h.done {
		h.result = deterministicResult(h.task)
		h.done = true
	}
	return h.result, nil
}

// deterministicResult 按 task kind 返回固定结构化块（echo 语义：确定性优先于真实性）。
func deterministicResult(task agent.Task) agent.Result {
	switch task.Kind {
	case agent.KindObserve:
		return agent.Result{
			Block: "attention_assessment",
			Data:  map[string]any{"salience": 0.5, "disposition": "consider"},
		}
	case agent.KindEvaluateIntent:
		return agent.Result{
			Block: "turn_intent",
			Data: map[string]any{
				"action":          "speak",
				"type":            "extend",
				"public_rationale": "echo adapter: deterministic intent",
				"scores": map[string]any{
					"relevance": 0.5, "novelty": 0.5, "urgency": 0.5, "confidence": 0.5,
				},
				"estimated_tokens": 64,
			},
		}
	case agent.KindGenerate:
		return agent.Result{
			Block: "public_draft",
			Data:  map[string]any{"body": "[echo] deterministic draft", "declared_relations": []any{}},
		}
	case agent.KindSummarize:
		return agent.Result{
			Block: "grounded_summary",
			Data:  map[string]any{"summary": "[echo] deterministic summary", "cited_event_ids": []any{"evt_echo_0"}},
		}
	case agent.KindEvaluateClosure:
		return agent.Result{
			Block: "closure_intent",
			Data:  map[string]any{"action": "abstain"},
		}
	default:
		return agent.Result{Block: "unsupported"}
	}
}
