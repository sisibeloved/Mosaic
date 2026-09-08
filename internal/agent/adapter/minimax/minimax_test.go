// UT 层：minimax（mcode）适配器——stream-json 解析（fixtures 钉 0.2.7）、argv 契约
// （exec 无头 + --input - stdin 提示词 + --session 恢复 + --cwd）、发布门委托、
// 取消语义、conformance 套件（桩输出钉结构）。真机三件套见 minimax_it_test.go（IT 层）。
package minimax

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/agent/conformance"
)

// ---- Execer 捕获装置 ----

type capturedCall struct {
	argv  []string
	env   []string
	stdin string
}

type fakeExecer struct {
	mu      sync.Mutex
	calls   []capturedCall
	outputs []string // 按调用序返回预置 JSONL stdout
	code    int
	err     error
	block   bool // 阻塞至 ctx 取消（取消语义用）
}

func (f *fakeExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, int, error) {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, capturedCall{argv: append([]string(nil), argv...), env: env, stdin: stdin})
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return "", -1, ctx.Err()
	}
	if f.err != nil {
		return "", f.code, f.err
	}
	if idx < len(f.outputs) {
		return f.outputs[idx], f.code, nil
	}
	return "", f.code, nil
}

func newTestAdapter(exec Execer) *Adapter {
	return New(Config{McodePath: "/home/u/.nvm/versions/node/v24.14.1/bin/mcode", Execer: exec, Timeout: 30 * time.Second})
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("fixtures/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

// TestParseStreamFixtures：真实捕获的 stream-json（0.2.7）解析——agent_message 正文、
// sessionId、usage 各自就位；reasoning 不入 Messages（内部推理不外发）。
func TestParseStreamFixtures(t *testing.T) {
	p := ParseStream([]byte(readFixture(t, "exec-intent.jsonl")))
	if len(p.Messages) != 1 {
		t.Fatalf("intent fixture 应 1 条 agent_message，got %d", len(p.Messages))
	}
	data, err := agent.ExtractJSON(p.Messages[0])
	if err != nil {
		t.Fatalf("intent 正文应含 JSON: %v", err)
	}
	if data["action"] != "silent" {
		t.Fatalf("intent action = %v", data["action"])
	}
	if p.SessionID == "" || !strings.HasPrefix(p.SessionID, "mvs_") {
		t.Fatalf("应捕获 sessionId：%q", p.SessionID)
	}
	if p.Usage == nil || p.Usage.InputTokens <= 0 || p.Usage.Model != "minimax" {
		t.Fatalf("应捕获 turn.completed.usage：%+v", p.Usage)
	}

	g := ParseStream([]byte(readFixture(t, "exec-generate.jsonl")))
	if len(g.Messages) != 1 || !strings.Contains(g.Messages[0], "body") {
		t.Fatalf("generate fixture 解析不符：%+v", g)
	}
	if g.Err != "" {
		t.Fatalf("成功流不得有 Err：%q", g.Err)
	}
}

// TestParseStreamErrorStatus：exec.completed result.status 非 succeeded → Err（不虚构输出）。
func TestParseStreamErrorStatus(t *testing.T) {
	p := ParseStream([]byte(
		`{"type":"session.started","sessionId":"mvs_x"}` + "\n" +
			`{"type":"exec.completed","result":{"status":"failed"}}` + "\n"))
	if p.Err == "" {
		t.Fatal("status=failed 应产生 Err")
	}
	if len(p.Messages) != 0 {
		t.Fatalf("无 agent_message 不得虚构：%v", p.Messages)
	}
}

// TestIntentMapping：桩输出（真 fixture）映射 turn_intent 块；argv 契约与 stdin 提示词。
func TestIntentMapping(t *testing.T) {
	exec := &fakeExecer{outputs: []string{readFixture(t, "exec-intent.jsonl")}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "minimax"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t1", Kind: agent.KindEvaluateIntent})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Block != agent.BlockTurnIntent {
		t.Fatalf("block = %q", res.Block)
	}
	if err := agent.ValidateBlock(res.Block, res.Data); err != nil {
		t.Fatalf("端口级结构校验: %v", err)
	}
	// argv 契约：exec 无头 + --permission full（负责人裁定：无 auto 档选 full——
	// smart 下网络工具需交互宿主弹权限，无头 exec 结构性不可用）+ stream-json
	// + stdin 提示词；提示词含任务身份与指令
	argv := exec.calls[0].argv
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"exec\x00--permission\x00full\x00--output-format\x00stream-json", "--input\x00-"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv 契约不符（缺 %q）：%v", want, argv)
		}
	}
	if !strings.Contains(exec.calls[0].stdin, `"task_id":"t1"`) ||
		!strings.Contains(exec.calls[0].stdin, "Decide whether to reply") {
		t.Fatalf("stdin 应为完整提示词（指令+任务身份）：%q", exec.calls[0].stdin)
	}
	for _, a := range argv { // 精确元素匹配：--permission 含子串 "-p"，朴素 Contains 会误伤
		if a == "-p" {
			t.Fatal("提示词不得走 -p argv（kimi 护栏类问题）")
		}
	}
}

