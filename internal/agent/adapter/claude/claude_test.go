// UT 层：claude 适配器——stream-json 解析（fixtures 钉 2.1.172 鉴权失败真机捕获；
// 成功形状以文档 schema 内联钉住，登录恢复后补真机捕获）、argv 契约（stdin 提示词
// + 恒定 --verbose（实证必需）+ --dangerously-skip-permissions + --resume 恢复 +
// --model 覆盖 + 工作目录经 Execer dir）、零退出码但 result.is_error 的成功判定
// 陷阱、矫正重试、发布门委托、取消语义、conformance 套件（桩输出钉结构）。
// 真机三件套见 claude_it_test.go（IT 层）。
package claude

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
	dir   string
	stdin string
}

type fakeExecer struct {
	mu      sync.Mutex
	calls   []capturedCall
	outputs []string // 按调用序返回预置 JSONL stdout
	stderr  string   // 诊断通道
	code    int
	err     error
	block   bool // 阻塞至 ctx 取消（取消语义用）
}

func (f *fakeExecer) Exec(ctx context.Context, argv []string, env []string, dir string, stdin string) (string, string, int, error) {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, capturedCall{argv: append([]string(nil), argv...), env: env, dir: dir, stdin: stdin})
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

// promptExecer 按 stdin 提示词内容路由的桩（conformance 用——提示词走 stdin，
// -p 一等通道，桩面即 stdin）。
type promptExecer struct {
	fn func(prompt string) string
}

func (p *promptExecer) Exec(_ context.Context, argv []string, _ []string, _ string, stdin string) (string, string, int, error) {
	_ = argv
	return p.fn(stdin), "", 0, nil
}

func newTestAdapter(exec Execer) *Adapter {
	return New(Config{ClaudePath: "/home/u/.local/bin/claude", Execer: exec, Timeout: 30 * time.Second})
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("fixtures/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// resultStream 成功形态最小流（文档 schema；session_id sess_retry 供 --resume 断言，
// usage 固定 100/50 供重试合计断言）——成功态真机捕获待登录恢复后补进 fixtures。
func resultStream(t *testing.T, response string) string {
	t.Helper()
	return `{"type":"system","subtype":"init","cwd":"/tmp","session_id":"sess_retry","model":"claude-stub","tools":["Bash"]}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` + mustJSON(t, response) + `}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":` + mustJSON(t, response) + `,"session_id":"sess_retry","total_cost_usd":0.001,"usage":{"input_tokens":100,"output_tokens":50}}` + "\n"
}

// TestParseStreamAuthFailFixture：真机捕获（2.1.172，鉴权失败全程）——session_id
// 自 system/init 捕获、result.is_error 置 Err（错误正文取 result 字段）、usage
// 如实透传（失败轮全零）、失败 assistant 转录不冒充正文、hook/api_retry 无关事件
// 跳过、非 JSON 噪声行忽略。
func TestParseStreamAuthFailFixture(t *testing.T) {
	p := ParseStream([]byte(readFixture(t, "exec-authfail.jsonl")))
	if p.SessionID != "d1af77e4-4946-41a3-837e-b6ec655d5eb7" {
		t.Fatalf("应自 init 捕获 session_id：%q", p.SessionID)
	}
	if !strings.Contains(p.Err, "Failed to authenticate") || !strings.Contains(p.Err, "401") {
		t.Fatalf("is_error result 应置 Err（错误正文）：%q", p.Err)
	}
	if len(p.Messages) != 0 {
		t.Fatalf("is_error 流不得产出正文（assistant 失败转录不冒充）：%v", p.Messages)
	}
	if p.Usage == nil || p.Usage.InputTokens != 0 || p.Usage.OutputTokens != 0 || p.Usage.Model != "claude" {
		t.Fatalf("失败轮 usage 如实透传（全零）：%+v", p.Usage)
	}
}

// TestParseStreamSuccess：成功流（文档 schema 内联）——result.result 为最终正文、
// usage 非零、无 Err、session_id 捕获、无关事件跳过。
func TestParseStreamSuccess(t *testing.T) {
	p := ParseStream([]byte(resultStream(t, `{"action":"silent","public_rationale":"等一手"}`)))
	if p.Err != "" {
		t.Fatalf("成功流不得有 Err：%q", p.Err)
	}
	if len(p.Messages) != 1 || p.Messages[0] != `{"action":"silent","public_rationale":"等一手"}` {
		t.Fatalf("最终正文应取 result.result：%+v", p.Messages)
	}
	if p.SessionID != "sess_retry" {
		t.Fatalf("session_id 捕获不符：%q", p.SessionID)
	}
	if p.Usage == nil || p.Usage.InputTokens != 100 || p.Usage.OutputTokens != 50 {
		t.Fatalf("应捕获 result.usage：%+v", p.Usage)
	}
}

// TestParseStreamResultMissingFallsBackToAssistant：result 终止事件缺失时正文兜底
// assistant 文本块（转录面），多文本块按出现序拼接。
func TestParseStreamResultMissingFallsBackToAssistant(t *testing.T) {
	p := ParseStream([]byte(
		`{"type":"system","subtype":"init","session_id":"uuid-x"}` + "\n" +
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"第一块"},{"type":"thinking","text":"内部推理不入正文"},{"type":"text","text":"第二块"}]}}` + "\n"))
	if len(p.Messages) != 2 || p.Messages[0] != "第一块" || p.Messages[1] != "第二块" {
		t.Fatalf("应兜底 assistant 文本块（非 text 块跳过）：%+v", p.Messages)
	}
}

// TestParseStreamIsErrorWithoutResultText：is_error 而 result 无正文——Err 给出
// subtype 而非空串（不吞错）。
func TestParseStreamIsErrorWithoutResultText(t *testing.T) {
	p := ParseStream([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"uuid-y"}` + "\n"))
	if !strings.Contains(p.Err, "error_max_turns") {
		t.Fatalf("is_error 无正文应携 subtype：%q", p.Err)
	}
}

