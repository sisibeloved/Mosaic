//go:build it

// IT 层（ADR-0018 / 0.5.1）：agent_events 事件流经真实 SQLite——migration v8
// 建表、v7 存量回填（含重开幂等）、转移事务（投影 + 事件一致）与并发恰一胜者。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/personalagent"
)

// TestAgentLifecycleMigrationIT 新库：v8 表与迁移登记齐备。
func TestAgentLifecycleMigrationIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()

	var hasEvents int
	store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('agent_events','idx_agent_events_agent')`).Scan(&hasEvents)
	if hasEvents != 2 {
		t.Fatalf("v8 表/索引未建: %d", hasEvents)
	}
	var v8 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 8`).Scan(&v8)
	if v8 != 1 {
		t.Fatalf("migration v8 未登记: %d", v8)
	}
}

// TestAgentEventsBackfillIT v0.5.0 存量库（v7 表、无事件流）升级：每个已有
// Agent 行按创建序补恰一条 created 事件（payload 对齐行值、occurred_at 取
// created_at）；重开不重复回填。
func TestAgentEventsBackfillIT(t *testing.T) {
	// v0.5.0 的 schemaDDL = 当前 DDL 截去 v8 段
	marker := "-- v8（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v8 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]

	path := filepath.Join(t.TempDir(), "mosaic-v050.db")
	db, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(filepath.ToSlash(path), "?", "%3F")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	// 0.5.0 存量：3 行 Agent（含一行已人工置 suspended 的形态——v7 期 status
	// 虽恒 active，但列可写；回填不得依赖 status 值）
	for i := 1; i <= 3; i++ {
		if _, err := db.Exec(`INSERT INTO agents (agent_id, display_name, adapter, status, source, created_at)
			VALUES (?,?,?,?,?,?)`,
			"ag_v050_"+strconv.Itoa(i), "Codex"+strconv.Itoa(i), "codex", "active", "migration",
			"2026-10-2"+strconv.Itoa(7+i)+"T10:00:00.000Z"); err != nil {
			db.Close()
			t.Fatalf("legacy agent #%d: %v", i, err)
		}
	}
	db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer store.Close()

	var evCount int
	store.db.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&evCount)
	if evCount != 3 {
		t.Fatalf("回填事件 %d 条, want 3", evCount)
	}
	// 升级前无事件流——回填必为 created 且按 rowid（创建序）
	rows, err := store.db.Query(`SELECT agent_id, event_type, payload, occurred_at FROM agent_events ORDER BY seq`)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		i++
		var id, typ, payload, at string
		rows.Scan(&id, &typ, &payload, &at)
		if id != "ag_v050_"+strconv.Itoa(i) {
			t.Fatalf("回填序错：第 %d 条 = %s", i, id)
		}
		if typ != "created" {
			t.Fatalf("回填类型 = %s, want created", typ)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if p["source"] != "migration" || p["adapter"] != "codex" || p["display_name"] != "Codex"+strconv.Itoa(i) {
			t.Fatalf("payload 对齐失败: %v", p)
		}
		if at != "2026-10-2"+strconv.Itoa(7+i)+"T10:00:00.000Z" {
			t.Fatalf("occurred_at 应取行内 created_at: %s", at)
		}
	}

	// 重开幂等：不重复回填
	store.Close()
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	store2.db.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE event_type='created'`).Scan(&evCount)
	if evCount != 3 {
		t.Fatalf("重开后 created 事件 %d 条, want 3（幂等）", evCount)
	}
}

// TestTransitionAgentIT 转移事务：投影更新与 status_changed 事件同落；
// 非法转移不动投影不落事件；未知身份 ErrAgentNotFound。
func TestTransitionAgentIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	_, created, err := store.EnsureAgentWithAlias(ctx, personalagent.Agent{
		AgentID: "ag_lc_1", DisplayName: "Codex", Adapter: "codex",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:00.000Z",
	}, personalagent.AliasBotID, "b_lc")
	if err != nil || !created {
		t.Fatalf("建行: %v %v", created, err)
	}

	from, ok, err := store.TransitionAgent(ctx, "ag_lc_1",
		[]string{personalagent.StatusActive}, personalagent.StatusSuspended, "休假", "2026-10-29T11:00:00.000Z")
	if err != nil || !ok || from != personalagent.StatusActive {
		t.Fatalf("转移: from=%s ok=%v err=%v", from, ok, err)
	}
	a, found, _ := store.AgentByID(ctx, "ag_lc_1")
	if !found || a.Status != personalagent.StatusSuspended {
		t.Fatalf("投影未更新: %+v", a)
	}
	evs, err := store.AgentEvents(ctx, "ag_lc_1")
	if err != nil || len(evs) != 2 {
		t.Fatalf("事件数: %d err=%v（建行 created + 转移）", len(evs), err)
	}
	var p struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(evs[1].Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.From != "active" || p.To != "suspended" || p.Reason != "休假" {
		t.Fatalf("转移 payload = %+v", p)
	}

	// 非法转移：suspended 不在 allowedFrom=[active]
	if _, ok, err := store.TransitionAgent(ctx, "ag_lc_1",
		[]string{personalagent.StatusActive}, personalagent.StatusSuspended, "", "t"); err != nil || ok {
		t.Fatalf("非法转移应 ok=false: ok=%v err=%v", ok, err)
	}
	a, _, _ = store.AgentByID(ctx, "ag_lc_1")
	if a.Status != personalagent.StatusSuspended {
		t.Fatalf("非法转移动了投影: %s", a.Status)
	}
	evs, _ = store.AgentEvents(ctx, "ag_lc_1")
	if len(evs) != 2 {
		t.Fatalf("非法转移落了事件: %d", len(evs))
	}

	// 未知身份
	if _, _, err := store.TransitionAgent(ctx, "ag_missing",
		[]string{personalagent.StatusActive}, personalagent.StatusSuspended, "", "t"); !errors.Is(err, personalagent.ErrAgentNotFound) {
		t.Fatalf("未知身份 err=%v, want ErrAgentNotFound", err)
	}
}

// TestTransitionAgentConcurrentIT 并发转移：8 路同域（active→suspended）恰一
// 胜者、恰一条 status_changed 事件、终态一致。
func TestTransitionAgentConcurrentIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	store.EnsureAgent(ctx, personalagent.Agent{
		AgentID: "ag_lc_race", DisplayName: "Kimi", Adapter: "kimi",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:00.000Z",
	})

	var wg sync.WaitGroup
	wins := 0
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := store.TransitionAgent(ctx, "ag_lc_race",
				[]string{personalagent.StatusActive}, personalagent.StatusSuspended, "", "2026-10-29T11:00:00.000Z")
			if err != nil {
				t.Errorf("并发转移: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("胜者 %d 个, want 1", wins)
	}
	var changed int
	store.db.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE agent_id='ag_lc_race' AND event_type='status_changed'`).Scan(&changed)
	if changed != 1 {
		t.Fatalf("status_changed 事件 %d 条, want 1", changed)
	}
	a, _, _ := store.AgentByID(ctx, "ag_lc_race")
	if a.Status != personalagent.StatusSuspended {
		t.Fatalf("终态 = %s, want suspended", a.Status)
	}
}