// TestResumeAndCwdArgv：首轮捕获 sessionId 后，后续任务以 --session 恢复；
// --cwd 与 --session 共存（实证）。
func TestResumeAndCwdArgv(t *testing.T) {
	exec := &fakeExecer{outputs: []string{readFixture(t, "exec-generate.jsonl"), readFixture(t, "exec-generate.jsonl")}}
	adapter := New(Config{
		McodePath: "/home/u/bin/mcode", WorkDir: "/home/u/.mosaic/agent-work/prof_m",
		Execer: exec, Timeout: 30 * time.Second,
	})
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "minimax"})
	defer sess.Close()

	run := func(id string) {
		t.Helper()
		h, _ := sess.Run(context.Background(), agent.Task{TaskID: id, Kind: agent.KindGenerate})
		if _, err := h.Result(); err != nil {
			t.Fatalf("result %s: %v", id, err)
		}
	}
	run("t1")
	first := exec.calls[0].argv
	if !strings.Contains(strings.Join(first, "\x00"), "--cwd\x00/home/u/.mosaic/agent-work/prof_m") {
		t.Fatalf("首轮应带 --cwd：%v", first)
	}
	run("t2")
	second := strings.Join(exec.calls[1].argv, "\x00")
	if !strings.Contains(second, "--session\x00mvs_") {
		t.Fatalf("次轮应以 --session 恢复（且保留 --cwd）：%v", exec.calls[1].argv)
	}
	if !strings.Contains(second, "--cwd") {
		t.Fatalf("--session 与 --cwd 应共存（实证）：%v", exec.calls[1].argv)
	}
}

// TestCancelStale：在途取消 → ErrStale（不发布正文）。
func TestCancelStale(t *testing.T) {
	exec := &fakeExecer{block: true}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "minimax"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t-cancel", Kind: agent.KindGenerate})
	h.Cancel()
	if _, err := h.Result(); !errors.Is(err, agent.ErrStale) {
		t.Fatalf("取消后应 ErrStale：%v", err)
	}
}

// TestWSLArgvShape：WSL 运行面 argv 包装（--exec 直 exec + env -i 清空；mcode 自带
// --cwd，无需 sh -c 包装）。--exec 是 v1.30 实证修复（`--` 拼接经默认 shell 解释毁参数）。
func TestWSLArgvShape(t *testing.T) {
	metacharPrompt := `Reply {"action":"speak|silent"}` // 走 stdin——argv 不含提示词
	got := wslArgs("Ubuntu", []string{"HOME=/home/u", "PATH=/x"}, []string{"/home/u/bin/mcode",
		"exec", "--permission", "full", "--output-format", "stream-json", "--input", "-", metacharPrompt})
	want := []string{
		"-d", "Ubuntu", "--exec", "env", "-i",
		"HOME=/home/u", "PATH=/x",
		"/home/u/bin/mcode", "exec", "--permission", "full", "--output-format", "stream-json", "--input", "-", metacharPrompt,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslArgs = %v\n期望 %v", got, want)
	}
}

