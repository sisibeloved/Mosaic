// Package conformance：适配器 conformance 套件（RFC-0002 §3.5.1 三件套的判定面）。
// 同一套检查判定所有适配器——echo（正确回环）、chaos（违规注入，每个旋钮必须被
// 抓获，见 chaos 包反例门禁）、真实适配器（UT 以桩输出钉结构，IT 以真机验证）。
// 新适配器注册前必须全绿（RFC-0002 A-11）。
package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// Failure 一项契约违反。
type Failure struct {
	Check  string // 检查 ID（boot / kind_mapping / block_structure / ...）
	Detail string
}

// resultTimeout 单任务结果看门狗：Result 永挂（如进程忽视取消）必须被判定失败，
// 而不是让套件跟着挂死。
const resultTimeout = 5 * time.Second

// updatesDrainWindow Streaming=false 声明下 Updates 通道的排空窗口：
// 声明无流式却发射 draft（或通道不关闭）即违例。
const updatesDrainWindow = 300 * time.Millisecond

// Run 执行全部检查，返回违反清单（空 = 通过）。
func Run(a agent.Adapter) []Failure {
	var fails []Failure
	fail := func(check, format string, args ...any) {
		fails = append(fails, Failure{Check: check, Detail: fmt.Sprintf(format, args...)})
	}

	if a.Name() == "" {
		fail("name", "适配器名为空")
		return fails
	}
	caps := a.Capabilities()

	sess, err := a.Boot(context.Background(), agent.Profile{ProfileID: "prof_conformance", Adapter: a.Name()})
	if err != nil {
		fail("boot", "Boot 失败: %v", err)
		return fails
	}
	defer sess.Close()

	// 任务类型 → 结构化块（端口映射，RFC-0002 §3.5.2）
	kindBlocks := []struct {
		kind  agent.TaskKind
		block string
	}{
		{agent.KindEvaluateIntent, agent.BlockTurnIntent},
		{agent.KindGenerate, agent.BlockPublicDraft},
		{agent.KindSummarize, agent.BlockGroundedSummary},
		{agent.KindEvaluateClosure, agent.BlockClosureIntent},
	}
	for _, kb := range kindBlocks {
		runTaskCheck(sess, caps, kb.kind, kb.block, fail)
	}

	// observe 门控：声明支持则必须返回合法 attention_assessment；声明不支持则
	// Run 必须显式拒绝（端口按声明降级，声明与行为不得背离）。
	h, err := sess.Run(context.Background(), sampleTask(agent.KindObserve))
	if caps.Observe {
		if err != nil {
			fail("observe_honesty", "声明 Observe=true 但 observe 任务被拒: %v", err)
		} else {
			res, rerr, ok := resultWithTimeout(h, resultTimeout)
			switch {
			case !ok:
				fail("result_timeout", "observe 任务 Result 超 %v 未返回", resultTimeout)
			case rerr != nil:
				fail("observe_honesty", "observe 任务失败: %v", rerr)
			case res.Block != agent.BlockAttentionAssessment:
				fail("kind_mapping", "observe 块 = %q，应为 %q", res.Block, agent.BlockAttentionAssessment)
			default:
				if verr := agent.ValidateBlock(res.Block, res.Data); verr != nil {
					fail("block_structure", "attention_assessment 结构非法: %v", verr)
				}
			}
		}
	} else if err == nil {
		fail("observe_honesty", "声明 Observe=false 但 observe 任务被接受")
	}

	// 取消契约：Cancel 后 Result 必须返回 ErrStale（迟到拒绝语义，RFC-0002 §3.1.6）。
	h, err = sess.Run(context.Background(), sampleTask(agent.KindGenerate))
	if err != nil {
		fail("cancel_stale", "generate 任务下发失败（取消契约无从验证）: %v", err)
	} else {
		h.Cancel()
		res, rerr, ok := resultWithTimeout(h, resultTimeout)
		switch {
		case !ok:
			fail("cancel_stale", "Cancel 后 Result 超 %v 未返回（适配器忽视取消）", resultTimeout)
		case !errors.Is(rerr, agent.ErrStale):
			fail("cancel_stale", "Cancel 后 Result = (%+v, %v)，应为 ErrStale", res, rerr)
		}
	}
	return fails
}

