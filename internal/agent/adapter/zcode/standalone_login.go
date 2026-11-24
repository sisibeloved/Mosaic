package zcode

// 账号登录任务（独立登录档的设置页入口）：以任务进程同一驱动面执行内核 login 子命令
// （--no-browser——授权链接经事件交给界面，由界面请桌面壳调系统浏览器），逐行解析
// 输出至终态；任务面按实例单飞、事件重放与订阅——形态与 harness 更新任务同构
// （POST 受理即返 + SSE 过程可视化）。令牌只落独立登录档（Config.StandaloneDir），
// 本包不解析也不持有。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// AccountLoginFamilyBigmodel / AccountLoginFamilyZai 登录家族（内核 login 子命令的
	// 位置参数：`login [zai|bigmodel]`）。
	AccountLoginFamilyBigmodel = "bigmodel"
	AccountLoginFamilyZai      = "zai"

	// accountLoginJobTimeout 登录任务上限：内核授权窗口 5 分钟 + 余量。
	accountLoginJobTimeout = 6 * time.Minute
	// standaloneSecretBytes 受管密钥熵（内核要求密钥文件 32–4096 字节）。
	standaloneSecretBytes = 32
	// standaloneResultName 登录结果旁车文件（账号/模型呈现用；无秘密）。
	standaloneResultName = "login-result.json"
	// accountLoginSubBuffer 订阅通道缓冲（登录事件量级个位到十余条）。
	accountLoginSubBuffer = 64
)

// ErrAccountLoginUnsupported 该实例不支持账号登录：非 native 运行面、未配置独立登录档
// 或登录家族不在支持列表。
var ErrAccountLoginUnsupported = errors.New("zcode: 该实例不支持账号登录")

// AccountLoginEvent 登录任务事件（SSE data 载荷）。
type AccountLoginEvent struct {
	Kind    string `json:"kind"` // url（授权链接）| line（过程行）| done（成功终态）| failed（失败终态）| idle（无任务）
	Line    string `json:"line,omitempty"`
	URL     string `json:"url,omitempty"`
	Account string `json:"account,omitempty"` // done：登录账号
	Model   string `json:"model,omitempty"`   // done：内核钉选的默认模型（provider/model）
	Error   string `json:"error,omitempty"`
}

// AccountLoginStatus 独立登录档状态。
type AccountLoginStatus struct {
	Supported bool     `json:"supported"` // 该实例可发起账号登录
	LoggedIn  bool     `json:"logged_in"`
	Account   string   `json:"account,omitempty"` // 最近一次登录的账号
	Model     string   `json:"model,omitempty"`   // 最近一次登录钉选的默认模型
	Providers []string `json:"providers"`         // 已登录的账号 provider
}

// accountLoginSupported 是否支持账号登录：独立登录档已配置且为 native 运行面。
func (a *Adapter) accountLoginSupported() bool {
	return a.cfg.StandaloneDir != "" && a.cfg.WSLDistro == ""
}

// AccountLoginStatus 独立登录档状态（纯文件读取，不起进程）：已登录 = 档内有凭据库且
// 含账号绑定条目；账号/模型取登录结果旁车文件（登录由本任务完成过才有）。
func (a *Adapter) AccountLoginStatus() AccountLoginStatus {
	st := AccountLoginStatus{Supported: a.accountLoginSupported(), Providers: []string{}}
	if !a.accountLoginSupported() {
		return st
	}
	if providers, err := standaloneAccountProviders(a.cfg.StandaloneDir); err == nil && len(providers) > 0 {
		st.LoggedIn, st.Providers = true, providers
	}
	if res, err := readAccountLoginResult(a.cfg.StandaloneDir); err == nil {
		st.Account, st.Model = res.Account, res.Model
	}
	return st
}

