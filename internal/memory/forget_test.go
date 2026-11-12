// forget_test.go —— 0.7.8 / ADR-0045：记忆 Forget（隐私删除）的域级测试
// （TDD 红段先行——墓碑化、正文清除、留痕、幂等、查面口径）。
package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func forgetFixture(t *testing.T) (*Service, Item) {
	t.Helper()
	svc := NewService(NewMemStore(), allAnchors(true, nil), fixedNow)
	it, err := svc.Create(ctxBg(), CreateCmd{
		Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
		Title: "秘制配方", Content: "正文包含隐私内容", SourceRefs: []string{"ev_1"},
		ObservedAt: "2026-11-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, it
}

func ctxBg() context.Context { return context.Background() }

// TestForgetTombstoneSemantics 墓碑化全语义（ADR-0045 §1）。
func TestForgetTombstoneSemantics(t *testing.T) {
	svc, it := forgetFixture(t)

	gone, err := svc.Forget(ctxBg(), it.MemoryID, "隐私清除")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	// 墓碑：status=deleted、正文清空、留痕 cause=delete、版本递增。
	if gone.Status != StatusDeleted {
		t.Fatalf("status: %s", gone.Status)
	}
	if gone.Title != "" || gone.Content != "" {
		t.Fatalf("正文须清空（不含正文的墓碑）: %q %q", gone.Title, gone.Content)
	}
	if gone.Version != it.Version+1 {
		t.Fatalf("version: %d → %d", it.Version, gone.Version)
	}
	last := gone.EditHistory[len(gone.EditHistory)-1]
	if last.Cause != "delete" || last.Note != "隐私清除" {
		t.Fatalf("留痕: %+v", last)
	}
	// 保留面：归属/类型/来源引用（派生追踪依据）/敏感度/时间戳。
	if gone.Namespace != NSAgent || gone.ScopeID != "ag_1" || gone.Kind != KindSemantic {
		t.Fatalf("归属面须保留: %+v", gone)
	}
	if len(gone.SourceRefs) != 1 || gone.SourceRefs[0] != "ev_1" {
		t.Fatalf("source_refs 须保留（派生追踪）: %v", gone.SourceRefs)
	}
	if gone.Sensitivity != it.Sensitivity {
		t.Fatalf("sensitivity 保留: %+v", gone)
	}
	// Get 可见墓碑；List 默认口径（status=active）不可见。
	stored, err := svc.Get(ctxBg(), it.MemoryID)
	if err != nil || stored.Status != StatusDeleted || stored.Content != "" {
		t.Fatalf("get: %+v %v", stored, err)
	}
	lr, err := svc.List(ctxBg(), ListFilter{Namespace: NSAgent, ScopeID: "ag_1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.Items) != 0 {
		t.Fatalf("默认口径不可见: %+v", lr.Items)
	}
	// 幂等：再删 no-op（版本不再递增）。
	again, err := svc.Forget(ctxBg(), it.MemoryID, "重复删除")
	if err != nil {
		t.Fatalf("幂等: %v", err)
	}
	if again.Version != gone.Version {
		t.Fatalf("幂等版本不递增: %d vs %d", again.Version, gone.Version)
	}
	if l := again.EditHistory[len(again.EditHistory)-1]; l.Cause != "delete" || l.Note != "隐私清除" {
		t.Fatalf("幂等不追留痕: %+v", l)
	}
}

// TestForgetAnyLiveStatus 三态皆可删（superseded 条目同样承载隐私内容）。
func TestForgetAnyLiveStatus(t *testing.T) {
	cases := []struct {
		name   string
		status Status
	}{
		{"active", StatusActive},
		{"proposed", StatusProposed},
		{"superseded", StatusSuperseded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, it := forgetFixture(t)
			if tc.status != StatusActive {
				if _, ok, err := svc.store.UpdateItem(ctxBg(), it.MemoryID, it.Version, it.Status,
					mutateStatus(it, tc.status)); err != nil || !ok {
					t.Fatalf("预置 %s: %v %v", tc.status, ok, err)
				}
			}
			gone, err := svc.Forget(ctxBg(), it.MemoryID, "")
			if err != nil {
				t.Fatalf("forget %s: %v", tc.status, err)
			}
			if gone.Status != StatusDeleted {
				t.Fatalf("status: %s", gone.Status)
			}
		})
	}
}

// TestForgetUnknown 不存在 404（不静默）。
func TestForgetUnknown(t *testing.T) {
	svc, _ := forgetFixture(t)
	_, err := svc.Forget(ctxBg(), "mem_ghost", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err: %v", err)
	}
}

// TestForgetNoteLimit note ≤500 字（留痕口径同策展）。
func TestForgetNoteLimit(t *testing.T) {
	svc, it := forgetFixture(t)
	if _, err := svc.Forget(ctxBg(), it.MemoryID, strings.Repeat("长", 501)); err == nil {
		t.Fatal("超长 note 须拒")
	}
}

func mutateStatus(it Item, st Status) Item {
	it.Status = st
	return it
}

// TestCurationBlocksDeleted 0.7.8 审校 #1：墓碑是策展不可达终态——
// Accept/Reject/Revise/Supersede 均不得消除墓碑（防复活依据）。
func TestCurationBlocksDeleted(t *testing.T) {
	mkDeleted := func(t *testing.T) (*Service, Item) {
		svc, it := forgetFixture(t)
		gone, err := svc.Forget(ctxBg(), it.MemoryID, "")
		if err != nil {
			t.Fatal(err)
		}
		return svc, gone
	}
	t.Run("Accept", func(t *testing.T) {
		svc, gone := mkDeleted(t)
		if _, err := svc.Accept(ctxBg(), gone.MemoryID); !errors.Is(err, ErrTerminal) {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("Reject", func(t *testing.T) {
		svc, gone := mkDeleted(t)
		if _, err := svc.Reject(ctxBg(), gone.MemoryID, ""); !errors.Is(err, ErrTerminal) {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("Revise", func(t *testing.T) {
		svc, gone := mkDeleted(t)
		v := gone.Version
		if _, err := svc.Revise(ctxBg(), gone.MemoryID, ReviseCmd{ExpectedVersion: v, Content: strp("重写正文")}); !errors.Is(err, ErrTerminal) {
			t.Fatalf("err: %v", err)
		}
		stored, _ := svc.Get(ctxBg(), gone.MemoryID)
		if stored.Content != "" {
			t.Fatalf("墓碑正文不得被重写")
		}
	})
	t.Run("Supersede", func(t *testing.T) {
		svc, gone := mkDeleted(t)
		cmd := CreateCmd{Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
			Title: "t", Content: "c", ObservedAt: "2026-11-01T00:00:00Z"}
		if _, _, err := svc.Supersede(ctxBg(), gone.MemoryID, gone.Version, "", cmd); !errors.Is(err, ErrTerminal) {
			t.Fatalf("err: %v", err)
		}
	})
}

func strp(s string) *string { return &s }

// TestTombstoneHitsNormalized 0.7.8 审校 #2：键归一到稳定记忆身份——
// 不同 bundle_id 的标记（尾段同源 ID）跨导出命中。
func TestTombstoneHitsNormalized(t *testing.T) {
	svc, it := forgetFixture(t)
	if _, err := svc.Forget(ctxBg(), it.MemoryID, ""); err != nil {
		t.Fatal(err)
	}
	// 墓碑 SourceRefs 预置一个来源标记（模拟激活产物再删：bundle:B1:mem_X）。
	gone, _ := svc.Get(ctxBg(), it.MemoryID)
	next := gone
	next.SourceRefs = append(next.SourceRefs, "bundle:bdl_B1:mem_origin")
	if _, ok, err := svc.store.UpdateItem(ctxBg(), gone.MemoryID, gone.Version, gone.Status, next); err != nil || !ok {
		t.Fatalf("预置: %v %v", ok, err)
	}
	// 另一 bundle_id 的同源标记（尾段 mem_origin）须命中；裸源 ID 亦命中。
	hits, err := svc.TombstoneHits(ctxBg(), []string{"bundle:bdl_B2:mem_origin"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("跨包同源命中: %v %v", hits, err)
	}
	hits, err = svc.TombstoneHits(ctxBg(), []string{"mem_origin"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("裸稳定 ID 命中: %v %v", hits, err)
	}
	if hits, _ := svc.TombstoneHits(ctxBg(), []string{"mem_unrelated"}); len(hits) != 0 {
		t.Fatalf("无关不命中: %v", hits)
	}
}

// TestCreateGuarded 0.7.8 审校 #3：守卫创建——命中墓碑 false 不落库，
// 未命中 true 落库（核对与创建同 forgetMu 边界——竞态窗口结构性消除）。
func TestCreateGuarded(t *testing.T) {
	svc, it := forgetFixture(t)
	if _, err := svc.Forget(ctxBg(), it.MemoryID, ""); err != nil {
		t.Fatal(err)
	}
	cmd := CreateCmd{
		Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
		Title: "t", Content: "c", ObservedAt: "2026-11-01T00:00:00Z",
	}
	created, err := svc.CreateGuarded(ctxBg(), []string{it.MemoryID}, cmd)
	if err != nil || created {
		t.Fatalf("墓碑命中须拒: %v %v", created, err)
	}
	lr, _ := svc.List(ctxBg(), ListFilter{Namespace: NSAgent, ScopeID: "ag_1", Limit: 10})
	if len(lr.Items) != 0 {
		t.Fatalf("不得落库: %+v", lr.Items)
	}
	created, err = svc.CreateGuarded(ctxBg(), []string{"mem_other"}, cmd)
	if err != nil || !created {
		t.Fatalf("未命中须创建: %v %v", created, err)
	}
}

// TestForgetCreateGuardedInterleaving 复审 #1：Forget 与 CreateGuarded 同键
// 真实并发交错（-race）——不变式：Forget 提交后创建必拒；创建成功必然先于
// Forget 提交（同 forgetMu 边界——无"查后插入仍写回"窗口）。
func TestForgetCreateGuardedInterleaving(t *testing.T) {
	svc, it := forgetFixture(t)
	cmd := CreateCmd{
		Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
		Title: "t", Content: "c", ObservedAt: "2026-11-01T00:00:00Z",
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = svc.Forget(ctxBg(), it.MemoryID, "") }()
		go func() { defer wg.Done(); _, _ = svc.CreateGuarded(ctxBg(), []string{it.MemoryID}, cmd) }()
	}
	wg.Wait()
	gone, err := svc.Get(ctxBg(), it.MemoryID)
	if err != nil || gone.Status != StatusDeleted {
		t.Fatalf("末态须墓碑: %+v %v", gone, err)
	}
	created, err := svc.CreateGuarded(ctxBg(), []string{it.MemoryID}, cmd)
	if err != nil || created {
		t.Fatalf("墓碑后创建必拒: %v %v", created, err)
	}
}

// TestTombstoneHitsIgnoresEvidenceRefs 复审 #2：普通证据引用不是记忆身份
// ——两条记忆共享 ev_1 不等于同一记忆（误入键集会把同源不同事实静默跳过）。
func TestTombstoneHitsIgnoresEvidenceRefs(t *testing.T) {
	svc, it := forgetFixture(t) // it.SourceRefs = ["ev_1"]
	if _, err := svc.Forget(ctxBg(), it.MemoryID, ""); err != nil {
		t.Fatal(err)
	}
	if hits, _ := svc.TombstoneHits(ctxBg(), []string{"ev_1"}); len(hits) != 0 {
		t.Fatalf("证据引用不得命中: %v", hits)
	}
	// 键集混入证据引用与无关记忆 ID：仅记忆身份参与。
	hits, _ := svc.TombstoneHits(ctxBg(), []string{"ev_1", "mem_other"})
	if len(hits) != 0 {
		t.Fatalf("无关身份不命中: %v", hits)
	}
	// 守卫创建同理：仅证据引用为键 → 照常创建。
	cmd := CreateCmd{Namespace: NSAgent, ScopeID: "ag_1", Kind: KindSemantic,
		Title: "t2", Content: "c2", ObservedAt: "2026-11-01T00:00:00Z"}
	created, err := svc.CreateGuarded(ctxBg(), []string{"ev_1"}, cmd)
	if err != nil || !created {
		t.Fatalf("证据引用不拦创建: %v %v", created, err)
	}
}
