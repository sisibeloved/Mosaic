// Package codexadapter：native-codex 适配器（RFC-0002 原生适配首批）。
// 对接 codex exec --json（JSONL 事件流；实证 codex-cli 0.149.1，事件 schema 无稳定性
// 承诺——fixtures 钉版本，漂移由 conformance 测试暴露）。
// 已知缺口（实证 2026-08-29）：--output-schema 在当前 provider 组合下上游 400
// （text.format.schema 序列化 bug），结构化输出走提示词约束 + 本地提取校验。
package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// Config 适配器配置。
type Config struct {
	CodexPath string        // 可执行路径（来自 harness 注册表）
	WorkDir   string        // 工作根（默认临时目录；不给仓库权限）
	ExtraArgs []string      // 预留（-c model=... 等 per-Profile 覆盖）
	Timeout   time.Duration // 单任务超时（默认 120s）
	Execer    Execer        // 测试注入；nil 用真实进程执行
}

// Execer 进程执行抽象（UT 捕获/阻塞；生产为真实 codex 子进程）。
type Execer interface {
	Exec(ctx context.Context, argv []string, env []string, stdin string) (stdout string, exitCode int, err error)
}

// Adapter 实现 agent.Adapter。
type Adapter struct {
	cfg Config
}

// New 构造。
func New(cfg Config) *Adapter {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	return &Adapter{cfg: cfg}
}

// Name 适配器名。
func (a *Adapter) Name() string { return "codex" }

// Capabilities 能力声明（RFC-0002 §3.1.2）。
func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false, // exec 无流式（--json 事件流非增量草稿）
		CancelMode:     "interrupt",
		HistoryChannel: "structured_request",
		Continuity:     true, // resume <thread_id>
		UsageReporting: true, // turn.completed.usage
		Observe:        false,
	}
}

// Boot 建立逻辑会话（无进程：codex exec 按任务拉起，会话身份 = thread_id）。
func (a *Adapter) Boot(_ context.Context, _ agent.Profile) (agent.Session, error) {
	return &session{adapter: a}, nil
}

// session 逻辑会话：持有 thread_id 实现跨任务连续性（RFC-0002 §3.2）。
type session struct {
	adapter *Adapter
	mu      sync.Mutex
	thread  string
}

func (s *session) Run(ctx context.Context, task agent.Task) (agent.Handle, error) {
	if task.Kind == agent.KindObserve {
		return nil, fmt.Errorf("codexadapter: 不支持 observe（Capabilities.Observe=false）")
	}
	h := &handle{done: make(chan struct{})}
	// ctx 与 cancel 同步接线：Cancel() 必须能立即杀掉在途任务（端口取消契约）
	taskCtx, cancel := context.WithTimeout(ctx, s.adapter.cfg.Timeout)
	h.cancel = cancel
	go s.execute(taskCtx, task, h)
	return h, nil
}

func (s *session) Cancel(string) {}
func (s *session) Close()        {}

// execute 单任务执行：构建提示词 → codex exec [--json]（resume 连续性）→ 解析 → 映射。
func (s *session) execute(taskCtx context.Context, task agent.Task, h *handle) {
	defer close(h.done)

	prompt, err := buildPrompt(task)
	if err != nil {
		h.err = err
		return
	}

	s.mu.Lock()
	thread := s.thread
	s.mu.Unlock()

	argv := []string{s.adapter.cfg.CodexPath, "exec", "--json", "--skip-git-repo-check", "-s", "read-only"}
	argv = append(argv, s.adapter.cfg.ExtraArgs...)
	if thread != "" {
		// 连续性：resume <thread_id>（子命令不接受 -s，沙箱继承）
		argv = []string{s.adapter.cfg.CodexPath, "exec", "resume", "--json", "--skip-git-repo-check", thread}
	}
	if s.adapter.cfg.WorkDir != "" {
		argv = append(argv, "-C", s.adapter.cfg.WorkDir)
	}
	argv = append(argv, "-") // 提示词走 stdin：避免 argv 转义与长度问题

	stdout, _, err := s.execer().Exec(taskCtx, argv, codexEnv(s.adapter.cfg.CodexPath), prompt)
	if err != nil {
		if taskCtx.Err() != nil {
			h.stale = true // 取消/超时：不发布正文，语义同迟到拒绝
			return
		}
		h.err = fmt.Errorf("codexadapter: exec: %w", err)
		return
	}
	parsed, err := ParseStream([]byte(stdout))
	if err != nil {
		h.err = fmt.Errorf("codexadapter: parse: %w", err)
		return
	}
	if parsed.Err != "" {
		h.err = fmt.Errorf("codexadapter: turn failed: %s", parsed.Err)
		return
	}
	if parsed.ThreadID != "" && parsed.ThreadID != thread {
		s.mu.Lock()
		s.thread = parsed.ThreadID
		s.mu.Unlock()
	}
	if len(parsed.Messages) == 0 {
		h.err = fmt.Errorf("codexadapter: 无 agent_message 输出")
		return
	}
	h.result, h.err = mapResult(task.Kind, parsed)
}

