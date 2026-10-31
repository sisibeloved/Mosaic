// UT 层：本地 Environment / Runtime（0.6.3 / ADR-0026）。发现幂等
// （同 (adapter,host,distro,path) 不新建行、状态不动）、探测转移
// （失败 → unhealthy 可见；恢复且已绑环境 → ready；未绑不升）、准备
// （绑环境 + 确保目录 + 探测；成功才换绑）、环境校验与目录占用、
// 事件流（discovered/health_checked/state_changed）。
package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// ---- 测试件 ----

type fakeProber struct {
	result ProbeResult
	err    error
	calls  int
}

func (f *fakeProber) Probe(_ context.Context, _ ProbeTarget) (ProbeResult, error) {
	f.calls++
	return f.result, f.err
}

type fakeFS struct {
	mkdirErr error
	ensured  []string
}

func (f *fakeFS) EnsureDir(dir string) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.ensured = append(f.ensured, dir)
	return nil
}

func okProbe() ProbeResult { return ProbeResult{OK: true, Version: "1.2.3", Login: "logged_in"} }
func badProbe() ProbeResult {
	return ProbeResult{OK: false, Version: "", Login: "unknown", Note: "version_probe_failed"}
}

func entryA(version string) DiscoveredEntry {
	return DiscoveredEntry{Adapter: "codex", Host: HostNative, Path: "/usr/local/bin/codex", Version: version, Login: "logged_in"}
}

func newFixture(t *testing.T) (fixture, *fakeProber) {
	t.Helper()
	prober := &fakeProber{result: okProbe()}
	fs := &fakeFS{}
	var n int64
	svc := New(NewMemStore(), prober, fs,
		WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		WithNewIDs(func() string { n++; return "rtx_ut" + strconv.FormatInt(n, 10) },
			func() string { n++; return "env_ut" + strconv.FormatInt(n, 10) }))
	return fixture{svc: svc, fs: fs}, prober
}

type fixture struct {
	svc *Service
	fs  *fakeFS
}

// firstRT 取库内第一行（同步后单行场景）。
func firstRT(t *testing.T, svc *Service) Runtime {
	t.Helper()
	list, err := svc.ListRuntimes(context.Background())
	if err != nil || len(list) == 0 {
		t.Fatalf("库内无运行时：%v %d", err, len(list))
	}
	return list[0]
}

func (f fixture) mustDefaultEnv(t *testing.T) Environment {
	t.Helper()
	env, err := f.svc.EnsureDefaultEnvironment(context.Background(), "默认工作区", "/data/workspace")
	if err != nil {
		t.Fatalf("建默认环境: %v", err)
	}
	return env
}

func TestSyncIdempotent(t *testing.T) {
	f, _ := newFixture(t)
	ctx := context.Background()

	created, refreshed, err := f.svc.Sync(ctx, []DiscoveredEntry{entryA("1.0.0"), {
		Adapter: "kimi", Host: HostWSL, Distro: "openEuler-24.03", Path: "/home/u/.local/bin/kimi", Login: "unknown",
	}})
	if err != nil || created != 2 || refreshed != 0 {
		t.Fatalf("首同步：created=%d refreshed=%d err=%v", created, refreshed, err)
	}
	list, _ := f.svc.ListRuntimes(ctx)
	if len(list) != 2 {
		t.Fatalf("应 2 行，得到 %d", len(list))
	}
	var a Runtime
	for _, r := range list {
		if r.Adapter == "codex" {
			a = r
		}
	}
	if a.State != StateDiscovered || a.EnvironmentID != "" || a.DiscoveredAt == "" {
		t.Fatalf("新行应为 discovered 未绑：%+v", a)
	}
	evs, _, _ := f.svc.RuntimeEventsByRuntime(ctx, a.RuntimeID)
	if len(evs) != 1 || evs[0].EventType != EventDiscovered {
		t.Fatalf("新行应记 discovered 事件：%+v", evs)
	}

	// 再同步（版本变化）：既有行只刷新事实，不新建行不动状态
	created, refreshed, err = f.svc.Sync(ctx, []DiscoveredEntry{entryA("1.1.0")})
	if err != nil || created != 0 || refreshed != 1 {
		t.Fatalf("重同步：created=%d refreshed=%d err=%v", created, refreshed, err)
	}
	list, _ = f.svc.ListRuntimes(ctx)
	if len(list) != 2 {
		t.Fatalf("重复发现不得新建行：%d", len(list))
	}
	for _, r := range list {
		if r.Adapter == "codex" && (r.Version != "1.1.0" || r.State != StateDiscovered) {
			t.Fatalf("事实刷新不得动状态：%+v", r)
		}
	}
	// 状态不被同步覆盖：ready 行再同步仍 ready
	f.mustDefaultEnv(t)
	prober := &fakeProber{result: okProbe()}
	fs := &fakeFS{}
	svc := New(NewMemStore(), prober, fs,
		WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		WithNewIDs(func() string { return "rtx_ut2" }, func() string { return "env_ut2" }))
	if _, _, err := svc.Sync(ctx, []DiscoveredEntry{entryA("1.0.0")}); err != nil {
		t.Fatalf("同步: %v", err)
	}
	env, _ := svc.CreateEnvironment(ctx, "默认", "/d/w")
	list2, _ := svc.ListRuntimes(ctx)
	prep, err := svc.Prepare(ctx, list2[0].RuntimeID, env.EnvironmentID)
	if err != nil || prep.State != StateReady {
		t.Fatalf("准备: %+v %v", prep, err)
	}
	if _, _, err := svc.Sync(ctx, []DiscoveredEntry{entryA("9.9.9")}); err != nil {
		t.Fatalf("再同步: %v", err)
	}
	after, _, _ := svc.RuntimeByID(ctx, list2[0].RuntimeID)
	if after.State != StateReady || after.Version != "9.9.9" || after.EnvironmentID != env.EnvironmentID {
		t.Fatalf("同步不得覆盖状态/绑定：%+v", after)
	}
}

