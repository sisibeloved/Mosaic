// UT 层：RuntimeLease（0.6.4 / ADR-0027）。互斥（环境独占跨运行时 +
// 单运行时容量 1——并发恰一胜者）、前置（ready+已绑环境；主体存在且非
// canceled）、代次递增与围栏四关、续租从现在延长、到期惰性判定、终态族。
package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

// fakeLeaseMissions 主体端口测试件：mission_id → 契约状态。
type fakeLeaseMissions struct{ states map[string]string }

func (f fakeLeaseMissions) MissionState(_ context.Context, id string) (string, bool, error) {
	st, ok := f.states[id]
	return st, ok, nil
}

// leaseFixture：可推进时钟 + 就绪运行时（默认环境）+ 主体端口。
type leaseFixture struct {
	svc    *Service
	clock  *string
	prober *fakeProber
}

func newLeaseFixture(t *testing.T) leaseFixture {
	t.Helper()
	now := "2026-10-30T12:00:00.000000000Z"
	f := leaseFixture{clock: &now}
	f.prober = &fakeProber{result: ProbeResult{OK: true, Version: "1.0.0", Login: "logged_in"}}
	var n int64
	f.svc = New(NewMemStore(), f.prober, &fakeFS{},
		WithNow(func() string { return *f.clock }),
		WithNewIDs(func() string { n++; return "rtx_lt" + strconv.FormatInt(n, 10) },
			func() string { n++; return "env_lt" + strconv.FormatInt(n, 10) }),
		WithNewLeaseID(func() string { n++; return "lse_lt" + strconv.FormatInt(n, 10) }),
		WithNewFenceToken(func() string { n++; return "fnc_lt" + strconv.FormatInt(n, 10) }),
		WithMissions(fakeLeaseMissions{states: map[string]string{
			"mi_ok":  mission.StatusReady,
			"mi_ok2": mission.StatusReady,
			"mi_cx":  mission.StatusCanceled,
		}}))
	return f
}

// advance 推进时钟（秒）。
func (f *leaseFixture) advance(sec int) {
	base, _ := time.Parse(time.RFC3339Nano, *f.clock)
	*f.clock = base.Add(time.Duration(sec) * time.Second).UTC().Format(time.RFC3339Nano)
}

// readyRuntime 同步并准备一行（返回其 ID 与绑定环境目录）。
func (f *leaseFixture) readyRuntime(t *testing.T, adapter, path string) (Runtime, Environment) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.svc.Sync(ctx, []DiscoveredEntry{{Adapter: adapter, Host: HostNative, Path: path, Version: "1.0.0", Login: "logged_in"}}); err != nil {
		t.Fatalf("同步: %v", err)
	}
	list, _ := f.svc.ListRuntimes(ctx)
	var rt Runtime
	for _, r := range list {
		if r.Adapter == adapter {
			rt = r
		}
	}
	env, err := f.svc.EnsureDefaultEnvironment(ctx, "默认", "/d/lease-ws")
	if err != nil {
		t.Fatalf("默认环境: %v", err)
	}
	got, err := f.svc.Prepare(ctx, rt.RuntimeID, "")
	if err != nil || got.State != StateReady {
		t.Fatalf("准备: %+v %v", got, err)
	}
	return got, env
}

func TestLeaseAcquireHappy(t *testing.T) {
	f := newLeaseFixture(t)
	rt, env := f.readyRuntime(t, "codex", "/bin/codex")
	l, err := f.svc.Acquire(context.Background(), rt.RuntimeID, "mi_ok", 0, nil)
	if err != nil {
		t.Fatalf("领取: %v", err)
	}
	if l.State != LeaseStateActive || l.Generation != 1 || l.FenceToken == "" {
		t.Fatalf("租约事实错误: %+v", l)
	}
	if l.EnvironmentID != env.EnvironmentID || l.WorkspaceRef != env.WorkspaceDir || l.MissionID != "mi_ok" {
		t.Fatalf("租约绑定错误: %+v", l)
	}
	if l.ResourceLimits["concurrency"] != 1 {
		t.Fatalf("缺省限额错误: %+v", l.ResourceLimits)
	}
	want, _ := time.Parse(time.RFC3339Nano, *f.clock)
	if got, _ := time.Parse(time.RFC3339Nano, l.ExpiresAt); !got.Equal(want.Add(3600 * time.Second)) {
		t.Fatalf("缺省租期错误: %s", l.ExpiresAt)
	}
}

