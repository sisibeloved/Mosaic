// UT 层：文档面 HTTP 端点（RFC-0014 Phase 2）——写门三件套、create→snapshot
// 往返、幂等回放、CAS 409 体（当前态摘要）、归档只读 409、404、list 过滤、
// export、search、doc:{id} SSE 追平/直播/去重/resync。
package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// newDocTestServer 文档面装配（内存存储；确定性 ID——doc_<12 位数字> 合
// ^doc_[0-9a-f]{12}$ 形态）。ownerToken 非空时启用写端点 token 门。
func newDocTestServer(t *testing.T, ownerToken string) (*httptest.Server, *doc.MemStore, *sse.Hub) {
	t.Helper()
	store := doc.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := doc.NewService(doc.Config{
		Store:    store,
		Searcher: store, // 全文检索读路径（MemStore 线性基准实现）
		Lister:   store,
		Clock:    func() string { return "2026-09-16T11:00:00.000Z" },
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return prefix + "_doc_" + fmt.Sprintf("%08d", n)
		},
		NewDocID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return "doc_" + fmt.Sprintf("%012d", n)
		},
		Tenant: "ten_local",
	})
	hub := sse.NewHub()
	handler := New(Deps{
		DocSVC:     svc,
		DocReader:  store,
		Hub:        hub,
		Actor:      room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken: ownerToken,
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, store, hub
}

func postDocJSON(t *testing.T, url string, body any, headers map[string]string) (*http.Response, apigen.DocCommandResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out apigen.DocCommandResponse
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

func docCommand(kind string, expected int64, idem string, payload map[string]any) map[string]any {
	return map[string]any{
		"command_kind": kind, "expected_doc_version": expected,
		"idempotency_key": idem, "issued_at": "2026-09-16T12:00:00.000Z",
		"payload": payload,
	}
}

// createTestDoc 经 API 建文档（无初始块，version=1）。
func createTestDoc(t *testing.T, ts *httptest.Server, idem, title string) apigen.DocCommandResponse {
	t.Helper()
	resp, created := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, idem, map[string]any{"title": title}), nil)
	if resp.StatusCode != 200 || created.DocId == "" || created.DocVersion != 1 || created.Replayed {
		t.Fatalf("create doc: status=%d body=%+v", resp.StatusCode, created)
	}
	return created
}

// 写门三件套：token 401 / 跨源 403 / 非 JSON 415（房间写端点同规——四轮复审 #15 平移）。
func TestDocWriteGuards(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "tok_doc")
	raw, _ := json.Marshal(docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db001",
		map[string]any{"title": "门"}))
	post := func(origin, contentType, token string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/docs", bytes.NewReader(raw))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if token != "" {
			req.Header.Set("X-Owner-Token", token)
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("", "application/json", ""); code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，got %d", code)
	}
	if code := post("", "application/json", "tok_wrong"); code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401，got %d", code)
	}
	if code := post("http://evil.example", "application/json", "tok_doc"); code != http.StatusForbidden {
		t.Fatalf("跨源写应 403，got %d", code)
	}
	if code := post("", "text/plain", "tok_doc"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain 应 415，got %d", code)
	}
	if code := post("", "application/json", "tok_doc"); code != http.StatusOK {
		t.Fatalf("合法写应 200，got %d", code)
	}
	// GET 读端点不设 token 门（与房间 GET 一致）
	resp, err := http.Get(ts.URL + "/v1/docs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/docs 无 token 应 200，got %d", resp.StatusCode)
	}
}

// create（含 initial_blocks）→ snapshot 往返：块清单与服务端分配的 block_id 回读一致。
func TestDocCreateSnapshotRoundtrip(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	resp, created := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db010", map[string]any{
			"title": "设计稿",
			"initial_blocks": []map[string]any{
				{"type": "paragraph", "text": "第一段"},
				{"type": "heading", "text": "小节"},
			},
		}), nil)
	// created + 首条 revision 一批 → version 2
	if resp.StatusCode != 200 || created.DocVersion != 2 {
		t.Fatalf("create: status=%d body=%+v", resp.StatusCode, created)
	}

	get, err := http.Get(ts.URL + "/v1/docs/" + created.DocId)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d", get.StatusCode)
	}
	var state doc.DocState
	if err := json.NewDecoder(get.Body).Decode(&state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.DocID != created.DocId || state.Title != "设计稿" || state.Version != 2 ||
		state.Status != "active" || state.CreatedBy != "par_owner" || state.Format != "markdown" {
		t.Fatalf("快照不符：%+v", state)
	}
	if len(state.Blocks) != 2 || state.Blocks[0].Text != "第一段" || state.Blocks[1].Type != "heading" {
		t.Fatalf("块清单不符：%+v", state.Blocks)
	}
	for _, b := range state.Blocks {
		if !strings.HasPrefix(b.BlockID, "blk_") {
			t.Fatalf("block_id 应由服务端分配 blk_*：%+v", b)
		}
	}
}