// RunAccountLogin 登录任务主体（同步至终态）：备妥登录档（目录 + 受管密钥）→ 以任务
// 进程同一驱动面执行内核 login 子命令 → 逐行解析（授权链接即出即报）→ 终态事件。
// emit 收 url/line 过程事件；返回终态事件（done/failed）。error 仅为本地前置失败
// （不支持/登录档备妥失败/驱动面缺失）——命令本身的失败体现在终态事件里。
func (a *Adapter) RunAccountLogin(ctx context.Context, family string, emit func(AccountLoginEvent)) (AccountLoginEvent, error) {
	if !a.accountLoginSupported() {
		return AccountLoginEvent{}, fmt.Errorf("%w（登录档未配置或非 native 运行面）", ErrAccountLoginUnsupported)
	}
	switch family {
	case "":
		family = AccountLoginFamilyBigmodel
	case AccountLoginFamilyBigmodel, AccountLoginFamilyZai:
	default:
		return AccountLoginEvent{}, fmt.Errorf("%w（登录家族 %q）", ErrAccountLoginUnsupported, family)
	}
	dir, err := a.ensureStandaloneProfile()
	if err != nil {
		return AccountLoginEvent{}, err
	}
	s := &session{adapter: a}
	argv, env, err := s.accountLoginCommand(ctx, family)
	if err != nil {
		return AccountLoginEvent{}, err
	}
	// 登录尚未完成时档内无凭据库——任务进程的 standaloneEnv 不生效，这里显式指向登录档。
	env = append(env, standaloneEnvFor(dir)...)

	out := &accountLoginOutput{}
	line := func(ln string) { out.feed(ln, emit) }
	var stdout, stderr string
	code := -1
	if se, ok := s.execer().(streamExecer); ok {
		stdout, stderr, code, err = se.ExecStream(ctx, argv, env, "", line)
		// stderr 收尾整段返回：错误行（"Error: …"）只提取不呈现——失败原因进终态事件。
		for _, ln := range strings.Split(stderr, "\n") {
			out.feed(ln, nil)
		}
	} else {
		stdout, stderr, code, err = s.execer().Exec(ctx, argv, env, "")
		for _, ln := range strings.Split(stdout+"\n"+stderr, "\n") {
			line(ln)
		}
	}
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return AccountLoginEvent{Kind: "failed", Error: "登录超时：内核未在授权窗口内完成"}, nil
		}
		return AccountLoginEvent{Kind: "failed", Error: "登录进程执行失败：" + err.Error()}, nil
	case code != 0:
		msg := out.lastErr
		if msg == "" {
			msg = fmt.Sprintf("登录命令退出码 %d：%s", code, firstLineOf(strings.TrimSpace(stdout+" "+stderr), 200))
		}
		return AccountLoginEvent{Kind: "failed", Error: msg}, nil
	case out.account == "":
		return AccountLoginEvent{Kind: "failed", Error: "登录命令未报告成功（输出未见账号行）"}, nil
	}
	res := accountLoginResult{Account: out.account, Model: out.model, Family: family, At: time.Now().UTC().Format(time.RFC3339)}
	if raw, merr := json.Marshal(res); merr == nil {
		if werr := os.WriteFile(filepath.Join(dir, standaloneResultName), raw, 0o600); werr != nil {
			return AccountLoginEvent{Kind: "failed", Error: "登录已成功，但写登录结果文件失败：" + werr.Error()}, nil
		}
	}
	return AccountLoginEvent{Kind: "done", Account: out.account, Model: out.model}, nil
}

// accountLoginCommand 登录命令的驱动面（与任务进程同构）：桌面渠道 = 解释器 + 托管
// 缓存 bundle 副本，CLI 渠道 = 可执行本身；环境取各自运行面（HOME/会话库私有化）。
func (s *session) accountLoginCommand(ctx context.Context, family string) (argv, env []string, err error) {
	bundle, err := s.driveBundle(ctx)
	if err != nil {
		return nil, nil, err
	}
	args := []string{"login", family, "--no-browser"}
	if s.adapter.cfg.WorkDir != "" {
		args = append(args, "--cwd", s.adapter.cfg.WorkDir)
	}
	if bundle != "" {
		interp := desktopInterpreter(s.adapter.cfg.ZcodePath, bundle)
		return append([]string{interp, bundle}, args...), s.desktopEnvFor(), nil
	}
	return append([]string{s.adapter.cfg.ZcodePath}, args...), s.envFor(), nil
}

// ensureStandaloneProfile 备妥独立登录档（登录前调用）：目录 0700；受管密钥文件缺失则
// 生成（32 随机字节的 base64——内核要求 32–4096 字节、非 Windows 上属主只读）。
func (a *Adapter) ensureStandaloneProfile() (string, error) {
	dir := a.cfg.StandaloneDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("zcode: 建独立登录档目录 %s 失败：%w", dir, err)
	}
	secret := filepath.Join(dir, standaloneSecretName)
	if _, err := os.Stat(secret); err == nil {
		return dir, nil
	}
	buf := make([]byte, standaloneSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("zcode: 生成受管密钥失败：%w", err)
	}
	if err := os.WriteFile(secret, []byte(base64.StdEncoding.EncodeToString(buf)+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("zcode: 写独立登录档受管密钥 %s 失败：%w", secret, err)
	}
	return dir, nil
}

// accountLoginResult 登录结果旁车文件（状态端点的账号/模型呈现；无秘密）。
type accountLoginResult struct {
	Account string `json:"account"`
	Model   string `json:"model"`
	Family  string `json:"family"`
	At      string `json:"at"`
}

func readAccountLoginResult(dir string) (accountLoginResult, error) {
	var res accountLoginResult
	raw, err := os.ReadFile(filepath.Join(dir, standaloneResultName))
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(raw, &res)
	return res, err
}

