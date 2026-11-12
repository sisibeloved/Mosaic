// activate_test.go —— 0.7.7 / ADR-0044：激活与执行权接管的域级测试
// （takeover/copy 双模式、防双写门、实时复核、幂等重试、重锚映射）。
package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeActPorts 激活写入端口桩：map 驱动 + 写入留痕（映射/幂等断言用）。
type fakeActPorts struct {
	agents      map[string]bool   // 已存在身份（AgentExists）
	newAgent    string            // AgentEnsureImported 固定返回
	agentCalls  []string          // agentID 参数留痕（重试复用断言）
	originIndex map[string]string // originRef → 已建身份（P1#3 找回）

	missionExisting map[string]MissionState // 同 ID 任务（复核 422 注入）
	stateErrOn      string                  // 状态查询失败定向注入目标（复审 #1）
	stateErr        error
	owned           []MissionFact // MissionsOfOwner 返回（origin 查重）
	createdMission  []MissionFact // MissionCreate 留痕
	paused          []string      // MissionPause 留痕
	nextMissionSeq  int

	checkpoints map[string]CheckpointFact // missionID → 最新
	ckptImports []CheckpointFact          // CheckpointImport 留痕
	ckptSeq     int

	memories  map[string][]MemoryFact // "ns|scope" → 已写
	memWrites []string                // MemoryCreate 留痕（memoryID）
	harnesses []string

	memFailOn string // MemoryCreate 失败注入（该 memory_id 一次）
	memFailed bool
}

func (f *fakeActPorts) ports() ActivationPorts {
	return ActivationPorts{
		AgentEnsureImported: func(_ context.Context, _, _, agentID, originRef string) (string, error) {
			f.agentCalls = append(f.agentCalls, agentID)
			// 一审 #3：持久 origin 关联找回（同 wiring——身份行已落即有 source）。
			if id, ok := f.originIndex[originRef]; ok {
				return id, nil
			}
			id := agentID
			if id == "" {
				id = f.newAgent
			}
			if f.agents == nil {
				f.agents = map[string]bool{}
			}
			f.agents[id] = true
			if f.originIndex == nil {
				f.originIndex = map[string]string{}
			}
			f.originIndex[originRef] = id
			return id, nil
		},
		AgentExists: func(_ context.Context, agentID string) (bool, error) {
			return f.agents[agentID], nil
		},
		MissionCreate: func(_ context.Context, owner string, c ContractFacts, originRef, _ string) (MissionRef, error) {
			f.nextMissionSeq++
			id := "mi_local" + strings.Repeat("x", f.nextMissionSeq) // mi_localx / mi_localxx…
			// 一审 #1：端口语义 = 原子 paused（完备契约无 ready 窗口）。
			fact := MissionFact{MissionID: id, ContractVersion: 1, Status: "paused", OwnerAgentID: owner, Contract: c}
			fact.Contract.SourceRefs = append(append([]string{}, c.SourceRefs...), originRef)
			f.createdMission = append(f.createdMission, fact)
			f.owned = append(f.owned, fact)
			if f.missionExisting == nil {
				f.missionExisting = map[string]MissionState{}
			}
			f.missionExisting[id] = MissionState{ContractVersion: 1, Status: "paused"}
			return MissionRef{MissionID: id, ContractVersion: 1, Status: "paused"}, nil
		},
		MissionPause: func(_ context.Context, missionID string, version int, reason string) error {
			f.paused = append(f.paused, missionID)
			_ = version
			_ = reason
			return nil
		},
		MissionState: func(_ context.Context, missionID string) (MissionState, bool, error) {
			if f.stateErr != nil && (f.stateErrOn == "" || f.stateErrOn == missionID) {
				return MissionState{}, false, f.stateErr
			}
			st, ok := f.missionExisting[missionID]
			return st, ok, nil
		},
		MissionsOfOwner: func(_ context.Context, owner string) ([]MissionFact, error) {
			var out []MissionFact
			for _, m := range f.owned {
				if m.OwnerAgentID == owner {
					out = append(out, m)
				}
			}
			return out, nil
		},
		CheckpointImported: func(_ context.Context, missionID, originCheckpointID string) (bool, error) {
			// 复审 #4：全量水位查重（业务与系统两类同覆盖）。
			c, ok := f.checkpoints[missionID]
			if !ok {
				return false, nil
			}
			return c.SourceWatermarks["bundle.origin_checkpoint"] == originCheckpointID, nil
		},
		CheckpointImport: func(_ context.Context, ckpt CheckpointFact, targetMissionID string, targetVersion int, marks map[string]string) error {
			if f.checkpoints == nil {
				f.checkpoints = map[string]CheckpointFact{}
			}
			// 模拟域面（一审 #5/#6）：全新 checkpoint_id/run_id 域生成、Source
			// 保留源语义、水位合并 marks、版本对齐目标。
			f.ckptSeq++
			stored := CheckpointFact{
				CheckpointID: fmt.Sprintf("ckp_new%d", f.ckptSeq),
				RunID:        fmt.Sprintf("run_new%d", f.ckptSeq),
				MissionID:    targetMissionID, MissionVersion: targetVersion,
				CompletedSteps: ckpt.CompletedSteps, PendingSteps: ckpt.PendingSteps,
				ArtifactRefs: ckpt.ArtifactRefs, ActionReceipts: ckpt.ActionReceipts,
				UncertainActions: ckpt.UncertainActions, WaitingConditions: ckpt.WaitingConditions,
				SourceWatermarks: mergeWatermarks(ckpt.SourceWatermarks, marks),
				ProgressSummary:  ckpt.ProgressSummary, Source: ckpt.Source,
			}
			f.checkpoints[targetMissionID] = stored
			f.ckptImports = append(f.ckptImports, stored)
			return nil
		},
		MemoryCreate: func(_ context.Context, item MemoryFact, scopeID, originRef string) error {
			if item.MemoryID == f.memFailOn && !f.memFailed {
				f.memFailed = true
				return errors.New("injected failure")
			}
			if f.memories == nil {
				f.memories = map[string][]MemoryFact{}
			}
			stored := item
			stored.ScopeID = scopeID
			stored.SourceRefs = append(append([]string{}, item.SourceRefs...), originRef)
			f.memories[item.Namespace+"|"+scopeID] = append(f.memories[item.Namespace+"|"+scopeID], stored)
			f.memWrites = append(f.memWrites, item.MemoryID)
			return nil
		},
		MemoryList: func(_ context.Context, ns, scopeID string) ([]MemoryFact, error) {
			return f.memories[ns+"|"+scopeID], nil
		},
		Harnesses: func(context.Context) ([]string, error) { return f.harnesses, nil },
	}
}

