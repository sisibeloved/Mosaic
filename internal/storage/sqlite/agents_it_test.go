//go:build it

// IT 层（ADR-0017 / 0.5.0）：agents / agent_aliases 两表经真实 SQLite 往返——
// migration v7 建表、CRUD、别名唯一约束与冲突语义。
package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/personalagent"

	_ "modernc.org/sqlite"
)

func TestAgentIdentityStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()

	// migration v7 已应用
	var v7 int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 7`).Scan(&v7); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if v7 != 1 {
		t.Fatalf("migration v7 未登记: %d", v7)
	}

	// EnsureAgent：新建与幂等
	a := personalagent.Agent{
		AgentID: "ag_it_1", DisplayName: "Codex", Adapter: "codex",
		Status: "active", Source: "migration", CreatedAt: "2026-10-28T10:00:00.000Z",
	}
	created, err := store.EnsureAgent(ctx, a)
	if err != nil || !created {
		t.Fatalf("ensure 新建: %v %v", created, err)
	}
	created, err = store.EnsureAgent(ctx, a)
	if err != nil || created {
		t.Fatalf("ensure 幂等应 created=false: %v %v", created, err)
	}

	// 别名：登记 → 解析 → 幂等 → 冲突拒绝
	if _, err := store.AddAlias(ctx, personalagent.AliasBotID, "b1", "ag_it_1"); err != nil {
		t.Fatalf("add alias: %v", err)
	}
	if _, err := store.AddAlias(ctx, personalagent.AliasBotID, "b1", "ag_it_1"); err != nil {
		t.Fatalf("同目标幂等: %v", err)
	}
	if got, ok, err := store.AgentByAlias(ctx, personalagent.AliasBotID, "b1"); err != nil || !ok || got != "ag_it_1" {
		t.Fatalf("解析: %q %v %v", got, ok, err)
	}
	store.EnsureAgent(ctx, personalagent.Agent{AgentID: "ag_it_2", DisplayName: "Kimi", Adapter: "kimi", Status: "active", Source: "migration", CreatedAt: "2026-10-28T10:00:01.000Z"})
	if _, err := store.AddAlias(ctx, personalagent.AliasBotID, "b1", "ag_it_2"); err != personalagent.ErrAliasConflict {
		t.Fatalf("异目标应 ErrAliasConflict, got %v", err)
	}

	// 列表按插入序（创建序）+ 按 Agent 取别名
	agents, err := store.ListAgents(ctx)
	if err != nil || len(agents) != 2 || agents[0].AgentID != "ag_it_1" {
		t.Fatalf("list: %v %+v", err, agents)
	}
	aliases, err := store.AliasesOf(ctx, "ag_it_1")
	if err != nil || len(aliases) != 1 || aliases[0].Kind != personalagent.AliasBotID || aliases[0].Value != "b1" {
		t.Fatalf("aliases of: %v %+v", err, aliases)
	}
}

// TestAgentIdentityOldDatabaseUpgradesIT 旧库（v6 时期文件）开库即补 v7 表——
// CREATE TABLE IF NOT EXISTS 幂等路径，身份面无需停机脚本。
func TestAgentIdentityOldDatabaseUpgradesIT(t *testing.T) {
	store, path := openTempStore(t)
	store.Close()

	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	ctx := context.Background()
	created, err := store2.EnsureAgent(ctx, personalagent.Agent{
		AgentID: "ag_it_reopen", DisplayName: "ZCode", Adapter: "zcode",
		Status: "active", Source: "migration", CreatedAt: "2026-10-28T10:00:02.000Z",
	})
	if err != nil || !created {
		t.Fatalf("reopen 后身份写入: %v %v", created, err)
	}
}

// TestEnsureAgentWithAliasAtomicIT（审校 P1#1）：建行与 bot_id 认领同事务——
// 多路并发以不同候选 ID 认领同一别名，恰一行 Agent + 一行别名，所有调用方
// 拿到同一胜者；库内不存在未被别名认领的孤立 Agent 行。
func TestEnsureAgentWithAliasAtomicIT(t *testing.T) {
	store, _ := openTempStore(t)
	ctx := context.Background()

	const n = 16
	owners := make([]string, n)
	created := make([]bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := personalagent.Agent{
				AgentID:     "ag_atomic_" + strconv.Itoa(i),
				DisplayName: "Codex", Adapter: "codex",
				Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:00.000Z",
			}
			owner, createdOne, err := store.EnsureAgentWithAlias(ctx, a, personalagent.AliasBotID, "b_atomic")
			if err != nil {
				t.Errorf("并发认领: %v", err)
				return
			}
			owners[i], created[i] = owner, createdOne
		}(i)
	}
	wg.Wait()

	createdCount := 0
	for _, c := range created {
		if c {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created 恰一路为真（胜者），得 %d", createdCount)
	}
	winner := owners[0]
	for i, o := range owners {
		if o != winner {
			t.Fatalf("各方应同指胜者 %s，第 %d 路得 %s", winner, i, o)
		}
	}
	var agentRows, aliasRows, orphans int
	store.db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&agentRows)
	store.db.QueryRow(`SELECT COUNT(*) FROM agent_aliases`).Scan(&aliasRows)
	store.db.QueryRow(`SELECT COUNT(*) FROM agents a WHERE NOT EXISTS
		(SELECT 1 FROM agent_aliases al WHERE al.agent_id = a.agent_id)`).Scan(&orphans)
	if agentRows != 1 || aliasRows != 1 || orphans != 0 {
		t.Fatalf("行数错：agents=%d aliases=%d orphans=%d（要 1/1/0）", agentRows, aliasRows, orphans)
	}
}

// TestAgentIdentityLegacyUpgradeIT（审校 P2#4）：真实 v0.4.1 表结构（schemaDDL
// 去 v7 段 + 迁移登记 1–5）建库并写旧记录；Open 升级后身份表可用、旧数据在、
// 迁移登记恰好一条 v7；重开幂等。
func TestAgentIdentityLegacyUpgradeIT(t *testing.T) {
	// v0.4.1 的 schemaDDL = 当前 DDL 截去 v7 段（v7 之前无其它差异；v6 表格
	// 交换在 v0.4.1 已在席且本就不登记 migrations 行）
	marker := "-- v7（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v7 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-legacy.db")
	db, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(filepath.ToSlash(path), "?", "%3F")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	// 旧记录：一条房间事件 + 一条 outbox 积压
	oldEvt := `{"event_id":"evt_legacy_1","tenant_id":"ten_local","room_id":"room_legacy","seq":1,"type":"message.posted","schema_version":1,"occurred_at":"2026-10-01T00:00:00.000Z","actor":{"participant_id":"par_owner","kind":"human"},"visibility":{"kind":"public"},"payload":{"body":"legacy"},"metadata":{}}`
	if _, err := db.Exec(`INSERT INTO room_events (event_id, tenant_id, room_id, seq, type, schema_version, occurred_at, envelope)
		VALUES ('evt_legacy_1','ten_local','room_legacy',1,'message.posted',1,'2026-10-01T00:00:00.000Z',?)`, oldEvt); err != nil {
		db.Close()
		t.Fatalf("legacy event: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO outbox (room_id, global_pos, event_id, envelope) VALUES ('room_legacy',1,'evt_legacy_1',?)`, oldEvt); err != nil {
		db.Close()
		t.Fatalf("legacy outbox: %v", err)
	}
	db.Close()

	// 升级前断言：无身份表、无 v7 登记
	probe, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(filepath.ToSlash(path), "?", "%3F"))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	var hasAgents int
	probe.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('agents','agent_aliases')`).Scan(&hasAgents)
	if hasAgents != 0 {
		probe.Close()
		t.Fatalf("升级前不应有身份表，得 %d", hasAgents)
	}
	probe.Close()

	// 升级路径：当前 Open
	store, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	ctx := context.Background()
	if _, _, err := store.EnsureAgentWithAlias(ctx, personalagent.Agent{
		AgentID: "ag_legacy_1", DisplayName: "Codex", Adapter: "codex",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:00.000Z",
	}, personalagent.AliasBotID, "b_legacy"); err != nil {
		t.Fatalf("升级后身份写入: %v", err)
	}
	// 旧数据保留
	var body string
	if err := store.db.QueryRow(`SELECT envelope FROM room_events WHERE event_id='evt_legacy_1'`).Scan(&body); err != nil || body == "" {
		t.Fatalf("旧事件丢失: %v", err)
	}
	var outboxPending int
	store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE dispatched_at IS NULL`).Scan(&outboxPending)
	if outboxPending != 1 {
		t.Fatalf("旧 outbox 积压应保留 1 条，得 %d", outboxPending)
	}
	// 迁移登记：1–5、7–9 各恰一条（v8/v9 事件流与职责表随 0.5.1/0.5.2 加入）
	rows, err := store.db.Query(`SELECT version, COUNT(*) FROM migrations GROUP BY version ORDER BY version`)
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	got := map[int]int{}
	for rows.Next() {
		var v, c int
		rows.Scan(&v, &c)
		got[v] = c
	}
	rows.Close()
	for _, v := range []int{1, 2, 3, 4, 5, 7, 8, 9, 10, 11, 12, 13, 14} {
		if got[v] != 1 {
			t.Fatalf("迁移 version=%d 登记数 %d（要 1）；全量 %v", v, got[v], got)
		}
	}
	if len(got) != 13 {
		t.Fatalf("迁移登记集合异常: %v", got)
	}
	store.Close()

	// 重开幂等
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	var v7count int
	store2.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 7`).Scan(&v7count)
	if v7count != 1 {
		t.Fatalf("重开后 v7 登记 %d（要恰 1）", v7count)
	}
	if owner, ok, _ := store2.AgentByAlias(ctx, personalagent.AliasBotID, "b_legacy"); !ok || owner != "ag_legacy_1" {
		t.Fatalf("重开后别名解析断裂: %v %s", ok, owner)
	}
}
