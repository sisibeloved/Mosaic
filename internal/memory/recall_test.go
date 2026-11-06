// 召回测试（0.7.2 / ADR-0039）：命中集/大小写/total 计数（过滤先行——
// 授权与时效排除项不计）/召回模式无游标。
package memory

import (
	"context"
	"testing"
)

func mkRecall(t *testing.T, svc *Service, ns Namespace, title, content string) Item {
	t.Helper()
	cmd := CreateCmd{Namespace: ns, ScopeID: "ag_1", Kind: KindSemantic,
		Title: title, Content: content, ObservedAt: "2026-11-05T10:00:00Z"}
	if ns == NSWorkspace {
		cmd.ScopeID = WorkspaceScope
	}
	it, err := svc.Create(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestRecallMemStore(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), Anchors{}, fixedNow)
	mkRecall(t, svc, NSAgent, "部署流程", "先建单后执行，验收前自查。")
	mkRecall(t, svc, NSAgent, "Weekly Report", "Export the report every Monday.")
	mkRecall(t, svc, NSWorkspace, "异域同词", "部署流程也在本条——不应出现在 agent 召回")

	t.Run("命中集 + total", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "部署流程"})
		if err != nil || len(res.Items) != 1 || res.TotalHits != 1 {
			t.Fatalf("中文召回错：%d/%d %v", len(res.Items), res.TotalHits, err)
		}
		if res.Items[0].Title != "部署流程" {
			t.Fatalf("命中错：%s", res.Items[0].Title)
		}
		if res.NextCursor != "" {
			t.Fatal("召回模式不应有游标")
		}
	})
	t.Run("大小写不敏感（与 LIKE 基准同语义）", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "report"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("大小写召回错：%d %v", res.TotalHits, err)
		}
	})
	t.Run("title 与 content 都可命中", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "验收前自查"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("content 命中错：%d %v", res.TotalHits, err)
		}
	})
	t.Run("过滤先行——异域命中不计入 total 与结果", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "部署流程"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("异域词条泄漏：%d %v", res.TotalHits, err)
		}
	})
	t.Run("status/时效排除项不计 total", func(t *testing.T) {
		// 过期条目（含查询词）默认口径不计。
		exp := CreateCmd{Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
			Title: "部署流程（旧版）", Content: "c", ObservedAt: "2026-11-01T10:00:00Z",
			ValidFrom: "2026-11-01T00:00:00Z", ValidTo: "2026-11-02T00:00:00Z"}
		if _, err := svc.Create(ctx, exp); err != nil {
			t.Fatal(err)
		}
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "部署流程"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("过期命中应不计 total：%d %v", res.TotalHits, err)
		}
		// include_expired 后计入。
		res, err = svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "部署流程", IncludeExpired: true})
		if err != nil || res.TotalHits != 2 {
			t.Fatalf("include_expired 应 2：%d %v", res.TotalHits, err)
		}
	})
	t.Run("召回模式 limit 截断但 total 全量", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "部署流程", IncludeExpired: true, Limit: 1})
		if err != nil || len(res.Items) != 1 || res.TotalHits != 2 {
			t.Fatalf("截断/计数错：%d/%d %v", len(res.Items), res.TotalHits, err)
		}
	})
	t.Run("一审 #2：字面 %/_ 不作通配（与 sqlite ESCAPE 同语义）", func(t *testing.T) {
		mkRecall(t, svc, NSAgent, "折扣 100%OFF", "c")
		mkRecall(t, svc, NSAgent, "普通甲", "c")
		mkRecall(t, svc, NSAgent, "普通乙", "c")
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "100%"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("字面 %% 应恰 1：%d %v", res.TotalHits, err)
		}
		res, err = svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "_"})
		if err != nil || res.TotalHits != 0 {
			t.Fatalf("字面 _ 应 0（无含该字符条目）：%d %v", res.TotalHits, err)
		}
	})
	t.Run("一审 #3：每页 total 恒定（七条 limit=2）", func(t *testing.T) {
		svc2 := NewService(NewMemStore(), Anchors{}, fixedNow)
		for i := 0; i < 7; i++ {
			cmd := CreateCmd{Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
				Title: "页" + string(rune('A'+i)), Content: "c", ObservedAt: "2026-11-05T10:00:00Z"}
			if _, err := svc2.Create(ctx, cmd); err != nil {
				t.Fatal(err)
			}
		}
		res, err := svc2.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2})
		if err != nil || res.TotalHits != 7 {
			t.Fatalf("首页 total 应 7，得 %d %v", res.TotalHits, err)
		}
		for res.NextCursor != "" {
			res, err = svc2.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2, Before: res.NextCursor})
			if err != nil {
				t.Fatal(err)
			}
			if res.TotalHits != 7 {
				t.Fatalf("续页 total 应恒 7，得 %d", res.TotalHits)
			}
		}
	})
	t.Run("二审 #1：游标条撤回后续页不空（域内定位先于状态过滤）", func(t *testing.T) {
		svc3 := NewService(NewMemStore(), Anchors{}, fixedNow)
		var ids []string
		for i := 0; i < 4; i++ {
			cmd := CreateCmd{Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
				Title: "游标" + string(rune('A'+i)), Content: "c", ObservedAt: "2026-11-05T10:00:00Z"}
			it, err := svc3.Create(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, it.MemoryID)
		}
		p1, err := svc3.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2})
		if err != nil || len(p1.Items) != 2 || p1.NextCursor == "" {
			t.Fatalf("首页错：%d %v", len(p1.Items), err)
		}
		// 首页末条（游标条）被撤回——续页仍应取到更早 2 条。
		if _, err := svc3.Reject(ctx, p1.NextCursor, "撤回游标条"); err != nil {
			t.Fatal(err)
		}
		p2, err := svc3.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2, Before: p1.NextCursor})
		if err != nil || len(p2.Items) != 2 {
			t.Fatalf("游标条撤回后续页应 2 条，得 %d %v", len(p2.Items), err)
		}
		if p2.Items[0].Title != "游标B" || p2.Items[1].Title != "游标A" {
			t.Fatalf("续页序错：%s %s", p2.Items[0].Title, p2.Items[1].Title)
		}
		if p2.TotalHits != 3 { // 4 - 撤回 1
			t.Fatalf("total 应 3，得 %d", p2.TotalHits)
		}
	})
	t.Run("二审 #1：游标条过期后续页不空（时效过滤同解）", func(t *testing.T) {
		// 注入时钟推进使游标条过期。
		now := "2026-11-05T12:00:00Z"
		svc4 := NewService(NewMemStore(), Anchors{}, func() string { return now })
		var last string
		for i := 0; i < 4; i++ {
			cmd := CreateCmd{Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
				Title: "时效游标" + string(rune('A'+i)), Content: "c", ObservedAt: "2026-11-05T10:00:00Z"}
			it, err := svc4.Create(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			last = it.MemoryID
			_ = last
		}
		p1, err := svc4.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		// 游标条（首页末条）置有效期止于 now 之前——续页不应漏。
		exp := "2026-11-05T11:00:00Z"
		if _, err := svc4.Revise(ctx, p1.NextCursor, ReviseCmd{ExpectedVersion: 1, ValidTo: &exp}); err != nil {
			t.Fatal(err)
		}
		p2, err := svc4.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2, Before: p1.NextCursor})
		if err != nil || len(p2.Items) != 2 {
			t.Fatalf("游标条过期后续页应 2 条，得 %d %v", len(p2.Items), err)
		}
	})
	t.Run("空查询词 = 非召回（既有列表语义）", func(t *testing.T) {
		// 本测试 svc 上 active 条目：初始 2 + 字面转义用例新增 3 = 5。
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "  "})
		if err != nil || len(res.Items) != 5 || res.TotalHits != 5 {
			t.Fatalf("空白词应走列表：%d/%d %v", len(res.Items), res.TotalHits, err)
		}
	})
}