// 幂等回放：同命令同键 → replayed=true 同事件；同键异指纹 → 409 idempotency_conflict。
func TestDocIdempotentReplay(t *testing.T) {
	ts, store, _ := newDocTestServer(t, "")
	cmd := docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db020", map[string]any{"title": "回放"})
	_, first := postDocJSON(t, ts.URL+"/v1/docs", cmd, nil)
	resp, replay := postDocJSON(t, ts.URL+"/v1/docs", cmd, nil)
	if resp.StatusCode != 200 || !replay.Replayed || replay.EventId != first.EventId || replay.DocId != first.DocId {
		t.Fatalf("replay: status=%d body=%+v", resp.StatusCode, replay)
	}
	if n := len(store.DocEvents(first.DocId)); n != 1 {
		t.Fatalf("事件数 = %d（期望 1，回放不追加）", n)
	}

	// 同键异载荷 → 409 idempotency_conflict
	conflict := docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db020", map[string]any{"title": "换内容"})
	resp, _ = postDocJSON(t, ts.URL+"/v1/docs", conflict, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同键异指纹 status = %d（期望 409）", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "idempotency_conflict" {
		t.Fatalf("code = %q（期望 idempotency_conflict）：%v", code, body)
	}
}

// CAS 409 体：version_conflict 响应带 current_version 与当前态摘要（RFC-0014 §2.3）。
func TestDocVersionConflictBody(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	created := createTestDoc(t, ts, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db030", "冲突面")

	resp, _ := postDocJSON(t, ts.URL+"/v1/docs/"+created.DocId+"/commands",
		docCommand("commit_doc_revision", 99, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db031", map[string]any{
			"ops": []map[string]any{{"op": "append", "block_id": nil, "block": map[string]any{"type": "paragraph", "text": "过期批"}}},
		}), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d（期望 409）", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		DocID          string       `json:"doc_id"`
		CurrentVersion int64        `json:"current_version"`
		State          doc.DocState `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "version_conflict" {
		t.Fatalf("code = %q（期望 version_conflict）", body.Error.Code)
	}
	if body.DocID != created.DocId || body.CurrentVersion != 1 {
		t.Fatalf("冲突体不符：%+v", body)
	}
	if body.State.Version != 1 || body.State.Title != "冲突面" || body.State.Blocks == nil {
		t.Fatalf("当前态摘要缺失：%+v", body.State)
	}
}

// 归档只读：rename/revision/archive 拒绝（409 doc_archived）；duplicate/export 允许。
func TestDocArchivedReadOnly(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	created := createTestDoc(t, ts, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db040", "归档前")

	resp, archived := postDocJSON(t, ts.URL+"/v1/docs/"+created.DocId+"/commands",
		docCommand("archive_doc", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db041", map[string]any{}), nil)
	if resp.StatusCode != 200 || archived.DocVersion != 2 {
		t.Fatalf("archive: status=%d body=%+v", resp.StatusCode, archived)
	}

	resp, _ = postDocJSON(t, ts.URL+"/v1/docs/"+created.DocId+"/commands",
		docCommand("rename_doc", 2, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db042", map[string]any{"title": "新名"}), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("归档后 rename status = %d（期望 409）", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "doc_archived" {
		t.Fatalf("code = %q（期望 doc_archived）：%v", code, body)
	}

	// 归档可导出（只读约束只拦写）
	exp, err := http.Get(ts.URL + "/v1/docs/" + created.DocId + "/export")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	exp.Body.Close()
	if exp.StatusCode != http.StatusOK {
		t.Fatalf("归档导出 status = %d（期望 200）", exp.StatusCode)
	}

	// 恢复后写放行
	resp, _ = postDocJSON(t, ts.URL+"/v1/docs/"+created.DocId+"/commands",
		docCommand("restore_doc", 2, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db043", map[string]any{}), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("restore status = %d", resp.StatusCode)
	}
	resp, renamed := postDocJSON(t, ts.URL+"/v1/docs/"+created.DocId+"/commands",
		docCommand("rename_doc", 3, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db044", map[string]any{"title": "新名"}), nil)
	if resp.StatusCode != 200 || renamed.DocVersion != 4 {
		t.Fatalf("恢复后 rename: status=%d body=%+v", resp.StatusCode, renamed)
	}
}

// 404：未创建/已删除文档的 snapshot/commands/export 全部 doc_not_found。
func TestDocNotFound(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	ghost := "doc_999999999999"

	get, err := http.Get(ts.URL + "/v1/docs/" + ghost)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("snapshot status = %d（期望 404）", get.StatusCode)
	}

	resp, _ := postDocJSON(t, ts.URL+"/v1/docs/"+ghost+"/commands",
		docCommand("rename_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db050", map[string]any{"title": "鬼"}), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("commands status = %d（期望 404）", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "doc_not_found" {
		t.Fatalf("code = %q（期望 doc_not_found）：%v", code, body)
	}

	exp, err := http.Get(ts.URL + "/v1/docs/" + ghost + "/export")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	exp.Body.Close()
	if exp.StatusCode != http.StatusNotFound {
		t.Fatalf("export status = %d（期望 404）", exp.StatusCode)
	}

	// 删除后 404（墓碑先例：级联清除）
	created := createTestDoc(t, ts, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db051", "将删")
	resp, _ = postDocJSON(t, ts.URL+"/v1/docs/"+created.DocId+"/commands",
		docCommand("delete_doc", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db052", map[string]any{"reason": "清理"}), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	get, _ = http.Get(ts.URL + "/v1/docs/" + created.DocId)
	get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后 snapshot status = %d（期望 404）", get.StatusCode)
	}
}

// 列表过滤：created_by（"我/各 agent"）与 status（active/archived）筛选。
func TestDocListFilters(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	first := createTestDoc(t, ts, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db060", "第一篇")
	second := createTestDoc(t, ts, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db061", "第二篇")

	listDocs := func(query string) (int, []doc.DocSummary) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/docs" + query)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Docs []doc.DocSummary `json:"docs"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Docs
	}

	_, docs := listDocs("")
	if len(docs) != 2 {
		t.Fatalf("全量 = %d（期望 2）", len(docs))
	}
	byID := map[string]doc.DocSummary{}
	for _, d := range docs {
		byID[d.DocID] = d
	}
	if byID[first.DocId].Title != "第一篇" || byID[second.DocId].CreatedBy != "par_owner" {
		t.Fatalf("摘要字段不符：%+v", docs)
	}

	_, docs = listDocs("?created_by=par_owner")
	if len(docs) != 2 {
		t.Fatalf("created_by=par_owner = %d（期望 2）", len(docs))
	}
	_, docs = listDocs("?created_by=par_nobody")
	if len(docs) != 0 {
		t.Fatalf("created_by=par_nobody = %d（期望 0）", len(docs))
	}

	// 归档一篇后 status 过滤
	resp, _ := postDocJSON(t, ts.URL+"/v1/docs/"+first.DocId+"/commands",
		docCommand("archive_doc", 1, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db062", map[string]any{}), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("archive status = %d", resp.StatusCode)
	}
	_, docs = listDocs("?status=archived")
	if len(docs) != 1 || docs[0].DocID != first.DocId || docs[0].Status != "archived" {
		t.Fatalf("status=archived 不符：%+v", docs)
	}
	_, docs = listDocs("?status=active")
	if len(docs) != 1 || docs[0].DocID != second.DocId {
		t.Fatalf("status=active 不符：%+v", docs)
	}

	// 非法 status → 400
	code, _ := listDocs("?status=bogus")
	if code != http.StatusBadRequest {
		t.Fatalf("status=bogus 应 400，got %d", code)
	}
}

// export：markdown 渲染（标题 H1 + 块依序）+ Content-Disposition。
func TestDocExport(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	resp, created := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db070", map[string]any{
			"title":          "导出稿",
			"initial_blocks": []map[string]any{{"type": "paragraph", "text": "正文一段"}},
		}), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create status = %d", resp.StatusCode)
	}

	exp, err := http.Get(ts.URL + "/v1/docs/" + created.DocId + "/export")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer exp.Body.Close()
	if exp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", exp.StatusCode)
	}
	if ct := exp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("Content-Type = %q（期望 text/markdown）", ct)
	}
	if cd := exp.Header.Get("Content-Disposition"); !strings.Contains(cd, created.DocId+".md") {
		t.Fatalf("Content-Disposition = %q（期望含 %s.md）", cd, created.DocId)
	}
	raw, _ := io.ReadAll(exp.Body)
	md := string(raw)
	if !strings.Contains(md, "# 导出稿") || !strings.Contains(md, "正文一段") {
		t.Fatalf("markdown 内容不符：%q", md)
	}
}

