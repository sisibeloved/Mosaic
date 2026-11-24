package zcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// loginStreamFake 登录命令流式桩：按序逐行回调 stdout（可阻塞于闸门——任务面时序
// 控制），随后返回预设 stderr/退出码/错误。记录 argv/env 供断言。
type loginStreamFake struct {
	argv, env []string
	stdout    []string
	stderr    string
	code      int
	err       error
	gate      chan struct{} // 非空时回调完 stdout 行后等闸门关闭才返回
}

func (f *loginStreamFake) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, string, int, error) {
	return f.exec(argv, env, nil)
}

func (f *loginStreamFake) ExecStream(ctx context.Context, argv []string, env []string, stdin string, onLine func(string)) (string, string, int, error) {
	return f.exec(argv, env, onLine)
}

func (f *loginStreamFake) exec(argv, env []string, onLine func(string)) (string, string, int, error) {
	f.argv, f.env = argv, env
	var out strings.Builder
	for _, ln := range f.stdout {
		out.WriteString(ln + "\n")
		if onLine != nil {
			onLine(ln)
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	return out.String(), f.stderr, f.code, f.err
}

const loginTestURL = "https://bigmodel.cn/login?appId=zcode&redirect=https://zcode.z.ai/api/v1/oauth/cli/callback/bigmodel&state=60825da4"

func newLoginTestAdapter(t *testing.T, exec Execer) (*Adapter, string) {
	t.Helper()
	prof := filepath.Join(t.TempDir(), "zcode-standalone")
	return New(Config{ZcodePath: "/bin/zcode", StandaloneDir: prof, Execer: exec, Timeout: 30 * time.Second}), prof
}

// TestRunAccountLoginSuccess：登录成功全链——授权链接即出即报（首事件）、账号/模型
// 提取进终态、过程行不含引导噪声；命令为内核 login 子命令（--no-browser，家族默认
// bigmodel）；环境指向独立登录档且受管密钥自动备妥；登录结果落旁车文件。输出形态
// 钉 2026-11-24 真机实证（ZCode 4.0.0）。
func TestRunAccountLoginSuccess(t *testing.T) {
	fake := &loginStreamFake{stdout: []string{
		"Open this URL to sign in:", loginTestURL,
		"Login successful as alice.",
		"Model: account:bigmodel-individual-coding-plan/GLM-5.4",
		"Credentials: /x/zcode-standalone/credentials.json",
	}}
	adapter, prof := newLoginTestAdapter(t, fake)
	var events []AccountLoginEvent
	final, err := adapter.RunAccountLogin(context.Background(), "", func(ev AccountLoginEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final.Kind != "done" || final.Account != "alice" || final.Model != "account:bigmodel-individual-coding-plan/GLM-5.4" {
		t.Fatalf("终态应为 done 携账号与模型：%+v", final)
	}
	if len(events) == 0 || events[0] != (AccountLoginEvent{Kind: "url", URL: loginTestURL}) {
		t.Fatalf("首事件应为授权链接：%+v", events)
	}
	for _, ev := range events[1:] {
		if ev.Kind != "line" || ev.Line == "Open this URL to sign in:" {
			t.Fatalf("后续事件应为过程行且不含引导噪声：%+v", ev)
		}
	}
	if want := []string{"/bin/zcode", "login", "bigmodel", "--no-browser"}; !reflect.DeepEqual(fake.argv, want) {
		t.Fatalf("argv = %v，期望 %v", fake.argv, want)
	}
	for _, want := range []string{"ZCODE_CREDENTIALS_FILE=" + filepath.Join(prof, standaloneCredentialsName),
		"ZCODE_CREDENTIAL_SECRET_FILE=" + filepath.Join(prof, standaloneSecretName)} {
		if !hasEnv(fake.env, want) {
			t.Fatalf("登录进程 env 应携 %s：%v", want, fake.env)
		}
	}
	secret, err := os.ReadFile(filepath.Join(prof, standaloneSecretName))
	if err != nil || len(secret) < 32 {
		t.Fatalf("受管密钥应自动生成且 ≥32 字节：%v len=%d", err, len(secret))
	}
	if st, _ := os.Stat(filepath.Join(prof, standaloneSecretName)); st.Mode().Perm() != 0o600 {
		t.Fatalf("受管密钥应属主只读：%v", st.Mode().Perm())
	}
	res, err := readAccountLoginResult(prof)
	if err != nil || res.Account != "alice" || res.Family != AccountLoginFamilyBigmodel {
		t.Fatalf("登录结果旁车文件：%+v %v", res, err)
	}
}

// TestRunAccountLoginFamilyZai：登录家族透传为位置参数（zai）。
func TestRunAccountLoginFamilyZai(t *testing.T) {
	fake := &loginStreamFake{stdout: []string{loginTestURL, "Login successful as a.", "Model: p/m"}}
	adapter, _ := newLoginTestAdapter(t, fake)
	if _, err := adapter.RunAccountLogin(context.Background(), AccountLoginFamilyZai, func(AccountLoginEvent) {}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.argv[2] != "zai" {
		t.Fatalf("家族应透传：%v", fake.argv)
	}
}

// TestRunAccountLoginFailureKeepsReason：授权超时（退出码 1，错误行在 stderr——收尾
// 整段返回）→ 终态 failed 携内核原话；授权链接已在途报出（界面可复述现场）。
func TestRunAccountLoginFailureKeepsReason(t *testing.T) {
	fake := &loginStreamFake{
		stdout: []string{"Open this URL to sign in:", loginTestURL},
		stderr: "Error: Authorization timed out. Please retry login.\n",
		code:   1,
	}
	adapter, _ := newLoginTestAdapter(t, fake)
	var events []AccountLoginEvent
	final, err := adapter.RunAccountLogin(context.Background(), "", func(ev AccountLoginEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final.Kind != "failed" || final.Error != "Authorization timed out. Please retry login." {
		t.Fatalf("终态应为 failed 携内核原话：%+v", final)
	}
	if len(events) != 1 || events[0].Kind != "url" {
		t.Fatalf("stderr 错误行只提取不呈现；过程事件仅授权链接：%+v", events)
	}
}

// TestRunAccountLoginExitWithoutErrorLine：非零退出且无 Error 行 → 退出码 + 输出首行。
func TestRunAccountLoginExitWithoutErrorLine(t *testing.T) {
	fake := &loginStreamFake{stdout: []string{"something odd"}, code: 2}
	adapter, _ := newLoginTestAdapter(t, fake)
	final, err := adapter.RunAccountLogin(context.Background(), "", func(AccountLoginEvent) {})
	if err != nil || final.Kind != "failed" || !strings.Contains(final.Error, "退出码 2") || !strings.Contains(final.Error, "something odd") {
		t.Fatalf("终态应携退出码与输出首行：%+v %v", final, err)
	}
}

// TestRunAccountLoginUnsupported：未配置独立登录档 / 家族不在支持列表 → 本地前置失败
// （不起进程）。
func TestRunAccountLoginUnsupported(t *testing.T) {
	fake := &loginStreamFake{}
	if _, err := New(Config{ZcodePath: "/bin/zcode", Execer: fake}).RunAccountLogin(context.Background(), "", func(AccountLoginEvent) {}); !errors.Is(err, ErrAccountLoginUnsupported) {
		t.Fatalf("未配置登录档应 ErrAccountLoginUnsupported：%v", err)
	}
	adapter, _ := newLoginTestAdapter(t, fake)
	if _, err := adapter.RunAccountLogin(context.Background(), "openai", func(AccountLoginEvent) {}); !errors.Is(err, ErrAccountLoginUnsupported) {
		t.Fatalf("家族不合法应 ErrAccountLoginUnsupported：%v", err)
	}
	if fake.argv != nil {
		t.Fatalf("前置失败不得起进程：%v", fake.argv)
	}
}

// TestAccountLoginStatus：状态为纯文件读取——支持性由登录档配置决定；已登录 = 档内
// 凭据库含账号绑定条目；账号/模型取登录结果旁车文件。
func TestAccountLoginStatus(t *testing.T) {
	if st := New(Config{ZcodePath: "/bin/zcode"}).AccountLoginStatus(); st.Supported || st.Providers == nil {
		t.Fatalf("未配置登录档：supported=false 且 providers 非 nil：%+v", st)
	}
	adapter, prof := newLoginTestAdapter(t, &loginStreamFake{})
	if st := adapter.AccountLoginStatus(); !st.Supported || st.LoggedIn || len(st.Providers) != 0 || st.Account != "" {
		t.Fatalf("支持但未登录：%+v", st)
	}
	writeStandaloneCredentials(t, prof, "account:bigmodel-individual-coding-plan")
	if st := adapter.AccountLoginStatus(); !st.LoggedIn || !reflect.DeepEqual(st.Providers, []string{"account:bigmodel-individual-coding-plan"}) || st.Account != "" {
		t.Fatalf("已登录（无旁车文件）：logged_in 且 providers 列出账号 provider、账号为空：%+v", st)
	}
	if err := os.WriteFile(filepath.Join(prof, standaloneResultName), []byte(`{"account":"carol","model":"acct/m","family":"bigmodel","at":"2026-11-24T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := adapter.AccountLoginStatus(); st.Account != "carol" || st.Model != "acct/m" {
		t.Fatalf("账号/模型应取旁车文件：%+v", st)
	}
}

// writeStandaloneCredentials 在给定登录档备一个只含账号绑定键名的凭据库（值随意
// ——适配器只读键名）。
func writeStandaloneCredentials(t *testing.T, dir, account string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, standaloneCredentialsName),
		[]byte(`{"`+standaloneAccountPrefix+account+standaloneIdentitySuffix+`":"enc:v2:env:opaque"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAccountLoginServiceLifecycle：任务面——未知实例/不支持的家族显式失败；同实例
// 在途幂等；订阅先重放后实时、终态后关流；终态后仍可重放全过程。
func TestAccountLoginServiceLifecycle(t *testing.T) {
	gate := make(chan struct{})
	fake := &loginStreamFake{stdout: []string{loginTestURL, "Login successful as bob.", "Model: acct/m"}, gate: gate}
	adapter, _ := newLoginTestAdapter(t, fake)
	svc := NewAccountLoginService(func(id string) (*Adapter, bool) {
		return adapter, id == "z1"
	})
	ctx := context.Background()
	if _, err := svc.Start(ctx, "nope", ""); !errors.Is(err, ErrAccountLoginUnsupported) {
		t.Fatalf("未知实例应显式失败：%v", err)
	}
	if _, err := svc.Start(ctx, "z1", "openai"); !errors.Is(err, ErrAccountLoginUnsupported) {
		t.Fatalf("家族不合法应显式失败：%v", err)
	}
	if _, _, ok := svc.Subscribe("z1"); ok {
		t.Fatal("从未启动的任务不应可订阅")
	}
	started, err := svc.Start(ctx, "z1", "bigmodel")
	if err != nil || !started {
		t.Fatalf("start：%v started=%v", err, started)
	}
	if again, _ := svc.Start(ctx, "z1", "bigmodel"); again {
		t.Fatal("在途任务应幂等（started=false）")
	}
	ch, cancel, ok := svc.Subscribe("z1")
	if !ok {
		t.Fatal("在途任务应可订阅")
	}
	if first := <-ch; first.Kind != "url" || first.URL != loginTestURL {
		t.Fatalf("首事件应为授权链接：%+v", first)
	}
	close(gate)
	var got []AccountLoginEvent
	for ev := range ch {
		got = append(got, ev)
	}
	cancel()
	if len(got) != 3 || got[2].Kind != "done" || got[2].Account != "bob" {
		t.Fatalf("闸门后应收过程行与终态并关流：%+v", got)
	}
	// 终态后重放全过程
	ch2, cancel2, ok := svc.Subscribe("z1")
	if !ok {
		t.Fatal("终态后仍应可重放")
	}
	var replay []AccountLoginEvent
	for ev := range ch2 {
		replay = append(replay, ev)
	}
	cancel2()
	if len(replay) != 4 || replay[0].Kind != "url" || replay[3].Kind != "done" {
		t.Fatalf("重放应为 url + 过程行 + 终态：%+v", replay)
	}
	if st, ok := svc.Status("z1"); !ok || !st.Supported {
		t.Fatalf("status：%+v ok=%v", st, ok)
	}
	if _, ok := svc.Status("nope"); ok {
		t.Fatal("未知实例 status 应 ok=false")
	}
}

// TestProcessExecerStreamLines：真实子进程流式执行——行陆续到达（非收尾整段）、stderr
// 收尾返回、非零退出码透传。
func TestProcessExecerStreamLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh 桩；Windows 面无进程组语义，另行覆盖")
	}
	var got []string
	var at []time.Time
	stdout, stderr, code, err := (&processExecer{}).ExecStream(context.Background(),
		[]string{"sh", "-c", `printf 'a\n'; sleep 0.4; printf 'b\n'; printf 'err\n' >&2; exit 3`}, nil, "",
		func(ln string) { got = append(got, ln); at = append(at, time.Now()) })
	if err != nil || code != 3 || stdout != "a\nb\n" || stderr != "err\n" {
		t.Fatalf("流式执行结果：out=%q err=%q code=%d err=%v", stdout, stderr, code, err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("应逐行回调：%v", got)
	}
	// 第二行在首行 ≥0.3s 之后才到——证明是边执行边回调，而非收尾整段
	if d := at[1].Sub(at[0]); d < 300*time.Millisecond {
		t.Fatalf("行应陆续到达（实测间隔 %v）", d)
	}
}

// TestProcessExecerStreamGrandchildHoldsPipe：主进程退出而后台孙进程仍握着写端——
// 整组收尾后读侧解除阻塞，不悬挂（登录任务不得因残留进程卡死）。
func TestProcessExecerStreamGrandchildHoldsPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX 进程组击杀")
	}
	done := make(chan struct{})
	var stdout string
	var code int
	var err error
	go func() {
		defer close(done)
		stdout, _, code, err = (&processExecer{}).ExecStream(context.Background(),
			[]string{"sh", "-c", `echo hi; (sleep 30) & exit 0`}, nil, "", nil)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("孙进程握着管道写端时 ExecStream 悬挂")
	}
	if err != nil || code != 0 || stdout != "hi\n" {
		t.Fatalf("out=%q code=%d err=%v", stdout, code, err)
	}
}
