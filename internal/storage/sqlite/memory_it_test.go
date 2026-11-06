// 分域记忆存储 IT（0.7.0 / ADR-0037 / migration v32）：新库建表与登记、
// CRUD/过滤/游标（与 memstore 同语义）、幂等重开（旧库升级面——v32 重放）。
package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/memory"
)

// 计时助手（性能粗断言用）。
func timeNow() time.Time                  { return time.Now() }
func timeSince(t time.Time) time.Duration { return time.Since(t) }

const timeSecond = time.Second

func openMemItStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mem-it.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func memIt(ns memory.Namespace, scope, title string) memory.Item {
	return memory.Item{
		MemoryID: "mem_it_" + title, Version: 1, Namespace: ns, ScopeID: scope,
		Kind: memory.KindSemantic, Title: title, Content: "c-" + title,
		SourceRefs: []string{"room:evt_" + title}, ObservedAt: "2026-11-04T10:00:00Z",
		Confidence: 1.0, Status: memory.StatusActive, Visibility: "namespace",
		Sensitivity: memory.SensNormal, Supersedes: []string{},
		CreatedAt: "2026-11-04T12:00:00Z", UpdatedAt: "2026-11-04T12:00:00Z",
	}
}

func TestMemoryItemsMigrationV32IT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	var got int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM migrations WHERE version = 32`).Scan(&got); err != nil || got != 1 {
		t.Fatalf("v32 未登记：%v %d", err, got)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_items'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("memory_items 未建：%v %d", err, n)
	}
}

func TestMemoryItemsCRUDAndCursorIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	// agent 3 条 + mission 1 条（异域）+ proposed 1 条（同域异状态）。
	for _, title := range []string{"a1", "a2", "a3"} {
		if _, err := s.CreateItem(ctx, memIt(memory.NSAgent, "ag_1", title)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateItem(ctx, memIt(memory.NSMission, "m_1", "m1")); err != nil {
		t.Fatal(err)
	}
	proposed := memIt(memory.NSAgent, "ag_1", "p1")
	proposed.MemoryID, proposed.Status = "mem_it_proposed", memory.StatusProposed
	if _, err := s.CreateItem(ctx, proposed); err != nil {
		t.Fatal(err)
	}

	// 单查。
	it, ok, err := s.ItemByID(ctx, "mem_it_a2")
	if err != nil || !ok || it.Title != "a2" || it.SourceRefs[0] != "room:evt_a2" {
		t.Fatalf("单查错：%v %v %+v", err, ok, it)
	}
	if _, ok, _ := s.ItemByID(ctx, "mem_404"); ok {
		t.Fatal("未知项应 ok=false")
	}

	// 默认 active 域隔离：agent 3 条降序。
	items, _, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 50})
	if err != nil || len(items) != 3 {
		t.Fatalf("agent active 应 3 条，得 %d %v", len(items), err)
	}
	if items[0].Title != "a3" || items[2].Title != "a1" {
		t.Fatalf("rowid 降序错：%s %s", items[0].Title, items[2].Title)
	}
	// proposed 不入默认口径；显式请求可见。
	items, _, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusProposed, Limit: 50})
	if err != nil || len(items) != 1 || items[0].MemoryID != "mem_it_proposed" {
		t.Fatalf("proposed 口径错：%d %v", len(items), err)
	}
	// scope/kind 过滤。
	if items, _, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, ScopeID: "ag_404", Limit: 50}); err != nil || len(items) != 0 {
		t.Fatal("scope 过滤应空")
	}
	if items, _, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Kind: memory.KindEpisodic, Limit: 50}); err != nil || len(items) != 0 {
		t.Fatal("kind 过滤应空")
	}

	// 游标分页（limit=2）：Store 返回 limit+1 条供 service 截断；次页
	// before=mem_it_a2 → [a1]。
	p1, _, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 2})
	if err != nil || len(p1) != 3 {
		t.Fatalf("首页应 limit+1 条，得 %d %v", len(p1), err)
	}
	if p1[0].Title != "a3" || p1[1].Title != "a2" || p1[2].Title != "a1" {
		t.Fatal("首页序错")
	}
	p2, _, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 2, Before: "mem_it_a2"})
	if err != nil || len(p2) != 1 || p2[0].Title != "a1" {
		t.Fatalf("次页错：%d %v", len(p2), err)
	}
	// 游标失效 = 空页。
	p3, _, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 2, Before: "mem_deleted"})
	if err != nil || len(p3) != 0 {
		t.Fatalf("游标失效应空页：%d %v", len(p3), err)
	}
}

// 一审 #1：v32 首版写入的 null 列读回兼容——统一空数组。
func TestMemoryItemsNullRefsCompatIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	it := memIt(memory.NSAgent, "ag_1", "n1")
	it.SourceRefs = nil
	if _, err := s.CreateItem(ctx, it); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ItemByID(ctx, it.MemoryID)
	if err != nil || !ok {
		t.Fatalf("读回：%v %v", err, ok)
	}
	if got.SourceRefs == nil || len(got.SourceRefs) != 0 {
		t.Fatalf("null 列应读回空数组，得 %#v", got.SourceRefs)
	}
}

// 一审 #4：sqlite 游标只在本域定位——外域同 ID = 空页。
func TestMemoryItemsCursorScopedIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CreateItem(ctx, memIt(memory.NSAgent, "ag_1", "a1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItem(ctx, memIt(memory.NSWorkspace, memory.WorkspaceScope, "w1")); err != nil {
		t.Fatal(err)
	}
	// workspace 以 agent 域条目 ID 作 before：子查询限本域不命中 → 0 → 空页
	//（与未知 ID 同语义）。
	items, _, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSWorkspace,
		Status: memory.StatusActive, Limit: 50, Before: "mem_it_a1"})
	if err != nil || len(items) != 0 {
		t.Fatalf("外域游标应空页，得 %d %v", len(items), err)
	}
	// 本域末条游标正常（其后无更早条目 → 空）。
	items, _, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSWorkspace,
		Status: memory.StatusActive, Limit: 50, Before: "mem_it_w1"})
	if err != nil || len(items) != 0 {
		t.Fatalf("本域末条游标应空页，得 %d %v", len(items), err)
	}
	// agent 域列表不受影响。
	items, _, err = s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 50})
	if err != nil || len(items) != 1 {
		t.Fatalf("agent 列表应 1 条，得 %d %v", len(items), err)
	}
}

// 策展动作 IT（0.7.1 / ADR-0038）：状态机/CAS/supersede 事务/时效口径。
func TestMemoryCurationIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := memory.NewService(s, memory.Anchors{}, func() string { return "2026-11-05T12:00:00Z" })

	// proposed → accept → edit_history 落库读回。
	cmd := memory.CreateCmd{Namespace: memory.NSAgent, ScopeID: "ag_1", Kind: memory.KindSemantic,
		Title: "候选", Content: "c", ObservedAt: "2026-11-05T10:00:00Z", Status: memory.StatusProposed}
	prop, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Accept(ctx, prop.MemoryID)
	if err != nil || got.Status != memory.StatusActive || len(got.EditHistory) != 1 {
		t.Fatalf("接受错：%v %+v", err, got)
	}
	back, _, _ := s.ItemByID(ctx, prop.MemoryID)
	if back.Status != memory.StatusActive || len(back.EditHistory) != 1 || back.EditHistory[0].Cause != "accept" {
		t.Fatalf("落库读回错：%+v", back.EditHistory)
	}

	// revise：版本递增 + 留痕；CAS 失败。
	title := "修订标题"
	rev, err := svc.Revise(ctx, prop.MemoryID, memory.ReviseCmd{ExpectedVersion: got.Version, Title: &title, Note: "n"})
	if err != nil || rev.Version != got.Version+1 || len(rev.EditHistory) != 2 {
		t.Fatalf("修订错：%v v%d %d", err, rev.Version, len(rev.EditHistory))
	}
	if _, err = svc.Revise(ctx, prop.MemoryID, memory.ReviseCmd{ExpectedVersion: got.Version, Title: &title}); err == nil {
		t.Fatal("CAS 应失败")
	}

	// supersede：事务双动作——旧终结 + 新 supersedes 链；CAS 失败新条目不落。
	repl := cmd
	repl.Title = "替代版"
	repl.Status = ""
	oldIt, newIt, err := svc.Supersede(ctx, prop.MemoryID, rev.Version, "换条", repl)
	if err != nil {
		t.Fatal(err)
	}
	if oldIt.Status != memory.StatusSuperseded || newIt.Supersedes[0] != prop.MemoryID {
		t.Fatalf("替代错：%s %v", oldIt.Status, newIt.Supersedes)
	}
	if _, _, err = svc.Supersede(ctx, prop.MemoryID, oldIt.Version, "", repl); err == nil {
		t.Fatal("终结态应拒")
	}
	// reject 撤回 + 终结动作全拒。
	rej, err := svc.Reject(ctx, newIt.MemoryID, "弃用")
	if err != nil || rej.Status != memory.StatusSuperseded {
		t.Fatalf("撤回错：%v %s", err, rej.Status)
	}
	if _, err = svc.Accept(ctx, newIt.MemoryID); err == nil {
		t.Fatal("终结复活应拒")
	}

	// 时效口径：默认 active 排除过期（julianday 时刻比较）；include_expired 可见。
	exp := memory.CreateCmd{Namespace: memory.NSAgent, ScopeID: "ag_1", Kind: memory.KindSemantic,
		Title: "过期", Content: "c", ObservedAt: "2026-11-05T10:00:00Z",
		ValidFrom: "2026-11-05T00:00:00Z", ValidTo: "2026-11-05T11:59:59Z"}
	if _, err = svc.Create(ctx, exp); err != nil {
		t.Fatal(err)
	}
	alv := exp
	alv.Title, alv.ValidTo = "有效", "2026-11-06T00:00:00Z"
	if _, err = svc.Create(ctx, alv); err != nil {
		t.Fatal(err)
	}
	res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent})
	if err != nil || len(res.Items) != 1 || res.Items[0].Title != "有效" {
		t.Fatalf("默认口径应 1 条有效，得 %d %v", len(res.Items), err)
	}
	// include_expired=true：有效 + 过期共 2 条（superseded 终结条目 status 非
	// active，不入 active 口径——终结态经显式 status=superseded 查询）。
	res, err = svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, IncludeExpired: true})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("include_expired 应 2 条 active（有效+过期），得 %d %v", len(res.Items), err)
	}
}

// 一审 #4：旧数据 +15:00 偏移（julianday NULL）保守包含——未过期不误隐。
func TestMemoryItemsLegacyOffsetCompatIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	// 直插旧格式行（绕过域层 UTC 规范化——模拟 8a03aeb 期间写入）。
	it := memIt(memory.NSAgent, "ag_1", "off1")
	it.ValidFrom = "2026-11-01T00:00:00+15:00"
	it.ValidTo = "2026-11-30T00:00:00+15:00" // 未过期（现 2026-11）
	if _, err := s.CreateItem(ctx, it); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 50, Now: "2026-11-05T12:00:00Z"})
	if err != nil || len(items) != 1 {
		t.Fatalf("旧偏移未过期条目应保守包含，得 %d %v", len(items), err)
	}
}

// 一审 #2（二审测试缺口补强）：UpdateItem 状态基准——**同版本、状态失配**
// 拒且数据/审计不变（移除状态条件本测试必红：纯版本 CAS 会改写 proposed 行）。
func TestMemoryItemsStatusBoundedCASIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	// proposed v1（带留痕基线——不变断言覆盖审计面）。
	base := memIt(memory.NSAgent, "ag_1", "cas1")
	base.Status = memory.StatusProposed
	base.EditHistory = []memory.EditRecord{{Version: 1, Cause: "accept", EditedBy: "owner", OccurredAt: "2026-11-05T12:00:00Z"}}
	it, err := s.CreateItem(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	// 同版本（1 相符）、状态失配（行 proposed，期望 active）——纯版本 CAS
	// 会过并写回，双基准必须拒。
	active := it
	active.Status = memory.StatusActive
	_, ok, err := s.UpdateItem(ctx, it.MemoryID, 1, memory.StatusActive, active)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("同版本状态失配应拒（ok=false）——纯版本 CAS 的并发交错窗口")
	}
	// 数据/审计不变断言（整行回读比对）。
	cur, _, _ := s.ItemByID(ctx, it.MemoryID)
	if cur.Status != memory.StatusProposed || cur.Version != 1 || len(cur.EditHistory) != 1 ||
		cur.EditHistory[0].Cause != "accept" || cur.Title != it.Title || cur.UpdatedAt != it.UpdatedAt {
		t.Fatalf("失配拒绝后行被改动：%+v", cur)
	}
	// 版本失配（状态相符）同拒且不变。
	bump := it
	bump.Version = 2
	if _, ok, _ := s.UpdateItem(ctx, it.MemoryID, 99, memory.StatusProposed, bump); ok {
		t.Fatal("版本失配应拒")
	}
	cur2, _, _ := s.ItemByID(ctx, it.MemoryID)
	if cur2.Version != 1 || cur2.Status != memory.StatusProposed {
		t.Fatalf("版本失配拒绝后行被改动：%+v", cur2)
	}
	// 基准相符（版本 + 状态都对）正常通过——语义完备性。
	acc := it
	acc.Status = memory.StatusActive
	acc.EditHistory = append(acc.EditHistory, memory.EditRecord{Version: 1, Cause: "accept", EditedBy: "owner", OccurredAt: "2026-11-05T12:01:00Z"})
	if _, ok, _ := s.UpdateItem(ctx, it.MemoryID, 1, memory.StatusProposed, acc); !ok {
		t.Fatal("基准相符应通过")
	}
}

// 二审 #1：小数秒时效边界——截止 .900Z 在 .500Z 未过期、.950Z 过期
// （sqlite julianday 小数比较；整秒截断会提前隐藏）。
func TestMemoryItemsFractionalExpiryIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	live := memIt(memory.NSAgent, "ag_1", "frac1")
	live.ValidTo = "2026-11-06T12:00:00.900Z"
	if _, err := s.CreateItem(ctx, live); err != nil {
		t.Fatal(err)
	}
	gone := memIt(memory.NSAgent, "ag_1", "frac0")
	gone.ValidTo = "2026-11-06T12:00:00.400Z"
	if _, err := s.CreateItem(ctx, gone); err != nil {
		t.Fatal(err)
	}
	// now=.500Z：frac1 未过期可见；frac0 已过期隐藏。
	items, _, err := s.ListItems(ctx, memory.ListFilter{Namespace: memory.NSAgent,
		Status: memory.StatusActive, Limit: 50, Now: "2026-11-06T12:00:00.500Z"})
	if err != nil || len(items) != 1 || items[0].Title != "frac1" {
		t.Fatalf("小数秒边界过滤错：%d %v", len(items), err)
	}
}

// 召回 IT（0.7.2 / ADR-0039）：trigram 中文/英文、<3 字 LIKE 回退、过滤先行
// （异域/异状态命中不出现在结果与 total）、修订后 FTS 同步（触发器）。
func TestMemoryRecallIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := memory.NewService(s, memory.Anchors{}, func() string { return "2026-11-05T12:00:00Z" })
	mk := func(ns memory.Namespace, scope, title, content string) memory.Item {
		cmd := memory.CreateCmd{Namespace: ns, ScopeID: scope, Kind: memory.KindSemantic,
			Title: title, Content: content, ObservedAt: "2026-11-05T10:00:00Z"}
		it, err := svc.Create(ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	mk(memory.NSAgent, "ag_1", "部署流程", "先建单后执行，验收前自查。")
	mk(memory.NSAgent, "ag_1", "Weekly Report", "Export the report every Monday.")
	mk(memory.NSWorkspace, memory.WorkspaceScope, "异域同词", "部署流程也在本条——不应出现在 agent 召回")

	t.Run("trigram 中文 ≥3 字", func(t *testing.T) {
		res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "部署流程"})
		if err != nil || len(res.Items) != 1 || res.TotalHits != 1 || res.Items[0].Title != "部署流程" {
			t.Fatalf("中文召回错：%d/%d %v", len(res.Items), res.TotalHits, err)
		}
		if res.NextCursor != "" {
			t.Fatal("召回模式不应有游标")
		}
	})
	t.Run("英文子串 + 大小写", func(t *testing.T) {
		res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "REPORT"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("英文召回错：%d %v", res.TotalHits, err)
		}
	})
	t.Run("<3 字 LIKE 回退（单字符）", func(t *testing.T) {
		// "p" 命中 Report/Export（COLLATE NOCASE）——LIKE 回退路径。
		res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "p"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("短词回退错：%d %v", res.TotalHits, err)
		}
	})
	t.Run("过滤先行——异域命中不计结果与 total", func(t *testing.T) {
		res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "部署流程"})
		if err != nil || res.TotalHits != 1 {
			t.Fatalf("异域泄漏：%d %v", res.TotalHits, err)
		}
		for _, it := range res.Items {
			if it.Namespace != memory.NSAgent {
				t.Fatalf("跨域结果：%+v", it)
			}
		}
	})
	t.Run("修订后 FTS 同步（触发器）——旧词不中新词中", func(t *testing.T) {
		it := mk(memory.NSAgent, "ag_1", "旧标题甲", "旧内容乙")
		title := "新标题丙"
		if _, err := svc.Revise(ctx, it.MemoryID, memory.ReviseCmd{ExpectedVersion: 1, Title: &title}); err != nil {
			t.Fatal(err)
		}
		if res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "旧标题甲"}); err != nil || res.TotalHits != 0 {
			t.Fatalf("旧词应不中：%d %v", res.TotalHits, err)
		}
		if res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "新标题丙"}); err != nil || res.TotalHits != 1 {
			t.Fatalf("新词应中：%d %v", res.TotalHits, err)
		}
	})
	t.Run("superseded 不入召回（status 过滤同 WHERE）", func(t *testing.T) {
		it := mk(memory.NSAgent, "ag_1", "终结条目", "内容独特词")
		if _, err := svc.Reject(ctx, it.MemoryID, ""); err != nil {
			t.Fatal(err)
		}
		if res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "内容独特词"}); err != nil || res.TotalHits != 0 {
			t.Fatalf("终结条目不应入召回：%d %v", res.TotalHits, err)
		}
		// 显式 superseded 口径可查（历史解释）。
		if res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Status: memory.StatusSuperseded, Query: "内容独特词"}); err != nil || res.TotalHits != 1 {
			t.Fatalf("历史口径应中：%d %v", res.TotalHits, err)
		}
	})
	t.Run("bm25 相关性排序（更相关在前）", func(t *testing.T) {
		mk(memory.NSAgent, "ag_1", "搜索词一次", "无关内容。")
		mk(memory.NSAgent, "ag_1", "标题无关", "搜索词出现两次：搜索词重复加权。")
		res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "搜索词"})
		if err != nil || len(res.Items) != 2 {
			t.Fatalf("相关召回错：%d %v", len(res.Items), err)
		}
		// 双次命中（标题+正文）应更相关——bm25 升序在前。
		if res.Items[0].Title != "标题无关" {
			t.Fatalf("bm25 序错：首条 %s", res.Items[0].Title)
		}
	})
}

// 一审 #2：LIKE 字面 %/_/反斜杠——五条仅一条含目标字符（ESCAPE 转义后
// 精确命中；未转义会通配命中全部）。
func TestMemoryRecallLikeLiteralIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := memory.NewService(s, memory.Anchors{}, func() string { return "2026-11-05T12:00:00Z" })
	mk := func(title string) {
		cmd := memory.CreateCmd{Namespace: memory.NSAgent, ScopeID: "ag_1", Kind: memory.KindSemantic,
			Title: title, Content: "普通内容", ObservedAt: "2026-11-05T10:00:00Z"}
		if _, err := svc.Create(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	mk("折扣 100%OFF")
	mk("下划线_a 样式")
	mk("反斜杠\\x 路径")
	mk("普通标题一")
	mk("普通标题二")
	for _, tc := range []struct{ q string }{
		{"100%"}, {"%"}, {"_a"}, {"_"}, {"\\"},
	} {
		res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: tc.q})
		if err != nil {
			t.Fatalf("q=%q：%v", tc.q, err)
		}
		if res.TotalHits < 1 || res.TotalHits > 2 {
			t.Fatalf("q=%q 字面命中异常：%d（未转义通配会命中全部 5）", tc.q, res.TotalHits)
		}
	}
	// 纯 % 只命中标题含 % 的一条（LIKE 未转义时 5）。
	res, _ := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "100%"})
	if res.TotalHits != 1 || res.Items[0].Title != "折扣 100%OFF" {
		t.Fatalf("字面 %% 命中错：%d %+v", res.TotalHits, res.Items)
	}
}

// 一审 #3：total 与分页解耦——七条、limit=2，首页与每续页 total 恒 7。
func TestMemoryRecallTotalConstantPagingIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := memory.NewService(s, memory.Anchors{}, func() string { return "2026-11-05T12:00:00Z" })
	for i := 0; i < 7; i++ {
		cmd := memory.CreateCmd{Namespace: memory.NSAgent, ScopeID: "ag_1", Kind: memory.KindSemantic,
			Title: "条目" + string(rune('A'+i)), Content: "c", ObservedAt: "2026-11-05T10:00:00Z"}
		if _, err := svc.Create(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Limit: 2})
	if err != nil || res.TotalHits != 7 || len(res.Items) != 2 {
		t.Fatalf("首页 total 应 7，得 %d（items %d）%v", res.TotalHits, len(res.Items), err)
	}
	pages := 1
	for res.NextCursor != "" && pages < 5 {
		res, err = svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Limit: 2, Before: res.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if res.TotalHits != 7 {
			t.Fatalf("第 %d 页 total 应恒 7，得 %d", pages, res.TotalHits)
		}
	}
	if pages != 4 { // 2+2+2+1
		t.Fatalf("分页页数错：%d", pages)
	}
}

// 一审 #4：强命中先创建排前——bm25 真实评分（脱离 MATCH 的子查询恒零会
// 退化为纯 rowid 序，新弱命中挤出旧强命中）。
func TestMemoryRecallBM25RealScoringIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := memory.NewService(s, memory.Anchors{}, func() string { return "2026-11-05T12:00:00Z" })
	mk := func(title, content string) {
		cmd := memory.CreateCmd{Namespace: memory.NSAgent, ScopeID: "ag_1", Kind: memory.KindSemantic,
			Title: title, Content: content, ObservedAt: "2026-11-05T10:00:00Z"}
		if _, err := svc.Create(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	// 先建：标题强命中（词在标题 + 正文重复）；后建：仅正文一次弱命中。
	mk("缓存策略详解", "缓存策略应考虑失效与预热，缓存策略正文重复加权。")
	mk("无关标题", "提一句缓存策略。")
	res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "缓存策略", Limit: 1})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("召回错：%d %v", len(res.Items), err)
	}
	if res.Items[0].Title != "缓存策略详解" {
		t.Fatalf("bm25 评分未生效——旧强命中被挤出：%s", res.Items[0].Title)
	}
}

// 一审 #1：MATCH 驱动 JOIN——受控 300 条命中在毫秒级（逐候选子查询为秒级
// 全表扫描；粗断言防性能回归）。
func TestMemoryRecallPerfIT(t *testing.T) {
	s := openMemItStore(t)
	defer s.Close()
	ctx := context.Background()
	svc := memory.NewService(s, memory.Anchors{}, func() string { return "2026-11-05T12:00:00Z" })
	for i := 0; i < 300; i++ {
		cmd := memory.CreateCmd{Namespace: memory.NSAgent, ScopeID: "ag_1", Kind: memory.KindSemantic,
			Title: "性能条目", Content: "基准词命中内容", ObservedAt: "2026-11-05T10:00:00Z"}
		if _, err := svc.Create(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	start := timeNow()
	res, err := svc.List(ctx, memory.ListFilter{Namespace: memory.NSAgent, Query: "基准词命中", Limit: 50})
	elapsed := timeSince(start)
	if err != nil || res.TotalHits != 300 || len(res.Items) != 50 {
		t.Fatalf("受控召回错：%d/%d %v", len(res.Items), res.TotalHits, err)
	}
	if elapsed > 2*timeSecond {
		t.Fatalf("召回耗时 %v（MATCH 驱动应在毫秒级——逐候选子查询回归）", elapsed)
	}
}

func TestMemoryItemsIdempotentReopenIT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateItem(ctx, memIt(memory.NSWorkspace, memory.WorkspaceScope, "w1")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// 模拟旧库（v31 面）：删表 + 撤销 v32 登记，重开走迁移重放。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.ExecContext(ctx, "DROP TABLE memory_items"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.ExecContext(ctx, "DELETE FROM migrations WHERE version = 32"); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	// v32 重放：表恢复 + 数据可再写（旧行已随 DROP 丢失——幂等重放语义）。
	if _, err := s3.CreateItem(ctx, memIt(memory.NSWorkspace, memory.WorkspaceScope, "w2")); err != nil {
		t.Fatalf("重放后不可写：%v", err)
	}
	it, ok, err := s3.ItemByID(ctx, "mem_it_w2")
	if err != nil || !ok || it.Title != "w2" {
		t.Fatalf("重放后读回错：%v %v", err, ok)
	}
}
