package slot

// SlotInstance 职责实例（RFC-0018 §3.1/§5.2 / ADR-0020，0.5.3）。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func newInstTestService(t *testing.T) *Service {
	t.Helper()
	var n atomic.Int64 // 并发实例化测试共用服务——生成器取号原子化
	return New(NewMemStore(),
		WithNow(fixedNow),
		WithNewInstanceID(func() string { return "si_test_" + strconv.FormatInt(n.Add(1), 10) }),
		WithNewNamespace(func() string { return "slotns_test_" + strconv.FormatInt(n.Add(1), 10) }),
	)
}

func mustSpec(t *testing.T, svc *Service, id string) {
	t.Helper()
	if _, err := svc.Create(context.Background(), id, validContent()); err != nil {
		t.Fatalf("建谱系 %s: %v", id, err)
	}
}

// TestCreateInstanceDefaults 缺省标题 = 实例化时模板最新名存档；created_from_version
// 记录当时最新版；视图携带模板现势连接。
func TestCreateInstanceDefaults(t *testing.T) {
	svc := newInstTestService(t)
	ctx := context.Background()
	mustSpec(t, svc, "slot.research-lead")

	inst, err := svc.CreateInstance(ctx, "slot.research-lead", "")
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if !hasPrefix(inst.SlotInstanceID, "si_") || !hasPrefix(inst.KnowledgeNamespace, "slotns_") {
		t.Fatalf("ID/命名空间前缀错: %s %s", inst.SlotInstanceID, inst.KnowledgeNamespace)
	}
	if inst.Title != "研究负责人" || inst.CreatedFromVersion != 1 || inst.Scope != "workspace" {
		t.Fatalf("缺省形状错: %+v", inst.SlotInstance)
	}
	if inst.Spec.Name != "研究负责人" || inst.Spec.LatestVersion != 1 {
		t.Fatalf("模板现势连接错: %+v", inst.Spec)
	}

	// 模板追加 v2（改名）：实例标题（存档）不变，现势连接演进
	c2 := validContent()
	c2.Name = "研究负责人（修订）"
	if _, err := svc.AppendVersion(ctx, "slot.research-lead", c2); err != nil {
		t.Fatalf("追加: %v", err)
	}
	got, ok, err := svc.GetInstance(ctx, inst.SlotInstanceID)
	if err != nil || !ok {
		t.Fatalf("get: %v %v", err, ok)
	}
	if got.Title != "研究负责人" {
		t.Fatalf("实例标题被模板演进回写: %q", got.Title)
	}
	if got.Spec.Name != "研究负责人（修订）" || got.Spec.LatestVersion != 2 {
		t.Fatalf("现势连接未演进: %+v", got.Spec)
	}
	// 新实例：缺省标题取新最新名、from_version=2
	inst2, _ := svc.CreateInstance(ctx, "slot.research-lead", "")
	if inst2.Title != "研究负责人（修订）" || inst2.CreatedFromVersion != 2 {
		t.Fatalf("新实例缺省错: %+v", inst2.SlotInstance)
	}
}

// TestCreateInstanceTitleValidation 显式标题校验（空/超长拒）。
func TestCreateInstanceTitleValidation(t *testing.T) {
	svc := newInstTestService(t)
	ctx := context.Background()
	mustSpec(t, svc, "slot.a")
	if _, err := svc.CreateInstance(ctx, "slot.a", "  "); !errors.Is(err, ErrInvalidInstance) {
		t.Fatalf("空标题 err = %v", err)
	}
	if _, err := svc.CreateInstance(ctx, "slot.a", maxRunes(65)); !errors.Is(err, ErrInvalidInstance) {
		t.Fatalf("超长标题 err = %v", err)
	}
	if _, err := svc.CreateInstance(ctx, "slot.a", " 显式标题 "); err != nil {
		t.Fatalf("合法显式标题: %v", err)
	}
}

// TestCreateInstanceUnknownSpec 未知谱系 → ErrNotFound。
func TestCreateInstanceUnknownSpec(t *testing.T) {
	svc := newInstTestService(t)
	if _, err := svc.CreateInstance(context.Background(), "slot.missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestMultipleInstancesDistinctNamespaces 同模板多实例：命名空间与 ID 互异、
// 稳定；列表创建序。
func TestMultipleInstancesDistinctNamespaces(t *testing.T) {
	svc := newInstTestService(t)
	ctx := context.Background()
	mustSpec(t, svc, "slot.multi")
	ids := map[string]bool{}
	namespaces := map[string]bool{}
	for i := 0; i < 3; i++ {
		inst, err := svc.CreateInstance(ctx, "slot.multi", "")
		if err != nil {
			t.Fatalf("实例#%d: %v", i, err)
		}
		if ids[inst.SlotInstanceID] || namespaces[inst.KnowledgeNamespace] {
			t.Fatalf("ID/命名空间重复: %s %s", inst.SlotInstanceID, inst.KnowledgeNamespace)
		}
		ids[inst.SlotInstanceID] = true
		namespaces[inst.KnowledgeNamespace] = true
	}
	list, err := svc.ListInstances(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v n=%d", err, len(list))
	}
	// 创建序 = 生成序（si_test_2 < si_test_4 < si_test_6——生成器交替取号）
	for i := 1; i < len(list); i++ {
		if list[i].SlotInstanceID <= list[i-1].SlotInstanceID {
			t.Fatalf("列表非创建序: %v", list)
		}
	}
}

// TestGetInstanceUnknown 单查未知实例。
func TestGetInstanceUnknown(t *testing.T) {
	svc := newInstTestService(t)
	if _, ok, err := svc.GetInstance(context.Background(), "si_missing"); err != nil || ok {
		t.Fatalf("get: %v %v", err, ok)
	}
}

// TestConcurrentCreateInstances 并发建 8 实例：ID 与命名空间全唯一。
func TestConcurrentCreateInstances(t *testing.T) {
	svc := newInstTestService(t)
	ctx := context.Background()
	mustSpec(t, svc, "slot.race")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.CreateInstance(ctx, "slot.race", ""); err != nil {
				t.Errorf("并发实例: %v", err)
			}
		}()
	}
	wg.Wait()
	list, _ := svc.ListInstances(ctx)
	if len(list) != 8 {
		t.Fatalf("实例数 = %d, want 8", len(list))
	}
	ids := map[string]bool{}
	nss := map[string]bool{}
	for _, inst := range list {
		ids[inst.SlotInstanceID] = true
		nss[inst.KnowledgeNamespace] = true
	}
	if len(ids) != 8 || len(nss) != 8 {
		t.Fatalf("唯一性破：ids=%d namespaces=%d", len(ids), len(nss))
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
