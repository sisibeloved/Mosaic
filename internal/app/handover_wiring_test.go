package app

// 0.7.9 审校 #1（P1）反例钉住：换将任务转移不得只翻 owner/暂停——旧执行器
// 仍在途 Run。handoverPorts（app 装配单源）的 ReassignMission 闭包须**信号
// 先行**：核未终态 Run → 停止信号 → 归属翻转；信号失败不翻转。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/memory"
	"github.com/sisibeloved/Mosaic/internal/mission"
)

// fakeRunStopper run 停止面替身（记录调用序——断言"信号先于翻转"）。
type fakeRunStopper struct {
	mu        sync.Mutex
	unfin     map[string]bool // missionID → HasUnfinished
	calls     []string        // "has:<id>" | "signal:<id>:<cause>"（全局序）
	hasErr    error
	signalErr error
	signaled  map[string]bool
}

func (f *fakeRunStopper) HasUnfinished(_ context.Context, missionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "has:"+missionID)
	if f.hasErr != nil {
		return false, f.hasErr
	}
	return f.unfin[missionID], nil
}

func (f *fakeRunStopper) SignalStopByMission(_ context.Context, missionID, cause string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("signal:%s:%s", missionID, cause))
	if f.signalErr != nil {
		return 0, f.signalErr
	}
	if f.signaled == nil {
		f.signaled = map[string]bool{}
	}
	f.signaled[missionID] = true
	return 1, nil
}

// newHandoverWiring 装配：mission（memstore）+ memory + handoverPorts。
func newHandoverWiring(t *testing.T) (*mission.Service, *mission.MemStore) {
	t.Helper()
	lookup := &mapAgents{ids: map[string]bool{"ag_a": true, "ag_b": true}}
	mem := mission.NewMemStore()
	svc := mission.New(mem,
		mission.WithNow(func() string { return "2026-11-12T12:00:00Z" }),
		mission.WithNewMissionID(func() string { return "mi_w1" }),
		mission.WithAgentLookup(lookup))
	return svc, mem
}

type mapAgents struct{ ids map[string]bool }

func (m *mapAgents) AgentExists(_ context.Context, id string) (bool, error) {
	return m.ids[id], nil
}

func (m *mapAgents) AgentStatus(_ context.Context, id string) (string, bool, error) {
	if !m.ids[id] {
		return "", false, nil
	}
	return mission.AgentStatusActive, true, nil
}

// runningSlotMission 直插 running 任务（挂实例、归属 ag_a——在途 Run 场景）。
func runningSlotMission(t *testing.T, mem *mission.MemStore) {
	t.Helper()
	row := mission.Mission{
		MissionID: "mi_w1", ContractVersion: 1, Status: mission.StatusRunning,
		OwnerAgentID: "ag_a", SlotInstanceID: "si_w", Requester: mission.RequesterOwner,
		Content: mission.Content{
			Objective: "在途", Scope: "s",
			AcceptanceCriteria: []string{"a"}, DeliverableSpecs: []string{"d"},
			SourceRefs: []string{"r:1"}, Budget: mission.DefaultBudget(),
		},
		CreatedAt: "2026-11-12T11:00:00Z", UpdatedAt: "2026-11-12T11:00:00Z",
	}
	if _, err := mem.CreateMission(context.Background(), row, nil); err != nil {
		t.Fatal(err)
	}
}

// TestHandoverPortsSignalBeforeFlip 有未终态 Run：停止信号**先于**归属翻转
// （调用序断言）；信号后转移照常（paused + owner 翻转）。
func TestHandoverPortsSignalBeforeFlip(t *testing.T) {
	mSvc, mem := newHandoverWiring(t)
	runningSlotMission(t, mem)
	stopper := &fakeRunStopper{unfin: map[string]bool{"mi_w1": true}}
	ports := handoverPorts(mSvc, stopper, memory.NewService(memory.NewMemStore(), memory.Anchors{}, func() string { return "2026-11-12T12:00:00Z" }))

	post, ok, err := ports.ReassignMission(context.Background(), "mi_w1", "ag_a", "ag_b")
	if err != nil || !ok {
		t.Fatalf("转移: ok=%v %v", ok, err)
	}
	if post != mission.StatusPaused {
		t.Fatalf("转移后状态: %s", post)
	}
	got, _, _ := mSvc.Get(context.Background(), "mi_w1")
	if got.OwnerAgentID != "ag_b" || got.Status != mission.StatusPaused {
		t.Fatalf("转移后: %+v", got)
	}
	// 次序：has → signal → 翻转（fake 全局序——signal 在 has 之后即先行）。
	stopper.mu.Lock()
	defer stopper.mu.Unlock()
	if len(stopper.calls) != 2 || stopper.calls[0] != "has:mi_w1" ||
		stopper.calls[1] != "signal:mi_w1:"+mission.SlotHandoverCause {
		t.Fatalf("信号先行次序: %v", stopper.calls)
	}
}

// TestHandoverPortsNoRunNoSignal 无未终态 Run：不发信号（幂等面干净）。
func TestHandoverPortsNoRunNoSignal(t *testing.T) {
	mSvc, mem := newHandoverWiring(t)
	runningSlotMission(t, mem)
	stopper := &fakeRunStopper{unfin: map[string]bool{}}
	ports := handoverPorts(mSvc, stopper, memory.NewService(memory.NewMemStore(), memory.Anchors{}, func() string { return "2026-11-12T12:00:00Z" }))

	if _, ok, err := ports.ReassignMission(context.Background(), "mi_w1", "ag_a", "ag_b"); err != nil || !ok {
		t.Fatalf("转移: ok=%v %v", ok, err)
	}
	stopper.mu.Lock()
	defer stopper.mu.Unlock()
	if len(stopper.calls) != 1 || stopper.calls[0] != "has:mi_w1" {
		t.Fatalf("无在途不得发信号: %v", stopper.calls)
	}
}

// TestHandoverPortsSignalFailureAbortsFlip 信号失败：不翻转（重跑重发信号
// 收敛——不存在"翻转成功而停止信号永不发出"的空洞）。
func TestHandoverPortsSignalFailureAbortsFlip(t *testing.T) {
	mSvc, mem := newHandoverWiring(t)
	runningSlotMission(t, mem)
	stopper := &fakeRunStopper{unfin: map[string]bool{"mi_w1": true}, signalErr: errors.New("boom")}
	ports := handoverPorts(mSvc, stopper, memory.NewService(memory.NewMemStore(), memory.Anchors{}, func() string { return "2026-11-12T12:00:00Z" }))

	if _, ok, err := ports.ReassignMission(context.Background(), "mi_w1", "ag_a", "ag_b"); err == nil || ok {
		t.Fatalf("信号失败须中止: ok=%v %v", ok, err)
	}
	got, _, _ := mSvc.Get(context.Background(), "mi_w1")
	if got.OwnerAgentID != "ag_a" {
		t.Fatalf("信号失败不得翻转: %+v", got)
	}

	// 修复信号面后重跑：翻转成功（信号先行闭环）。
	stopper.mu.Lock()
	stopper.signalErr = nil
	stopper.mu.Unlock()
	if _, ok, err := ports.ReassignMission(context.Background(), "mi_w1", "ag_a", "ag_b"); err != nil || !ok {
		t.Fatalf("重跑: ok=%v %v", ok, err)
	}
	got, _, _ = mSvc.Get(context.Background(), "mi_w1")
	if got.OwnerAgentID != "ag_b" {
		t.Fatalf("重跑后: %+v", got)
	}
}
