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

// Submit 按需 Boot 会话并下发任务。
func (s *Supervisor) Submit(ctx context.Context, profile Profile, task Task) (Handle, error) {
	s.mu.Lock()
	adapter, ok := s.adapters[profile.Adapter]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("agent: adapter %q not registered", profile.Adapter)
	}
	session, ok := s.sessions[profile.ProfileID]
	s.mu.Unlock()

	if !ok {
		var err error
		session, err = adapter.Boot(ctx, profile)
		if err != nil {
			return nil, fmt.Errorf("agent: boot %s: %w", profile.Adapter, err)
		}
		s.mu.Lock()
		s.sessions[profile.ProfileID] = session
		s.mu.Unlock()
	}
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
