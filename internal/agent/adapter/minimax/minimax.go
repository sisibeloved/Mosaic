// Package minimax：MiniMax Code CLI（mcode）适配器（M3-1 观测基座：第三真实适配器）。
// 对接 mcode exec --input - --output-format stream-json（实证 0.2.7：事件 schema 无上游
// 稳定性承诺——fixtures 钉版本，漂移由 conformance 暴露）。
// 会话连续性：session.started.sessionId（mvs_*），后续任务以 --session <id> 恢复
// （实证：跨任务回忆）；--session 与 --cwd 可共存（实证）。
// 提示词走 --input - stdin（实证读 stdin）——无 argv 长度上限，kimi -p 的
// MaxPromptRunes 护栏类问题在本面不存在。
// 权限面：无头默认 smart（ask 需 TUI/ACP、off 全放行不可取——实证 exec --help）；
// 工作目录为 per-profile scratch（装配层隔离），smart 下文件操作爆炸半径受限。
// 输出分流：机器输出 stdout / 诊断 stderr（实证 stderr 恒空——比 kimi 合流干净）。
package minimax

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
	"github.com/sisibeloved/Mosaic/internal/wslenv"
)

// Config 适配器配置。
type Config struct {
	McodePath string        // 可执行路径（来自 harness 注册表）
	WorkDir   string        // 工作目录（mcode 有 --cwd，无需 sh cd 包装）
	ExtraArgs []string      // 预留（--model provider/model 等 per-Profile 覆盖）
	Timeout   time.Duration // 单任务超时（默认 120s）
	Execer    Execer        // 测试注入；nil 用真实进程执行
	// WSL 运行面（Windows 宿主）：非空 = McodePath 是发行版内 Linux 路径，
	// 经 wsl.exe -d <WSLDistro> --exec 包装执行（v1.30 实证：`--` 拼接经默认
	// shell 解释会毁参数；--exec 直 exec 保参数边界，stdin 照通）。
	WSLDistro string
	WSLHome   string // 发行版内 HOME（登录态在 $HOME/.minimax——实证 cli-auth）
	// MaxOutputRunes 发布正文硬上限（runes；与 codex/kimi 同一发布门）。
	MaxOutputRunes int
	// EvalModel 评估任务专用模型（--model provider/model；空 = 与生成同模型）。
	// dogfood 性能治理：评估输出仅几十 token，评估可降档、生成保持主模型。
	EvalModel string
}

// Execer 进程执行抽象（UT 捕获/阻塞；生产为真实 mcode 子进程）。
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
	if cfg.MaxOutputRunes <= 0 {
		cfg.MaxOutputRunes = 4000
	}
	return &Adapter{cfg: cfg}
}

// Name 适配器名。
func (a *Adapter) Name() string { return "minimax" }

// Capabilities 能力声明（RFC-0002 §3.1.2）。
func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false, // stream-json 是转录事件流而非增量草稿（同 codex/kimi 面）
		CancelMode:     "interrupt",
		HistoryChannel: "structured_request",
		Continuity:     true, // --session <id>
		UsageReporting: true, // turn.completed.usage（含 cache/reasoning 细分）
		Observe:        false,
	}
}

// Boot 建立逻辑会话（无进程：mcode exec 按任务拉起，会话身份 = sessionId）。
func (a *Adapter) Boot(_ context.Context, _ agent.Profile) (agent.Session, error) {
	return &session{adapter: a}, nil
}

// session 逻辑会话：持有 sessionId 实现跨任务连续性（RFC-0002 §3.2）。
type session struct {
	adapter *Adapter
	mu      sync.Mutex
	sessID  string
}

func (s *session) Run(ctx context.Context, task agent.Task) (agent.Handle, error) {
	if task.Kind == agent.KindObserve {
		return nil, fmt.Errorf("minimax: 不支持 observe（Capabilities.Observe=false）")
	}
	// 发布上限取 grant 宣告 ResponseCap 与适配器自身上限的较小者——宣告即执行（同 codex/kimi 面）。
	maxRunes := s.adapter.cfg.MaxOutputRunes
	if task.Kind == agent.KindGenerate && task.Grant != nil && task.Grant.ResponseCap > 0 &&
		int(task.Grant.ResponseCap) < maxRunes {
		maxRunes = int(task.Grant.ResponseCap)
	}
	h := &handle{done: make(chan struct{}), maxRunes: maxRunes}
	taskCtx, cancel := context.WithTimeout(ctx, s.adapter.cfg.Timeout)
	h.cancel = cancel
	go s.execute(taskCtx, task, h)
	return h, nil
}

func (s *session) Cancel(string) {}
func (s *session) Close()        {}

