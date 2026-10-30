//go:build it

// IT 层（ADR-0022 / 0.5.5）：slot_agent_grants 经真实 SQLite——migration v12
// 新库 + v0.5.4 旧库升级、upsert 幂等与并发、SpecByVersion、blocked 建任职与
// 重试的授权数据往返。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/slot"
)

func grantRow(agentID string) slot.AgentGrant {
	return slot.AgentGrant{
		AgentID: agentID,
		Skills:  []slot.SkillRef{{Ref: "workspace/skills/evidence-review", Version: 1}},
		Tools:   []string{"documents.read", "documents.write"},
		Context: []string{"mission.contract"},
		Runtime: slot.AgentGrantRuntime{Capabilities: []string{"execute"}, WorkspaceAccess: "read_write"},
	}
}

// TestSlotGrantStoreIT v12 建表 + 迁移登记 + upsert 全量替换 + 往返。
func TestSlotGrantStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v12 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 12`).Scan(&v12)
	if v12 != 1 {
		t.Fatalf("migration v12 未登记: %d", v12)
	}

	if _, ok, _ := store.GrantByAgent(ctx, "ag_it"); ok {
		t.Fatal("未设置档案应 ok=false")
	}
	if err := store.PutGrant(ctx, grantRow("ag_it")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := store.GrantByAgent(ctx, "ag_it")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if len(got.Skills) != 1 || got.Skills[0].Ref != "workspace/skills/evidence-review" ||
		len(got.Tools) != 2 || got.Runtime.WorkspaceAccess != "read_write" {
		t.Fatalf("往返失真: %+v", got)
	}

	// upsert 全量替换：清空 skills、降权限
	replace := slot.AgentGrant{AgentID: "ag_it", Runtime: slot.AgentGrantRuntime{WorkspaceAccess: "none"}}
	if err := store.PutGrant(ctx, replace); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, _ = store.GrantByAgent(ctx, "ag_it")
	if len(got.Skills) != 0 || len(got.Tools) != 0 || got.Runtime.WorkspaceAccess != "none" {
		t.Fatalf("全量替换失真: %+v", got)
	}
}

// TestSlotGrantConcurrentUpsertIT 并发 upsert 同一 Agent：终态恰一条、内容
// 为某一次写入的完整快照（无半写）。
func TestSlotGrantConcurrentUpsertIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g := grantRow("ag_race")
			g.Tools = []string{"documents.read", "documents.write", "tool." + strings.Repeat("a", i+1)}
			if err := store.PutGrant(ctx, g); err != nil {
				t.Errorf("并发 put %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	got, ok, _ := store.GrantByAgent(ctx, "ag_race")
	if !ok {
		t.Fatal("并发后档案丢失")
	}
	var rows int
	store.db.QueryRow(`SELECT COUNT(*) FROM slot_agent_grants WHERE agent_id='ag_race'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("终态行数 = %d, want 1", rows)
	}
	if len(got.Tools) != 3 {
		t.Fatalf("半写快照: %+v", got)
	}
}

