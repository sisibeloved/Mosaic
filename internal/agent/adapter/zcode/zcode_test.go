// UT 层：zcode 适配器——stream-json 解析（fixtures 钉 0.16.9）、argv 契约（恒定 sh
// 包装 + 提示词不进 argv 走 stdin + --resume 恢复 + --cwd）、provider 配置 overlay
// 模型覆盖（zcode 无 --model flag 的对应实现）、发布门委托、取消语义、conformance
// 套件（桩输出钉结构）。真机三件套见 zcode_it_test.go（IT 层）。
package zcode

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
	stderr  string   // 退出码非零时的诊断通道
	code    int
	err     error
	block   bool // 阻塞至 ctx 取消（取消语义用）
}

func (f *fakeExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, string, int, error) {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, capturedCall{argv: append([]string(nil), argv...), env: env, stdin: stdin})
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	}
	if f.err != nil {
		return "", f.stderr, f.code, f.err
	}
	if idx < len(f.outputs) {
		return f.outputs[idx], f.stderr, f.code, nil
	}
	return "", f.stderr, f.code, nil
}

// routeExecer 按 argv 内容路由的桩（overlay 测试用：cat 读写请求与 zcode 执行分流）。
type routeExecer struct {
	mu      sync.Mutex
	calls   []capturedCall
	route   func(argv []string, stdin string) (stdout, stderr string, code int)
	written map[string]string // cat > 写入捕获（路径 → 内容）
}

func (r *routeExecer) Exec(_ context.Context, argv []string, env []string, stdin string) (string, string, int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, capturedCall{argv: append([]string(nil), argv...), env: env, stdin: stdin})
	r.mu.Unlock()
	stdout, stderr, code := r.route(argv, stdin)
	if strings.Contains(strings.Join(argv, "\x00"), "cat > ") {
		r.mu.Lock()
		if r.written == nil {
			r.written = map[string]string{}
		}
		r.written[argv[len(argv)-1]] = stdin
		r.mu.Unlock()
	}
	return stdout, stderr, code, nil
}

