package dispatch

// 0.6.1 网关首切片（ADR-0024）：resolve resolved/blocked 族、dispatch 幂等
// 三态、决策过期、复核状态变化、并发孪生恰一 Mission。测试先行。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/mission"
)

type fakeSources struct{ tasks map[[2]string]bool }

func (f fakeSources) RoomTaskExists(_ context.Context, roomID, taskID string) (bool, error) {
	return f.tasks[[2]string{roomID, taskID}], nil
}

type fakeAgents struct{ ids map[string]bool }

func (f fakeAgents) AgentExists(_ context.Context, agentID string) (bool, error) {
	return f.ids[agentID], nil
}

func contract() mission.Content {
	return mission.Content{
		Objective:          "房间任务交办",
		Scope:              "0.6.1 网关首切片",
		AcceptanceCriteria: []string{"验收全绿"},
		DeliverableSpecs:   []string{"报告"},
		SourceRefs:         []string{"room:r1:tsk_a"},
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// newFixture 真 mission 服务 + dispatch（同 memstore，真域链）。
func newFixture(t *testing.T) (*Service, *mission.MemStore) {
	t.Helper()
	mem := mission.NewMemStore()
	agents := &fakeAgents{ids: map[string]bool{"ag_a": true}}
	sources := fakeSources{tasks: map[[2]string]bool{{"r1", "tsk_a"}: true}}
	var n atomic.Int64
	mSvc := mission.New(mem,
		mission.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_d" + itoa(n.Add(1)) }),
		mission.WithAgentLookup(agents),
		mission.WithSourceLookup(sources))
	svc := New(NewMemStore(), mSvc, Lookups{
		Agents:  agents,
		Sources: sources,
		Mapped:  mSvc.SourceMapped,
	}, WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		WithNewRouteID(func() string { return "rd_" + itoa(n.Add(1)) }),
		WithNewReceiptID(func() string { return "dpr_" + itoa(n.Add(1)) }))
	return svc, mem
}

func request() ResolveRequest {
	return ResolveRequest{
		Kind:         KindCreateMission,
		RoomSource:   mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"},
		OwnerAgentID: "ag_a",
		Contract:     contract(),
	}
}

// TestResolveBlockedFamily 预检 blocked 族（恒 200 决策，blocked 非错误）；
// 非法 kind → ErrInvalidRequest（结构问题不是决策）。
func TestResolveBlockedFamily(t *testing.T) {
	svc, _ := newFixture(t)
	ctx := context.Background()

	for name, mutate := range map[string]func(*ResolveRequest) string{
		"任务卡不存在": func(r *ResolveRequest) string {
			r.RoomSource = mission.RoomSource{RoomID: "rX", TaskID: "tsk_z"}
			return "task_not_found"
		},
		"owner 不存在": func(r *ResolveRequest) string {
			r.OwnerAgentID = "ag_none"
			return "agent_not_found"
		},
		"契约非法": func(r *ResolveRequest) string {
			r.Contract.AcceptanceCriteria = []string{" "}
			return "invalid_mission"
		},
	} {
		req := request()
		want := mutate(&req)
		d, err := svc.Resolve(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d.Status != "blocked" || d.ReasonCode != want || len(d.TargetRefs) != 0 {
			t.Fatalf("%s 决策失真: %+v", name, d)
		}
	}

	if _, err := svc.Resolve(ctx, ResolveRequest{Kind: "semantic", RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("kind 应 ErrInvalidRequest: %v", err)
	}
}

// TestDispatchHappyPathAndIdempotency 全链：resolved → 投递建 Mission（含
// 来源映射与 ready）→ 同键同路由重放 → 同键异路由冲突 → 回执查询 →
// 映射后解析 blocked 回显 mission_id。
func TestDispatchHappyPathAndIdempotency(t *testing.T) {
	svc, mem := newFixture(t)
	ctx := context.Background()

	d, err := svc.Resolve(ctx, request())
	if err != nil || d.Status != "resolved" {
		t.Fatalf("解析: %+v %v", d, err)
	}
	r1, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d.RouteID, IdempotencyKey: "key-1", RequestID: "req-1"})
	if err != nil {
		t.Fatalf("投递: %v", err)
	}
	if r1.DomainCommandID == "" || r1.RequestDigest != d.RequestDigest || r1.CommittedAt == "" || r1.DomainCommandID[:3] != "mi_" {
		t.Fatalf("回执失真: %+v", r1)
	}
	m, ok, _ := mem.MissionByID(ctx, r1.DomainCommandID)
	if !ok || m.RoomSource == nil || m.RoomSource.TaskID != "tsk_a" || m.Status != mission.StatusReady {
		t.Fatalf("Mission 建立失真: %+v", m)
	}

	// 顺序重放 ×7：同键同路由 → 同一回执
	for i := 0; i < 7; i++ {
		r2, replayed, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d.RouteID, IdempotencyKey: "key-1"})
		if err != nil && !replayed || replayed && r2.ReceiptID != r1.ReceiptID || err != nil {
			t.Fatalf("幂等重放失真: %+v %v", r2, err)
		}
	}
	// 同一请求重新解析（任务卡已映射 → blocked，但摘要相同）→ 同键重试
	// 应重放原回执（审校 P2#4：摘要即请求身份，路由不同亦重放；投递成功
	// 改变预检状态是常态，重试不得因此失去重放）
	d2, _ := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission,
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, OwnerAgentID: "ag_a", Contract: contract()})
	r3, replayed3, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "key-1"})
	if err != nil || !replayed3 || r3.ReceiptID != r1.ReceiptID {
		t.Fatalf("同摘要异路由应重放: replayed=%v err=%v", replayed3, err)
	}
	// 真正的异载荷（不同契约）同键 → 冲突
	diffContract := contract()
	diffContract.Objective = "不同请求"
	d4, _ := svc.Resolve(ctx, ResolveRequest{Kind: KindCreateMission,
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: diffContract})
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d4.RouteID, IdempotencyKey: "key-1"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("同键异载荷应冲突: %v", err)
	}
	// 回执查询
	got, found, _ := svc.ReceiptByKey(ctx, "key-1")
	if !found || got.ReceiptID != r1.ReceiptID {
		t.Fatalf("回执查询失真: %+v", got)
	}
	// 映射后解析同来源 → blocked 回显 mission_id
	d3, _ := svc.Resolve(ctx, request())
	if d3.Status != "blocked" || d3.ReasonCode != "source_already_mapped" || !strings.Contains(d3.ReasonSummary, r1.DomainCommandID) {
		t.Fatalf("映射后解析应 blocked 并回显 mission_id: %+v", d3)
	}
}

