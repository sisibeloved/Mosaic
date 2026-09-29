// 表格交换 HTTP 面 UT（RFC-0017 M2）：脚本 MCP 上游驱动 preview→execute→
// 历史全链（导入+导出）、写门矩阵、stale 预览 409、幂等重放、未装配 404。
// 交换记录走真 sqlite Store（httpapi 侧即集成面）。
package httpapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/storage/sqlite"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
)

// transferTestEnv 交换面测试环境。
type transferTestEnv struct {
	ts     *httptest.Server
	docSvc *doc.Service
}

// newTransferTestEnv 脚本 MCP（腾讯侧工具分发 + 写入捕获回显）+ 真 sqlite
// 交换表 + 真 doc.Service。
func newTransferTestEnv(t *testing.T, ownerToken string) *transferTestEnv {
	t.Helper()
	var mu sync.Mutex
	writes := map[string][]map[string]any{}
	var seq int64
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		tool := req.Params.Name
		toolJSON := func(payload string) {
			enc, _ := json.Marshal(payload)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":%s}]}}`, enc)
		}
		switch tool {
		case "manage.query_file_info":
			toolJSON(`{"file_id":"f1","title":"云端表","type":"sheet","status":"normal"}`)
		case "manage.create_file":
			seq++
			toolJSON(fmt.Sprintf(`{"file_id":"NEW1","title":"t","url":"https://docs.qq.com/sheet/NEW%d","type":"sheet"}`, seq))
		case "sheet.get_sheet_info":
			toolJSON(`{"sheets":[{"sheet_id":"S0","sheet_name":"工作表1","sheet_type":"worksheet","row_count":10,"col_count":3}]}`)
		case "sheet.get_cell_data":
			mu.Lock()
			sid, _ := req.Params.Arguments["sheet_id"].(string)
			var sb strings.Builder
			sb.WriteString(`{"cells":[`)
			for i, c := range writes[sid] {
				if i > 0 {
					sb.WriteString(",")
				}
				raw, _ := json.Marshal(c)
				sb.Write(raw)
			}
			sb.WriteString(`]}`)
			mu.Unlock()
			toolJSON(sb.String())
		case "sheet.get_merged_cells":
			toolJSON(`{"merged_cells":[]}`)
		case "sheet.set_range_value":
			mu.Lock()
			sid, _ := req.Params.Arguments["sheet_id"].(string)
			vals, _ := req.Params.Arguments["values"].([]any)
			for _, v := range vals {
				if vm, ok := v.(map[string]any); ok {
					writes[sid] = append(writes[sid], vm)
				}
			}
			mu.Unlock()
			toolJSON(`{}`)
		default: // rename_sheet / add_sheet
			if tool == "sheet.add_sheet" {
				toolJSON(`{"sheet_id":"S2"}`)
				return
			}
			toolJSON(`{}`)
		}
	}))
	t.Cleanup(mcpSrv.Close)

	docStore := doc.NewMemStore()
	var seq2 int64
	docSvc := doc.NewService(doc.Config{
		Store:    docStore,
		Searcher: docStore,
		Lister:   docStore,
		Clock:    func() string { return "2026-09-29T12:00:00Z" },
		NewID: func(prefix string) string {
			seq2++
			return fmt.Sprintf("%s%010d", prefix, seq2)
		},
		NewDocID: func() string { return "doc_0000000000aa" },
		Tenant:   "ten_local",
	})
	mgr, err := extdocs.LoadManager(t.TempDir() + "/extdocs")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Save(extdocs.ProviderTencent, map[string]string{"token": "tok"}); err != nil {
		t.Fatal(err)
	}
	tencent := extdocs.NewTencentProvider(mgr, mcpSrv.URL)
	svc := extdocs.NewService(nil)
	svc.Register(tencent)
	store, err := sqlite.Open(t.TempDir() + "/transfers.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var seq3 int64
	transfers := extdocs.NewSheetTransferService(svc, docSvc, store, func(prefix string) string {
		seq3++
		return fmt.Sprintf("%s_t%04d", prefix, seq3)
	})
	roomStore := room.NewMemStore()
	roomSvc := room.NewService(room.Config{
		Store:  roomStore,
		Lister: roomStore,
		Clock:  func() string { return "2026-09-29T12:00:00Z" },
		NewID:  func(prefix string) string { return prefix + "000000000001" },
		Tenant: "ten_local",
	})
	handler := New(Deps{
		SVC:            roomSvc,
		DocSVC:         docSvc,
		DocReader:      docStore,
		ExtDocs:        svc,
		SheetTransfers: transfers,
		Actor:          room.Actor{ParticipantID: "par_owner", Kind: "human"},
		OwnerToken:     ownerToken,
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &transferTestEnv{ts: ts, docSvc: docSvc}
}

func TestSheetTransferImportExportFlow(t *testing.T) {
	env := newTransferTestEnv(t, "tok_tr")
	h := extJSON("tok_tr")

	// 预览（导入，腾讯 URL → host 分派）
	resp, body := extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/preview",
		map[string]any{"direction": "import", "url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh"}, h)
	if resp.StatusCode != 200 {
		t.Fatalf("preview = %d %v", resp.StatusCode, body)
	}
	hash, _ := body["snapshot_hash"].(string)
	if hash == "" || body["provider"] != extdocs.ProviderTencent {
		t.Fatalf("preview body = %v", body)
	}
	sheets, _ := body["sheets"].([]any)
	if len(sheets) != 1 {
		t.Fatalf("sheets = %v", sheets)
	}

	// 写门：无 token → 401
	resp, _ = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers",
		map[string]any{"direction": "import", "url": "https://docs.qq.com/sheet/x", "idempotency_key": "k12345678"},
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != 401 {
		t.Fatalf("无 token = %d", resp.StatusCode)
	}

	// 执行（携 hash）
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers",
		map[string]any{"direction": "import", "url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh",
			"snapshot_hash": hash, "idempotency_key": "key-import-1"}, h)
	if resp.StatusCode != 201 {
		t.Fatalf("execute = %d %v", resp.StatusCode, body)
	}
	docID, _ := body["doc_id"].(string)
	tid, _ := body["transfer_id"].(string)
	if docID == "" || tid == "" || body["status"] != "succeeded" {
		t.Fatalf("record = %v", body)
	}
	// 落库可读：文档面 sheets 端点
	resp, body = extRequest(t, "GET", env.ts.URL+"/v1/docs/"+docID+"/sheets", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("doc sheets = %d %v", resp.StatusCode, body)
	}

	// 幂等重放：同键同记录
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers",
		map[string]any{"direction": "import", "url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh",
			"idempotency_key": "key-import-1"}, h)
	if resp.StatusCode != 201 || body["transfer_id"] != tid {
		t.Fatalf("replay = %d %v", resp.StatusCode, body)
	}

	// stale hash → 409
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers",
		map[string]any{"direction": "import", "url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh",
			"snapshot_hash": "deadbeef", "idempotency_key": "key-stale-1"}, h)
	if resp.StatusCode != 409 || extCodeOf(body) != "sheet_transfer_stale_preview" {
		t.Fatalf("stale = %d %v", resp.StatusCode, body)
	}

	// 导出预览 + 执行
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/preview",
		map[string]any{"direction": "export", "doc_id": docID, "provider": "tencent_docs"}, h)
	if resp.StatusCode != 200 {
		t.Fatalf("export preview = %d %v", resp.StatusCode, body)
	}
	ver := int64(0)
	if f, ok := body["source_version"].(float64); ok {
		ver = int64(f)
	}
	if ver == 0 {
		t.Fatalf("export preview = %v", body)
	}
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers",
		map[string]any{"direction": "export", "doc_id": docID, "provider": "tencent_docs",
			"source_version": ver, "idempotency_key": "key-export-1"}, h)
	if resp.StatusCode != 201 {
		t.Fatalf("export execute = %d %v", resp.StatusCode, body)
	}
	if u, _ := body["external_url"].(string); !strings.Contains(u, "docs.qq.com/sheet/NEW") {
		t.Fatalf("export record = %v", body)
	}

	// 历史：导入 + stale 失败留痕 + 导出（失败尝试也记录——M2 纪律）
	resp, body = extRequest(t, "GET", env.ts.URL+"/v1/extdocs/sheet-transfers?limit=10", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	trs := body["transfers"].([]any)
	if len(trs) != 3 {
		t.Fatalf("transfers = %d", len(trs))
	}
	statuses := map[string]int{}
	for _, raw := range trs {
		m := raw.(map[string]any)
		statuses[m["status"].(string)]++
	}
	if statuses["succeeded"] != 2 || statuses["failed"] != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
}

