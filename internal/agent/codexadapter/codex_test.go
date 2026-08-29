// UT 层：native-codex 适配器（切片 E 本体）——
// 事件流解析（真实 fixtures）、JSON 提取、任务→提示词→结果映射、
// 会话 resume 连续性、取消语义、PATH 注入。
// TDD：本文件先行于实现（红→绿）。fixtures 为 2026-08-29 真机 codex 0.149.1 捕获。
package codexadapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// ---- 解析器 ----

func TestParseSimpleStream(t *testing.T) {
	parsed, err := ParseStream(loadFixture(t, "exec-simple.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.ThreadID != "01a04b2c-4142-78b3-8954-67b581ebd0eb" {
		t.Fatalf("thread id = %s", parsed.ThreadID)
	}
	if len(parsed.Messages) != 1 || parsed.Messages[0] != "pong" {
		t.Fatalf("messages = %v", parsed.Messages)
	}
	if parsed.Usage == nil || parsed.Usage.InputTokens != 21290 || parsed.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", parsed.Usage)
	}
	if parsed.Err != "" {
		t.Fatalf("不应有错误：%s", parsed.Err)
	}
}

func TestParseIntentStream(t *testing.T) {
	parsed, err := ParseStream(loadFixture(t, "exec-intent.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data, err := ExtractJSON(parsed.Messages[0])
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, field := range []string{"action", "type", "public_rationale", "scores"} {
		if _, ok := data[field]; !ok {
			t.Fatalf("turn_intent 缺字段 %q", field)
		}
	}
	if parsed.Usage == nil || parsed.Usage.OutputTokens != 113 {
		t.Fatalf("usage = %+v", parsed.Usage)
	}
}

func TestParseErrorStream(t *testing.T) {
	parsed, err := ParseStream(loadFixture(t, "exec-error.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Err == "" {
		t.Fatal("错误流必须置 Err")
	}
	if !strings.Contains(parsed.Err, "invalid_request_error") {
		t.Fatalf("Err 应保留上游信息：%s", parsed.Err)
	}
}

func TestParseIgnoresNonJSONLines(t *testing.T) {
	stream := "noise line\n" + `{"type":"thread.started","thread_id":"t1"}` + "\nwarn: something\n"
	parsed, err := ParseStream([]byte(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.ThreadID != "t1" {
		t.Fatalf("thread = %s", parsed.ThreadID)
	}
}

// ---- JSON 提取 ----

func TestExtractJSONVariants(t *testing.T) {
	cases := map[string]string{
		"裸 JSON": `{"a":1}`,
		"围栏":     "```json\n{\"a\":1}\n```",
		"前后有散文":  `Here you go: {"a":1} hope it helps`,
		"嵌套":     `{"scores":{"relevance":0.5},"action":"speak"}`,
	}
	for name, text := range cases {
		data, err := ExtractJSON(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := data["a"].(float64); !ok {
			if _, ok2 := data["action"]; !ok2 {
				t.Fatalf("%s: 提取结果不符 %v", name, data)
			}
		}
	}
	if _, err := ExtractJSON("no json here at all"); err == nil {
		t.Fatal("无 JSON 必须报错")
	}
}

// ---- Execer 捕获装置 ----

type capturedCall struct {
	argv  []string
	env   []string
	stdin string
}

type fakeExecer struct {
	mu    sync.Mutex
	calls []capturedCall
	// 按调用序返回预置输出
	outputs []string // 每次调用的 JSONL stdout
}

func (f *fakeExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := len(f.calls)
	f.calls = append(f.calls, capturedCall{argv: append([]string(nil), argv...), env: env, stdin: stdin})
	if idx < len(f.outputs) {
		return f.outputs[idx], 0, nil
	}
	return "", 0, nil
}

func newTestAdapter(exec Execer) *Adapter {
	return New(Config{CodexPath: "/nvm/bin/codex", Execer: exec, Timeout: 30 * time.Second})
}

func TestSessionRunUsesExecThenResume(t *testing.T) {
	first := `{"type":"thread.started","thread_id":"thr_1"}` + "\n" +
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"{\"action\":\"silent\"}"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`
	second := `{"type":"thread.started","thread_id":"thr_1"}` + "\n" +
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"{\"body\":\"draft\"}"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":3}}`
	exec := &fakeExecer{outputs: []string{first, second}}
	adapter := newTestAdapter(exec)

	ctx := context.Background()
	session, err := adapter.Boot(ctx, agent.Profile{ProfileID: "p1", Adapter: "codex"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer session.Close()

	// 第一次：codex exec（无 resume）
	intentTask := agent.Task{TaskID: "t1", Kind: agent.KindEvaluateIntent, ParticipantID: "par_c", RoomID: "r"}
	h1, err := session.Run(ctx, intentTask)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	res1, err := h1.Result()
	if err != nil {
		t.Fatalf("result1: %v", err)
	}
	if res1.Block != "turn_intent" || res1.Data["action"] != "silent" {
		t.Fatalf("intent 映射不符：%+v", res1)
	}
	if res1.Usage == nil || res1.Usage.InputTokens != 10 {
		t.Fatalf("usage 丢失：%+v", res1.Usage)
	}
	if got := exec.calls[0].argv; got[0] != "/nvm/bin/codex" || got[1] != "exec" || contains(got, "resume") {
		t.Fatalf("首次应 exec 不 resume：%v", got[:3])
	}
	// PATH 注入：可执行目录前置（nvm 布局教训）
	if !envHasPATHPrefix(exec.calls[0].env, "/nvm/bin") {
		t.Fatalf("env PATH 未前置可执行目录：%v", exec.calls[0].env)
	}

	// 第二次：resume thr_1（会话连续性，RFC-0002 逻辑会话）
	genTask := agent.Task{TaskID: "t2", Kind: agent.KindGenerate, ParticipantID: "par_c", RoomID: "r"}
	h2, err := session.Run(ctx, genTask)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	res2, err := h2.Result()
	if err != nil {
		t.Fatalf("result2: %v", err)
	}
	if res2.Block != "public_draft" {
		t.Fatalf("generate 映射不符：%+v", res2)
	}
	argv := exec.calls[1].argv
	if argv[1] != "exec" || argv[2] != "resume" || !contains(argv, "thr_1") {
		t.Fatalf("第二次应 resume thr_1：%v", argv[:4])
	}
}

func TestGeneratePlainFallback(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"thr_x"}` + "\n" +
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"纯文本回答，不是 JSON"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":5}}`
	exec := &fakeExecer{outputs: []string{stream}}
	adapter := newTestAdapter(exec)
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "codex"})
	defer session.Close()
	h, err := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Block != "public_draft" || res.Data["body"] != "纯文本回答，不是 JSON" {
		t.Fatalf("纯文本应回退 body 映射：%+v", res)
	}
}

func TestCancelYieldsStale(t *testing.T) {
	// 阻塞型 Execer：等到 ctx 取消
	blocking := &blockingExecer{}
	adapter := newTestAdapter(blocking)
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "codex"})
	defer session.Close()
	h, err := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	h.Cancel()
	if _, err := h.Result(); err != agent.ErrStale {
		t.Fatalf("取消后应 ErrStale，got %v", err)
	}
}

type blockingExecer struct{}

func (b *blockingExecer) Exec(ctx context.Context, argv, env []string, stdin string) (string, int, error) {
	<-ctx.Done() // 模拟长任务被杀
	return "", -1, ctx.Err()
}

func TestUnknownKindRejected(t *testing.T) {
	adapter := newTestAdapter(&fakeExecer{})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "codex"})
	defer session.Close()
	if _, err := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindObserve}); err == nil {
		t.Fatal("codex 不支持 observe（Capabilities.Observe=false），必须拒绝")
	}
}

