package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// sessionKeySep 会话键分隔（profile × 房间——ADR-0013/M4-4：群聊与私聊、
// 不同房间各持各的 CLI 线程，kimi -S / mcode --session 句柄不跨房串线）。
const sessionKeySep = "\x00"

// sessionKey (profile, room) → 会话键；无房间上下文（测试/内部直调）退回纯 profile 键。
func sessionKey(profileID, roomID string) string {
	if roomID == "" {
		return profileID
	}
	return profileID + sessionKeySep + roomID
}

// Supervisor 管理适配器注册与逻辑 Session 的生命周期（M0 最小实现）。
// v1 演进点（RFC-0002 v0.5）：会话驱动权 lease + fencing token、进程树资源限额。
type Supervisor struct {
	mu       sync.Mutex
	adapters map[string]Adapter
	sessions map[string]Session // key: profile_id 或 profile_id\x00room_id（M4-4 按房间独立映射）
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
// 的全部会话（含各房间分支——四轮复审 #9：不驱逐则旧 session 继续吃旧适配器
// 实例与旧 thread，替换形同虚设；M4-4 实例替换不复用旧会话）；空键拒绝。
func (s *Supervisor) RegisterFor(profileKey string, a Adapter) error {
	if profileKey == "" {
		return fmt.Errorf("agent: RegisterFor 需要 profile 键")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapters[profileKey] = a
	prefix := profileKey + sessionKeySep
	for k, sess := range s.sessions {
		if k == profileKey || strings.HasPrefix(k, prefix) {
			sess.Close() // 换实例：旧会话（旧进程路径/旧 thread 连续性）不再复用
			delete(s.sessions, k)
		}
	}
	return nil
}

// Submit 按需 Boot 会话并下发任务。会话按（ProfileID, RoomID）独立映射
// （ADR-0013：每房间一线程；房间上下文由任务携带）。
//
// Boot 在锁内完成（M0 取舍）：保证"每会话键恰好 Boot 一次"的 ownership 语义
// （见 TestSupervisorBootsSessionOnceConcurrent）。真实适配器引入后如 Boot 变慢，
// 演进为 per-key once + 租约（RFC-0002 v0.5 fencing 演进项）。
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
	key := sessionKey(profile.ProfileID, task.RoomID)
	if session, ok := s.sessions[key]; ok {
		return session.Run(ctx, task)
	}
	session, err := adapter.Boot(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("agent: boot %s: %w", profile.Adapter, err)
	}
	s.sessions[key] = session
	return session.Run(ctx, task)
}

// CapableOf 查询 Profile 解析到的适配器是否具备某能力（与 Submit 同一解析
// 顺序：先 ProfileID 后适配器名）。M4-1：run_task 命令面的能力门。
func (s *Supervisor) CapableOf(profile Profile, has func(Capabilities) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	adapter, ok := s.adapters[profile.ProfileID]
	if !ok {
		adapter, ok = s.adapters[profile.Adapter]
	}
	if !ok {
		return false
	}
	return has(adapter.Capabilities())
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
