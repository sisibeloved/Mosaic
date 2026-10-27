// Package claude：Claude Code CLI 适配器（0.4.0 "Agent 兼容性扩充"：第五个真实适配器）。
// 对接 claude -p --output-format stream-json --verbose（实证 2026-10-27，claude
// 2.1.172 = WSL openEuler 原生安装位 ~/.local/bin/claude；Windows 原生安装位
// %USERPROFILE%\.local\bin 同构发现）。事件 schema 无上游稳定性承诺——fixtures
// 钉版本，漂移由 conformance 暴露。
//
// 实证钉死的三处 CLI 行为（2.1.172，均捕获于 fixtures/exec-authfail.jsonl 与
// 交付记录）：
//  1. -p --output-format stream-json 必须搭配 --verbose——缺省时退出码 0、
//     stdout 空、真因只在 stderr（"Error: When using --print, --output-format=
//     stream-json requires --verbose"）。argv 恒带 --verbose，缺省形态不出现。
//  2. 退出码 0 ≠ 成功：鉴权失败（OAuth token 失效）同样退出码 0，真因在
//     result 终止事件——{"type":"result","subtype":"success","is_error":true,
//     "api_error_status":401,"result":"Failed to authenticate. …"}。成功判定
//     必须检查 result.is_error，退出码只兜底进程级失败（其余四家适配器以
//     非零退出码为主信号的口径在此不适用）。
//  3. 提示词 stdin 是 -p 的一等通道（"Input must be provided either through
//     stdin or as a prompt argument"）——与 mcode 同形（无 argv 长度上限，
//     无 sh "$(cat)" 代入包装）；空 stdin 的报错同样退出码 0，无输出错误的
//     诊断行取 stderr 首行。
//
// 会话连续性：session_id（UUID）从任意流内事件捕获（system/init 起即携带），
// 后续任务以 --resume <id> 恢复（官方命令面；fork 语义用 --fork-session 另开，
// 本适配器沿用原 ID 续聊，与其他四家同构）。模型覆盖：原生 --model flag（别名
// sonnet/opus 或全名），评估降档同 kimi 面（EvalModel > Model > CLI 默认）。
// 权限面（v1.52 裁定 "要么有 YOLO 模式，要么权限 Full 级别"）：--dangerously-
// skip-permissions 是 YOLO 等价（--permission-mode bypassPermissions 同义）。
// 网络注意（实证）：CLI 对 401 等鉴权错误内部重试（system/api_retry，
// max_retries 10）——任务超时兜底，勿以短超时驱动。
// 输出分流：机器输出 stdout / 诊断 stderr（zcode/minimax 同构）。
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/wslenv"
)

// Config 适配器配置。
type Config struct {
	ClaudePath string        // 可执行路径（来自 harness 注册表）
	WorkDir    string        // 工作目录（claude 无 --cwd 等价 flag：native 用进程 cwd，WSL 用 sh cd 包装——kimi 同构）
	ExtraArgs  []string      // 预留（per-Profile 覆盖）
	Timeout    time.Duration // 单任务超时（默认 120s）
	Execer     Execer        // 测试注入；nil 用真实进程执行
	// WSL 运行面（Windows 宿主）：非空 = ClaudePath 是发行版内 Linux 路径，
	// 经 wsl.exe -d <WSLDistro> --exec 包装执行（v1.30 实证：--exec 直 exec
	// 保参数边界；sh cd 包装进发行版内完成，stdin 照通）。
	WSLDistro string
	WSLHome   string // 发行版内 HOME（登录态/配置在 $HOME/.claude）
	// EvalModel 评估任务专用模型（--model；空 = 与生成同模型）。dogfood 性能
	// 治理：评估输出仅几十 token，评估可降档、生成保持主模型。
	EvalModel string
	// Model 模型覆盖（--model；空 = CLI 默认，尊重 settings.json 侧既有选择）。
	// 值取 CLI 模型别名（sonnet/opus 等）或全名；CLI 侧校验，无效名任务报错。
	Model string
}

