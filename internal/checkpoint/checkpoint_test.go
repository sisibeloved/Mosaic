// 检查点域测试（0.7.3 / ADR-0040）：校验矩阵 / 双锚 / 版本分配 / 降序。
package checkpoint

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func fixedNow() string { return "2026-11-06T12:00:00Z" }

func okCmd() CreateCmd {
	return CreateCmd{
		RunID:            "run_1",
		MissionID:        "m_1",
		MissionVersion:   2,
		CompletedSteps:   []string{"采集数据"},
		PendingSteps:     []string{"清洗数据", "出报告"},
		ArtifactRefs:     []string{"artifact:a1"},
		SourceWatermarks: map[string]string{"room:rm_1": "42"},
		ProgressSummary:  "已完成采集，待清洗。",
	}
}

func anchorFor(missionID string, exists bool) Anchors {
	return Anchors{RunMission: func(context.Context, string) (string, bool, error) {
		return missionID, exists, nil
	}}
}

func TestValidateMatrix(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*CreateCmd)
		want error
	}{
		{"mission_id 空", func(c *CreateCmd) { c.MissionID = " " }, ErrInvalid},
		{"mission_version 零", func(c *CreateCmd) { c.MissionVersion = 0 }, ErrInvalid},
		{"work_item 超长", func(c *CreateCmd) { c.WorkItemID = strings.Repeat("w", 129) }, ErrInvalid},
		{"步骤超长", func(c *CreateCmd) { c.CompletedSteps = []string{strings.Repeat("步", 301)} }, ErrInvalid},
		{"列表超 64", func(c *CreateCmd) { c.PendingSteps = make([]string, 65) }, ErrInvalid},
		{"步骤空白", func(c *CreateCmd) { c.CompletedSteps = []string{"  "} }, ErrInvalid},
		{"交集非空（pending 偷换 completed）", func(c *CreateCmd) {
			c.PendingSteps = append(c.PendingSteps, " 采集数据 ") // trim 后与 completed 相同
		}, ErrInvalid},
		{"摘要超长", func(c *CreateCmd) { c.ProgressSummary = strings.Repeat("摘", 501) }, ErrInvalid},
		{"水位键超长", func(c *CreateCmd) { c.SourceWatermarks[strings.Repeat("k", 65)] = "1" }, ErrInvalid},
		{"合法全集", func(c *CreateCmd) {
			c.WorkItemID = "wi_1"
			c.ActionReceipts = []string{"rcp_1"}
			c.UncertainActions = []string{"步骤 X 是否已完成待核"}
			c.WaitingConditions = []string{"等待用户确认范围"}
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := okCmd()
			tc.mut(&cmd)
			err := cmd.validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("应通过，得 %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("应 %v，得 %v", tc.want, err)
			}
		})
	}
}

func TestAnchorGate(t *testing.T) {
	ctx := context.Background()
	t.Run("Run 不存在 404", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", false), fixedNow)
		if _, err := svc.Create(ctx, okCmd()); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("应 ErrRunNotFound，得 %v", err)
		}
	})
	t.Run("mission 归属不一致 409", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_other", true), fixedNow)
		if _, err := svc.Create(ctx, okCmd()); !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("应 ErrScopeMismatch，得 %v", err)
		}
	})
	t.Run("归属一致通过", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
		if _, err := svc.Create(ctx, okCmd()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("列表/单查 Run 不存在", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", false), fixedNow)
		if _, err := svc.ListOfRun(ctx, "run_404"); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("应 ErrRunNotFound，得 %v", err)
		}
	})
	t.Run("无落盘 Latest 404", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
		if _, err := svc.Latest(ctx, "run_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("应 ErrNotFound，得 %v", err)
		}
	})
}

func TestVersionAllocationAndOrder(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
	// 串行三级。
	for i := 0; i < 3; i++ {
		cp, err := svc.Create(ctx, okCmd())
		if err != nil {
			t.Fatal(err)
		}
		if cp.CheckpointVersion != i+1 {
			t.Fatalf("版本应 %d，得 %d", i+1, cp.CheckpointVersion)
		}
		if !strings.HasPrefix(cp.CheckpointID, "ckp_") {
			t.Fatalf("id 形态错：%s", cp.CheckpointID)
		}
	}
	// 并发 20 落盘——版本不撞号（原子分配）。
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Create(ctx, okCmd()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	items, err := svc.Latest(ctx, "run_1")
	_ = items
	_ = err
	list, err := svc.ListOfRun(ctx, "run_1")
	if err != nil || len(list) != 23 {
		t.Fatalf("应 23 条，得 %d %v", len(list), err)
	}
	seen := map[int]bool{}
	for _, cp := range list {
		if seen[cp.CheckpointVersion] {
			t.Fatalf("版本撞号：%d", cp.CheckpointVersion)
		}
		seen[cp.CheckpointVersion] = true
	}
	if len(seen) != 23 || seen[23] == false || list[0].CheckpointVersion != 23 {
		t.Fatalf("版本序列不连续或降序错：%v", seen)
	}
	// Latest = 最高版本。
	lat, err := svc.Latest(ctx, "run_1")
	if err != nil || lat.CheckpointVersion != 23 {
		t.Fatalf("Latest 应 v23，得 %d %v", lat.CheckpointVersion, err)
	}
}