func TestErrorStreamSurfacesFailure(t *testing.T) {
	exec := &fakeExecer{outputs: []string{string(loadFixture(t, "exec-error.jsonl"))}}
	adapter := newTestAdapter(exec)
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "codex"})
	defer session.Close()
	h, err := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := h.Result(); err == nil {
		t.Fatal("错误流必须导致任务失败（不虚构结果）")
	}
}

func TestPromptContainsStimulusAndSchemaInstruction(t *testing.T) {
	exec := &fakeExecer{outputs: []string{
		`{"type":"thread.started","thread_id":"t"}` + "\n" +
			`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"{\"action\":\"silent\"}"}}` + "\n" +
			`{"type":"turn.completed","usage":{}}`,
	}}
	adapter := newTestAdapter(exec)
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "codex"})
	defer session.Close()
	stimulus := map[string]any{"body": "讨论刺激：选 SQLite 还是 PG"}
	task := agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent, Context: agent.Context{Inline: stimulus}}
	h, err := session.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := h.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	// 提示词经 stdin（避免 argv 转义问题），含刺激内容与 JSON 指令
	stdin := exec.calls[0].stdin
	if !strings.Contains(stdin, "选 SQLite 还是 PG") {
		t.Fatalf("提示词缺刺激内容：%q", stdin)
	}
	if !strings.Contains(stdin, "JSON") {
		t.Fatalf("提示词缺 JSON 输出指令：%q", stdin)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func envHasPATHPrefix(env []string, dir string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") && strings.HasPrefix(strings.TrimPrefix(kv, "PATH="), dir) {
			return true
		}
	}
	return false
}

var _ = json.Marshal
