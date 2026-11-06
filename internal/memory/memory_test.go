// 分域记忆域测试（ADR-0037 §7）：校验矩阵 / 锚矩阵 / 默认填充 / 列表隔离
// 与游标（memstore 驱动——存储无关的域语义；sqlite 集成随 IT）。
package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fixedNow() string { return "2026-11-04T12:00:00Z" }

func okCmd() CreateCmd {
	return CreateCmd{
		Namespace:  NSAgent,
		ScopeID:    "ag_1",
		Kind:       KindSemantic,
		Title:      "偏好：简洁回复",
		Content:    "用户偏好简洁的中文回复。",
		ObservedAt: "2026-11-04T10:00:00Z",
	}
}

func allAnchors(ok bool, err error) Anchors {
	f := func(context.Context, string) (bool, error) { return ok, err }
	return Anchors{AgentExists: f, RoomExists: f, MissionExists: f, SlotInstanceExists: f}
}

func TestValidateMatrix(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*CreateCmd)
		want error
	}{
		{"namespace 未知", func(c *CreateCmd) { c.Namespace = "global" }, ErrInvalid},
		{"kind 未知", func(c *CreateCmd) { c.Kind = "other" }, ErrInvalid},
		{"sensitivity 未知", func(c *CreateCmd) { c.Sensitivity = "secret" }, ErrInvalid},
		{"scope 空", func(c *CreateCmd) { c.ScopeID = "  " }, ErrInvalid},
		{"scope 超长", func(c *CreateCmd) { c.ScopeID = strings.Repeat("a", 129) }, ErrInvalid},
		{"workspace 锚非 default", func(c *CreateCmd) { c.Namespace, c.ScopeID = NSWorkspace, "ws_2" }, ErrInvalid},
		{"title 空", func(c *CreateCmd) { c.Title = " " }, ErrInvalid},
		{"title 超长", func(c *CreateCmd) { c.Title = strings.Repeat("标", 201) }, ErrInvalid},
		{"content 空", func(c *CreateCmd) { c.Content = "" }, ErrInvalid},
		{"content 超长", func(c *CreateCmd) { c.Content = strings.Repeat("字", 20001) }, ErrInvalid},
		{"source_refs 17 条", func(c *CreateCmd) {
			c.SourceRefs = make([]string, 17)
		}, ErrInvalid},
		{"source_ref 超长", func(c *CreateCmd) {
			c.SourceRefs = []string{strings.Repeat("r", 501)}
		}, ErrInvalid},
		{"observed_at 非 RFC3339", func(c *CreateCmd) { c.ObservedAt = "2026-11-04" }, ErrInvalid},
		{"valid_from 非 RFC3339", func(c *CreateCmd) { c.ValidFrom = "bad" }, ErrInvalid},
		{"valid_to 早于 valid_from", func(c *CreateCmd) {
			c.ValidFrom, c.ValidTo = "2026-11-04T10:00:00Z", "2026-11-04T09:00:00Z"
		}, ErrInvalid},
		{"valid_to 等于 valid_from", func(c *CreateCmd) {
			c.ValidFrom, c.ValidTo = "2026-11-04T10:00:00Z", "2026-11-04T10:00:00Z"
		}, ErrInvalid},
		{"confidence 负", func(c *CreateCmd) { v := -0.1; c.Confidence = &v }, ErrInvalid},
		{"confidence 超 1", func(c *CreateCmd) { v := 1.1; c.Confidence = &v }, ErrInvalid},
		{"合法全集", func(c *CreateCmd) {
			c.SourceRefs = []string{"room:evt_1", "doc:rev_2"}
			c.ValidFrom, c.ValidTo = "2026-11-04T10:00:00Z", "2026-12-01T00:00:00Z"
			v := 0.8
			c.Confidence = &v
			c.Sensitivity = SensSensitive
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

func TestCreateAnchorMatrix(t *testing.T) {
	ctx := context.Background()
	scopes := []Namespace{NSAgent, NSRoom, NSMission, NSSlot}
	for _, ns := range scopes {
		t.Run(string(ns)+" 锚不存在", func(t *testing.T) {
			svc := NewService(NewMemStore(), allAnchors(false, nil), fixedNow)
			cmd := okCmd()
			cmd.Namespace, cmd.ScopeID = ns, "missing"
			if _, err := svc.Create(ctx, cmd); !errors.Is(err, ErrScopeNotFound) {
				t.Fatalf("应 ErrScopeNotFound，得 %v", err)
			}
		})
	}
	t.Run("锚存在", func(t *testing.T) {
		svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)
		if _, err := svc.Create(ctx, okCmd()); err != nil {
			t.Fatalf("应通过，得 %v", err)
		}
	})
	t.Run("workspace 恒过（nil 闭包不涉）", func(t *testing.T) {
		svc := NewService(NewMemStore(), Anchors{}, fixedNow)
		cmd := okCmd()
		cmd.Namespace, cmd.ScopeID = NSWorkspace, WorkspaceScope
		if _, err := svc.Create(ctx, cmd); err != nil {
			t.Fatalf("应通过，得 %v", err)
		}
	})
	t.Run("锚查询错误透传", func(t *testing.T) {
		boom := errors.New("boom")
		svc := NewService(NewMemStore(), allAnchors(false, boom), fixedNow)
		if _, err := svc.Create(ctx, okCmd()); !errors.Is(err, boom) {
			t.Fatalf("应透传 boom，得 %v", err)
		}
	})
	t.Run("nil 闭包跳过（测试装配语义）", func(t *testing.T) {
		svc := NewService(NewMemStore(), Anchors{}, fixedNow)
		if _, err := svc.Create(ctx, okCmd()); err != nil {
			t.Fatalf("应通过，得 %v", err)
		}
	})
}

func TestCreateDefaults(t *testing.T) {
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)
	it, err := svc.Create(context.Background(), okCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(it.MemoryID, "mem_") {
		t.Fatalf("memory_id 应 mem_ 前缀，得 %q", it.MemoryID)
	}
	if it.Version != 1 || it.Status != StatusActive || it.Visibility != "namespace" {
		t.Fatalf("0.7.0 固定默认错：version=%d status=%s visibility=%s", it.Version, it.Status, it.Visibility)
	}
	if it.Confidence != 1.0 {
		t.Fatalf("人工写入默认满置信，得 %v", it.Confidence)
	}
	if it.Sensitivity != SensNormal || it.Supersedes == nil || len(it.Supersedes) != 0 {
		t.Fatalf("默认 sensitivity/supersedes 错：%v %v", it.Sensitivity, it.Supersedes)
	}
	if it.CreatedAt != fixedNow() || it.UpdatedAt != fixedNow() {
		t.Fatalf("时钟单源未生效：%s", it.CreatedAt)
	}
	if it.ScopeID != "ag_1" || it.Title != "偏好：简洁回复" {
		t.Fatalf("trim 未生效：%q %q", it.ScopeID, it.Title)
	}
}

// 一审 #1：省略 source_refs → 落库为空数组（非 nil——JSON [] 不出 null）。
func TestCreateSourceRefsNormalizedNonNil(t *testing.T) {
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)
	it, err := svc.Create(context.Background(), okCmd()) // okCmd 不带 source_refs
	if err != nil {
		t.Fatal(err)
	}
	if it.SourceRefs == nil || len(it.SourceRefs) != 0 {
		t.Fatalf("SourceRefs 应为非 nil 空数组，得 %#v", it.SourceRefs)
	}
	// 显式空数组同样规范化。
	cmd := okCmd()
	cmd.SourceRefs = []string{}
	it2, err := svc.Create(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if it2.SourceRefs == nil {
		t.Fatal("显式空数组 SourceRefs 不应为 nil")
	}
}

// 一审 #4：memstore 游标只在本域定位——外域同 ID = 空页。
func TestMemStoreCursorScopedToNamespace(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	ag := Item{MemoryID: "mem_ag_1", Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
		Title: "a", Content: "c", Status: StatusActive, Visibility: "namespace",
		Sensitivity: SensNormal, Supersedes: []string{}, CreatedAt: fixedNow(), UpdatedAt: fixedNow()}
	ws := Item{MemoryID: "mem_ws_1", Namespace: NSWorkspace, ScopeID: WorkspaceScope, Kind: KindSemantic,
		Title: "w", Content: "c", Status: StatusActive, Visibility: "namespace",
		Sensitivity: SensNormal, Supersedes: []string{}, CreatedAt: fixedNow(), UpdatedAt: fixedNow()}
	if _, err := store.CreateItem(ctx, ag); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateItem(ctx, ws); err != nil {
		t.Fatal(err)
	}
	// workspace 查询以 agent 域条目 ID 作 before：外域 ID 不生效（空页——
	// 与未知 ID 同语义，越域存在性不泄漏）。
	items, _, err := store.ListItems(ctx, ListFilter{Namespace: NSWorkspace,
		Status: StatusActive, Limit: 50, Before: "mem_ag_1"})
	if err != nil || len(items) != 0 {
		t.Fatalf("外域游标应空页，得 %d %v", len(items), err)
	}
	// 本域游标正常。
	items, _, err = store.ListItems(ctx, ListFilter{Namespace: NSWorkspace,
		Status: StatusActive, Limit: 50, Before: "mem_ws_1"})
	if err != nil || len(items) != 0 {
		t.Fatalf("本域末条游标应空页（其后无更早条目），得 %d %v", len(items), err)
	}
	items, _, err = store.ListItems(ctx, ListFilter{Namespace: NSAgent,
		Status: StatusActive, Limit: 50})
	if err != nil || len(items) != 1 || items[0].MemoryID != "mem_ag_1" {
		t.Fatalf("agent 列表应 1 条，得 %d %v", len(items), err)
	}
}

func TestListIsolationAndCursor(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	svc := NewService(store, allAnchors(true, nil), fixedNow)
	// agent 3 条（含一条 proposed 直插）、mission 2 条。
	for i := 0; i < 3; i++ {
		cmd := okCmd()
		cmd.Title = strings.Repeat("A", i+1)
		if _, err := svc.Create(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	proposed := Item{MemoryID: "mem_proposed", Namespace: NSAgent, ScopeID: "ag_1",
		Kind: KindSemantic, Title: "候选", Content: "c", Status: StatusProposed,
		Visibility: "namespace", Sensitivity: SensNormal, Supersedes: []string{},
		CreatedAt: fixedNow(), UpdatedAt: fixedNow()}
	if _, err := store.CreateItem(ctx, proposed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cmd := okCmd()
		cmd.Namespace, cmd.ScopeID, cmd.Title = NSMission, "m_1", "M"
		if _, err := svc.Create(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("namespace 必填", func(t *testing.T) {
		if _, err := svc.List(ctx, ListFilter{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("应 ErrInvalid，得 %v", err)
		}
	})
	t.Run("默认 status=active——proposed 不出现", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 3 {
			t.Fatalf("应 3 条 active，得 %d", len(res.Items))
		}
	})
	t.Run("proposed 显式请求可见", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Status: StatusProposed})
		if err != nil || len(res.Items) != 1 || res.Items[0].MemoryID != "mem_proposed" {
			t.Fatalf("proposed 口径错：%v %d", err, len(res.Items))
		}
	})
	t.Run("域隔离——mission 列表不见 agent 条目", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSMission})
		if err != nil || len(res.Items) != 2 {
			t.Fatalf("应 2 条，得 %d %v", len(res.Items), err)
		}
		for _, it := range res.Items {
			if it.Namespace != NSMission {
				t.Fatalf("跨域泄漏：%+v", it)
			}
		}
	})
	t.Run("limit=2 + next_cursor 续页", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2})
		if err != nil || len(res.Items) != 2 || res.NextCursor == "" {
			t.Fatalf("首页错：%d %q %v", len(res.Items), res.NextCursor, err)
		}
		if res.Items[0].Title != "AAA" { // 创建序降序——最新在前
			t.Fatalf("降序错：首条 %q", res.Items[0].Title)
		}
		res, err = svc.List(ctx, ListFilter{Namespace: NSAgent, Limit: 2, Before: res.NextCursor})
		if err != nil || len(res.Items) != 1 || res.NextCursor != "" {
			t.Fatalf("次页错：%d %q %v", len(res.Items), res.NextCursor, err)
		}
		if res.Items[0].Title != "A" {
			t.Fatalf("续页错：%q", res.Items[0].Title)
		}
	})
	t.Run("scope/kind 过滤", func(t *testing.T) {
		res, err := svc.List(ctx, ListFilter{Namespace: NSMission, ScopeID: "m_404"})
		if err != nil || len(res.Items) != 0 {
			t.Fatalf("scope 过滤错：%d %v", len(res.Items), err)
		}
		res, err = svc.List(ctx, ListFilter{Namespace: NSAgent, Kind: KindEpisodic})
		if err != nil || len(res.Items) != 0 {
			t.Fatalf("kind 过滤错：%d %v", len(res.Items), err)
		}
	})
	t.Run("limit 越界拒绝", func(t *testing.T) {
		if _, err := svc.List(ctx, ListFilter{Namespace: NSAgent, Limit: 101}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("应 ErrInvalid，得 %v", err)
		}
	})
}

func TestGetNotFound(t *testing.T) {
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)
	if _, err := svc.Get(context.Background(), "mem_404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应 ErrNotFound，得 %v", err)
	}
}
