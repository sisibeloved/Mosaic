package echo

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/conformance"
)

// TestConformanceSuite：echo 作为常备正确回环适配器（RFC-0002 §3.5.1 三件套之一），
// 必须过 conformance 全套检查；套件变更对 echo 误伤即套件自身回归。
func TestConformanceSuite(t *testing.T) {
	conformance.Suite(t, Adapter{})
}

func sampleTask(kind agent.TaskKind) agent.Task {
	return agent.Task{
		TaskID: "tsk_01H8TEST000000000000000000",
		Kind:   kind,
		Grant: &agent.Grant{
			GrantID:        "grant_01H8TEST0000000000000000",
			Rank:           1,
			RevealStrategy: "sequential",
			ViewCursor:     "cur_test",
			Epoch:          1,
		},
	}
}

// TestKindBlockMapping：五种任务各自映射到端口规范的结构化块。
func TestKindBlockMapping(t *testing.T) {
	cases := map[agent.TaskKind]string{
		agent.KindObserve:         "attention_assessment",
		agent.KindEvaluateIntent:  "turn_intent",
		agent.KindGenerate:        "public_draft",
		agent.KindSummarize:       "grounded_summary",
		agent.KindEvaluateClosure: "closure_intent",
	}
	for kind, wantBlock := range cases {
		h, err := (&session{}).Run(context.Background(), sampleTask(kind))
		if err != nil {
			t.Fatalf("run %s: %v", kind, err)
		}
		res, err := h.Result()
		if err != nil {
			t.Fatalf("result %s: %v", kind, err)
		}
		if res.Block != wantBlock {
			t.Errorf("kind %s: block = %q, want %q", kind, res.Block, wantBlock)
		}
	}
}

// TestDeterministicOutput：同输入必同输出（conformance 基础断言）。
func TestDeterministicOutput(t *testing.T) {
	task := sampleTask(agent.KindEvaluateIntent)
	marshal := func() string {
		h, err := (&session{}).Run(context.Background(), task)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		res, err := h.Result()
		if err != nil {
			t.Fatalf("result: %v", err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	first, second := marshal(), marshal()
	if first != second {
		t.Errorf("echo output not deterministic:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestCancelBeforeResult：取消后 Result 返回 ErrStale（迟到拒绝语义）。
func TestCancelBeforeResult(t *testing.T) {
	h, err := (&session{}).Run(context.Background(), sampleTask(agent.KindGenerate))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	h.Cancel()
	if _, err := h.Result(); !errors.Is(err, agent.ErrStale) {
		t.Errorf("after cancel: err = %v, want ErrStale", err)
	}
}
