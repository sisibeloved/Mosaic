// RuntimeLease（0.6.4 / ADR-0027）：Run 对 Runtime 的限时占用、租约代次
// 与资源限额声明。互斥双面——同一可写工作区（环境）至多一个活跃租约
// （跨运行时，RFC §5.2"同一可写工作区须独占租约"）+ 单运行时并发 1。
// 围栏令牌拒绝旧实例提交（只保护 Mosaic 接收的状态与受控工具调用，不撤
// 销外部已发生动作——RFC §10.3.1 原文裁定）；到期惰性判定（触碰即转）。
package runtime

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

// 租约状态。
const (
	LeaseStateActive   = "active"
	LeaseStateReleased = "released"
	LeaseStateExpired  = "expired"
	LeaseStateAll      = "all"
)

// 租期（秒）：缺省 3600，界限 [60, 86400]（一天——与 Mission 墙钟上限同刻度）。
const (
	DefaultLeaseTTLSeconds = 3600
	MinLeaseTTLSeconds     = 60
	MaxLeaseTTLSeconds     = 86400
)

// 围栏校验失败原因。
const (
	FenceReasonTokenMismatch = "fence_token_mismatch"
	FenceReasonNotActive     = "lease_not_active"
	FenceReasonExpired       = "lease_expired"
	FenceReasonStaleGen      = "generation_stale"
)

var (
	runtimeIDLeasePattern = regexp.MustCompile(`^rtx_[0-9A-Za-z_-]+$`)
	missionIDPattern      = regexp.MustCompile(`^mi_[0-9A-Za-z_-]+$`)
)

// 领域错误（租约面）。
var (
	// ErrInvalidLease 领取/续租请求不合法（ID 格式/TTL 界限）。
	ErrInvalidLease = fmt.Errorf("runtime: 租约请求不合法")
	// ErrMissionNotFound 占用主体不存在。
	ErrMissionNotFound = fmt.Errorf("runtime: 占用主体不存在")
	// ErrMissionCanceled 占用主体已取消。
	ErrMissionCanceled = fmt.Errorf("runtime: 占用主体已取消")
	// ErrRuntimeNotReady 运行时未就绪（非 ready 或未绑环境）。
	ErrRuntimeNotReady = fmt.Errorf("runtime: 运行时未就绪")
	// ErrLeaseConflictRuntime 单运行时容量占用。
	ErrLeaseConflictRuntime = fmt.Errorf("runtime: 运行时已被租约占用")
	// ErrLeaseConflictWorkspace 工作区独占冲突（跨运行时）。
	ErrLeaseConflictWorkspace = fmt.Errorf("runtime: 工作区已被租约独占")
	// ErrLeaseNotFound 租约不存在。
	ErrLeaseNotFound = fmt.Errorf("runtime: 租约不存在")
	// ErrLeaseInactive 租约非活跃（released/expired）。
	ErrLeaseInactive = fmt.Errorf("runtime: 租约非活跃")
)

// Lease 租约行（RFC §10.3.1 的 0.6.4 子集；run_id 随 0.6.5 Run 对象增列）。
type Lease struct {
	LeaseID        string         `json:"lease_id"` // lse_ 前缀
	RuntimeID      string         `json:"runtime_id"`
	EnvironmentID  string         `json:"environment_id"`
	WorkspaceRef   string         `json:"workspace_ref"` // 环境工作区目录
	MissionID      string         `json:"mission_id"`    // 占用主体
	Generation     int            `json:"generation"`    // 领取代次（运行时当代 = MAX）
	FenceToken     string         `json:"fence_token"`   // fnc_ 前缀围栏令牌
	State          string         `json:"state"`         // active | released | expired
	ResourceLimits map[string]int `json:"resource_limits"`
	AcquiredAt     string         `json:"acquired_at"`
	ExpiresAt      string         `json:"expires_at"`
	ReleasedAt     string         `json:"released_at"`
	UpdatedAt      string         `json:"updated_at"`
}

// Missions 占用主体端口（app 适配 missionSvc——域不 import mission 包）。
type Missions interface {
	// MissionState 主体契约状态（draft/ready/canceled）；不存在 ok=false。
	MissionState(ctx context.Context, missionID string) (state string, ok bool, err error)
}

// ---- Service 租约面 ----

// WithNewLeaseID 注入租约 ID 生成器（测试定序用）。
func WithNewLeaseID(f func() string) Option { return func(s *Service) { s.newLeaseID = f } }

// WithNewFenceToken 注入围栏令牌生成器（测试定序用）。
func WithNewFenceToken(f func() string) Option { return func(s *Service) { s.newFenceToken = f } }

// WithMissions 注入占用主体端口。
func WithMissions(m Missions) Option { return func(s *Service) { s.missions = m } }

