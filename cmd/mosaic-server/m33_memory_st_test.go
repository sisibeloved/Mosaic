//go:build st

// ST 层（M3-3 记忆系统）：真实二进制 + 真实 SQLite（FTS5 trigram）——
// (1) GET /v1/rooms/{id}/search 全文检索（CJK ≥3 字 trigram、短词 LIKE 回退）；
// (2) 快照 tasks 字段在位（空态 [] 非 null）+ resolve_task 对不存在任务 400；
// (3) GET /v1/rooms/{id}/memory 空态胶囊与容量水位可见。
// tasklist 派生链（宣言→pending→裁定）由 room 包 UT 钉死（TestTask*），
// 引擎注入面由 TestAssembleChatMemoryFaces 钉死——ST 覆盖传输与存储真实形态。
package main_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/room"
)

func TestMemorySearch_ST(t *testing.T) {
	bin := buildServer(t)
	cmd, base, _ := startLoggedServer(t, bin, t.TempDir(), "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9801", "issued_at": "2026-09-03T09:00:00.000Z",
		"payload": map[string]any{"display_name": "memory st"},
	})
	roomID := created["room_id"].(string)

	version := int64(1)
	post := func(idem, body string) {
		res := postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
			"command_kind": "post_message", "expected_room_version": version,
			"idempotency_key": idem, "issued_at": "2026-09-03T09:00:01.000Z",
			"payload": map[string]any{"body": body, "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
		})
		version = int64(res["room_version"].(float64))
	}
	post("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9802", "预算超限的问题需要先排查迁移脚本")
	post("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9803", "去查一下预算的口径")

	client := &http.Client{Timeout: 5 * time.Second}

	// FTS5 trigram：CJK ≥3 字
	hits := searchST(t, client, base, roomID, "预算超限")
	if len(hits) != 1 || hits[0].Actor != "par_owner" {
		t.Fatalf("trigram 命中 = %+v", hits)
	}
	// 短词 LIKE 回退
	hits = searchST(t, client, base, roomID, "预算")
	if len(hits) != 2 {
		t.Fatalf("LIKE 回退命中 = %d（期望 2）", len(hits))
	}
	// 无命中
	if hits := searchST(t, client, base, roomID, "完全不存在的词组"); len(hits) != 0 {
		t.Fatalf("无命中应为空 = %+v", hits)
	}
	// 参数校验：空 q 400
	resp, err := client.Get(base + "/v1/rooms/" + roomID + "/search?q=")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 q 应 400，得到 %d", resp.StatusCode)
	}
}

func TestMemoryViewAndTaskGate_ST(t *testing.T) {
	bin := buildServer(t)
	cmd, base, _ := startLoggedServer(t, bin, t.TempDir(), "-dev")
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	created := postJSONST(t, base, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9901", "issued_at": "2026-09-03T09:10:00.000Z",
		"payload": map[string]any{"display_name": "task st"},
	})
	roomID := created["room_id"].(string)
	client := &http.Client{Timeout: 5 * time.Second}

	// 人类消息 → 波收敛（echo 发言 + 礼貌静默）
	postJSONST(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9902", "issued_at": "2026-09-03T09:10:01.000Z",
		"payload": map[string]any{"body": "谁来拉一下数据", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if closedCountST(t, client, base, roomID) >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)

	// 快照 tasks：字段在位（空态为 []，非 null——JSON 契约稳定）
	snap := snapshotST(t, client, base, roomID)
	if snap.Tasks == nil {
		t.Fatal("快照应携带 tasks 数组（空态为 []，非 null）")
	}

	// resolve_task 对不存在任务 → 400（版本预检先行，须先校准真实版本）
	snapNow := snapshotST(t, client, base, roomID)
	if code := postJSONSTErr(t, base, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "resolve_task", "expected_room_version": snapNow.RoomVersion,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9903", "issued_at": "2026-09-03T09:10:02.000Z",
		"payload": map[string]any{"task_id": "tsk_nope0000000", "resolution": "delivered"},
	}); code != http.StatusBadRequest {
		t.Fatalf("不存在任务应 400，得到 %d", code)
	}

	// 记忆端点：空态（无胶囊）——capsules 数组在位、容量水位可见
	mem := memoryST(t, client, base, roomID)
	capsules, _ := mem["capsules"].([]any)
	if len(capsules) != 0 {
		t.Fatalf("空房胶囊应为空：%v", capsules)
	}
	budget, _ := mem["capsule_budget"].(map[string]any)
	if budget == nil || budget["budget_runes"].(float64) != float64(room.CapsuleBudgetRunes) {
		t.Fatalf("容量水位应可见：%v", budget)
	}
}

// ---- ST 辅助 ----

func searchST(t *testing.T, client *http.Client, base, roomID, q string) []room.SearchHit {
	t.Helper()
	resp, err := client.Get(base + "/v1/rooms/" + roomID + "/search?q=" + q)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search %q 状态 %d", q, resp.StatusCode)
	}
	var doc struct {
		Hits []room.SearchHit `json:"hits"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return doc.Hits
}

func snapshotST(t *testing.T, client *http.Client, base, roomID string) room.Snapshot {
	t.Helper()
	resp, err := client.Get(base + "/v1/rooms/" + roomID + "/snapshot")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer resp.Body.Close()
	var snap room.Snapshot
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	return snap
}

func memoryST(t *testing.T, client *http.Client, base, roomID string) map[string]any {
	t.Helper()
	resp, err := client.Get(base + "/v1/rooms/" + roomID + "/memory")
	if err != nil {
		t.Fatalf("memory: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("memory 状态 %d", resp.StatusCode)
	}
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return doc
}

// postJSONSTErr：POST 命令但只关心状态码（期望非 200 的负路径断言）。
func postJSONSTErr(t *testing.T, base, path string, body map[string]any) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Owner-Token", ownerTokenST(t, base))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
