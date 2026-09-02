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
	// argv 契约：exec 无头 + stream-json + stdin 提示词；提示词含任务身份与指令
	argv := exec.calls[0].argv
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"exec\x00--output-format\x00stream-json", "--input\x00-"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv 契约不符（缺 %q）：%v", want, argv)
		}
	}
	if !strings.Contains(exec.calls[0].stdin, `"task_id":"t1"`) ||
		!strings.Contains(exec.calls[0].stdin, "Decide whether to reply") {
		t.Fatalf("stdin 应为完整提示词（指令+任务身份）：%q", exec.calls[0].stdin)
	}
	if strings.Contains(joined, "-p") {
		t.Fatal("提示词不得走 -p argv（kimi 护栏类问题）")
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

// TestPublishCapEnforced：grant 宣告的 ResponseCap 必须真实约束发布正文。
func TestPublishCapEnforced(t *testing.T) {
	long := strings.Repeat("字", 3000)
	payload := `{"sessionId":"mvs_cap","type":"item.completed","item":{"type":"agent_message","content":"` +
		`{\"body\":\"` + long + `\",\"declared_relations\":[]}"}}` + "\n" +
		`{"type":"turn.completed","usage":{"inputTokens":10,"outputTokens":5}}` + "\n"
	exec := &fakeExecer{outputs: []string{payload}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "minimax"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{
		TaskID: "t-cap", Kind: agent.KindGenerate,
		Grant: &agent.Grant{GrantID: "g1", ResponseCap: 50},
	})
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	body, _ := res.Data["body"].(string)
	// 截断 + 截断标注（"\n[Mosaic: 输出超限已截断]"）——与 codex 面同口径
	if n := len([]rune(body)); n > 50+len("\n[Mosaic: 输出超限已截断]") {
		t.Fatalf("发布正文 %d runes 超过宣告 cap 50+标注", n)
	}
	if res.Usage == nil || res.Usage.OutputTokens != 5 {
		t.Fatalf("usage 应透传：%+v", res.Usage)
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
		"exec", "--output-format", "stream-json", "--input", "-", metacharPrompt})
	want := []string{
		"-d", "Ubuntu", "--exec", "env", "-i",
		"HOME=/home/u", "PATH=/x",
		"/home/u/bin/mcode", "exec", "--output-format", "stream-json", "--input", "-", metacharPrompt,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslArgs = %v\n期望 %v", got, want)
	}
}

// TestConformanceSuite：桩输出按任务类型回合法块——minimax 适配器过 conformance 全套
// 检查（RFC-0002 A-11 注册门禁；真机结构由 IT 验证）。路由标记与 kimi 桩同源
// （charter/summarize/closure 指令内），intent 缺省走 silent（合法块）。
func TestConformanceSuite(t *testing.T) {
	execFn := func(prompt string) string {
		var data string
		switch {
		case strings.Contains(prompt, "granted the floor"):
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