// TestConformanceSuite：桩输出按任务类型回合法块——minimax 适配器过 conformance 全套
// 检查（RFC-0002 A-11 注册门禁；真机结构由 IT 验证）。路由标记必须是现行指令的
// 逐字子串（v1.69 教训：旧标记 "granted the floor" 已不在任何指令中，generate 检查
// 靠散文回退假绿——回退收紧后当场暴露；标记与指令不再漂移由
// TestPromptIsolatesArbitration 钉住）。
func TestConformanceSuite(t *testing.T) {
	execFn := func(prompt string) string {
		var data string
		switch {
		case strings.Contains(prompt, "Write your chat message"):
			data = `{"body":"[minimax-stub] draft body","declared_relations":[]}`
		case strings.Contains(prompt, "Summarize the discussion"):
			data = `{"summary":"[minimax-stub] summary","cited_event_ids":[]}`
		case strings.Contains(prompt, "has converged"):
			data = `{"action":"abstain","rationale":"stub"}`
		default:
			data = `{"action":"silent"}`
		}
		return `{"type":"session.started","sessionId":"mvs_stub"}` + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","content":` + mustJSON(t, data) + `}}` + "\n" +
			`{"type":"turn.completed","usage":{"inputTokens":12,"outputTokens":5}}` + "\n" +
			`{"type":"exec.completed","result":{"status":"succeeded"}}` + "\n"
	}
	conformance.Suite(t, New(Config{McodePath: "/x/mcode", Execer: &promptExecer{fn: execFn}}))
}

// promptExecer 按 stdin 提示词内容路由的桩（conformance 用——mcode 提示词走 stdin）。
type promptExecer struct {
	fn func(prompt string) string
}