func TestHealthCheckTransitions(t *testing.T) {
	f, prober := newFixture(t)
	ctx := context.Background()
	f.mustDefaultEnv(t)
	if _, _, err := f.svc.Sync(ctx, []DiscoveredEntry{entryA("1.0.0")}); err != nil {
		t.Fatalf("同步: %v", err)
	}
	rt := firstRT(t, f.svc)

	// discovered + 探测成功（未绑环境）→ 保持 discovered（ready 须绑环境经 prepare）
	prober.result = okProbe()
	got, err := f.svc.HealthCheck(ctx, rt.RuntimeID)
	if err != nil || got.State != StateDiscovered || got.Version != "1.2.3" || got.LastHealthAt == "" {
		t.Fatalf("未绑环境探测成功不得升 ready：%+v %v", got, err)
	}

	// prepare → ready（事件 state_changed）
	got, err = f.svc.Prepare(ctx, rt.RuntimeID, "")
	if err != nil || got.State != StateReady || got.EnvironmentID == "" {
		t.Fatalf("准备默认环境应 ready：%+v %v", got, err)
	}

	// 探测失败 → unhealthy（note 可见）
	prober.result = badProbe()
	got, err = f.svc.HealthCheck(ctx, rt.RuntimeID)
	if err != nil || got.State != StateUnhealthy || !strings.Contains(got.LastHealthNote, "version_probe_failed") {
		t.Fatalf("探测失败应 unhealthy 携 note：%+v %v", got, err)
	}

	// 探测恢复且已绑环境 → ready
	prober.result = okProbe()
	got, err = f.svc.HealthCheck(ctx, rt.RuntimeID)
	if err != nil || got.State != StateReady {
		t.Fatalf("已绑环境恢复应回 ready：%+v %v", got, err)
	}

	// 事件流：discovered + health_checked×4（含 prepare 内探测）+ state_changed×3
	evs, _, _ := f.svc.RuntimeEventsByRuntime(ctx, rt.RuntimeID)
	var healthN, stateN int
	for _, e := range evs {
		switch e.EventType {
		case EventHealthChecked:
			healthN++
		case EventStateChanged:
			stateN++
		}
	}
	if healthN != 4 || stateN != 3 {
		t.Fatalf("事件计数错误：health=%d state=%d（全量 %+v）", healthN, stateN, evs)
	}
}

func TestHealthCheckUnboundUnhealthyStays(t *testing.T) {
	f, prober := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.svc.Sync(ctx, []DiscoveredEntry{entryA("1.0.0")}); err != nil {
		t.Fatalf("同步: %v", err)
	}
	rt := firstRT(t, f.svc)
	// 未绑环境：探测失败 → unhealthy；探测恢复无环境可依 → 保持 unhealthy
	prober.result = badProbe()
	got, _ := f.svc.HealthCheck(ctx, rt.RuntimeID)
	if got.State != StateUnhealthy {
		t.Fatalf("探测失败应 unhealthy：%+v", got)
	}
	prober.result = okProbe()
	got, err := f.svc.HealthCheck(ctx, rt.RuntimeID)
	if err != nil || got.State != StateUnhealthy || got.EnvironmentID != "" {
		t.Fatalf("未绑环境的恢复不得升 ready：%+v %v", got, err)
	}
}

