package echo

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

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

// TestAdapterViaSupervisor：经 supervisor 注册、提交、关闭的最小链路。
func TestAdapterViaSupervisor(t *testing.T) {
	sup := agent.NewSupervisor()
	if err := sup.Register(Adapter{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := sup.Register(Adapter{}); err == nil {
		t.Error("duplicate register should fail")
	}

	profile := agent.Profile{ProfileID: "par_echo", Adapter: "echo"}
	h, err := sup.Submit(context.Background(), profile, sampleTask(agent.KindEvaluateIntent))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Block != "turn_intent" {
		t.Errorf("block = %q, want turn_intent", res.Block)
	}

	// 会话复用：同一 Profile 第二次提交走缓存会话。
	h2, err := sup.Submit(context.Background(), profile, sampleTask(agent.KindObserve))
	if err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	if _, err := h2.Result(); err != nil {
		t.Fatalf("result 2: %v", err)
	}

	sup.Shutdown()
}
