package harness

import (
	"context"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestWSLGlobArgsPreserveInnerShellExpansion(t *testing.T) {
	pattern := "/home/user/.nvm/versions/node/*/bin"
	got := wslGlobArgs(pattern)
	want := []string{
		"sh",
		"-c",
		`for p in "\$@"; do [ -e "\$p" ] && printf "%s\n" "\$p"; done`,
		"--",
		pattern,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslGlobArgs() = %#v, want %#v", got, want)
	}
}

func TestWSLRunWithDirArgsPreservePATHAndArguments(t *testing.T) {
	args := []string{"/home/user/bin/codex", "login", "status"}
	got := wslRunWithDirArgs("/home/user/bin", args)
	want := []string{
		"sh",
		"-c",
		`export PATH='/home/user/bin':\$PATH; exec "\$@"`,
		"--",
		"/home/user/bin/codex",
		"login",
		"status",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wslRunWithDirArgs() = %#v, want %#v", got, want)
	}
}

func TestWSLRunWithEnvArgsInjectsEnvAndPreservesArguments(t *testing.T) {
	args := wslRunWithEnvArgs([]string{"ELECTRON_RUN_AS_NODE=1"}, []string{"/mnt/c/ZCode.exe", "/mnt/c/b.cjs", "--version"})
	want := []string{
		"sh",
		"-c",
		`export 'ELECTRON_RUN_AS_NODE=1'; exec "\$@"`,
		"--",
		"/mnt/c/ZCode.exe",
		"/mnt/c/b.cjs",
		"--version",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("wslRunWithEnvArgs() = %#v, want %#v", args, want)
	}
}

func TestWSLRunWithEnvArgsEmptyEnvPassthrough(t *testing.T) {
	args := wslRunWithEnvArgs(nil, []string{"cmd", "-v"})
	if !reflect.DeepEqual(args, []string{"cmd", "-v"}) {
		t.Fatalf("空 env 应纯透传（export 空表会打印环境）：%#v", args)
	}
}

// native 面真实注入：变量经 cmd.Env 追加后对子进程可见（桌面渠道版本探测的
// ELECTRON_RUN_AS_NODE 即此路径——CI 三平台覆盖两分支）。
func TestHostRunnerRunWithEnvNative(t *testing.T) {
	var probe []string
	if goruntime.GOOS == "windows" {
		probe = []string{"cmd", "/c", "echo %MOSAIC_PROBE%"}
	} else {
		probe = []string{"sh", "-c", `printf %s "$MOSAIC_PROBE"`}
	}
	out, code, err := NewHostRunner().RunWithEnv(context.Background(), RuntimeNative, "", []string{"MOSAIC_PROBE=injected"}, probe)
	if err != nil || code != 0 {
		t.Fatalf("run: %v code=%d", err, code)
	}
	if strings.TrimSpace(out) != "injected" {
		t.Fatalf("env 应注入子进程：out=%q", out)
	}
}
