//go:build it

// IT 层（ADR-0027 v0.2 / 0.6.4 审校）：migration v18 工作区别名排他、
// 原子条件续租/释放（不复活过期租约）、单语句到期（续租后不误过期）、
// v17 开发期旧库升级（ALTER 容错）。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	mosaicruntime "github.com/sisibeloved/Mosaic/internal/runtime"
)

// TestEnvironmentWorkspaceKeyIT v18 建列 + 身份判重 + 领取面按身份排他。
func TestEnvironmentWorkspaceKeyIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v18 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 18`).Scan(&v18)
	if v18 != 1 {
		t.Fatalf("migration v18 未登记: %d", v18)
	}

	// 同 workspace_key 双注册 → 目录占用冲突（environment_id 不同也拒）
	mk := func(id, dir, key string) mosaicruntime.Environment {
		return mosaicruntime.Environment{EnvironmentID: id, Name: id, WorkspaceDir: dir,
			WorkspaceKey: key, CreatedAt: "t", UpdatedAt: "t"}
	}
	if err := store.CreateEnvironment(ctx, mk("env_a", "/d/a", "/real/ws")); err != nil {
		t.Fatalf("建环境甲: %v", err)
	}
	if err := store.CreateEnvironment(ctx, mk("env_b", "/d/b-symlink", "/real/ws")); !errors.Is(err, mosaicruntime.ErrEnvironmentDirTaken) {
		t.Fatalf("同身份（别名）应判重: %v", err)
	}
	// 空键（历史行）不受部分唯一索引限制
	if err := store.CreateEnvironment(ctx, mk("env_c", "/d/c", "")); err != nil {
		t.Fatalf("空键历史行应放行: %v", err)
	}
	// 身份唯一是库级事实：直改第二行到既有键也被索引拦截（同键双行
	// 不可能存在——领取面的身份 JOIN 因此是纵深防御，语义在内存存储
	// 同径测试钉住 TestWorkspaceKeyAcquireExclusivity）
	if _, err := store.db.Exec(
		`UPDATE environments SET workspace_key = '/real/ws' WHERE environment_id = 'env_c'`); err == nil {
		t.Fatal("同身份直改应被部分唯一索引拦截")
	}
}

// TestLeaseRenewAtomicIT 审校 P1：到期先落后续租——原子条件更新零行
// 409，不复活 active、不改到期时刻与令牌。
func TestLeaseRenewAtomicIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	l, err := store.AcquireLease(ctx, activeLease("lse_r1", "rtx_r1", "env_r", 0, "2026-10-30T10:01:00.000000000Z"))
	if err != nil {
		t.Fatalf("领取: %v", err)
	}
	// 到期先落（单语句判定）
	if err := store.ExpireOverdue(ctx, "2026-10-30T10:02:00.000000000Z"); err != nil {
		t.Fatalf("到期: %v", err)
	}
	if _, err := store.RenewLease(ctx, l.LeaseID, "2026-10-30T11:00:00.000000000Z", "2026-10-30T10:02:30.000000000Z"); !errors.Is(err, mosaicruntime.ErrLeaseInactive) {
		t.Fatalf("过期续租: %v", err)
	}
	got, _, _ := store.LeaseByID(ctx, l.LeaseID)
	if got.State != mosaicruntime.LeaseStateExpired || got.ExpiresAt != l.ExpiresAt || got.FenceToken != l.FenceToken {
		t.Fatalf("过期租约被旧续租改动: %+v → %+v", l, got)
	}
	// 活跃续租：只更新续租字段（令牌/代次/状态不动）
	l2, err := store.AcquireLease(ctx, activeLease("lse_r2", "rtx_r1", "env_r", 0, "2026-10-30T10:03:00.000000000Z"))
	if err != nil {
		t.Fatalf("再领: %v", err)
	}
	rn, err := store.RenewLease(ctx, l2.LeaseID, "2026-10-30T10:30:00.000000000Z", "2026-10-30T10:03:30.000000000Z")
	if err != nil || rn.ExpiresAt != "2026-10-30T10:30:00.000000000Z" || rn.FenceToken != l2.FenceToken || rn.Generation != l2.Generation {
		t.Fatalf("活跃续租: %+v %v", rn, err)
	}
}

// TestLeaseExpireDoesNotClobberRenewalIT 审校 P2#3：续租后的未来截止不因
// 旧到期时刻被误转 expired（单语句原子判定——窗口结构性消除）。
func TestLeaseExpireDoesNotClobberRenewalIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	l, err := store.AcquireLease(ctx, activeLease("lse_e1", "rtx_e1", "env_e", 0, "2026-10-30T10:01:00.000000000Z"))
	if err != nil {
		t.Fatalf("领取: %v", err)
	}
	// 续租到未来（晚于本次到期判定的 now）
	if _, err := store.RenewLease(ctx, l.LeaseID, "2026-10-30T12:00:00.000000000Z", "2026-10-30T10:00:30.000000000Z"); err != nil {
		t.Fatalf("续租: %v", err)
	}
	// 到期判定（now 晚于旧到期、早于新到期）——不得误转
	if err := store.ExpireOverdue(ctx, "2026-10-30T10:02:00.000000000Z"); err != nil {
		t.Fatalf("到期判定: %v", err)
	}
	got, _, _ := store.LeaseByID(ctx, l.LeaseID)
	if got.State != mosaicruntime.LeaseStateActive || got.ExpiresAt != "2026-10-30T12:00:00.000000000Z" {
		t.Fatalf("续租被到期扫描覆盖: %+v", got)
	}
	// 越过新到期后再判 → 转 expired
	if err := store.ExpireOverdue(ctx, "2026-10-30T12:00:01.000000000Z"); err != nil {
		t.Fatalf("再判: %v", err)
	}
	got, _, _ = store.LeaseByID(ctx, l.LeaseID)
	if got.State != mosaicruntime.LeaseStateExpired {
		t.Fatalf("真到期应转 expired: %+v", got)
	}
}

// TestLeaseV17LegacyUpgradeIT 开发期 v17 旧库（无 workspace_key 列）升级：
// v18 ALTER 增列（duplicate column 容错）后别名排他生效、旧数据不动。
func TestLeaseV17LegacyUpgradeIT(t *testing.T) {
	// v18 独立成块（migrationV18）——schemaDDL 本身即开发期 v17 完整面
	legacyDDL := schemaDDL
	if strings.Contains(legacyDDL, "workspace_key TEXT") {
		t.Fatal("schemaDDL 不应含 v18 列——测试前提失效")
	}
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v064dev.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	// 开发期 v17 旧数据：无 key 列的环境行
	if _, err := db.Exec(`INSERT INTO environments (environment_id, name, workspace_dir, is_default, created_at, updated_at)
VALUES ('env_old', '旧', '/d/old', 1, 't', 't')`); err != nil {
		db.Close()
		t.Fatalf("旧环境行: %v", err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("升级打开: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var v18 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 18`).Scan(&v18)
	if v18 != 1 {
		t.Fatalf("升级后 v18 应登记: %d", v18)
	}
	// 旧行保留且空键放行
	old, ok, _ := store.EnvironmentByID(ctx, "env_old")
	if !ok || old.WorkspaceDir != "/d/old" || old.WorkspaceKey != "" {
		t.Fatalf("旧环境行应保留（空键）: %+v", old)
	}
	// 新建带 key 环境可用；同 key 判重生效
	env := mosaicruntime.Environment{EnvironmentID: "env_new", Name: "新", WorkspaceDir: "/d/new",
		WorkspaceKey: "/real/n", CreatedAt: "t", UpdatedAt: "t"}
	if err := store.CreateEnvironment(ctx, env); err != nil {
		t.Fatalf("新建: %v", err)
	}
	env.EnvironmentID = "env_dup"
	if err := store.CreateEnvironment(ctx, env); !errors.Is(err, mosaicruntime.ErrEnvironmentDirTaken) {
		t.Fatalf("同身份判重: %v", err)
	}
}
