// Package slowrun 提供开发者模式（-dev）的受控执行适配器：M4-1 ST 与本地
// 验证用——声明 TaskRuns 能力（echo 如实拒绝），指令内 "SLEEP <秒>" 控制
// 执行时长（ctx 取消即中断），其余行为确定性。仅经 app 的 Dev 门注册入席，
// 生产装配（无 -dev）不出现该座位；波内评估恒 silent（执行通道专用，不参与
// 群聊发言——不伪造过程态）。
package slowrun

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// sleepCap 桩内时长上限（防 ST 误写长挂）。
const sleepCap = 120 * time.Second

var sleepPattern = regexp.MustCompile(`SLEEP\s+(\d+)`)

// sleepOf 指令中的受控时长；无标记 = 立即完成。
func sleepOf(instruction string) time.Duration {
	m := sleepPattern.FindStringSubmatch(instruction)
	if m == nil {
		return 0
	}
	sec, err := strconv.Atoi(m[1])
	if err != nil || sec <= 0 {
		return 0
	}
	d := time.Duration(sec) * time.Second
	if d > sleepCap {
		d = sleepCap
	}
	return d
}

// Adapter 实现 agent.Adapter。
type Adapter struct{}

func (Adapter) Name() string { return "slowrun" }

func (Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false,
		CancelMode:     "context",
		HistoryChannel: "structured_request",
		Continuity:     true,
		UsageReporting: false,
		TaskRuns:       true, // M4-1：受控执行面（与 echo 的如实拒绝对照）
		Observe:        true,
	}
}

func (Adapter) Boot(_ context.Context, _ agent.Profile) (agent.Session, error) {
	return &session{}, nil
}

type session struct{}

func (*session) Run(ctx context.Context, task agent.Task) (agent.Handle, error) {
	h := &handle{task: task, cancelCh: make(chan struct{})}
	if d := sleepOf(instructionOf(task)); d > 0 {
		go func() { // 取消即中断：ctx/handle 双路（引擎 CancelRun 两者都打）
			select {
			case <-ctx.Done():
				h.Cancel()
			case <-h.cancelCh:
			}
		}()
	}
	return h, nil
}
func (*session) Cancel(string) {}
func (*session) Close()        {}

// instructionOf 语境取指令（引擎 runContext 注入的 instruction 键）。
func instructionOf(task agent.Task) string {
	if s, ok := task.Context.Inline["instruction"].(string); ok {
		return s
	}
	return ""
}

type handle struct {
	mu       sync.Mutex
	task     agent.Task
	cancelCh chan struct{}
	result   agent.Result
	done     bool
}

func (h *handle) Updates() <-chan agent.DraftUpdate {
	ch := make(chan agent.DraftUpdate)
	close(ch) // 无草稿流能力：立即关闭（端口按能力声明降级）
	return ch
}

func (h *handle) Cancel() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.cancelCh:
	default:
		close(h.cancelCh)
	}
}

func (h *handle) Result() (agent.Result, error) {
	d := sleepOf(instructionOf(h.task))
	h.mu.Lock()
	cancelCh := h.cancelCh
	h.mu.Unlock()
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-cancelCh:
			return agent.Result{}, agent.ErrStale
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.done {
		h.result = deterministicResult(h.task)
		h.done = true
	}
	return h.result, nil
}

// deterministicResult 按 task kind 返回固定结构化块（slowrun 语义：受控时长
// 的确定性输出；执行正文带指令回显供 ST 断言溯源）。
func deterministicResult(task agent.Task) agent.Result {
	switch task.Kind {
	case agent.KindObserve:
		return agent.Result{Block: "attention_assessment", Data: map[string]any{"salience": 0.5, "disposition": "consider"}}
	case agent.KindEvaluateIntent:
		// 波内恒静默：本桩只服务执行通道，不参与群聊发言
		return agent.Result{
			Block: "turn_intent",
			Data:  map[string]any{"action": "silent", "public_rationale": "slowrun adapter: run-channel only"},
		}
	case agent.KindGenerate:
		return agent.Result{
			Block: "public_draft",
			Data: map[string]any{
				"body":               fmt.Sprintf("[slowrun] 独立任务执行完成：%s", instructionOf(task)),
				"declared_relations": []any{},
			},
		}
	case agent.KindSummarize:
		return agent.Result{Block: "grounded_summary", Data: map[string]any{"summary": "[slowrun] deterministic summary", "cited_event_ids": []any{}}}
	case agent.KindEvaluateClosure:
		return agent.Result{Block: "closure_intent", Data: map[string]any{"action": "abstain"}}
	default:
		return agent.Result{Block: "unsupported"}
	}
}