// mergeWatermarks 源水位与 marks 合并（测试同 wiring 语义）。
func mergeWatermarks(src, marks map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range src {
		out[k] = v
	}
	for k, v := range marks {
		out[k] = v
	}
	return out
}

// activationFixture 三命名空间夹具：agent/workspace/mission 记忆各一 + 任务 + 检查点。
func activationFixture() *fakePorts {
	fp := exportFixture()
	fp.memories["mission|mi_a"] = []MemoryFact{memItem("mem_m", "mission", "mi_a", "active", "normal", "")}
	// 源契约 v2（missionFact）vs 快照 v1——漂移事实须随水位保留（一审 #4）。
	fp.ckpts["mi_a"] = CheckpointFact{
		CheckpointID: "ck_1", RunID: "run_1", MissionID: "mi_a", MissionVersion: 1,
		CheckpointVersion: 3, Source: "", CompletedSteps: []string{"s1"}, PendingSteps: []string{"s2"},
		CreatedAt: "2026-11-08T10:00:00Z",
	}
	return fp
}

// importForActivation 导入一个三命名空间包，返回（导入服务、激活服务桩、目录）。
func importForActivation(t *testing.T, ports *fakeActPorts) (*ImportService, *ActivationService, string) {
	t.Helper()
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, dir := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatalf("import: %v", err)
	}
	return imp, NewActivation(imp, ports.ports()), dir
}

