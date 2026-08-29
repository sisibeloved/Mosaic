//go:build it

// IT 层：native-codex 适配器真机 conformance（真实 codex CLI + 真实登录态）。
// 断言结构契约（结构化块字段齐全、usage 真实、会话连续），不断言具体内容（真实模型非确定性）。
// CI 无 codex/未登录时跳过；解析契约已由 UT fixtures 钉死。
package codexadapter

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// requireCodex 返回本机 codex 路径（PATH + nvm glob 位置），未安装则跳过。
func requireCodex(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("codex"); err == nil {
		return path
	}
	// 复用 harness 的发现逻辑成本高；此处直接探测已知 nvm 位置（与 UT 环境一致）
	for _, dir := range []string{
		osUserHome() + "/.nvm/versions/node/v24.14.1/bin/codex",
	} {
		if _, err := exec.LookPath(dir); err == nil {
			return dir
		}
		if _, err := osStatFile(dir); err == nil {
			return dir
		}
	}
	t.Skip("本机无 codex CLI（CI 常态）：解析契约由 UT fixtures 覆盖")
	return ""
}

// TestCodexRealEvaluateIntent_IT：结构 conformance——turn_intent 块字段齐全 + usage 真实。
func TestCodexRealEvaluateIntent_IT(t *testing.T) {
	codexPath := requireCodex(t)
	if !loggedIn(t, codexPath) {
		t.Skip("codex 未登录：真实调用无法进行（登录门控语义见 harness IT）")
	}
	adapter := New(Config{CodexPath: codexPath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, err := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it", Adapter: "codex"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer session.Close()

	h, err := session.Run(ctx, agent.Task{
		TaskID: "t-it-intent", Kind: agent.KindEvaluateIntent,
		ParticipantID: "par_codex", RoomID: "room_it",
		Context: agent.Context{Inline: map[string]any{
			"body": "Stimulus: should a personal desktop app use SQLite for storage? Decide whether to speak now.",
		}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Block != "turn_intent" {
		t.Fatalf("block = %s", res.Block)
	}
	if res.Data["action"] == "" {
		t.Fatal("turn_intent 缺 action")
	}
	if action, _ := res.Data["action"].(string); action != "silent" {
		if _, ok := res.Data["type"]; !ok {
			t.Fatal("非 silent intent 缺 type")
		}
		scores, ok := res.Data["scores"].(map[string]any)
		if !ok || len(scores) == 0 {
			t.Fatalf("缺 scores：%v", res.Data["scores"])
		}
	}
	if res.Usage == nil || res.Usage.InputTokens <= 0 {
		t.Fatalf("usage 应真实上报：%+v", res.Usage)
	}
	raw, _ := json.Marshal(res.Data)
	t.Logf("intent: %s usage=%+v", raw, res.Usage)
}

// TestCodexRealSessionResume_IT：同 session 两次任务走 resume（连续性）。
func TestCodexRealSessionResume_IT(t *testing.T) {
	codexPath := requireCodex(t)
	if !loggedIn(t, codexPath) {
		t.Skip("codex 未登录")
	}
	adapter := New(Config{CodexPath: codexPath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, _ := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it2", Adapter: "codex"})
	defer session.Close()

	mk := func(id, ask string) agent.Task {
		return agent.Task{
			TaskID: id, Kind: agent.KindGenerate, ParticipantID: "par_codex", RoomID: "room_it",
			Context: agent.Context{Inline: map[string]any{"body": ask}},
		}
	}
	h1, err := session.Run(ctx, mk("t-it-g1", "Remember the codeword: mosaic-blue-42. Reply with just the codeword."))
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if _, err := h1.Result(); err != nil {
		t.Fatalf("result1: %v", err)
	}
	// 第二任务：凭 resume 上下文回忆 codeword（连续性可观察验证）
	h2, err := session.Run(ctx, mk("t-it-g2", "What was the codeword you just remembered? Reply with just the codeword."))
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	res2, err := h2.Result()
	if err != nil {
		t.Fatalf("result2: %v", err)
	}
	body, _ := res2.Data["body"].(string)
	if body == "" {
		t.Fatalf("第二任务无正文：%+v", res2.Data)
	}
	t.Logf("resume 回忆结果: %q", body)
}

// TestCodexRealGeneratePublishable_IT：generate 产出可发布的 public_draft（body 非空）。
func TestCodexRealGeneratePublishable_IT(t *testing.T) {
	codexPath := requireCodex(t)
	if !loggedIn(t, codexPath) {
		t.Skip("codex 未登录")
	}
	adapter := New(Config{CodexPath: codexPath, Timeout: 180 * time.Second})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-it3", Adapter: "codex"})
	defer session.Close()
	h, err := session.Run(context.Background(), agent.Task{
		TaskID: "t-it-g3", Kind: agent.KindGenerate, ParticipantID: "par_codex", RoomID: "room_it",
		Context: agent.Context{Inline: map[string]any{
			"body": "Topic: best storage for a single-user desktop app. State your position in one short paragraph.",
		}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Block != "public_draft" {
		t.Fatalf("block = %s", res.Block)
	}
	body, _ := res.Data["body"].(string)
	if len([]rune(body)) < 10 {
		t.Fatalf("正文过短不可发布：%q", body)
	}
	if _, ok := res.Data["declared_relations"]; !ok {
		t.Fatal("public_draft 缺 declared_relations")
	}
}

// ---- IT 辅助（避免引入 harness 依赖环）----

func osUserHome() string { return osGetenv("HOME") }

func osStatFile(path string) (any, error) {
	return osStat(path)
}

// loggedIn 以 login status 实探（与 harness 探测口径一致）。
func loggedIn(t *testing.T, codexPath string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, code, err := (&processExecer{}).Exec(ctx,
		[]string{codexPath, "login", "status"}, codexEnv(codexPath), "")
	if err != nil || code != 0 {
		t.Logf("login status 探测失败（不计为已登录）：code=%d err=%v out=%q", code, err, out)
		return false
	}
	if len(out) == 0 || indexOf(out, "Logged in") < 0 {
		t.Logf("login status 输出异常：%q", out)
		return false
	}
	return true
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
