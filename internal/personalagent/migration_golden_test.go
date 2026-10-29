package personalagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/harness"
)

// P0 门槛件（RFC-0018 §13.1 / ADR-0017）：迁移 fixture 可重复且旧行为不变。
// fixture 覆盖：多 adapter、多实例（双 codex 独立身份）、缺 bot_id 祖父化
// （kimi）、bot_id ≠ id 的历史重绑（zcode）、同 bot_id 双可执行项归一（claude）。
// 确定性：注入序号 AgentID + 固定时钟；注册表 List() 规范序（adapter→priority→path）
// → 期望值可以逐字断言。
func TestMigrationFixtureGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "registry-migration.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	tmp := filepath.Join(t.TempDir(), "harness-registry.json")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	reg, err := harness.LoadOrCreate(tmp) // 装载即祖父化：kimi 补 bot_id = id
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	store := NewMemStore()
	svc := New(store, NewRegistryAdapter(reg), WithNewAgentID(seqID()), WithNow(fixedNow))
	rep, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Created != 5 {
		t.Fatalf("created = %d, want 5（6 可执行项、claude 归一）", rep.Created)
	}
	// 别名数：5 Agent × 3 = 15（claude 双 exe 的派生别名同值幂等）
	if rep.AliasesAdded != 15 {
		t.Fatalf("aliases added = %d, want 15", rep.AliasesAdded)
	}

	// 规范序（adapter 升序）：claude → codex(/usr/local 先于 C:/) → kimi → zcode
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 5 {
		t.Fatalf("views = %d, want 5", len(views))
	}
	want := []struct {
		agentID, adapter, botID string
		exeCount                int
	}{
		{"ag_test_1", "claude", "claude-b", 2},
		{"ag_test_2", "codex", "codex@native:/usr/local/bin/codex", 1},
		{"ag_test_3", "codex", "codex@native:C:/Users/u/AppData/Roaming/npm/codex.cmd", 1},
		{"ag_test_4", "kimi", "kimi@wsl:Ubuntu:/home/u/.nvm/versions/node/v22.20.0/bin/kimi", 1},
		{"ag_test_5", "zcode", "zcode@native:C:/Users/u/AppData/Local/Programs/ZCode/ZCode.exe", 1},
	}
	for i, w := range want {
		v := views[i]
		if v.AgentID != w.agentID || v.Adapter != w.adapter || v.Aliases.BotID != w.botID || len(v.Executables) != w.exeCount {
			t.Errorf("views[%d] = {id:%s adapter:%s bot:%s exes:%d}, want {%s %s %s %d}",
				i, v.AgentID, v.Adapter, v.Aliases.BotID, len(v.Executables),
				w.agentID, w.adapter, w.botID, w.exeCount)
		}
	}

	// 回写：每个可执行项得 agent_id；旧字段（bot_id/id）原样——旧行为不变
	exes := reg.List()
	if len(exes) != 6 {
		t.Fatalf("exes = %d, want 6", len(exes))
	}
	agentByBot := map[string]string{}
	for _, v := range views {
		agentByBot[v.Aliases.BotID] = v.AgentID
	}
	for _, exe := range exes {
		if exe.AgentID == "" {
			t.Fatalf("exe %s 未回写 agent_id", exe.ID)
		}
		if want := agentByBot[exe.BotIDOf()]; exe.AgentID != want {
			t.Fatalf("exe %s agent_id = %s, want %s", exe.ID, exe.AgentID, want)
		}
	}
	// 规范序：claude(/home) 、claude(C:/)、codex(/usr/local)、codex(npm)、kimi、zcode
	if exes[4].BotIDOf() != exes[4].ID {
		t.Fatalf("kimi 祖父化未生效: bot_id = %q", exes[4].BotIDOf())
	}
	if exes[5].BotIDOf() == exes[5].ID {
		t.Fatal("zcode 历史 bot_id 被改写（应保留重绑身份）")
	}

	// 重跑零效果 + 视图逐字稳定（确定性 golden）
	rep2, err := svc.SyncFromHarness(context.Background())
	if err != nil {
		t.Fatalf("sync2: %v", err)
	}
	if rep2.Created != 0 || rep2.AliasesAdded != 0 || len(rep2.Conflicts) != 0 {
		t.Fatalf("重跑应零效果，得 %+v", rep2)
	}
	views2, _ := svc.List(context.Background())
	b1, _ := json.Marshal(views)
	b2, _ := json.Marshal(views2)
	if string(b1) != string(b2) {
		t.Fatalf("两次同步视图不一致:\n%s\n%s", b1, b2)
	}

	// 磁盘侧：注册表 JSON 含 agent_id（增量字段），旧字段不丢
	persisted, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	if err := json.Unmarshal(persisted, &doc); err != nil {
		t.Fatalf("parse persisted: %v", err)
	}
	if len(doc.Executables) != 6 {
		t.Fatalf("persisted exes = %d", len(doc.Executables))
	}
	for _, exe := range doc.Executables {
		if exe.AgentID == "" || exe.BotID == "" || exe.ID == "" || exe.Path == "" {
			t.Fatalf("persisted exe 旧字段/新字段缺失: %+v", exe)
		}
	}
}