func (p *promptExecer) Exec(_ context.Context, argv []string, _ []string, stdin string) (string, int, error) {
	_ = argv
	return p.fn(stdin), 0, nil
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// 评估降档（dogfood 性能治理）：评估任务 argv 带 --model <EvalModel>，生成任务不带。
func TestEvalModelArgsOnlyForEval(t *testing.T) {
	exec := &fakeExecer{outputs: []string{readFixture(t, "exec-intent.jsonl"), readFixture(t, "exec-generate.jsonl")}}
	adapter := New(Config{McodePath: "/bin/mcode", Execer: exec, Timeout: 30 * time.Second, EvalModel: "minimax/m2-fast"})
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
	if !hasArg(exec.calls[0].argv, "--model") || !hasArg(exec.calls[0].argv, "minimax/m2-fast") {
		t.Fatalf("评估任务应带降档模型：%v", exec.calls[0].argv)
	}
	if hasArg(exec.calls[1].argv, "--model") || hasArg(exec.calls[1].argv, "minimax/m2-fast") {
		t.Fatalf("生成任务不得带评估模型：%v", exec.calls[1].argv)
	}
}

func hasArg(argv []string, v string) bool {
	for _, a := range argv {
		if a == v {
			return true
		}
	}
	return false
}

// ---- v1.69：真机评估漂移矫正（2026-09-08 狗粮实证事故链） ----
// 事故：会话历史含强待办语境时，MiniMax 把仲裁元任务当作房间任务执行——评估提示词
// 下直接交付报告正文（CLI 转录思考链明确"不要 JSON 包装"），无 JSON 可提取，座位
// 整轮弃权；此前一轮 generate 位回了 silent 意图 JSON，被散文回退原样发布进房间
// 正文（房间投诉"仲裁 JSON 是内部件"即此）。

// agentMessageStream 单条 agent_message 的成功流（usage 固定 100/50，重试合计断言用）。
func agentMessageStream(t *testing.T, content string) string {
	t.Helper()
	return `{"type":"session.started","sessionId":"mvs_retry"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","content":` + mustJSON(t, content) + `}}` + "\n" +
		`{"type":"turn.completed","usage":{"inputTokens":100,"outputTokens":50}}` + "\n" +
		`{"type":"exec.completed","result":{"status":"succeeded"}}` + "\n"
}

// TestGenerateRejectsDecisionJSON：generate 位回决策 JSON（无 body）→ 拒绝发布，
// 不得把内部件冒充发言正文（旧散文回退会原样发布）；generate 非严格契约，无重试。
func TestGenerateRejectsDecisionJSON(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		agentMessageStream(t, `{"action":"silent","public_rationale":"已交付"}`),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "minimax"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	_, rerr := h.Result()
	if rerr == nil {
		t.Fatal("决策 JSON 误入 generate 位必须失败，不得发布")
	}
	if !strings.Contains(rerr.Error(), "缺可用 body") {
		t.Fatalf("错误应指明缺可用 body：%v", rerr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("generate 非严格契约不得重试：%d 次调用", len(exec.calls))
	}
}

// TestEvalCorrectiveRetry：评估输出漂移（散文正文，无 JSON）→ 一次矫正重试（同会话，
// 模型可见自己的跑偏输出）恢复合法意图；两跳真实消耗合计入账。
func TestEvalCorrectiveRetry(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		agentMessageStream(t, "会，重贴一遍，仲裁 JSON 不再带进来。"),
		agentMessageStream(t, `{"action":"silent","public_rationale":"等待原稿"}`),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "minimax"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, rerr := h.Result()
	if rerr != nil {
		t.Fatalf("矫正重试应恢复：%v", rerr)
	}
	if res.Block != agent.BlockTurnIntent {
		t.Fatalf("block = %q", res.Block)
	}
	if err := agent.ValidateBlock(res.Block, res.Data); err != nil {
		t.Fatalf("端口级结构校验: %v", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("应恰好一次矫正重试：%d 次调用", len(exec.calls))
	}
	if !strings.Contains(exec.calls[1].stdin, "CORRECTION") {
		t.Fatal("重试提示词应带矫正追记（原提示词 + CORRECTION 段）")
	}
	if !strings.Contains(strings.Join(exec.calls[1].argv, "\x00"), "--session\x00mvs_retry") {
		t.Fatalf("重试应同会话连续：%v", exec.calls[1].argv)
	}
	if res.Usage == nil || res.Usage.InputTokens != 200 || res.Usage.OutputTokens != 100 {
		t.Fatalf("两跳消耗应合计入账：%+v", res.Usage)
	}
}

// TestEvalRetryExhausted：两跳皆漂移 → 失败保留末次语义并附模型输出首行——
// 排障不再依赖 CLI 侧转录（本轮定位靠 mcode runtime-state 才拿到原文）。
func TestEvalRetryExhausted(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		agentMessageStream(t, "会，重贴一遍，仲裁 JSON 不再带进来。"),
		agentMessageStream(t, "还是决定直接贴报告。"),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "minimax"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	_, rerr := h.Result()
	if rerr == nil {
		t.Fatal("两跳皆漂移应失败")
	}
	if !strings.Contains(rerr.Error(), "还是决定直接贴报告") {
		t.Fatalf("错误应附模型输出首行：%v", rerr)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("重试上限一次：%d 次调用", len(exec.calls))
	}
}

// TestPromptIsolatesArbitration：评估/生成指令必须隔离元任务与房间任务（措辞与
// codex/kimi 同源——三适配器狗粮口径一致；conformance 桩路由标记同源于此）。
func TestPromptIsolatesArbitration(t *testing.T) {
	p, err := buildPrompt(agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent,
		Context: agent.Context{Inline: map[string]any{"k": "v"}}})
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if !strings.Contains(p, "internal arbitration request") {
		t.Fatal("评估指令应声明内部仲裁语义（不执行房间任务）")
	}
	g, err := buildPrompt(agent.Task{TaskID: "t", Kind: agent.KindGenerate,
		Context: agent.Context{Inline: map[string]any{"k": "v"}}})
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if !strings.Contains(g, "never an arbitration decision") {
		t.Fatal("生成指令应排除决策 JSON")
	}
}
