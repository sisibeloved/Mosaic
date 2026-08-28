//go:build it

// IT 层：真实组件跨包装配（supervisor × echo 适配器）。
// 构建标签 it；CI 独立步骤执行（go test -tags it ./...）。
package echo

import (
	"context"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

// 装配链路：注册 → 提交 → 结果 → 关闭 → 关闭后可重建会话（逻辑会话语义）。
func TestSupervisorWiring_IT(t *testing.T) {
	sup := agent.NewSupervisor()
	if err := sup.Register(Adapter{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	profile := agent.Profile{ProfileID: "par_echo_it", Adapter: "echo"}

	h, err := sup.Submit(context.Background(), profile, sampleTask(agent.KindEvaluateIntent))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Block != "turn_intent" {
		t.Errorf("block = %q, want turn_intent", res.Block)
	}

	sup.Shutdown()

	// Shutdown 后再次提交：按需重新 Boot（逻辑会话可重建，不是永久态）。
	h2, err := sup.Submit(context.Background(), profile, sampleTask(agent.KindObserve))
	if err != nil {
		t.Fatalf("submit after shutdown: %v", err)
	}
	res2, err := h2.Result()
	if err != nil {
		t.Fatalf("result after shutdown: %v", err)
	}
	if res2.Block != "attention_assessment" {
		t.Errorf("block = %q, want attention_assessment", res2.Block)
	}
}

// 并发混合任务：多 goroutine 同时经 supervisor 提交，全部得到确定性正确块。
func TestConcurrentMixedTasks_IT(t *testing.T) {
	sup := agent.NewSupervisor()
	if err := sup.Register(Adapter{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	kinds := []agent.TaskKind{
		agent.KindObserve, agent.KindEvaluateIntent, agent.KindGenerate,
		agent.KindSummarize, agent.KindEvaluateClosure,
	}
	wantBlock := map[agent.TaskKind]string{
		agent.KindObserve:         "attention_assessment",
		agent.KindEvaluateIntent:  "turn_intent",
		agent.KindGenerate:        "public_draft",
		agent.KindSummarize:       "grounded_summary",
		agent.KindEvaluateClosure: "closure_intent",
	}

	const goroutines = 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := kinds[i%len(kinds)]
			profile := agent.Profile{ProfileID: "par_echo_it", Adapter: "echo"}
			h, err := sup.Submit(context.Background(), profile, sampleTask(kind))
			if err != nil {
				t.Errorf("submit %d (%s): %v", i, kind, err)
				return
			}
			res, err := h.Result()
			if err != nil {
				t.Errorf("result %d (%s): %v", i, kind, err)
				return
			}
			if res.Block != wantBlock[kind] {
				t.Errorf("task %d: block = %q, want %q", i, res.Block, wantBlock[kind])
			}
		}(i)
	}
	wg.Wait()
	sup.Shutdown()
}