// TestIntentMapping：桩输出映射 turn_intent 块；argv 契约（提示词不进 argv 走
// stdin——-p 一等通道；恒定 --verbose（2.1.172 实证 stream-json 必需）+
// --dangerously-skip-permissions + stream-json）与任务身份注入。
func TestIntentMapping(t *testing.T) {
	exec := &fakeExecer{outputs: []string{resultStream(t, `{"action":"silent","public_rationale":"等一手"}`)}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "claude"})
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
	// argv 契约：可执行直起（无 sh 包装——stdin 即提示词通道）+ stream-json +
	// --verbose + YOLO 旗标；提示词不进 argv。
	call := exec.calls[0]
	argv := call.argv
	if argv[0] != "/home/u/.local/bin/claude" {
		t.Fatalf("argv[0] 应为可执行直起：%v", argv)
	}
	joined := strings.Join(argv, "\x00")
	if !strings.Contains(joined, "-p\x00--output-format\x00stream-json\x00--verbose\x00--dangerously-skip-permissions") {
		t.Fatalf("argv 契约不符（缺 -p stream-json --verbose --dangerously-skip-permissions）：%v", argv)
	}
	if strings.Contains(joined, "task_id") || strings.Contains(joined, "Decide whether to reply") {
		t.Fatalf("提示词不得进 argv（stdin 才是提示词通道）：%v", argv)
	}
	if !strings.Contains(call.stdin, `"task_id":"t1"`) ||
		!strings.Contains(call.stdin, "Decide whether to reply") {
		t.Fatalf("stdin 应为完整提示词（指令+任务身份）：%q", call.stdin)
	}
}