func newFenceTokenRandom() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	return "fnc_" + hex.EncodeToString(b[:])
}

// Acquire 领取租约：前置（ready+已绑环境；主体存在且非 canceled）→ 惰性
// 清障 → 互斥判定（事务内）→ 代次 = 当代+1 → 签发围栏令牌。
func (s *Service) Acquire(ctx context.Context, runtimeID, missionID string, ttlSeconds int, limits map[string]int) (Lease, error) {
	if !runtimeIDLeasePattern.MatchString(runtimeID) || !missionIDPattern.MatchString(missionID) {
		return Lease{}, fmt.Errorf("%w：runtime_id/mission_id 前缀不合法", ErrInvalidLease)
	}
	ttl := DefaultLeaseTTLSeconds
	if ttlSeconds != 0 {
		if ttlSeconds < MinLeaseTTLSeconds || ttlSeconds > MaxLeaseTTLSeconds {
			return Lease{}, fmt.Errorf("%w：ttl_seconds 须 [%d, %d]（%d）", ErrInvalidLease, MinLeaseTTLSeconds, MaxLeaseTTLSeconds, ttlSeconds)
		}
		ttl = ttlSeconds
	}
	if limits == nil {
		limits = map[string]int{"concurrency": 1}
	}
	now := s.now()
	if err := s.expireOverdue(ctx, now); err != nil {
		return Lease{}, err
	}

	mu := s.lockOf(runtimeID)
	mu.Lock()
	defer mu.Unlock()

	rt, ok, err := s.store.RuntimeByID(ctx, runtimeID)
	if err != nil {
		return Lease{}, fmt.Errorf("runtime: 读运行时失败：%w", err)
	}
	if !ok {
		return Lease{}, ErrRuntimeNotFound
	}
	if rt.State != StateReady || rt.EnvironmentID == "" {
		return Lease{}, fmt.Errorf("%w：%s（%s，未绑环境=%v）", ErrRuntimeNotReady, runtimeID, rt.State, rt.EnvironmentID == "")
	}
	env, ok, err := s.store.EnvironmentByID(ctx, rt.EnvironmentID)
	if err != nil {
		return Lease{}, fmt.Errorf("runtime: 读环境失败：%w", err)
	}
	if !ok {
		return Lease{}, fmt.Errorf("%w：绑定环境缺失 %s", ErrRuntimeNotReady, rt.EnvironmentID)
	}
	if s.missions != nil {
		state, ok, err := s.missions.MissionState(ctx, missionID)
		if err != nil {
			return Lease{}, fmt.Errorf("runtime: 读占用主体失败：%w", err)
		}
		if !ok {
			return Lease{}, ErrMissionNotFound
		}
		if state == "canceled" {
			return Lease{}, ErrMissionCanceled
		}
	}

	lease := Lease{
		LeaseID:   s.newLeaseID(),
		RuntimeID: rt.RuntimeID, EnvironmentID: env.EnvironmentID, WorkspaceRef: env.WorkspaceDir,
		MissionID:      missionID,
		FenceToken:     s.newFenceToken(),
		State:          LeaseStateActive,
		ResourceLimits: limits,
		AcquiredAt:     now, ExpiresAt: addTTL(now, ttl), UpdatedAt: now,
	}
	return s.store.AcquireLease(ctx, lease)
}

// Renew 续租（心跳）：从现在起延长（不从旧到期点叠加）；非活跃 409。
func (s *Service) Renew(ctx context.Context, leaseID string, ttlSeconds int) (Lease, error) {
	ttl := DefaultLeaseTTLSeconds
	if ttlSeconds != 0 {
		if ttlSeconds < MinLeaseTTLSeconds || ttlSeconds > MaxLeaseTTLSeconds {
			return Lease{}, fmt.Errorf("%w：ttl_seconds 须 [%d, %d]", ErrInvalidLease, MinLeaseTTLSeconds, MaxLeaseTTLSeconds)
		}
		ttl = ttlSeconds
	}
	now := s.now()
	if err := s.expireOverdue(ctx, now); err != nil {
		return Lease{}, err
	}
	mu := s.lockOf(s.runtimeOfLease(ctx, leaseID))
	mu.Lock()
	defer mu.Unlock()

	l, ok, err := s.store.LeaseByID(ctx, leaseID)
	if err != nil {
		return Lease{}, fmt.Errorf("runtime: 读租约失败：%w", err)
	}
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	if l.State != LeaseStateActive {
		return Lease{}, fmt.Errorf("%w：%s（%s）", ErrLeaseInactive, leaseID, l.State)
	}
	l.ExpiresAt = addTTL(now, ttl)
	l.UpdatedAt = now
	if err := s.store.UpdateLease(ctx, l); err != nil {
		return Lease{}, fmt.Errorf("runtime: 续租落库失败：%w", err)
	}
	return l, nil
}