func newTestAdapter(exec Execer) *Adapter {
	return New(Config{ZcodePath: "/home/u/.zcode/bin/zcode", Execer: exec, Timeout: 30 * time.Second})
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("fixtures/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

// TestParseStreamFixtures：真实捕获的 stream-json（0.16.9）解析——sessionId 从任意
// 事件捕获、result.response 为最终正文、result.usage 就位、非 JSON 行忽略
// （exec-resume.jsonl 首行是 "ZCode Built-in skipped" 噪声行——由本测试钉住忽略语义）。
func TestParseStreamFixtures(t *testing.T) {
	p := ParseStream([]byte(readFixture(t, "exec-eval.jsonl")))
	if p.SessionID != "sess_64c81f53-52ad-45a7-9be6-decc9f61c928" {
		t.Fatalf("应捕获 sessionId：%q", p.SessionID)
	}
	if len(p.Messages) != 1 || p.Messages[0] != `{"scores":[{"seat":"a","score":0.5}],"verdict":"pass"}` {
		t.Fatalf("最终正文应取 result.response：%+v", p.Messages)
	}
	if p.Usage == nil || p.Usage.InputTokens != 23727 || p.Usage.OutputTokens != 94 || p.Usage.Model != "zcode" {
		t.Fatalf("应捕获 result.usage：%+v", p.Usage)
	}
	if p.Err != "" {
		t.Fatalf("成功流不得有 Err：%q", p.Err)
	}

	r := ParseStream([]byte(readFixture(t, "exec-resume.jsonl")))
	if r.SessionID != p.SessionID {
		t.Fatalf("--resume 捕获应与上轮同 sessionId：%q vs %q", r.SessionID, p.SessionID)
	}
	if len(r.Messages) != 1 || r.Usage == nil || r.Usage.InputTokens != 23854 || r.Usage.OutputTokens != 78 {
		t.Fatalf("resume fixture 解析不符：%+v", r)
	}
	if r.Err != "" {
		t.Fatalf("成功流不得有 Err：%q", r.Err)
	}
}

// TestParseStreamTurnFailed：turn.failed → Err（不虚构输出）；reasoning 流不入 Messages。
func TestParseStreamTurnFailed(t *testing.T) {
	p := ParseStream([]byte(
		`{"eventId":"e1","sessionId":"sess_x","seq":1,"timestamp":0,"traceId":"t","turnId":"turn_1","type":"turn.started","payload":{"turnNumber":0}}` + "\n" +
			`{"eventId":"e2","sessionId":"sess_x","seq":2,"timestamp":0,"traceId":"t","turnId":"turn_1","type":"model.streaming","payload":{"kind":"reasoning_delta","delta":"internal"}}` + "\n" +
			`{"eventId":"e3","sessionId":"sess_x","seq":3,"timestamp":0,"traceId":"t","turnId":"turn_1","type":"turn.failed","payload":{"error":{"message":"Model creation failed"}}}` + "\n"))
	if p.Err != "Model creation failed" {
		t.Fatalf("turn.failed 应产生 Err：%q", p.Err)
	}
	if len(p.Messages) != 0 {
		t.Fatalf("无 result 不得虚构正文：%v", p.Messages)
	}
	if p.SessionID != "sess_x" {
		t.Fatalf("sessionId 应从失败流同样捕获：%q", p.SessionID)
	}
}

// TestParseStreamResultMissingFallsBackToTurnCompleted：result 终止行缺失时正文兜底
// turn.completed payload.response（实证 schema 的双载体口径）。
func TestParseStreamResultMissingFallsBackToTurnCompleted(t *testing.T) {
	p := ParseStream([]byte(
		`{"eventId":"e1","sessionId":"sess_y","seq":1,"timestamp":0,"traceId":"t","turnId":"turn_1","type":"turn.completed","payload":{"response":"{\"action\":\"silent\"}","usage":{"inputTokens":7,"outputTokens":3}}}` + "\n"))
	if len(p.Messages) != 1 || p.Messages[0] != `{"action":"silent"}` {
		t.Fatalf("应兜底 turn.completed payload.response：%+v", p.Messages)
	}
	if p.Usage == nil || p.Usage.InputTokens != 7 || p.Usage.OutputTokens != 3 {
		t.Fatalf("应兜底 turn.completed usage：%+v", p.Usage)
	}
}

// TestIntentMapping：桩输出映射 turn_intent 块；argv 契约（恒定 sh 包装，提示词不进
// argv 走 stdin——zcode -p 只认 argv 同 kimi 面）与任务身份注入。
func TestIntentMapping(t *testing.T) {
	exec := &fakeExecer{outputs: []string{resultStream(t, `{"action":"silent","public_rationale":"等一手"}`)}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "zcode"})
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
	// argv 契约：恒定 sh 包装 + stream-json + --mode yolo（headless 默认 yolo 全自动
	// 审批，显式钉死）；提示词不进 argv、走 stdin（含任务身份与指令）
	argv := exec.calls[0].argv
	if len(argv) < 4 || argv[0] != "sh" || argv[1] != "-c" || argv[2] != `exec "$@" -p "$(cat)"` || argv[3] != "sh" {
		t.Fatalf("argv 应为恒定 sh 包装：%v", argv[:min(4, len(argv))])
	}
	joined := strings.Join(argv, "\x00")
	if !strings.Contains(joined, "zcode\x00--output-format\x00stream-json\x00--mode\x00yolo") {
		t.Fatalf("argv 契约不符（缺 stream-json --mode yolo）：%v", argv)
	}
	if strings.Contains(joined, "task_id") || strings.Contains(joined, "Decide whether to reply") {
		t.Fatalf("提示词不得进 argv（-p 只认 argv 由 sh $(cat) 代入）：%v", argv)
	}
	if !strings.Contains(exec.calls[0].stdin, `"task_id":"t1"`) ||
		!strings.Contains(exec.calls[0].stdin, "Decide whether to reply") {
		t.Fatalf("stdin 应为完整提示词（指令+任务身份）：%q", exec.calls[0].stdin)
	}
}

// TestResumeAndCwdArgv：首轮捕获 sessionId 后，后续任务以 --resume 恢复；
// --cwd 与 --resume 共存（实证）。
func TestResumeAndCwdArgv(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		resultStream(t, `{"body":"第一轮","declared_relations":[]}`),
		resultStream(t, `{"body":"第二轮","declared_relations":[]}`),
	}}
	adapter := New(Config{
		ZcodePath: "/home/u/.zcode/bin/zcode", WorkDir: "/home/u/.mosaic/agent-work/prof_z",
		Execer: exec, Timeout: 30 * time.Second,
	})
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "zcode"})
	defer sess.Close()

	run := func(id string) {
		t.Helper()
		h, _ := sess.Run(context.Background(), agent.Task{TaskID: id, Kind: agent.KindGenerate})
		if _, err := h.Result(); err != nil {
			t.Fatalf("result %s: %v", id, err)
		}
	}
	run("t1")
	first := strings.Join(exec.calls[0].argv, "\x00")
	if !strings.Contains(first, "--cwd\x00/home/u/.mosaic/agent-work/prof_z") {
		t.Fatalf("首轮应带 --cwd：%v", exec.calls[0].argv)
	}
	if strings.Contains(first, "--resume") {
		t.Fatalf("首轮无会话不得 --resume：%v", exec.calls[0].argv)
	}
	run("t2")
	second := strings.Join(exec.calls[1].argv, "\x00")
	if !strings.Contains(second, "--resume\x00sess_retry") {
		t.Fatalf("次轮应以 --resume 恢复（且保留 --cwd）：%v", exec.calls[1].argv)
	}
	if !strings.Contains(second, "--cwd") {
		t.Fatalf("--resume 与 --cwd 应共存（实证）：%v", exec.calls[1].argv)
	}
}