// TestResumeAndDirArgv：首轮捕获 session_id 后，后续任务以 --resume 恢复；
// 工作目录经 Execer dir 通道传递（claude 无 --cwd 等价物——进程 cwd 即会话工作区）。
func TestResumeAndDirArgv(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		resultStream(t, `{"body":"第一轮","declared_relations":[]}`),
		resultStream(t, `{"body":"第二轮","declared_relations":[]}`),
	}}
	adapter := New(Config{
		ClaudePath: "/home/u/.local/bin/claude", WorkDir: "/home/u/.mosaic/agent-work/prof_c",
		Execer: exec, Timeout: 30 * time.Second,
	})
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "claude"})
	defer sess.Close()

	run := func(id string) {
		t.Helper()
		h, _ := sess.Run(context.Background(), agent.Task{TaskID: id, Kind: agent.KindGenerate})
		if _, err := h.Result(); err != nil {
			t.Fatalf("result %s: %v", id, err)
		}
	}
	run("t1")
	if exec.calls[0].dir != "/home/u/.mosaic/agent-work/prof_c" {
		t.Fatalf("工作目录应经 Execer dir 传递：%q", exec.calls[0].dir)
	}
	if strings.Contains(strings.Join(exec.calls[0].argv, "\x00"), "--resume") {
		t.Fatalf("首轮无会话不得 --resume：%v", exec.calls[0].argv)
	}
	run("t2")
	if !strings.Contains(strings.Join(exec.calls[1].argv, "\x00"), "--resume\x00sess_retry") {
		t.Fatalf("次轮应以 --resume 恢复：%v", exec.calls[1].argv)
	}
}

// TestModelOverrideArgv：--model 原生 flag（对照 zcode 的 provider overlay）；
// 评估任务 EvalModel 优先于 Model（评估降档不被主模型覆盖吞掉）；未配置不出现 --model。
func TestModelOverrideArgv(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		resultStream(t, `{"action":"silent"}`),
		resultStream(t, `{"body":"生成正文","declared_relations":[]}`),
		resultStream(t, `{"action":"silent"}`),
	}}
	adapter := New(Config{ClaudePath: "/home/u/.local/bin/claude", Execer: exec,
		Timeout: 30 * time.Second, Model: "opus", EvalModel: "haiku"})
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "claude"})
	defer sess.Close()
	run := func(kind agent.TaskKind) []string {
		t.Helper()
		h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: kind})
		if _, err := h.Result(); err != nil {
			t.Fatalf("result: %v", err)
		}
		return exec.calls[len(exec.calls)-1].argv
	}
	if got := run(agent.KindEvaluateIntent); !containsPair(got, "--model", "haiku") {
		t.Fatalf("评估任务应取 EvalModel：%v", got)
	}
	if got := run(agent.KindGenerate); !containsPair(got, "--model", "opus") {
		t.Fatalf("生成任务应取 Model：%v", got)
	}
	bare := &fakeExecer{outputs: []string{resultStream(t, `{"action":"silent"}`)}}
	sess2, _ := newTestAdapter(bare).Boot(context.Background(), agent.Profile{ProfileID: "p2", Adapter: "claude"})
	defer sess2.Close()
	h2, _ := sess2.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	if _, err := h2.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	if strings.Contains(strings.Join(bare.calls[0].argv, "\x00"), "--model") {
		t.Fatalf("未配置模型不得出现 --model（尊重 CLI 默认）：%v", bare.calls[0].argv)
	}
}

func containsPair(argv []string, flag, val string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == val {
			return true
		}
	}
	return false
}