func TestActivateCopyFresh(t *testing.T) {
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	imp, act, dir := importForActivation(t, ports)

	view, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if view.Mode != ModeCopy || view.Status != "activated" || view.LocalAgentID != "ag_new" || view.OriginAgentID != "ag_ok" {
		t.Fatalf("view: %+v", view)
	}
	if view.ConfirmSourceStoppedAt != "" {
		t.Fatalf("copy 无须确认: %q", view.ConfirmSourceStoppedAt)
	}
	// 任务映射 + paused 承接。
	if len(view.Missions) != 1 || view.Missions[0].OriginMissionID != "mi_a" || !strings.HasPrefix(view.Missions[0].LocalMissionID, "mi_local") || view.Missions[0].Status != "paused" {
		t.Fatalf("missions: %+v", view.Missions)
	}
	if len(ports.createdMission) != 1 || ports.createdMission[0].Status != "paused" {
		t.Fatalf("原子 paused 创建（一审 #1——无 ready 窗口）: %+v", ports.createdMission)
	}
	// 检查点：目标全新 ID/run（一审 #5——源主键不随行）、源语义保留、
	// 版本对齐新任务、源版本事实入水位（一审 #4——漂移不丢）。
	if len(ports.ckptImports) != 1 {
		t.Fatalf("ckptImports: %+v", ports.ckptImports)
	}
	ck := ports.ckptImports[0]
	if ck.CheckpointID == "ck_1" || ck.RunID == "run_1" {
		t.Fatalf("目标侧须全新 ID/run: %+v", ck)
	}
	if ck.MissionID != view.Missions[0].LocalMissionID || ck.MissionVersion != 1 || ck.Source != "" {
		t.Fatalf("ckpt 重锚/源语义: %+v", ck)
	}
	if ck.SourceWatermarks["bundle.origin_checkpoint"] != "ck_1" || ck.SourceWatermarks["bundle.origin_run"] != "run_1" {
		t.Fatalf("origin 水位: %v", ck.SourceWatermarks)
	}
	if ck.SourceWatermarks["bundle.origin_versions"] != "checkpoint=v1:contract=v2" {
		t.Fatalf("源版本事实（漂移保留）: %v", ck.SourceWatermarks)
	}
	// 记忆三命名空间重锚。
	if _, ok := ports.memories["agent|ag_new"]; !ok {
		t.Fatalf("agent 域须重锚新身份: %v", memKeys(ports.memories))
	}
	if _, ok := ports.memories["workspace|default"]; !ok {
		t.Fatalf("workspace 域合入共享: %v", memKeys(ports.memories))
	}
	if _, ok := ports.memories["mission|"+view.Missions[0].LocalMissionID]; !ok {
		t.Fatalf("mission 域随映射: %v", memKeys(ports.memories))
	}
	if view.Counts != (ActivationCounts{MemoriesCreated: 3, MissionsCreated: 1, CheckpointsCreated: 1}) {
		t.Fatalf("counts: %+v", view.Counts)
	}
	// 边车：status=activated + activation 块；attempt 清除。
	stored, err := imp.Get("bdi_t01")
	if err != nil || stored.Status != "activated" || stored.Activation == nil || stored.Activation.LocalAgentID != "ag_new" {
		t.Fatalf("边车: %v %+v", err, stored.Status)
	}
	if _, err := os.Stat(filepath.Join(dir, importDirName, "bdi_t01", attemptName)); !os.IsNotExist(err) {
		t.Fatalf("attempt 须清除: %v", err)
	}
}

