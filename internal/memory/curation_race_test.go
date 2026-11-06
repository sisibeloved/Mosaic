// 一审并发交错回归（0.7.1 / ADR-0038 审校 #1/#2）：interceptStore 在 CAS
// 前注入并发改动，精确复现"Get 之后、Update 之前条目被并发策展"的场景。
package memory

import (
	"context"
	"errors"
	"testing"
	"time"
)

// timeParse 测试助手（RFC3339 解析——失败由调用方断言处理）。
func timeParse(v string) (time.Time, error) { return time.Parse(time.RFC3339, v) }

// interceptStore 包装 MemStore——onBeforeCAS 在每次 UpdateItem/SupersedeItem
// 前回调（模拟并发交错），再透传真 CAS。
type interceptStore struct {
	*MemStore
	onBeforeCAS func(s *interceptStore)
}

func (i *interceptStore) UpdateItem(ctx context.Context, id string, v int, st Status, next Item) (Item, bool, error) {
	if i.onBeforeCAS != nil {
		i.onBeforeCAS(i)
	}
	return i.MemStore.UpdateItem(ctx, id, v, st, next)
}

func (i *interceptStore) SupersedeItem(ctx context.Context, id string, v int, st Status, oldNext, repl Item) (Item, Item, bool, error) {
	if i.onBeforeCAS != nil {
		i.onBeforeCAS(i)
	}
	return i.MemStore.SupersedeItem(ctx, id, v, st, oldNext, repl)
}

// raceFixture：一条 proposed v1 + intercept 存储。
func raceFixture(t *testing.T) (*interceptStore, *Service, Item) {
	t.Helper()
	ctx := context.Background()
	store := &interceptStore{MemStore: NewMemStore()}
	svc := NewService(store, Anchors{}, fixedNow)
	cmd := okCmd()
	cmd.Status = StatusProposed
	it, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return store, svc, it
}

// 一审 #1：并发撤回后旧修订不得复活终结条目（读快照 v1 + 请求 expected=2
// 的窗口——快照绑定预检直接拒；同版并撤由状态基准 CAS 拦）。
func TestReviseCannotResurrectSupersededRace(t *testing.T) {
	ctx := context.Background()

	t.Run("快照版本与请求不符即拒（不等 CAS）", func(t *testing.T) {
		_, svc, it := raceFixture(t)
		title := "x"
		// 请求 expected=2 但快照是 v1——旧代码会把 next（v1 基准）送到
		// 纯版本 CAS；若库已被并发推到 v2（如撤回 v1→v2）则误过。
		if _, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{ExpectedVersion: 2, Title: &title}); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("应 ErrVersionConflict（快照绑定），得 %v", err)
		}
	})

	t.Run("同版并发撤回——状态基准 CAS 拦（不复活）", func(t *testing.T) {
		store, svc, it := raceFixture(t)
		// 先接受为 active v1（被修订的对象）。
		if _, err := svc.Accept(ctx, it.MemoryID); err != nil {
			t.Fatal(err)
		}
		store.onBeforeCAS = func(s *interceptStore) {
			// CAS 前并发撤回：active v1 → superseded v2。
			c, _, _ := s.MemStore.ItemByID(ctx, it.MemoryID)
			c2 := c
			c2.Version, c2.Status = 2, StatusSuperseded
			s.MemStore.UpdateItem(ctx, it.MemoryID, 1, StatusActive, c2)
			s.onBeforeCAS = nil // 只注入一次
		}
		title := "旧修订"
		_, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{ExpectedVersion: 1, Title: &title})
		if !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("应 ErrVersionConflict，得 %v", err)
		}
		// 终结事实未被覆盖。
		cur, _, _ := store.ItemByID(ctx, it.MemoryID)
		if cur.Status != StatusSuperseded || cur.Version != 2 {
			t.Fatalf("终结条目被覆盖：%s v%d", cur.Status, cur.Version)
		}
	})
}

// 一审 #2：并发接受后陈旧修订不得撤销接受/抹掉留痕（accept 不递增版本——
// 状态基准 CAS 分辨同名版本）。
func TestReviseCannotUndoAcceptRace(t *testing.T) {
	ctx := context.Background()
	store, svc, it := raceFixture(t)
	title := "陈旧修订"
	store.onBeforeCAS = func(s *interceptStore) {
		// CAS 前并发接受：proposed v1 → active v1（版本不变）。
		c, _, _ := s.MemStore.ItemByID(ctx, it.MemoryID)
		c2 := c
		c2.Status = StatusActive
		c2.EditHistory = appendEdit(c.EditHistory, EditRecord{Version: 1, Cause: CauseAccept, EditedBy: "owner", OccurredAt: fixedNow()})
		s.MemStore.UpdateItem(ctx, it.MemoryID, 1, StatusProposed, c2)
		s.onBeforeCAS = nil
	}
	// 陈旧修订基于 proposed v1 快照（expected=1 版本相符——纯版本 CAS 会过）。
	if _, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{ExpectedVersion: 1, Title: &title}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("应 ErrVersionConflict（状态基准），得 %v", err)
	}
	cur, _, _ := store.ItemByID(ctx, it.MemoryID)
	if cur.Status != StatusActive {
		t.Fatalf("接受被撤销：%s", cur.Status)
	}
	if len(cur.EditHistory) != 1 || cur.EditHistory[0].Cause != CauseAccept {
		t.Fatalf("接受留痕被抹：%+v", cur.EditHistory)
	}
}

