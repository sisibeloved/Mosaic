//go:build it

// IT 层（ADR-0026 / 0.6.3）：environments/runtimes/runtime_events 经真实
// SQLite——migration v16 新库、发现幂等 upsert（既有行只刷事实）、事件流
// 回读、目录占用冲突、默认环境恰一、v0.6.2 真旧库升级。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	mosaicruntime "github.com/sisibeloved/Mosaic/internal/runtime"
)

func discoveredRow(id, version string) mosaicruntime.Runtime {
	return mosaicruntime.Runtime{
		RuntimeID: id, Adapter: "codex", Host: mosaicruntime.HostNative,
		Path: "/usr/local/bin/codex", Channel: "cli", Version: version,
		Login: "logged_in", State: mosaicruntime.StateDiscovered,
		DiscoveredAt: "2026-10-30T10:00:00.000Z", UpdatedAt: "2026-10-30T10:00:00.000Z",
	}
}

// TestRuntimeStoreIT v16 建表 + 同步幂等 + 状态不被覆盖 + 事件流。
func TestRuntimeStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v16 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 16`).Scan(&v16)
	if v16 != 1 {
		t.Fatalf("migration v16 未登记: %d", v16)
	}

	// 首同步：新行 + discovered 事件
	created, refreshed, err := store.SyncRuntimes(ctx, "2026-10-30T10:00:00.000Z",
		[]mosaicruntime.Runtime{discoveredRow("rtx_a", "1.0.0")},
		[]mosaicruntime.Event{{RuntimeID: "rtx_a", EventType: mosaicruntime.EventDiscovered,
			Payload: map[string]any{"version": "1.0.0"}, OccurredAt: "2026-10-30T10:00:00.000Z"}})
	if err != nil || created != 1 || refreshed != 0 {
		t.Fatalf("首同步：created=%d refreshed=%d err=%v", created, refreshed, err)
	}

	// 准备后（state=ready、绑定、发现时间不动）再同步：既有行只刷事实
	ready := discoveredRow("rtx_a", "1.0.0")
	ready.State = mosaicruntime.StateReady
	ready.EnvironmentID = "env_x"
	// discovered_at 是创建事实：UpdateRuntime 与同步均不改写（保持首同步时刻）
	if err := store.UpdateRuntime(ctx, ready, []mosaicruntime.Event{{
		RuntimeID: "rtx_a", EventType: mosaicruntime.EventStateChanged,
		Payload: map[string]any{"from": "discovered", "to": "ready"}, OccurredAt: "2026-10-30T10:01:00.000Z",
	}}); err != nil {
		t.Fatalf("更新运行时: %v", err)
	}
	updated := discoveredRow("rtx_a", "9.9.9") // 服务层以既有行（含状态）构造
	updated.State = mosaicruntime.StateReady
	updated.EnvironmentID = "env_x"
	created, refreshed, err = store.SyncRuntimes(ctx, "2026-10-30T10:02:00.000Z", []mosaicruntime.Runtime{updated}, nil)
	if err != nil || created != 0 || refreshed != 1 {
		t.Fatalf("重同步：created=%d refreshed=%d err=%v", created, refreshed, err)
	}
	got, ok, err := store.RuntimeByID(ctx, "rtx_a")
	if err != nil || !ok {
		t.Fatalf("单查: ok=%v err=%v", ok, err)
	}
	if got.State != mosaicruntime.StateReady || got.EnvironmentID != "env_x" ||
		got.DiscoveredAt != "2026-10-30T10:00:00.000Z" || got.Version != "9.9.9" {
		t.Fatalf("同步覆盖了本域事实：%+v", got)
	}

	// 事件流回读（payload 反序列化）
	evs, _, err := store.RuntimeEvents(ctx, "rtx_a")
	if err != nil || len(evs) != 2 {
		t.Fatalf("事件流: %v %d", err, len(evs))
	}
	if evs[0].EventType != mosaicruntime.EventDiscovered || evs[1].EventType != mosaicruntime.EventStateChanged ||
		evs[1].Payload["to"] != "ready" {
		t.Fatalf("事件回读错误：%+v", evs)
	}

	list, err := store.ListRuntimes(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("列表: %v %d", err, len(list))
	}
}

// TestEnvironmentStoreIT 建环境 + 目录占用 + 默认恰一 + 列表。
func TestEnvironmentStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	env := mosaicruntime.Environment{
		EnvironmentID: "env_a", Name: "默认", WorkspaceDir: "/tmp/ws-a",
		IsDefault: true, CreatedAt: "2026-10-30T10:00:00.000Z", UpdatedAt: "2026-10-30T10:00:00.000Z",
	}
	if err := store.CreateEnvironment(ctx, env); err != nil {
		t.Fatalf("建环境: %v", err)
	}
	// 目录占用 → 冲突
	dup := env
	dup.EnvironmentID, dup.IsDefault, dup.Name = "env_b", false, "乙"
	if err := store.CreateEnvironment(ctx, dup); !errors.Is(err, mosaicruntime.ErrEnvironmentDirTaken) {
		t.Fatalf("目录占用期望 ErrEnvironmentDirTaken：%v", err)
	}
	// 第二默认 → 部分唯一索引拒绝
	d2 := mosaicruntime.Environment{
		EnvironmentID: "env_c", Name: "丙", WorkspaceDir: "/tmp/ws-c",
		IsDefault: true, CreatedAt: "2026-10-30T10:00:00.000Z", UpdatedAt: "2026-10-30T10:00:00.000Z",
	}
	if err := store.CreateEnvironment(ctx, d2); err == nil {
		t.Fatal("第二默认应被部分唯一索引拒绝")
	}
	// 默认查询 + 普通环境
	d3 := d2
	d3.EnvironmentID, d3.IsDefault, d3.WorkspaceDir = "env_d", false, "/tmp/ws-d"
	if err := store.CreateEnvironment(ctx, d3); err != nil {
		t.Fatalf("建普通环境: %v", err)
	}
	def, ok, err := store.DefaultEnvironment(ctx)
	if err != nil || !ok || def.EnvironmentID != "env_a" {
		t.Fatalf("默认环境: ok=%v err=%v %+v", ok, err, def)
	}
	list, err := store.ListEnvironments(ctx)
	if err != nil || len(list) != 2 || !list[0].IsDefault {
		t.Fatalf("环境列表（默认在前）: %v %+v", err, list)
	}
	if _, ok, _ := store.EnvironmentByID(ctx, "env_none"); ok {
		t.Fatal("未知环境应 ok=false")
	}
}

// TestRuntimeLegacyUpgradeIT v0.6.2 真旧库升级到 v16（三表在席、旧数据不动）。
func TestRuntimeLegacyUpgradeIT(t *testing.T) {
	marker := "-- v16（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v16 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v062.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧库升级打开: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var v16 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 16`).Scan(&v16)
	if v16 != 1 {
		t.Fatalf("升级后 migration v16 应登记: %d", v16)
	}
	if _, _, err := store.SyncRuntimes(ctx, "2026-10-30T10:00:00.000Z",
		[]mosaicruntime.Runtime{discoveredRow("rtx_l", "1.0.0")}, nil); err != nil {
		t.Fatalf("升级库同步: %v", err)
	}
	env := mosaicruntime.Environment{EnvironmentID: "env_l", Name: "默认", WorkspaceDir: "/tmp/ws-l",
		IsDefault: true, CreatedAt: "t", UpdatedAt: "t"}
	if err := store.CreateEnvironment(ctx, env); err != nil {
		t.Fatalf("升级库建环境: %v", err)
	}
}
