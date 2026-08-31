package agent

import (
	"context"
	"fmt"
	"sync"
)

// Supervisor 管理适配器注册与逻辑 Session 的生命周期（M0 最小实现）。
// v1 演进点（RFC-0002 v0.5）：会话驱动权 lease + fencing token、进程树资源限额。
type Supervisor struct {
	mu       sync.Mutex
	adapters map[string]Adapter
	sessions map[string]Session // key: profile_id（M0：每 Profile 一个逻辑会话）
}

func NewSupervisor() *Supervisor {
	return &Supervisor{
		adapters: make(map[string]Adapter),
		sessions: make(map[string]Session),
	}
}

// Register 登记适配器；重名拒绝。
func (s *Supervisor) Register(a Adapter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.adapters[a.Name()]; ok {
		return fmt.Errorf("agent: adapter %q already registered", a.Name())
	}
	s.adapters[a.Name()] = a
	return nil
}

// RegisterFor 以 Profile 键登记适配器（复审 #3：同名适配器的多个 executable——
// 如 native 与 WSL 两份 codex——若只按 Name() 登记，后注册者被拒、座位却照加，
// 全部流量折叠到首个实例）。同键重登即替换（resync 刷新配置）并驱逐该 Profile
// 的既有会话（四轮复审 #9：不驱逐则旧 session 继续吃旧适配器实例与旧 thread，
// 替换形同虚设）；空键拒绝。
func (s *Supervisor) RegisterFor(profileKey string, a Adapter) error {
	if profileKey == "" {
		return fmt.Errorf("agent: RegisterFor 需要 profile 键")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapters[profileKey] = a
	if sess, ok := s.sessions[profileKey]; ok {
		sess.Close() // 换实例：旧会话（旧进程路径/旧 thread 连续性）不再复用
		delete(s.sessions, profileKey)
	}
	return nil
}

// Submit 按需 Boot 会话并下发任务。
//
// Boot 在锁内完成（M0 取舍）：保证"每 Profile 恰好 Boot 一次"的 ownership 语义
// （见 TestSupervisorBootsSessionOnceConcurrent）。真实适配器引入后如 Boot 变慢，
// 演进为 per-profile once + 租约（RFC-0002 v0.5 fencing 演进项）。
func (s *Supervisor) Submit(ctx context.Context, profile Profile, task Task) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 解析顺序：先按 ProfileID 精确命中（多 executable 同名适配器各有实例），
	// 回退按适配器名单实例（echo 等 conformance 适配器）。
	adapter, ok := s.adapters[profile.ProfileID]
	if !ok {
		adapter, ok = s.adapters[profile.Adapter]
	}
	if !ok {
		return nil, fmt.Errorf("agent: adapter %q not registered", profile.Adapter)
	}
	if session, ok := s.sessions[profile.ProfileID]; ok {
		return session.Run(ctx, task)
	}
	session, err := adapter.Boot(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("agent: boot %s: %w", profile.Adapter, err)
	}
	s.sessions[profile.ProfileID] = session
	return session.Run(ctx, task)
}

// Shutdown 关闭全部逻辑会话。
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, session := range s.sessions {
		session.Close()
		delete(s.sessions, id)
	}
}
