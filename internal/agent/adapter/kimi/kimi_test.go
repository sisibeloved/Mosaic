// UT 层：native-kimi 适配器——stream-json 解析（fixtures 钉 0.39.1）、argv 契约
// （-p 走 argv + -S resume）、发布门委托、提示词长度护栏、取消语义、
// conformance 套件（桩输出钉结构）。真机三件套见 kimi_it_test.go（IT 层）。
package kimi

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
	argv []string
	env  []string
	dir  string
}

type fakeExecer struct {
	mu      sync.Mutex
	calls   []capturedCall
	outputs []string // 按调用序返回预置 JSONL stdout
	code    int
	err     error
	block   bool // 阻塞至 ctx 取消（取消语义用）
}

func (f *fakeExecer) Exec(ctx context.Context, argv []string, env []string, dir string) (string, int, error) {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, capturedCall{argv: append([]string(nil), argv...), env: env, dir: dir})
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
	return New(Config{KimiPath: "/home/u/.kimi-code/bin/kimi", Execer: exec, Timeout: 30 * time.Second})
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("fixtures/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

// TestParseStreamFixtures：真实捕获的 stream-json（0.39.1）解析——assistant 文本与
// session_id（resume_hint）各自就位；噪声行跳过。
func TestParseStreamFixtures(t *testing.T) {
	p := ParseStream([]byte(readFixture(t, "stream-intent.jsonl")))
	if len(p.Messages) != 1 {
		t.Fatalf("intent fixture 应 1 条 assistant 消息，got %d", len(p.Messages))
	}
	data, err := agent.ExtractJSON(p.Messages[0])
	if err != nil {
		t.Fatalf("intent 正文应含 JSON: %v", err)
	}
	if data["action"] != "speak" {
		t.Fatalf("intent action = %v", data["action"])
	}
	if p.SessionID == "" || !strings.HasPrefix(p.SessionID, "session_") {
		t.Fatalf("应捕获 session_id：%q", p.SessionID)
	}

	g := ParseStream([]byte(readFixture(t, "stream-generate.jsonl")))
	if len(g.Messages) != 1 || !strings.Contains(g.Messages[0], "SQLite") {
		t.Fatalf("generate fixture 解析不符：%+v", g)
	}

	// 反例：无 assistant 行的合法流 → Messages 为空（不得虚构）
	none := ParseStream([]byte(readFixture(t, "stream-no-assistant.jsonl")))
	if len(none.Messages) != 0 {
		t.Fatalf("无 assistant 流应解析为空：%+v", none.Messages)
	}
}

// TestIntentMapping：桩输出映射 turn_intent 块（字段齐全，usage 为 nil——不虚构）。
func TestIntentMapping(t *testing.T) {
	exec := &fakeExecer{outputs: []string{readFixture(t, "stream-intent.jsonl")}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut", Adapter: "kimi"})
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
	if res.Usage != nil {
		t.Fatalf("kimi 无 usage 面，不得虚构：%+v", res.Usage)
	}
	// argv 契约：-p <prompt> --output-format stream-json；提示词含任务身份
	argv := exec.calls[0].argv
	joined := strings.Join(argv, "\x00")
	if !strings.Contains(joined, "-p") || !strings.Contains(joined, "--output-format\x00stream-json") {
		t.Fatalf("argv 契约不符：%v", argv)
	}
	if !strings.Contains(joined, `"task_id":"t1"`) {
		t.Fatalf("提示词应携带任务身份：%v", argv)
	}
}

// TestResumeArgv：首轮捕获 session_id 后，后续任务以 -S <id> 恢复（连续性）。
func TestResumeArgv(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		readFixture(t, "stream-intent.jsonl"),
		readFixture(t, "stream-generate.jsonl"),
	}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-ut2", Adapter: "kimi"})
	defer sess.Close()

	h1, _ := sess.Run(context.Background(), agent.Task{TaskID: "t1", Kind: agent.KindEvaluateIntent})
	if _, err := h1.Result(); err != nil {
		t.Fatalf("result1: %v", err)
	}
	h2, _ := sess.Run(context.Background(), agent.Task{
		TaskID: "t2", Kind: agent.KindGenerate,
		Grant: &agent.Grant{GrantID: "g1", Rank: 1, Epoch: 1},
	})
	res2, err := h2.Result()
	if err != nil {
		t.Fatalf("result2: %v", err)
	}
	if res2.Block != agent.BlockPublicDraft {
		t.Fatalf("block = %q", res2.Block)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("应 2 次调用：%d", len(exec.calls))
	}
	first := ParseStream([]byte(exec.outputs[0]))
	joined := strings.Join(exec.calls[1].argv, "\x00")
	if !strings.Contains(joined, "-S\x00"+first.SessionID) {
		t.Fatalf("第二次调用应携带 -S %s：%v", first.SessionID, exec.calls[1].argv)
	}
}

// TestGeneratePublishGate：发布门——秘密形状剔除 / 空正文拒发布 / 超限截断标注
// （与 codex 同一套 agent.PublishGate）。
func TestGeneratePublishGate(t *testing.T) {
	mkStream := func(text string) string {
		return `{"role":"meta","type":"system.version","version":"0.39.1"}` + "\n" +
			`{"role":"assistant","content":` + mustJSON(t, text) + `}` + "\n"
	}
	task := agent.Task{TaskID: "tg", Kind: agent.KindGenerate, Grant: &agent.Grant{GrantID: "g", Rank: 1, Epoch: 1}}

	t.Run("秘密剔除", func(t *testing.T) {
		exec := &fakeExecer{outputs: []string{mkStream(`正文 sk-abcdefghijklmnop1234 完`)}}
		sess, _ := newTestAdapter(exec).Boot(context.Background(), agent.Profile{ProfileID: "p1", Adapter: "kimi"})
		defer sess.Close()
		h, _ := sess.Run(context.Background(), task)
		res, err := h.Result()
		if err != nil {
			t.Fatalf("result: %v", err)
		}
		body, _ := res.Data["body"].(string)
		if !strings.Contains(body, "[REDACTED]") || strings.Contains(body, "sk-") {
			t.Fatalf("秘密应替换为 [REDACTED]：%q", body)
		}
	})

	t.Run("空正文拒发布", func(t *testing.T) {
		exec := &fakeExecer{outputs: []string{mkStream(`{"body":"   ","declared_relations":[]}`)}}
		sess, _ := newTestAdapter(exec).Boot(context.Background(), agent.Profile{ProfileID: "p2", Adapter: "kimi"})
		defer sess.Close()
		h, _ := sess.Run(context.Background(), task)
		if _, err := h.Result(); err == nil || !strings.Contains(err.Error(), "发布正文为空") {
			t.Fatalf("空正文应拒发布：%v", err)
		}
	})

	t.Run("超限截断标注", func(t *testing.T) {
		long := strings.Repeat("长", 5000)
		exec := &fakeExecer{outputs: []string{mkStream(long)}}
		sess, _ := newTestAdapter(exec).Boot(context.Background(), agent.Profile{ProfileID: "p3", Adapter: "kimi"})
		defer sess.Close()
		h, _ := sess.Run(context.Background(), task)
		res, err := h.Result()
		if err != nil {
			t.Fatalf("result: %v", err)
		}
		body, _ := res.Data["body"].(string)
		if !strings.Contains(body, "[Mosaic: 输出超限已截断]") || len([]rune(body)) > 4100 {
			t.Fatalf("超限应显式截断标注：%d runes", len([]rune(body)))
		}
	})

	t.Run("grant ResponseCap 约束发布", func(t *testing.T) {
		exec := &fakeExecer{outputs: []string{mkStream(strings.Repeat("字", 100))}}
		sess, _ := newTestAdapter(exec).Boot(context.Background(), agent.Profile{ProfileID: "p4", Adapter: "kimi"})
		defer sess.Close()
		h, _ := sess.Run(context.Background(), agent.Task{
			TaskID: "tg2", Kind: agent.KindGenerate,
			Grant: &agent.Grant{GrantID: "g2", Rank: 1, Epoch: 1, ResponseCap: 20},
		})
		res, err := h.Result()
		if err != nil {
			t.Fatalf("result: %v", err)
		}
		body, _ := res.Data["body"].(string)
		if runes := len([]rune(body)); runes > 60 || !strings.Contains(body, "截断") {
			t.Fatalf("grant ResponseCap=20 应约束发布：got %d runes", runes)
		}
	})
}

// TestPromptTooLargeGuard：提示词超 argv 安全上限 fail fast（kimi -p 不读 stdin，实证）。
func TestPromptTooLargeGuard(t *testing.T) {
	exec := &fakeExecer{}
	adapter := New(Config{KimiPath: "/x/kimi", Execer: exec, MaxPromptRunes: 16})
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p5", Adapter: "kimi"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t-big", Kind: agent.KindEvaluateIntent})
	if _, err := h.Result(); err == nil || !strings.Contains(err.Error(), "argv 安全上限") {
		t.Fatalf("超上限应明确报错：%v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("护栏应先于进程拉起：%d 次调用", len(exec.calls))
	}
}