func TestActivateTakeoverGates(t *testing.T) {
	t.Run("同身份承接", func(t *testing.T) {
		ports := &fakeActPorts{agents: map[string]bool{"ag_ok": true}, harnesses: []string{"exe_1"}}
		_, act, _ := importForActivation(t, ports)
		view, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeTakeover, ConfirmSourceStopped: true})
		if err != nil {
			t.Fatalf("activate: %v", err)
		}
		if view.LocalAgentID != "ag_ok" || view.ConfirmSourceStoppedAt == "" {
			t.Fatalf("view: %+v", view)
		}
		if _, ok := ports.memories["agent|ag_ok"]; !ok {
			t.Fatalf("记忆须挂既有身份: %v", memKeys(ports.memories))
		}
		if len(ports.agentCalls) != 0 {
			t.Fatalf("takeover 不建身份: %v", ports.agentCalls)
		}
	})
	t.Run("未确认源端已停", func(t *testing.T) {
		ports := &fakeActPorts{agents: map[string]bool{"ag_ok": true}, harnesses: []string{"exe_1"}}
		_, act, _ := importForActivation(t, ports)
		_, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeTakeover})
		if !errors.Is(err, ErrConfirmRequired) {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("模式与事实不匹配", func(t *testing.T) {
		// 有同 ID 却要求 copy → 409。
		ports := &fakeActPorts{agents: map[string]bool{"ag_ok": true}, harnesses: []string{"exe_1"}}
		_, act, _ := importForActivation(t, ports)
		_, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
		if !errors.Is(err, ErrModeMismatch) || !strings.Contains(err.Error(), "takeover") {
			t.Fatalf("err: %v", err)
		}
		// 无同 ID 却要求 takeover → 409。
		ports2 := &fakeActPorts{agents: map[string]bool{}, harnesses: []string{"exe_1"}}
		_, act2, _ := importForActivation(t, ports2)
		_, err = act2.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeTakeover, ConfirmSourceStopped: true})
		if !errors.Is(err, ErrModeMismatch) || !strings.Contains(err.Error(), "copy") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("模式枚举", func(t *testing.T) {
		ports := &fakeActPorts{harnesses: []string{"exe_1"}}
		_, act, _ := importForActivation(t, ports)
		if _, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: "auto"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err: %v", err)
		}
	})
}

