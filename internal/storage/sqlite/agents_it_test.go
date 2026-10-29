//go:build it

// IT 层（ADR-0017 / 0.5.0）：agents / agent_aliases 两表经真实 SQLite 往返——
// migration v7 建表、CRUD、别名唯一约束与冲突语义。
package sqlite

import (
	"context"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/personalagent"
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
