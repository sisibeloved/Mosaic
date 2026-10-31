package harness

// 0.6.3 审校 P2#3：版本探测只认 exit 0——非零退出的错误文本（常含数字）
// 不得充当版本（经 runtime 健康适配器影响 ready 事实）。
import (
	"testing"
)

func TestProbeExecutableNonzeroExitIsNotVersion(t *testing.T) {
	spec := ProbeSpec{Adapter: "fakecli", Binary: "fakecli", VersionArgs: []string{"--version"}}

	// 普通 CLI 分支：exit 1 + 带数字错误文本
	r := newFakeRunner()
	r.lookups["native||fakecli"] = "/bin/fakecli"
	r.exists["native||/bin/fakecli"] = true
	r.runs["native||/bin/fakecli --version"] = "error: panic code 12345\x00EXIT:1"
	exe := probeExecutable(t.Context(), r, spec, RuntimeNative, "", "/bin/fakecli", "")
	if exe.Version != "" {
		t.Fatalf("非零退出的错误文本不得充当版本：%q", exe.Version)
	}

	// bundle 分支（桌面渠道）：同口径
	r2 := newFakeRunner()
	r2.lookups["native||fakecli"] = "/bin/fakecli"
	r2.exists["native||/bin/fakecli"] = true
	r2.runs["native||/bin/fakecli /bundle.js --version"] = "SyntaxError: unexpected token 42\x00EXIT:1"
	exe2 := probeExecutable(t.Context(), r2, spec, RuntimeNative, "", "/bin/fakecli", "/bundle.js")
	if exe2.Version != "" {
		t.Fatalf("bundle 分支非零退出不得充当版本：%q", exe2.Version)
	}

	// 对照：exit 0 正常取版本
	r3 := newFakeRunner()
	r3.lookups["native||fakecli"] = "/bin/fakecli"
	r3.exists["native||/bin/fakecli"] = true
	r3.runs["native||/bin/fakecli --version"] = "fakecli 2.0.1"
	exe3 := probeExecutable(t.Context(), r3, spec, RuntimeNative, "", "/bin/fakecli", "")
	if exe3.Version != "fakecli 2.0.1" { // sanitizeVersion 保整行（含数字即版本事实）
		t.Fatalf("exit 0 应取版本：%q", exe3.Version)
	}
}