func TestLeaseAcquireConflicts(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	rt, _ := f.readyRuntime(t, "codex", "/bin/codex")

	// 单运行时容量：第二个领取（即使同主体）冲突
	if _, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 0, nil); err != nil {
		t.Fatalf("首领: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok2", 0, nil); !errors.Is(err, ErrLeaseConflictRuntime) {
		t.Fatalf("容量互斥期望 ErrLeaseConflictRuntime：%v", err)
	}

	// 环境独占跨运行时：另一运行时准备到同一环境后领取 → 工作区冲突
	if _, _, err := f.svc.Sync(ctx, []DiscoveredEntry{{Adapter: "kimi", Host: HostNative, Path: "/bin/kimi", Version: "1.0.0", Login: "logged_in"}}); err != nil {
		t.Fatalf("同步乙: %v", err)
	}
	list, _ := f.svc.ListRuntimes(ctx)
	var rtB Runtime
	for _, r := range list {
		if r.Adapter == "kimi" {
			rtB = r
		}
	}
	envs, _ := f.svc.ListEnvironments(ctx)
	if _, err := f.svc.Prepare(ctx, rtB.RuntimeID, envs[0].EnvironmentID); err != nil {
		t.Fatalf("准备乙: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rtB.RuntimeID, "mi_ok2", 0, nil); !errors.Is(err, ErrLeaseConflictWorkspace) {
		t.Fatalf("工作区独占期望 ErrLeaseConflictWorkspace：%v", err)
	}

	// 释放后再领：代次严格递增
	first, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 0, nil) // 已领——重取行
	_ = first
	leases, _ := f.svc.ListLeases(ctx, LeaseStateActive, "")
	if len(leases) != 1 {
		t.Fatalf("活跃租约应 1: %+v", leases)
	}
	if _, err := f.svc.Release(ctx, leases[0].LeaseID); err != nil {
		t.Fatalf("释放: %v", err)
	}
	_ = rtB
	second, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok2", 0, nil)
	if err != nil || second.Generation != 2 {
		t.Fatalf("再领应成功且代次 2: %+v %v", second, err)
	}
}

// TestLeaseAcquireConcurrentExactlyOne 并发领取恰一胜者（容量互斥）。
func TestLeaseAcquireConcurrentExactlyOne(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	rt, _ := f.readyRuntime(t, "codex", "/bin/codex")
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 0, nil)
		}(i)
	}
	wg.Wait()
	okN, conflictN := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			okN++
		case errors.Is(err, ErrLeaseConflictRuntime):
			conflictN++
		default:
			t.Fatalf("并发领取意外错误: %v", err)
		}
	}
	if okN != 1 || conflictN != 4 {
		t.Fatalf("并发领取应恰一胜者: ok=%d conflict=%d", okN, conflictN)
	}
}