// TestEnsureAgentAppendsCreatedEventIT 建行与 created 事件同事务：EnsureAgent /
// EnsureAgentWithAlias 新建即落恰一条 created；幂等重放不增。
func TestEnsureAgentAppendsCreatedEventIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	a := personalagent.Agent{
		AgentID: "ag_ev_1", DisplayName: "ZCode", Adapter: "zcode",
		Status: "active", Source: "manual", CreatedAt: "2026-10-29T10:00:00.000Z",
	}
	if created, err := store.EnsureAgent(ctx, a); err != nil || !created {
		t.Fatalf("ensure: %v %v", created, err)
	}
	if created, err := store.EnsureAgent(ctx, a); err != nil || created {
		t.Fatalf("ensure 幂等: %v %v", created, err)
	}
	owner, created, err := store.EnsureAgentWithAlias(ctx, personalagent.Agent{
		AgentID: "ag_ev_2", DisplayName: "Codex", Adapter: "codex",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:01.000Z",
	}, personalagent.AliasBotID, "b_ev")
	if err != nil || !created || owner != "ag_ev_2" {
		t.Fatalf("认领建行: %s %v %v", owner, created, err)
	}
	// 认领竞争失败者：不建行、不落事件
	if owner, created, err := store.EnsureAgentWithAlias(ctx, personalagent.Agent{
		AgentID: "ag_ev_3", DisplayName: "Codex", Adapter: "codex",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:02.000Z",
	}, personalagent.AliasBotID, "b_ev"); err != nil || created || owner != "ag_ev_2" {
		t.Fatalf("竞争失败者应回读胜者: %s %v %v", owner, created, err)
	}

	for _, id := range []string{"ag_ev_1", "ag_ev_2", "ag_ev_3"} {
		var n int
		want := 1
		if id == "ag_ev_3" {
			want = 0 // 未建行
		}
		store.db.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE agent_id=?`, id).Scan(&n)
		if n != want {
			t.Fatalf("%s created 事件 %d 条, want %d", id, n, want)
		}
		var rows int
		store.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE agent_id=?`, id).Scan(&rows)
		if rows != want {
			t.Fatalf("%s agents 行 %d, want %d", id, rows, want)
		}
	}
}

