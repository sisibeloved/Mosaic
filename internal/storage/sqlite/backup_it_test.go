//go:build it

// IT 层：备份面 × 真实 SQLite——VACUUM INTO 快照一致性（含在途 WAL）与
// 快照可独立打开（恢复启动段的物理前提）。
package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/backup"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func msgEnvelope(id string, seq int64, body string) protocol.Envelope {
	return protocol.Envelope{
		EventID: id, TenantID: "ten_local", RoomID: "r_backup", Seq: seq,
		Type: protocol.EventMessagePosted, SchemaVersion: 1,
		OccurredAt: "2026-09-07T00:00:00.000Z",
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Visibility: protocol.Visibility{Kind: "public"},
		Payload:    []byte(`{"body":"` + body + `"}`),
	}
}

// VACUUM INTO 产物是自包含库：WAL 中未 checkpoint 的事件必须在快照里。
func TestBackupToProducesIndependentSnapshot(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{msgEnvelope("e1", 1, "before-backup")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := store.BackupTo(ctx, dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// 快照后再写（WAL 前进）——不影响已生成快照
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{msgEnvelope("e2", 2, "after-backup")}); err != nil {
		t.Fatalf("append2: %v", err)
	}

	snap, err := Open(dest)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()
	events, _, err := snap.EventsAfter(ctx, "r_backup", "", 100)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if len(events) != 1 || string(events[0].Envelope.Payload) == "" {
		t.Fatalf("快照应含备份时点事件且不含其后事件: %d 项", len(events))
	}
}

// backup.Manager × 真实库：创建→列表→恢复标记→启动换库全链（fake DB 的
// UT 语义在此以真实 VACUUM INTO 复验）。库直接开在 dataDir/mosaic.db——
// 与生产装配同布局（安全副本断言依赖此路径）。
func TestManagerRoundtripWithRealStore(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(filepath.Join(dataDir, "mosaic.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{msgEnvelope("e1", 1, "room-created-era")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	m := &backup.Manager{DB: store, Dir: dataDir}
	sum, err := m.Create(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Verify(sum.BackupID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// "后续演进"：追加事件 + WAL 前进
	if _, err := store.AppendEvents(ctx, []protocol.Envelope{msgEnvelope("e2", 2, "later-era")}); err != nil {
		t.Fatalf("append2: %v", err)
	}
	if err := m.RequestRestore(sum.BackupID); err != nil {
		t.Fatalf("request restore: %v", err)
	}
	// 换库（先关原库句柄——模拟重启时序）
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	applied, err := backup.ApplyPendingRestore(dataDir)
	if err != nil || applied == nil {
		t.Fatalf("apply: %v %+v", err, applied)
	}
	restored, err := Open(filepath.Join(dataDir, "mosaic.db"))
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer restored.Close()
	events, _, err := restored.EventsAfter(ctx, "r_backup", "", 100)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("恢复后应回到备份时点（1 事件），got %d", len(events))
	}
	// 原库（含后来事件）在安全副本目录可回滚
	if _, err := os.Stat(filepath.Join(applied.SafetyDir, "mosaic.db")); err != nil {
		t.Fatalf("safety copy: %v", err)
	}
}
