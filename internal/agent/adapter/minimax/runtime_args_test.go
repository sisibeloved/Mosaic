// v1.48 运行参数 argv 契约：模型覆盖（--model provider/model）全任务生效；
// 评估任务 EvalModel 优先于 Model。mcode 无思考强度面（实证 0.2.7）。
package minimax

import (
	"context"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

func TestRuntimeModelArgsMinimax(t *testing.T) {
	out := `{"type":"message","role":"assistant","content":"{\"action\":\"silent\"}"}` + "\n" +
		`{"type":"exec.completed","result":{"status":"succeeded"}}` + "\n"
	exec := &fakeExecer{outputs: []string{out, out, out}}
	adapter := New(Config{McodePath: "/bin/mcode", Execer: exec, Timeout: 30 * time.Second,
		Model: "minimax/MiniMax-M2", EvalModel: "minimax/mini"})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "minimax"})
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
	if !hasArg(exec.calls[0].argv, "minimax/mini") || hasArg(exec.calls[0].argv, "minimax/MiniMax-M2") {
		t.Fatalf("评估应 EvalModel 优先: %v", exec.calls[0].argv)
	}
	if !hasArg(exec.calls[1].argv, "minimax/MiniMax-M2") || hasArg(exec.calls[1].argv, "minimax/mini") {
		t.Fatalf("生成应用主模型: %v", exec.calls[1].argv)
	}
	// 无覆盖：不注入
	exec2 := &fakeExecer{outputs: []string{out, out}}
	adapter2 := New(Config{McodePath: "/bin/mcode", Execer: exec2, Timeout: 30 * time.Second})
	session2, _ := adapter2.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "minimax"})
	defer session2.Close()
	h, _ := session2.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	_, _ = h.Result()
	if hasArg(exec2.calls[0].argv, "--model") {
		t.Fatalf("无覆盖不得注入 --model: %v", exec2.calls[0].argv)
	}
}