func (s *session) execer() Execer {
	if s.adapter.cfg.Execer != nil {
		return s.adapter.cfg.Execer
	}
	return &processExecer{}
}

// handle 单任务句柄：同步等待结果；Cancel 置位后 Result 报 ErrStale。
type handle struct {
	done   chan struct{}
	once   sync.Once
	cancel context.CancelFunc

	mu     sync.Mutex
	stale  bool
	result agent.Result
	err    error
}

func (h *handle) Updates() <-chan agent.DraftUpdate {
	ch := make(chan agent.DraftUpdate)
	close(ch) // 无草稿流能力（Capabilities.Streaming=false）
	return ch
}

func (h *handle) Cancel() {
	h.mu.Lock()
	h.stale = true
	h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
}

func (h *handle) Result() (agent.Result, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stale {
		return agent.Result{}, agent.ErrStale
	}
	return h.result, h.err
}

// ---- 事件流解析（fixtures 钉 schema）----

// Parsed 一次 exec 的解析结果。
type Parsed struct {
	ThreadID string
	Messages []string // agent_message 文本（按完成序）
	Usage    *agent.Usage
	Err      string // error / turn.failed 的信息
}

type streamEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Message string `json:"message"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ParseStream 解析 codex exec --json 的 JSONL 输出；非 JSON 行忽略（stderr 混入等噪声）。
func ParseStream(raw []byte) (Parsed, error) {
	var out Parsed
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // 未知/噪声行
		}
		switch ev.Type {
		case "thread.started":
			out.ThreadID = ev.ThreadID
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				out.Messages = append(out.Messages, ev.Item.Text)
			}
		case "turn.completed":
			if ev.Usage != nil {
				out.Usage = &agent.Usage{
					InputTokens:  ev.Usage.InputTokens,
					OutputTokens: ev.Usage.OutputTokens,
					Model:        "codex",
				}
			}
		case "error":
			out.Err = ev.Message
		case "turn.failed":
			if out.Err == "" && ev.Error != nil {
				out.Err = ev.Error.Message
			}
		}
	}
	return out, nil
}

// ExtractJSON 从模型文本提取 JSON 对象：容忍围栏与前后散文；无 JSON 报错。
func ExtractJSON(text string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	if s := stripFence(text); s != "" {
		text = s
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("codexadapter: 文本中无 JSON 对象")
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text[start:end+1]), &data); err != nil {
		return nil, fmt.Errorf("codexadapter: JSON 解析失败: %w", err)
	}
	return data, nil
}

func stripFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return ""
	}
	lines := strings.Split(text, "\n")
	var body []string
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "```" {
			continue
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}

// ---- 提示词与结果映射 ----

const intentInstruction = `You are a participant in a group discussion. Given the stimulus below, decide whether to speak.
Reply with ONLY a JSON object, no prose, no code fences:
{"action":"speak|react|fork|summarize|silent","type":"answer|extend|challenge|support|question|redirect|synthesize","public_rationale":"<=280 chars","scores":{"relevance":0.0-1.0,"novelty":0.0-1.0,"urgency":0.0-1.0,"confidence":0.0-1.0}}`

const generateInstruction = `You are a participant in a group discussion and have been granted the floor.
Respond to the discussion below.
Reply with ONLY a JSON object, no prose, no code fences:
{"body":"your public message","declared_relations":[]}`

const summarizeInstruction = `Summarize the discussion below faithfully.
Reply with ONLY a JSON object: {"summary":"...","cited_event_ids":["..."]}`

const closureInstruction = `Judge whether the discussion below has converged.
Reply with ONLY a JSON object: {"action":"conclude|object|abstain","rationale":"..."}`

