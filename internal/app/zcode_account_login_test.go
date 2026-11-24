// zcode 账号登录面装配 UT：宿主侧路径与配置拼装（任务进程与登录任务同一份——
// zcodeWiring/zcodeConfigFor），登录任务面到 httpapi 边界的适配（状态/错误映射、事件
// 转发与关流——zcodeAccountLoginBackend）。
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent/adapter/zcode"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi"
)

func TestZcodeWiringPaths(t *testing.T) {
	cache, sessionDB, standalone := zcodeWiring(filepath.Join("Caches", "mosaic", "agent-work"))
	if want := filepath.Join("Caches", "mosaic", "zcode-desktop"); cache != want {
		t.Fatalf("托管缓存根 = %q，期望 %q", cache, want)
	}
	if want := filepath.Join("Caches", "mosaic", "zcode-session", "db.sqlite"); sessionDB != want {
		t.Fatalf("私有会话库 = %q，期望 %q", sessionDB, want)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无家目录")
	}
	if want := filepath.Join(home, ".mosaic", "zcode-standalone"); standalone != want {
		t.Fatalf("独立登录档 = %q，期望 %q", standalone, want)
	}
}

func TestZcodeConfigFor(t *testing.T) {
	exe := harness.Executable{Adapter: "zcode", Path: "/x/ZCode", Bundle: "/x/zcode.cjs",
		Distro: "Ubuntu", Model: "glm-5.4", EvalModel: "k3"}
	native := zcodeConfigFor(exe, "/w", "", "/cache", "/sess/db.sqlite", "/standalone")
	if native.StandaloneDir != "/standalone" || native.SessionDB != "/sess/db.sqlite" ||
		native.CacheRoot != "/cache" || native.WorkDir != "/w" || native.Bundle != "/x/zcode.cjs" ||
		native.Model != "glm-5.4" || native.EvalModel != "k3" || native.WSLDistro != "" {
		t.Fatalf("native 面配置不符：%+v", native)
	}
	wsl := zcodeConfigFor(exe, "/w", "/home/u/", "/cache", "/sess/db.sqlite", "/standalone")
	if wsl.StandaloneDir != "" {
		t.Fatalf("WSL 面不接登录档：%+v", wsl)
	}
	if wsl.WSLDistro != "Ubuntu" || wsl.WSLHome != "/home/u/" ||
		wsl.SessionDB != "/home/u/.mosaic/zcode-session/db.sqlite" {
		t.Fatalf("WSL 面配置不符：%+v", wsl)
	}
}

// loginStub 登录命令流式桩（zcode.streamExecer 的结构化实现——接口名未导出，方法集
// 全导出，跨包可满足）：逐行回调后等闸门关闭才返回。
type loginStub struct {
	gate chan struct{}
}

func (s *loginStub) Exec(ctx context.Context, argv []string, env []string, stdin string) (string, string, int, error) {
	return s.ExecStream(ctx, argv, env, stdin, nil)
}

func (s *loginStub) ExecStream(_ context.Context, _ []string, _ []string, _ string, onLine func(string)) (string, string, int, error) {
	for _, ln := range []string{
		"Open this URL to sign in:",
		"https://bigmodel.cn/login?appId=zcode&state=glue",
	} {
		if onLine != nil {
			onLine(ln)
		}
	}
	<-s.gate
	if onLine != nil {
		onLine("Login successful as dave.")
		onLine("Model: account:bigmodel-individual-coding-plan/GLM-5.4")
	}
	return "", "", 0, nil
}

func TestZcodeAccountLoginBackendGlue(t *testing.T) {
	stub := &loginStub{gate: make(chan struct{})}
	adapter := zcode.New(zcode.Config{ZcodePath: "/bin/zcode",
		StandaloneDir: filepath.Join(t.TempDir(), "zcode-standalone"), Execer: stub})
	svc := zcode.NewAccountLoginService(func(id string) (*zcode.Adapter, bool) {
		return adapter, id == "z"
	})
	backend := zcodeAccountLoginBackend{svc: svc}
	ctx := context.Background()

	if st, ok := backend.AccountLoginStatus("other"); ok {
		t.Fatalf("未知实例应 ok=false：%+v", st)
	}
	st, ok := backend.AccountLoginStatus("z")
	if !ok || !st.Supported || st.LoggedIn || len(st.Providers) != 0 {
		t.Fatalf("z 实例应支持且未登录：%+v ok=%v", st, ok)
	}
	if _, err := backend.StartAccountLogin(ctx, "other", ""); !errors.Is(err, httpapi.ErrAccountLoginUnsupported) {
		t.Fatalf("未知实例应映射为边界不支持错误：%v", err)
	}
	if _, err := backend.StartAccountLogin(ctx, "z", "openai"); !errors.Is(err, httpapi.ErrAccountLoginUnsupported) {
		t.Fatalf("家族不合法应映射为边界不支持错误：%v", err)
	}
	if _, _, ok := backend.SubscribeAccountLogin("z"); ok {
		t.Fatal("从未启动的任务不应可订阅")
	}

	started, err := backend.StartAccountLogin(ctx, "z", "zai")
	if err != nil || !started {
		t.Fatalf("start：%v started=%v", err, started)
	}
	events, cancel, ok := backend.SubscribeAccountLogin("z")
	if !ok {
		t.Fatal("在途任务应可订阅")
	}
	defer cancel()
	select {
	case ev := <-events:
		if ev.Kind != "url" || ev.URL != "https://bigmodel.cn/login?appId=zcode&state=glue" {
			t.Fatalf("首事件应为转换后的授权链接：%+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到授权链接事件")
	}
	close(stub.gate)
	var got []httpapi.AccountLoginEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) < 2 || got[len(got)-1] != (httpapi.AccountLoginEvent{
		Kind: "done", Account: "dave", Model: "account:bigmodel-individual-coding-plan/GLM-5.4"}) {
		t.Fatalf("应转发过程行并以转换后的终态关流：%+v", got)
	}
	for _, ev := range got[:len(got)-1] {
		if ev.Kind != "line" || !strings.HasPrefix(ev.Line, "Login successful") && !strings.HasPrefix(ev.Line, "Model:") {
			t.Fatalf("过程行转换不符：%+v", ev)
		}
	}
	if st, _ := backend.AccountLoginStatus("z"); st.Account != "dave" || !strings.HasSuffix(st.Model, "/GLM-5.4") {
		t.Fatalf("登录后状态应透出账号与模型：%+v", st)
	}
}