// search：标题/正文命中；q 与 limit 校验（同房内检索纪律）。
func TestDocSearch(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	resp, created := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db080", map[string]any{
			"title":          "协同编辑设计稿",
			"initial_blocks": []map[string]any{{"type": "paragraph", "text": "修订批语义与乐观并发"}},
		}), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	createTestDoc(t, ts, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db081", "购物清单")

	search := func(query string) (int, []doc.DocSearchHit) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/docs/search" + query)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Hits []doc.DocSearchHit `json:"hits"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Hits
	}

	code, hits := search("?q=协同编辑")
	if code != 200 || len(hits) != 1 || hits[0].DocID != created.DocId || hits[0].Title != "协同编辑设计稿" {
		t.Fatalf("标题命中不符：code=%d %+v", code, hits)
	}
	_, hits = search("?q=乐观并发")
	if len(hits) != 1 || hits[0].DocID != created.DocId {
		t.Fatalf("正文命中不符：%+v", hits)
	}
	_, hits = search("?q=不存在的词")
	if len(hits) != 0 {
		t.Fatalf("无命中应空：%+v", hits)
	}
	if code, _ := search("?q="); code != http.StatusBadRequest {
		t.Fatalf("空 q 应 400，got %d", code)
	}
	if code, _ := search("?q=x&limit=0"); code != http.StatusBadRequest {
		t.Fatalf("limit=0 应 400，got %d", code)
	}
	if code, _ := search("?q=x&limit=101"); code != http.StatusBadRequest {
		t.Fatalf("limit=101 应 400，got %d", code)
	}
}

