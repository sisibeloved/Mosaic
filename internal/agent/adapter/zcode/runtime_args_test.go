// v1.48 运行参数语义（zcode 面）：模型覆盖全任务生效；评估任务 EvalModel 优先于
// Model。zcode 无 --model flag——覆盖实现为 provider 配置 overlay（env 重定向，
// 0.16.9 实证）；zcode 无思考强度面。
package zcode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// TestRuntimeModelOverlayZcode：EvalModel > Model > CLI 默认的 overlay 语义——
// 评估任务 overlay 写 EvalModel（不被主模型覆盖吞掉）、生成任务 overlay 写 Model、
// 无覆盖零 overlay IO。
func TestRuntimeModelOverlayZcode(t *testing.T) {
	exec := newOverlayExecer(t, 0)
	adapter := New(Config{
		ZcodePath: "/home/u/.zcode/bin/zcode", Execer: exec, Timeout: 30 * time.Second,
		WSLDistro: "Ubuntu", WSLHome: "/home/u",
		Model: "glm-4.6", EvalModel: "glm-4.5",
	})
	session, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer session.Close()
	run := func(kind agent.TaskKind) {
		h, err := session.Run(context.Background(), agent.Task{TaskID: "t", Kind: kind})
		if err != nil {
			t.Fatalf("run %s: %v", kind, err)
		}
		if _, err := h.Result(); err != nil {
			t.Fatalf("result %s: %v", kind, err)
		}
	}
	run(agent.KindEvaluateIntent)
	// 调用序：评估（读/写/执行）+ 生成（读/写/执行）
	if len(exec.calls) != 3 {
		t.Fatalf("评估任务 overlay 全链应为 读/写/执行：%d 次调用", len(exec.calls))
	}
	evalEnv := exec.calls[2].env
	if !hasEnv(evalEnv, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=/home/u/.zcode/v2/provider_config.mosaic.glm-4.5.json") {
		t.Fatalf("评估任务应 overlay EvalModel：%v", evalEnv)
	}
	if hasEnv(evalEnv, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=/home/u/.zcode/v2/provider_config.mosaic.glm-4.6.json") {
		t.Fatalf("评估降档不得被主模型覆盖吞掉：%v", evalEnv)
	}
	run(agent.KindGenerate)
	if len(exec.calls) != 6 {
		t.Fatalf("生成任务 overlay 全链应为 读/写/执行：%d 次调用", len(exec.calls))
	}
	if !hasEnv(exec.calls[5].env, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=/home/u/.zcode/v2/provider_config.mosaic.glm-4.6.json") {
		t.Fatalf("生成任务应 overlay 主模型：%v", exec.calls[5].env)
	}
	if w := exec.written["/home/u/.zcode/v2/provider_config.mosaic.glm-4.5.json"]; !strings.Contains(w, `"modelId":"glm-4.5"`) {
		t.Fatalf("评估 overlay 应钉 EvalModel：%s", w)
	}
	if w := exec.written["/home/u/.zcode/v2/provider_config.mosaic.glm-4.6.json"]; !strings.Contains(w, `"modelId":"glm-4.6"`) {
		t.Fatalf("生成 overlay 应钉 Model：%s", w)
	}

	// 无覆盖：不设 env、无 overlay IO
	exec2 := newOverlayExecer(t, 0)
	adapter2 := New(Config{ZcodePath: "/bin/zcode", Execer: exec2, Timeout: 30 * time.Second})
	session2, _ := adapter2.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "zcode"})
	defer session2.Close()
	h, _ := session2.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindEvaluateIntent})
	if _, err := h.Result(); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(exec2.calls) != 1 {
		t.Fatalf("无覆盖不得有 overlay IO：%d 次调用", len(exec2.calls))
	}
	if hasEnv(exec2.calls[0].env, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=") {
		t.Fatalf("无覆盖不得注入重定向变量：%v", exec2.calls[0].env)
	}
}

func hasEnv(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}
