// Package kimi：native-kimi 适配器（RFC-0002 分级晋级：第二个真实适配器，M2 C 轨）。
// 对接 kimi -p <prompt> --output-format stream-json（官方命令参考钉住 -p 非交互语义与
// stream-json 行流；实证 kimi-code 0.39.1：meta system.version / assistant content /
// meta session.resume_hint 三类行——fixtures 钉版本，漂移由 conformance 暴露）。
// 会话连续性：resume_hint 捕获 session_id，后续任务以 -S <id> 恢复（实证 codeword 回忆）。
// 已知约束（实证 2026-08-31）：-p 提示词只走 argv（"-p -" 不读 stdin）——argv 有 OS
// 长度上限（Windows CreateProcess ≈32k 字符），MaxPromptRunes 前置护栏fail fast；
// 大上下文/流式草稿的解法是 ACP 通道（kimi acp 已在官方命令面），登记为演进项。
package kimi

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
	KimiPath  string        // 可执行路径（来自 harness 注册表）
	WorkDir   string        // 会话工作目录（kimi 无 -C 等价物：native 用进程 cwd，WSL 用 sh cd 包装）
	ExtraArgs []string      // 预留（-m <model> 等 per-Profile 覆盖）
	Timeout   time.Duration // 单任务超时（默认 120s）
	Execer    Execer        // 测试注入；nil 用真实进程执行
	// WSL 运行面（Windows 宿主）：非空 = KimiPath 是发行版内 Linux 路径，
	// 经 wsl.exe -d <WSLDistro> 包装执行。
	WSLDistro string
	WSLHome   string // 发行版内 HOME（宿主 HOME 不适用；由 harness.HostRunner.Home 解析）
	// MaxPromptRunes 提示词 argv 安全上限（默认 6000）：kimi -p 不读 stdin（实证），
	// 提示词全量进 argv；超限 fail fast 报明确错误，胜过 OS spawn 失败。
	MaxPromptRunes int
	// EvalModel 评估任务专用模型（-m；空 = 与生成同模型）。dogfood 性能治理：
	// 评估输出仅几十 token，单座延迟瓶颈在模型档位——评估可降档、生成保持主模型。
	EvalModel string
}

// Execer 进程执行抽象（UT 捕获/阻塞；生产为真实 kimi 子进程）。
// dir 为进程工作目录（kimi 无 -C 等价物——cwd 即会话工作区）。
type Execer interface {
	Exec(ctx context.Context, argv []string, env []string, dir string) (stdout string, exitCode int, err error)
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
	if cfg.MaxPromptRunes <= 0 {
		cfg.MaxPromptRunes = 6000
	}
	return &Adapter{cfg: cfg}
}

// Name 适配器名。
func (a *Adapter) Name() string { return "kimi" }

// Capabilities 能力声明（RFC-0002 §3.1.2）。
func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false, // stream-json 是转录事件流而非增量草稿（与 codex --json 同形态）
		CancelMode:     "interrupt",
		HistoryChannel: "structured_request",
		Continuity:     true,  // -S <session_id>
		UsageReporting: false, // stream-json 未见 usage 面（实证 0.39.1）：缺失记 unknown，不虚构
		Observe:        false,
	}
}

// Boot 建立逻辑会话（无进程：kimi -p 按任务拉起，会话身份 = session_id）。
func (a *Adapter) Boot(_ context.Context, _ agent.Profile) (agent.Session, error) {
	return &session{adapter: a}, nil
}

// session 逻辑会话：持有 session_id 实现跨任务连续性（RFC-0002 §3.2）。
type session struct {
	adapter *Adapter
	mu      sync.Mutex
	sessID  string
}

func (s *session) Run(ctx context.Context, task agent.Task) (agent.Handle, error) {
	if task.Kind == agent.KindObserve {
		return nil, fmt.Errorf("kimi: 不支持 observe（Capabilities.Observe=false）")
	}
	h := &handle{done: make(chan struct{})}
	taskCtx, cancel := context.WithTimeout(ctx, s.adapter.cfg.Timeout)
	h.cancel = cancel
	go s.execute(taskCtx, task, h)
	return h, nil
}

func (s *session) Cancel(string) {}
func (s *session) Close()        {}