// doc:{id} SSE：追平（存储事件）+ 直播（hub 帧）+ 去重 + 无内部字段泄露。
func TestDocSSEStreamCatchUpLiveAndDedup(t *testing.T) {
	ts, store, hub := newDocTestServer(t, "")
	deliver := DocHubConsumer(hub)

	resp, created := postDocJSON(t, ts.URL+"/v1/docs",
		docCommand("create_doc", 0, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db090", map[string]any{
			"title":          "sse 稿",
			"initial_blocks": []map[string]any{{"type": "paragraph", "text": "追平我"}},
		}), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	// 未接 dispatcher：手动 Deliver 模拟已分发（entry.RoomID=doc_id、GlobalPos=version）
	events := store.DocEvents(created.DocId)
	for _, env := range events {
		raw, _ := json.Marshal(env)
		deliver.Deliver(context.Background(), outbox.Entry{
			RoomID: created.DocId, EventID: env.EventID, GlobalPos: env.Version, Envelope: raw,
		})
	}

	// 打开订阅（从头）：追平 2 帧后，延时投递第三帧走直播通道
	go func() {
		time.Sleep(200 * time.Millisecond)
		hub.Publish("doc:"+created.DocId, sse.ViewEvent{
			Cursor: protocol.EncodeCursor(99),
			Type:   protocol.EventDocRevisionCommitted,
			Data:   []byte(`{"event_id":"evt_live","position":"` + protocol.EncodeCursor(99) + `"}`),
		})
	}()
	frames, cancel := readSSE(t, ts.URL+"/v1/docs/"+created.DocId+"/events", 3, 3*time.Second)
	defer cancel()

	if len(frames) < 3 {
		t.Fatalf("帧数 = %d", len(frames))
	}
	last := int64(-1)
	seen := map[string]bool{}
	for _, f := range frames {
		for _, leaked := range []string{"tenant_id", "version", "metadata"} {
			if _, ok := f.Data[leaked]; ok {
				t.Fatalf("SSE 帧泄露内部字段 %q", leaked)
			}
		}
		pos, err := protocol.DecodeCursor(f.ID)
		if err != nil {
			t.Fatalf("帧 id 非法：%q", f.ID)
		}
		if seen[f.ID] {
			t.Fatalf("重复帧 id=%s", f.ID)
		}
		seen[f.ID] = true
		if pos <= last {
			t.Fatalf("游标非严格递增：%d after %d", pos, last)
		}
		last = pos
	}
	if frames[0].Name != protocol.EventDocCreated || frames[1].Name != protocol.EventDocRevisionCommitted {
		t.Fatalf("追平帧类型不符：%s, %s", frames[0].Name, frames[1].Name)
	}
	if docID, _ := frames[0].Data["doc_id"].(string); docID != created.DocId {
		t.Fatalf("帧 doc_id = %q（期望 %s）", docID, created.DocId)
	}
}