// ---- 登录命令输出解析（实证 ZCode 4.0.0 `login <family> --no-browser`）----
//
//	Open this URL to sign in:
//	https://bigmodel.cn/login?appId=zcode&redirect=…&state=…
//	Login successful as <account>.
//	Model: <provider>/<model>
//
// 失败形态（退出码 1，stderr）："Error: Authorization timed out. Please retry login."

var (
	loginURLRe     = regexp.MustCompile(`^https?://\S+$`)
	loginOKRe      = regexp.MustCompile(`Login successful as (.+?)\.?$`)
	loginModelRe   = regexp.MustCompile(`^Model:\s*(\S+)`)
	loginLineNoise = []string{"Open this URL to sign in:"}
)

type accountLoginOutput struct {
	account, model, lastErr string
	sawURL                  bool
}

// feed 解析一行输出：授权链接即出即报（只报一次）；账号/模型/错误行提取进状态；
// 其余非噪声行作为过程行呈现（emit 为 nil 时只提取不呈现——stderr 面）。
func (o *accountLoginOutput) feed(ln string, emit func(AccountLoginEvent)) {
	t := strings.TrimSpace(ln)
	if t == "" {
		return
	}
	if loginURLRe.MatchString(t) {
		if !o.sawURL {
			o.sawURL = true
			if emit != nil {
				emit(AccountLoginEvent{Kind: "url", URL: t})
			}
		}
		return
	}
	if strings.HasPrefix(t, "Error:") {
		o.lastErr = strings.TrimSpace(strings.TrimPrefix(t, "Error:"))
	}
	if m := loginOKRe.FindStringSubmatch(t); m != nil {
		o.account = m[1]
	}
	if m := loginModelRe.FindStringSubmatch(t); m != nil {
		o.model = m[1]
	}
	if emit == nil {
		return
	}
	for _, noise := range loginLineNoise {
		if t == noise {
			return
		}
	}
	emit(AccountLoginEvent{Kind: "line", Line: t})
}

// ---- 流式执行（登录任务：授权链接须在进程结束前交给界面）----

// streamExecer Execer 的流式扩展：stdout 逐行回调；返回值语义同 Exec。登录任务优先
// 走它；Execer 未实现时退化为收尾后整段回放（链接迟至进程结束——仅桩/测试面）。
type streamExecer interface {
	ExecStream(ctx context.Context, argv []string, env []string, stdin string, onLine func(string)) (stdout, stderr string, exitCode int, err error)
}

// ExecStream 流式执行：stdout 经管道逐行回调，stderr 收尾整段返回。两路都是自建
// 管道（*os.File）——不经 os/exec 的内部拷贝协程，主进程一退 Wait 即返（否则孙进程
// 握着写端时 Wait 会空等满 WaitDelay 才报错）。父进程在 Start 后即关闭写端：读侧
// EOF = 子进程侧全部写端关闭。Wait 后给 1 秒宽限让输出自然收尾，未完（残留孙进程
// ——插件宿主）则整组收尾；仍不结束（Windows 无进程组语义）强制关读端。退出码/
// 错误语义同 Exec。
func (p *processExecer) ExecStream(ctx context.Context, argv []string, env []string, stdin string, onLine func(string)) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	applySysProc(cmd)
	prOut, pwOut, err := os.Pipe()
	if err != nil {
		return "", "", -1, err
	}
	prErr, pwErr, err := os.Pipe()
	if err != nil {
		prOut.Close()
		pwOut.Close()
		return "", "", -1, err
	}
	cmd.Stdout, cmd.Stderr = pwOut, pwErr
	if err := cmd.Start(); err != nil {
		prOut.Close()
		pwOut.Close()
		prErr.Close()
		pwErr.Close()
		return "", "", -1, err
	}
	var out, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // stdout：逐行回调
		defer wg.Done()
		defer prOut.Close()
		sc := bufio.NewScanner(prOut)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			out.WriteString(sc.Text())
			out.WriteByte('\n')
			if onLine != nil {
				onLine(sc.Text())
			}
		}
	}()
	go func() { // stderr：整段收集（诊断行——失败原因载体）
		defer wg.Done()
		defer prErr.Close()
		_, _ = io.Copy(&errBuf, prErr)
	}()
	// 父进程不持有写端：子进程退出（含其派生的全部写端）后读侧才见 EOF。
	pwOut.Close()
	pwErr.Close()
	err = cmd.Wait()
	copied := make(chan struct{})
	go func() {
		wg.Wait()
		close(copied)
	}()
	select {
	case <-copied:
	case <-time.After(time.Second):
		killGroup(cmd.Process.Pid) // 残余孙进程握着写端——整组收尾，读侧才见 EOF
		select {
		case <-copied:
		case <-time.After(10 * time.Second):
			prOut.Close()
			prErr.Close()
			<-copied
		}
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), errBuf.String(), ee.ExitCode(), nil
		}
		return out.String(), errBuf.String(), -1, err
	}
	return out.String(), errBuf.String(), 0, nil
}

