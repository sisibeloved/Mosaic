// 0.6.4 审校修复回归（ADR-0027 v0.2）：P1 旧续租不得复活已过期租约及
// 原令牌（原子核对状态、只更新续租字段）；P2#2 工作区别名绕过独占
// （EvalSymlinks 规范身份判重 + 领取面按身份排他兜底）。
package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestRenewDoesNotReviveExpired 审校 P1：并发到期先落，旧续租不得整行写回
// 复活 active 与原令牌——续租必须原子核对当前状态，只更新续租字段。
func TestRenewDoesNotReviveExpired(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	rt, _ := f.readyRuntime(t, "codex", "/bin/codex")
	l, err := f.svc.Acquire(ctx, rt.RuntimeID, "mi_ok", 60, nil)
	if err != nil {
		t.Fatalf("领取: %v", err)
	}
	// 竞争交错：到期判定先于续租提交落库
	f.advance(61)
	if err := f.svc.expireOverdue(ctx, *f.clock); err != nil {
		t.Fatalf("到期: %v", err)
	}
	if _, err := f.svc.Renew(ctx, l.LeaseID, 120); !errors.Is(err, ErrLeaseInactive) {
		t.Fatalf("过期续租: %v", err)
	}
	got, _, _ := f.svc.GetLease(ctx, l.LeaseID)
	if got.State != LeaseStateExpired || got.ExpiresAt != l.ExpiresAt || got.FenceToken != l.FenceToken {
		t.Fatalf("过期租约被旧续租改动（复活/改期/换令牌）：%+v → %+v", l, got)
	}
}

// TestWorkspaceAliasExclusivity 审校 P2#2：同一物理目录经符号链接别名注册
// 不得绕过工作区独占——按规范工作区身份（EvalSymlinks）判重。
func TestWorkspaceAliasExclusivity(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	base := t.TempDir()
	real := base + "/ws"
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	alias := base + "/link-ws"
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("符号链接不可用（权限面）——别名排他以 linux CI 覆盖: %v", err)
	}
	if _, err := f.svc.CreateEnvironment(ctx, "甲", real); err != nil {
		t.Fatalf("建环境甲: %v", err)
	}
	if _, err := f.svc.CreateEnvironment(ctx, "乙", alias); !errors.Is(err, ErrEnvironmentDirTaken) {
		t.Fatalf("别名目录应判重（同一物理工作区）：%v", err)
	}
}

// TestWorkspaceKeyAcquireExclusivity 审校 P2#2（领取面）：库内已有同身份
// 工作区的活跃租约时，另一环境（同 workspace_key）领取必须工作区冲突——
// 排他以规范身份计，不依 environment_id。
func TestWorkspaceKeyAcquireExclusivity(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := context.Background()
	base := t.TempDir()
	wsA, wsB := base+"/a", base+"/b"
	if err := os.MkdirAll(wsA, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.MkdirAll(wsB, 0o755); err != nil {
		t.Fatalf("建目录乙: %v", err)
	}
	envA, err := f.svc.CreateEnvironment(ctx, "甲", wsA)
	if err != nil {
		t.Fatalf("建环境甲: %v", err)
	}
	envB, err := f.svc.CreateEnvironment(ctx, "乙", wsB)
	if err != nil {
		t.Fatalf("建环境乙: %v", err)
	}
	// 直接播种同 workspace_key（模拟历史别名行——创建面已拒，领取面兜底）
	ms := f.svc.store.(*MemStore)
	ms.mu.Lock()
	envB.WorkspaceKey = envA.WorkspaceKey
	ms.envs[envB.EnvironmentID] = envB
	ms.mu.Unlock()

	rtA, _ := f.readyRuntime(t, "codex", "/bin/codex")
	if _, _, err := f.svc.Sync(ctx, []DiscoveredEntry{{Adapter: "kimi", Host: HostNative, Path: "/bin/kimi", Version: "1.0.0"}}); err != nil {
		t.Fatalf("同步乙: %v", err)
	}
	list, _ := f.svc.ListRuntimes(ctx)
	var rtB Runtime
	for _, r := range list {
		if r.Adapter == "kimi" {
			rtB = r
		}
	}
	if _, err := f.svc.Prepare(ctx, rtA.RuntimeID, envA.EnvironmentID); err != nil {
		t.Fatalf("准备甲: %v", err)
	}
	if _, err := f.svc.Prepare(ctx, rtB.RuntimeID, envB.EnvironmentID); err != nil {
		t.Fatalf("准备乙: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rtA.RuntimeID, "mi_ok", 0, nil); err != nil {
		t.Fatalf("首领: %v", err)
	}
	if _, err := f.svc.Acquire(ctx, rtB.RuntimeID, "mi_ok2", 0, nil); !errors.Is(err, ErrLeaseConflictWorkspace) {
		t.Fatalf("同身份工作区跨环境领取应冲突：%v", err)
	}
}