// execute 单任务执行：构建提示词 → kimi -p [--output-format stream-json]（-S 连续性）
// → 解析 → 映射。提示词走 argv（实证 -p 不读 stdin）；argv 长度前置护栏。
func (s *session) execute(taskCtx context.Context, task agent.Task, h *handle) {
	defer close(h.done)

	prompt, err := buildPrompt(task)
	if err != nil {
		h.err = err
		return
	}
	if n := len([]rune(prompt)); n > s.adapter.cfg.MaxPromptRunes {
		h.err = fmt.Errorf("kimi: 提示词超 argv 安全上限（%d > %d runes）：kimi -p 不读 stdin（实证），缩小上下文或走 ACP 通道", n, s.adapter.cfg.MaxPromptRunes)
		return
	}

	s.mu.Lock()
	sessID := s.sessID
	s.mu.Unlock()

	argv := []string{s.adapter.cfg.KimiPath}
	argv = append(argv, s.adapter.cfg.ExtraArgs...)
	argv = append(argv, s.evalModelArgs(task)...)
	argv = append(argv, "-p", prompt, "--output-format", "stream-json")
	if sessID != "" {
		argv = append(argv, "-S", sessID) // 连续性（实证：-p 与 -S 可组合）
	}

	stdout, code, err := s.execer().Exec(taskCtx, argv, s.envFor(), s.adapter.cfg.WorkDir)
	if err != nil {
		if taskCtx.Err() != nil {
			h.stale = true // 取消/超时：不发布正文，语义同迟到拒绝
			return
		}
		h.err = fmt.Errorf("kimi: exec: %w", err)
		return
	}
	parsed := ParseStream([]byte(stdout))
	if parsed.SessionID != "" && parsed.SessionID != sessID {
		s.mu.Lock()
		s.sessID = parsed.SessionID
		s.mu.Unlock()
	}
	if code != 0 {
		h.err = fmt.Errorf("kimi: kimi 退出码 %d：%s", code, firstLineOf(stdout, 200))
		return
	}
	if len(parsed.Messages) == 0 {
		h.err = fmt.Errorf("kimi: 无 assistant 输出")
		return
	}
	h.result, h.err = mapResult(task.Kind, parsed)
	if h.err == nil && task.Kind == agent.KindGenerate {
		h.sanitizePublish()
	}
}