// TestCancelStale：在途取消 → ErrStale（不发布正文）。
func TestCancelStale(t *testing.T) {
	exec := &fakeExecer{block: true}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "zcode"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t-cancel", Kind: agent.KindGenerate})
	h.Cancel()
	if _, err := h.Result(); !errors.Is(err, agent.ErrStale) {
		t.Fatalf("取消后应 ErrStale：%v", err)
	}
}

// TestMaxPromptBytes：提示词超单参数物理上限 fail fast（Linux MAX_ARG_STRLEN 128KiB
// 边界——zcode -p 只认 argv，提示词经 sh $(cat) 代入后是单参数，kimi 同口径）。
func TestMaxPromptBytes(t *testing.T) {
	exec := &fakeExecer{}
	adapter := New(Config{ZcodePath: "/bin/zcode", Execer: exec, Timeout: 30 * time.Second, MaxPromptBytes: 64})
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "zcode"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t-big", Kind: agent.KindEvaluateIntent})
	if _, err := h.Result(); err == nil || !strings.Contains(err.Error(), "物理上限") {
		t.Fatalf("超上限应 fail fast：%v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("fail fast 不得发起 exec：%d 次调用", len(exec.calls))
	}
}

// TestWSLArgvShape：WSL 运行面 argv 包装（--exec 直 exec + env -i 清空；zcode 自带
// --cwd，sh $(cat) 包装在发行版内完成提示词代入）。--exec 是 v1.30 实证修复
// （`--` 拼接经默认 shell 解释毁参数）。
func TestWSLArgvShape(t *testing.T) {
	got := wslArgs("Ubuntu", []string{"HOME=/home/u", "PATH=/x"}, []string{"sh", "-c",
		`exec "$@" -p "$(cat)"`, "sh", "/home/u/.zcode/bin/zcode", "--output-format", "stream-json", "--mode", "yolo"})
	want := []string{
		"-d", "Ubuntu", "--exec", "env", "-i",
		"HOME=/home/u", "PATH=/x",
		"sh", "-c", `exec "$@" -p "$(cat)"`, "sh", "/home/u/.zcode/bin/zcode", "--output-format", "stream-json", "--mode", "yolo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslArgs = %v\n期望 %v", got, want)
	}
}

