//go:build it

// IT 层：zcode 适配器真机 conformance（真实 zcode CLI + 真实登录态）。
// 断言结构契约（结构化块字段齐全、usage 透传不虚构、会话连续），不断言具体内容
// （真实模型非确定性）。CI 无 zcode/未登录时跳过；解析契约已由 UT fixtures 钉死
// （zcode 0.16.9）。
package zcode

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

// requireZcode 返回本机 zcode 路径（PATH + 官方安装布局），未安装则跳过。
func requireZcode(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("zcode"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, candidate := range []string{
			filepath.Join(home, ".zcode/bin/zcode"),
			filepath.Join(home, ".local/bin/zcode"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	t.Skip("本机无 zcode CLI（CI 常态）：解析契约由 UT fixtures 覆盖")
	return ""
}

// loggedIn 与 harness 探测口径一致：~/.zcode/v2/credentials.json（OAuth）或
// ~/.zcode/v2/provider_config.json（API-key 配置）任一存在即已登录（实证 2026-09-23
// 双凭证面，zcode 0.16.9）。
func loggedIn(t *testing.T) bool {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, rel := range []string{
		filepath.Join(".zcode", "v2", "credentials.json"),
		filepath.Join(".zcode", "v2", "provider_config.json"),
	} {
		if _, err := os.Stat(filepath.Join(home, rel)); err == nil {
			return true
		}
	}
	t.Logf("zcode 双凭证面均不存在（未登录）: %s", filepath.Join(home, ".zcode", "v2"))
	return false
}

// TestZcodeRealEvaluateIntent_IT：结构 conformance——turn_intent 块字段齐全 + usage 透传。
func TestZcodeRealEvaluateIntent_IT(t *testing.T) {
	zcodePath := requireZcode(t)
	if !loggedIn(t) {
		t.Skip("zcode 未登录：真实调用无法进行（登录门控语义见 harness IT）")
	}
	adapter := New(Config{ZcodePath: zcodePath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, err := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it", Adapter: "zcode"})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer session.Close()

	h, err := session.Run(ctx, agent.Task{
		TaskID: "t-it-intent", Kind: agent.KindEvaluateIntent,
		ParticipantID: "par_zcode", RoomID: "room_it",
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

// TestZcodeRealSessionResume_IT：同 session 两次任务走 --resume 恢复（连续性可观察验证）。
func TestZcodeRealSessionResume_IT(t *testing.T) {
	zcodePath := requireZcode(t)
	if !loggedIn(t) {
		t.Skip("zcode 未登录")
	}
	adapter := New(Config{ZcodePath: zcodePath, Timeout: 180 * time.Second})
	ctx := context.Background()
	session, _ := adapter.Boot(ctx, agent.Profile{ProfileID: "p-it2", Adapter: "zcode"})
	defer session.Close()

	mk := func(id, ask string) agent.Task {
		return agent.Task{
			TaskID: id, Kind: agent.KindGenerate, ParticipantID: "par_zcode", RoomID: "room_it",
			Context: agent.Context{Inline: map[string]any{"body": ask}},
		}
	}
	h1, err := session.Run(ctx, mk("t-it-g1", "Remember the codeword: mosaic-zcode-42. Reply with just the codeword."))
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

// TestZcodeRealGeneratePublishable_IT：generate 产出可发布的 public_draft（过发布门后 body 非空）。
func TestZcodeRealGeneratePublishable_IT(t *testing.T) {
	zcodePath := requireZcode(t)
	if !loggedIn(t) {
		t.Skip("zcode 未登录")
	}
	adapter := New(Config{ZcodePath: zcodePath, Timeout: 180 * time.Second})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p-it3", Adapter: "zcode"})
	defer session.Close()

	h, err := session.Run(context.Background(), agent.Task{
		TaskID: "t-it-g3", Kind: agent.KindGenerate, ParticipantID: "par_zcode", RoomID: "room_it",
		Grant: &agent.Grant{GrantID: "g-it", Rank: 1, ResponseCap: 500},
		Context: agent.Context{Inline: map[string]any{
			"body": "Write one short friendly chat message answering: what is two plus two?",
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
		t.Fatalf("正文为空：%+v", res.Data)
	}
	if n := len([]rune(body)); n > 500 {
		t.Fatalf("发布门未约束正文：%d runes", n)
	}
	t.Logf("generate: %q", body)
}
