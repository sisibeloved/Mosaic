// 分域记忆端点测试（0.7.0 / ADR-0037）：写门/域校验/锚门/隔离列表/单查/
// 未装配面。（区别于 memory_api_test.go 的 v1.70 房间记忆策展端点测试。）
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/memory"
	"github.com/sisibeloved/Mosaic/internal/room"
)

func fixedMemNow() string { return "2026-11-04T12:00:00Z" }

// newDomainedMemoryTestServer ownerToken 非空时写端点要求 X-Owner-Token。
func newDomainedMemoryTestServer(t *testing.T, ownerToken string) (*httptest.Server, *memory.MemStore) {
	t.Helper()
	store := memory.NewMemStore()
	svc := memory.NewService(store, memory.Anchors{
		AgentExists: func(_ context.Context, id string) (bool, error) {
			return id == "ag_ok", nil
		},
	}, fixedMemNow)
	handler := New(Deps{
		MemorySVC:  svc,
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: ownerToken,
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, store
}

func memPost(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Owner-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func memBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func mustJSONMem(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func memCreateBody() map[string]any {
	return map[string]any{
		"namespace": "agent", "scope_id": "ag_ok", "kind": "semantic",
		"title": "偏好", "content": "偏好简洁回复。",
		"observed_at": "2026-11-04T10:00:00Z",
		"source_refs": []string{"room:evt_1"},
	}
}

func memCreateHTTP(t *testing.T, url string, body map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(mustJSONMem(t, body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("种子写入应 201，得 %d", resp.StatusCode)
	}
}

func TestMemoryCreateDomainedAPIT(t *testing.T) {
	ts, _ := newDomainedMemoryTestServer(t, "tok")
	resp := memPost(t, ts.URL+"/v1/memory/items", "tok", memCreateBody())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("应 201，得 %d：%v", resp.StatusCode, memBody(t, resp))
	}
	v := memBody(t, resp)
	for k, want := range map[string]any{
		"namespace": "agent", "scope_id": "ag_ok", "kind": "semantic",
		"status": "active", "visibility": "namespace", "confidence": 1.0,
		"sensitivity": "normal", "version": 1.0,
	} {
		if v[k] != want {
			t.Fatalf("%s 应 %v，得 %v", k, want, v[k])
		}
	}
	if id, _ := v["memory_id"].(string); len(id) < 8 || id[:4] != "mem_" {
		t.Fatalf("memory_id 形态错：%v", v["memory_id"])
	}
}

// 一审 #1：省略 source_refs → 契约响应恒空数组（不出 null——页面 .length 面）。
func TestMemoryCreateSourceRefsAlwaysArrayDomainedAPIT(t *testing.T) {
	ts, _ := newDomainedMemoryTestServer(t, "tok")
	body := memCreateBody()
	delete(body, "source_refs")
	resp := memPost(t, ts.URL+"/v1/memory/items", "tok", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("应 201，得 %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte(`"source_refs":[]`)) {
		t.Fatalf("source_refs 应序列化为 []，得 %s", raw)
	}
	// 显式空数组同口径。
	body2 := memCreateBody()
	body2["source_refs"] = []string{}
	resp2 := memPost(t, ts.URL+"/v1/memory/items", "tok", body2)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("应 201，得 %d", resp2.StatusCode)
	}
	raw2, _ := io.ReadAll(resp2.Body)
	if !bytes.Contains(raw2, []byte(`"source_refs":[]`)) {
		t.Fatalf("source_refs 应序列化为 []，得 %s", raw2)
	}
}

func TestMemoryCreateGuardAndValidationDomainedAPIT(t *testing.T) {
	ts, _ := newDomainedMemoryTestServer(t, "tok")
	t.Run("写门 401", func(t *testing.T) {
		resp := memPost(t, ts.URL+"/v1/memory/items", "", memCreateBody())
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("应 401，得 %d", resp.StatusCode)
		}
	})
	t.Run("锚不存在 409", func(t *testing.T) {
		body := memCreateBody()
		body["scope_id"] = "ag_missing"
		resp := memPost(t, ts.URL+"/v1/memory/items", "tok", body)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("应 409，得 %d", resp.StatusCode)
		}
		if v := memBody(t, resp); v["error"].(map[string]any)["code"] != "memory_scope_not_found" {
			t.Fatalf("错误码错：%v", v)
		}
	})
	t.Run("kind 非法 400", func(t *testing.T) {
		body := memCreateBody()
		body["kind"] = "other"
		resp := memPost(t, ts.URL+"/v1/memory/items", "tok", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("应 400，得 %d", resp.StatusCode)
		}
	})
	t.Run("workspace 锚非 default 400", func(t *testing.T) {
		body := memCreateBody()
		body["namespace"], body["scope_id"] = "workspace", "ws_2"
		resp := memPost(t, ts.URL+"/v1/memory/items", "tok", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("应 400，得 %d", resp.StatusCode)
		}
	})
	t.Run("未知字段拒收", func(t *testing.T) {
		body := memCreateBody()
		body["extra"] = 1
		resp := memPost(t, ts.URL+"/v1/memory/items", "tok", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("应 400，得 %d", resp.StatusCode)
		}
	})
}

func TestMemoryListAndGetDomainedAPIT(t *testing.T) {
	ts, _ := newDomainedMemoryTestServer(t, "")
	// 两条 agent + 一条 mission（域隔离）。
	for i := 0; i < 2; i++ {
		body := memCreateBody()
		body["title"] = "偏好"
		memCreateHTTP(t, ts.URL+"/v1/memory/items", body)
	}
	mb := memCreateBody()
	mb["namespace"], mb["scope_id"], mb["title"] = "mission", "m_1", "经验"
	memCreateHTTP(t, ts.URL+"/v1/memory/items", mb)

	t.Run("namespace 必填", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/v1/memory/items")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("应 400，得 %d", resp.StatusCode)
		}
	})
	t.Run("域隔离 + 游标", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/v1/memory/items?namespace=agent&limit=1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("应 200，得 %d", resp.StatusCode)
		}
		v := memBody(t, resp)
		items := v["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("limit=1 应 1 条，得 %d", len(items))
		}
		cur, ok := v["next_cursor"].(string)
		if !ok || cur == "" {
			t.Fatal("应有 next_cursor")
		}
		resp2, err := http.Get(ts.URL + "/v1/memory/items?namespace=agent&limit=1&before=" + cur)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp2.Body.Close() })
		v2 := memBody(t, resp2)
		if len(v2["items"].([]any)) != 1 {
			t.Fatalf("次页应 1 条，得 %d", len(v2["items"].([]any)))
		}
		if _, has := v2["next_cursor"]; has {
			t.Fatal("两页后不应再有 next_cursor")
		}
	})
	t.Run("scope 过滤", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/v1/memory/items?namespace=mission&scope_id=m_404")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		v := memBody(t, resp)
		if len(v["items"].([]any)) != 0 {
			t.Fatal("scope 过滤应空")
		}
	})
	t.Run("单查 404", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/v1/memory/items/mem_404")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("应 404，得 %d", resp.StatusCode)
		}
	})
	t.Run("单查 200", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/v1/memory/items?namespace=mission&scope_id=m_1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		id := memBody(t, resp)["items"].([]any)[0].(map[string]any)["memory_id"].(string)
		resp2, err := http.Get(ts.URL + "/v1/memory/items/" + id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp2.Body.Close() })
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("应 200，得 %d", resp2.StatusCode)
		}
		if v := memBody(t, resp2); v["title"] != "经验" {
			t.Fatalf("单查内容错：%v", v["title"])
		}
	})
}