func TestActivateRealtimeBlocked(t *testing.T) {
	t.Run("同 ID 任务冲突", func(t *testing.T) {
		ports := &fakeActPorts{
			missionExisting: map[string]MissionState{"mi_a": {ContractVersion: 1, Status: "ready"}},
			harnesses:       []string{"exe_1"},
		}
		_, act, _ := importForActivation(t, ports)
		_, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
		if !errors.Is(err, ErrActivationBlocked) || !strings.Contains(err.Error(), "mi_a") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("依赖缺失", func(t *testing.T) {
		ports := &fakeActPorts{harnesses: []string{}}
		_, act, _ := importForActivation(t, ports)
		_, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
		if !errors.Is(err, ErrActivationBlocked) || !strings.Contains(err.Error(), "exe_1") {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("包重校验不过", func(t *testing.T) {
		ports := &fakeActPorts{harnesses: []string{"exe_1"}}
		imp, act, dir := importForActivation(t, ports)
		zipPath := filepath.Join(dir, importDirName, "bdi_t01", zipName)
		if err := os.WriteFile(zipPath, []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
		if !errors.Is(err, ErrActivationBlocked) {
			t.Fatalf("err: %v", err)
		}
		_ = imp
	})
}

func TestActivateIdempotentReturn(t *testing.T) {
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	_, act, _ := importForActivation(t, ports)
	first, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	created := len(ports.createdMission)
	second, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if second.ActivatedAt != first.ActivatedAt || second.LocalAgentID != first.LocalAgentID {
		t.Fatalf("幂等返回既有结果: %+v vs %+v", first, second)
	}
	if len(ports.createdMission) != created {
		t.Fatalf("重复激活不得重复写: %d", len(ports.createdMission))
	}
}

func TestActivateRetryAfterFailure(t *testing.T) {
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}, memFailOn: "mem_w"}
	imp, act, dir := importForActivation(t, ports)

	// 第一次：任务/检查点已写，记忆 mem_w 失败 → 整体失败保持 inactive，attempt 留痕。
	_, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("err: %v", err)
	}
	stored, _ := imp.Get("bdi_t01")
	if stored.Status != "inactive" {
		t.Fatalf("失败保持 inactive: %s", stored.Status)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, importDirName, "bdi_t01", attemptName))
	if rerr != nil {
		t.Fatalf("attempt 留痕: %v", rerr)
	}
	var att attemptFile
	if json.Unmarshal(raw, &att) != nil || att.TargetAgent != "ag_new" || len(att.Missions) != 1 {
		t.Fatalf("attempt: %s", raw)
	}

	// 第二次：身份经 attempt 复用（不二次建 ID）、任务按映射 skipped、
	// 已写记忆 skipped、失败记忆补写。
	view, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if len(ports.createdMission) != 1 {
		t.Fatalf("任务不得重复建: %d", len(ports.createdMission))
	}
	// attempt 复用后不再调建身份（首次调用参数空 = 域生成；重试零调用）。
	if len(ports.agentCalls) != 1 || ports.agentCalls[0] != "" {
		t.Fatalf("agentCalls: %v", ports.agentCalls)
	}
	if view.Counts.MissionsSkipped != 1 || view.Counts.MissionsCreated != 0 {
		t.Fatalf("counts: %+v", view.Counts)
	}
	// 记忆：首次中止于 mem_w（mem_n 已写、mem_m 未达）；重试补写 mem_w + mem_m。
	if view.Counts.MemoriesCreated != 2 || view.Counts.MemoriesSkipped != 1 {
		t.Fatalf("memories: %+v", view.Counts)
	}
	if len(ports.memWrites) != 3 { // 首次 1 条成功（失败不记）+ 重试补 2 条
		t.Fatalf("memWrites: %v", ports.memWrites)
	}
	// 检查点：首写后重试按源 ID 跳过。
	if len(ports.ckptImports) != 1 || view.Counts.CheckpointsSkipped != 1 {
		t.Fatalf("checkpoint: %+v counts=%+v", ports.ckptImports, view.Counts)
	}
}

func memKeys(m map[string][]MemoryFact) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- 一审反例钉住（P1#2/#3、P2#2/#4）----

// TestActivateResumesLeftoverPause 一审 #2：上次激活"创建成功、暂停失败"的
// 残留任务（origin 标记、ready）——重激活必须补做暂停，不得凭存在就跳过。
func TestActivateResumesLeftoverPause(t *testing.T) {
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// 预置残留：目标身份名下已有 origin 标记任务，状态 ready（暂停失败遗留）。
	originRef := "bundle:bdl_test01:mi_a"
	ports.owned = []MissionFact{{
		MissionID: "mi_left", ContractVersion: 1, Status: "ready", OwnerAgentID: "ag_new",
		Contract: ContractFacts{Objective: "o", SourceRefs: []string{originRef}},
	}}
	ports.missionExisting = map[string]MissionState{"mi_left": {ContractVersion: 1, Status: "ready"}}
	act := NewActivation(imp, ports.ports())

	view, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if len(ports.paused) != 1 || ports.paused[0] != "mi_left" {
		t.Fatalf("须补做暂停: %v", ports.paused)
	}
	if view.Missions[0].LocalMissionID != "mi_left" || view.Missions[0].Status != "paused" {
		t.Fatalf("映射: %+v", view.Missions[0])
	}
	if view.Counts.MissionsSkipped != 1 || len(ports.createdMission) != 0 {
		t.Fatalf("counts: %+v", view.Counts)
	}
}

// TestActivateAgentRecoveryByOrigin 一审 #3：进程中断 attempt 未写——身份行
// 已落库（source=originRef）时经持久 origin 关联找回，不重复建身份。
func TestActivateAgentRecoveryByOrigin(t *testing.T) {
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	ports.originIndex = map[string]string{"bundle:bdl_test01:ag_ok": "ag_new"} // 中断前已建身份
	act := NewActivation(imp, ports.ports())

	view, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if view.LocalAgentID != "ag_new" {
		t.Fatalf("找回身份: %s", view.LocalAgentID)
	}
	if len(ports.agentCalls) != 1 { // 找回路径零新建调用
		t.Fatalf("agentCalls: %v", ports.agentCalls)
	}
}

// TestActivateDualImportsSameCheckpoint 一审 #5：同源检查点（ck_1）经两个
// 导入记录各自激活——目标侧全新 ID 使两条落库并存（源主键沿用会撞 UNIQUE）。
func TestActivateDualImportsSameCheckpoint(t *testing.T) {
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp1, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp1.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// 第二个包：同源任务二次导出——bundle_id 不同、源 checkpoint_id 相同。
	files := unpackZip(t, raw)
	files["manifest.json"] = bytes.ReplaceAll(files["manifest.json"],
		[]byte(`"bundle_id": "bdl_test01"`), []byte(`"bundle_id": "bdl_test02"`))
	dir2 := t.TempDir()
	imp2 := NewImporter(emptyImportPorts().ports(), dir2, "0.7.7-dev",
		WithImporterNow(fixedLaterNow), WithImporterNewID(func() string { return "bdi_t02" }))
	if _, err := imp2.Import(context.Background(), bytes.NewReader(buildZip(t, files))); err != nil {
		t.Fatal(err)
	}
	if _, err := (NewActivation(imp1, ports.ports())).Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy}); err != nil {
		t.Fatal(err)
	}
	if _, err := (NewActivation(imp2, ports.ports())).Activate(context.Background(), "bdi_t02", ActivationRequest{Mode: ModeCopy}); err != nil {
		t.Fatalf("第二包激活不得撞源主键: %v", err)
	}
	if len(ports.ckptImports) != 2 || ports.ckptImports[0].CheckpointID == ports.ckptImports[1].CheckpointID {
		t.Fatalf("两条目标检查点须并存且 ID 不同: %+v", ports.ckptImports)
	}
	if len(ports.createdMission) != 2 {
		t.Fatalf("两任务: %d", len(ports.createdMission))
	}
}