func TestDocSSEBadCursorRejected(t *testing.T) {
	ts, _, _ := newDocTestServer(t, "")
	resp, err := http.Get(ts.URL + "/v1/docs/doc_aaaaaaaaaaaa/events?cursor=not-base64!!")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cursor status = %d", resp.StatusCode)
	}
}

// flakyDocReader 首次 DocEventsAfter 即失败：驱动 resync_required 信号。
type flakyDocReader struct{ doc.DocEventReader }

func (f flakyDocReader) DocEventsAfter(ctx context.Context, docID, cursor string, limit int) ([]doc.StoredDocEvent, string, error) {
	return nil, "", fmt.Errorf("doc reader boom")
}

// 追平失败必须发 resync_required 具名帧（房间订阅同规，RFC-0001 §订阅平移）。
func TestDocSSEResyncRequiredOnCatchUpFailure(t *testing.T) {
	store := doc.NewMemStore()
	svc := doc.NewService(doc.Config{
		Store: store, Clock: func() string { return "t" },
		NewID: func(p string) string { return p + "_x" }, NewDocID: func() string { return "doc_aaaaaaaaaaaa" },
		Tenant: "ten_local",
	})
	ts := httptest.NewServer(New(Deps{
		DocSVC: svc, DocReader: flakyDocReader{DocEventReader: store}, Hub: sse.NewHub(),
		Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"},
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/docs/doc_aaaaaaaaaaaa/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get sse: %v", err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	var sawResync bool
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event: resync_required") {
			sawResync = true
		}
	}
	if !sawResync {
		t.Fatal("追平失败必须发出 event: resync_required 具名帧")
	}
}

// 未装配文档面（nil DocSVC）：全部 doc 端点 404 不暴露面（回执/监控 nil 先例）。
func TestDocEndpointsUnavailableWhenUnassembled(t *testing.T) {
	ts, _, _ := newTestServer(t) // 房间装配：无 DocSVC/DocReader
	for _, path := range []string{
		"/v1/docs", "/v1/docs/doc_aaaaaaaaaaaa", "/v1/docs/doc_aaaaaaaaaaaa/export", "/v1/docs/search?q=x",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s status = %d（期望 404）", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("GET %s 应为 JSON 404（非 mux 默认），ct=%q", path, ct)
		}
	}
	resp, err := http.Post(ts.URL+"/v1/docs", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /v1/docs status = %d（期望 404）", resp.StatusCode)
	}
}

// dev 调试导出：manifest + NDJSON 事件流（房间导出先例平移）。
func TestDebugDocExport(t *testing.T) {
	store := doc.NewMemStore()
	svc := doc.NewService(doc.Config{
		Store: store, Clock: func() string { return "2026-09-16T11:00:00.000Z" },
		NewID:    func(p string) string { return p + "_dbg" },
		NewDocID: func() string { return "doc_deb000000001" },
		Tenant:   "ten_local",
	})
	ts := httptest.NewServer(New(Deps{
		DocSVC: svc, DocReader: store, Hub: sse.NewHub(), Dev: true,
		Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"},
	}))
	defer ts.Close()
	createTestDoc(t, ts, "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4db0a0", "调试导出")

	resp, err := http.Get(ts.URL + "/v1/debug/docs/doc_deb000000001/export")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q（期望 application/x-ndjson）", ct)
	}
	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 2 { // manifest + doc.created
		t.Fatalf("行数 = %d（期望 2）", len(lines))
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &manifest); err != nil {
		t.Fatalf("manifest 非法：%v", err)
	}
	if manifest["kind"] != "mosaic.doc.export" || manifest["doc_id"] != "doc_deb000000001" ||
		manifest["event_count"].(float64) != 1 || manifest["watermark"].(float64) != 1 {
		t.Fatalf("manifest 不符：%v", manifest)
	}
	var env protocol.DocEnvelope
	if err := json.Unmarshal([]byte(lines[1]), &env); err != nil || env.Type != protocol.EventDocCreated {
		t.Fatalf("事件行不符：%v %v", err, env.Type)
	}
}
