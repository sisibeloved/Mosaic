// 桌面渠道驱动面（app:zcode-desktop）契约：argv 直拼（无 sh 依赖——无感不假设
// Git Bash）、ELECTRON_RUN_AS_NODE 注入、托管缓存副本（bundle + stock provider
// 文件复制与 <scriptdir>/provider 定位满足）、Windows CreateProcess 32KiB 命令行
// 边界 fail fast、native 面 overlay 文件 IO（os 包直用）。
package zcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// newDesktopInstall 造一棵桌面版安装树（temp）：<root>/ZCode.exe（占位字节）+
// <root>/resources/glm/zcode.cjs（内容带标记）+ 可选 stock
// <root>/resources/config/provider/zcode-builtin.json。返回根与 bundle 绝对路径。
func newDesktopInstall(t *testing.T, stockProvider bool) (root, bundle string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "resources", "glm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "resources", "config", "provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle = filepath.Join(root, "resources", "glm", "zcode.cjs")
	if err := os.WriteFile(bundle, []byte("// desktop bundle marker "+root), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ZCode.exe"), []byte("MZ placeholder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if stockProvider {
		if err := os.WriteFile(filepath.Join(root, "resources", "config", "provider", "zcode-builtin.json"),
			[]byte(`{"schemaVersion":1,"revision":30,"stock":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, bundle
}

// newDesktopAdapter 桌面面适配器（无 WSL、Execer 注入；CacheRoot 指向 temp）。
func newDesktopAdapter(t *testing.T, exec Execer, root, bundle string) *Adapter {
	t.Helper()
	return New(Config{
		ZcodePath: filepath.Join(root, "ZCode.exe"),
		Bundle:    bundle,
		CacheRoot: filepath.Join(t.TempDir(), "zcode-desktop"),
		WorkDir:   filepath.Join(root, "work"),
		Execer:    exec,
		Timeout:   30 * time.Second,
	})
}

// TestDesktopArgvEnvAndCache：桌面面全链——托管缓存副本（bundle 复制 + stock
// provider 就位于 <cache>/<hash>/provider/）、argv 直拼（exe, 缓存 bundle, 旗标,
// -p 提示词收尾；无 sh 包装）、env 注入 ELECTRON_RUN_AS_NODE/HOME/USERPROFILE、
// stdin 不再携带提示词。
func TestDesktopArgvEnvAndCache(t *testing.T) {
	root, bundle := newDesktopInstall(t, true)
	exec := &fakeExecer{outputs: []string{resultStream(t, `{"action":"silent"}`)}}
	adapter := newDesktopAdapter(t, exec, root, bundle)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := h.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("无模型覆盖不得有 overlay IO：%d 次调用", len(exec.calls))
	}
	call := exec.calls[0]
	if call.argv[0] == "sh" {
		t.Fatalf("桌面面不得经 sh 包装（无感不依赖宿主 sh）：%v", call.argv)
	}
	if call.argv[0] != filepath.Join(root, "ZCode.exe") {
		t.Fatalf("argv[0] 应为桌面 exe：%v", call.argv)
	}
	cached := call.argv[1]
	if !strings.HasPrefix(cached, adapter.cfg.CacheRoot) {
		t.Fatalf("argv[1] 应为托管缓存副本（%s 下）：%s", adapter.cfg.CacheRoot, cached)
	}
	if !strings.HasSuffix(filepath.ToSlash(cached), "/zcode.cjs") {
		t.Fatalf("缓存副本应保文件名 zcode.cjs（bundle 升级比对语义）：%s", cached)
	}
	wantTail := []string{"--output-format", "stream-json", "--mode", "yolo", "--cwd", adapter.cfg.WorkDir}
	got := call.argv[2 : 2+len(wantTail)]
	for i := range wantTail {
		if got[i] != wantTail[i] {
			t.Fatalf("旗标序列应与 CLI 面同构：want %v got %v", wantTail, got)
		}
	}
	if call.argv[len(call.argv)-2] != "-p" {
		t.Fatalf("提示词应 -p 直进 argv 收尾：argv 尾 %v", call.argv[len(call.argv)-2:])
	}
	if call.stdin != "" {
		t.Fatalf("桌面面提示词不走 stdin（无 sh $(cat) 代入）：%q", call.stdin)
	}
	if !strings.Contains(call.argv[len(call.argv)-1], intentInstruction[:40]) {
		t.Fatalf("-p 参数应为完整提示词：%s", firstLineOf(call.argv[len(call.argv)-1], 80))
	}
	if !hasEnv(call.env, "ELECTRON_RUN_AS_NODE=1") {
		t.Fatalf("桌面面 env 必须注入 ELECTRON_RUN_AS_NODE=1：%v", call.env)
	}
	if !hasEnv(call.env, "USERPROFILE=") || !hasEnv(call.env, "HOME=") {
		t.Fatalf("桌面面 env 必须锚定 USERPROFILE/HOME（node homedir 定位凭证）：%v", call.env)
	}
	// 缓存副本内容与 stock provider 就位
	if raw, err := os.ReadFile(cached); err != nil || !strings.Contains(string(raw), "// desktop bundle marker") {
		t.Fatalf("缓存 bundle 应为安装 bundle 副本：%v", err)
	}
	provRaw, err := os.ReadFile(filepath.Join(filepath.Dir(cached), "provider", "zcode-builtin.json"))
	if err != nil {
		t.Fatalf("缓存目录须有 provider/zcode-builtin.json（bundle CLI 模式强制定位）：%v", err)
	}
	if !strings.Contains(string(provRaw), `"stock":true`) {
		t.Fatalf("provider 文件应取 stock config/provider 副本：%s", firstLineOf(string(provRaw), 60))
	}
}

// TestDesktopCacheMemoizedAndFallback：缓存进程内记忆（两次任务只落一份哈希目录）；
// stock provider 缺失时回退 scriptdir/provider（定位已满足的安装）。
func TestDesktopCacheMemoizedAndFallback(t *testing.T) {
	root, bundle := newDesktopInstall(t, false) // 无 stock provider
	// scriptdir/provider 放置（手工满足定位的安装形态）
	if err := os.MkdirAll(filepath.Join(root, "resources", "glm", "provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "resources", "glm", "provider", "zcode-builtin.json"),
		[]byte(`{"scriptdir":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecer{outputs: []string{
		resultStream(t, `{"action":"silent"}`),
		resultStream(t, `{"action":"silent"}`),
	}}
	adapter := newDesktopAdapter(t, exec, root, bundle)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	for i := 0; i < 2; i++ {
		h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if _, err := h.Result(); err != nil {
			t.Fatalf("result %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(adapter.cfg.CacheRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("两任务应共用同一份缓存目录（内容寻址 + 进程内记忆）：%v %v", entries, err)
	}
	prov := filepath.Join(adapter.cfg.CacheRoot, entries[0].Name(), "provider", "zcode-builtin.json")
	raw, err := os.ReadFile(prov)
	if err != nil || !strings.Contains(string(raw), `"scriptdir":true`) {
		t.Fatalf("stock 缺失应回退 scriptdir/provider 来源：%v", err)
	}
}

// TestDesktopProviderMissing：两候选 provider 来源均缺 → 显式报错（布局漂移可见），
// 不静默回退 CLI 面。
func TestDesktopProviderMissing(t *testing.T) {
	root, bundle := newDesktopInstall(t, false)
	exec := &fakeExecer{}
	adapter := newDesktopAdapter(t, exec, root, bundle)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := h.Result(); err == nil {
		t.Fatal("provider 双来源均缺必须显式失败")
	} else if !strings.Contains(err.Error(), "zcode-builtin.json") {
		t.Fatalf("报错应指明定位失败对象：%v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("缓存未就绪不得执行子进程：%d 次调用", len(exec.calls))
	}
}

// TestDesktopPromptCap：提示词超 Windows CreateProcess 命令行边界 → fail fast，
// 不拉起子进程（错误信息指明 Windows 边界，区别于 CLI 面 Linux 边界）。
func TestDesktopPromptCap(t *testing.T) {
	root, bundle := newDesktopInstall(t, true)
	exec := &fakeExecer{outputs: []string{resultStream(t, `{"action":"silent"}`)}}
	adapter := newDesktopAdapter(t, exec, root, bundle)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	adapter.cfg.MaxPromptBytes = desktopPromptCap + 10 // 显式抬高也不得越过物理边界
	h, err := sess.Run(context.Background(), agent.Task{
		TaskID:  "t",
		Kind:    agent.KindEvaluateIntent,
		Context: agent.Context{Inline: map[string]any{"pad": strings.Repeat("x", desktopPromptCap)}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	_, err = h.Result()
	if err == nil {
		t.Fatal("超 Windows 命令行边界的提示词必须 fail fast")
	}
	if !strings.Contains(err.Error(), "Windows CreateProcess 32KiB") {
		t.Fatalf("错误应指明 Windows 命令行边界：%v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("超限不得执行子进程：%d 次调用", len(exec.calls))
	}
}

// TestDesktopOverlayNative：native 面（桌面/未来 Windows CLI）模型覆盖 overlay
// 走 os 包直读写（无 sh）——基座取 HOME 下 provider_config.json，写
// provider_config.mosaic.<model>.json，env 携重定向路径。
func TestDesktopOverlayNative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".zcode", "v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".zcode", "v2", "provider_config.json"),
		[]byte(overlayBaseJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	root, bundle := newDesktopInstall(t, true)
	exec := &fakeExecer{outputs: []string{resultStream(t, `{"action":"silent"}`)}}
	adapter := newDesktopAdapter(t, exec, root, bundle)
	adapter.cfg.Model = "glm-4.6"
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := h.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	overlay := filepath.Join(home, ".zcode", "v2", "provider_config.mosaic.glm-4.6.json")
	raw, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatalf("native 面 overlay 应落盘（os 直写）：%v", err)
	}
	if !strings.Contains(string(raw), `"modelId":"glm-4.6"`) {
		t.Fatalf("overlay 应钉目标模型：%s", firstLineOf(string(raw), 80))
	}
	if !hasEnv(exec.calls[0].env, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=") {
		t.Fatalf("桌面面模型覆盖同样经 env 重定向生效：%v", exec.calls[0].env)
	}
}