// 策展四端点（0.7.1 / ADR-0038）：accept/reject/revise/supersede + 409 族。
func TestMemoryCurationDomainedAPIT(t *testing.T) {
	ts, _ := newDomainedMemoryTestServer(t, "tok")
	ctx := context.Background()

	mk := func(status string) string {
		body := memCreateBody()
		if status != "" {
			body["status"] = status
		}
		resp := memPost(t, ts.URL+"/v1/memory/items", "tok", body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("种子写入失败：%d", resp.StatusCode)
		}
		return memBody(t, resp)["memory_id"].(string)
	}

	t.Run("accept：proposed→active；active 幂等；superseded 409", func(t *testing.T) {
		id := mk("proposed")
		resp := memPost(t, ts.URL+"/v1/memory/items/"+id+"/accept", "tok", map[string]any{})
		if resp.StatusCode != http.StatusOK || memBody(t, resp)["status"] != "active" {
			t.Fatalf("接受错：%d", resp.StatusCode)
		}
		resp = memPost(t, ts.URL+"/v1/memory/items/"+id+"/accept", "tok", map[string]any{})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("幂等接受错：%d", resp.StatusCode)
		}
		memPost(t, ts.URL+"/v1/memory/items/"+id+"/reject", "tok", map[string]any{"note": "弃"})
		resp = memPost(t, ts.URL+"/v1/memory/items/"+id+"/accept", "tok", map[string]any{})
		if resp.StatusCode != http.StatusConflict || memBody(t, resp)["error"].(map[string]any)["code"] != "memory_terminal" {
			t.Fatalf("终结复活应 409 memory_terminal，得 %d", resp.StatusCode)
		}
	})

	t.Run("revise：版本递增 + CAS 409 + edit_history 透出", func(t *testing.T) {
		id := mk("")
		resp := memPost(t, ts.URL+"/v1/memory/items/"+id+"/revise", "tok",
			map[string]any{"expected_version": 1, "title": "新标题", "note": "改"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("修订错：%d %v", resp.StatusCode, memBody(t, resp))
		}
		v := memBody(t, resp)
		if v["version"].(float64) != 2 {
			t.Fatalf("版本应 2，得 %v", v["version"])
		}
		hist := v["edit_history"].([]any)
		if len(hist) != 1 || hist[0].(map[string]any)["cause"] != "revise" {
			t.Fatalf("留痕透出错：%v", hist)
		}
		resp = memPost(t, ts.URL+"/v1/memory/items/"+id+"/revise", "tok",
			map[string]any{"expected_version": 1, "title": "撞"})
		if resp.StatusCode != http.StatusConflict || memBody(t, resp)["error"].(map[string]any)["code"] != "memory_version_conflict" {
			t.Fatalf("CAS 应 409 memory_version_conflict，得 %d", resp.StatusCode)
		}
	})

	t.Run("supersede：双动作结果 + replacement 锚 409", func(t *testing.T) {
		id := mk("")
		resp := memPost(t, ts.URL+"/v1/memory/items/"+id+"/supersede", "tok", map[string]any{
			"expected_version": 1, "note": "换",
			"replacement": map[string]any{
				"namespace": "agent", "scope_id": "ag_ok", "kind": "semantic",
				"title": "替代", "content": "c", "observed_at": "2026-11-05T10:00:00Z",
			},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("替代错：%d %v", resp.StatusCode, memBody(t, resp))
		}
		v := memBody(t, resp)
		if v["superseded"].(map[string]any)["status"] != "superseded" ||
			v["replacement"].(map[string]any)["supersedes"].([]any)[0] != id {
			t.Fatalf("双动作结果错：%v", v)
		}
		resp = memPost(t, ts.URL+"/v1/memory/items/"+id+"/supersede", "tok", map[string]any{
			"expected_version": 9,
			"replacement": map[string]any{
				"namespace": "agent", "scope_id": "ag_ok", "kind": "semantic",
				"title": "t", "content": "c", "observed_at": "2026-11-05T10:00:00Z",
			},
		})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("CAS 应 409，得 %d", resp.StatusCode)
		}
	})

	t.Run("include_expired：默认排除/显式可见", func(t *testing.T) {
		body := memCreateBody()
		body["valid_from"] = "2025-01-01T00:00:00Z"
		body["valid_to"] = "2025-01-02T00:00:00Z" // 早已过期
		if resp := memPost(t, ts.URL+"/v1/memory/items", "tok", body); resp.StatusCode != http.StatusCreated {
			t.Fatalf("过期条目写入失败：%d", resp.StatusCode)
		}
		listNS := func(q string) int {
			resp, err := http.Get(ts.URL + "/v1/memory/items?namespace=agent" + q)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { resp.Body.Close() })
			return len(memBody(t, resp)["items"].([]any))
		}
		base := listNS("") // 前序子测试的存量 active（未过期）——基线法
		if got := listNS(""); got != base {
			t.Fatalf("默认口径不应变化，base=%d got=%d", base, got)
		}
		if n := listNS("&include_expired=true"); n != base+1 {
			t.Fatalf("显式 include 应 base+1=%d 条，得 %d", base+1, n)
		}
	})
	_ = ctx
}

func TestMemoryUnavailableDomainedAPIT(t *testing.T) {
	handler := New(Deps{Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"}})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/memory/items?namespace=agent")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("应 503，得 %d", resp.StatusCode)
	}
}
