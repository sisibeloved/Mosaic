// Package zcode：ZCode CLI 适配器（0.3.0 "Agent兼容性扩充" 首员：第四真实适配器）。
// 对接 zcode -p <prompt> --output-format stream-json --mode yolo（实证 2026-09-23，
// zcode 0.16.9 = 桌面版 bundled；开源仓库 zai-org/ZCode 3.14.3 同面）：headless 默认
// yolo 全自动审批，显式传 --mode yolo 钉死；--cwd / --resume <sess_...> 续会话均实证
// 可用。事件 schema 无上游稳定性承诺——fixtures 钉版本，漂移由 conformance 暴露。
// 提示词传输（与 kimi 同约束）：-p 只认 argv（无 --input/stdin 通道）——经
// sh -c 'exec "$@" -p "$(cat)"' 由 stdin 代入 argv：宿主命令行长度恒定，残余物理
// 边界是 Linux MAX_ARG_STRLEN（128KiB/参数），MaxPromptBytes 兜底 fail fast。
// 会话连续性：sessionId 从任意流内事件捕获（sess_*），后续任务以 --resume <id> 恢复
// （实证：跨任务回忆；fixture 见 exec-resume.jsonl 的 session.resumed 事件）。
// 输出面：stream-json = NDJSON 信封 {"eventId","sessionId","seq","timestamp","traceId",
// "turnId","type","payload"} + 顶格终止行 {"type":"result","sessionId","response",
// "usage",...}（无信封字段）；最终正文取 result.response（兜底 turn.completed
// payload.response），usage 取 result.usage.inputTokens/outputTokens。
// 模型覆盖：zcode 无 --model flag、无 API key env——模型只能来自 provider 配置文件
// ~/.zcode/v2/provider_config.json（schemaVersion 1）。实证 0.16.9：env
// ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=<path> 可将该文件重定向到任意路径（指向空
// provider 文件 → "Model creation failed"；指向重排 modelOrder 的副本 → 遥测
// modelId 切换成功）。覆盖实现为 provider 配置 overlay（见 ensureOverlay）——
// 0.16.9 无 defaultModelSelection 字段，headless 选模 = 首个 enabled provider 的
// modelOrder[0]；overlay 同时写 defaultModelSelection 兼容开源 3.14.3 schema。
// 输出分流：机器输出 stdout / 诊断 stderr（与 minimax 同构：退出码错误原因优先级
// 流内 error > stderr 首行 > stdout 首行）。
// 桌面渠道（app:zcode-desktop，无感适配 2026-09-23）：Windows 桌面版（Electron）
// 安装自带同一 CLI 内核（resources/glm/zcode.cjs，实证 = 0.16.9），经
// ELECTRON_RUN_AS_NODE=1 <exe> <bundle> 无头驱动，会话面与 CLI 同 schema。两处
// 垫片：(1) bundle CLI 模式强制定位 <scriptdir>/provider/zcode-builtin.json（源码
// resolveBundledZCodeBuiltinProviderConfig 两候选：scriptdir/provider 与 dev 布局
// 5 级上溯，安装位均不满足）而 stock 安装该文件在 <install>/resources/config/
// provider，且安装目录不宜写（机器级在 Program Files 需管理员，升级即失）——
// ensureDesktopCache
// 把 bundle 与 provider 文件复制进 Mosaic 托管缓存目录（bundle 内容 sha256 前
// 8 位为键，桌面升级自动换目录）再驱动副本；(2) 无感不依赖宿主 sh——提示词 -p
// 直进 argv（CLI 面的 sh "$(cat)" stdin 代入不适用），受 Windows CreateProcess
// 32KiB 命令行物理边界，desktopPromptCap fail fast。env 白名单/凭证定位/模型
// overlay 与 CLI 面同口径（HOME 之外补 USERPROFILE——node os.homedir 主锚点；
// SYSTEMROOT 等 Windows 必需键透传）。
// 桌面 PiP 耦合与隔离（macOS 实证 2026-10-06）：ZCode 桌面版常驻 CUA PiP 悬浮球
// （屏幕边缘小窗），由 GUI 派生的 zcode-cli 镜像进程盯默认会话库激活——该库路径
// 硬编码 homedir()/.zcode/cli/db/db.sqlite（ZCODE_STORAGE_DIR 管不到它），Mosaic
// 任务（经 --cwd 项目绑定会话）写入共享库即被镜像进程观测，悬浮球亮起标题移位，
// 用户看到"ZCode 弹窗"。隔离面：bundle 有官方 env 配置 ZCODE_SESSION_DB_PATH →
// storage.sessionDbPath，把 Mosaic 子进程会话库重定向到 Mosaic 托管私有路径
// （Config.SessionDB）即切断耦合——桌面版历史不再出现 Mosaic 会话；凭据面
// （credentials.json/provider_config.json 锚定 ZCODE_DATA_BASE_DIR，默认
// homedir → ~/.zcode/v2）不受影响，登录态照常共享。
// 4.0.0 实证（2026-11-23，桌面版 3.x→4.0.0 升级后）：桌面渠道驱动链路照常
// （ZCode Helper 承载、缓存副本按 bundle 哈希换目录、内核启动时自刷新内置目录
// ~/.zcode/v2/runtime/provider/…）；模型覆盖对个人清单未列的模型见
// patchProviderConfig 末位回退（服务端目录修订 48 新增 GLM-5.4，个人清单未列）。
// 服务端 529/[1305]（模型访问量过大）时内核按指数退避重试至多 11 次，总时长可超
// 适配器时限——上游容量问题，非适配器缺陷。
// 独立登录档（2026-11-24 实证）：桌面版自身经 OAuth 账号 provider 调模型，而 Mosaic
// 独立驱动的内核原先只能走个人 provider（API Key）——账号 provider 在独立模式要求
// 凭据库里有 account-provider:<id>:identity 等绑定条目（仅内核的独立登录流程写入），
// 且桌面版凭据以系统钥匙串加密，独立内核加载不了 keyring 原生模块（只在 Electron
// asar 内）解不开。按官方无头方案另立一份登录态（Config.StandaloneDir）：
// ZCODE_CREDENTIALS_FILE + ZCODE_CREDENTIAL_SECRET_FILE（受管密钥）。真机对照同窗口：
// API Key 通道 GLM-5.4 17 次全被 529 拒绝，账号通道零重试。登录入口在设置页（zcode
// 实例行"账号登录"块 → AccountLoginService → RunAccountLogin，standalone_login.go）：
// 以 Mosaic 驱动内核的同一方式执行内核的 login 子命令，授权链接经 SSE 交给界面。
// 等价的命令行形态（排障用；受管密钥文件须先存在）：
//
//	ELECTRON_RUN_AS_NODE=1 \
//	ZCODE_CREDENTIALS_FILE=<档>/credentials.json \
//	ZCODE_CREDENTIAL_SECRET_FILE=<档>/credential-secret \
//	"<app>/Contents/Frameworks/ZCode Helper.app/Contents/MacOS/ZCode Helper" \
//	"<app>/Contents/Resources/glm/zcode.cjs" login bigmodel --no-browser
//
// 命令打印授权链接，浏览器确认后凭据写入档内（设置页入口自动备妥目录与受管密钥
// ——32 随机字节的 base64、属主只读；内核要求 32–4096 字节），并在
// ~/.zcode/v2/provider_config.json 追加 defaultModelSelection
// 指向账号 provider（内核登录流程的既有行为，个人规则不变）。令牌有效期与续期
// 未验证，过期后重新登录即可。桌面版凭据（~/.zcode/v2/credentials.json）不被读写。
package zcode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/wslenv"
)