// TestDispatchStaleAndNotFound 未知决策 route_not_found；过期决策 route_stale；
// 复核时映射被占（另一 Mission 先占任务卡）→ source_already_mapped。
func TestDispatchStaleAndNotFound(t *testing.T) {
	svc, _ := newFixture(t)
	ctx := context.Background()

	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: "rd_none", IdempotencyKey: "k"}); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("未知决策: %v", err)
	}
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: "rd_x", IdempotencyKey: "bad key!"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("非法幂等键: %v", err)
	}

	// 过期：解析后把决策改写为已过期
	d, _ := svc.Resolve(ctx, request())
	stale := d
	stale.ExpiresAt = "2026-10-30T11:00:00.000Z"
	if err := svc.store.SaveDecision(ctx, stale); err != nil {
		t.Fatalf("改写决策: %v", err)
	}
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d.RouteID, IdempotencyKey: "k2"}); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("过期决策应 route_stale: %v", err)
	}

	// 复核时映射被占（占位者非本键投递）→ source_already_mapped 可见恢复
	d2, _ := svc.Resolve(ctx, request()) // 映射仍空闲时解析
	if _, err := svc.missions.Create(ctx, "", contract(), &mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}); err != nil {
		t.Fatalf("占位: %v", err)
	}
	if _, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "k3"}); !errors.Is(err, mission.ErrSourceAlreadyMapped) {
		t.Fatalf("映射被占应 source_already_mapped: %v", err)
	}
}

// TestDispatchConcurrentSameKey 并发同键孪生：恰一 Mission 落库（败者重放
// 胜者回执或可见冲突——绝不二次派单）。
func TestDispatchConcurrentSameKey(t *testing.T) {
	svc, mem := newFixture(t)
	ctx := context.Background()
	d, _ := svc.Resolve(ctx, request())

	var wg sync.WaitGroup
	var receiptMu sync.Mutex
	receipts := map[string]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d.RouteID, IdempotencyKey: "same-key"})
			if err != nil {
				return // 孪生竞态败者：可见错误（不产生重复派单）——由恰一 Mission 断言兜底
			}
			receiptMu.Lock()
			receipts[r.ReceiptID] = true
			receiptMu.Unlock()
		}()
	}
	wg.Wait()
	if len(receipts) > 1 {
		t.Fatalf("孪生应共享回执: %v", receipts)
	}
	ms, _ := mem.ListMissions(ctx)
	if len(ms) != 1 {
		t.Fatalf("同键应恰一 Mission: %d", len(ms))
	}
}

