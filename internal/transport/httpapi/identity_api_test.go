package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// 0.5.0 身份面（ADR-0017）：/v1/identity/agents 两端点 + 可执行项 agent_id 增量
// 字段 + 座位面 /v1/agents 不变的兼容断言。
func identityTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-10-28T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_id" },
		Tenant: "ten_local",
	})
	reg, err := harness.LoadOrCreate(t.TempDir() + "/harness.json")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ident := personalagent.New(personalagent.NewMemStore(), personalagent.NewRegistryAdapter(reg),
		personalagent.WithNewAgentID(func() string { return "ag_fixed_1" }),
		personalagent.WithNow(func() string { return "2026-10-28T12:00:00.000Z" }))
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:       room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Harness:     reg,
		ProbeRunner: miniRunner{},
		Identity:    ident,
		Seats: func() []room.AgentSeat {
			return []room.AgentSeat{{
				ParticipantID: "par_echo",
				Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
			}}
		},
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestIdentityAgentsEndpoints(t *testing.T) {
	ts := identityTestServer(t)

	// 空注册表：空列表而非错误
	resp, err := http.Get(ts.URL + "/v1/identity/agents")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var empty struct {
		Agents []json.RawMessage `json:"agents"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&empty)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(empty.Agents) != 0 {
		t.Fatalf("空表 status=%d agents=%d", resp.StatusCode, len(empty.Agents))
	}

	// 手动登记 codex（miniRunner 探测通过）→ 惰性同步建 Agent
	add, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"codex","runtime":"native","path":"/opt/tools/codex"}`))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	add.Body.Close()
	if add.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", add.StatusCode)
	}

	list, err := http.Get(ts.URL + "/v1/identity/agents")
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	var doc struct {
		Agents []struct {
			AgentID     string `json:"agent_id"`
			Adapter     string `json:"adapter"`
			DisplayName string `json:"display_name"`
			Status      string `json:"status"`
			Source      string `json:"source"`
			Aliases     struct {
				BotID          string   `json:"bot_id"`
				ProfileIDs     []string `json:"profile_ids"`
				ParticipantIDs []string `json:"participant_ids"`
			} `json:"aliases"`
			Executables []struct {
				ExecutableID string `json:"executable_id"`
				Runtime      string `json:"runtime"`
				Channel      string `json:"channel"`
				Enabled      bool   `json:"enabled"`
			} `json:"executables"`
		} `json:"agents"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	if len(doc.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(doc.Agents))
	}
	a := doc.Agents[0]
	if a.AgentID != "ag_fixed_1" || a.Adapter != "codex" || a.Status != "active" || a.Source != "migration" {
		t.Fatalf("agent 字段错: %+v", a)
	}
	if a.Aliases.BotID == "" || len(a.Aliases.ProfileIDs) != 1 || len(a.Aliases.ParticipantIDs) != 1 {
		t.Fatalf("别名集错: %+v", a.Aliases)
	}
	if !strings.HasPrefix(a.Aliases.ParticipantIDs[0], "par_codex_") ||
		!strings.HasPrefix(a.Aliases.ProfileIDs[0], "prof_codex_") {
		t.Fatalf("派生别名前缀错: %+v", a.Aliases)
	}
	if len(a.Executables) != 1 || a.Executables[0].ExecutableID == "" || a.Executables[0].Channel != "cli" {
		t.Fatalf("可执行引用错: %+v", a.Executables)
	}

	// 单查命中与 404
	one, err := http.Get(ts.URL + "/v1/identity/agents/ag_fixed_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	one.Body.Close()
	if one.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", one.StatusCode)
	}
	miss, err := http.Get(ts.URL + "/v1/identity/agents/ag_missing")
	if err != nil {
		t.Fatalf("get404: %v", err)
	}
	var errDoc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(miss.Body).Decode(&errDoc)
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound || errDoc.Error.Code != "identity_not_found" {
		t.Fatalf("404 面: status=%d code=%q", miss.StatusCode, errDoc.Error.Code)
	}

	// 兼容：可执行项列表含增量 agent_id，旧字段（id/bot_id/path）原样
	exes, err := http.Get(ts.URL + "/v1/harness/executables")
	if err != nil {
		t.Fatalf("exes: %v", err)
	}
	var exeDoc struct {
		Executables []struct {
			ID     string `json:"id"`
			BotID  string `json:"bot_id"`
			Path   string `json:"path"`
			AgentI string `json:"agent_id"`
		} `json:"executables"`
	}
	_ = json.NewDecoder(exes.Body).Decode(&exeDoc)
	exes.Body.Close()
	if len(exeDoc.Executables) != 1 {
		t.Fatalf("exes = %d", len(exeDoc.Executables))
	}
	e := exeDoc.Executables[0]
	if e.AgentI != "ag_fixed_1" || e.ID == "" || e.BotID == "" || e.Path == "" {
		t.Fatalf("可执行项新旧字段: %+v", e)
	}

	// 兼容：座位面 /v1/agents 无 agent_id 键（旧 API 输出保持旧形态）
	seats, err := http.Get(ts.URL + "/v1/agents")
	if err != nil {
		t.Fatalf("seats: %v", err)
	}
	raw, err := io.ReadAll(seats.Body)
	seats.Body.Close()
	if err != nil {
		t.Fatalf("read seats: %v", err)
	}
	if strings.Contains(string(raw), "agent_id") {
		t.Fatalf("座位面不应出现 agent_id: %s", raw)
	}
	var seatDoc struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(raw, &seatDoc); err != nil || len(seatDoc.Agents) != 1 {
		t.Fatalf("座位面解析失败或席位数错: %v %s", err, raw)
	}
	if _, leaked := seatDoc.Agents[0]["agent_id"]; leaked {
		t.Fatal("座位项泄漏 agent_id")
	}
}

// TestManualRegisterThenExecutablesHasAgentID（审校 P2#2）：真实用户流程——
// 手动登记后设置页只刷新可执行列表（不先调身份 API），徽标所需的 agent_id
// 必须已经同步回写。既有测试先 GET 身份列表触发了惰性同步，掩盖了本缺口。
func TestManualRegisterThenExecutablesHasAgentID(t *testing.T) {
	ts := identityTestServer(t)

	add, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"kimi","runtime":"native","path":"/opt/tools/kimi"}`))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	add.Body.Close()
	if add.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", add.StatusCode)
	}

	// 唯一读取面 = 可执行列表（与设置页 refreshExecutables 同一调用）
	list, err := http.Get(ts.URL + "/v1/harness/executables")
	if err != nil {
		t.Fatalf("exes: %v", err)
	}
	var doc struct {
		Executables []struct {
			ID      string `json:"id"`
			AgentID string `json:"agent_id"`
		} `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	found := false
	for _, e := range doc.Executables {
		if e.ID != "" && e.AgentID == "" {
			t.Fatalf("可执行项 %s 缺 agent_id（手动登记后列表读取必须已同步）", e.ID)
		}
		if strings.Contains(e.ID, "kimi") && e.AgentID != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("kimi 手动登记项未出现或未带 agent_id: %+v", doc.Executables)
	}
}