// Config 适配器配置。
type Config struct {
	ZcodePath string        // 可执行路径（来自 harness 注册表）
	WorkDir   string        // 工作目录（zcode 有 --cwd，无需 sh cd 包装）
	ExtraArgs []string      // 预留（per-Profile 覆盖）
	Timeout   time.Duration // 单任务超时（默认 120s）
	Execer    Execer        // 测试注入；nil 用真实进程执行
	// WSL 运行面（Windows 宿主）：非空 = ZcodePath 是发行版内 Linux 路径，
	// 经 wsl.exe -d <WSLDistro> --exec 包装执行（v1.30 实证：`--` 拼接经默认
	// shell 解释会毁参数；--exec 直 exec 保参数边界，stdin 照通——sh $(cat)
	// 提示词代入在发行版内完成，与 kimi 同路径）。
	WSLDistro string
	WSLHome   string // 发行版内 HOME（登录态/配置在 $HOME/.zcode/v2——实证双凭证面）
	// Bundle 桌面渠道（app:zcode-desktop）内嵌 agent bundle 安装路径（注册表
	// Executable.Bundle；空 = CLI 面）。非空时经托管缓存副本 + ELECTRON_RUN_AS_NODE
	// 驱动（见包注释桌面渠道段）。桌面渠道只在 native 运行面（Windows 安装位）。
	Bundle string
	// CacheRoot 桌面渠道托管缓存根目录（app 装配传入 Mosaic 缓存位；缺省 = 桌面
	// 驱动不可用，首任务显式报错——静默回退 CLI 面会打错可执行形态）。
	CacheRoot string
	// EvalModel 评估任务专用模型（空 = 与生成同模型）。dogfood 性能治理：
	// 评估输出仅几十 token，评估可降档、生成保持主模型。zcode 无 --model
	// flag——生效经 provider 配置 overlay（ensureOverlay）。
	EvalModel string
	// Model 模型覆盖（空 = CLI 默认 = 首个 enabled provider 的 modelOrder[0]，
	// 尊重 CLI 侧既有选择）。值必须是某 enabled provider 模型清单内的 modelId
	//（如 glm-4.6），否则任务报错（不静默回落）。
	Model string
	// MaxPromptBytes 提示词物理上限（默认 100KiB）：提示词经 sh "$(cat)" 代入
	// zcode argv，受 Linux MAX_ARG_STRLEN（128KiB/参数）约束——留边际 fail fast，
	// 胜过 OS exec 失败（物理边界而非策略闸，与 kimi 同口径）。
	MaxPromptBytes int
	// SessionDB 私有会话库路径（空 = 不隔离，沿用默认共享库，保持旧行为）。
	// 非空时经 ZCODE_SESSION_DB_PATH 重定向会话存储（CLI/桌面/WSL 三面同注）——
	// 桌面 PiP 耦合隔离用，见包注释末节。
	SessionDB string
	// StandaloneDir ZCode 独立登录档目录（空 = 不启用；仅 native 运行面——档内
	// 文件经宿主 os 包直读）。非空且档内有登录态（credentials.json）时，任务进程
	// 以 ZCODE_CREDENTIALS_FILE/ZCODE_CREDENTIAL_SECRET_FILE 指向该档：内核独立
	// 运行时访问不了系统钥匙串（keyring 原生模块只在 Electron asar 内），按官方
	// 无头方案以受管密钥承载一份独立登录态——OAuth 账号 provider 由此可选，桌面版
	// ~/.zcode/v2 的凭据不被读写。登录方式见包注释。
	StandaloneDir string
}

// Execer 进程执行抽象（UT 捕获/阻塞；生产为真实 zcode 子进程）。
type Execer interface {
	Exec(ctx context.Context, argv []string, env []string, stdin string) (stdout, stderr string, exitCode int, err error)
}

// Adapter 实现 agent.Adapter。
type Adapter struct {
	cfg Config
	// 桌面渠道托管缓存记忆（ensureDesktopCache 成功路径；失败不缓存，任务级重试）。
	mu          sync.Mutex
	bundleCache string
}

// New 构造。
func New(cfg Config) *Adapter {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxPromptBytes <= 0 {
		cfg.MaxPromptBytes = 100 * 1024
	}
	return &Adapter{cfg: cfg}
}

// Name 适配器名。
func (a *Adapter) Name() string { return "zcode" }

// Capabilities 能力声明（RFC-0002 §3.1.2）。
func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:      false, // stream-json 是转录事件流而非增量草稿（同 codex/kimi/minimax 面）
		CancelMode:     "interrupt",
		HistoryChannel: "structured_request",
		Continuity:     true, // --resume <sess_...>
		UsageReporting: true, // result.usage（含 cache/reasoning 细分）
		Observe:        false,
		TaskRuns:       true, // M4-1：exec 进程可由 Mosaic 托管为长任务（结果回传房间）
		ReplyOrPass:    true, // M4-3：单次 reply-or-pass（限定路径）
		MemoryCuration: true, // v1.70：每波记忆评审（Hermes 同构自助策展）
	}
}

// Boot 建立逻辑会话（无进程：zcode -p 按任务拉起，会话身份 = sessionId）。
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
		return nil, fmt.Errorf("zcode: 不支持 observe（Capabilities.Observe=false）")
	}
	h := &handle{done: make(chan struct{})}
	taskCtx, cancel := context.WithTimeout(ctx, s.adapter.cfg.Timeout)
	h.cancel = cancel
	go s.execute(ctx, taskCtx, task, h)
	return h, nil
}

func (s *session) Cancel(string) {}
func (s *session) Close()        {}