// TestDispatchKeyClaimIsolation（审校 P1）：同键异源并发——旧代码在首投
// 落回执前双方都通过幂等检查、各自创建 Mission；修复后后到者在创建前于
// 键认领处退出。门控证明双向确定：创建器进入即报数。
type gatedMissions struct {
	inner   Missions
	entered chan struct{}
	release chan struct{}
}

func (g *gatedMissions) Create(ctx context.Context, owner string, c mission.Content, src *mission.RoomSource) (mission.Mission, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.inner.Create(ctx, owner, c, src)
}

func TestDispatchKeyClaimIsolation(t *testing.T) {
	svc, mem := newFixture(t)
	ctx := context.Background()
	srcFake := fakeSources{tasks: map[[2]string]bool{{"r1", "tsk_a"}: true, {"r1", "tsk_b"}: true}}
	mem2 := mission.NewMemStore()
	agents := &fakeAgents{ids: map[string]bool{"ag_a": true}}
	var n atomic.Int64
	mSvc := mission.New(mem2,
		mission.WithNow(func() string { return "2026-10-30T12:00:00.000Z" }),
		mission.WithNewMissionID(func() string { return "mi_c" + itoa(n.Add(1)) }),
		mission.WithAgentLookup(agents), mission.WithSourceLookup(srcFake))
	gated := &gatedMissions{inner: mSvc, entered: make(chan struct{}, 4), release: make(chan struct{})}
	svc.missions = gated
	svc.lookups.Sources = srcFake
	svc.lookups.Mapped = mSvc.SourceMapped

	reqA := ResolveRequest{Kind: KindCreateMission,
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_a"}, Contract: contract()}
	reqB := ResolveRequest{Kind: KindCreateMission,
		RoomSource: mission.RoomSource{RoomID: "r1", TaskID: "tsk_b"}, Contract: contract()}
	dA, _ := svc.Resolve(ctx, reqA)
	dB, _ := svc.Resolve(ctx, reqB)

	// A 投递：进入创建即挂起（回执未落）
	errCh := make(chan error, 1)
	go func() {
		_, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: dA.RouteID, IdempotencyKey: "same"})
		errCh <- err
	}()
	<-gated.entered

	// B 同键异源：应在创建前退出（创建器不出现第二次进入），零副作用
	bErr := make(chan error, 1)
	go func() {
		_, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: dB.RouteID, IdempotencyKey: "same"})
		bErr <- err
	}()
	select {
	case <-gated.entered:
		close(gated.release)
		t.Fatal("同键异源不得进入创建（应在键认领处退出）")
	case err := <-bErr:
		if !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("同键异源应冲突退出: %v", err)
		}
	}
	close(gated.release)
	if err := <-errCh; err != nil {
		t.Fatalf("首投: %v", err)
	}
	ms, _ := mem2.ListMissions(ctx)
	if len(ms) != 1 {
		t.Fatalf("同键异源不得产生第二份 Mission: %d", len(ms))
	}
	_ = mem
}

// TestDispatchSameDigestDifferentRouteReplay（审校 P2#4）：同一请求解析两次
// 得两个路由（摘要相同）——首投后，第二路由同键重试应重放原回执。
func TestDispatchSameDigestDifferentRouteReplay(t *testing.T) {
	svc, _ := newFixture(t)
	ctx := context.Background()
	req := request()
	d1, _ := svc.Resolve(ctx, req)
	d2, _ := svc.Resolve(ctx, req)
	if d1.RequestDigest != d2.RequestDigest || d1.RouteID == d2.RouteID {
		t.Fatalf("同请求两次解析应同摘要异路由: %+v vs %+v", d1, d2)
	}
	r1, _, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d1.RouteID, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("首投: %v", err)
	}
	r2, replayed, err := svc.Dispatch(ctx, DispatchCmd{RouteID: d2.RouteID, IdempotencyKey: "k"})
	if err != nil || !replayed || r2.ReceiptID != r1.ReceiptID {
		t.Fatalf("同摘要异路由应重放原回执: replayed=%v err=%v %+v", replayed, err, r2)
	}
}