func TestSheetTransferUnavailable(t *testing.T) {
	// SheetTransfers 未装配 → 404 extdocs_unavailable
	ts := httptest.NewServer(New(Deps{}))
	t.Cleanup(ts.Close)
	resp, _ := extRequest(t, "GET", ts.URL+"/v1/extdocs/sheet-transfers", nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unavailable = %d", resp.StatusCode)
	}
}

// 编译期锚：确保生成类型在契约面被引用（漂移即编译失败）。
var _ apigen.SheetTransferRecord

func TestSheetTransferUpdateFlow(t *testing.T) {
	env := newTransferTestEnv(t, "tok_u")
	h := extJSON("tok_u")

	// 造本地表（导入空表 + 写 5 格）
	resp, body := extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/preview",
		map[string]any{"direction": "import", "url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh"}, h)
	if resp.StatusCode != 200 {
		t.Fatalf("import preview = %d %v", resp.StatusCode, body)
	}
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers",
		map[string]any{"direction": "import", "url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh",
			"snapshot_hash": body["snapshot_hash"], "idempotency_key": "key-u-import-1"}, h)
	if resp.StatusCode != 201 {
		t.Fatalf("import = %d %v", resp.StatusCode, body)
	}
	docID := body["doc_id"].(string)
	// 写 5 格（差异）
	_, sheetsBody := extRequest(t, "GET", env.ts.URL+"/v1/docs/"+docID+"/sheets", nil, nil)
	sid := sheetsBody["sheets"].([]any)[0].(map[string]any)["sheet_id"].(string)
	_, stBody := extRequest(t, "GET", env.ts.URL+"/v1/docs/"+docID, nil, nil)
	ver := int(stBody["version"].(float64))
	cells := make([]map[string]any, 5)
	for i := range cells {
		cells[i] = map[string]any{"r": 1, "c": i + 1, "type": "number", "number": float64(i)}
	}
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/docs/"+docID+"/commands", map[string]any{
		"command_kind": "commit_sheet_revision", "expected_doc_version": ver,
		"idempotency_key": uuid7(), "issued_at": "2026-09-29T00:00:00Z",
		"payload": map[string]any{"base_version": ver, "ops": []any{
			map[string]any{"op": "set_cells", "sheet_id": sid, "cells": cells}}}}, h)
	if resp.StatusCode != 200 {
		t.Fatalf("commit = %d %v", resp.StatusCode, body)
	}

	// 回写预览（目标 = 导入源 URL；目标远端空 → 差异 5 格）
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/update/preview",
		map[string]any{"doc_id": docID, "provider": "tencent_docs", "external_url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh"}, h)
	if resp.StatusCode != 200 {
		t.Fatalf("update preview = %d %v", resp.StatusCode, body)
	}
	if body["cells_total"].(float64) != 5 {
		t.Fatalf("cells_total = %v", body["cells_total"])
	}

	// 执行（异步 → 轮询到终态）
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/update",
		map[string]any{"doc_id": docID, "provider": "tencent_docs",
			"external_url": "https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh",
			"target_hash":  body["target_hash"], "source_version": body["source_version"],
			"idempotency_key": "key-u-exec-001"}, h)
	if resp.StatusCode != 202 && resp.StatusCode != 200 {
		t.Fatalf("update exec = %d %v", resp.StatusCode, body)
	}
	tid := body["transfer_id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body = extRequest(t, "GET", env.ts.URL+"/v1/extdocs/sheet-transfers/by-id/"+tid, nil, nil)
		if body["status"] != "running" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if body["status"] == "running" || body["status"] == nil {
		t.Fatalf("update 终态 = %v", body["status"])
	}
	if cw, ok := body["cells_written"].(float64); !ok || cw != 5 {
		t.Fatalf("cells_written = %v（body=%v）", body["cells_written"], body)
	}

	// 终态后取消 → 幂等 200；续跑 → 409 not_resumable
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/by-id/"+tid+"/cancel", nil, h)
	if resp.StatusCode != 200 {
		t.Fatalf("cancel(终态) = %d %v", resp.StatusCode, body)
	}
	resp, body = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/by-id/"+tid+"/resume", nil, h)
	if resp.StatusCode != 409 || extCodeOf(body) != "sheet_transfer_not_resumable" {
		t.Fatalf("resume(终态) = %d %v", resp.StatusCode, body)
	}
	// 写门：无 token 取消 → 401
	resp, _ = extRequest(t, "POST", env.ts.URL+"/v1/extdocs/sheet-transfers/by-id/"+tid+"/cancel", nil,
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != 401 {
		t.Fatalf("cancel 无 token = %d", resp.StatusCode)
	}
}

// uuid7 UUIDv7（doc 命令幂等键契约）。
func uuid7() string {
	ms := time.Now().UnixMilli()
	var b [16]byte
	b[0], b[1] = byte(ms>>40), byte(ms>>32)
	b[2], b[3] = byte(ms>>24), byte(ms>>16)
	b[4], b[5] = byte(ms>>8), byte(ms)
	rnd := make([]byte, 10)
	_, _ = rand.Read(rnd)
	b[6] = rnd[0]&0x0F | 0x70
	b[7] = rnd[1]
	b[8] = rnd[2]&0x3F | 0x80
	copy(b[9:], rnd[3:])
	h := fmt.Sprintf("%x", b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