// TestActivateSystemCheckpointSourceKept 一审 #6：源 system 终态摘要的
// Source 语义保留——不被统一改写为业务快照（否则遮蔽目标新 system 摘要）。
func TestActivateSystemCheckpointSourceKept(t *testing.T) {
	fp := activationFixture()
	ck := fp.ckpts["mi_a"]
	ck.Source = "system"
	fp.ckpts["mi_a"] = ck
	raw, _ := exportZip(t, fp, ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	if _, err := (NewActivation(imp, ports.ports())).Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy}); err != nil {
		t.Fatal(err)
	}
	if len(ports.ckptImports) != 1 || ports.ckptImports[0].Source != "system" {
		t.Fatalf("Source 保留源语义: %+v", ports.ckptImports)
	}
}

// ---- 复审反例钉住 ----

// TestActivateStateProbeFailureAborts 复审 #1：任务状态核验失败必须中止
// （不得默认 paused 继续——实际 ready 任务未暂停也报 activated）。
func TestActivateStateProbeFailureAborts(t *testing.T) {
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// 预置 origin 查重命中的残留任务 + 状态查询失败注入。
	ports.owned = []MissionFact{{
		MissionID: "mi_left", ContractVersion: 1, Status: "ready", OwnerAgentID: "ag_new",
		Contract: ContractFacts{Objective: "o", SourceRefs: []string{"bundle:bdl_test01:mi_a"}},
	}}
	ports.stateErrOn, ports.stateErr = "mi_left", errors.New("probe down")
	act := NewActivation(imp, ports.ports())
	if _, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy}); err == nil || !strings.Contains(err.Error(), "状态核验") {
		t.Fatalf("查询失败须中止: %v", err)
	}
	stored, _ := imp.Get("bdi_t01")
	if stored.Status != "inactive" {
		t.Fatalf("中止保持 inactive: %s", stored.Status)
	}
}

// TestActivateAttemptMappingReverified 复审 #1：attempt 命中不采信记录状态
// ——重试前任务被恢复为 ready 的，须补做暂停并如实报告。
func TestActivateAttemptMappingReverified(t *testing.T) {
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"},
		missionExisting: map[string]MissionState{"mi_left": {ContractVersion: 1, Status: "ready"}}}
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	// 预置 attempt：映射记录 paused——但任务实际已被恢复为 ready。
	os.WriteFile(filepath.Join(imp.dir, importDirName, "bdi_t01", attemptName),
		[]byte(`{"mode":"copy","target_agent_id":"ag_new","missions":[{"origin_mission_id":"mi_a","local_mission_id":"mi_left","status":"paused"}]}`), 0o600)
	act := NewActivation(imp, ports.ports())
	view, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if len(ports.paused) != 1 || ports.paused[0] != "mi_left" {
		t.Fatalf("attempt 命中也须补暂停: %v", ports.paused)
	}
	if view.Missions[0].LocalMissionID != "mi_left" || view.Missions[0].Status != "paused" {
		t.Fatalf("映射如实: %+v", view.Missions[0])
	}
}