// ---- 登录任务面（按实例单飞、事件重放与订阅）----

// AccountLoginService 账号登录任务面：resolve 把实例 id 解析为已装配的适配器
// （装配层提供——登录档目录、驱动面等与任务进程同一份配置）。
type AccountLoginService struct {
	mu      sync.Mutex
	jobs    map[string]*accountLoginJob
	resolve func(id string) (*Adapter, bool)
}

// NewAccountLoginService 构造。
func NewAccountLoginService(resolve func(id string) (*Adapter, bool)) *AccountLoginService {
	return &AccountLoginService{resolve: resolve}
}

type accountLoginJob struct {
	mu     sync.Mutex
	events []AccountLoginEvent
	done   bool
	final  AccountLoginEvent
	subs   map[chan AccountLoginEvent]struct{}
}

func (j *accountLoginJob) emit(ev AccountLoginEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return
	}
	j.events = append(j.events, ev)
	for ch := range j.subs {
		select {
		case ch <- ev:
		default: // 慢消费者丢事件不阻塞登录进程（终态事件经重放必达）
		}
	}
}

func (j *accountLoginJob) finish(ev AccountLoginEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return
	}
	j.done, j.final = true, ev
	for ch := range j.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	for ch := range j.subs {
		close(ch)
	}
	j.subs = map[chan AccountLoginEvent]struct{}{}
}

func (j *accountLoginJob) isDone() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

// Status 实例的独立登录档状态；ok=false 表示实例不存在。
func (s *AccountLoginService) Status(id string) (AccountLoginStatus, bool) {
	a, ok := s.resolve(id)
	if !ok {
		return AccountLoginStatus{}, false
	}
	return a.AccountLoginStatus(), true
}

// Start 异步启动登录任务（过程经 Subscribe 事件可视化）。同实例互斥：任务在途返回
// started=false（幂等，不报错）；任务历史保留至下次启动（重放用）。不支持账号登录的
// 实例返回 ErrAccountLoginUnsupported。任务上下文独立于请求生命周期。
func (s *AccountLoginService) Start(ctx context.Context, id, family string) (bool, error) {
	a, ok := s.resolve(id)
	if !ok {
		return false, ErrAccountLoginUnsupported
	}
	if !a.accountLoginSupported() {
		return false, fmt.Errorf("%w（登录档未配置或非 native 运行面）", ErrAccountLoginUnsupported)
	}
	switch family {
	case "", AccountLoginFamilyBigmodel, AccountLoginFamilyZai:
	default:
		return false, fmt.Errorf("%w（登录家族 %q）", ErrAccountLoginUnsupported, family)
	}
	s.mu.Lock()
	if job, ok := s.jobs[id]; ok && !job.isDone() {
		s.mu.Unlock()
		return false, nil // 在途幂等
	}
	job := &accountLoginJob{subs: map[chan AccountLoginEvent]struct{}{}}
	if s.jobs == nil {
		s.jobs = map[string]*accountLoginJob{}
	}
	s.jobs[id] = job
	s.mu.Unlock()
	go func() {
		runCtx, cancel := context.WithTimeout(context.Background(), accountLoginJobTimeout)
		defer cancel()
		final, err := a.RunAccountLogin(runCtx, family, job.emit)
		if err != nil {
			final = AccountLoginEvent{Kind: "failed", Error: err.Error()}
		}
		job.finish(final)
	}()
	return true, nil
}

// Subscribe 订阅登录任务事件：先重放已积累事件，再实时；终态事件后通道关闭。无任务
// （从未启动）返回 ok=false。cancel 必须调用（回收订阅）。
func (s *AccountLoginService) Subscribe(id string) (<-chan AccountLoginEvent, func(), bool) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return nil, nil, false
	}
	ch := make(chan AccountLoginEvent, accountLoginSubBuffer)
	job.mu.Lock()
	// 重放与注册同临界区：快照后新事件必然经通道，无丢失窗口。
	for _, ev := range job.events {
		ch <- ev
	}
	if job.done {
		ch <- job.final
		close(ch)
		job.mu.Unlock()
		return ch, func() {}, true
	}
	job.subs[ch] = struct{}{}
	job.mu.Unlock()
	return ch, func() {
		job.mu.Lock()
		defer job.mu.Unlock()
		if _, ok := job.subs[ch]; ok {
			delete(job.subs, ch)
			close(ch)
		}
	}, true
}