// Execer 进程执行抽象（UT 捕获/阻塞；生产为真实 claude 子进程）。
// dir 为进程工作目录（claude 无 --cwd 等价物——cwd 即会话工作区）；stdin 为
// 提示词（-p 的 stdin 一等通道，实证见包注释）；stdout/stderr 分流返回。
type Execer interface {
	Exec(ctx context.Context, argv []string, env []string, dir string, stdin string) (stdout, stderr string, exitCode int, err error)
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
func (a *Adapter) Name() string { return "claude" }

// Capabilities 能力声明（RFC-0002 §3.1.2）。
func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false, // stream-json 是转录事件流而非增量草稿（同其余四家面）
		CancelMode:     "interrupt",
		HistoryChannel: "structured_request",
		Continuity:     true, // --resume <session_id>
		UsageReporting: true, // result.usage（input_tokens/output_tokens；含 cache 细分未拆）
		Observe:        false,
		TaskRuns:       true, // M4-1：exec 进程可由 Mosaic 托管为长任务（结果回传房间）
		ReplyOrPass:    true, // M4-3：单次 reply-or-pass（限定路径）
		MemoryCuration: true, // v1.70：每波记忆评审（Hermes 同构自助策展）
	}
}

// Boot 建立逻辑会话（无进程：claude -p 按任务拉起，会话身份 = session_id）。
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
		return nil, fmt.Errorf("claude: 不支持 observe（Capabilities.Observe=false）")
	}
	h := &handle{done: make(chan struct{})}
	taskCtx, cancel := context.WithTimeout(ctx, s.adapter.cfg.Timeout)
	h.cancel = cancel
	go s.execute(taskCtx, task, h)
	return h, nil
}

func (s *session) Cancel(string) {}
func (s *session) Close()        {}

// execute 单任务执行：构建提示词 → claude -p stream-json（stdin 提示词 +
// --resume 连续性 + --model 覆盖）→ 解析 → 映射；严格 JSON 契约任务在输出
// 形状失败时做一次矫正重试。
func (s *session) execute(taskCtx context.Context, task agent.Task, h *handle) {
	defer close(h.done)

	prompt, err := buildPrompt(task)
	if err != nil {
		h.err = err
		return
	}

	parsed, err := s.execOnce(taskCtx, task, prompt)
	if err != nil {
		if taskCtx.Err() != nil {
			h.stale = true // 取消/超时：不发布正文，语义同迟到拒绝
			return
		}
		h.err = err
		return
	}
	h.result, h.err = mapResult(task.Kind, parsed)
	if h.err != nil && strictJSONTask(task.Kind) && taskCtx.Err() == nil {
		h.result, h.err = s.retryStrictTask(taskCtx, task, prompt, h.err, parsed)
	}
	if h.err == nil && task.Kind == agent.KindGenerate && h.result.Block == agent.BlockPublicDraft {
		h.sanitizePublish() // history_request 非发布物，不过发布门
	}
}