func buildPrompt(task agent.Task) (string, error) {
	stimulus, _ := json.Marshal(task.Context.Inline)
	switch task.Kind {
	case agent.KindEvaluateIntent:
		return intentInstruction + "\n\nStimulus: " + string(stimulus), nil
	case agent.KindGenerate:
		return generateInstruction + "\n\nDiscussion: " + string(stimulus), nil
	case agent.KindSummarize:
		return summarizeInstruction + "\n\nDiscussion: " + string(stimulus), nil
	case agent.KindEvaluateClosure:
		return closureInstruction + "\n\nDiscussion: " + string(stimulus), nil
	default:
		return "", fmt.Errorf("codexadapter: 未知任务类型 %q", task.Kind)
	}
}

// mapResult 任务类型 → 端口结果块（结构校验失败即任务失败，不虚构）。
func mapResult(kind agent.TaskKind, parsed Parsed) (agent.Result, error) {
	text := parsed.Messages[len(parsed.Messages)-1]
	switch kind {
	case agent.KindEvaluateIntent:
		data, err := ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		action, _ := data["action"].(string)
		if action == "" {
			return agent.Result{}, fmt.Errorf("codexadapter: turn_intent 缺字段 %q", "action")
		}
		// RFC-0003 §3.1.2：silent Intent 其余字段可选
		if action != "silent" {
			for _, field := range []string{"type", "scores"} {
				if _, ok := data[field]; !ok {
					return agent.Result{}, fmt.Errorf("codexadapter: turn_intent 缺字段 %q", field)
				}
			}
		}
		return agent.Result{Block: "turn_intent", Data: data, Usage: parsed.Usage}, nil
	case agent.KindGenerate:
		if data, err := ExtractJSON(text); err == nil {
			if body, ok := data["body"].(string); ok && body != "" {
				if data["declared_relations"] == nil {
					data["declared_relations"] = []any{}
				}
				return agent.Result{Block: "public_draft", Data: data, Usage: parsed.Usage}, nil
			}
		}
		// 纯文本回退：正文即发言
		return agent.Result{
			Block: "public_draft",
			Data:  map[string]any{"body": text, "declared_relations": []any{}},
			Usage: parsed.Usage,
		}, nil
	case agent.KindSummarize:
		if data, err := ExtractJSON(text); err == nil {
			if data["cited_event_ids"] == nil {
				data["cited_event_ids"] = []any{}
			}
			return agent.Result{Block: "grounded_summary", Data: data, Usage: parsed.Usage}, nil
		}
		return agent.Result{
			Block: "grounded_summary",
			Data:  map[string]any{"summary": text, "cited_event_ids": []any{}},
			Usage: parsed.Usage,
		}, nil
	case agent.KindEvaluateClosure:
		data, err := ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		return agent.Result{Block: "closure_intent", Data: data, Usage: parsed.Usage}, nil
	default:
		return agent.Result{}, fmt.Errorf("codexadapter: 未知任务类型 %q", kind)
	}
}

// codexEnv 构造子进程环境：PATH 前置可执行目录（nvm 布局：node 与 CLI 同目录）；
// 代理/CA 等网络配置从宿主透传（实证：本机经 127.0.0.1:7890 出网，剥掉即连不上 API；
// 这些是网络配置而非凭据——OQ-20 禁的是持有凭证与代理流量）；其余不透传（不携带
// Mosaic 自身密钥）。
func codexEnv(codexPath string) []string {
	dir := codexPath
	if i := strings.LastIndex(codexPath, "/"); i > 0 {
		dir = codexPath[:i]
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	env := []string{
		"PATH=" + dir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
		"CODEX_HOME=" + home + "/.codex", // 登录态所在（实证）
	}
	for _, key := range []string{
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
	} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

type processExecer struct{}

// Exec 返回 stdout+stderr 合并流：
// codex 的诊断信息（如 login status）可能走 stderr（实证），ParseStream 忽略非 JSON 行。
func (p *processExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader([]byte(stdin))
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	// 卡死防御（实证）：超时只杀直接子进程，codex 的孙进程仍握管道会让 Wait 永挂；
	// WaitDelay 在进程退出后放弃残留 IO；POSIX 整组击杀见 sysproc_posix.go
	cmd.WaitDelay = 10 * time.Second
	applySysProc(cmd)
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return combined.String(), ee.ExitCode(), nil
		}
		return combined.String(), -1, err
	}
	return combined.String(), 0, nil
}
