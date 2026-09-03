// v1.48 运行参数 argv 契约：模型覆盖（-m）全任务生效；评估任务 EvalModel 优先
// 于 Model（降档不被主模型吞掉）；思考强度（-c model_reasoning_effort=）首轮
// 与 resume 面均带（2026-09-03 实证 resume 接受 -m/-c）。
package codex

import (
	"context"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

func runSilentTask(t *testing.T, cfg Config, kind agent.TaskKind) *fakeExecer {
	t.Helper()
	msg := `{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"{\"action\":\"silent\"}"}}` + "\n" +
		`{"type":"turn.completed"}`
	first := `{"type":"thread.started","thread_id":"thr_1"}` + "\n" + msg
	exec := &fakeExecer{outputs: []string{first, msg}}
	cfg.CodexPath = "/bin/codex"
	cfg.Execer = exec
	cfg.Timeout = 30 * time.Second
	a := New(cfg)
	session, _ := a.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "codex"})
	defer session.Close()
	h, err := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: kind})
	if err != nil {
		t.Fatalf("run %s: %v", kind, err)
	}
	_, _ = h.Result()
	return exec
}

func TestRuntimeModelArgsCodex(t *testing.T) {
	exec := runSilentTask(t, Config{Model: "gpt-5.6-sol", ReasoningEffort: "xhigh"}, agent.KindEvaluateIntent)
	argv := exec.calls[0].argv
	if !contains(argv, "-m") || !contains(argv, "gpt-5.6-sol") {
		t.Fatalf("主模型覆盖应注入 -m: %v", argv)
	}
	if !contains(argv, "-c") || !contains(argv, "model_reasoning_effort=xhigh") {
		t.Fatalf("思考强度应注入 -c: %v", argv)
	}
}

func TestRuntimeEvalModelWinsCodex(t *testing.T) {
	cfg := Config{Model: "gpt-5.6-sol", EvalModel: "mini-x", ReasoningEffort: "low"}
	// 评估任务：EvalModel 优先；生成任务：主模型
	exec := runSilentTask(t, cfg, agent.KindEvaluateIntent)
	if !contains(exec.calls[0].argv, "mini-x") || contains(exec.calls[0].argv, "gpt-5.6-sol") {
		t.Fatalf("评估应 EvalModel 优先: %v", exec.calls[0].argv)
	}
	exec2 := runSilentTask(t, cfg, agent.KindGenerate)
	last := exec2.calls[len(exec2.calls)-1].argv
	if !contains(last, "gpt-5.6-sol") || contains(last, "mini-x") {
		t.Fatalf("生成应用主模型: %v", last)
	}
}

func TestRuntimeEffortOnResumeCodex(t *testing.T) {
	msg := `{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"{\"action\":\"silent\"}"}}` + "\n" +
		`{"type":"turn.completed"}`
	exec := &fakeExecer{outputs: []string{`{"type":"thread.started","thread_id":"thr_9"}` + "\n" + msg, msg, msg}}
	a2 := New(Config{CodexPath: "/bin/codex", Execer: exec, Timeout: 30 * time.Second, Model: "m1", ReasoningEffort: "high"})
	session, _ := a2.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "codex"})
	defer session.Close()
	for _, kind := range []agent.TaskKind{agent.KindEvaluateIntent, agent.KindGenerate} {
		h, _ := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: kind})
		_, _ = h.Result()
	}
	resumeArgv := exec.calls[1].argv // 第二次任务走 exec resume
	hasResume := false
	for _, v := range resumeArgv {
		if v == "resume" {
			hasResume = true
		}
	}
	if !hasResume {
		t.Fatalf("第二次任务应走 resume: %v", resumeArgv)
	}
	if !contains(resumeArgv, "-m") || !contains(resumeArgv, "m1") || !contains(resumeArgv, "model_reasoning_effort=high") {
		t.Fatalf("resume 面应带模型与强度（实证 2026-09-03）: %v", resumeArgv)
	}
}

func TestRuntimeNoOverrideNoArgsCodex(t *testing.T) {
	exec := runSilentTask(t, Config{}, agent.KindEvaluateIntent)
	if contains(exec.calls[0].argv, "-m") || contains(exec.calls[0].argv, "-c") {
		t.Fatalf("无覆盖不得注入（尊重 CLI 配置默认）: %v", exec.calls[0].argv)
	}
}
