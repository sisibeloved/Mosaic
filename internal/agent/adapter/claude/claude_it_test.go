//go:build it

// IT 层：claude 适配器真机 conformance（真实 claude CLI + 真实登录态）。
// 断言结构契约（结构化块字段齐全、usage 透传不虚构、会话连续），不断言具体内容
// （真实模型非确定性）。CI 无 claude/未登录时跳过；解析契约已由 UT fixtures 钉死
// （claude 2.1.172 鉴权失败态真机捕获；成功态文档 schema 内联——登录恢复后补捕获）。
// 2026-10-27 本机实录：WSL 侧 OAuth token 失效（401 is_error，刷新端点 429 限流，
// 环境态）——本组用例按 ittest 纪律降级跳过；负责人重新 `claude auth login` 后
// 自动回绿。
package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/ittest"
)

// requireClaude 返回本机 claude 路径（PATH + 官方安装布局 ~/.local/bin），未安装则跳过。
func requireClaude(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, candidate := range []string{
			filepath.Join(home, ".local", "bin", "claude"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	t.Skip("本机无 claude CLI（CI 常态）：解析契约由 UT fixtures 覆盖")
	return ""
}

// loggedIn 与 harness 探测口径一致：~/.claude/.credentials.json（claudeAiOauth）
// 存在即已登录。注意（2026-10-27 实证）：文件存在而 OAuth token 失效时 -p 仍以
// 退出码 0 + result.is_error 报告鉴权失败——该环境态由 ittest 降级判定承接
// （"failed to authenticate"），本门只挡"从未登录"的确定态。
func loggedIn(t *testing.T) bool {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
		return true
	}
	t.Logf("claude 凭证文件不存在（未登录）: %s", filepath.Join(home, ".claude", ".credentials.json"))
	return false
}

// TestClaudeRealEvaluateIntent_IT：结构 conformance——turn_intent 块字段齐全 + usage 透传。
func TestClaudeRealEvaluateIntent_IT(t *testing.T) {
	claudePath := requireClaude(t)
	if !loggedIn(t) {
		t.Skip("claude 未登录：真实调用无法进行（登录门控语义见 harness IT）")
	}
	adapter := New(Config{ClaudePath: claudePath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, err := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it", Adapter: "claude"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer session.Close()

	h, err := session.Run(ctx, agent.Task{
		TaskID: "t-it-intent", Kind: agent.KindEvaluateIntent,
		ParticipantID: "par_claude", RoomID: "room_it",
		Context: agent.Context{Inline: map[string]any{
			"body": "Stimulus: should a personal desktop app use SQLite for storage? Decide whether to speak now.",
		}},
	})
	if err != nil && !ittest.SkipIfProviderUnavailable(t, "run", err) {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil && !ittest.SkipIfProviderUnavailable(t, "result", err) {
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

// TestClaudeRealSessionResume_IT：同 session 两次任务走 --resume 恢复（连续性可观察验证）。
func TestClaudeRealSessionResume_IT(t *testing.T) {
	claudePath := requireClaude(t)
	if !loggedIn(t) {
		t.Skip("claude 未登录")
	}
	adapter := New(Config{ClaudePath: claudePath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, _ := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it2", Adapter: "claude"})
	defer session.Close()

	mk := func(id, ask string) agent.Task {
		return agent.Task{
			TaskID: id, Kind: agent.KindGenerate, ParticipantID: "par_claude", RoomID: "room_it",
			Context: agent.Context{Inline: map[string]any{"body": ask}},
		}
	}
	h1, err := session.Run(ctx, mk("t-it-g1", "Remember the codeword: mosaic-claude-42. Reply with just the codeword."))
	if err != nil && !ittest.SkipIfProviderUnavailable(t, "run1", err) {
		t.Fatalf("run1: %v", err)
	}
	if _, err := h1.Result(); err != nil && !ittest.SkipIfProviderUnavailable(t, "result1", err) {
		t.Fatalf("result1: %v", err)
	}
	// 第二任务：凭 --resume 恢复的上下文回忆 codeword
	h2, err := session.Run(ctx, mk("t-it-g2", "What was the codeword you just remembered? Reply with just the codeword."))
	if err != nil && !ittest.SkipIfProviderUnavailable(t, "run2", err) {
		t.Fatalf("run2: %v", err)
	}
	res2, err := h2.Result()
	if err != nil && !ittest.SkipIfProviderUnavailable(t, "result2", err) {
		t.Fatalf("result2: %v", err)
	}
	body, _ := res2.Data["body"].(string)
	if body == "" {
		t.Fatalf("第二任务无正文：%+v", res2.Data)
	}
	t.Logf("resume 回忆结果: %q", body)
}

// TestClaudeRealGeneratePublishable_IT：generate 产出可发布的 public_draft（过发布门后 body 非空）。
func TestClaudeRealGeneratePublishable_IT(t *testing.T) {
	claudePath := requireClaude(t)
	if !loggedIn(t) {
		t.Skip("claude 未登录")
	}
	adapter := New(Config{ClaudePath: claudePath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, _ := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it3", Adapter: "claude"})
	defer session.Close()

	h, err := session.Run(ctx, agent.Task{
		TaskID: "t-it-gen", Kind: agent.KindGenerate, ParticipantID: "par_claude", RoomID: "room_it",
		Context: agent.Context{Inline: map[string]any{
			"body": "Say hello to the room in one short sentence.",
		}},
	})
	if err != nil && !ittest.SkipIfProviderUnavailable(t, "run", err) {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil && !ittest.SkipIfProviderUnavailable(t, "result", err) {
		t.Fatalf("result: %v", err)
	}
	if res.Block != agent.BlockPublicDraft {
		t.Fatalf("block = %s", res.Block)
	}
	body, _ := res.Data["body"].(string)
	if body == "" {
		t.Fatalf("public_draft 缺 body：%+v", res.Data)
	}
	t.Logf("public_draft body: %q", body)
}