// TestExitCodeZeroButIsError：零退出码 + result.is_error（实证 2.1.172 鉴权失败
// 形态）→ 任务失败且错误携 result 正文——退出码不可信，成功判定必须查 is_error。
func TestExitCodeZeroButIsError(t *testing.T) {
	exec := &fakeExecer{outputs: []string{readFixture(t, "exec-authfail.jsonl")}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "claude"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	_, rerr := h.Result()
	if rerr == nil {
		t.Fatal("零退出码但 is_error 必须任务失败，不得发布")
	}
	if !strings.Contains(rerr.Error(), "Failed to authenticate") {
		t.Fatalf("错误应携 result 错误正文：%v", rerr)
	}
	// 矫正重试不适用于 is_error（供应商态非输出形状问题）：不得二次调用。
	if len(exec.calls) != 1 {
		t.Fatalf("is_error 不触发矫正重试：%d 次调用", len(exec.calls))
	}
}

// TestExitCodeErrorCarriesStderr：非零退出码且流内无错误 → stderr 首行（诊断通道）。
func TestExitCodeErrorCarriesStderr(t *testing.T) {
	exec := &fakeExecer{code: 2, outputs: []string{""}, stderr: "Error: When using --print, --output-format=stream-json requires --verbose"}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "claude"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if _, err := h.Result(); err == nil || !strings.Contains(err.Error(), "requires --verbose") {
		t.Fatalf("退出码错误应携 stderr 真因：%v", err)
	}
}

// TestNoOutputCarriesStderrDiag：零退出码且无流输出（用法错误形态，如空 stdin）
// ——错误携带 stderr 首行诊断（"Error: Input must be provided…" 实证）。
func TestNoOutputCarriesStderrDiag(t *testing.T) {
	exec := &fakeExecer{outputs: []string{""}, stderr: "Error: Input must be provided either through stdin or as a prompt argument when using --print"}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "claude"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if _, err := h.Result(); err == nil || !strings.Contains(err.Error(), "Input must be provided") {
		t.Fatalf("无输出错误应携 stderr 诊断：%v", err)
	}
}

// TestCancelStale：在途取消 → ErrStale（不发布正文）。
func TestCancelStale(t *testing.T) {
	exec := &fakeExecer{block: true}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "claude"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t-cancel", Kind: agent.KindGenerate})
	h.Cancel()
	if _, err := h.Result(); !errors.Is(err, agent.ErrStale) {
		t.Fatalf("取消后应 ErrStale：%v", err)
	}
}

// TestWSLArgvShape：WSL 运行面 argv 包装（--exec 直 exec + env -i 清空 + sh cd
// 包装——claude 无 --cwd 等价物，kimi 同构）。
func TestWSLArgvShape(t *testing.T) {
	got := wslArgs("Ubuntu", []string{"HOME=/home/u", "PATH=/x"}, "/home/u/.mosaic/agent-work/prof_c",
		[]string{"/home/u/.local/bin/claude", "-p", "--output-format", "stream-json", "--verbose"})
	want := []string{
		"-d", "Ubuntu", "--exec", "env", "-i",
		"HOME=/home/u", "PATH=/x",
		"sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", "/home/u/.mosaic/agent-work/prof_c",
		"/home/u/.local/bin/claude", "-p", "--output-format", "stream-json", "--verbose",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslArgs = %v\n期望 %v", got, want)
	}
	if got2 := wslArgs("Ubuntu", nil, "", []string{"/bin/claude"}); got2[len(got2)-1] != "/bin/claude" || containsPair(got2, "sh", "-c") {
		t.Fatalf("空工作目录不得 cd 包装：%v", got2)
	}
}

// TestEnvShape：POSIX 面 HOME 锚定 + PATH 前置可执行目录；Windows 面
// HOME/USERPROFILE 双锚（Node os.homedir 走 USERPROFILE 定位 %USERPROFILE%\.claude
// 凭证——zcode 桌面面同构）。凭据类键（ANTHROPIC_API_KEY）不在白名单。
func TestEnvShape(t *testing.T) {
	posix := claudeEnvPOSIX("/home/u/.local/bin", "/home/u")
	if !containsExact(posix, "HOME=/home/u") {
		t.Fatalf("POSIX env 应含 HOME：%v", posix)
	}
	if !hasPrefixKey(posix, "PATH=") || !strings.Contains(keyOf(posix, "PATH"), "/home/u/.local/bin") {
		t.Fatalf("POSIX PATH 应前置可执行目录：%v", posix)
	}
	win := claudeEnvWindows(`C:\Users\u\.local\bin`, `C:\Users\u`)
	if !containsExact(win, "USERPROFILE=C:\\Users\\u") || !containsExact(win, "HOME=C:\\Users\\u") {
		t.Fatalf("Windows env 应双锚 HOME/USERPROFILE：%v", win)
	}
	if !hasPrefixKey(win, "PATH=") || !strings.Contains(keyOf(win, "PATH"), `C:\Users\u\.local\bin`) {
		t.Fatalf("Windows PATH 应前置可执行目录：%v", win)
	}
	for _, env := range [][]string{posix, win} {
		if hasPrefixKey(env, "ANTHROPIC_API_KEY=") || hasPrefixKey(env, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatalf("凭据类键不得透传（OQ-20）：%v", env)
		}
	}
}

func containsExact(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func hasPrefixKey(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func keyOf(env []string, prefix string) string {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e
		}
	}
	return ""
}

// TestGenerateRejectsDecisionJSON：generate 位回决策 JSON（无 body）→ 拒绝发布，
// 不得把内部件冒充发言正文；generate 非严格契约，无重试。
func TestGenerateRejectsDecisionJSON(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		resultStream(t, `{"action":"silent","public_rationale":"已交付"}`),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "claude"})
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
		resultStream(t, "会，重贴一遍，仲裁 JSON 不再带进来。"),
		resultStream(t, `{"action":"silent","public_rationale":"这次只出 JSON"}`),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "claude"})
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
	if len(exec.calls) != 2 {
		t.Fatalf("恰一次矫正重试：%d 次调用", len(exec.calls))
	}
	if !strings.Contains(exec.calls[1].stdin, "CORRECTION") {
		t.Fatal("重试提示词应携矫正追记")
	}
	if res.Usage == nil || res.Usage.InputTokens != 200 || res.Usage.OutputTokens != 100 {
		t.Fatalf("两跳 usage 应合计（100+100 / 50+50）：%+v", res.Usage)
	}
}

