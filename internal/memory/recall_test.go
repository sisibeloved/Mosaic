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
	t.Run("空查询词 = 非召回（既有列表语义）", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Query: "  "})
		if err != nil || len(res.Items) != 2 || res.TotalHits != 2 {
			t.Fatalf("空白词应走列表：%d/%d %v", len(res.Items), res.TotalHits, err)
		}
	})
}
