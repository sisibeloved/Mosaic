// 表格交换适配器 UT（RFC-0017 M2）：httptest 脚本上游驱动两家 provider 的
// 读快照/建副本全链路。腾讯 URL→file_id 解码用 M0 真机数据对定案
// （file_id bNbufpufnTqa ↔ URL 尾 DYk5idWZwdWZuVHFh）。
package extdocs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// txTool 腾讯 tools/call 应答（payload 为结果 JSON 文本）。
func txTool(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "application/json")
	enc, _ := json.Marshal(payload)
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":%s}]}}`, enc)
}

// txRPC 解析上游请求的工具名与参数。
func txRPC(r *http.Request) (tool string, args map[string]any) {
	var req struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	return req.Params.Name, req.Params.Arguments
}

func TestTencentFileIDCandidates(t *testing.T) {
	// M0 真机数据对：URL = "D" + base64(file_id)
	cands := tencentFileIDCandidates("https://docs.qq.com/sheet/DYk5idWZwdWZuVHFh")
	if len(cands) == 0 || cands[0] != "bNbufpufnTqa" {
		t.Fatalf("候选 = %v（应含去前缀解码 bNbufpufnTqa）", cands)
	}
	found := false
	for _, c := range cands {
		if c == "bNbufpufnTqa" {
			found = true
		}
	}
	if !found {
		t.Fatalf("候选缺 M0 实证 file_id: %v", cands)
	}
	// 裸 file_id 直传
	cands = tencentFileIDCandidates("https://docs.qq.com/sheet/bNbufpufnTqa")
	if len(cands) == 0 || cands[0] != "bNbufpufnTqa" {
		t.Fatalf("裸 file_id 候选 = %v", cands)
	}
	for _, bad := range []string{"https://docs.qq.com/sheet/", "https://docs.qq.com/doc/x", "not-a-url"} {
		if c := tencentFileIDCandidates(bad); len(c) != 0 {
			t.Fatalf("%q 应无候选，得 %v", bad, c)
		}
	}
}