// TestRetireAgentAtomicIT（审校 P2#4）：两段事件与终态同一事务——happy 路径
// 恰两条 status_changed；预取消上下文整体回滚（不滞留 retiring、不留事件）；
// 来源不匹配 ok=false 不落任何行。
func TestRetireAgentAtomicIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	store.EnsureAgent(ctx, personalagent.Agent{
		AgentID: "ag_ret_1", DisplayName: "Codex", Adapter: "codex",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:00.000Z",
	})

	// 取消上下文：整体回滚
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.RetireAgent(canceled, "ag_ret_1",
		[]string{personalagent.StatusActive, personalagent.StatusSuspended}, "", "t"); err == nil {
		t.Fatal("取消上下文应返回错误")
	}
	a, _, _ := store.AgentByID(ctx, "ag_ret_1")
	if a.Status != personalagent.StatusActive {
		t.Fatalf("取消回滚后 status=%s, want active（不留 retiring）", a.Status)
	}
	evs, _ := store.AgentEvents(ctx, "ag_ret_1")
	if len(evs) != 1 { // 仅 created
		t.Fatalf("取消回滚后事件 %d 条, want 1", len(evs))
	}

	// happy：active → retired，恰 created + retiring + retired
	from, ok, err := store.RetireAgent(ctx, "ag_ret_1",
		[]string{personalagent.StatusActive, personalagent.StatusSuspended}, "清理", "2026-10-29T11:00:00.000Z")
	if err != nil || !ok || from != personalagent.StatusActive {
		t.Fatalf("retire: from=%s ok=%v err=%v", from, ok, err)
	}
	a, _, _ = store.AgentByID(ctx, "ag_ret_1")
	if a.Status != personalagent.StatusRetired {
		t.Fatalf("终态 = %s, want retired", a.Status)
	}
	evs, _ = store.AgentEvents(ctx, "ag_ret_1")
	if len(evs) != 3 {
		t.Fatalf("事件 %d 条, want 3（created+retiring+retired）", len(evs))
	}
	var p1, p2 struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	json.Unmarshal(evs[1].Payload, &p1)
	json.Unmarshal(evs[2].Payload, &p2)
	if p1.From != "active" || p1.To != "retiring" || p2.From != "retiring" || p2.To != "retired" {
		t.Fatalf("两段事件语义错: %+v %+v", p1, p2)
	}

	// 来源不匹配（retired 不在 allowedFrom）：ok=false 不落事件
	if _, ok, err := store.RetireAgent(ctx, "ag_ret_1",
		[]string{personalagent.StatusActive}, "", "t"); err != nil || ok {
		t.Fatalf("终态再 retire 应 ok=false: ok=%v err=%v", ok, err)
	}
	evs, _ = store.AgentEvents(ctx, "ag_ret_1")
	if len(evs) != 3 {
		t.Fatalf("非法 retire 落了事件: %d", len(evs))
	}
}

// TestIdentityConcurrentMixedWritesIT（审校 P1 靶向）：并发转移/原子退役/别名
// 写混合施压同库——任何非语义错误（SQLITE_BUSY 外泄等）即失败；各 Agent 恰一
// 胜者、事件数恰定值。旧实现（deferred 事务读后升级）重复运行可复现 BUSY 500。
func TestIdentityConcurrentMixedWritesIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()
	store.EnsureAgent(ctx, personalagent.Agent{
		AgentID: "ag_mix_1", DisplayName: "Codex", Adapter: "codex",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:00.000Z",
	})
	store.EnsureAgent(ctx, personalagent.Agent{
		AgentID: "ag_mix_2", DisplayName: "Kimi", Adapter: "kimi",
		Status: "active", Source: "migration", CreatedAt: "2026-10-29T10:00:01.000Z",
	})

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(3)
		go func() { // 转移 ag_mix_1：active → suspended
			defer wg.Done()
			if _, _, err := store.TransitionAgent(ctx, "ag_mix_1",
				[]string{personalagent.StatusActive}, personalagent.StatusSuspended, "", "t"); err != nil {
				t.Errorf("并发转移: %v", err)
			}
		}()
		go func() { // 原子退役 ag_mix_2
			defer wg.Done()
			if _, _, err := store.RetireAgent(ctx, "ag_mix_2",
				[]string{personalagent.StatusActive, personalagent.StatusSuspended}, "", "t"); err != nil {
				t.Errorf("并发退役: %v", err)
			}
		}()
		go func(i int) { // 同库别名写（附加写竞争面）
			defer wg.Done()
			if _, err := store.AddAlias(ctx, personalagent.AliasProfileID,
				"prof_mix_"+strconv.Itoa(i), "ag_mix_1"); err != nil {
				t.Errorf("并发别名: %v", err)
			}
		}(i)
	}
	wg.Wait()

	for _, tc := range []struct {
		id, want string
		changed  int
	}{
		{"ag_mix_1", personalagent.StatusSuspended, 1},
		{"ag_mix_2", personalagent.StatusRetired, 2},
	} {
		a, ok, _ := store.AgentByID(ctx, tc.id)
		if !ok || a.Status != tc.want {
			t.Fatalf("%s 终态 = %s, want %s", tc.id, a.Status, tc.want)
		}
		var changed int
		store.db.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE agent_id=? AND event_type='status_changed'`, tc.id).Scan(&changed)
		if changed != tc.changed {
			t.Fatalf("%s status_changed 事件 %d 条, want %d", tc.id, changed, tc.changed)
		}
	}
}