// TestNoAssistantAndNonZeroExit：无 assistant 输出 / 非零退出均为任务失败（不虚构结果）。
func TestNoAssistantAndNonZeroExit(t *testing.T) {
	exec := &fakeExecer{outputs: []string{readFixture(t, "stream-no-assistant.jsonl")}}
	sess, _ := newTestAdapter(exec).Boot(context.Background(), agent.Profile{ProfileID: "p6", Adapter: "kimi"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t-na", Kind: agent.KindGenerate})
	if _, err := h.Result(); err == nil || !strings.Contains(err.Error(), "无 assistant 输出") {
		t.Fatalf("无 assistant 输出应失败：%v", err)
	}

	exec2 := &fakeExecer{outputs: []string{"Error: model not found\n"}, code: 1}
	sess2, _ := newTestAdapter(exec2).Boot(context.Background(), agent.Profile{ProfileID: "p7", Adapter: "kimi"})
	defer sess2.Close()
	h2, _ := sess2.Run(context.Background(), agent.Task{TaskID: "t-nz", Kind: agent.KindGenerate})
	if _, err := h2.Result(); err == nil || !strings.Contains(err.Error(), "退出码 1") {
		t.Fatalf("非零退出应失败：%v", err)
	}
}

// TestCancelBeforeResult：取消后 Result 返回 ErrStale（迟到拒绝语义）。
func TestCancelBeforeResult(t *testing.T) {
	exec := &fakeExecer{block: true}
	sess, _ := newTestAdapter(exec).Boot(context.Background(), agent.Profile{ProfileID: "p8", Adapter: "kimi"})
	defer sess.Close()
	h, _ := sess.Run(context.Background(), agent.Task{TaskID: "t-cancel", Kind: agent.KindGenerate})
	h.Cancel()
	if _, err := h.Result(); !errors.Is(err, agent.ErrStale) {
		t.Fatalf("取消后应 ErrStale：%v", err)
	}
}

// TestWSLArgvShape：WSL 运行面的 argv 包装（纯函数钉住：--exec 直 exec + env -i 清空
// + cd 工作目录）。--exec 是 2026-09-01 实证修复：`--` 剩余参数被 wsl.exe 拼接后交
// 发行版默认 shell 解释（引号剥除/元字符执行），kimi -p 提示词必毁（Kimi 每波评估
// 静默失败的根因）——含 | 与引号的提示词必须作为单一 argv 元素原样存活。
func TestWSLArgvShape(t *testing.T) {
	metacharPrompt := `Reply {"action":"speak|silent","body":"pong"} quote"pipe|`
	args := wslArgs("Ubuntu", []string{"HOME=/home/u", "PATH=/x:/bin"}, "/home/u/.mosaic/agent-work/prof_k", []string{"/home/u/.kimi-code/bin/kimi", "-p", metacharPrompt, "--output-format", "stream-json"})
	want := []string{
		"-d", "Ubuntu", "--exec", "env", "-i",
		"HOME=/home/u", "PATH=/x:/bin",
		"sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", "/home/u/.mosaic/agent-work/prof_k",
		"/home/u/.kimi-code/bin/kimi", "-p", metacharPrompt, "--output-format", "stream-json",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("wslArgs = %v\n期望 %v", args, want)
	}
}

// TestConformanceSuite：桩输出按任务类型回合法块——kimi 适配器过 conformance 全套检查
// （RFC-0002 A-11 注册门禁；真机结构由 IT 三件套验证）。
func TestConformanceSuite(t *testing.T) {
	// 按提示词内容识别任务类型（桩：模型面确定性代理）
	execFn := func(prompt string) string {
		var data string
		switch {
		case strings.Contains(prompt, "decide whether to speak"):
			data = `{"action":"speak","type":"extend","public_rationale":"stub intent","scores":{"relevance":0.5,"novelty":0.5,"urgency":0.5,"confidence":0.5}}`
		case strings.Contains(prompt, "granted the floor"):
			data = `{"body":"[kimi-stub] draft body","declared_relations":[]}`
		case strings.Contains(prompt, "Summarize the discussion"):
			data = `{"summary":"[kimi-stub] summary","cited_event_ids":[]}`
		case strings.Contains(prompt, "has converged"):
			data = `{"action":"abstain","rationale":"stub"}`
		default:
			data = `{"action":"silent"}`
		}
		return `{"role":"meta","type":"system.version","version":"0.39.1"}` + "\n" +
			`{"role":"assistant","content":` + mustJSON(t, data) + `}` + "\n" +
			`{"role":"meta","type":"session.resume_hint","session_id":"session_stub"}` + "\n"
	}
	conformance.Suite(t, New(Config{KimiPath: "/x/kimi", Execer: &promptExecer{fn: execFn}}))
}

// promptExecer 按提示词内容路由的桩（conformance 用）。
type promptExecer struct {
	fn func(prompt string) string
}

func (p *promptExecer) Exec(_ context.Context, argv []string, _ []string, _ string) (string, int, error) {
	prompt := ""
	for i, a := range argv {
		if a == "-p" && i+1 < len(argv) {
			prompt = argv[i+1]
		}
	}
	return p.fn(prompt), 0, nil
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
