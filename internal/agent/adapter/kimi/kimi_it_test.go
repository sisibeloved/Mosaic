//go:build it

// IT 层：native-kimi 适配器真机 conformance（真实 kimi CLI + 真实登录态）。
// 断言结构契约（结构化块字段齐全、usage 不虚构、会话连续），不断言具体内容（真实模型非确定性）。
// CI 无 kimi/未登录时跳过；解析契约已由 UT fixtures 钉死（0.39.1）。
package kimi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// requireKimi 返回本机 kimi 路径（PATH + 官方安装位置），未安装则跳过。
func requireKimi(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("kimi"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".kimi-code", "bin", "kimi")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("本机无 kimi CLI（CI 常态）：解析契约由 UT fixtures 覆盖")
	return ""
}

// loggedIn 与 harness 探测口径一致：凭证文件存在即已登录。
func loggedIn(t *testing.T) bool {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	cred := filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json")
	if _, err := os.Stat(cred); err != nil {
		t.Logf("kimi 凭证文件不存在（未登录）: %s", cred)
		return false
	}
	return true
}

// TestKimiRealEvaluateIntent_IT：结构 conformance——turn_intent 块字段齐全 + usage 不虚构。
func TestKimiRealEvaluateIntent_IT(t *testing.T) {
	kimiPath := requireKimi(t)
	if !loggedIn(t) {
		t.Skip("kimi 未登录：真实调用无法进行（登录门控语义见 harness IT）")
	}
	adapter := New(Config{KimiPath: kimiPath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, err := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it", Adapter: "kimi"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer session.Close()

	h, err := session.Run(ctx, agent.Task{
		TaskID: "t-it-intent", Kind: agent.KindEvaluateIntent,
		ParticipantID: "par_kimi", RoomID: "room_it",
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
	if res.Block != agent.BlockTurnIntent {
		t.Fatalf("block = %s", res.Block)
	}
	if err := agent.ValidateBlock(res.Block, res.Data); err != nil {
		t.Fatalf("端口级结构校验：%v（原始：%+v）", err, res.Data)
	}
	if res.Usage != nil {
		t.Fatalf("Capabilities.UsageReporting=false：usage 应缺失记 unknown，不得虚构：%+v", res.Usage)
	}
	raw, _ := json.Marshal(res.Data)
	t.Logf("intent: %s", raw)
}

// TestKimiRealSessionResume_IT：同 session 两次任务走 -S 恢复（连续性可观察验证）。
func TestKimiRealSessionResume_IT(t *testing.T) {
	kimiPath := requireKimi(t)
	if !loggedIn(t) {
		t.Skip("kimi 未登录")
	}
	adapter := New(Config{KimiPath: kimiPath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, _ := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it2", Adapter: "kimi"})
	defer session.Close()

	mk := func(id, ask string) agent.Task {
		return agent.Task{
			TaskID: id, Kind: agent.KindGenerate, ParticipantID: "par_kimi", RoomID: "room_it",
			Context: agent.Context{Inline: map[string]any{"body": ask}},
		}
	}
	h1, err := session.Run(ctx, mk("t-it-g1", "Remember the codeword: mosaic-kimi-42. Reply with just the codeword."))
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if _, err := h1.Result(); err != nil {
		t.Fatalf("result1: %v", err)
	}
	// 第二任务：凭 -S 恢复的上下文回忆 codeword
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

// TestKimiRealGeneratePublishable_IT：generate 产出可发布的 public_draft（过发布门后 body 非空）。
func TestKimiRealGeneratePublishable_IT(t *testing.T) {
	kimiPath := requireKimi(t)
	if !loggedIn(t) {
		t.Skip("kimi 未登录")
	}
	adapter := New(Config{KimiPath: kimiPath, Timeout: 180 * time.Second})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-it3", Adapter: "kimi"})
	defer session.Close()
	h, err := session.Run(context.Background(), agent.Task{
		TaskID: "t-it-g3", Kind: agent.KindGenerate, ParticipantID: "par_kimi", RoomID: "room_it",
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
	if res.Block != agent.BlockPublicDraft {
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