func TestPrepareSemantics(t *testing.T) {
	f, prober := newFixture(t)
	ctx := context.Background()
	env, err := f.svc.CreateEnvironment(ctx, "甲", "/data/ws-a")
	if err != nil {
		t.Fatalf("建环境: %v", err)
	}
	envB, err := f.svc.CreateEnvironment(ctx, "乙", "/data/ws-b")
	if err != nil {
		t.Fatalf("建环境乙: %v", err)
	}
	if _, _, err := f.svc.Sync(ctx, []DiscoveredEntry{entryA("1.0.0")}); err != nil {
		t.Fatalf("同步: %v", err)
	}
	rt := firstRT(t, f.svc)

	// 显式环境准备成功 → ready 绑定该环境；目录确保发生
	prober.result = okProbe()
	got, err := f.svc.Prepare(ctx, rt.RuntimeID, env.EnvironmentID)
	if err != nil || got.State != StateReady || got.EnvironmentID != env.EnvironmentID {
		t.Fatalf("准备: %+v %v", got, err)
	}
	if len(f.fs.ensured) == 0 || f.fs.ensured[len(f.fs.ensured)-1] != "/data/ws-a" {
		t.Fatalf("工作区目录未确保：%v", f.fs.ensured)
	}

	// re-prepare 换绑（0.6.3 无租约无在途）
	got, err = f.svc.Prepare(ctx, rt.RuntimeID, envB.EnvironmentID)
	if err != nil || got.EnvironmentID != envB.EnvironmentID || got.State != StateReady {
		t.Fatalf("换绑: %+v %v", got, err)
	}

	// 探测失败：state unhealthy，绑定保持（成功才换绑）
	prober.result = badProbe()
	got, err = f.svc.Prepare(ctx, rt.RuntimeID, env.EnvironmentID)
	if err != nil || got.State != StateUnhealthy || got.EnvironmentID != envB.EnvironmentID {
		t.Fatalf("失败准备应保持原绑定：%+v %v", got, err)
	}

	// 未知环境 404；未知运行时 404
	if _, err := f.svc.Prepare(ctx, rt.RuntimeID, "env_none"); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("未知环境期望 ErrEnvironmentNotFound：%v", err)
	}
	if _, err := f.svc.Prepare(ctx, "rtx_none", env.EnvironmentID); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("未知运行时期望 ErrRuntimeNotFound：%v", err)
	}

	// 目录确保失败 → ErrWorkspaceUnavailable
	f.fs.mkdirErr = errors.New("disk full")
	if _, err := f.svc.Prepare(ctx, rt.RuntimeID, envB.EnvironmentID); !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Fatalf("目录不可用期望 ErrWorkspaceUnavailable：%v", err)
	}

	// 无默认环境 + 空 id → environment_not_found 口径
	f2, _ := newFixture(t)
	if _, _, err := f2.svc.Sync(ctx, []DiscoveredEntry{entryA("1.0.0")}); err != nil {
		t.Fatalf("同步: %v", err)
	}
	rt2 := firstRT(t, f2.svc)
	if _, err := f2.svc.Prepare(ctx, rt2.RuntimeID, ""); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("无默认环境期望 ErrEnvironmentNotFound：%v", err)
	}
}

func TestCreateEnvironmentValidation(t *testing.T) {
	f, _ := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.CreateEnvironment(ctx, "", "/d/w"); !errors.Is(err, ErrInvalidEnvironment) {
		t.Fatalf("空名期望 ErrInvalidEnvironment：%v", err)
	}
	if _, err := f.svc.CreateEnvironment(ctx, strings.Repeat("名", 65), "/d/w"); !errors.Is(err, ErrInvalidEnvironment) {
		t.Fatalf("超长名期望 ErrInvalidEnvironment：%v", err)
	}
	if _, err := f.svc.CreateEnvironment(ctx, "名", " "); !errors.Is(err, ErrInvalidEnvironment) {
		t.Fatalf("空目录期望 ErrInvalidEnvironment：%v", err)
	}
	// 目录创建失败 → invalid（契约 400 invalid_environment）
	f.fs.mkdirErr = errors.New("ro")
	if _, err := f.svc.CreateEnvironment(ctx, "名", "/ro/w"); !errors.Is(err, ErrInvalidEnvironment) {
		t.Fatalf("目录不可创建期望 ErrInvalidEnvironment：%v", err)
	}
	f.fs.mkdirErr = nil // 复位：后续用例正常建目录
	// 同目录占用 → 冲突
	if _, err := f.svc.CreateEnvironment(ctx, "甲", "/d/w"); err != nil {
		t.Fatalf("建环境: %v", err)
	}
	if _, err := f.svc.CreateEnvironment(ctx, "乙", "/d/w"); !errors.Is(err, ErrEnvironmentDirTaken) {
		t.Fatalf("目录占用期望 ErrEnvironmentDirTaken：%v", err)
	}
	// 默认环境查询（首建即默认？不——默认由装配确保；此处库内无默认）
	if _, ok, _ := f.svc.DefaultEnv(ctx); ok {
		t.Fatal("未建默认不应命中")
	}
}