// TestConformanceSuite：桩输出按任务类型回合法块——zcode 适配器过 conformance 全套
// 检查（RFC-0002 A-11 注册门禁；真机结构由 IT 验证）。路由标记必须是现行指令的
// 逐字子串（v1.69 教训：标记与指令不再漂移由 TestPromptIsolatesArbitration 钉住）。
func TestConformanceSuite(t *testing.T) {
	execFn := func(prompt string) string {
		var data string
		switch {
		case strings.Contains(prompt, "Write your chat message"):
			data = `{"body":"[zcode-stub] draft body","declared_relations":[]}`
		case strings.Contains(prompt, "Summarize the discussion"):
			data = `{"summary":"[zcode-stub] summary","cited_event_ids":[]}`
		case strings.Contains(prompt, "has converged"):
			data = `{"action":"abstain","rationale":"stub"}`
		default:
			data = `{"action":"silent"}`
		}
		return `{"eventId":"e1","sessionId":"sess_stub","seq":1,"timestamp":0,"traceId":"t","turnId":"turn_stub","type":"turn.started","payload":{"turnNumber":0}}` + "\n" +
			`{"type":"result","sessionId":"sess_stub","response":` + mustJSON(t, data) + `,"usage":{"inputTokens":12,"outputTokens":5},"eventCount":1}` + "\n"
	}
	conformance.Suite(t, New(Config{ZcodePath: "/x/zcode", Execer: &promptExecer{fn: execFn}}))
}

// TestExitCodeErrorCarriesStreamReason：退出码非零时错误原因优先级——流内 turn.failed
// 优先于 stderr/stdout 首行（实证 2026-09-23：空 provider 文件时退出码 1、真因
// "Model creation failed" 在流内）。
func TestExitCodeErrorCarriesStreamReason(t *testing.T) {
	exec := &fakeExecer{
		code: 1,
		outputs: []string{
			`{"eventId":"e1","sessionId":"sess_f","seq":1,"timestamp":0,"traceId":"t","turnId":"turn_1","type":"turn.failed","payload":{"error":{"message":"Model creation failed"}}}` + "\n",
		},
		stderr: "some stderr noise",
	}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "zcode"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t1", Kind: agent.KindEvaluateIntent})
	if _, err := h.Result(); err == nil {
		t.Fatal("退出码 1 应产生错误")
	} else if msg := err.Error(); !strings.Contains(msg, "退出码 1") || !strings.Contains(msg, "Model creation failed") {
		t.Fatalf("错误应优先携带流内真因：%q", msg)
	}
}

// TestExitCodeErrorFallsBackToStderr：流内无 error 事件时退 stderr 首行
// （minimax 2026-09-23 登录过期实证同构——真因只在 stderr 的形态）。
func TestExitCodeErrorFallsBackToStderr(t *testing.T) {
	exec := &fakeExecer{code: 3, stderr: "zcode: authentication required, run `zcode` to sign in"}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "zcode"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t1", Kind: agent.KindEvaluateIntent})
	if _, err := h.Result(); err == nil {
		t.Fatal("退出码 3 应产生错误")
	} else if msg := err.Error(); !strings.Contains(msg, "退出码 3") || !strings.Contains(msg, "authentication required") {
		t.Fatalf("错误应携带 stderr 真因：%q", msg)
	}
}

// promptExecer 按 stdin 提示词内容路由的桩（conformance 用——提示词走 stdin 经
// sh $(cat) 代入 zcode argv，桩面即 stdin）。
type promptExecer struct {
	fn func(prompt string) string
}