// execOnce 单次 claude -p 全链：argv 组装（恒定 --verbose + YOLO 旗标 + 模型
// 覆盖 + 会话连续性）→ 执行 → stream 解析 → 错误检查。错误判定两段（claude
// 特有，见包注释）：非零退出码走进程级错误（流内 error > stderr 首行 > stdout
// 首行，zcode 同构）；零退出码仍须检查 result.is_error（鉴权失败等以
// subtype:"success" + is_error:true 报告，退出码不可信）。
func (s *session) execOnce(taskCtx context.Context, task agent.Task, prompt string) (Parsed, error) {
	s.mu.Lock()
	sessID := s.sessID
	s.mu.Unlock()

	args := []string{s.adapter.cfg.ClaudePath}
	args = append(args, s.adapter.cfg.ExtraArgs...)
	args = append(args, s.runtimeArgs(task)...)
	args = append(args, "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions")
	if sessID != "" {
		args = append(args, "--resume", sessID)
	}

	// 提示词走 stdin（-p 一等通道，实证）：argv 不含提示词，无 argv 长度上限
	//（kimi/zcode 的 MAX_ARG_STRLEN 边界在此不存在，亦无需 sh "$(cat)" 代入）。
	stdout, stderr, code, err := s.execer().Exec(taskCtx, args, s.envFor(), s.adapter.cfg.WorkDir, prompt)
	if err != nil {
		return Parsed{}, fmt.Errorf("claude: exec: %w", err)
	}
	parsed := ParseStream([]byte(stdout))
	if parsed.SessionID != "" && parsed.SessionID != sessID {
		s.mu.Lock()
		s.sessID = parsed.SessionID
		s.mu.Unlock()
	}
	if code != 0 {
		// 进程级失败：原因优先级流内 error > stderr 首行 > stdout 首行。
		if parsed.Err != "" {
			return parsed, fmt.Errorf("claude: claude 退出码 %d：%s", code, parsed.Err)
		}
		if msg := firstLineOf(stderr, 200); msg != "" {
			return parsed, fmt.Errorf("claude: claude 退出码 %d：%s", code, msg)
		}
		return parsed, fmt.Errorf("claude: claude 退出码 %d：%s", code, firstLineOf(stdout, 200))
	}
	// 零退出码的成功判定以 result.is_error 为准（实证：鉴权失败退出码 0）。
	if parsed.Err != "" {
		return parsed, fmt.Errorf("claude: result 报告错误（is_error）：%s", parsed.Err)
	}
	if len(parsed.Messages) == 0 {
		// 用法类错误（如空 stdin）同样退出码 0 且 stdout 无事件——诊断行取
		// stderr 首行（"Error: Input must be provided…" 实证形态）。
		if msg := firstLineOf(stderr, 200); msg != "" {
			return parsed, fmt.Errorf("claude: 无 result 正文输出：%s", msg)
		}
		return parsed, fmt.Errorf("claude: 无 result 正文输出")
	}
	return parsed, nil
}

// retryStrictTask 矫正重试（一次，仅严格 JSON 契约任务）：minimax v1.69 真机
// 实证先例、zcode 同构——会话历史含强待办语境时模型可能把仲裁元任务当作房间
// 任务执行。矫正追记附在原提示词之后（同会话：模型可见自己的跑偏输出），
// 恢复该座位；仍失败则保留末次错误并附输出首行。
func (s *session) retryStrictTask(taskCtx context.Context, task agent.Task, prompt string, firstErr error, first Parsed) (agent.Result, error) {
	second, err := s.execOnce(taskCtx, task, prompt+correctiveNote(task.Kind))
	if err != nil {
		if taskCtx.Err() != nil {
			return agent.Result{}, err // 重试中取消/超时：无结果可发布，错误即语义
		}
		return agent.Result{}, fmt.Errorf("claude: %v；矫正重试执行失败：%v", firstErr, err)
	}
	res, mapErr := mapResult(task.Kind, second)
	if mapErr != nil {
		return agent.Result{}, fmt.Errorf("claude: %v；重试后仍不可解析（模型输出首行：%s）",
			mapErr, firstLineOf(second.Messages[len(second.Messages)-1], 120))
	}
	res.Usage = sumUsage(first.Usage, second.Usage)
	return res, nil
}

// sumUsage 两跳消耗合计（重试不是免费的——账本按真实消耗入账）。
func sumUsage(a, b *agent.Usage) *agent.Usage {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	sum := *b
	sum.InputTokens += a.InputTokens
	sum.OutputTokens += a.OutputTokens
	return &sum
}

// strictJSONTask 严格 JSON 契约任务（无散文回退）：输出形状失败可安全矫正重试。
func strictJSONTask(k agent.TaskKind) bool {
	return k == agent.KindEvaluateIntent || k == agent.KindEvaluateClosure || k == agent.KindReplyOrPass
}

// correctiveNote 矫正追记（重试提示词 = 原提示词 + 本段）。评估/收敛为英文指令面，
// reply_or_pass 为中文指令面——追记跟随各自指令语言（四家逐字同源）。
const correctiveNoteEval = `

CORRECTION: Your previous reply was not the required JSON object — you may have started answering or performing the discussion's task. This is an internal arbitration request, not a message to the room and not a task to do. Reply now with ONLY the required JSON object, nothing else.`

const correctiveNoteROP = `

矫正：你上一条回复不是所要求的 JSON 对象——你可能已经开始回答或执行讨论中的任务。这是内部决策请求，不是要发给房间的消息，也不是要执行的任务。现在只输出所要求的 JSON 对象，无其他文本。`