// Release 释放（终态）：该租约代次自此失效（围栏令牌不可再用）。
func (s *Service) Release(ctx context.Context, leaseID string) (Lease, error) {
	now := s.now()
	if err := s.expireOverdue(ctx, now); err != nil {
		return Lease{}, err
	}
	mu := s.lockOf(s.runtimeOfLease(ctx, leaseID))
	mu.Lock()
	defer mu.Unlock()

	l, ok, err := s.store.LeaseByID(ctx, leaseID)
	if err != nil {
		return Lease{}, fmt.Errorf("runtime: 读租约失败：%w", err)
	}
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	if l.State != LeaseStateActive {
		return Lease{}, fmt.Errorf("%w：%s（%s）", ErrLeaseInactive, leaseID, l.State)
	}
	l.State = LeaseStateReleased
	l.ReleasedAt = now
	l.UpdatedAt = now
	if err := s.store.UpdateLease(ctx, l); err != nil {
		return Lease{}, fmt.Errorf("runtime: 释放落库失败：%w", err)
	}
	return l, nil
}

// CheckFence 围栏校验（probe 纪律恒结果不错误）：四关——令牌匹配、
// active、未过期（惰性已转）、代次为该运行时当代（无更新租约取代）。
func (s *Service) CheckFence(ctx context.Context, leaseID, fenceToken string) (bool, string, error) {
	now := s.now()
	if err := s.expireOverdue(ctx, now); err != nil {
		return false, "", err
	}
	l, ok, err := s.store.LeaseByID(ctx, leaseID)
	if err != nil {
		return false, "", fmt.Errorf("runtime: 读租约失败：%w", err)
	}
	if !ok {
		return false, "", ErrLeaseNotFound
	}
	switch {
	case l.FenceToken != fenceToken:
		return false, FenceReasonTokenMismatch, nil
	case l.State == LeaseStateExpired || !timeAfter(l.ExpiresAt, now):
		// 到期比非活跃更精确（惰性已转/时限双保险——先报 lease_expired）
		return false, FenceReasonExpired, nil
	case l.State != LeaseStateActive:
		return false, FenceReasonNotActive, nil
	}
	if cur, err := s.store.MaxLeaseGeneration(ctx, l.RuntimeID); err != nil {
		return false, "", err
	} else if l.Generation != cur {
		return false, FenceReasonStaleGen, nil
	}
	return true, "", nil
}

// GetLease 详情（惰性清障先行——读面不出现过期仍标 active 的行）。
func (s *Service) GetLease(ctx context.Context, leaseID string) (Lease, bool, error) {
	if err := s.expireOverdue(ctx, s.now()); err != nil {
		return Lease{}, false, err
	}
	return s.store.LeaseByID(ctx, leaseID)
}

// ListLeases 租约历史（诊断视图；state 缺省 all；领取序倒序）。
func (s *Service) ListLeases(ctx context.Context, state, runtimeID string) ([]Lease, error) {
	if err := s.expireOverdue(ctx, s.now()); err != nil {
		return nil, err
	}
	return s.store.ListLeases(ctx, state, runtimeID)
}

// expireOverdue 惰性到期：任一租约操作触碰时先把已过期的活跃租约转
// expired（确定性，无后台计时器）。
func (s *Service) expireOverdue(ctx context.Context, now string) error {
	return s.store.ExpireOverdue(ctx, now)
}

// runtimeOfLease 预读租约所属运行时（串行锁定位；锁后重读为准）。
func (s *Service) runtimeOfLease(ctx context.Context, leaseID string) string {
	l, ok, _ := s.store.LeaseByID(ctx, leaseID)
	if !ok {
		return leaseID // 缺省键：未知租约的锁不与真实运行时互串
	}
	return l.RuntimeID
}

func addTTL(nowRFC string, ttlSeconds int) string {
	base, err := time.Parse(time.RFC3339Nano, nowRFC)
	if err != nil {
		return nowRFC
	}
	return base.Add(time.Duration(ttlSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
}

// timeAfter a 时刻严格晚于 b（RFC3339Nano 解析比较——字符串比较不可靠）。
func timeAfter(a, b string) bool {
	ta, erra := time.Parse(time.RFC3339Nano, a)
	tb, errb := time.Parse(time.RFC3339Nano, b)
	if erra != nil || errb != nil {
		return false
	}
	return ta.After(tb)
}

// ---- Store 租约端口扩展（见 runtime.go Store 接口；SQLite/MemStore 实现）----
// AcquireLease 单事务：互斥判定（运行时容量 + 工作区独占——两查在
// BEGIN IMMEDIATE 临界区内，并发恰一胜者，库级部分唯一索引兜底）→
// 代次 = 当代 MAX+1 → 插入；冲突映射 ErrLeaseConflictRuntime/Workspace。
