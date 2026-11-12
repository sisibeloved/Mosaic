// 续办层测试（0.7.4 / ADR-0041）：层装入/漂移标记/无检查点 omitted。
package execctx

import (
	"context"
	"testing"
)

func fixtureWithCheckpoint(t *testing.T, facts *CheckpointFacts, fail bool) *Service {
	t.Helper()
	newFixture(t)
	svc := New(NewMemStore(), Ports{
		Missions: fakeMissions{m: testMission(), ok: true},
		Bindings: fakeBindings{facts: map[string]BindingFacts{"bd_1": {Binding: testBinding("ag_own", slotBindingActiveFor()), Spec: testSpec()}}},
		Grants:   &fakeGrants{grant: fullGrant(), ok: true},
		Rooms:    &fakeRooms{events: map[string][]RoomEvent{}},
		Checkpoints: func(context.Context, string) (*CheckpointFacts, bool, error) {
			if fail {
				return nil, false, context.DeadlineExceeded
			}
			if facts == nil {
				return nil, false, nil
			}
			return facts, true, nil
		},
	})
	return svc
}

func slotBindingActiveFor() string { return "active" }

func TestCheckpointLayerAssembled(t *testing.T) {
	facts := &CheckpointFacts{
		CheckpointID: "ckp_1", RunID: "run_1",
		CheckpointVersion: 2, MissionVersion: 3,
		CompletedSteps: []string{"采集"}, PendingSteps: []string{"清洗"},
		ProgressSummary: "进行中",
	}
	svc := fixtureWithCheckpoint(t, facts, false)
	res, err := svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatal(err)
	}
	l := layerOf(t, res.Manifest, LayerCheckpoint)
	if l.State != LayerIncluded {
		t.Fatalf("checkpoint 层应装入：%+v", l)
	}
	cf, ok := l.Content.(CheckpointFacts)
	if !ok {
		t.Fatalf("层内容应 CheckpointFacts：%T", l.Content)
	}
	if cf.CheckpointID != "ckp_1" || len(cf.CompletedSteps) != 1 {
		t.Fatalf("层内容错：%+v", cf)
	}
	// testMission 契约版本 3 == MissionVersion 3 → 无漂移。
	if cf.ContractDrift {
		t.Fatalf("版本一致不应漂移：%+v", cf)
	}
}

func TestCheckpointLayerDriftMarked(t *testing.T) {
	facts := &CheckpointFacts{CheckpointID: "ckp_2", MissionVersion: 7}
	svc := fixtureWithCheckpoint(t, facts, false)
	res, err := svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatal(err)
	}
	l := layerOf(t, res.Manifest, LayerCheckpoint)
	cf, ok := l.Content.(CheckpointFacts)
	if !ok || !cf.ContractDrift {
		t.Fatalf("版本漂移应标记：%+v", l.Content)
	}
}

func TestCheckpointLayerOmittedWhenNone(t *testing.T) {
	svc := fixtureWithCheckpoint(t, nil, false)
	res, err := svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatal(err)
	}
	l := layerOf(t, res.Manifest, LayerCheckpoint)
	if l.State != LayerOmitted || l.ReasonCode != ReasonCheckpointUnavailable {
		t.Fatalf("无检查点应 omitted（首跑常态）：%+v", l)
	}
}

func TestCheckpointLayerReadErrorBlocks(t *testing.T) {
	svc := fixtureWithCheckpoint(t, nil, true)
	if _, err := svc.Build(context.Background(), "mi_1", "bd_1"); err == nil {
		t.Fatal("读取失败应报错（不静默 omitted——§7.3 恢复面不能丢事实）")
	}
}

// TestOriginDriftMergesIntoContractDrift 0.7.7 复审 #3：导入检查点的源侧
// 契约漂移（版本重映射后差值不可表达）经 OriginDrift 传导——续办上下文
// 的 contract_drift 为真且源版本证据随层携带（testMission 契约 v3 ==
// MissionVersion v3 → 重映射无漂移，仅源侧漂移成立）。
func TestOriginDriftMergesIntoContractDrift(t *testing.T) {
	facts := &CheckpointFacts{
		CheckpointID: "ckp_new1", MissionVersion: 3, // == 契约版本（重映射后无差）
		OriginDrift: true, OriginVersions: "checkpoint=v1:contract=v2",
	}
	svc := fixtureWithCheckpoint(t, facts, false)
	res, err := svc.Build(context.Background(), "mi_1", "bd_1")
	if err != nil {
		t.Fatal(err)
	}
	cf, ok := layerOf(t, res.Manifest, LayerCheckpoint).Content.(CheckpointFacts)
	if !ok {
		t.Fatalf("层内容应 CheckpointFacts")
	}
	if !cf.ContractDrift {
		t.Fatalf("源侧漂移须并入 contract_drift：%+v", cf)
	}
	if cf.OriginVersions != "checkpoint=v1:contract=v2" {
		t.Fatalf("源版本证据随层携带：%+v", cf)
	}
}
