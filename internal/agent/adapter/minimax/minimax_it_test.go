//go:build it

// IT 层：minimax（mcode）适配器真机 conformance（真实 mcode CLI + 真实登录态）。
// 断言结构契约（结构化块字段齐全、usage 透传不虚构、会话连续），不断言具体内容
// （真实模型非确定性）。CI 无 mcode/未登录时跳过；解析契约已由 UT fixtures 钉死（0.2.7）。
package minimax

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

// requireMcode 返回本机 mcode 路径（PATH + nvm 官方布局），未安装则跳过。
func requireMcode(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("mcode"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".nvm/versions/node/v24.14.1/bin/mcode")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("本机无 mcode CLI（CI 常态）：解析契约由 UT fixtures 覆盖")
	return ""
}

// loggedIn 与 harness 探测口径一致：~/.minimax/cli-auth 存在即已登录（实证目录形态）。
func loggedIn(t *testing.T) bool {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(home, ".minimax", "cli-auth")); err != nil {
		t.Logf("mcode 登录态目录不存在（未登录）: %s", filepath.Join(home, ".minimax", "cli-auth"))
		return false
	}
	return true
}

// TestMinimaxRealEvaluateIntent_IT：结构 conformance——turn_intent 块字段齐全 + usage 透传。
func TestMinimaxRealEvaluateIntent_IT(t *testing.T) {
	mcodePath := requireMcode(t)
	if !loggedIn(t) {
		t.Skip("mcode 未登录：真实调用无法进行（登录门控语义见 harness IT）")
	}
	adapter := New(Config{McodePath: mcodePath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, err := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it", Adapter: "minimax"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer session.Close()

	h, err := session.Run(ctx, agent.Task{
		TaskID: "t-it-intent", Kind: agent.KindEvaluateIntent,
		ParticipantID: "par_minimax", RoomID: "room_it",
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
	if res.Usage == nil || res.Usage.InputTokens <= 0 {
		t.Fatalf("Capabilities.UsageReporting=true：usage 应透传：%+v", res.Usage)
	}
	raw, _ := json.Marshal(res.Data)
	t.Logf("intent: %s（usage in=%d out=%d）", raw, res.Usage.InputTokens, res.Usage.OutputTokens)
}

// TestMinimaxRealSessionResume_IT：同 session 两次任务走 --session 恢复（连续性可观察验证）。
func TestMinimaxRealSessionResume_IT(t *testing.T) {
	mcodePath := requireMcode(t)
	if !loggedIn(t) {
		t.Skip("mcode 未登录")
	}
	adapter := New(Config{McodePath: mcodePath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, _ := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it2", Adapter: "minimax"})
	defer session.Close()

	mk := func(id, ask string) agent.Task {
		return agent.Task{
			TaskID: id, Kind: agent.KindGenerate, ParticipantID: "par_minimax", RoomID: "room_it",
			Context: agent.Context{Inline: map[string]any{"body": ask}},
		}
	}
	h1, err := session.Run(ctx, mk("t-it-g1", "Remember the codeword: mosaic-minimax-42. Reply with just the codeword."))
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if _, err := h1.Result(); err != nil {
		t.Fatalf("result1: %v", err)
	}
	// 第二任务：凭 --session 恢复的上下文回忆 codeword
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

// TestMinimaxRealGeneratePublishable_IT：generate 产出可发布的 public_draft（过发布门后 body 非空）。
func TestMinimaxRealGeneratePublishable_IT(t *testing.T) {
	mcodePath := requireMcode(t)
	if !loggedIn(t) {
		t.Skip("mcode 未登录")
	}
	adapter := New(Config{McodePath: mcodePath, Timeout: 180 * time.Second})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-it3", Adapter: "minimax"})
	defer session.Close()

	h, err := session.Run(context.Background(), agent.Task{
		TaskID: "t-it-g3", Kind: agent.KindGenerate, ParticipantID: "par_minimax", RoomID: "room_it",
		Grant: &agent.Grant{GrantID: "g-it", Rank: 1, ResponseCap: 500},
		Context: agent.Context{Inline: map[string]any{
			"body": "Write one short friendly chat message answering: what is two plus two?",
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
	if body == "" {
		t.Fatalf("正文为空：%+v", res.Data)
	}
	if n := len([]rune(body)); n > 500 {
		t.Fatalf("发布门未约束正文：%d runes", n)
	}
	t.Logf("generate: %q", body)
}