// TestMemoryReviewAndHistoryQuery：memory_review 指令与 memory_ops 映射（空批合法）；
// generate 位回 history_query → history_request 块（引擎两段式入口）。
func TestMemoryReviewAndHistoryQuery(t *testing.T) {
	stream := resultStream(t, `{"ops":[{"action":"add","content":"用户偏好简短回复"}],"public_rationale":"沉淀偏好"}`)
	exec := &fakeExecer{outputs: []string{stream}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "claude"})
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

	qstream := resultStream(t, `{"history_query":"早期选型结论"}`)
	exec2 := &fakeExecer{outputs: []string{qstream}}
	sess2, _ := newTestAdapter(exec2).Boot(context.Background(), agent.Profile{ProfileID: "p2", Adapter: "claude"})
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

// TestPromptIsolatesArbitration：评估/生成指令必须隔离元任务与房间任务（措辞与
// codex/kimi/minimax/zcode 同源——五适配器狗粮口径一致；conformance 桩路由标记同源于此）。
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
	if !strings.Contains(g, "history_query") {
		t.Fatal("生成指令应说明 history_query 出口")
	}
}

// TestConformanceSuite：桩输出按任务类型回合法块——claude 适配器过 conformance 全套
// 检查（RFC-0002 A-11 注册门禁；真机结构由 IT 验证）。路由标记必须是现行指令的
// 逐字子串（v1.69 教训：标记与指令不再漂移由 TestPromptIsolatesArbitration 钉住）。
func TestConformanceSuite(t *testing.T) {
	execFn := func(prompt string) string {
		var data string
		switch {
		case strings.Contains(prompt, "Write your chat message"):
			data = `{"body":"[claude-stub] draft body","declared_relations":[]}`
		case strings.Contains(prompt, "Summarize the discussion"):
			data = `{"summary":"[claude-stub] summary","cited_event_ids":[]}`
		case strings.Contains(prompt, "has converged"):
			data = `{"action":"abstain","rationale":"stub"}`
		default:
			data = `{"action":"silent"}`
		}
		return `{"type":"system","subtype":"init","session_id":"uuid-stub","tools":[]}` + "\n" +
			`{"type":"result","subtype":"success","is_error":false,"result":` + mustJSON(t, data) + `,"session_id":"uuid-stub","usage":{"input_tokens":12,"output_tokens":5}}` + "\n"
	}
	conformance.Suite(t, New(Config{ClaudePath: "/x/claude", Execer: &promptExecer{fn: execFn}}))
}