// execute 单任务执行：构建提示词 → mcode exec（stdin 提示词 + --session 连续性）→
// 解析 → 映射。
func (s *session) execute(taskCtx context.Context, task agent.Task, h *handle) {
	defer close(h.done)

	prompt, err := buildPrompt(task)
	if err != nil {
		h.err = err
		return
	}

	s.mu.Lock()
	sessID := s.sessID
	s.mu.Unlock()

	argv := []string{s.adapter.cfg.McodePath, "exec", "--output-format", "stream-json", "--input", "-"}
	argv = append(argv, s.adapter.cfg.ExtraArgs...)
	argv = append(argv, s.evalModelArgs(task)...)
	if s.adapter.cfg.WorkDir != "" {
		argv = append(argv, "--cwd", s.adapter.cfg.WorkDir) // 与 --session 可共存（实证）
	}
	if sessID != "" {
		argv = append(argv, "--session", sessID)
	}

	stdout, code, err := s.execer().Exec(taskCtx, argv, s.envFor(), prompt)
	if err != nil {
		if taskCtx.Err() != nil {
			h.stale = true // 取消/超时：不发布正文，语义同迟到拒绝
			return
		}
		h.err = fmt.Errorf("minimax: exec: %w", err)
		return
	}
	parsed := ParseStream([]byte(stdout))
	if parsed.SessionID != "" && parsed.SessionID != sessID {
		s.mu.Lock()
		s.sessID = parsed.SessionID
		s.mu.Unlock()
	}
	if code != 0 {
		h.err = fmt.Errorf("minimax: mcode 退出码 %d：%s", code, firstLineOf(stdout, 200))
		return
	}
	if parsed.Err != "" {
		h.err = fmt.Errorf("minimax: run failed: %s", parsed.Err)
		return
	}
	if len(parsed.Messages) == 0 {
		h.err = fmt.Errorf("minimax: 无 agent_message 输出")
		return
	}
	h.result, h.err = mapResult(task.Kind, parsed)
	if h.err == nil && task.Kind == agent.KindGenerate {
		h.sanitizePublish()
	}
}

// sanitizePublish 发布边界：委托端口级共享门 agent.PublishGate（与 codex/kimi 同一套门）。
func (h *handle) sanitizePublish() {
	body, _ := h.result.Data["body"].(string)
	clean, rels, err := agent.PublishGate(body, h.result.Data["declared_relations"], h.maxRunes)
	if err != nil {
		h.err = fmt.Errorf("minimax: %w", err)
		return
	}
	h.result.Data["body"] = clean
	h.result.Data["declared_relations"] = rels
}

func (s *session) execer() Execer {
	if s.adapter.cfg.Execer != nil {
		return s.adapter.cfg.Execer
	}
	if s.adapter.cfg.WSLDistro != "" {
		return &wslExecer{distro: s.adapter.cfg.WSLDistro}
	}
	return &processExecer{}
}

// evalModelArgs 评估降档（dogfood 性能治理）：评估任务追加 --model <EvalModel>。
func (s *session) evalModelArgs(task agent.Task) []string {
	if task.Kind == agent.KindEvaluateIntent && s.adapter.cfg.EvalModel != "" {
		return []string{"--model", s.adapter.cfg.EvalModel}
	}
	return nil
}