// execute 单任务执行：构建提示词 → sh -c 'exec "$@" -p "$(cat)"' 包装（提示词走
// stdin，zcode -p 不读 stdin 由 sh 代入 argv）→ zcode stream-json（--resume 连续性）
// → 解析 → 映射；严格 JSON 契约任务在输出形状失败时做一次矫正重试。ctx 为父
// 会话 ctx（引擎关停取消面），taskCtx 为带 Timeout 的任务 ctx——两者分流超时/
// 取消语义（见下）。
func (s *session) execute(ctx, taskCtx context.Context, task agent.Task, h *handle) {
	defer close(h.done)

	prompt, err := buildPrompt(task)
	if err != nil {
		h.err = err
		return
	}
	// 提示词物理上限（fail fast 胜过 OS exec 失败）：CLI 面 = MaxPromptBytes
	// （Linux MAX_ARG_STRLEN 128KiB/参数边界，默认 100KiB 留边际）；桌面面直
	// argv 仅在 Windows 受 CreateProcess 32KiB 命令行边界（2026-11-21 真机实证：
	// darwin 桌面面被该 Windows 边界无差别拦下——36722 字节研究型刺激 fail fast，
	// 而 darwin ARG_MAX 1MiB 远未触界；goruntimeOS 守卫后 darwin/linux 桌面面
	// 与 CLI 面同边界）。
	limit, boundary := s.adapter.cfg.MaxPromptBytes, "Linux MAX_ARG_STRLEN 128KiB"
	if s.adapter.cfg.Bundle != "" && goruntimeOS == "windows" && limit > desktopPromptCap {
		limit, boundary = desktopPromptCap, "Windows CreateProcess 32KiB 命令行"
	}
	if n := len(prompt); n > limit {
		h.err = fmt.Errorf("zcode: 提示词超单参数物理上限（%d > %d 字节，%s 边界）：缩小上下文", n, limit, boundary)
		return
	}

	parsed, err := s.execOnce(taskCtx, task, prompt)
	if err != nil {
		// 超时与取消分流（2026-11-23 真机实证：上游 529/[1305] 限流时内核按指数
		// 退避重试至多 11 次、总时长超适配器时限，此前报 ErrStale——"stale grant"
		// 的围栏失配假象掩盖真实死因；ErrStale 专属 Cancel 后迟到拒绝语义
		// RFC-0002 §3.1.6）。h.Cancel 取消 taskCtx 时同样落入超时分支，但 Result
		// 以 stale 优先返回 ErrStale——取消语义不受分流影响。
		if taskCtx.Err() != nil && ctx.Err() == nil {
			h.err = timeoutError(s.adapter.cfg.Timeout, err)
			return
		}
		if taskCtx.Err() != nil {
			h.stale = true // 引擎取消/关停：迟到拒绝语义不变
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

// execOnce 单次 zcode -p 全链：argv 组装（会话连续性 + 双面分支）→ 模型覆盖
// overlay → 执行 → stream 解析 → 执行级错误检查（退出码/turn.failed/空输出）。
// 映射级错误（mapResult）留给调用方。双面：CLI 面 sh "$(cat)" stdin 代入提示词
// （宿主命令行恒定，kimi 同构）；桌面面直 argv（native Windows 无 sh 依赖——
// 无感不假设 Git Bash，提示词 -p 直进参数，desktopPromptCap fail fast）。
func (s *session) execOnce(taskCtx context.Context, task agent.Task, prompt string) (Parsed, error) {
	s.mu.Lock()
	sessID := s.sessID
	s.mu.Unlock()

	bundle, err := s.driveBundle(taskCtx)
	if err != nil {
		return Parsed{}, err
	}
	args := []string{s.adapter.cfg.ZcodePath, "--output-format", "stream-json", "--mode", "yolo"}
	if bundle != "" {
		// 桌面驱动：exe 以 node 语义执行缓存 bundle（argv[1]），其后与 CLI 同参。
		// darwin 以内嵌 Helper 二进制承载（desktopbin_darwin.go——主二进制会注册
		// 主 bundle 致 Dock 弹出第二个 ZCode，2026-10-07 真机实证）。
		interp := desktopInterpreter(s.adapter.cfg.ZcodePath, bundle)
		args = append([]string{interp, bundle}, args[1:]...)
	}
	if s.adapter.cfg.WorkDir != "" {
		args = append(args, "--cwd", s.adapter.cfg.WorkDir)
	}
	if sessID != "" {
		args = append(args, "--resume", sessID) // 与 --cwd 可共存（实证）
	}
	args = append(args, s.adapter.cfg.ExtraArgs...)

	var argv []string
	var stdin string
	var env []string
	if bundle != "" {
		argv = append(args, "-p", prompt)
		env = s.desktopEnvFor()
	} else {
		// 提示词传输（kimi 同构）：stdin + sh "$(cat)" 代入——宿主命令行恒定；
		// $(cat) 剥除尾换行无害。
		argv = append([]string{"sh", "-c", `exec "$@" -p "$(cat)"`, "sh"}, args...)
		env = s.envFor()
		stdin = prompt
	}
	if model := s.effectiveModel(task); model != "" {
		// 模型覆盖：zcode 无 --model flag——provider 配置 overlay + env 重定向
		// （实证 0.16.9：ZCODE_PERSONAL_PROVIDER_CONFIG_FILE 重定向生效）。
		overlay, err := s.ensureOverlay(taskCtx, model)
		if err != nil {
			return Parsed{}, err
		}
		env = append(env, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE="+overlay)
	}

	stdout, stderr, code, err := s.execer().Exec(taskCtx, argv, env, stdin)
	if err != nil {
		return Parsed{}, &execError{err: err, stdout: stdout}
	}
	parsed := ParseStream([]byte(stdout))
	if parsed.SessionID != "" && parsed.SessionID != sessID {
		s.mu.Lock()
		s.sessID = parsed.SessionID
		s.mu.Unlock()
	}
	if code != 0 {
		// 退出码错误的原因优先级（minimax 同构）：流内 error 事件 > stderr 首行 >
		// stdout 首行——2026-09-23 实证空 provider 文件时退出码 1、真因
		// "Model creation failed" 在流内 turn.failed。
		if parsed.Err != "" {
			return parsed, fmt.Errorf("zcode: zcode 退出码 %d：turn failed: %s", code, parsed.Err)
		}
		if msg := firstLineOf(stderr, 200); msg != "" {
			return parsed, fmt.Errorf("zcode: zcode 退出码 %d：%s", code, msg)
		}
		return parsed, fmt.Errorf("zcode: zcode 退出码 %d：%s", code, firstLineOf(stdout, 200))
	}
	if parsed.Err != "" {
		return parsed, fmt.Errorf("zcode: turn failed: %s", parsed.Err)
	}
	if len(parsed.Messages) == 0 {
		return parsed, fmt.Errorf("zcode: 无 result 正文输出")
	}
	return parsed, nil
}

// retryStrictTask 矫正重试（一次，仅严格 JSON 契约任务）：minimax v1.69 真机实证
// 先例同构——会话历史含强待办语境时模型可能把仲裁元任务当作房间任务执行（评估
// 提示词下直接交付正文，无 JSON 可提取）。矫正追记附在原提示词之后（同会话：
// 模型可见自己的跑偏输出），恢复该座位；仍失败则保留末次错误并附输出首行，
// 排障不再依赖 CLI 侧转录。
func (s *session) retryStrictTask(taskCtx context.Context, task agent.Task, prompt string, firstErr error, first Parsed) (agent.Result, error) {
	second, err := s.execOnce(taskCtx, task, prompt+correctiveNote(task.Kind))
	if err != nil {
		if taskCtx.Err() != nil {
			return agent.Result{}, err // 重试中取消/超时：无结果可发布，错误即语义
		}
		return agent.Result{}, fmt.Errorf("zcode: %v；矫正重试执行失败：%v", firstErr, err)
	}
	res, mapErr := mapResult(task.Kind, second)
	if mapErr != nil {
		return agent.Result{}, fmt.Errorf("zcode: %v；重试后仍不可解析（模型输出首行：%s）",
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
// reply_or_pass 为中文指令面——追记跟随各自指令语言（minimax 逐字同源）。
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

// sanitizePublish 发布边界：委托端口级共享门 agent.PublishGate（与 codex/kimi/minimax 同一套门）。
func (h *handle) sanitizePublish() {
	body, _ := h.result.Data["body"].(string)
	clean, rels, err := agent.PublishGate(body, h.result.Data["declared_relations"])
	if err != nil {
		h.err = fmt.Errorf("zcode: %w", err)
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

// effectiveModel 目标任务生效模型（runtimeArgs 语义与 minimax 对齐）：评估任务
// 优先级 EvalModel > Model > CLI 默认（评估降档不被主模型覆盖吞掉）。非空时
// 经 provider 配置 overlay 生效（zcode 无 --model flag）。
func (s *session) effectiveModel(task agent.Task) string {
	model := s.adapter.cfg.Model
	if task.Kind == agent.KindEvaluateIntent && s.adapter.cfg.EvalModel != "" {
		model = s.adapter.cfg.EvalModel
	}
	return model
}

// homeDir 运行面 HOME（native 用宿主家目录；WSL 用发行版内 HOME）。native 侧
// UserHomeDir 优先于 HOME 环境变量：Windows 宿主常无 HOME，且从 Git Bash 类壳
// 启动时 HOME 可能是 POSIX 形（/c/Users/…）——文件路径构造需原生形态。
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

// envFor 按运行面构造子进程环境（native 用宿主 HOME；wsl 用发行版内 HOME）。
func (s *session) envFor() []string {
	env := zcodeEnvWithHome(s.adapter.cfg.ZcodePath, s.homeDir())
	env = append(env, sessionDBEnv(s.adapter.cfg.SessionDB)...)
	return append(env, s.standaloneEnv()...)
}

// sessionDBEnv 会话库私有化注入（三面共用）：空 = 不隔离（默认共享库，旧行为）；
// 非空 = ZCODE_SESSION_DB_PATH 重定向会话存储——桌面版 CUA PiP 悬浮球的镜像进程
// 只盯默认库（homedir()/.zcode/cli/db/db.sqlite 硬编码），私有库内的 Mosaic 会话
// 不再激活桌面悬浮球（实证 2026-10-06，见包注释末节）。
func sessionDBEnv(sessionDB string) []string {
	if sessionDB == "" {
		return nil
	}
	return []string{"ZCODE_SESSION_DB_PATH=" + sessionDB}
}

// passthroughNetEnv 网络配置透传白名单（两运行面共用）：代理/CA 从宿主透传
// （与 codex/kimi/minimax 同口径：网络配置非凭据——OQ-20 禁的是持有凭证与代理
// 流量）；不传 API key；其余不透传。
// ZCODE_HTTP_PROXY 是 zcode 官方代理键（0.16.9 实证生效：标准 http_proxy 系
// 变量其 HTTP 客户端不理会）——WSL 直连运营商出口不稳时，走宿主代理是
// 重试风暴（maxAttempts 11 吃满任务超时）的唯一稳定解。
func passthroughNetEnv() []string {
	var env []string
	for _, key := range []string{
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"ZCODE_HTTP_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
	} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// desktopEnvFor 桌面渠道（native Windows）子进程环境：ELECTRON_RUN_AS_NODE=1
// （Electron 以 node 语义执行 bundle——桌面驱动核心开关，实证 2026-09-23）+
// HOME/USERPROFILE 双锚（node os.homedir 走 USERPROFILE 定位 .zcode/v2 凭证；
// HOME 兜底自定路径读取）+ Windows 必需系统键（SYSTEMROOT——node winsock 初始
// 化依赖；TEMP/TMP）+ PATH（exe 目录前置，其余沿用宿主）+ 网络透传白名单同
// CLI 面。最小集真机实证：会话建立、provider 解析、模型请求均通。
func (s *session) desktopEnvFor() []string {
	home := s.homeDir()
	env := []string{
		"ELECTRON_RUN_AS_NODE=1",
		"HOME=" + home,
		"USERPROFILE=" + home,
	}
	for _, key := range []string{"SYSTEMROOT", "SYSTEMDRIVE", "TEMP", "TMP", "APPDATA", "LOCALAPPDATA"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	dir := filepath.Dir(s.adapter.cfg.ZcodePath)
	env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = append(env, sessionDBEnv(s.adapter.cfg.SessionDB)...)
	env = append(env, s.standaloneEnv()...)
	return append(env, passthroughNetEnv()...)
}

// ---- 独立登录档（OAuth 账号 provider 的承载面；见 Config.StandaloneDir）----

const (
	standaloneCredentialsName = "credentials.json"
	standaloneSecretName      = "credential-secret"
	// 独立登录档凭据库里"账号 provider 已登录"的键名形态（只读键名——值保持加密，
	// 适配器不解读也不持有任何令牌）。
	standaloneAccountPrefix  = "account-provider:"
	standaloneIdentitySuffix = ":identity"
)

// standaloneProfile 独立登录档目录与登录态：未配置或档内无 credentials.json
// （尚未登录/已登出）= 未启用，行为与无此功能时逐字一致。
func (a *Adapter) standaloneProfile() (dir string, ok bool) {
	if a.cfg.StandaloneDir == "" {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(a.cfg.StandaloneDir, standaloneCredentialsName)); err != nil {
		return "", false
	}
	return a.cfg.StandaloneDir, true
}

// standaloneEnv 独立登录档进程环境（CLI/桌面两面同注）：凭据库与受管密钥文件。
func (s *session) standaloneEnv() []string {
	dir, ok := s.adapter.standaloneProfile()
	if !ok {
		return nil
	}
	return standaloneEnvFor(dir)
}

// standaloneEnvFor 指向给定独立登录档的进程环境（登录任务在登录完成前也用它——
// 此时档内尚无凭据库，standaloneEnv 不生效）。
func standaloneEnvFor(dir string) []string {
	return []string{
		"ZCODE_CREDENTIALS_FILE=" + filepath.Join(dir, standaloneCredentialsName),
		"ZCODE_CREDENTIAL_SECRET_FILE=" + filepath.Join(dir, standaloneSecretName),
	}
}

// standaloneAccounts 独立登录档里已登录的账号 provider。未启用返回空；档内凭据库
// 读不到/解析失败 → 显式报错（已配置的登录态不该静默失效）。
func (s *session) standaloneAccounts() ([]string, error) {
	dir, ok := s.adapter.standaloneProfile()
	if !ok {
		return nil, nil
	}
	accounts, err := standaloneAccountProviders(dir)
	if err != nil {
		return nil, fmt.Errorf("zcode: %w", err)
	}
	return accounts, nil
}

// standaloneAccountProviders 独立登录档里已登录的账号 provider（按凭据库键名
// account-provider:<providerId>:identity 识别——只读键名，值保持加密）。凭据库读不到/
// 解析失败时错误信息带路径（调用方按面包装）。
func standaloneAccountProviders(dir string) ([]string, error) {
	path := filepath.Join(dir, standaloneCredentialsName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取独立登录档凭据库 %s 失败：%w", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("独立登录档凭据库 %s 解析失败：%v", path, err)
	}
	var out []string
	for key := range doc {
		if strings.HasPrefix(key, standaloneAccountPrefix) && strings.HasSuffix(key, standaloneIdentitySuffix) {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(key, standaloneAccountPrefix), standaloneIdentitySuffix))
		}
	}
	sort.Strings(out)
	return out, nil
}

// zcodeEnvWithHome 环境构造（CLI 面，native 与 WSL 共用，仅 HOME 来源不同）：
// PATH 前置可执行目录；登录态/配置在 $HOME/.zcode/v2（实证 credentials.json /
// provider_config.json 双凭证面），HOME 即锚点；网络透传见 passthroughNetEnv。
func zcodeEnvWithHome(zcodePath, home string) []string {
	if home == "" {
		home = "/root"
	}
	dir := zcodePath
	if i := strings.LastIndex(zcodePath, "/"); i > 0 {
		dir = zcodePath[:i]
	}
	env := []string{
		"PATH=" + dir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
	}
	return append(env, passthroughNetEnv()...)
}

// ---- 桌面渠道驱动（app:zcode-desktop：ELECTRON_RUN_AS_NODE + 托管缓存副本）----

// desktopPromptCap 桌面面提示词上限（字节）：提示词 -p 直进 argv（无 sh stdin
// 代入），Windows CreateProcess 命令行物理边界 32767 UTF-16 字符（exe+全部参数+
// 引号转义共享）——UTF-8 字节数 ≥ 同文本 UTF-16 单元数，按字节计量保守，留 ~2.7KiB
// 边际兜转义膨胀，fail fast 胜过 CreateProcess 静默截断/失败。
const desktopPromptCap = 30000

// goruntimeOS 单测可替换面（harness.update 同源）：桌面面降限仅 Windows 生效，
// darwin/linux 桌面面沿用 CLI 面边界。
var goruntimeOS = runtime.GOOS

// desktopProviderStockRel / desktopProviderScriptRel provider 文件定位的两候选
// （bundle 源码 resolveBundledZCodeBuiltinProviderConfig 同构）：stock 安装自带
// <resources>/config/provider/zcode-builtin.json（实证 md5 与可驱动副本一致；
// 相对 bundle <install>/resources/glm/ 上溯一级）；scriptdir 候选是定位已满足
// 的安装（如手工放置）。
const (
	desktopProviderStockRel  = "../config/provider/zcode-builtin.json"
	desktopProviderScriptRel = "provider/zcode-builtin.json"
	desktopProviderCacheName = "zcode-builtin.json"
)

// driveBundle 解析本任务的驱动面：空 = CLI 面（sh 包装 stdin 提示词）；非空 =
// 桌面面（返回应进 argv[1] 的 bundle 路径——托管缓存副本，非安装原始路径）。
// 缓存成功进程内记忆（14MB 级复制不做每任务重复）；失败不记忆，任务级重试。
func (s *session) driveBundle(ctx context.Context) (string, error) {
	if s.adapter.cfg.Bundle == "" {
		return "", nil
	}
	s.adapter.mu.Lock()
	cached := s.adapter.bundleCache
	s.adapter.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	cached, err := s.adapter.ensureDesktopCache(ctx)
	if err != nil {
		return "", err
	}
	s.adapter.mu.Lock()
	s.adapter.bundleCache = cached
	s.adapter.mu.Unlock()
	return cached, nil
}

// ensureDesktopCache 桌面渠道托管缓存（幂等）：安装 bundle 复制到
// <CacheRoot>/<sha256 前 8 hex>/zcode.cjs，旁边 provider/zcode-builtin.json——
// bundle CLI 模式强制定位 <scriptdir>/provider/zcode-builtin.json（stock 安装
// 放 <root>/config/provider，安装目录不宜写：机器级在 Program Files 需管理员、
// 升级即失）。缓存键 = bundle 内容哈希：桌面升级自动换目录，旧版本目录保留
// （回滚即用，14MB/版本量级不清理）。写入走 tmp+rename 原子化（半写副本不被
// 后续快路径误判完整）。文件 IO 直用 os 包——桌面渠道只在 native 运行面，
// 无 WSL 路径翻译问题（CLI 面的 execer 跑 sh 约定不适用）。
func (a *Adapter) ensureDesktopCache(ctx context.Context) (string, error) {
	if a.cfg.CacheRoot == "" {
		return "", fmt.Errorf("zcode: 桌面渠道未配置托管缓存目录（装配缺 CacheRoot）——安装面注册不完整")
	}
	raw, err := os.ReadFile(a.cfg.Bundle)
	if err != nil {
		return "", fmt.Errorf("zcode: 桌面渠道读安装 bundle %s 失败：%w", a.cfg.Bundle, err)
	}
	sum := sha256.Sum256(raw)
	dir := filepath.Join(a.cfg.CacheRoot, hex.EncodeToString(sum[:4]))
	cached := filepath.Join(dir, "zcode.cjs")
	prov := filepath.Join(dir, "provider", desktopProviderCacheName)
	if _, err := os.Stat(cached); err == nil {
		if _, err := os.Stat(prov); err == nil {
			return cached, nil // 快路径：两件齐备（写入原子化保证齐备即完整）
		}
	}

	// provider 文件来源：stock resources/config/provider 优先（安装自带），
	// scriptdir/provider 回退（定位已满足的安装）；均缺 = 布局漂移，显式报错不静默。
	scriptDir := filepath.Dir(a.cfg.Bundle)
	src := filepath.Join(scriptDir, desktopProviderStockRel)
	if _, err := os.Stat(src); err != nil {
		alt := filepath.Join(scriptDir, desktopProviderScriptRel)
		if _, err := os.Stat(alt); err != nil {
			return "", fmt.Errorf("zcode: 桌面渠道定位 %s 失败（%s 与 %s 均不存在——安装布局漂移）：重装 ZCode 桌面版或改登记 CLI 渠道",
				desktopProviderCacheName, src, alt)
		}
		src = alt
	}
	provRaw, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("zcode: 桌面渠道读 %s 失败：%w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(prov), 0o700); err != nil {
		return "", fmt.Errorf("zcode: 桌面渠道建缓存目录 %s 失败：%w", dir, err)
	}
	if err := writeAtomic(cached, raw); err != nil {
		return "", fmt.Errorf("zcode: 桌面渠道写缓存 bundle %s 失败：%w", cached, err)
	}
	if err := writeAtomic(prov, provRaw); err != nil {
		return "", fmt.Errorf("zcode: 桌面渠道写缓存 provider 配置 %s 失败：%w", prov, err)
	}
	return cached, nil
}

// writeAtomic 同目录 tmp 写入 + rename（半写文件不出现在最终路径上）。
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- 模型覆盖：provider 配置 overlay（zcode 无 --model flag 的对应实现）----

// providerConfigRel 基座 provider 配置文件（相对 HOME；schemaVersion 1）。
const providerConfigRel = ".zcode/v2/provider_config.json"

// ensureOverlay 生成模型覆盖 overlay 并返回其路径（每次 exec 幂等重写——内容同
// 模型恒定，2KB 级无缓存必要）：读基座 provider_config.json → 把目标模型所在
// provider 的 personalModelIds/modelOrder 重排为目标在最前 + 写
// defaultModelSelection（兼容开源 3.14.3 schema；0.16.9 选模 = 首个 enabled
// provider 的 modelOrder[0]）→ 写基座同目录 provider_config.mosaic.<model>.json。
// 文件 IO：WSL 面（发行版内路径）经 execer 跑 sh；native 面（CLI/桌面同）直用
// os 包——桌面渠道无 sh 依赖（无感适配不假设 Git Bash）。基座读不到/解析失败/
// 模型不在任何 provider 清单 → 任务报错（信息带路径，不静默回落 CLI 默认——
// 显式覆盖必须生效或显式失败）。
func (s *session) ensureOverlay(taskCtx context.Context, model string) (string, error) {
	base := s.homeDir() + "/" + providerConfigRel
	var raw []byte
	if s.adapter.cfg.WSLDistro != "" {
		out, _, code, err := s.execer().Exec(taskCtx, []string{"sh", "-c", `cat "$1"`, "sh", base}, s.envFor(), "")
		if err != nil {
			return "", fmt.Errorf("zcode: 模型覆盖读取 provider 基座配置 %s 失败：%w", base, err)
		}
		if code != 0 {
			return "", fmt.Errorf("zcode: 模型覆盖读取 provider 基座配置 %s 失败（cat 退出码 %d——文件不存在或不可读）", base, code)
		}
		raw = []byte(out)
	} else {
		b, err := os.ReadFile(filepath.FromSlash(base))
		if err != nil {
			return "", fmt.Errorf("zcode: 模型覆盖读取 provider 基座配置 %s 失败：%w", base, err)
		}
		raw = b
	}
	accounts, err := s.standaloneAccounts()
	if err != nil {
		return "", err
	}
	patched, err := patchProviderConfig(raw, model, accounts)
	if err != nil {
		return "", fmt.Errorf("zcode: 模型覆盖处理 provider 基座配置 %s 失败：%w", base, err)
	}
	overlay := s.homeDir() + "/.zcode/v2/provider_config.mosaic." + sanitizeModelName(model) + ".json"
	if s.adapter.cfg.WSLDistro != "" {
		_, stderr, code, err := s.execer().Exec(taskCtx, []string{"sh", "-c", `cat > "$1"`, "sh", overlay}, s.envFor(), string(patched))
		if err != nil {
			return "", fmt.Errorf("zcode: 模型覆盖写入 overlay %s 失败：%w", overlay, err)
		}
		if code != 0 {
			return "", fmt.Errorf("zcode: 模型覆盖写入 overlay %s 失败（cat 退出码 %d：%s）", overlay, code, firstLineOf(stderr, 200))
		}
		return overlay, nil
	}
	if err := os.WriteFile(filepath.FromSlash(overlay), patched, 0o600); err != nil {
		return "", fmt.Errorf("zcode: 模型覆盖写入 overlay %s 失败：%w", overlay, err)
	}
	return overlay, nil
}

// patchProviderConfig 重排目标模型所在 provider 的模型清单并钉选默认模型
// （纯函数，UT 覆盖）。找规则：config.providerConfigRules.providerRules 里
// enabled != false 且 config.personalModelIds 含目标模型者；找不到则回退
// config.modelConfigRules.providerModelRules 里 modelId 匹配的 providerId（声明
// 了归属但无对应规则 → 报错，不落到别的 provider）；再找不到走末位回退——先看
// 独立登录档（standaloneAccounts：已登录的账号 provider 唯一时把默认模型选择指向
// 它，多个则显式报错），否则配置对该模型一无所知且启用规则唯一时落其上（模型随
// ZCode 服务端目录更新，个人清单未列 ≠ 不可用；可用与否由内核/服务端裁决，见
// 函数内注释），启用规则不唯一则显式报错。未知键原样保留（map 往返）。
func patchProviderConfig(raw []byte, model string, standaloneAccounts []string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("JSON 解析失败：%v", err)
	}
	config, _ := doc["config"].(map[string]any)
	rules, _ := config["providerConfigRules"].(map[string]any)
	providerRules, _ := rules["providerRules"].([]any)

	findIn := func(rule map[string]any) bool {
		rcfg, _ := rule["config"].(map[string]any)
		ids, _ := rcfg["personalModelIds"].([]any)
		for _, id := range ids {
			if s, _ := id.(string); s == model {
				return true
			}
		}
		return false
	}
	ruleEnabled := func(rule map[string]any) bool {
		enabled, ok := rule["enabled"].(bool)
		return !ok || enabled // 键缺失 = enabled（实证 0.16.9 本机文件形态）
	}

	var target map[string]any
	for _, r := range providerRules {
		rule, _ := r.(map[string]any)
		if rule == nil || !ruleEnabled(rule) {
			continue
		}
		if findIn(rule) {
			target = rule
			break
		}
	}
	if target == nil {
		// 回退：modelConfigRules.providerModelRules 里 modelId 匹配 → providerId
		declared := ""
		modelRules, _ := config["modelConfigRules"].(map[string]any)
		pmRules, _ := modelRules["providerModelRules"].([]any)
		for _, r := range pmRules {
			entry, _ := r.(map[string]any)
			if mid, _ := entry["modelId"].(string); mid == model {
				declared, _ = entry["providerId"].(string)
				break
			}
		}
		for _, r := range providerRules {
			rule, _ := r.(map[string]any)
			if pid, _ := rule["providerId"].(string); declared != "" && pid == declared {
				target = rule
				break
			}
		}
		if target == nil && declared != "" {
			return nil, fmt.Errorf("模型 %s 在 zcode 配置中归属 provider %s，但无对应 provider 规则", model, declared)
		}
	}
	if target == nil && len(standaloneAccounts) > 0 {
		// 末位回退之一：独立登录档的账号 provider（OAuth）。2026-11-24 真机实证 ZCode
		// 4.0.0：个人规则保持不动、仅把 defaultModelSelection 指向已登录的账号 provider
		// 即生效（模型 ID 按用户输入原样写入，内核自行归一）；可用与否由内核/服务端
		// 裁决。已登录的账号 provider 不唯一则归属不可判定——显式报错不猜。
		if len(standaloneAccounts) != 1 {
			return nil, fmt.Errorf("模型 %s 不在 zcode 任何 provider 的模型清单，且独立登录档已登录的账号 provider 有 %d 个（%s），无法判定归属",
				model, len(standaloneAccounts), strings.Join(standaloneAccounts, "、"))
		}
		config["defaultModelSelection"] = map[string]any{"providerId": standaloneAccounts[0], "modelId": model}
		return json.Marshal(doc)
	}
	if target == nil {
		// 末位回退（2026-11-23 真机实证 ZCode 4.0.0）：配置对该模型一无所知 ≠ 不可用
		// ——模型随服务端内置目录更新（目录修订 48 新增 GLM-5.4），个人清单只在用户
		// 于 ZCode 选模时落盘。启用规则唯一时落其上（moveToFront 前置插入使其可选），
		// 可用与否由内核/服务端裁决：可用则生效，不存在则显式报错（实证 [1211]
		// 模型不存在），无静默回落。启用规则不唯一则归属不可判定——显式报错不猜。
		var enabledIDs []string
		var enabled map[string]any
		for _, r := range providerRules {
			rule, _ := r.(map[string]any)
			if rule == nil || !ruleEnabled(rule) {
				continue
			}
			pid, _ := rule["providerId"].(string)
			enabledIDs = append(enabledIDs, pid)
			enabled = rule
		}
		if len(enabledIDs) != 1 {
			msg := fmt.Sprintf("模型 %s 不在 zcode 任何 provider 的模型清单", model)
			if len(enabledIDs) > 1 {
				msg += fmt.Sprintf("，且启用中的 provider 有 %d 个（%s），无法判定归属：在 ZCode 中把该模型加入对应 provider 的个人清单",
					len(enabledIDs), strings.Join(enabledIDs, "、"))
			}
			return nil, errors.New(msg)
		}
		target = enabled
	}

	providerID, _ := target["providerId"].(string)
	rcfg, _ := target["config"].(map[string]any)
	if rcfg == nil {
		rcfg = map[string]any{}
		target["config"] = rcfg
	}
	rcfg["personalModelIds"] = moveToFront(rcfg["personalModelIds"], model)
	rcfg["modelOrder"] = moveToFront(rcfg["modelOrder"], model)
	config["defaultModelSelection"] = map[string]any{"providerId": providerID, "modelId": model}
	return json.Marshal(doc)
}

// moveToFront 字符串清单把目标值排到最前（缺失则前置插入——回退路径下
// personalModelIds 本就不含目标模型，overlay 必须使其可选）。
func moveToFront(list any, model string) []any {
	out := []any{model}
	if ids, ok := list.([]any); ok {
		for _, id := range ids {
			if s, _ := id.(string); s != model {
				out = append(out, id)
			}
		}
	}
	return out
}

// sanitizeModelName overlay 文件名的模型名安全化（非 [a-zA-Z0-9._-] 换 _）。
func sanitizeModelName(model string) string {
	var b strings.Builder
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
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

// ---- stream-json 解析（fixtures 钉 0.16.9 schema；信封键 camelCase 为实证形状）----

// Parsed 一次 zcode -p 的解析结果。
type Parsed struct {
	SessionID string   // 任意流内事件的 sessionId（sess_*，连续性句柄）
	Messages  []string // 最终正文：result.response（兜底 turn.completed payload.response）
	Usage     *agent.Usage
	Err       string // turn.failed 的错误信息
}

type streamEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	// result 终止行（顶格，无信封字段）：response + usage + eventCount + projection
	Response string `json:"response"`
	Usage    *struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
	} `json:"usage"`
	// 信封事件载荷（turn.completed / turn.failed / model.streaming / ...）
	Payload *struct {
		Kind     string `json:"kind"` // model.streaming：text_delta/reasoning_delta/...（转录面，不入 Messages）
		Response string `json:"response"`
		Usage    *struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"usage"`
		Message string `json:"message"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"payload"`
}

// ParseStream 解析 zcode -p --output-format stream-json 的 JSONL 输出；
// 非 JSON 行忽略（防御性：stderr 混入等噪声，与 minimax 同口径）。
func ParseStream(raw []byte) Parsed {
	var out Parsed
	var fallback string // turn.completed payload.response（result 行缺失时兜底）
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
			// 终止行（顶格无信封）：最终正文与 usage 的权威来源
			if ev.Response != "" {
				out.Messages = []string{ev.Response}
			}
			if ev.Usage != nil {
				out.Usage = &agent.Usage{
					InputTokens:  ev.Usage.InputTokens,
					OutputTokens: ev.Usage.OutputTokens,
					Model:        "zcode",
				}
			}
		case "turn.completed":
			if ev.Payload != nil {
				if ev.Payload.Response != "" {
					fallback = ev.Payload.Response
				}
				if out.Usage == nil && ev.Payload.Usage != nil {
					out.Usage = &agent.Usage{
						InputTokens:  ev.Payload.Usage.InputTokens,
						OutputTokens: ev.Payload.Usage.OutputTokens,
						Model:        "zcode",
					}
				}
			}
		case "turn.failed":
			if ev.Payload != nil {
				switch {
				case ev.Payload.Error != nil && ev.Payload.Error.Message != "":
					out.Err = ev.Payload.Error.Message
				case ev.Payload.Message != "":
					out.Err = ev.Payload.Message
				}
			}
		}
	}
	if len(out.Messages) == 0 && fallback != "" {
		out.Messages = []string{fallback}
	}
	return out
}

// execError 执行级错误（进程未能正常收尾），携带已捕获的部分 stdout——超时被杀
// 时流里可能只有模型重试事件，是排障的唯一现场（见 retryDetail）。错误串与原
// fmt.Errorf("zcode: exec: %w") 逐字一致，errors.Is/As 语义经 Unwrap 保留。
type execError struct {
	err    error
	stdout string
}

func (e *execError) Error() string { return "zcode: exec: " + e.err.Error() }
func (e *execError) Unwrap() error { return e.err }

// timeoutError 超时的如实报错：时限 + 流内重试现场（有则附）。实证 2026-11-23：
// GLM-5.4 接口 529/[1305] 限流持续三分钟，内核 9 次重试后的退避总时长越过 180s
// 时限——此前日志只见 "stale grant"，真因要去翻内核结构化日志。
func timeoutError(limit time.Duration, cause error) error {
	msg := fmt.Sprintf("zcode: 任务超时（%s）：内核未在时限内完成", limit)
	var pe *execError
	if errors.As(cause, &pe) {
		if detail := retryDetail(pe.stdout); detail != "" {
			msg += "——" + detail
		}
	}
	return errors.New(msg)
}

// retryEnvelope stream-json 的 model_retry_scheduled 事件（payload 字段，实证
// ZCode 4.0.0）。
type retryEnvelope struct {
	Payload struct {
		Type        string `json:"type"`
		Attempt     int    `json:"attempt"`
		MaxAttempts int    `json:"maxAttempts"`
		Message     string `json:"message"`
		ModelID     string `json:"modelId"`
		Reason      string `json:"reason"`
	} `json:"payload"`
}

// retryDetail 流内模型重试现场摘要：取尝试序号最大的一条——"模型 X 请求已失败
// N 次（上限 M），最近一次：<原因>"；无重试事件返回空串。
func retryDetail(stdout string) string {
	var last *retryEnvelope
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, "model_retry_scheduled") {
			continue // 廉价预筛：流以千行计，仅重试事件行含该子串
		}
		var ev retryEnvelope
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil || ev.Payload.Type != "model_retry_scheduled" {
			continue
		}
		if last == nil || ev.Payload.Attempt >= last.Payload.Attempt {
			last = &ev
		}
	}
	if last == nil {
		return ""
	}
	p := last.Payload
	detail := fmt.Sprintf("模型 %s 请求已失败 %d 次", p.ModelID, p.Attempt)
	if p.MaxAttempts > 0 {
		detail += fmt.Sprintf("（上限 %d 次）", p.MaxAttempts)
	}
	if msg := firstLineOf(p.Message, 160); msg != "" {
		detail += "，最近一次：" + msg
	}
	return detail
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

// ---- 提示词与结果映射（与 codex/kimi/minimax 同一措辞——四适配器狗粮口径一致）----

const intentInstruction = `You are a participant in an ongoing group chat. You have just observed the latest messages. Decide whether to reply; staying silent is a valid, often good choice — reply only when you have something to add.
Most messages in a busy group chat do not need YOUR reply — silence is the norm, not the exception. Speak when: you are directly addressed or asked; the topic is squarely your expertise; you can correct an error or add substance others missed. If your_activity shows you spoke recently, hold back unless addressed — let others take the floor.
Reply with ONLY a JSON object, no prose, no code fences:
{"action":"speak|react|fork|summarize|silent","type":"answer|extend|challenge|support|question|redirect|synthesize","public_rationale":"<=280 chars","scores":{"relevance":0.0-1.0,"novelty":0.0-1.0,"urgency":0.0-1.0,"confidence":0.0-1.0}}
This is an internal arbitration request, not a message to the room: never answer, perform, or start the discussion's tasks here — the only valid reply is the JSON decision above.`

const generateInstruction = `You are a participant in an ongoing group chat and have decided to reply.
Write your chat message directly below — concise, conversational, addressed to the room (no speeches, no meta commentary).
You are on a hard wall-clock budget for this turn (about 2 minutes, including any tool use): tools are fine for quick lookups, but keep to a few calls and finish within the budget — if deeper research would run over, answer from what you already have.
Reply with ONLY a JSON object, no prose, no code fences:
{"body":"your public message","declared_relations":[]}
The JSON must contain your public chat message in "body" — never an arbitration decision (action/silent/scores) or any other internal JSON.
If (and only if) the discussion clearly refers to earlier context you cannot see, reply {"history_query":"search phrase"} instead of a body — you will receive the matching room messages and retry once.
When you create or edit a document, narrate the change in "body" and add "doc_ops" in the same JSON (at most 8 ops): {"op":"create","title":"...","blocks":[{"type":"heading|paragraph|list|code|quote|hr","text":"..."}]} | {"op":"append","doc_id":"doc_...","blocks":[...]} | {"op":"replace_section","doc_id":"doc_...","heading":"<exact heading text>","blocks":[...]} | {"op":"insert_after","doc_id":"doc_...","block_id":"blk_...","blocks":[...]} | {"op":"delete_section","doc_id":"doc_...","heading":"<exact heading text>"}. Document excerpts in the context carry [block_id] prefixes; a section is a heading plus the blocks up to the next heading. Never emit doc_ops without explaining the change in "body". For heading blocks prefer "level" (1..6, default 2) over hand-written "#" prefixes; for code blocks prefer "lang" (e.g. "go") over hand-written fences. When you create or edit a spreadsheet, narrate the change in "body" and add "sheet_ops" in the same JSON (at most 8 ops): {"op":"create","title":"...","cells":[{"r":1,"c":1,"type":"text|number|bool|formula|null","text":"..."|...}]} | {"op":"add_sheet","doc_id":"doc_...","base_version":N,"name":"..."} | {"op":"set_cells","doc_id":"doc_...","base_version":N,"sheet_id":"sht_... (empty = first sheet)","cells":[...]} | {"op":"clear_range","doc_id":"doc_...","base_version":N,"range":{"r1":1,"c1":1,"r2":10,"c2":5}} | {"op":"insert_rows|insert_columns|delete_rows|delete_columns","doc_id":"doc_...","base_version":N,"at":N,"count":N}. Edit ops MUST carry the current "base_version" (the v-number in the spreadsheet excerpt header) and explicit coordinates. Formulas (e.g. "SUM(A1:B3)*2") are stored verbatim and evaluated by the server; supported functions: SUM, AVERAGE, MIN, MAX, COUNT, COUNTA, IF, ROUND. Never emit sheet_ops without explaining the change in "body".`

// reviewInstruction 每波记忆评审（v1.70，Hermes background_review 同构——
// 措辞四家同源；容量门在引擎侧，提示词只强调"先查清单再动笔"的策展纪律）。
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
		return "", fmt.Errorf("zcode: 未知任务类型 %q", task.Kind)
	}
}

// mapResult 任务类型 → 端口结果块。校验口径与 codex/kimi/minimax 对齐（最小结构
// 检查 + 域层严格门：畸形 intent 由引擎弃权记录 R-01，不在适配器层变成任务失败）。
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
			return agent.Result{}, fmt.Errorf("zcode: reply_or_pass 缺字段 action（speak|pass）")
		}
		if action == "speak" {
			body, _ := data["body"].(string)
			if strings.TrimSpace(body) == "" {
				return agent.Result{}, fmt.Errorf("zcode: reply_or_pass speak 缺正文")
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
			return agent.Result{}, fmt.Errorf("zcode: turn_intent 缺字段 %q", "action")
		}
		if action != "silent" {
			for _, field := range []string{"type", "scores"} {
				if _, ok := data[field]; !ok {
					return agent.Result{}, fmt.Errorf("zcode: turn_intent 缺字段 %q", field)
				}
			}
		}
		agent.CapIntentRationale(data) // 超长 rationale 截断保决策（IT 实证防回归）
		return agent.Result{Block: agent.BlockTurnIntent, Data: data, Usage: parsed.Usage}, nil
	case agent.KindGenerate:
		// 封闭 DTO 投影：模型输出只投影已知字段进 message.posted 载荷（附加键不透传）。
		// JSON 而无可用 body = 决策/内部件误入生成位（v1.69 狗粮实证：silent 意图 JSON
		// 被散文回退原样发布进房间正文）——拒绝发布，不冒充发言；例外是自报
		// history_query（v1.70 两段式检索，引擎携结果重发）；纯散文回复仍走回退。
		// v1.76：JSON 形状但解析失败（真机实证 body 值内裸引号致原文冒充发言）
		// 先宽松引号修复重试；仍失败拒绝发布。
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
				return agent.Result{}, fmt.Errorf("zcode: generate 输出为 JSON 但缺可用 body（不发布）：%s", firstLineOf(text, 120))
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
			return agent.Result{}, fmt.Errorf("zcode: generate 输出呈 JSON 形状但解析失败（不发布）：%s", firstLineOf(text, 120))
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
		return agent.Result{}, fmt.Errorf("zcode: 未知任务类型 %q", kind)
	}
}

type processExecer struct{}

// Exec 返回 stdout（stream-json 协议流）与 stderr（诊断通道——退出码非零且流内
// 无 error 事件时是唯一真因载体，minimax 2026-09-23 登录过期实证同构；分流返回
// 不合并）。卡死防御与 codex/kimi/minimax 同构：WaitDelay + POSIX 进程组击杀
// （sysproc_posix.go）。
func (p *processExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, string, int, error) {
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
		return "", "", -1, err
	}
	err := cmd.Wait()
	// 正常结束同样整组收尾：插件宿主孙进程不随主进程退出（sysproc_posix.go
	// killGroup 注释——残留宿主重派子进程即桌面壳弹窗根因，2026-10-07 真机实证）。
	defer killGroup(cmd.Process.Pid)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), errBuf.String(), ee.ExitCode(), nil
		}
		return out.String(), errBuf.String(), -1, err
	}
	return out.String(), errBuf.String(), 0, nil
}

// wslExecer 把任务交给发行版内执行：wsl.exe -d <distro> --exec env -i K=V... <argv...>。
// --exec（v1.30 实证）：`--` 剩余参数经发行版默认 shell 解释（引号剥除/元字符执行），
// 直 exec 保参数边界；stdin（提示词，经 sh $(cat) 代入 argv）照常流动。env -i 口径
// 与 codex/kimi/minimax 一致。
type wslExecer struct {
	distro string
}

// wslArgs 构造 wsl.exe 参数（纯函数，UT 覆盖）。zcode 自带 --cwd——argv 直传；
// 提示词不在 argv（sh $(cat) 包装在发行版内完成代入）。
func wslArgs(distro string, env []string, argv []string) []string {
	args := make([]string, 0, 5+len(env)+len(argv))
	args = append(args, "-d", distro, "--exec", "env", "-i")
	args = append(args, env...)
	return append(args, argv...)
}

func (w *wslExecer) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, string, int, error) {
	// 网络配置改取发行版侧（同 codex 真机复现结论：宿主无代理变量 → 发行版内
	// CLI 直连被墙）。宿主同名键剥除，发行版登录环境白名单键注入。
	env = wslenv.MergeForWSL(env, wslenv.NetEnv(w.distro))
	cmd := exec.CommandContext(ctx, "wsl.exe", wslArgs(w.distro, env, argv)...)
	cmd.Stdin = bytes.NewReader([]byte(stdin))
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	cmd.WaitDelay = 10 * time.Second
	// 已知缺口同 codex/kimi/minimax：击杀 wsl.exe 不必然终止发行版内进程——
	// Windows Job Object 属 M2 进程管理项；超时值内任务自行退出为主路径。
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