func correctiveNote(k agent.TaskKind) string {
	if k == agent.KindReplyOrPass {
		return correctiveNoteROP
	}
	return correctiveNoteEval
}

// sanitizePublish 发布边界：委托端口级共享门 agent.PublishGate（与其余四家同一套门）。
func (h *handle) sanitizePublish() {
	body, _ := h.result.Data["body"].(string)
	clean, rels, err := agent.PublishGate(body, h.result.Data["declared_relations"])
	if err != nil {
		h.err = fmt.Errorf("claude: %w", err)
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

// runtimeArgs 运行参数：模型覆盖（--model，claude 原生 flag——zcode 需 provider
// overlay 的对应面在此直接生效）。评估任务优先级 EvalModel > Model > CLI
// 默认（评估降档不被主模型覆盖吞掉）。
func (s *session) runtimeArgs(task agent.Task) []string {
	model := s.adapter.cfg.Model
	if task.Kind == agent.KindEvaluateIntent && s.adapter.cfg.EvalModel != "" {
		model = s.adapter.cfg.EvalModel
	}
	if model != "" {
		return []string{"--model", model}
	}
	return nil
}

// homeDir 运行面 HOME（native 用宿主家目录；WSL 用发行版内 HOME）。native 侧
// UserHomeDir 优先于 HOME 环境变量（Windows 宿主常无 HOME，且从 Git Bash 类壳
// 启动时 HOME 可能是 POSIX 形——文件路径构造需原生形态）。
func (s *session) homeDir() string {
	if s.adapter.cfg.WSLDistro != "" {
		if s.adapter.cfg.WSLHome != "" {
			return s.adapter.cfg.WSLHome
		}
		return "/root"
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return "/root"
}

// envFor 按运行面构造子进程环境。
func (s *session) envFor() []string {
	home := s.homeDir()
	dir := claudeBinDir(s.adapter.cfg.ClaudePath)
	if s.adapter.cfg.WSLDistro == "" && runtime.GOOS == "windows" {
		return claudeEnvWindows(dir, home)
	}
	return claudeEnvPOSIX(dir, home)
}

// claudeBinDir 可执行目录（PATH 前置锚点；POSIX 分隔符形态——WSL/Linux 路径）。
func claudeBinDir(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return path
}

// passthroughNetEnv 网络配置透传白名单（两运行面共用）：代理/CA 从宿主透传
// （与 codex/kimi/minimax/zcode 同口径：网络配置非凭据——OQ-20 禁的是持有凭证
// 与代理流量）；不传 API key。ANTHROPIC_BASE_URL 是网络路由配置（非凭据），
// 供中转网关用户随宿主环境生效；ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN 属
// 凭据，不透传（凭据留 CLI 侧自有配置）。claude 是 Node 实现，标准 http_proxy
// 系变量原生生效（对照：zcode 需 ZCODE_HTTP_PROXY 专键）。
func passthroughNetEnv() []string {
	var env []string
	for _, key := range []string{
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"ANTHROPIC_BASE_URL",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
	} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// claudeEnvPOSIX 环境构造（WSL 面与 Linux/macOS native 共用）：PATH 前置可执行
// 目录；HOME 锚定登录态/配置（$HOME/.claude——OAuth credentials.json 实证位）。
func claudeEnvPOSIX(binDir, home string) []string {
	if home == "" {
		home = "/root"
	}
	env := []string{
		"PATH=" + binDir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
	}
	return append(env, passthroughNetEnv()...)
}

// claudeEnvWindows Windows native 面环境：HOME/USERPROFILE 双锚（claude 是 Node
// 实现，os.homedir 在 Windows 走 USERPROFILE 定位 %USERPROFILE%\.claude 凭证；
// HOME 兜底自定路径读取）+ Windows 必需系统键（SYSTEMROOT——node winsock 初始
// 化依赖；TEMP/TMP）+ PATH（exe 目录前置，Windows 分号分隔）+ 网络透传白名单
// （zcode desktopEnvFor 最小集同构）。
func claudeEnvWindows(binDir, home string) []string {
	env := []string{
		"HOME=" + home,
		"USERPROFILE=" + home,
	}
	for _, key := range []string{"SYSTEMROOT", "SYSTEMDRIVE", "TEMP", "TMP", "APPDATA", "LOCALAPPDATA"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return append(env, passthroughNetEnv()...)
}

// ---- stream-json 解析（fixtures 钉 2.1.172 schema；键 snake_case 为实证形状）----

// Parsed 一次 claude -p 的解析结果。
type Parsed struct {
	SessionID string   // 任意流内事件的 session_id（UUID，连续性句柄）
	Messages  []string // 最终正文：result.result（兜底 assistant 文本块拼接）
	Usage     *agent.Usage
	Err       string // result.is_error 时 result 字段的错误正文（退出码 0 亦携带——见包注释）
}

type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	// result 终止事件：result（最终正文/错误正文）+ is_error + usage。
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	// assistant 事件载荷：message.content[].text（result 缺失时的正文兜底）。
	Message *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// ParseStream 解析 claude -p --output-format stream-json 的 JSONL 输出；
// 非 JSON 行忽略（防御性：stderr 混入等噪声，与其余四家同口径）。无关事件
// （system/hook_*、system/api_retry、user/tool 转录等）跳过——正文只认 result
// 与 assistant 文本。
func ParseStream(raw []byte) Parsed {
	var out Parsed
	var assistantTexts []string
	sawResult := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // 未知/噪声行
		}
		if ev.SessionID != "" && out.SessionID == "" {
			out.SessionID = ev.SessionID // 任意事件首见即捕获（连续性句柄）
		}
		switch ev.Type {
		case "result":
			// 终止事件：最终正文与 usage 的权威来源；is_error 时 result 是错误
			// 正文（成功判定不在解析层做，Err 交由 execOnce 检查）。
			sawResult = true
			if ev.IsError {
				if ev.Result != "" {
					out.Err = ev.Result
				} else {
					out.Err = "is_error（result 无错误正文，subtype=" + ev.Subtype + "）"
				}
			} else if ev.Result != "" {
				out.Messages = []string{ev.Result}
			}
			if ev.Usage != nil {
				out.Usage = &agent.Usage{
					InputTokens:  ev.Usage.InputTokens,
					OutputTokens: ev.Usage.OutputTokens,
					Model:        "claude",
				}
			}
		case "assistant":
			// 转录面（result 缺失时的兜底正文）；model="<synthetic>" 的失败
			// assistant 消息同被收录——只要 result.is_error 已置 Err，execOnce
			// 先检查 Err，不会把失败转录当正文发布。
			if ev.Message != nil {
				for _, blk := range ev.Message.Content {
					if blk.Type == "text" && blk.Text != "" {
						assistantTexts = append(assistantTexts, blk.Text)
					}
				}
			}
		}
	}
	if !sawResult && len(assistantTexts) > 0 {
		out.Messages = append([]string(nil), assistantTexts...)
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

// ---- 提示词与结果映射（与 codex/kimi/minimax/zcode 同一措辞——五适配器狗粮口径一致）----

const intentInstruction = `You are a participant in an ongoing group chat. You have just observed the latest messages. Decide whether to reply; staying silent is a valid, often good choice — reply only when you have something to add.
Most messages in a busy group chat do not need YOUR reply — silence is the norm, not the exception. Speak when: you are directly addressed or asked; the topic is squarely your expertise; you can correct an error or add substance others missed. If your_activity shows you spoke recently, hold back unless addressed — let others take the floor.
Reply with ONLY a JSON object, no prose, no code fences:
{"action":"speak|react|fork|summarize|silent","type":"answer|extend|challenge|support|question|redirect|synthesize","public_rationale":"<=280 chars","scores":{"relevance":0.0-1.0,"novelty":0.0-1.0,"urgency":0.0-1.0,"confidence":0.0-1.0}}
This is an internal arbitration request, not a message to the room: never answer, perform, or start the discussion's tasks here — the only valid reply is the JSON decision above.`

const generateInstruction = `You are a participant in an ongoing group chat and have decided to reply.
Write your chat message directly below — concise, conversational, addressed to the room (no speeches, no meta commentary).
Reply with ONLY a JSON object, no prose, no code fences:
{"body":"your public message","declared_relations":[]}
The JSON must contain your public chat message in "body" — never an arbitration decision (action/silent/scores) or any other internal JSON.
If (and only if) the discussion clearly refers to earlier context you cannot see, reply {"history_query":"search phrase"} instead of a body — you will receive the matching room messages and retry once.
When you create or edit a document, narrate the change in "body" and add "doc_ops" in the same JSON (at most 8 ops): {"op":"create","title":"...","blocks":[{"type":"heading|paragraph|list|code|quote|hr","text":"..."}]} | {"op":"append","doc_id":"doc_...","blocks":[...]} | {"op":"replace_section","doc_id":"doc_...","heading":"<exact heading text>","blocks":[...]} | {"op":"insert_after","doc_id":"doc_...","block_id":"blk_...","blocks":[...]} | {"op":"delete_section","doc_id":"doc_...","heading":"<exact heading text>"}. Document excerpts in the context carry [block_id] prefixes; a section is a heading plus the blocks up to the next heading. Never emit doc_ops without explaining the change in "body". For heading blocks prefer "level" (1..6, default 2) over hand-written "#" prefixes; for code blocks prefer "lang" (e.g. "go") over hand-written fences. When you create or edit a spreadsheet, narrate the change in "body" and add "sheet_ops" in the same JSON (at most 8 ops): {"op":"create","title":"...","cells":[{"r":1,"c":1,"type":"text|number|bool|formula|null","text":"..."|...}]} | {"op":"add_sheet","doc_id":"doc_...","base_version":N,"name":"..."} | {"op":"set_cells","doc_id":"doc_...","base_version":N,"sheet_id":"sht_... (empty = first sheet)","cells":[...]} | {"op":"clear_range","doc_id":"doc_...","base_version":N,"range":{"r1":1,"c1":1,"r2":10,"c2":5}} | {"op":"insert_rows|insert_columns|delete_rows|delete_columns","doc_id":"doc_...","base_version":N,"at":N,"count":N}. Edit ops MUST carry the current "base_version" (the v-number in the spreadsheet excerpt header) and explicit coordinates. Formulas (e.g. "SUM(A1:B3)*2") are stored verbatim and evaluated by the server; supported functions: SUM, AVERAGE, MIN, MAX, COUNT, COUNTA, IF, ROUND. Never emit sheet_ops without explaining the change in "body".`

// reviewInstruction 每波记忆评审（v1.70，Hermes background_review 同构——
// 措辞五家同源；容量门在引擎侧，提示词只强调"先查清单再动笔"的策展纪律）。
const reviewInstruction = `You are the memory curator for this room. Review the wave transcript below and decide whether anything is worth persisting to the room's shared memory.
Worth saving: user preferences and standing expectations; local conclusions or decisions reached; durable facts about the room or its members; corrections to existing entries.
NOT worth saving: transient errors, one-off task narration, anything already captured, environment-specific failures the user can fix.
Memory is a small curated list under a hard character budget — prefer replace/remove over adding near-duplicates, and consult the current inventory (memory_inventory / memory_budget in the Stimulus) before writing.
Reply with ONLY a JSON object, no prose, no code fences:
{"ops":[{"action":"add","content":"..."},{"action":"replace","old_text":"<unique substring of an existing entry>","content":"..."},{"action":"remove","old_text":"<unique substring>"}],"public_rationale":"<=120 chars"}
Use {"ops":[],"public_rationale":"Nothing to save"} when nothing qualifies.`

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

// ropInstruction 单次 reply-or-pass（M4-3 限定路径：仅点名单人/任务交付两场景）。
const ropInstruction = `你是群聊中被明确点名或被追问任务交付的 Agent。基于 Stimulus 语境一次性决定：公开回应（speak）或保持沉默（pass）。
只输出一个 JSON 对象，无其他文本：
{"action": "speak", "body": "公开回复正文（不超过 300 字）", "public_rationale": "为何回应"}
或
{"action": "pass", "public_rationale": "为何沉默（公开可见）"}
被点名且你能有效回答 → speak；问题与你无关或你无增值 → pass。不虚构、不引用语境之外的信息。`

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
	case agent.KindReplyOrPass:
		return ropInstruction + "\nTask identity: " + ident + "\n\nStimulus: " + string(stimulus), nil
	case agent.KindReviewMemory:
		return reviewInstruction + "\nTask identity: " + ident + "\n\nStimulus: " + string(stimulus), nil
	default:
		return "", fmt.Errorf("claude: 未知任务类型 %q", task.Kind)
	}
}

// mapResult 任务类型 → 端口结果块。校验口径与 codex/kimi/minimax/zcode 对齐
// （最小结构检查 + 域层严格门：畸形 intent 由引擎弃权记录 R-01，不在适配器层
// 变成任务失败）。
func mapResult(kind agent.TaskKind, parsed Parsed) (agent.Result, error) {
	text := parsed.Messages[len(parsed.Messages)-1]
	switch kind {
	case agent.KindReplyOrPass:
		data, err := agent.ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		action, _ := data["action"].(string)
		if action != "speak" && action != "pass" {
			return agent.Result{}, fmt.Errorf("claude: reply_or_pass 缺字段 action（speak|pass）")
		}
		if action == "speak" {
			body, _ := data["body"].(string)
			if strings.TrimSpace(body) == "" {
				return agent.Result{}, fmt.Errorf("claude: reply_or_pass speak 缺正文")
			}
		}
		return agent.Result{Block: "reply_or_pass_decision", Data: data, Usage: parsed.Usage}, nil
	case agent.KindEvaluateIntent:
		data, err := agent.ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		action, _ := data["action"].(string)
		if action == "" {
			return agent.Result{}, fmt.Errorf("claude: turn_intent 缺字段 %q", "action")
		}
		if action != "silent" {
			for _, field := range []string{"type", "scores"} {
				if _, ok := data[field]; !ok {
					return agent.Result{}, fmt.Errorf("claude: turn_intent 缺字段 %q", field)
				}
			}
		}
		agent.CapIntentRationale(data) // 超长 rationale 截断保决策（IT 实证防回归）
		return agent.Result{Block: agent.BlockTurnIntent, Data: data, Usage: parsed.Usage}, nil
	case agent.KindGenerate:
		// 封闭 DTO 投影：模型输出只投影已知字段进 message.posted 载荷（附加键不透传）。
		// JSON 而无可用 body = 决策/内部件误入生成位——拒绝发布，不冒充发言；
		// 例外是自报 history_query（v1.70 两段式检索）；纯散文回复仍走回退。
		text = agent.NormalizeGenerateJSON(text)
		if data, err := agent.ExtractJSON(text); err == nil {
			body, _ := data["body"].(string)
			if strings.TrimSpace(body) == "" {
				if q, _ := data["history_query"].(string); strings.TrimSpace(q) != "" {
					return agent.Result{
						Block: agent.BlockHistoryRequest,
						Data:  map[string]any{"history_query": q},
						Usage: parsed.Usage,
					}, nil
				}
				return agent.Result{}, fmt.Errorf("claude: generate 输出为 JSON 但缺可用 body（不发布）：%s", firstLineOf(text, 120))
			}
			relations, _ := data["declared_relations"].([]any)
			if data["declared_relations"] == nil {
				relations = []any{}
			}
			draft := map[string]any{"body": body, "declared_relations": relations}
			// RFC-0014 §2.7 侧车写面：doc_ops 过端口校验才投影（封闭 DTO——
			// 校验不过视为走私形状，丢弃不透传；引擎侧仍有二次校验）。
			if rawOps, ok := data["doc_ops"]; ok {
				if verr := agent.ValidateBlock(agent.BlockDocOps, map[string]any{"ops": rawOps}); verr == nil {
					draft["doc_ops"] = rawOps
				}
			}
			// RFC-0017 §4.3 表格侧车：sheet_ops 同纪律（端口校验过门才投影）。
			if rawSheetOps, ok := data["sheet_ops"]; ok {
				if verr := agent.ValidateBlock(agent.BlockSheetOps, map[string]any{"ops": rawSheetOps}); verr == nil {
					draft["sheet_ops"] = rawSheetOps
				}
			}
			return agent.Result{
				Block: agent.BlockPublicDraft,
				Data:  draft,
				Usage: parsed.Usage,
			}, nil
		}
		// JSON 形状但解析失败：契约失败拒绝发布（座位失败可见）；纯散文回退。
		if agent.LooksLikeJSON(text) {
			return agent.Result{}, fmt.Errorf("claude: generate 输出呈 JSON 形状但解析失败（不发布）：%s", firstLineOf(text, 120))
		}
		// 纯文本回退：正文即发言
		return agent.Result{
			Block: agent.BlockPublicDraft,
			Data:  map[string]any{"body": text, "declared_relations": []any{}},
			Usage: parsed.Usage,
		}, nil
	case agent.KindReviewMemory:
		data, err := agent.ExtractJSON(text)
		if err != nil {
			return agent.Result{}, err
		}
		if data["ops"] == nil {
			data["ops"] = []any{}
		}
		return agent.Result{Block: agent.BlockMemoryOps, Data: data, Usage: parsed.Usage}, nil
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
		return agent.Result{}, fmt.Errorf("claude: 未知任务类型 %q", kind)
	}
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

type processExecer struct{}

// Exec 返回 stdout（stream-json 协议流）与 stderr（诊断通道；claude 的用法类
// 错误在此且退出码 0——"Error: Input must be provided…"，分流返回不合并）。
// dir 为进程工作目录（claude 无 --cwd 等价物）。卡死防御与四家同构：WaitDelay +
// POSIX 进程组击杀（sysproc_posix.go）。
func (p *processExecer) Exec(ctx context.Context, argv []string, env []string, dir string, stdin string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = bytes.NewReader([]byte(stdin))
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	cmd.WaitDelay = 10 * time.Second
	applySysProc(cmd)
	if err := cmd.Start(); err != nil {
		return "", "", -1, err
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), errBuf.String(), ee.ExitCode(), nil
		}
		return out.String(), errBuf.String(), -1, err
	}
	return out.String(), errBuf.String(), 0, nil
}

// wslExecer 把任务交给发行版内执行：wsl.exe -d <distro> --exec env -i K=V...
// [sh -c cd 包装] <argv...>。--exec（v1.30 实证）直 exec 保参数边界；claude
// 无 --cwd 等价物——工作目录经 sh -c 'cd "$1" && shift && exec "$@"' 包装
// （kimi 同构）；stdin（提示词，-p 一等通道）照常流动。
type wslExecer struct {
	distro string
}

// wslArgs 构造 wsl.exe 参数（纯函数，UT 覆盖）。
func wslArgs(distro string, env []string, dir string, argv []string) []string {
	args := make([]string, 0, 10+len(env)+len(argv))
	args = append(args, "-d", distro, "--exec", "env", "-i")
	args = append(args, env...)
	if dir != "" {
		args = append(args, "sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", dir)
	}
	return append(args, argv...)
}

func (w *wslExecer) Exec(ctx context.Context, argv []string, env []string, dir string, stdin string) (string, string, int, error) {
	// 网络配置改取发行版侧（codex 真机复现结论：宿主无代理变量 → 发行版内
	// CLI 直连被墙）。宿主同名键剥除，发行版登录环境白名单键注入。
	env = wslenv.MergeForWSL(env, wslenv.NetEnv(w.distro))
	cmd := exec.CommandContext(ctx, "wsl.exe", wslArgs(w.distro, env, dir, argv)...)
	cmd.Stdin = bytes.NewReader([]byte(stdin))
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	cmd.WaitDelay = 10 * time.Second
	// 已知缺口同其余四家：击杀 wsl.exe 不必然终止发行版内进程——Windows Job
	// Object 属 M2 进程管理项；超时值内任务自行退出为主路径。
	applySysProc(cmd) // Windows：不建控制台窗口（桌面壳防闪框）
	if err := cmd.Start(); err != nil {
		return "", "", -1, err
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), errBuf.String(), ee.ExitCode(), nil
		}
		return out.String(), errBuf.String(), -1, err
	}
	return out.String(), errBuf.String(), 0, nil
}