func TestLeaseAcquirePreconditions(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	rt, _ := f.readyRuntime(t, "codex", "/bin/codex")

	if _, err := f.svc.Acquire(ctx, "rtx_none", "mi_ok", 0, nil); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("未知运行时: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_none", 0, nil); !errors.Is(err, ErrMissionNotFound) {
		t.Fatalf("未知主体: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_cx", 0, nil); !errors.Is(err, ErrMissionCanceled) {
		t.Fatalf("canceled 主体: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 30, nil); !errors.Is(err, ErrInvalidLease) {
		t.Fatalf("TTL 下界: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 86401, nil); !errors.Is(err, ErrInvalidLease) {
		t.Fatalf("TTL 上界: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, "bad", "mi_ok", 0, nil); !errors.Is(err, ErrInvalidLease) {
		t.Fatalf("ID 格式: %v", err)
	}
	// discovered（未准备）→ not ready
	if _, _, err := f.svc.Sync(ctx, []DiscoveredEntry{{Adapter: "kimi", Host: HostNative, Path: "/bin/kimi", Version: "1.0.0"}}); err != nil {
		t.Fatalf("同步乙: %v", err)
	}
	list, _ := f.svc.ListRuntimes(ctx)
	for _, r := range list {
		if r.Adapter == "kimi" {
			if _, err := f.svc.Acquire(ctx, r.RuntimeID, "mi_ok", 0, nil); !errors.Is(err, ErrRuntimeNotReady) {
				t.Fatalf("未准备期望 ErrRuntimeNotReady: %v", err)
			}
		}
	}
}

func TestLeaseRenewReleaseExpiry(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	rt, _ := f.readyRuntime(t, "codex", "/bin/codex")
	l, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 120, nil)
	if err != nil {
		t.Fatalf("领取: %v", err)
	}

	// 续租从现在延长（推进 60s后续租 120 → 新到期 = now+120）
	f.advance(60)
	rn, err := f.svc.Renew(ctx, l.LeaseID, 120)
	if err != nil {
		t.Fatalf("续租: %v", err)
	}
	want, _ := time.Parse(time.RFC3339Nano, *f.clock)
	if got, _ := time.Parse(time.RFC3339Nano, rn.ExpiresAt); !got.Equal(want.Add(120 * time.Second)) {
		t.Fatalf("续租应从现在延长: %s", rn.ExpiresAt)
	}

	// 到期（惰性）：推进超限后读面已见 expired；续租/释放 409
	f.advance(121)
	got, ok, _ := f.svc.GetLease(ctx, l.LeaseID)
	if !ok || got.State != LeaseStateExpired {
		t.Fatalf("惰性到期应见 expired: %+v", got)
	}
	if _, err := f.svc.Renew(ctx, l.LeaseID, 120); !errors.Is(err, ErrLeaseInactive) {
		t.Fatalf("过期续租: %v", err)
	}
	if _, err := f.svc.Release(ctx, l.LeaseID); !errors.Is(err, ErrLeaseInactive) {
		t.Fatalf("过期释放: %v", err)
	}

	// 释放语义（新租约）：终态 + released_at；重复释放 409
	l2, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 0, nil)
	if err != nil {
		t.Fatalf("再领: %v", err)
	}
	rel, err := f.svc.Release(ctx, l2.LeaseID)
	if err != nil || rel.State != LeaseStateReleased || rel.ReleasedAt == "" {
		t.Fatalf("释放: %+v %v", rel, err)
	}
	if _, err := f.svc.Release(ctx, l2.LeaseID); !errors.Is(err, ErrLeaseInactive) {
		t.Fatalf("重复释放: %v", err)
	}
}

func TestLeaseFence(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	rt, _ := f.readyRuntime(t, "codex", "/bin/codex")
	l, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 120, nil)
	if err != nil {
		t.Fatalf("领取: %v", err)
	}

	// 有效围栏
	if ok, reason, err := f.svc.CheckFence(ctx, l.LeaseID, l.FenceToken); err != nil || !ok || reason != "" {
		t.Fatalf("有效围栏: ok=%v reason=%s err=%v", ok, reason, err)
	}
	// 令牌不匹配
	if ok, reason, _ := f.svc.CheckFence(ctx, l.LeaseID, "fnc_wrong"); ok || reason != "fence_token_mismatch" {
		t.Fatalf("错令牌: ok=%v reason=%s", ok, reason)
	}
	// 释放后：lease_not_active
	if _, err := f.svc.Release(ctx, l.LeaseID); err != nil {
		t.Fatalf("释放: %v", err)
	}
	if ok, reason, _ := f.svc.CheckFence(ctx, l.LeaseID, l.FenceToken); ok || reason != "lease_not_active" {
		t.Fatalf("释放后围栏: ok=%v reason=%s", ok, reason)
	}
	// 到期（惰性）后：lease_expired
	l2, _ := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 60, nil)
	f.advance(61)
	if ok, reason, _ := f.svc.CheckFence(ctx, l2.LeaseID, l2.FenceToken); ok || reason != "lease_expired" {
		t.Fatalf("到期围栏: ok=%v reason=%s", ok, reason)
	}
	// 代次落后（容量 1 下经直接播种构造——多 Run 容量随 0.6.5）：仍活跃
	// 但非当代 → generation_stale。播种 = 库内另放一行更高代次（终态，
	// 不占活跃互斥），使当代 MAX 超过 l3 的代次。
	l3, _ := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 0, nil)
	ms := f.svc.store.(*MemStore)
	ms.mu.Lock()
	ms.leases["lse_seed_highgen"] = Lease{
		LeaseID: "lse_seed_highgen", RuntimeID: rt.RuntimeID, EnvironmentID: "env_seed",
		MissionID: "mi_ok", Generation: l3.Generation + 1, FenceToken: "fnc_seed",
		State: LeaseStateReleased, ResourceLimits: map[string]int{"concurrency": 1},
		AcquiredAt: l3.AcquiredAt, ExpiresAt: l3.ExpiresAt, ReleasedAt: l3.AcquiredAt, UpdatedAt: l3.UpdatedAt,
	}
	ms.mu.Unlock()
	if ok, reason, _ := f.svc.CheckFence(ctx, l3.LeaseID, l3.FenceToken); ok || reason != "generation_stale" {
		t.Fatalf("落后代次: ok=%v reason=%s", ok, reason)
	}
	// 未知租约
	if _, _, err := f.svc.CheckFence(ctx, "lse_none", "x"); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("未知租约: %v", err)
	}
}

func TestLeaseList(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	rt, _ := f.readyRuntime(t, "codex", "/bin/codex")
	l1, _ := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 60, nil)
	if _, err := f.svc.Release(ctx, l1.LeaseID); err != nil {
		t.Fatalf("释放: %v", err)
	}
	l2, _ := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok2", 0, nil)

	all, err := f.svc.ListLeases(ctx, LeaseStateAll, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("全量应 2: %v %+v", err, all)
	}
	if all[0].LeaseID != l2.LeaseID { // 领取序倒序（新在前）
		t.Fatalf("列表顺序错误: %+v", all)
	}
	active, _ := f.svc.ListLeases(ctx, LeaseStateActive, "")
	if len(active) != 1 || active[0].LeaseID != l2.LeaseID {
		t.Fatalf("活跃过滤: %+v", active)
	}
	released, _ := f.svc.ListLeases(ctx, LeaseStateReleased, "")
	if len(released) != 1 || released[0].LeaseID != l1.LeaseID {
		t.Fatalf("释放过滤: %+v", released)
	}
	byRT, _ := f.svc.ListLeases(ctx, LeaseStateActive, rt.RuntimeID)
	if len(byRT) != 1 {
		t.Fatalf("按运行时过滤: %+v", byRT)
	}
	byNone, _ := f.svc.ListLeases(ctx, LeaseStateActive, "rtx_none")
	if len(byNone) != 0 {
		t.Fatalf("未知运行时过滤: %+v", byNone)
	}
	_ = strings.TrimSpace("")
}