// TestActivateSystemCheckpointDedup 复审 #4：system 检查点已写、记忆失败
// 后重试——全量水位查重不漏（不产生第二条）。
func TestActivateSystemCheckpointDedup(t *testing.T) {
	fp := activationFixture()
	ck := fp.ckpts["mi_a"]
	ck.Source = "system"
	fp.ckpts["mi_a"] = ck
	raw, _ := exportZip(t, fp, ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}, memFailOn: "mem_w"}
	act := NewActivation(imp, ports.ports())
	if _, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy}); err == nil {
		t.Fatal("首激活应失败于注入")
	}
	view, err := act.Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if len(ports.ckptImports) != 1 || view.Counts.CheckpointsSkipped != 1 {
		t.Fatalf("system 条重试须查重命中: %+v counts=%+v", ports.ckptImports, view.Counts)
	}
}

// TestActivateChainMigrationPreservesDrift 三审 #1：连续迁移（A→B→C）链上
// 未解决的源契约漂移证据不被再次重映射覆盖——B 侧包内检查点自带
// origin_versions（A 的事实）且快照未更新时，C 激活保留原证据；新快照
// （无证据）才按本跳事实生成。
func TestActivateChainMigrationPreservesDrift(t *testing.T) {
	// B 侧包：任务 v1（B 承接时新建）+ 检查点 mission_version=1（B 重映射）
	// 且自带 A 的漂移证据。
	fpB := exportFixture()
	fpB.missions["ag_ok"] = []MissionFact{missionFact("mi_a", "paused")}
	fpB.missions["ag_ok"][0].ContractVersion = 1
	fpB.ckpts["mi_a"] = CheckpointFact{
		CheckpointID: "ck_1", RunID: "run_1", MissionID: "mi_a", MissionVersion: 1,
		CheckpointVersion: 1, CompletedSteps: []string{"s1"},
		SourceWatermarks: map[string]string{"bundle.origin_versions": "checkpoint=v1:contract=v2"},
		CreatedAt:        "2026-11-12T10:00:00Z",
	}
	raw, _ := exportZip(t, fpB, ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	if _, err := (NewActivation(imp, ports.ports())).Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy}); err != nil {
		t.Fatal(err)
	}
	if len(ports.ckptImports) != 1 {
		t.Fatalf("ckptImports: %+v", ports.ckptImports)
	}
	wm := ports.ckptImports[0].SourceWatermarks
	if wm["bundle.origin_versions"] != "checkpoint=v1:contract=v2" {
		t.Fatalf("链上未解决漂移须保留 A 的证据（不得被 v1:contract=v1 覆盖）: %v", wm)
	}
	// 溯源定位水位记本跳（最近一次迁移）；漂移证据保最早——两类水位分工。
	if wm["bundle.origin_checkpoint"] != "ck_1" {
		t.Fatalf("本跳来源定位: %v", wm)
	}
}

// TestActivateFreshSnapshotGeneratesDrift 三审 #1 对照：新快照（不带证据）
// 按本跳事实生成——含漂移照记。
func TestActivateFreshSnapshotGeneratesDrift(t *testing.T) {
	raw, _ := exportZip(t, activationFixture(), ScopeRequest{Namespaces: []string{nsAgent, nsWorkspace, nsMission}})
	imp, _ := newImporterForTest(t, emptyImportPorts())
	if _, err := imp.Import(context.Background(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	ports := &fakeActPorts{newAgent: "ag_new", harnesses: []string{"exe_1"}}
	if _, err := (NewActivation(imp, ports.ports())).Activate(context.Background(), "bdi_t01", ActivationRequest{Mode: ModeCopy}); err != nil {
		t.Fatal(err)
	}
	wm := ports.ckptImports[0].SourceWatermarks
	if wm["bundle.origin_versions"] != "checkpoint=v1:contract=v2" {
		t.Fatalf("新快照按本跳事实生成: %v", wm)
	}
}
