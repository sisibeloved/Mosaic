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

// TestPoliteSilenceOnOwnMessage：礼貌语义（v1.40 结构冷却拆除后的测试基线）——
// 最近消息是自己刚发的（recent 尾条 actor==自己 且 event_id==stimulus_id）→
// 自决 silent；最新是他者消息 → 照常 speak。连续发言的防自言自语依赖本语义。
func TestPoliteSilenceOnOwnMessage(t *testing.T) {
	task := sampleTask(agent.KindEvaluateIntent)
	task.ParticipantID = "par_echo"
	task.Context = agent.Context{Inline: map[string]any{
		"stimulus_id": "evt_m1",
		"recent": []map[string]any{
			{"event_id": "evt_h1", "actor": "par_owner"},
			{"event_id": "evt_m1", "actor": "par_echo"},
		},
	}}
	h, err := (&session{}).Run(context.Background(), task)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Data["action"] != "silent" {
		t.Fatalf("自己最新消息应自决 silent：%v", res.Data["action"])
	}

	task.Context.Inline["stimulus_id"] = "evt_m2"
	task.Context.Inline["recent"] = []map[string]any{
		{"event_id": "evt_h1", "actor": "par_owner"},
		{"event_id": "evt_m2", "actor": "par_other"},
	}
	h2, err := (&session{}).Run(context.Background(), task)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res2, err := h2.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res2.Data["action"] != "speak" {
		t.Fatalf("他者最新消息应 speak：%v", res2.Data["action"])
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