// runTaskCheck 单任务类型检查链：结果时限 → 块名映射 → 端口级结构校验 →
// 能力声明诚实性（usage / streaming）。
func runTaskCheck(sess agent.Session, caps agent.Capabilities, kind agent.TaskKind, wantBlock string, fail func(string, string, ...any)) {
	h, err := sess.Run(context.Background(), sampleTask(kind))
	if err != nil {
		fail("kind_mapping", "%s 任务下发失败: %v", kind, err)
		return
	}
	if !caps.Streaming {
		if u, open, ok := drainUpdate(h.Updates(), updatesDrainWindow); !ok {
			fail("streaming_honesty", "%s：声明 Streaming=false 但 Updates 通道超 %v 未关闭", kind, updatesDrainWindow)
		} else if open {
			fail("streaming_honesty", "%s：声明 Streaming=false 却发射 draft %+v", kind, u)
		}
	}
	res, rerr, ok := resultWithTimeout(h, resultTimeout)
	if !ok {
		fail("result_timeout", "%s 任务 Result 超 %v 未返回", kind, resultTimeout)
		return
	}
	if rerr != nil {
		fail("kind_mapping", "%s 任务失败: %v", kind, rerr)
		return
	}
	if res.Block != wantBlock {
		fail("kind_mapping", "%s 块 = %q，应为 %q", kind, res.Block, wantBlock)
	}
	if verr := agent.ValidateBlock(res.Block, res.Data); verr != nil {
		fail("block_structure", "%s 块结构非法: %v", kind, verr)
	}
	if !caps.UsageReporting && res.Usage != nil {
		fail("usage_honesty", "%s：声明 UsageReporting=false 却携带 usage %+v（缺失应记 unknown，不虚构）", kind, res.Usage)
	}
	if caps.UsageReporting && res.Usage != nil &&
		(res.Usage.InputTokens < 0 || res.Usage.OutputTokens < 0) {
		fail("usage_honesty", "%s：usage 出现负值 %+v", kind, res.Usage)
	}
}

// sampleTask conformance 标准任务（grant/epoch 齐备，贴近引擎真实下发形态）。
func sampleTask(kind agent.TaskKind) agent.Task {
	return agent.Task{
		TaskID:        "tsk_conformance",
		Kind:          kind,
		ParticipantID: "par_conformance",
		RoomID:        "room_conformance",
		Grant: &agent.Grant{
			GrantID: "grant_conformance", Rank: 1,
			RevealStrategy: "sequential", ViewCursor: "cur_conformance", Epoch: 1,
		},
		Context: agent.Context{Inline: map[string]any{"body": "conformance stimulus"}},
	}
}

// resultWithTimeout Result 看门狗：ok=false 表示超期未返回。
func resultWithTimeout(h agent.Handle, d time.Duration) (res agent.Result, err error, ok bool) {
	type out struct {
		r agent.Result
		e error
	}
	ch := make(chan out, 1)
	go func() {
		r, e := h.Result()
		ch <- out{r, e}
	}()
	select {
	case o := <-ch:
		return o.r, o.e, true
	case <-time.After(d):
		return agent.Result{}, nil, false
	}
}

// drainUpdate 读一次 Updates 通道：ok=false 表示窗口内通道未关闭；
// open=true 表示收到一个还在传输中的 draft。
func drainUpdate(ch <-chan agent.DraftUpdate, d time.Duration) (u agent.DraftUpdate, open bool, ok bool) {
	select {
	case u, open := <-ch:
		return u, open, true
	case <-time.After(d):
		return agent.DraftUpdate{}, false, false
	}
}

// Suite 以 testing 报告 Run 的结果（适配器注册门禁）。
func Suite(t *testing.T, a agent.Adapter) {
	t.Helper()
	for _, f := range Run(a) {
		t.Errorf("conformance[%s]: %s", f.Check, f.Detail)
	}
}
