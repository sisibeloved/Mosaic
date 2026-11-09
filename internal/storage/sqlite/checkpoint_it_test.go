// 检查点存储 IT（0.7.3 / ADR-0040 / migration v34）：建表登记 / 版本原子
// 分配（并发）/ 降序 / JSON 列读回。
package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/checkpoint"
)

func openCkptStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ckpt-it.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ckptCmd(runID, missionID string, v int) checkpoint.CreateCmd {
	return checkpoint.CreateCmd{
		RunID: runID, MissionID: missionID, MissionVersion: v,
		CompletedSteps: []string{"采集"}, PendingSteps: []string{"清洗"},
		ArtifactRefs:     []string{"artifact:a1"},
		SourceWatermarks: map[string]string{"room:rm_1": "42"},
		ProgressSummary:  "进行中",
	}
}

func TestCheckpointMigrationV34IT(t *testing.T) {
	s := openCkptStore(t)
	defer s.Close()
	var got int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM migrations WHERE version = 34`).Scan(&got); err != nil || got != 1 {
		t.Fatalf("v34 未登记：%v %d", err, got)
	}
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='checkpoints'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("checkpoints 未建：%v %d", err, n)
	}
}

// 一审 #1：v35 source 列 + sqlite LatestOfMission 排除 system。
func TestCheckpointSystemSourceIT(t *testing.T) {
	s := openCkptStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := checkpoint.NewService(s, checkpoint.Anchors{}, func() string { return "2026-11-06T12:00:00Z" })
	biz := checkpoint.CreateCmd{RunID: "run_s1", MissionID: "m_s", MissionVersion: 1,
		CompletedSteps: []string{"业务步骤"}}
	if _, err := svc.Create(ctx, biz); err != nil {
		t.Fatal(err)
	}
	sys := checkpoint.CreateCmd{RunID: "run_s1", MissionID: "m_s", MissionVersion: 1,
		ProgressSummary: "Run run_s1 终态 completed", Source: "system"}
	if _, err := svc.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := s.LatestOfMission(ctx, "m_s")
	if err != nil || !ok {
		t.Fatalf("LatestOfMission：%v %v", ok, err)
	}
	if cp.Source == "system" || len(cp.CompletedSteps) != 1 {
		t.Fatalf("系统摘要遮蔽业务快照：%+v", cp)
	}
	// 全列表含系统条（行数 2）。
	list, _ := s.CheckpointsOfRun(ctx, "run_s1")
	if len(list) != 2 {
		t.Fatalf("历史应 2 条：%d", len(list))
	}
}

func TestCheckpointCRUDAndVersioningIT(t *testing.T) {
	s := openCkptStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := checkpoint.NewService(s, checkpoint.Anchors{}, func() string { return "2026-11-06T12:00:00Z" })
	// 串行三级。
	for i := 1; i <= 3; i++ {
		cp, err := svc.Create(ctx, ckptCmd("run_it_1", "m_1", 2))
		if err != nil {
			t.Fatal(err)
		}
		if cp.CheckpointVersion != i {
			t.Fatalf("版本应 %d，得 %d", i, cp.CheckpointVersion)
		}
	}
	// 并发 10 落盘——(run_id, checkpoint_version) 唯一 + 事务 MAX+1 不撞号。
	var wg sync.WaitGroup
	errCh := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Create(ctx, ckptCmd("run_it_1", "m_1", 2)); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	list, err := s.CheckpointsOfRun(ctx, "run_it_1")
	if err != nil || len(list) != 13 {
		t.Fatalf("应 13 条，得 %d %v", len(list), err)
	}
	if list[0].CheckpointVersion != 13 || list[12].CheckpointVersion != 1 {
		t.Fatalf("降序错：%d…%d", list[0].CheckpointVersion, list[12].CheckpointVersion)
	}
	// JSON 列读回。
	first := list[0]
	if first.CompletedSteps[0] != "采集" || first.PendingSteps[0] != "清洗" ||
		first.ArtifactRefs[0] != "artifact:a1" || first.SourceWatermarks["room:rm_1"] != "42" {
		t.Fatalf("JSON 列读回错：%+v", first)
	}
	// 异 run 隔离。
	other, err := svc.Create(ctx, ckptCmd("run_it_2", "m_1", 2))
	if err != nil || other.CheckpointVersion != 1 {
		t.Fatalf("异 run 版本应从 1 起：%d %v", other.CheckpointVersion, err)
	}
	list2, _ := s.CheckpointsOfRun(ctx, "run_it_2")
	if len(list2) != 1 {
		t.Fatalf("run 隔离错：%d", len(list2))
	}
}