// envFor 按运行面构造子进程环境（native 用宿主 HOME；wsl 用发行版内 HOME）。
func (s *session) envFor() []string {
	if s.adapter.cfg.WSLDistro != "" {
		return mcodeEnvWithHome(s.adapter.cfg.McodePath, s.adapter.cfg.WSLHome)
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return mcodeEnvWithHome(s.adapter.cfg.McodePath, home)
}

// mcodeEnvWithHome 环境构造（native 与 WSL 共用，仅 HOME 来源不同）：
// PATH 前置可执行目录（npm/nvm 布局：node 与 CLI 同目录）；登录态在
// $HOME/.minimax（实证 cli-auth），HOME 即锚点；代理/CA 等网络配置从宿主透传
// （与 codex/kimi 同口径：网络配置非凭据——OQ-20 禁的是持有凭证与代理流量）；
// 其余不透传。
func mcodeEnvWithHome(mcodePath, home string) []string {
	if home == "" {
		home = "/root"
	}
	dir := mcodePath
	if i := strings.LastIndex(mcodePath, "/"); i > 0 {
		dir = mcodePath[:i]
	}
	env := []string{
		"PATH=" + dir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
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

// handle 单任务句柄：同步等待结果；Cancel 置位后 Result 报 ErrStale。
type handle struct {
	done     chan struct{}
	cancel   context.CancelFunc
	maxRunes int

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

// ---- stream-json 解析（fixtures 钉 schema；camelCase 键为 mcode 实证形状）----

// Parsed 一次 mcode exec 的解析结果。
type Parsed struct {
	SessionID string   // session.started.sessionId（连续性句柄）
	Messages  []string // item.completed(agent_message).content（按完成序）
	Usage     *agent.Usage
	Err       string // exec.failed / turn.failed / result.status 非 succeeded
}

type streamEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Item      *struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	} `json:"item"`
	Usage *struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
	} `json:"usage"`
	Result *struct {
		Status string `json:"status"`
	} `json:"result"`
	Message string `json:"message"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ParseStream 解析 mcode exec --output-format stream-json 的 JSONL 输出；
// 非 JSON 行忽略（防御性：stderr 混入等噪声——实证 stderr 恒空，双保险）。
func ParseStream(raw []byte) Parsed {
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
		case "session.started":
			out.SessionID = ev.SessionID
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				out.Messages = append(out.Messages, ev.Item.Content)
			}
		case "turn.completed":
			if ev.Usage != nil {
				out.Usage = &agent.Usage{
					InputTokens:  ev.Usage.InputTokens,
					OutputTokens: ev.Usage.OutputTokens,
					Model:        "minimax",
				}
			}
		case "exec.failed", "turn.failed":
			if ev.Error != nil && ev.Error.Message != "" {
				out.Err = ev.Error.Message
			} else if ev.Message != "" {
				out.Err = ev.Message
			}
		case "exec.completed":
			if ev.Result != nil && ev.Result.Status != "" && ev.Result.Status != "succeeded" {
				out.Err = "status=" + ev.Result.Status
			}
		}
	}
	return out
}

func firstLineOf(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// ---- 提示词与结果映射（与 codex/kimi 同一措辞——三适配器狗粮口径一致）----

const intentInstruction = `You are a participant in an ongoing group chat. You have just observed the latest messages. Decide whether to reply; staying silent is a valid, often good choice — reply only when you have something to add.
Reply with ONLY a JSON object, no prose, no code fences:
{"action":"speak|react|fork|summarize|silent","type":"answer|extend|challenge|support|question|redirect|synthesize","public_rationale":"<=280 chars","scores":{"relevance":0.0-1.0,"novelty":0.0-1.0,"urgency":0.0-1.0,"confidence":0.0-1.0}}`

const generateInstruction = `You are a participant in an ongoing group chat and have decided to reply.
Write your chat message directly below — concise, conversational, addressed to the room (no speeches, no meta commentary).
Stay within the response_cap given in Task identity (characters, CJK chars count as one each); anything beyond it is cut.
Reply with ONLY a JSON object, no prose, no code fences:
{"body":"your public message","declared_relations":[]}`

const summarizeInstruction = `Summarize the discussion below faithfully.
Reply with ONLY a JSON object: {"summary":"...","cited_event_ids":["..."]}`

const closureInstruction = `Judge whether the discussion below has converged.
Reply with ONLY a JSON object: {"action":"conclude|object|abstain","rationale":"..."}`

// taskIdentity 任务身份要素（agent-native：模型必须能看到 receipt/grant/thread 身份，
// Context Receipt 才能证伪"给了什么上下文"）。
func taskIdentity(task agent.Task) string {
	ident := map[string]any{
		"task_id":     task.TaskID,
		"room_id":     task.RoomID,
		"thread_id":   task.ThreadID,
		"receipt_ref": task.Context.ReceiptRef,
	}
	if task.Grant != nil {
		ident["grant_id"] = task.Grant.GrantID
		ident["rank"] = task.Grant.Rank
		ident["epoch"] = task.Grant.Epoch
		// v1.37（dogfood 治本）：上限对模型可见——软约束在生成侧生效，
		// PublishGate 截断只做极端兜底（此前 cap 只执行不宣告，模型"无辜违规"）。
		ident["response_cap"] = task.Grant.ResponseCap
	}
	raw, _ := json.Marshal(ident)
	return string(raw)
}

const charterNote = "Charter: deterministic attention arbitration selects speakers; no hidden reasoning; keep replies within the granted floor."

func buildPrompt(task agent.Task) (string, error) {
	stimulus, _ := json.Marshal(task.Context.Inline)
	ident := taskIdentity(task)
	switch task.Kind {
	case agent.KindEvaluateIntent:
		return intentInstruction + "\n\n" + charterNote + "\nTask identity: " + ident + "\n\nStimulus: " + string(stimulus), nil
	case agent.KindGenerate:
		return generateInstruction + "\n\n" + charterNote + "\nTask identity: " + ident + "\n\nDiscussion: " + string(stimulus), nil
	case agent.KindSummarize:
		return summarizeInstruction + "\nTask identity: " + ident + "\n\nDiscussion: " + string(stimulus), nil
	case agent.KindEvaluateClosure:
		return closureInstruction + "\nTask identity: " + ident + "\n\nDiscussion: " + string(stimulus), nil
	default:
		return "", fmt.Errorf("minimax: 未知任务类型 %q", task.Kind)
	}
}

// mapResult 任务类型 → 端口结果块。校验口径与 codex/kimi 对齐（最小结构检查 +
// 域层严格门：畸形 intent 由引擎弃权记录 R-01，不在适配器层变成任务失败）。
func mapResult(kind agent.TaskKind, parsed Parsed) (agent.Result, error) {
	text := parsed.Messages[len(parsed.Messages)-1]
	switch kind {
	case agent.KindEvaluateIntent:
		data, err := agent.ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		action, _ := data["action"].(string)
		if action == "" {
			return agent.Result{}, fmt.Errorf("minimax: turn_intent 缺字段 %q", "action")
		}
		if action != "silent" {
			for _, field := range []string{"type", "scores"} {
				if _, ok := data[field]; !ok {
					return agent.Result{}, fmt.Errorf("minimax: turn_intent 缺字段 %q", field)
				}
			}
		}
		return agent.Result{Block: agent.BlockTurnIntent, Data: data, Usage: parsed.Usage}, nil
	case agent.KindGenerate:
		// 封闭 DTO 投影：模型输出只投影已知字段进 message.posted 载荷（附加键不透传）。
		if data, err := agent.ExtractJSON(text); err == nil {
			if body, ok := data["body"].(string); ok && body != "" {
				relations, _ := data["declared_relations"].([]any)
				if data["declared_relations"] == nil {
					relations = []any{}
				}
				return agent.Result{
					Block: agent.BlockPublicDraft,
					Data:  map[string]any{"body": body, "declared_relations": relations},
					Usage: parsed.Usage,
				}, nil
			}
		}
		// 纯文本回退：正文即发言
		return agent.Result{
			Block: agent.BlockPublicDraft,
			Data:  map[string]any{"body": text, "declared_relations": []any{}},
			Usage: parsed.Usage,
		}, nil
	case agent.KindSummarize:
		if data, err := agent.ExtractJSON(text); err == nil {
			if data["cited_event_ids"] == nil {
				data["cited_event_ids"] = []any{}
			}
			return agent.Result{Block: agent.BlockGroundedSummary, Data: data, Usage: parsed.Usage}, nil
		}
		return agent.Result{
			Block: agent.BlockGroundedSummary,
			Data:  map[string]any{"summary": text, "cited_event_ids": []any{}},
			Usage: parsed.Usage,
		}, nil
	case agent.KindEvaluateClosure:
		data, err := agent.ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		return agent.Result{Block: agent.BlockClosureIntent, Data: data, Usage: parsed.Usage}, nil
	default:
		return agent.Result{}, fmt.Errorf("minimax: 未知任务类型 %q", kind)
	}
}

type processExecer struct{}

// Exec 返回 stdout（诊断走 stderr，实证恒空——不合并，保持分流语义）。
// 卡死防御与 codex/kimi 同构：WaitDelay + POSIX 进程组击杀（sysproc_posix.go）。
func (p *processExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader([]byte(stdin))
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	cmd.WaitDelay = 10 * time.Second
	applySysProc(cmd)
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), ee.ExitCode(), nil
		}
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}

// wslExecer 把任务交给发行版内执行：wsl.exe -d <distro> --exec env -i K=V... <argv...>。
// --exec（v1.30 实证）：`--` 剩余参数经发行版默认 shell 解释（引号剥除/元字符执行），
// 直 exec 保参数边界；stdin（提示词）照常流动。env -i 口径与 codex/kimi 一致。
type wslExecer struct {
	distro string
}

// wslArgs 构造 wsl.exe 参数（纯函数，UT 覆盖）。mcode 自带 --cwd——无需 kimi 的
// sh -c 'cd' 包装，argv 直传。
func wslArgs(distro string, env []string, argv []string) []string {
	args := make([]string, 0, 5+len(env)+len(argv))
	args = append(args, "-d", distro, "--exec", "env", "-i")
	args = append(args, env...)
	return append(args, argv...)
}

func (w *wslExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, int, error) {
	// 网络配置改取发行版侧（同 codex 真机复现结论：宿主无代理变量 → 发行版内
	// CLI 直连被墙）。宿主侧同名键剥除，发行版登录环境白名单键注入。
	env = wslenv.MergeForWSL(env, wslenv.NetEnv(w.distro))
	cmd := exec.CommandContext(ctx, "wsl.exe", wslArgs(w.distro, env, argv)...)
	cmd.Stdin = bytes.NewReader([]byte(stdin))
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	cmd.WaitDelay = 10 * time.Second
	// 已知缺口同 codex/kimi：击杀 wsl.exe 不必然终止发行版内进程——Windows Job
	// Object 属 M2 进程管理项；超时值内任务自行退出为主路径。
	applySysProc(cmd) // Windows：不建控制台窗口（桌面壳防闪框）
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), ee.ExitCode(), nil
		}
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}