// TestSpecByVersionIT 钉版本取内容：精确版本、未知版本 ok=false。
func TestSpecByVersionIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	base := slot.SpecContent{Name: "n", Responsibility: "r", Deliverables: []string{"d"}}
	if _, err := store.CreateSpec(ctx, slot.SlotSpec{SlotSpecID: "slot.it", Version: 1, SchemaVersion: 1, SpecContent: base, CreatedAt: "t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	v2 := base
	v2.Name = "n2"
	if _, err := store.AppendVersion(ctx, "slot.it", v2, "t2"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, ok, err := store.SpecByVersion(ctx, "slot.it", 2)
	if err != nil || !ok || got.Name != "n2" {
		t.Fatalf("v2: ok=%v err=%v name=%s", ok, err, got.Name)
	}
	if _, ok, _ := store.SpecByVersion(ctx, "slot.it", 1); !ok {
		t.Fatal("v1 应存在")
	}
	if _, ok, _ := store.SpecByVersion(ctx, "slot.it", 3); ok {
		t.Fatal("v3 不应存在")
	}
	if _, ok, _ := store.SpecByVersion(ctx, "slot.none", 1); ok {
		t.Fatal("未知谱系不应存在")
	}
}

// TestSlotGrantLegacyUpgradeIT v0.5.4 旧库升级：截 v12 段建库 → 表就位、
// 登记恰一条 v12、重开幂等。
func TestSlotGrantLegacyUpgradeIT(t *testing.T) {
	marker := "-- v12（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v12 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-legacy-v054.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	db.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer upgraded.Close()
	ctx := context.Background()
	if err := upgraded.PutGrant(ctx, grantRow("ag_up")); err != nil {
		t.Fatalf("升级后写授权: %v", err)
	}
	var v12 int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 12`).Scan(&v12)
	if v12 != 1 {
		t.Fatalf("v12 登记数 = %d", v12)
	}
	var tbl int
	upgraded.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='slot_agent_grants'`).Scan(&tbl)
	if tbl != 1 {
		t.Fatal("slot_agent_grants 未建")
	}
	upgraded.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reopened.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 12`).Scan(&v12)
	if v12 != 1 {
		t.Fatalf("重开 v12 登记 %d", v12)
	}
	if g, ok, _ := reopened.GrantByAgent(ctx, "ag_up"); !ok || len(g.Tools) != 2 {
		t.Fatalf("重开后授权丢失: ok=%v", ok)
	}
}

// TestSlotBindingBlockedRetryGrantIT 授权数据驱动的 blocked → retry：v12
// 档案补齐后 RetryBindingPreparation 落 ready；仍缺则回 blocked。
func TestSlotBindingBlockedRetryGrantIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	if err := store.PutGrant(ctx, slot.AgentGrant{AgentID: "ag_g"}); err != nil {
		t.Fatalf("空授权: %v", err)
	}
	b, err := store.CreateBinding(ctx, bindingRow(), slot.BindingBlocked,
		map[string]string{"missing": "tool:documents.read；runtime:execute"})
	if err != nil || b.Status != slot.BindingBlocked {
		t.Fatalf("blocked 建任职: %+v %v", b, err)
	}

	// 仍缺 → retry → 回 blocked（missing 记事件）
	if _, done, err := store.RetryBindingPreparation(ctx, b.BindingID, slot.BindingBlocked,
		map[string]string{"missing": "tool:documents.read"}, "t2"); err != nil || !done {
		t.Fatalf("retry blocked: done=%v err=%v", done, err)
	}
	// 补齐授权 → retry → ready
	if err := store.PutGrant(ctx, grantRow("ag_it")); err != nil {
		t.Fatalf("补授权: %v", err)
	}
	got, done, err := store.RetryBindingPreparation(ctx, b.BindingID, slot.BindingReady, nil, "t3")
	if err != nil || !done || got.Status != slot.BindingReady {
		t.Fatalf("retry: done=%v err=%v status=%s", done, err, got.Status)
	}
	evs, _ := store.BindingEvents(ctx, b.BindingID)
	if len(evs) != 6 { // created + preparing→blocked + blocked→preparing + preparing→blocked + blocked→preparing + preparing→ready
		t.Fatalf("事件 %d 条, want 6: %+v", len(evs), evs)
	}
	// 回 blocked 那次重试的第二事件（序 3：prep→blocked）应含 missing
	if !strings.Contains(string(evs[3].Payload), "tool:documents.read") {
		t.Fatalf("回 blocked 事件应含 missing（%s）", evs[3].Payload)
	}
	// 非 blocked 重试 ok=false
	if _, done, _ := store.RetryBindingPreparation(ctx, b.BindingID, slot.BindingReady, nil, "t4"); done {
		t.Fatal("非 blocked 重试应 ok=false")
	}
	if _, _, err := store.RetryBindingPreparation(ctx, "bd_none", slot.BindingReady, nil, "t"); !errors.Is(err, slot.ErrBindingNotFound) {
		t.Fatalf("未知任职应 ErrBindingNotFound: %v", err)
	}
}
