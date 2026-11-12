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

// 0.7.4：任务级最新（跨 Run——续办装配入口）。
func TestLatestOfMission(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
	cmdA := okCmd() // run_1
	cmdB := okCmd()
	cmdB.RunID = "run_2"
	cmdC := okCmd()
	cmdC.RunID = "run_1"
	for _, c := range []CreateCmd{cmdA, cmdB, cmdC} {
		if _, err := svc.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// 插入序：A(run_1 v1) B(run_2 v1) C(run_1 v2)——任务级最新 = C。
	cp, err := svc.LatestOfMission(ctx, "m_1")
	if err != nil || cp.CheckpointID == "" || cp.RunID != "run_1" || cp.CheckpointVersion != 2 {
		t.Fatalf("任务级最新应 C（run_1 v2），得 run=%s v=%d %v", cp.RunID, cp.CheckpointVersion, err)
	}
	// 无落盘。
	if _, err := svc.LatestOfMission(ctx, "m_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound，得 %v", err)
	}
}

// 一审 #1：系统终态摘要不遮蔽业务快照——LatestOfMission 取最新**业务**条。
func TestSystemSummaryDoesNotShadowBusiness(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
	biz := okCmd()
	biz.CompletedSteps = []string{"采集", "清洗"}
	if _, err := svc.Create(ctx, biz); err != nil {
		t.Fatal(err)
	}
	sys := okCmd()
	sys.RunID = "run_1"
	sys.Source = "system"
	sys.CompletedSteps = nil
	sys.PendingSteps = nil
	sys.ProgressSummary = "Run run_1 终态 completed"
	if _, err := svc.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	cp, err := svc.LatestOfMission(ctx, "m_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.CompletedSteps) != 2 {
		t.Fatalf("系统摘要遮蔽业务快照：%+v", cp)
	}
	if cp.Source == "system" {
		t.Fatal("LatestOfMission 不得返回系统条")
	}
	// 历史仍可查（CheckpointsOfRun 含系统条）。
	list, _ := svc.ListOfRun(ctx, "run_1")
	if len(list) != 2 {
		t.Fatalf("历史应含系统条：%d", len(list))
	}
	// 非法 source 拒。
	bad := okCmd()
	bad.Source = "other"
	if _, err := svc.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("source 非法应拒：%v", err)
	}
}

// 二审 #1：无业务快照时回退系统摘要——续办层不丢停止/终态事实。
func TestLatestForResumeFallbackToSystem(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
	// 仅系统条。
	sys := okCmd()
	sys.Source = "system"
	sys.ProgressSummary = "Run run_1 终态 canceled：暂停"
	if _, err := svc.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	cp, err := svc.LatestForResume(ctx, "m_1")
	if err != nil || cp.Source != "system" || cp.ProgressSummary != "Run run_1 终态 canceled：暂停" {
		t.Fatalf("应回退系统摘要：%+v %v", cp, err)
	}
	// 后落业务条 → 业务优先（一审 #1 语义保持）。
	biz := okCmd()
	biz.CompletedSteps = []string{"采集"}
	if _, err := svc.Create(ctx, biz); err != nil {
		t.Fatal(err)
	}
	cp, err = svc.LatestForResume(ctx, "m_1")
	if err != nil || cp.Source == "system" || len(cp.CompletedSteps) != 1 {
		t.Fatalf("有业务条时系统条不得入选：%+v", cp)
	}
	// 两者皆无。
	if _, err := svc.LatestForResume(ctx, "m_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound：%v", err)
	}
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

// ---- CreateImported（0.7.7 / ADR-0044；一审 #4/#5/#6 语义钉住）----

func TestCreateImported(t *testing.T) {
	ctx := context.Background()

	t.Run("全新 ID/run 域生成 + Source 保留源语义 + 水位合并", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
		cmd := okCmd()
		cmd.SourceWatermarks = map[string]string{"room.events": "e9"}
		cp, err := svc.CreateImported(ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(cp.CheckpointID, "ckp_") || !strings.HasPrefix(cp.RunID, "run_") {
			t.Fatalf("目标全新 ID/run: %s %s", cp.CheckpointID, cp.RunID)
		}
		if cp.Source != cmd.Source {
			t.Fatalf("Source 保留源语义: %q", cp.Source)
		}
		if cp.SourceWatermarks["room.events"] != "e9" {
			t.Fatalf("源水位: %v", cp.SourceWatermarks)
		}
		// 版本由存储按新 run 原子分配（首条 v1）。
		if cp.CheckpointVersion != 1 {
			t.Fatalf("checkpoint_version: %d", cp.CheckpointVersion)
		}
	})

	t.Run("system 终态摘要语义保留（不遮蔽目标新摘要）", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
		cmd := okCmd()
		cmd.Source = "system"
		cp, err := svc.CreateImported(ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if cp.Source != "system" {
			t.Fatalf("system 语义保留: %q", cp.Source)
		}
	})

	t.Run("内容校验同源（一审 #5：免源 run 锚不免内容约束）", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
		cmd := okCmd()
		cmd.PendingSteps = append(cmd.PendingSteps, cmd.CompletedSteps[0]) // 交集
		if _, err := svc.CreateImported(ctx, cmd); !errors.Is(err, ErrInvalid) {
			t.Fatalf("交集应拒: %v", err)
		}
		bad := okCmd()
		bad.Source = "import" // 未知 source 枚举
		if _, err := svc.CreateImported(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("source 枚举应拒: %v", err)
		}
	})

	t.Run("同源两条并存（不同目标 run 不撞）", func(t *testing.T) {
		svc := NewService(NewMemStore(), anchorFor("m_1", true), fixedNow)
		a, err := svc.CreateImported(ctx, okCmd())
		if err != nil {
			t.Fatal(err)
		}
		b, err := svc.CreateImported(ctx, okCmd())
		if err != nil {
			t.Fatalf("第二条须并存（一审 #5 反例：源主键沿用会撞 UNIQUE）: %v", err)
		}
		if a.CheckpointID == b.CheckpointID || a.RunID == b.RunID {
			t.Fatalf("全新 ID/run: %s %s", a.CheckpointID, b.CheckpointID)
		}
	})
}