// sanitizePublish 发布边界：委托端口级共享门 agent.PublishGate（与 codex 同一套门）。
func (h *handle) sanitizePublish() {
	body, _ := h.result.Data["body"].(string)
	clean, rels, err := agent.PublishGate(body, h.result.Data["declared_relations"])
	if err != nil {
		h.err = fmt.Errorf("kimi: %w", err)
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

// evalModelArgs 评估降档（dogfood 性能治理）：评估任务追加 -m <EvalModel>。
func (s *session) evalModelArgs(task agent.Task) []string {
	if task.Kind == agent.KindEvaluateIntent && s.adapter.cfg.EvalModel != "" {
		return []string{"-m", s.adapter.cfg.EvalModel}
	}
	return nil
}

// envFor 按运行面构造子进程环境（native 用宿主 HOME；wsl 用发行版内 HOME）。
func (s *session) envFor() []string {
	if s.adapter.cfg.WSLDistro != "" {
		return kimiEnvWithHome(s.adapter.cfg.KimiPath, s.adapter.cfg.WSLHome)
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return kimiEnvWithHome(s.adapter.cfg.KimiPath, home)
}

// kimiEnvWithHome 环境构造（native 与 WSL 共用，仅 HOME 来源不同）：
// PATH 前置可执行目录；HOME/KIMI_CODE_HOME 指向登录态所在；代理/CA 等网络配置
// 从宿主透传（与 codex 同口径：网络配置非凭据）；其余不透传。
func kimiEnvWithHome(kimiPath, home string) []string {
	if home == "" {
		home = "/root"
	}
	dir := kimiPath
	if i := strings.LastIndex(kimiPath, "/"); i > 0 {
		dir = kimiPath[:i]
	}
	env := []string{
		"PATH=" + dir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
		"KIMI_CODE_HOME=" + home + "/.kimi-code", // 登录态/配置所在（官方 data-locations 文档）
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
	done   chan struct{}
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

// ---- stream-json 解析（fixtures 钉 schema）----

// Parsed 一次 kimi -p 的解析结果。
type Parsed struct {
	SessionID string   // meta session.resume_hint 的 session_id（连续性句柄）
	Messages  []string // assistant 文本（按出现序）
}

type streamLine struct {
	Role    string `json:"role"`
	Type    string `json:"type"`
	Content string `json:"content"`
	// session.resume_hint 的 session_id 在顶层
	SessionID string `json:"session_id"`
}

// ParseStream 解析 kimi -p --output-format stream-json 的 JSONL 输出；
// 非 JSON 行忽略（stderr 混入等噪声——官方文档：诊断信息走 stderr，但进程执行面
// 合并两流，防御性跳过）。
func ParseStream(raw []byte) Parsed {
	var out Parsed
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev streamLine
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch {
		case ev.Role == "assistant" && ev.Content != "":
			out.Messages = append(out.Messages, ev.Content)
		case ev.Role == "meta" && ev.Type == "session.resume_hint" && ev.SessionID != "":
			out.SessionID = ev.SessionID
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

// ---- 提示词与结果映射 ----

const intentInstruction = `You are a participant in an ongoing group chat. You have just observed the latest messages. Decide whether to reply; staying silent is a valid, often good choice — reply only when you have something to add.
Reply with ONLY a JSON object, no prose, no code fences:
{"action":"speak|react|fork|summarize|silent","type":"answer|extend|challenge|support|question|redirect|synthesize","public_rationale":"<=280 chars","scores":{"relevance":0.0-1.0,"novelty":0.0-1.0,"urgency":0.0-1.0,"confidence":0.0-1.0}}`

const generateInstruction = `You are a participant in an ongoing group chat and have decided to reply.
Write your chat message directly below — concise, conversational, addressed to the room (no speeches, no meta commentary).
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
		return "", fmt.Errorf("kimi: 未知任务类型 %q", task.Kind)
	}
}

// mapResult 任务类型 → 端口结果块。校验口径与 codex 适配器对齐（最小结构检查 +
// 域层严格门：畸形 intent 由引擎弃权记录 R-01，不在适配器层变成任务失败）；
// 结构校验的端口级规范由 conformance 套件以 agent.ValidateBlock 独立复核。
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
			return agent.Result{}, fmt.Errorf("kimi: turn_intent 缺字段 %q", "action")
		}
		if action != "silent" {
			for _, field := range []string{"type", "scores"} {
				if _, ok := data[field]; !ok {
					return agent.Result{}, fmt.Errorf("kimi: turn_intent 缺字段 %q", field)
				}
			}
		}
		return agent.Result{Block: agent.BlockTurnIntent, Data: data}, nil
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
				}, nil
			}
		}
		// 纯文本回退：正文即发言
		return agent.Result{
			Block: agent.BlockPublicDraft,
			Data:  map[string]any{"body": text, "declared_relations": []any{}},
		}, nil
	case agent.KindSummarize:
		if data, err := agent.ExtractJSON(text); err == nil {
			if data["cited_event_ids"] == nil {
				data["cited_event_ids"] = []any{}
			}
			return agent.Result{Block: agent.BlockGroundedSummary, Data: data}, nil
		}
		return agent.Result{
			Block: agent.BlockGroundedSummary,
			Data:  map[string]any{"summary": text, "cited_event_ids": []any{}},
		}, nil
	case agent.KindEvaluateClosure:
		data, err := agent.ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		return agent.Result{Block: agent.BlockClosureIntent, Data: data}, nil
	default:
		return agent.Result{}, fmt.Errorf("kimi: 未知任务类型 %q", kind)
	}
}

type processExecer struct{}

// Exec 返回 stdout+stderr 合并流（kimi 诊断走 stderr——官方文档；ParseStream 忽略非
// JSON 行）。卡死防御与 codex 同构：WaitDelay + POSIX 进程组击杀（sysproc_posix.go）。
func (p *processExecer) Exec(ctx context.Context, argv []string, env []string, dir string) (string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
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

// wslExecer 把任务交给发行版内执行：wsl.exe -d <distro> -- env -i K=V... sh -c cd 包装。
// env -i 口径与 codex 一致（复审 #7：发行版默认环境/WSLENV 透传不继承）。
type wslExecer struct {
	distro string
}

// wslArgs 构造 wsl.exe 参数（纯函数，UT 覆盖）：kimi 无 -C 等价物——
// 工作目录经 sh -c 'cd "$1" && shift && exec "$@"' 包装进入发行版内 Linux 路径。
// 实证 2026-09-01（openEuler-24.03）：`--` 剩余参数会被 wsl.exe 拼接后交发行版默认
// shell 解释——引号剥除、| 等元字符按 shell 语义执行，kimi -p 的提示词（含
// "speak|react|fork"、JSON 引号）必被毁掉（实测 exit 127，Kimi 每波意图评估静默失败）。
// `--exec` 绕过默认 shell 直 exec：参数边界完整保留，stdin 照常流动（中文+元字符
// 提示词真机验证完好）。
func wslArgs(distro string, env []string, dir string, argv []string) []string {
	args := make([]string, 0, 10+len(env)+len(argv))
	args = append(args, "-d", distro, "--exec", "env", "-i")
	args = append(args, env...)
	if dir != "" {
		args = append(args, "sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", dir)
	}
	args = append(args, argv...)
	return args
}

func (w *wslExecer) Exec(ctx context.Context, argv []string, env []string, dir string) (string, int, error) {
	// 网络配置改取发行版侧（同 codex 真机复现结论：宿主无代理变量 → 发行版内
	// CLI 直连被墙）。宿主侧同名键剥除，发行版登录环境白名单键注入。
	env = wslenv.MergeForWSL(env, wslenv.NetEnv(w.distro))
	cmd := exec.CommandContext(ctx, "wsl.exe", wslArgs(w.distro, env, dir, argv)...)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	cmd.WaitDelay = 10 * time.Second
	// 已知缺口同 codex：击杀 wsl.exe 不必然终止发行版内进程——Windows Job Object 属
	// M2 进程管理项；超时值内任务自行退出为主路径。
	applySysProc(cmd) // Windows：不建控制台窗口（桌面壳防闪框）
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