// 一审 #2 伴随面：并发接受交错时 Accept 幂等收敛（不误报冲突）。
func TestAcceptConcurrentIdempotentRace(t *testing.T) {
	ctx := context.Background()
	store, svc, it := raceFixture(t)
	store.onBeforeCAS = func(s *interceptStore) {
		c, _, _ := s.MemStore.ItemByID(ctx, it.MemoryID)
		c2 := c
		c2.Status = StatusActive
		s.MemStore.UpdateItem(ctx, it.MemoryID, 1, StatusProposed, c2)
		s.onBeforeCAS = nil
	}
	got, err := svc.Accept(ctx, it.MemoryID)
	if err != nil || got.Status != StatusActive {
		t.Fatalf("并发接受应幂等收敛：%v %s", err, got.Status)
	}
}

// 一审 #1 伴随面：并发撤回交错的 Supersede 同拦（替代不复活终结条目）。
func TestSupersedeRaceBlocked(t *testing.T) {
	ctx := context.Background()
	store, svc, it := raceFixture(t)
	if _, err := svc.Accept(ctx, it.MemoryID); err != nil {
		t.Fatal(err)
	}
	store.onBeforeCAS = func(s *interceptStore) {
		c, _, _ := s.MemStore.ItemByID(ctx, it.MemoryID)
		c2 := c
		c2.Version, c2.Status = 2, StatusSuperseded
		s.MemStore.UpdateItem(ctx, it.MemoryID, 1, StatusActive, c2)
		s.onBeforeCAS = nil
	}
	_, _, err := svc.Supersede(ctx, it.MemoryID, 1, "", okCmd())
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("应 ErrVersionConflict，得 %v", err)
	}
	cur, _, _ := store.ItemByID(ctx, it.MemoryID)
	if cur.Status != StatusSuperseded {
		t.Fatalf("终结被覆盖：%s", cur.Status)
	}
}

// 一审 #4：写入侧 UTC 规范化——+15:00 偏移读回为 Z 格式（与 julianday
// 过滤同语义）。
func TestCreateNormalizesTimeToUTC(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), Anchors{}, fixedNow)
	cmd := okCmd()
	cmd.ObservedAt = "2026-11-06T12:00:00+15:00"
	cmd.ValidFrom = "2026-11-05T00:00:00+15:00"
	cmd.ValidTo = "2026-11-07T00:00:00+15:00"
	it, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"observed_at": it.ObservedAt, "valid_from": it.ValidFrom, "valid_to": it.ValidTo} {
		if len(v) > 0 && v[len(v)-1] != 'Z' {
			t.Fatalf("%s 应规范化 UTC（Z 后缀），得 %s", name, v)
		}
	}
	// 时刻等价（+15:00 = UTC 前一日 21:00）。
	if it.ObservedAt != "2026-11-05T21:00:00Z" {
		t.Fatalf("observed_at 时刻错：%s", it.ObservedAt)
	}
	// 修订与替代同规范化。
	clear := "2026-11-08T00:00:00+15:00"
	got, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{ExpectedVersion: 1, ValidTo: &clear})
	if err != nil || got.ValidTo != "2026-11-07T09:00:00Z" {
		t.Fatalf("修订规范化错：%v %s", err, got.ValidTo)
	}
}

// 二审 #1：UTC 规范化不得丢失小数秒——`.100Z/.900Z` 区间经创建/替代后
// 塌缩成整秒会引发"仅改标题修订 400"与"截止 .900Z 在 .500Z 提前隐藏"。
func TestNormTimeKeepsFractionalSeconds(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemStore(), Anchors{}, fixedNow)
	cmd := okCmd()
	cmd.ObservedAt = "2026-11-06T12:00:00.100Z"
	cmd.ValidFrom = "2026-11-06T12:00:00.100Z"
	cmd.ValidTo = "2026-11-06T12:00:00.900Z"
	it, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if it.ValidFrom != "2026-11-06T12:00:00.1Z" || it.ValidTo != "2026-11-06T12:00:00.9Z" {
		t.Fatalf("小数秒丢失（区间塌缩）：from=%s to=%s", it.ValidFrom, it.ValidTo)
	}
	if it.ObservedAt != "2026-11-06T12:00:00.1Z" {
		t.Fatalf("observed_at 小数秒丢失：%s", it.ObservedAt)
	}
	// 时刻无损（解析等价）。
	f, _ := timeParse(it.ValidFrom)
	o, _ := timeParse("2026-11-06T12:00:00.100Z")
	if !f.Equal(o) {
		t.Fatalf("时刻不等价：%v vs %v", f, o)
	}
	// 回归场景：区间条目仅改标题的修订应成功（整秒截断会 from==to → 400）。
	title := "只改标题"
	if _, err := svc.Revise(ctx, it.MemoryID, ReviseCmd{ExpectedVersion: 1, Title: &title}); err != nil {
		t.Fatalf("小数秒区间条目仅改标题修订失败（回归）：%v", err)
	}
	// 时效边界：截止 .900Z 在 .500Z 时刻未过期（提前隐藏回归）。
	if expired(it.ValidTo, "2026-11-06T12:00:00.5Z") {
		t.Fatalf("截止 .900Z 不应在 .500Z 判过期（小数秒截断回归）")
	}
	if !expired(it.ValidTo, "2026-11-06T12:00:00.95Z") {
		t.Fatalf(".95Z 应已过期")
	}
	// 替代路径同规范化（replacement 携带小数秒区间）。
	cmd2 := okCmd()
	cmd2.Title = "替代小数秒"
	cmd2.ObservedAt = "2026-11-06T12:00:00.100+15:00"
	_, newIt, err := svc.Supersede(ctx, it.MemoryID, 2, "", cmd2)
	if err != nil {
		t.Fatal(err)
	}
	if newIt.ObservedAt != "2026-11-05T21:00:00.1Z" {
		t.Fatalf("替代路径小数秒丢失：%s", newIt.ObservedAt)
	}
}