func TestTencentReadSheetSnapshot(t *testing.T) {
	m, p, _ := newTencent(t, func(w http.ResponseWriter, r *http.Request) {
		tool, _ := txRPC(r)
		switch tool {
		case "manage.query_file_info":
			txTool(w, `{"file_id":"f123","title":"云端表","type":"sheet","status":"normal"}`)
		case "sheet.get_sheet_info":
			txTool(w, `{"sheets":[
				{"sheet_id":"S0","sheet_name":"工作表1","sheet_type":"worksheet","row_count":10,"col_count":3},
				{"sheet_id":"S1","sheet_name":"智能表","sheet_type":"smartsheet","row_count":5,"col_count":5}
			]}`)
		case "sheet.get_cell_data":
			txTool(w, `{"cells":[
				{"row":0,"col":0,"value_type":"STRING","string_value":"00123"},
				{"row":0,"col":1,"value_type":"NUMBER","number_value":123.456},
				{"row":0,"col":2,"value_type":"BOOL","bool_value":true},
				{"row":1,"col":0,"value_type":"NUMBER","number_value":70,"formula":"=SUM(A1:A2)"},
				{"row":1,"col":1,"value_type":"TIME_STRING","string_value":"2026-09-28"},
				{"row":1,"col":2,"value_type":"ERROR"}
			]}`)
		case "sheet.get_merged_cells":
			txTool(w, `{"merged_cells":["S0$A1:B2","S0$C3:D5"]}`)
		default:
			w.WriteHeader(400)
		}
	})
	if err := m.Save(ProviderTencent, tencentCreds{Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	snap, issues, err := p.ReadSheetSnapshot(context.Background(), "https://docs.qq.com/sheet/DZYtdkVwd1pRTHFh", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Title != "云端表" || len(snap.Sheets) != 1 {
		t.Fatalf("snapshot = %q sheets=%d", snap.Title, len(snap.Sheets))
	}
	sh := snap.Sheets[0]
	if sh.Name != "工作表1" || sh.MergeCount != 2 || sh.Rows != 10 || sh.Cols != 3 {
		t.Fatalf("sheet = %+v", sh)
	}
	type want struct {
		r, c int
		kind string
		text string
		num  float64
		fm   string
	}
	wants := []want{
		{1, 1, CloudCellText, "00123", 0, ""},
		{1, 2, CloudCellNumber, "", 123.456, ""},
		{1, 3, CloudCellBool, "", 0, ""},
		{2, 1, CloudCellFormula, "", 0, "=SUM(A1:A2)"},
		{2, 2, CloudCellDatetime, "2026-09-28T00:00:00Z", 0, ""},
		{2, 3, CloudCellText, "#ERROR!", 0, ""},
	}
	if len(sh.Cells) != len(wants) {
		t.Fatalf("cells = %d（期 %d）", len(sh.Cells), len(wants))
	}
	for i, wt := range wants {
		cc := sh.Cells[i]
		if cc.R != wt.r || cc.C != wt.c || cc.Kind != wt.kind {
			t.Fatalf("cell[%d] = %+v（期 r%dc%d %s）", i, cc, wt.r, wt.c, wt.kind)
		}
		switch wt.kind {
		case CloudCellText:
			if cc.Text != wt.text {
				t.Fatalf("text = %q（期 %q）", cc.Text, wt.text)
			}
		case CloudCellNumber:
			if cc.Number == nil || *cc.Number != wt.num {
				t.Fatalf("number = %v", cc.Number)
			}
		case CloudCellFormula:
			if cc.Formula != wt.fm {
				t.Fatalf("formula = %q（期 %q）", cc.Formula, wt.fm)
			}
		}
	}
	// 报告：smartsheet 丢弃 + 字面错误值转文本 + 日期 UTC 注记
	var dropped, errLit, dateNote bool
	for _, is := range issues {
		if is.Severity == IssueInfo && is.Action == ActionDropped && strings.Contains(is.Feature, "smartsheet") {
			dropped = true
		}
		if is.Feature == "字面错误值" {
			errLit = true
		}
		if is.Feature == "日期时间" {
			dateNote = true
		}
	}
	if !dropped || !errLit || !dateNote {
		t.Fatalf("issues 缺项: %v", issues)
	}
}

func TestTencentCreateSheetCopy(t *testing.T) {
	var mu sync.Mutex
	writes := map[string][]map[string]any{} // sheet_id → 已写格
	m, p, _ := newTencent(t, func(w http.ResponseWriter, r *http.Request) {
		tool, args := txRPC(r)
		switch tool {
		case "manage.create_file":
			txTool(w, `{"file_id":"NEW","title":"t","url":"https://docs.qq.com/sheet/NEWURL","type":"sheet"}`)
		case "sheet.get_sheet_info":
			txTool(w, `{"sheets":[{"sheet_id":"DEF","sheet_name":"工作表1","sheet_type":"worksheet","row_count":100,"col_count":20}]}`)
		case "sheet.rename_sheet", "sheet.add_sheet":
			if tool == "sheet.add_sheet" {
				txTool(w, `{"sheet_id":"S2"}`)
				return
			}
			txTool(w, `{}`)
		case "sheet.set_range_value":
			mu.Lock()
			sid, _ := args["sheet_id"].(string)
			vals, _ := args["values"].([]any)
			for _, v := range vals {
				if vm, ok := v.(map[string]any); ok {
					writes[sid] = append(writes[sid], vm)
				}
			}
			mu.Unlock()
			txTool(w, `{}`)
		case "sheet.get_cell_data": // 回读校验：回显已写格
			mu.Lock()
			sid, _ := args["sheet_id"].(string)
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
			txTool(w, sb.String())
		default:
			w.WriteHeader(400)
		}
	})
	if err := m.Save(ProviderTencent, tencentCreds{Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	n1, b := 9007199254740993.0, true
	wb := ExportWorkbook{Title: "本地表", Sheets: []ExportSheet{
		{Name: "总表", Rows: 10, Cols: 5, Cells: []protocol.SheetCell{
			{R: 1, C: 1, Type: "text", Text: "00123"},
			{R: 2, C: 1, Type: "number", Number: &n1},
			{R: 3, C: 1, Type: "bool", Bool: &b},
			{R: 4, C: 1, Type: "datetime", Text: "2026-09-28T00:00:00Z"},
			{R: 5, C: 1, Type: "formula", Formula: "SUM(A1:A2)"},
		}},
		{Name: "数据B", Rows: 5, Cols: 2, Cells: []protocol.SheetCell{
			{R: 1, C: 1, Type: "number", Number: ptrFloat(7)},
		}},
	}}
	res, err := p.CreateSheetCopy(context.Background(), wb)
	if err != nil {
		t.Fatal(err)
	}
	if res.TargetURL != "https://docs.qq.com/sheet/NEWURL" {
		t.Fatalf("url = %q", res.TargetURL)
	}
	mu.Lock()
	defer mu.Unlock()
	main := writes["DEF"]
	if len(main) != 5 || len(writes["S2"]) != 1 {
		t.Fatalf("writes DEF=%d S2=%d", len(main), len(writes["S2"]))
	}
	byKey := map[float64]map[string]any{}
	for _, c := range main {
		byKey[c["row"].(float64)*100+c["col"].(float64)] = c
	}
	if c := byKey[0]; c["value_type"] != "STRING" || c["string_value"] != "00123" {
		t.Fatalf("A1 = %v", c)
	}
	if c := byKey[100]; c["value_type"] != "STRING" || c["string_value"] != "9007199254740992" { // >15 位 → 文本（float64 实际值 …992）
		t.Fatalf("A2 = %v", c)
	}
	if c := byKey[200]; c["value_type"] != "BOOL" || c["bool_value"] != true {
		t.Fatalf("A3 = %v", c)
	}
	if c := byKey[300]; c["value_type"] != "STRING" || c["string_value"] != "2026-09-28T00:00:00Z" {
		t.Fatalf("A4 = %v", c)
	}
	if c := byKey[400]; c["value_type"] != "FORMULA" || c["formula"] != "=SUM(A1:A2)" {
		t.Fatalf("A5 = %v", c)
	}
	// 回读校验零差异（回显一致）+ 计划类 issue（超精度/日期）
	var digit, date bool
	for _, is := range res.Issues {
		if is.Feature == "超精度数值" {
			digit = true
		}
		if is.Feature == "日期时间" && is.Severity == IssueWarning {
			date = true
		}
		if is.Feature == "回读校验" {
			t.Fatalf("回读不应有差异: %v", is)
		}
	}
	if !digit || !date {
		t.Fatalf("issues 缺项: %v", res.Issues)
	}
}

func ptrFloat(f float64) *float64 { return &f }

// fsWrite JSON 断言辅助。
type capturedValues struct {
	mu      sync.Mutex
	entries []struct {
		rng  string
		rows [][]any
	}
	styles []map[string]any
}

// bySheet 取指定子表已写行（回读 fake 用）。
func (cv *capturedValues) bySheet(sheetID string) [][]any {
	cv.mu.Lock()
	defer cv.mu.Unlock()
	var rows [][]any
	for _, e := range cv.entries {
		if sid, _, ok := strings.Cut(e.rng, "!"); ok && sid == sheetID {
			rows = append(rows, e.rows...)
		}
	}
	return rows
}

func TestFeishuReadSheetSnapshot(t *testing.T) {
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sheets/query"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"sheets":[
				{"sheet_id":"sid1","title":"Sheet1","resource_type":"sheet","grid_properties":{"row_count":2,"column_count":3},"merges":[{"start_row_index":0}]},
				{"sheet_id":"sid2","title":"维度表","resource_type":"bitable"}
			]}}`))
		case strings.Contains(r.URL.Path, "/values/"):
			vo := r.URL.Query().Get("valueRenderOption")
			dto := r.URL.Query().Get("dateTimeRenderOption")
			switch {
			case vo == "UnformattedValue" && dto == "":
				_, _ = w.Write([]byte(`{"code":0,"data":{"revision":1,"valueRange":{"range":"sid1!A1:C2","values":[
					["00123",123.456,true],[46293,70,"=SUM(1,1)"]]}}}`))
			case vo == "UnformattedValue": // dateTimeRenderOption=FormattedString：日期格变串
				_, _ = w.Write([]byte(`{"code":0,"data":{"revision":1,"valueRange":{"range":"sid1!A1:C2","values":[
					["00123",123.456,true],["2026/09/28",70,"=SUM(1,1)"]]}}}`))
			case vo == "Formula":
				_, _ = w.Write([]byte(`{"code":0,"data":{"revision":1,"valueRange":{"range":"sid1!A1:C2","values":[
					["00123",123.456,true],[46293,"=SUM(B1:B1)","=SUM(1,1)"]]}}}`))
			}
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{"spreadsheet":{"title":"云表"}}}`))
		}
	})
	if err := p.mgr.Save(ProviderFeishu, feishuCreds{AppID: "a", AppSecret: "s", AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	snap, issues, err := p.ReadSheetSnapshot(context.Background(), "https://x.feishu.cn/sheets/TOKEN123456", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Title != "云表" || len(snap.Sheets) != 1 {
		t.Fatalf("snap title=%q sheets=%d", snap.Title, len(snap.Sheets))
	}
	sh := snap.Sheets[0]
	if sh.Name != "Sheet1" || sh.MergeCount != 1 {
		t.Fatalf("sheet = %+v", sh)
	}
	// (2,2) 公式：FM "=SUM(B1:B1)" ≠ U 70 → 公式格
	// (2,3) 文本转义格：FM "==SUM(1,1)" 与 U 同串 → 文本（M0 消歧法）
	kinds := map[int]string{}
	for _, c := range sh.Cells {
		kinds[c.R*100+c.C] = c.Kind
	}
	if kinds[101] != CloudCellText || kinds[102] != CloudCellNumber || kinds[103] != CloudCellBool ||
		kinds[201] != CloudCellDatetime || kinds[202] != CloudCellFormula || kinds[203] != CloudCellText {
		t.Fatalf("kinds = %v", kinds)
	}
	for _, c := range sh.Cells {
		if c.R == 2 && c.C == 1 && c.Text != "2026-09-28T00:00:00Z" {
			t.Fatalf("datetime = %q", c.Text)
		}
		if c.R == 2 && c.C == 2 && c.Formula != "=SUM(B1:B1)" {
			t.Fatalf("formula = %q", c.Formula)
		}
		if c.R == 2 && c.C == 3 && c.Text != "=SUM(1,1)" {
			t.Fatalf("escaped text = %q", c.Text)
		}
	}
	var dropped bool
	for _, is := range issues {
		if is.Action == ActionDropped && strings.Contains(is.Feature, "bitable") {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("issues 缺 bitable 丢弃: %v", issues)
	}
}

func TestFeishuCreateSheetCopy(t *testing.T) {
	cv := &capturedValues{}
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sheets/v3/spreadsheets"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"spreadsheet":{"spreadsheet_token":"NEW","url":"https://x.feishu.cn/sheets/NEW"}}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheets/query"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"sheets":[{"sheet_id":"def","title":"Sheet1","resource_type":"sheet","grid_properties":{"row_count":10,"column_count":10}}]}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sheets_batch_update"):
			var body struct {
				Requests []map[string]map[string]any `json:"requests"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Requests) > 0 {
				if _, ok := body.Requests[0]["addSheet"]; ok {
					_, _ = w.Write([]byte(`{"code":0,"data":{"replies":[{"addSheet":{"properties":{"sheetId":"s2","title":"b"}}}]}}`))
					return
				}
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"replies":[]}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/values"):
			var body struct {
				ValueRange struct {
					Range  string  `json:"range"`
					Values [][]any `json:"values"`
				} `json:"valueRange"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			cv.mu.Lock()
			cv.entries = append(cv.entries, struct {
				rng  string
				rows [][]any
			}{body.ValueRange.Range, body.ValueRange.Values})
			cv.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"revision":2,"updatedCells":1}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/style"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			cv.mu.Lock()
			cv.styles = append(cv.styles, body)
			cv.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"updates":{"revision":3}}}`))
		case strings.Contains(r.URL.Path, "/values/"): // 回读校验：按子表回显
			vo := r.URL.Query().Get("valueRenderOption")
			seg := r.URL.Path[strings.LastIndex(r.URL.Path, "/values/")+len("/values/"):]
			sid, _, _ := strings.Cut(seg, "!")
			rows := cv.bySheet(sid)
			var echoed [][]any
			for _, g := range rows {
				var row []any
				for _, cell := range g {
					row = append(row, feishuEcho(cell, vo, 0, 0))
				}
				echoed = append(echoed, row)
			}
			enc, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"revision": 4,
				"valueRange": map[string]any{"range": seg, "values": echoed}}})
			_, _ = w.Write(enc)
		default:
			t.Logf("unhandled: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	if err := p.mgr.Save(ProviderFeishu, feishuCreds{AppID: "a", AppSecret: "s", AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	n1, b := 9007199254740993.0, true
	wb := ExportWorkbook{Title: "本地表", Sheets: []ExportSheet{
		{Name: "总表", Rows: 5, Cols: 5, Cells: []protocol.SheetCell{
			{R: 1, C: 1, Type: "text", Text: "TRUE"}, // 防强转 → text 对象
			{R: 1, C: 2, Type: "text", Text: "=abc"}, // 防公式化 → text 对象
			{R: 1, C: 3, Type: "text", Text: "普通"},   // 纯字符串
			{R: 2, C: 1, Type: "number", Number: ptrFloat(123.456)},
			{R: 2, C: 2, Type: "number", Number: &n1},                    // >15 位 → text 对象
			{R: 3, C: 1, Type: "bool", Bool: &b},                         // =TRUE()
			{R: 4, C: 1, Type: "datetime", Text: "2026-09-28T00:00:00Z"}, // 序列+formatter
			{R: 5, C: 1, Type: "formula", Formula: "A1&B1"},              // formula 对象
		}},
		{Name: "数据B", Rows: 2, Cols: 2, Cells: []protocol.SheetCell{
			{R: 1, C: 1, Type: "number", Number: ptrFloat(7)},
		}},
	}}
	res, err := p.CreateSheetCopy(context.Background(), wb)
	if err != nil {
		t.Fatal(err)
	}
	if res.TargetURL != "https://x.feishu.cn/sheets/NEW" {
		t.Fatalf("url = %q", res.TargetURL)
	}
	cv.mu.Lock()
	defer cv.mu.Unlock()
	if len(cv.entries) != 2 || !strings.HasPrefix(cv.entries[0].rng, "def!") || !strings.Contains(cv.entries[0].rng, "!") {
		t.Fatalf("entries = %+v", cv.entries)
	}
	g := cv.entries[0].rows
	if len(g) != 5 {
		t.Fatalf("sheet1 rows = %d", len(g))
	}
	// 逐格形态断言（1-based → 网格 (r-1, c-1)）
	if o, ok := g[0][0].(map[string]any); !ok || o["type"] != "text" || o["text"] != "TRUE" {
		t.Fatalf("A1 = %v", g[0][0])
	}
	if o, ok := g[0][1].(map[string]any); !ok || o["type"] != "text" || o["text"] != "=abc" {
		t.Fatalf("B1 = %v", g[0][1])
	}
	if s, ok := g[0][2].(string); !ok || s != "普通" {
		t.Fatalf("C1 = %v", g[0][2])
	}
	if f, ok := g[1][0].(float64); !ok || f != 123.456 {
		t.Fatalf("A2 = %v", g[1][0])
	}
	if o, ok := g[1][1].(map[string]any); !ok || o["type"] != "text" || o["text"] != "9007199254740992" {
		t.Fatalf("B2 = %v（float64 实际值 …992）", g[1][1])
	}
	if o, ok := g[2][0].(map[string]any); !ok || o["type"] != "formula" || o["text"] != "=TRUE()" {
		t.Fatalf("A3 = %v", g[2][0])
	}
	if f, ok := g[3][0].(float64); !ok || f != 46293 {
		t.Fatalf("A4 = %v（期序列 46293）", g[3][0])
	}
	if o, ok := g[4][0].(map[string]any); !ok || o["type"] != "formula" || o["text"] != "=A1&B1" {
		t.Fatalf("A5 = %v", g[4][0])
	}
	if !strings.HasPrefix(cv.entries[1].rng, "s2!") {
		t.Fatalf("sheet2 range = %q", cv.entries[1].rng)
	}
	if len(cv.styles) != 1 {
		t.Fatalf("styles = %v", cv.styles)
	}
	var boolInfo, digitWarn bool
	for _, is := range res.Issues {
		if is.Feature == "布尔值" {
			boolInfo = true
		}
		if is.Feature == "超精度数值" {
			digitWarn = true
		}
		if is.Feature == "回读校验" {
			t.Fatalf("回读不应有差异: %v", is)
		}
	}
	if !boolInfo || !digitWarn {
		t.Fatalf("issues 缺项: %v", res.Issues)
	}
}

// feishuEcho 回读 fake：按写入形态生成 Unformatted / Formula 两模式的值。
func feishuEcho(cell any, vo string, r, c int) any {
	if cell == nil {
		return nil
	}
	switch v := cell.(type) {
	case string, float64, bool:
		return v
	case map[string]any:
		t, _ := v["type"].(string)
		text, _ := v["text"].(string)
		switch t {
		case "text":
			return text
		case "formula":
			if vo == "Formula" {
				return text
			}
			if text == "=TRUE()" {
				return true
			}
			if text == "=FALSE()" {
				return false
			}
			return float64(0) // 普通公式：U 模式值与本测试断言无关
		}
	}
	return nil
}

var _ = httptest.NewServer

// ---- 多维表格（Base）只读导入（RFC-0017 §13） ----

func TestFeishuResourceToken(t *testing.T) {
	for _, c := range []struct {
		url  string
		kind string
		tok  string
		msg  string
	}{
		{"https://x.feishu.cn/sheets/TOKEN123456", "sheet", "TOKEN123456", ""},
		{"https://x.feishu.cn/sheet/TOKEN123456/edit", "sheet", "TOKEN123456", ""},
		{"https://icn.feishu.cn/base/APPTOKEN12?from=wiki", "base", "APPTOKEN12", ""},
		{"https://x.feishu.cn/wiki/XxxxxxxxYy", "", "", "知识库"},
		{"https://x.feishu.cn/docx/AbCdEfGh1234", "", "", "不是飞书电子表格或多维表格"},
		{"https://x.feishu.cn/sheets/short", "", "", "不是飞书电子表格或多维表格"},
		{"https://x.feishu.cn/base/short", "", "", "不是飞书电子表格或多维表格"},
	} {
		kind, tok, err := feishuResourceToken(c.url)
		if c.msg == "" {
			if err != nil || kind != c.kind || tok != c.tok {
				t.Fatalf("%s → (%q,%q,%v)", c.url, kind, tok, err)
			}
			continue
		}
		var ue *UpstreamError
		if !errors.As(err, &ue) || ue.Code != "bad_sheet_link" || !strings.Contains(ue.Message, c.msg) {
			t.Fatalf("%s → err=%v（期望 bad_sheet_link 含 %q）", c.url, err, c.msg)
		}
	}
}

func TestFeishuReadBaseSnapshot(t *testing.T) {
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/apps/APPTOKEN12") && r.URL.Query().Get("page_size") == "":
			_, _ = w.Write([]byte(`{"code":0,"data":{"app":{"name":"管家盘点"}}}`))
		case strings.HasSuffix(r.URL.Path, "/tables"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[
				{"table_id":"tblA","name":"数据表A"},{"table_id":"tblB","name":"另一张表"}],
				"has_more":false}}`))
		case strings.HasSuffix(r.URL.Path, "/tables/tblA/fields"):
			if r.URL.Query().Get("page_token") == "" {
				_, _ = w.Write([]byte(`{"code":0,"data":{"items":[
					{"field_name":"名称","type":1},{"field_name":"日期","type":5},
					{"field_name":"分数","type":2},{"field_name":"完成","type":7},
					{"field_name":"负责人","type":11},{"field_name":"多选","type":4}],
					"has_more":true,"page_token":"f2"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"field_name":"链接","type":15}],"has_more":false}}`))
		case strings.HasSuffix(r.URL.Path, "/tables/tblA/records"):
			if r.URL.Query().Get("page_token") == "" {
				_, _ = w.Write([]byte(`{"code":0,"data":{"items":[
					{"record_id":"r1","fields":{
						"名称":[{"type":1,"text":"管家一号"}],
						"日期":1790000000000,
						"分数":93.5,
						"完成":true,
						"负责人":[{"name":"张三"},{"name":"李四"}],
						"多选":["高","中"],
						"链接":{"link":"https://x.example","text":"站点"},
						"不存在字段":"zzz"}}],
					"has_more":true,"page_token":"r2"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[
				{"record_id":"r2","fields":{"名称":"纯文本行","完成":false}}],"has_more":false}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
	if err := p.mgr.Save(ProviderFeishu, feishuCreds{AppID: "a", AppSecret: "s", AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	snap, issues, err := p.ReadSheetSnapshot(context.Background(), "https://icn.feishu.cn/base/APPTOKEN12", []string{"数据表A"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Title != "管家盘点" || len(snap.Sheets) != 1 {
		t.Fatalf("snap title=%q sheets=%d", snap.Title, len(snap.Sheets))
	}
	sh := snap.Sheets[0]
	if sh.Name != "数据表A" || sh.SheetID != "tblA" || sh.Rows != 3 || sh.Cols != 7 {
		t.Fatalf("sheet = %+v", sh)
	}
	wantDate := time.UnixMilli(1790000000000).UTC().Format(time.RFC3339)
	type key struct{ r, c int }
	got := map[key]CloudCell{}
	for _, c := range sh.Cells {
		got[key{c.R, c.C}] = c
	}
	assertCell := func(r, cc int, kind, text string, num float64, b bool) {
		t.Helper()
		c, ok := got[key{r, cc}]
		if !ok {
			t.Fatalf("缺格 (%d,%d)", r, cc)
		}
		if c.Kind != kind {
			t.Fatalf("(%d,%d) kind=%q 期望 %q", r, cc, c.Kind, kind)
		}
		switch kind {
		case CloudCellText, CloudCellDatetime:
			if c.Text != text {
				t.Fatalf("(%d,%d) text=%q 期望 %q", r, cc, c.Text, text)
			}
		case CloudCellNumber:
			if c.Number == nil || *c.Number != num {
				t.Fatalf("(%d,%d) number=%v 期望 %v", r, cc, c.Number, num)
			}
		case CloudCellBool:
			if c.Bool == nil || *c.Bool != b {
				t.Fatalf("(%d,%d) bool=%v 期望 %v", r, cc, c.Bool, b)
			}
		}
	}
	// 首行 = 字段列头
	for i, name := range []string{"名称", "日期", "分数", "完成", "负责人", "多选", "链接"} {
		assertCell(1, i+1, CloudCellText, name, 0, false)
	}
	// 记录行：形状映射矩阵
	assertCell(2, 1, CloudCellText, "管家一号", 0, false) // 文本分段数组 → join
	assertCell(2, 2, CloudCellDatetime, wantDate, 0, false)
	assertCell(2, 3, CloudCellNumber, "", 93.5, false)
	assertCell(2, 4, CloudCellBool, "", 0, true)
	assertCell(2, 5, CloudCellText, "张三、李四", 0, false) // 人员数组 → name
	assertCell(2, 6, CloudCellText, "高、中", 0, false)   // 多选字符串数组
	assertCell(2, 7, CloudCellText, "站点", 0, false)    // 超链接对象 → text
	// 第二页记录 + 空值省略（rec2 只有 名称/完成）
	assertCell(3, 1, CloudCellText, "纯文本行", 0, false)
	assertCell(3, 4, CloudCellBool, "", 0, false)
	if _, exists := got[key{3, 2}]; exists {
		t.Fatalf("rec2 缺省字段不应有格 (3,2)")
	}
	var flatten bool
	for _, is := range issues {
		if is.Feature == "多维表格展平" && is.Action == ActionConverted {
			flatten = true
		}
	}
	if !flatten {
		t.Fatalf("issues 缺展平注记: %v", issues)
	}
}

func TestFeishuOpenSheetTargetRejectsBase(t *testing.T) {
	_, p := newFeishu(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := p.mgr.Save(ProviderFeishu, feishuCreds{AppID: "a", AppSecret: "s", AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err := p.OpenSheetTarget(context.Background(), "https://x.feishu.cn/base/APPTOKEN12")
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Code != "bad_sheet_target" {
		t.Fatalf("err = %v（期望 bad_sheet_target）", err)
	}
}