func (p *promptExecer) Exec(_ context.Context, argv []string, _ []string, stdin string) (string, string, int, error) {
	_ = argv
	return p.fn(stdin), "", 0, nil
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// resultStream 单条 result 终止行的成功流（usage 固定 100/50，重试合计断言用；
// sessionId sess_retry 供 --resume 断言）。
func resultStream(t *testing.T, response string) string {
	t.Helper()
	return `{"eventId":"e1","sessionId":"sess_retry","seq":1,"timestamp":0,"traceId":"t","turnId":"turn_1","type":"turn.started","payload":{"turnNumber":0}}` + "\n" +
		`{"type":"result","sessionId":"sess_retry","response":` + mustJSON(t, response) + `,"usage":{"inputTokens":100,"outputTokens":50},"eventCount":1}` + "\n"
}

// TestGenerateRejectsDecisionJSON：generate 位回决策 JSON（无 body）→ 拒绝发布，
// 不得把内部件冒充发言正文（旧散文回退会原样发布）；generate 非严格契约，无重试。
func TestGenerateRejectsDecisionJSON(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		resultStream(t, `{"action":"silent","public_rationale":"已交付"}`),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
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
// 模型可见自己的跑偏输出）恢复合法意图；两跳真实消耗合计入账（minimax v1.69 先例同构）。
func TestEvalCorrectiveRetry(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		resultStream(t, "会，重贴一遍，仲裁 JSON 不再带进来。"),
		resultStream(t, `{"action":"silent","public_rationale":"等待原稿"}`),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
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
	if !strings.Contains(strings.Join(exec.calls[1].argv, "\x00"), "--resume\x00sess_retry") {
		t.Fatalf("重试应同会话连续：%v", exec.calls[1].argv)
	}
	if res.Usage == nil || res.Usage.InputTokens != 200 || res.Usage.OutputTokens != 100 {
		t.Fatalf("两跳消耗应合计入账：%+v", res.Usage)
	}
}

// TestEvalRetryExhausted：两跳皆漂移 → 失败保留末次语义并附模型输出首行——
// 排障不再依赖 CLI 侧转录。
func TestEvalRetryExhausted(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		resultStream(t, "会，重贴一遍，仲裁 JSON 不再带进来。"),
		resultStream(t, "还是决定直接贴报告。"),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
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

// v1.70：记忆评审任务（提示词/映射/能力）+ generate 位 history_query 两段式出口。
func TestMemoryReviewAndHistoryQuery(t *testing.T) {
	// memory_review：评审指令 + 清单入 Stimulus；产物映射 memory_ops（空批合法）
	stream := resultStream(t, `{"ops":[{"action":"add","content":"用户偏好简短回复"}],"public_rationale":"沉淀偏好"}`)
	exec := &fakeExecer{outputs: []string{stream}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindReviewMemory,
		Context: agent.Context{Inline: map[string]any{"memory_inventory": []any{}}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, rerr := h.Result()
	if rerr != nil {
		t.Fatalf("result: %v", rerr)
	}
	if res.Block != agent.BlockMemoryOps {
		t.Fatalf("block = %q", res.Block)
	}
	if err := agent.ValidateBlock(res.Block, res.Data); err != nil {
		t.Fatalf("端口级结构校验: %v", err)
	}
	if !strings.Contains(exec.calls[0].stdin, "memory curator") {
		t.Fatal("评审提示词应含策展指令")
	}
	if !adapter.Capabilities().MemoryCuration {
		t.Fatal("能力声明应含 MemoryCuration")
	}

	// generate 位回 history_query（无 body）→ history_request 块（引擎两段式入口）
	qstream := resultStream(t, `{"history_query":"早期选型结论"}`)
	exec2 := &fakeExecer{outputs: []string{qstream}}
	sess2, _ := newTestAdapter(exec2).Boot(context.Background(), agent.Profile{ProfileID: "p2", Adapter: "zcode"})
	defer sess2.Close()
	h2, _ := sess2.Run(context.Background(), agent.Task{TaskID: "t2", Kind: agent.KindGenerate})
	res2, err2 := h2.Result()
	if err2 != nil {
		t.Fatalf("history_query 不是错误: %v", err2)
	}
	if res2.Block != agent.BlockHistoryRequest || res2.Data["history_query"] != "早期选型结论" {
		t.Fatalf("history_request 块不符: %+v", res2)
	}
}

// TestGeneratePromptMentionsHistoryQuery：生成指令含两段式检索出口说明。
func TestGeneratePromptMentionsHistoryQuery(t *testing.T) {
	p, err := buildPrompt(agent.Task{TaskID: "t", Kind: agent.KindGenerate,
		Context: agent.Context{Inline: map[string]any{"k": "v"}}})
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if !strings.Contains(p, "history_query") {
		t.Fatal("生成指令应说明 history_query 出口")
	}
}

// TestPromptIsolatesArbitration：评估/生成指令必须隔离元任务与房间任务（措辞与
// codex/kimi/minimax 同源——四适配器狗粮口径一致；conformance 桩路由标记同源于此）。
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

// ---- 模型覆盖：provider 配置 overlay（zcode 无 --model flag 的对应实现）----

// overlayBaseJSON 合成基座 provider_config.json（schemaVersion 1 实证形状，无秘密）：
// p1 双模型 enabled；p2 单模型但 enabled=false；p3 只在 modelConfigRules 出现。
const overlayBaseJSON = `{
  "schemaVersion": 1,
  "config": {
    "providerConfigRules": {
      "providerRules": [
        {"providerId": "p1", "providerName": "Z.ai", "config": {"group": "standard-personal", "personalModelIds": ["glm-4.5", "glm-4.6"], "modelOrder": ["glm-4.5", "glm-4.6"]}},
        {"providerId": "p2", "providerName": "Kimi", "enabled": false, "config": {"personalModelIds": ["k3"], "modelOrder": ["k3"]}}
      ]
    },
    "modelConfigRules": {
      "providerModelRules": [
        {"modelId": "glm-4.6", "providerId": "p1"},
        {"modelId": "k3", "providerId": "p2"},
        {"modelId": "deepseek-v4-pro", "providerId": "p3"}
      ],
      "manualProviderModelRules": []
    }
  }
}`

// newOverlayExecer cat 读写与 zcode 执行分流的路由桩：读回 overlayBaseJSON（或按
// readCode 模拟基座不可读），写捕获进 written，执行按提示词路由回合法块
// （generate 回 body 形、其余回 silent 意图——防矫正重试改变调用序断言）。
func newOverlayExecer(t *testing.T, readCode int) *routeExecer {
	t.Helper()
	return &routeExecer{route: func(argv []string, stdin string) (string, string, int) {
		joined := strings.Join(argv, "\x00")
		switch {
		case strings.Contains(joined, `cat "$1"`):
			if readCode != 0 {
				return "", "cat: No such file or directory", readCode
			}
			return overlayBaseJSON, "", 0
		case strings.Contains(joined, "cat > "):
			return "", "", 0
		default:
			if strings.Contains(stdin, "Write your chat message") {
				return resultStream(t, `{"body":"overlay 路由正文","declared_relations":[]}`), "", 0
			}
			return resultStream(t, `{"action":"silent"}`), "", 0
		}
	}}
}

func newOverlayAdapter(exec Execer, model string) *Adapter {
	return New(Config{
		ZcodePath: "/home/u/.zcode/bin/zcode", Execer: exec, Timeout: 30 * time.Second,
		WSLDistro: "Ubuntu", WSLHome: "/home/u", // HOME 钉死：overlay 路径断言确定
		Model: model,
	})
}

// TestOverlayReorderAndEnv：模型覆盖全链——基座读取 → 目标 provider 的
// personalModelIds/modelOrder 重排目标在最前 + defaultModelSelection（兼容 3.14.3）
// → overlay 写基座同目录 provider_config.mosaic.<model>.json → exec env 携
// ZCODE_PERSONAL_PROVIDER_CONFIG_FILE（0.16.9 实证重定向生效）。
func TestOverlayReorderAndEnv(t *testing.T) {
	exec := newOverlayExecer(t, 0)
	adapter := newOverlayAdapter(exec, "glm-4.6")
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := h.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("overlay 全链应为 读/写/执行 三次调用：%d", len(exec.calls))
	}
	// overlay 内容：目标模型排到最前 + defaultModelSelection
	overlayPath := "/home/u/.zcode/v2/provider_config.mosaic.glm-4.6.json"
	written, ok := exec.written[overlayPath]
	if !ok {
		t.Fatalf("应写出 overlay %s（实有：%v）", overlayPath, keysOf(exec.written))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(written), &doc); err != nil {
		t.Fatalf("overlay 应为合法 JSON: %v", err)
	}
	config := doc["config"].(map[string]any)
	sel := config["defaultModelSelection"].(map[string]any)
	if sel["providerId"] != "p1" || sel["modelId"] != "glm-4.6" {
		t.Fatalf("defaultModelSelection 不符：%+v", sel)
	}
	rule := config["providerConfigRules"].(map[string]any)["providerRules"].([]any)[0].(map[string]any)
	rcfg := rule["config"].(map[string]any)
	for _, key := range []string{"personalModelIds", "modelOrder"} {
		ids := rcfg[key].([]any)
		if ids[0] != "glm-4.6" || len(ids) != 2 {
			t.Fatalf("%s 应重排目标在最前：%v", key, ids)
		}
	}
	// exec 调用 env 携重定向变量；cat 读写调用不带（仅执行面需要）
	execCall := exec.calls[2]
	found := false
	for _, kv := range execCall.env {
		if kv == "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE="+overlayPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("exec env 应携 ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=%s：%v", overlayPath, execCall.env)
	}
	if strings.Contains(strings.Join(execCall.argv, "\x00"), "glm-4.6") {
		t.Fatalf("模型覆盖不走 argv（无 --model flag）：%v", execCall.argv)
	}
}

// TestOverlayDisabledProviderFallback：目标模型所在 provider enabled=false 时主路径
// 跳过，回退 modelConfigRules.providerModelRules 按 modelId 找 providerId（回退面
// 不过滤 enabled——显式覆盖是用户意图，生效与否由 CLI 侧判定）。
func TestOverlayDisabledProviderFallback(t *testing.T) {
	exec := newOverlayExecer(t, 0)
	adapter := newOverlayAdapter(exec, "k3")
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if _, err := h.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	written := exec.written["/home/u/.zcode/v2/provider_config.mosaic.k3.json"]
	var doc map[string]any
	if err := json.Unmarshal([]byte(written), &doc); err != nil {
		t.Fatalf("overlay 应为合法 JSON: %v", err)
	}
	sel := doc["config"].(map[string]any)["defaultModelSelection"].(map[string]any)
	if sel["providerId"] != "p2" || sel["modelId"] != "k3" {
		t.Fatalf("回退路径应命中 p2/k3：%+v", sel)
	}
}

// TestOverlayModelNotFound：模型不在任何 provider 的模型清单 → 任务报错（不静默
// 回落 CLI 默认——显式覆盖必须生效或显式失败）；modelConfigRules 匹配到 providerId
// 但该 provider 规则不存在同样报错。
func TestOverlayModelNotFound(t *testing.T) {
	for _, model := range []string{"nonexistent-model", "deepseek-v4-pro"} {
		exec := newOverlayExecer(t, 0)
		adapter := newOverlayAdapter(exec, model)
		sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
		h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
		_, err := h.Result()
		if err == nil || !strings.Contains(err.Error(), "不在 zcode 任何 provider 的模型清单") {
			t.Fatalf("模型 %s 不在清单应报错：%v", model, err)
		}
		if len(exec.calls) != 1 { // 只有基座读取，不写不执行
			t.Fatalf("模型找不到不得写 overlay/执行：%d 次调用", len(exec.calls))
		}
		sess.Close()
	}
}

// TestOverlayBaseUnreadable：基座 provider_config.json 读不到 → 任务报错带路径。
func TestOverlayBaseUnreadable(t *testing.T) {
	exec := newOverlayExecer(t, 1)
	adapter := newOverlayAdapter(exec, "glm-4.6")
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	_, err := h.Result()
	if err == nil {
		t.Fatal("基座读不到应报错")
	}
	if !strings.Contains(err.Error(), "/home/u/.zcode/v2/provider_config.json") {
		t.Fatalf("错误应带基座路径：%v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("基座读不到不得写 overlay/执行：%d 次调用", len(exec.calls))
	}
}

// TestNoOverlayWithoutModel：无模型覆盖 → 零 overlay IO（无 cat 调用）、env 无重定向
// 变量——CLI 默认（首个 enabled provider 的 modelOrder[0]）不受打扰。
func TestNoOverlayWithoutModel(t *testing.T) {
	exec := newOverlayExecer(t, 0)
	adapter := New(Config{ZcodePath: "/bin/zcode", Execer: exec, Timeout: 30 * time.Second})
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if _, err := h.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("无覆盖应只有执行调用：%d", len(exec.calls))
	}
	for _, kv := range exec.calls[0].env {
		if strings.HasPrefix(kv, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=") {
			t.Fatalf("无覆盖不得设重定向变量：%v", exec.calls[0].env)
		}
	}
}

// TestSanitizeModelName：overlay 文件名的模型名安全化（非 [a-zA-Z0-9._-] 换 _）。
func TestSanitizeModelName(t *testing.T) {
	for in, want := range map[string]string{
		"glm-4.6":      "glm-4.6",
		"zai/glm 4.6":  "zai_glm_4.6",
		"a/b\\c:d;e":   "a_b_c_d_e",
		"k3-256k":      "k3-256k",
		"claude_4.5-x": "claude_4.5-x",
	} {
		if got := sanitizeModelName(in); got != want {
			t.Fatalf("sanitizeModelName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
