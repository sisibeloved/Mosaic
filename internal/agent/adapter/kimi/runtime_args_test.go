// v1.48 运行参数 argv 契约：模型覆盖（-m）全任务生效；评估任务 EvalModel 优先
// 于 Model。kimi 无思考强度面（thinking 内建于模型能力，实证 0.39.1）。
package kimi

import (
	"context"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

func TestRuntimeModelArgsKimi(t *testing.T) {
	out := `{"role":"assistant","content":"{\"action\":\"silent\"}"}` + "\n"
	exec := &fakeExecer{outputs: []string{out, out, out}}
	adapter := New(Config{KimiPath: "/bin/kimi", Execer: exec, Timeout: 30 * time.Second,
		Model: "kimi-code/k3-256k", EvalModel: "k2-fast"})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "kimi"})
	defer session.Close()
	run := func(kind agent.TaskKind) {
		h, err := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: kind})
		if err != nil {
			t.Fatalf("run %s: %v", kind, err)
		}
		_, _ = h.Result()
	}
	run(agent.KindEvaluateIntent)
	run(agent.KindGenerate)
	// 评估：EvalModel 优先；生成：主模型
	if !hasArg(exec.calls[0].argv, "k2-fast") || hasArg(exec.calls[0].argv, "kimi-code/k3-256k") {
		t.Fatalf("评估应 EvalModel 优先: %v", exec.calls[0].argv)
	}
	if !hasArg(exec.calls[1].argv, "kimi-code/k3-256k") || hasArg(exec.calls[1].argv, "k2-fast") {
		t.Fatalf("生成应用主模型: %v", exec.calls[1].argv)
	}
	// 无覆盖：不注入（尊重 kimi config.toml 的 default_model）
	exec2 := &fakeExecer{outputs: []string{out, out}}
	adapter2 := New(Config{KimiPath: "/bin/kimi", Execer: exec2, Timeout: 30 * time.Second})
	session2, _ := adapter2.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "kimi"})
	defer session2.Close()
	h, _ := session2.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	_, _ = h.Result()
	if hasArg(exec2.calls[0].argv, "-m") {
		t.Fatalf("无覆盖不得注入 -m: %v", exec2.calls[0].argv)
	}
}
