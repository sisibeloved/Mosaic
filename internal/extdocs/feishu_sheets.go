// 飞书表格交换适配（RFC-0017 M2，能力依据 §10.6 M0 实测档案）：Sheets v3
// 建表/查表 + v2 values 读写 + style。读侧三读合并（Unformatted×2 判日期格
// + Formula 取原文）；写侧类型化对象防强转（"TRUE"/"=" 开头文本 → text 对象、
// 布尔 → =TRUE() 公式、日期 → 序列数+formatter 两步写）。range 一律全形
// A1:B2（裸单格上游 90202 拒绝）。
package extdocs

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/sheet"
)

// 飞书表格交换常量。
const (
	fsSheetReadRows     = 200     // 每次读行窗（200×200=4 万格，响应可控）
	fsSheetReadCapBytes = 8 << 20 // 区域读响应上限
	fsSheetWriteCells   = 2000    // 每次写格数（上游单写 ≤5000 行 ×100 列）
	fsSheetWriteColSpan = 100     // 每次写列窗上限
	fsTitleMaxRunes     = 255     // v3 建表标题上限
	fsDateTimeStyleMax  = 200     // 日期 formatter 逐格设置上限（超出汇总跳过）
	fsVerifyIssueMax    = 20      // 回读差异逐格报告上限
)

// SheetCapabilities M0 实证：公式原文读写 ✓、样式写生效但读接口缺失 ✗、
// 布尔字面不可写（=TRUE() 公式形态）、类型化日期两步写 ✓、数值 float64。
func (p *FeishuProvider) SheetCapabilities() SheetCaps {
	return SheetCaps{
		ReadFormulas:  true,
		WriteFormulas: true,
		ReadStyles:    false,
		WriteStyles:   true,
		WriteBool:     false,
		WriteDatetime: true,
		NumberFloat64: true,
	}
}

// feishuResourceKind 链接指向的飞书资源类型。
const (
	fsKindSheet = "sheet" // 电子表格（/sheets/）
	fsKindBase  = "base"  // 多维表格（/base/）
)

// feishuResourceToken URL → (kind, token)。电子表格路径段即 spreadsheet_token
// （M0 实证）；多维表格 /base/ 段即 app_token（bitable v1）。
func feishuResourceToken(rawURL string) (string, string, error) {
	u, err := parseURL(rawURL)
	if err != nil {
		return "", "", &UpstreamError{Code: "bad_sheet_link", Message: "链接无法解析"}
	}
	if strings.HasPrefix(u.Path, "/wiki/") {
		return "", "", &UpstreamError{Code: "bad_sheet_link",
			Message: "知识库（/wiki/）链接请先在飞书里打开原表格，从地址栏复制 /sheets/ 或 /base/ 链接"}
	}
	seg, kind := u.Path, ""
	for _, p := range []struct{ prefix, kind string }{
		{"/sheets/", fsKindSheet}, {"/sheet/", fsKindSheet}, {"/base/", fsKindBase},
	} {
		if strings.HasPrefix(seg, p.prefix) {
			seg, kind = strings.TrimPrefix(seg, p.prefix), p.kind
			break
		}
	}
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	seg = strings.TrimSpace(seg)
	if kind == "" || len(seg) < 8 {
		return "", "", &UpstreamError{Code: "bad_sheet_link",
			Message: "链接不是飞书电子表格或多维表格（需 *.feishu.cn/sheets/ 或 /base/ 链接）"}
	}
	return kind, seg, nil
}

// ---- 上游响应形状 ----

type fsSheetsQueryResult struct {
	Data struct {
		Sheets []struct {
			SheetID        string `json:"sheet_id"`
			Title          string `json:"title"`
			Index          int    `json:"index"`
			ResourceType   string `json:"resource_type"` // sheet | bitable | #UNSUPPORTED_TYPE
			GridProperties struct {
				RowCount    int `json:"row_count"`
				ColumnCount int `json:"column_count"`
			} `json:"grid_properties"`
			Merges []struct {
				StartRowIndex    int `json:"start_row_index"`
				EndRowIndex      int `json:"end_row_index"`
				StartColumnIndex int `json:"start_column_index"`
				EndColumnIndex   int `json:"end_column_index"`
			} `json:"merges"`
		} `json:"sheets"`
	} `json:"data"`
}

type fsValuesResult struct {
	Data struct {
		Revision   int64 `json:"revision"`
		ValueRange struct {
			Range  string  `json:"range"`
			Values [][]any `json:"values"`
		} `json:"valueRange"`
	} `json:"data"`
}

type fsCreateResult struct {
	Data struct {
		Spreadsheet struct {
			SpreadsheetToken string `json:"spreadsheet_token"`
			Title            string `json:"title"`
			URL              string `json:"url"`
		} `json:"spreadsheet"`
	} `json:"data"`
}

type fsMetaResult struct {
	Data struct {
		Spreadsheet struct {
			Title string `json:"title"`
		} `json:"spreadsheet"`
	} `json:"data"`
}

type fsBatchUpdateResult struct {
	Data struct {
		Replies []struct {
			AddSheet *struct {
				Properties struct {
					SheetID string `json:"sheetId"`
					Title   string `json:"title"`
				} `json:"properties"`
			} `json:"addSheet"`
		} `json:"replies"`
	} `json:"data"`
}

// ReadSheetSnapshot 读整簿快照。电子表格走 sheets 三读合并；多维表格
// （/base/）走 bitable v1 只读展平。
func (p *FeishuProvider) ReadSheetSnapshot(ctx context.Context, rawURL string, sheetNames []string) (*CloudSnapshot, []TransferIssue, error) {
	token, err := p.ensureFresh(ctx)
	if err != nil {
		return nil, nil, err
	}
	kind, resToken, err := feishuResourceToken(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if kind == fsKindBase {
		return p.readBaseSnapshot(ctx, token, resToken, sheetNames)
	}
	spToken := resToken
	title := p.feishuSheetTitle(ctx, token, spToken)
	var query fsSheetsQueryResult
	if err := p.getJSON(ctx, token, "/open-apis/sheets/v3/spreadsheets/"+spToken+"/sheets/query", &query); err != nil {
		return nil, nil, err
	}
	if len(query.Data.Sheets) == 0 {
		return nil, nil, &UpstreamError{Code: "empty_sheet", Message: "该表格没有工作表"}
	}
	want := map[string]bool{}
	for _, n := range sheetNames {
		want[n] = true
	}
	snap := &CloudSnapshot{Title: title}
	var issues []TransferIssue
	for _, sh := range query.Data.Sheets {
		if sh.ResourceType != "sheet" {
			issues = append(issues, TransferIssue{
				Sheet: sh.Title, Feature: "子表类型 " + sh.ResourceType,
				Severity: IssueInfo, Action: ActionDropped,
				Reason: "非普通工作表（多维表格/不支持类型），M2 不迁移",
			})
			continue
		}
		if len(want) > 0 && !want[sh.Title] {
			continue
		}
		rows, cols := sh.GridProperties.RowCount, sh.GridProperties.ColumnCount
		if rows > sheet.MaxRows || cols > sheet.MaxCols {
			issues = append(issues, TransferIssue{
				Sheet: sh.Title, Range: fmt.Sprintf("读区截断为 %d×%d", sheet.MaxRows, sheet.MaxCols),
				Feature: "网格范围", Severity: IssueWarning, Action: ActionTruncated,
				Reason: fmt.Sprintf("云端网格 %d×%d 超出本地上限", rows, cols),
			})
			rows, cols = min(rows, sheet.MaxRows), min(cols, sheet.MaxCols)
		}
		cells, cellIssues, err := p.readFeishuCells(ctx, token, spToken, sh.SheetID, rows, cols, sh.Title)
		if err != nil {
			return nil, nil, err
		}
		issues = append(issues, cellIssues...)
		snap.Sheets = append(snap.Sheets, CloudSheet{
			SheetID: sh.SheetID, Name: sh.Title, Rows: rows, Cols: cols,
			MergeCount: len(sh.Merges), Cells: cells,
		})
	}
	if len(snap.Sheets) == 0 {
		return nil, nil, &UpstreamError{Code: "no_worksheet", Message: "没有可导入的普通工作表（全部被过滤或非 worksheet）"}
	}
	return snap, issues, nil
}

// feishuSheetTitle 表格标题尽力获取（v3 元信息；失败回退占位）。
func (p *FeishuProvider) feishuSheetTitle(ctx context.Context, token, spToken string) string {
	var meta fsMetaResult
	if err := p.getJSON(ctx, token, "/open-apis/sheets/v3/spreadsheets/"+spToken, &meta); err != nil {
		return "飞书表格"
	}
	if meta.Data.Spreadsheet.Title != "" {
		return meta.Data.Spreadsheet.Title
	}
	return "飞书表格"
}

// ---- 多维表格（Base）只读导入 ----

// bitable 读侧限额与字段类型（RFC-0017 §13）。
const (
	fsBaseTablesPage  = 50 // 数据表分页
	fsBaseTablesMax   = 50 // 数据表数上限（超出截断报告）
	fsBaseFieldsPage  = 100
	fsBaseRecordsPage = 500 // 记录分页（接口上限）
	fsBaseDateField   = 5   // dateTime 字段类型（值为 ms 纪元数）
)

type fsBaseAppResult struct {
	Data struct {
		App struct {
			Name string `json:"name"`
		} `json:"app"`
	} `json:"data"`
}

type fsBaseTable struct {
	TableID string `json:"table_id"`
	Name    string `json:"name"`
}

type fsBaseTablesResult struct {
	Data struct {
		Items     []fsBaseTable `json:"items"`
		PageToken string        `json:"page_token"`
		HasMore   bool          `json:"has_more"`
	} `json:"data"`
}

type fsBaseField struct {
	FieldName string `json:"field_name"`
	Type      int    `json:"type"`
}

type fsBaseFieldsResult struct {
	Data struct {
		Items     []fsBaseField `json:"items"`
		PageToken string        `json:"page_token"`
		HasMore   bool          `json:"has_more"`
	} `json:"data"`
}

type fsBaseRecordsResult struct {
	Data struct {
		Items []struct {
			Fields map[string]any `json:"fields"`
		} `json:"items"`
		PageToken string `json:"page_token"`
		HasMore   bool   `json:"has_more"`
	} `json:"data"`
}

// readBaseSnapshot 多维表格 → 快照：数据表=工作表、字段=首行列头、记录=数据行。
// 只读展平：富字段（人员/附件/单多选/超链接）取文本表示，日期字段按 ms 纪元
// 转 UTC。写回多维表格不支持（bad_sheet_target）。
func (p *FeishuProvider) readBaseSnapshot(ctx context.Context, token, appToken string, tableNames []string) (*CloudSnapshot, []TransferIssue, error) {
	title := "飞书多维表格"
	var app fsBaseAppResult
	if err := p.getJSON(ctx, token, "/open-apis/bitable/v1/apps/"+appToken, &app); err == nil && app.Data.App.Name != "" {
		title = app.Data.App.Name
	}
	var tables []fsBaseTable
	page, tablesTrunc := "", false
	for {
		path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables?page_size=%d", appToken, fsBaseTablesPage)
		if page != "" {
			path += "&page_token=" + url.QueryEscape(page)
		}
		var lt fsBaseTablesResult
		if err := p.getJSON(ctx, token, path, &lt); err != nil {
			return nil, nil, err
		}
		tables = append(tables, lt.Data.Items...)
		if !lt.Data.HasMore || lt.Data.PageToken == "" {
			break
		}
		if len(tables) >= fsBaseTablesMax {
			tablesTrunc = true
			break
		}
		page = lt.Data.PageToken
	}
	want := map[string]bool{}
	for _, n := range tableNames {
		want[n] = true
	}
	snap := &CloudSnapshot{Title: title}
	var issues []TransferIssue
	if tablesTrunc {
		issues = append(issues, TransferIssue{
			Range: "数据表列表", Feature: "多维表格展平", Severity: IssueWarning, Action: ActionTruncated,
			Reason: fmt.Sprintf("数据表超过 %d 张，超出部分不导入", fsBaseTablesMax),
		})
	}
	richCount := 0
	for _, tb := range tables {
		if len(want) > 0 && !want[tb.Name] {
			continue
		}
		fields, err := p.readBaseFields(ctx, token, appToken, tb.TableID)
		if err != nil {
			return nil, nil, err
		}
		if len(fields) == 0 {
			issues = append(issues, TransferIssue{
				Sheet: tb.Name, Feature: "空数据表", Severity: IssueInfo, Action: ActionDropped,
				Reason: "没有字段，跳过",
			})
			continue
		}
		if len(fields) > sheet.MaxCols {
			issues = append(issues, TransferIssue{
				Sheet: tb.Name, Feature: "多维表格展平", Severity: IssueWarning, Action: ActionTruncated,
				Reason: fmt.Sprintf("字段超过 %d 列，超出部分截断", sheet.MaxCols),
			})
			fields = fields[:sheet.MaxCols]
		}
		records, recTrunc, err := p.readBaseRecords(ctx, token, appToken, tb.TableID, sheet.MaxRows-1)
		if err != nil {
			return nil, nil, err
		}
		if recTrunc {
			issues = append(issues, TransferIssue{
				Sheet: tb.Name, Feature: "多维表格展平", Severity: IssueWarning, Action: ActionTruncated,
				Reason: fmt.Sprintf("记录超过 %d 行，超出部分截断", sheet.MaxRows-1),
			})
		}
		fieldType := map[string]int{}
		colOf := map[string]int{}
		cells := make([]CloudCell, 0, len(records)*len(fields)+len(fields))
		for i, f := range fields {
			cells = append(cells, CloudCell{R: 1, C: i + 1, Kind: CloudCellText, Text: f.FieldName})
			fieldType[f.FieldName] = f.Type
			colOf[f.FieldName] = i + 1
		}
		for ri, rec := range records {
			for name, v := range rec {
				if v == nil {
					continue
				}
				c, ok := colOf[name]
				if !ok {
					continue
				}
				switch v.(type) {
				case []any, map[string]any:
					richCount++
				}
				cc := feishuBaseValueToCloud(v, fieldType[name])
				if cc.Kind == CloudCellText && cc.Text == "" {
					continue
				}
				cc.R, cc.C = ri+2, c
				cells = append(cells, cc)
			}
		}
		snap.Sheets = append(snap.Sheets, CloudSheet{
			SheetID: tb.TableID, Name: tb.Name, Rows: 1 + len(records), Cols: len(fields), Cells: cells,
		})
	}
	if len(snap.Sheets) == 0 {
		return nil, nil, &UpstreamError{Code: "no_worksheet", Message: "没有可导入的数据表（全部被过滤、为空或截断）"}
	}
	if richCount > 0 {
		issues = append(issues, TransferIssue{
			Feature: "多维表格展平", Severity: IssueInfo, Action: ActionConverted,
			Reason: fmt.Sprintf("%d 格富字段（人员/附件/单多选/超链接等）取文本表示；字段为列头、记录为数据行", richCount),
		})
	}
	return snap, issues, nil
}

// readBaseFields 拉全字段（列序即字段序；>MaxCols+1 即止——足够判定截断）。
func (p *FeishuProvider) readBaseFields(ctx context.Context, token, appToken, tableID string) ([]fsBaseField, error) {
	var fields []fsBaseField
	page := ""
	for {
		path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/fields?page_size=%d", appToken, tableID, fsBaseFieldsPage)
		if page != "" {
			path += "&page_token=" + url.QueryEscape(page)
		}
		var fr fsBaseFieldsResult
		if err := p.getJSON(ctx, token, path, &fr); err != nil {
			return nil, err
		}
		fields = append(fields, fr.Data.Items...)
		if !fr.Data.HasMore || fr.Data.PageToken == "" || len(fields) > sheet.MaxCols {
			return fields, nil
		}
		page = fr.Data.PageToken
	}
}

// readBaseRecords 分页拉记录，limit 处截断（more = 还有未拉取记录）。
func (p *FeishuProvider) readBaseRecords(ctx context.Context, token, appToken, tableID string, limit int) ([]map[string]any, bool, error) {
	var records []map[string]any
	page := ""
	for {
		path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records?page_size=%d", appToken, tableID, fsBaseRecordsPage)
		if page != "" {
			path += "&page_token=" + url.QueryEscape(page)
		}
		var rr fsBaseRecordsResult
		if err := p.getJSON(ctx, token, path, &rr); err != nil {
			return nil, false, err
		}
		for _, it := range rr.Data.Items {
			records = append(records, it.Fields)
		}
		if !rr.Data.HasMore || rr.Data.PageToken == "" {
			return records, false, nil
		}
		if len(records) >= limit {
			return records[:limit], true, nil
		}
		page = rr.Data.PageToken
	}
}

// feishuBaseValueToCloud bitable 字段值 → 中性格：按 JSON 形状映射，日期字段
// （type 5）值为 ms 纪元数 → UTC 日期时间。
func feishuBaseValueToCloud(v any, fieldType int) CloudCell {
	switch x := v.(type) {
	case bool:
		return CloudCell{Kind: CloudCellBool, Bool: &x}
	case float64:
		if fieldType == fsBaseDateField {
			return CloudCell{Kind: CloudCellDatetime,
				Text: time.UnixMilli(int64(x)).UTC().Format(time.RFC3339)}
		}
		return CloudCell{Kind: CloudCellNumber, Number: &x}
	case string:
		return CloudCell{Kind: CloudCellText, Text: x}
	case []any:
		return CloudCell{Kind: CloudCellText, Text: feishuBaseJoin(x)}
	case map[string]any:
		return CloudCell{Kind: CloudCellText, Text: feishuBaseScalarText(x)}
	default:
		return CloudCell{Kind: CloudCellText, Text: fmt.Sprint(x)}
	}
}

// feishuBaseScalarText 富字段单元素取文本（text/name/link 键优先，不落 JSON）。
func feishuBaseScalarText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case map[string]any:
		for _, k := range []string{"text", "name", "en_name", "link"} {
			if s, ok := x[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	case []any:
		return feishuBaseJoin(x)
	default:
		return fmt.Sprint(x)
	}
}

// feishuBaseJoin 多值字段（多选/人员/附件）以「、」连接。
func feishuBaseJoin(xs []any) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		if s := feishuBaseScalarText(x); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "、")
}

// readFeishuCells 三读合并分块读：
//   - readU（UnformattedValue，日期渲染缺省=序列数）→ 值
//   - readD（UnformattedValue + dateTimeRenderOption=FormattedString）→ 日期格
//     变字符串（数值格不变）——「U 是数 && D 是串」即日期格（M0 判型法）
//   - readFM（Formula）→ 公式原文（"=..." 且与 U 值不同 = 公式格，M0 文本
//     转义格两模式同串的消歧）
func (p *FeishuProvider) readFeishuCells(ctx context.Context, token, spToken, sheetID string, rows, cols int, sheetName string) ([]CloudCell, []TransferIssue, error) {
	cells := make([]CloudCell, 0)
	dateCount := 0
	for r0 := 0; r0 < rows; r0 += fsSheetReadRows {
		rN := min(r0+fsSheetReadRows, rows)
		rng := fmt.Sprintf("%s!%s%d:%s%d", sheetID, sheet.ColName(1), r0+1, sheet.ColName(cols), rN)
		var ru, rd, rf fsValuesResult
		q := url.Values{}
		q.Set("valueRenderOption", "UnformattedValue")
		if err := p.getJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/values/"+rng+"?"+q.Encode(), &ru); err != nil {
			return nil, nil, err
		}
		q.Set("dateTimeRenderOption", "FormattedString")
		if err := p.getJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/values/"+rng+"?"+q.Encode(), &rd); err != nil {
			return nil, nil, err
		}
		q = url.Values{}
		q.Set("valueRenderOption", "Formula")
		if err := p.getJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/values/"+rng+"?"+q.Encode(), &rf); err != nil {
			return nil, nil, err
		}
		for i, row := range ru.Data.ValueRange.Values {
			for j, uv := range row {
				if uv == nil {
					continue
				}
				r, c := r0+i+1, j+1
				if c > cols {
					continue
				}
				fmv := at(rf.Data.ValueRange.Values, i, j)
				dv := at(rd.Data.ValueRange.Values, i, j)
				cc, isDate := feishuCellToCloud(uv, dv, fmv)
				if isDate {
					dateCount++
				}
				cc.R, cc.C = r, c
				cells = append(cells, cc)
			}
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].R != cells[j].R {
			return cells[i].R < cells[j].R
		}
		return cells[i].C < cells[j].C
	})
	var issues []TransferIssue
	if dateCount > 0 {
		issues = append(issues, TransferIssue{
			Sheet: sheetName, Feature: "日期时间", Severity: IssueInfo, Action: ActionKept,
			Reason: fmt.Sprintf("%d 格日期按 1900 系统序列数解析、UTC 记录（云端无时区信息）", dateCount),
		})
	}
	return cells, issues, nil
}

// feishuCellToCloud 单格映射。isDate 标记日期格（供计数）。
func feishuCellToCloud(uv, dv, fmv any) (CloudCell, bool) {
	cc := CloudCell{}
	// 公式格判定：Formula 模式为 "=..." 字符串且与 Unformatted 值不同
	//（{"type":"text"} 转义格两模式同串——M0 消歧）。
	if s, ok := fmv.(string); ok && strings.HasPrefix(s, "=") && fmt.Sprint(uv) != s {
		cc.Kind, cc.Formula = CloudCellFormula, s
		return cc, false
	}
	switch v := uv.(type) {
	case bool:
		cc.Kind, cc.Bool = CloudCellBool, &v
		return cc, false
	case float64:
		// 日期格：U 是数、D（FormattedString）是串
		if _, ok := dv.(string); ok {
			cc.Kind, cc.Text = CloudCellDatetime, excelSerialToTime(v).Format("2006-01-02T15:04:05Z07:00")
			return cc, true
		}
		cc.Kind, cc.Number = CloudCellNumber, &v
		return cc, false
	case string:
		cc.Kind, cc.Text = CloudCellText, v
		return cc, false
	default:
		cc.Kind, cc.Text = CloudCellText, fmt.Sprint(uv)
		return cc, false
	}
}

// at 二维数组越界安全取值。
func at(rows [][]any, i, j int) any {
	if i < 0 || i >= len(rows) {
		return nil
	}
	if j < 0 || j >= len(rows[i]) {
		return nil
	}
	return rows[i][j]
}

// ---- 建副本 ----

// CreateSheetCopy 建远端副本：v3 建表 → sheets_batch_update 改名/加表 →
// values 分块写 + 日期两步写 → 回读校验。失败不删远端。
func (p *FeishuProvider) CreateSheetCopy(ctx context.Context, wb ExportWorkbook) (SheetWriteResult, error) {
	token, err := p.ensureFresh(ctx)
	if err != nil {
		return SheetWriteResult{}, err
	}
	var issues []TransferIssue
	title := []rune(wb.Title)
	if len(title) > fsTitleMaxRunes {
		issues = append(issues, TransferIssue{
			Feature: "文档标题", Severity: IssueWarning, Action: ActionTruncated,
			Reason: fmt.Sprintf("飞书标题上限 %d 字符，已截断", fsTitleMaxRunes),
		})
		title = title[:fsTitleMaxRunes]
	}
	if len(title) == 0 {
		title = []rune("Mosaic 表格")
	}
	var created fsCreateResult
	if err := p.postJSON(ctx, token, "/open-apis/sheets/v3/spreadsheets", map[string]any{"title": string(title)}, &created); err != nil {
		return SheetWriteResult{}, err
	}
	spToken := created.Data.Spreadsheet.SpreadsheetToken
	if spToken == "" {
		return SheetWriteResult{}, &UpstreamError{Code: "unknown", Message: "创建表格未返回 spreadsheet_token"}
	}
	targetURL := created.Data.Spreadsheet.URL
	if targetURL == "" {
		targetURL = "https://feishu.cn/sheets/" + spToken
	}
	res := SheetWriteResult{TargetURL: targetURL, Issues: issues}

	var query fsSheetsQueryResult
	if err := p.getJSON(ctx, token, "/open-apis/sheets/v3/spreadsheets/"+spToken+"/sheets/query", &query); err != nil {
		return res, err
	}
	if len(query.Data.Sheets) == 0 {
		return res, &UpstreamError{Code: "unknown", Message: "新建表格无默认工作表"}
	}
	type target struct {
		sheetID string
		es      ExportSheet
	}
	targets := make([]target, 0, len(wb.Sheets))
	firstID := query.Data.Sheets[0].SheetID
	for i, es := range wb.Sheets {
		if i == 0 {
			if err := p.postJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/sheets_batch_update", map[string]any{
				"requests": []any{map[string]any{"updateSheet": map[string]any{
					"properties": map[string]any{"sheetId": firstID, "title": es.Name},
				}}},
			}, nil); err != nil {
				return res, err
			}
			targets = append(targets, target{sheetID: firstID, es: es})
			continue
		}
		var added fsBatchUpdateResult
		if err := p.postJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/sheets_batch_update", map[string]any{
			"requests": []any{map[string]any{"addSheet": map[string]any{
				"properties": map[string]any{"title": es.Name, "index": i},
			}}},
		}, &added); err != nil {
			return res, err
		}
		sid := ""
		if len(added.Data.Replies) > 0 && added.Data.Replies[0].AddSheet != nil {
			sid = added.Data.Replies[0].AddSheet.Properties.SheetID
		}
		if sid == "" {
			return res, &UpstreamError{Code: "unknown", Message: "addSheet 未返回 sheetId"}
		}
		targets = append(targets, target{sheetID: sid, es: es})
	}

	for _, t := range targets {
		if err := p.writeFeishuSheet(ctx, token, spToken, t.sheetID, t.es, &res.Issues); err != nil {
			return res, err
		}
	}
	return res, nil
}

// fsOutCell 写入期望（回读比对用；1-based）。
type fsOutCell struct {
	r, c       int
	expectKind string // text | number | bool | formula | serial
	expectNum  float64
	expectText string
	expectBool bool
}

// writeFeishuSheet 单表写入 + 日期 formatter + 回读校验。
func (p *FeishuProvider) writeFeishuSheet(ctx context.Context, token, spToken, sheetID string, es ExportSheet, issues *[]TransferIssue) error {
	cells := make([]protocol.SheetCell, len(es.Cells))
	copy(cells, es.Cells)
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].R != cells[j].R {
			return cells[i].R < cells[j].R
		}
		return cells[i].C < cells[j].C
	})
	var outs []fsOutCell
	outs, werr := p.feishuWriteCells(ctx, token, spToken, sheetID, es.Name, cells, issues)
	if werr != nil {
		return werr
	}
	// 日期 formatter 两步写（逐格；超限跳过并汇总告警）。
	var dateCells []fsOutCell
	for _, o := range outs {
		if o.expectKind == "serial" {
			dateCells = append(dateCells, o)
		}
	}
	if len(dateCells) > 0 {
		if len(dateCells) > fsDateTimeStyleMax {
			*issues = append(*issues, TransferIssue{
				Sheet: es.Name, Feature: "日期时间", Severity: IssueWarning, Action: ActionDropped,
				Reason: fmt.Sprintf("日期格 %d 个超逐格设置上限 %d，formatter 跳过（值仍为序列数）", len(dateCells), fsDateTimeStyleMax),
			})
		} else {
			for _, o := range dateCells {
				fmtter := "yyyy/mm/dd"
				if o.expectNum != float64(int64(o.expectNum)) {
					fmtter = "yyyy/mm/dd hh:mm:ss"
				}
				rng := fmt.Sprintf("%s!%s%d:%s%d", sheetID, sheet.ColName(o.c), o.r, sheet.ColName(o.c), o.r)
				if err := p.putJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/style", map[string]any{
					"appendStyle": map[string]any{"range": rng, "style": map[string]any{"formatter": fmtter}},
				}, nil); err != nil {
					return err
				}
			}
			*issues = append(*issues, TransferIssue{
				Sheet: es.Name, Feature: "日期时间", Severity: IssueInfo, Action: ActionKept,
				Reason: fmt.Sprintf("%d 格日期以序列数 + formatter 两步写（M0 实证通道）", len(dateCells)),
			})
		}
	}
	return p.verifyFeishuSheet(ctx, token, spToken, sheetID, es.Name, outs, issues)
}

// writeFeishuChunk 一个矩形窗写入（range 全形）。
func (p *FeishuProvider) writeFeishuChunk(ctx context.Context, token, spToken, sheetID, sheetName string, chunk []protocol.SheetCell, minR, minC, maxR, maxC int, outs *[]fsOutCell, issues *[]TransferIssue) error {
	rows := maxR - minR + 1
	cols := maxC - minC + 1
	grid := make([][]any, rows)
	for i := range grid {
		grid[i] = make([]any, cols)
	}
	for _, c := range chunk {
		oc := fsOutCell{r: c.R, c: c.C}
		var v any
		switch c.Type {
		case "text":
			if boolLikeText(c.Text) || strings.HasPrefix(c.Text, "=") {
				v = map[string]any{"type": "text", "text": c.Text} // 防强转/防公式化（M0 实证）
			} else {
				v = c.Text
			}
			oc.expectKind, oc.expectText = "text", c.Text
		case "number":
			if c.Number != nil && significantDigits(*c.Number) > 15 {
				s := strconv.FormatFloat(*c.Number, 'f', -1, 64)
				v = map[string]any{"type": "text", "text": s}
				oc.expectKind, oc.expectText = "text", s
			} else {
				v = *c.Number
				oc.expectKind, oc.expectNum = "number", *c.Number
			}
		case "bool":
			b := c.Bool != nil && *c.Bool
			if b {
				v = map[string]any{"type": "formula", "text": "=TRUE()"}
			} else {
				v = map[string]any{"type": "formula", "text": "=FALSE()"}
			}
			oc.expectKind, oc.expectBool = "bool", b
		case "datetime":
			t, ok := parseCloudDateTime(c.Text)
			serial := 0.0
			if ok {
				serial = timeToExcelSerial(t)
			} else if f, err := strconv.ParseFloat(c.Text, 64); err == nil {
				serial = f // 已是序列数形态
			} else {
				*issues = append(*issues, TransferIssue{
					Sheet: sheetName, Range: fmt.Sprintf("%s%d", sheet.ColName(c.C), c.R),
					Feature: "日期时间", Severity: IssueWarning, Action: ActionConverted,
					Reason: "日期值不可解析（" + truncate(c.Text, 40) + "），按文本写入",
				})
				v = map[string]any{"type": "text", "text": c.Text}
				oc.expectKind, oc.expectText = "text", c.Text
				*outs = append(*outs, oc)
				continue
			}
			v = serial
			oc.expectKind, oc.expectNum = "serial", serial
		case "formula":
			v = map[string]any{"type": "formula", "text": "=" + c.Formula}
			oc.expectKind, oc.expectText = "formula", "="+c.Formula
		default:
			continue
		}
		grid[c.R-minR][c.C-minC] = v
		*outs = append(*outs, oc)
	}
	rng := fmt.Sprintf("%s!%s%d:%s%d", sheetID, sheet.ColName(minC), minR, sheet.ColName(maxC), maxR)
	body := map[string]any{"valueRange": map[string]any{"range": rng, "values": grid}}
	var out fsValuesResult
	return p.putJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/values", body, &out)
}

// verifyFeishuSheet 回读比对（公式剥 ' 引号规范化——飞书会把 =数据B!A1 重写
// 为 ='数据B'!A1 形态，M0 实证）。
func (p *FeishuProvider) verifyFeishuSheet(ctx context.Context, token, spToken, sheetID, sheetName string, outs []fsOutCell, issues *[]TransferIssue) error {
	if len(outs) == 0 {
		return nil
	}
	minR, maxR, minC, maxC := outs[0].r, outs[0].r, outs[0].c, outs[0].c
	for _, o := range outs {
		minR, maxR, minC, maxC = min(minR, o.r), max(maxR, o.r), min(minC, o.c), max(maxC, o.c)
	}
	rng := fmt.Sprintf("%s!%s%d:%s%d", sheetID, sheet.ColName(minC), minR, sheet.ColName(maxC), maxR)
	var ru, rf fsValuesResult
	q := url.Values{}
	q.Set("valueRenderOption", "UnformattedValue")
	if err := p.getJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/values/"+rng+"?"+q.Encode(), &ru); err != nil {
		return err
	}
	q.Set("valueRenderOption", "Formula")
	if err := p.getJSON(ctx, token, "/open-apis/sheets/v2/spreadsheets/"+spToken+"/values/"+rng+"?"+q.Encode(), &rf); err != nil {
		return err
	}
	mismatches := 0
	emit := func(reason string) {
		mismatches++
		if mismatches <= fsVerifyIssueMax {
			*issues = append(*issues, TransferIssue{Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验", Reason: reason})
		}
	}
	for _, o := range outs {
		uv := at(ru.Data.ValueRange.Values, o.r-minR, o.c-minC)
		fmv := at(rf.Data.ValueRange.Values, o.r-minR, o.c-minC)
		switch o.expectKind {
		case "formula":
			fs, _ := fmv.(string)
			if sheet.CanonicalFormula(stripSheetQuotes(fs)) != sheet.CanonicalFormula(stripSheetQuotes(o.expectText)) {
				emit(fmt.Sprintf("公式不一致 %s%d：写 %q 读 %q", sheet.ColName(o.c), o.r, o.expectText, fs))
			}
		case "number", "serial":
			fn, ok := uv.(float64)
			if !ok || !numEqual(fn, o.expectNum) {
				emit(fmt.Sprintf("数值不一致 %s%d：写 %v 读 %v", sheet.ColName(o.c), o.r, o.expectNum, uv))
			}
		case "bool":
			b, ok := uv.(bool)
			if !ok || b != o.expectBool {
				emit(fmt.Sprintf("布尔不一致 %s%d", sheet.ColName(o.c), o.r))
			}
		default:
			s, ok := uv.(string)
			if !ok || s != o.expectText {
				emit(fmt.Sprintf("文本不一致 %s%d：写 %q 读 %q", sheet.ColName(o.c), o.r, truncate(o.expectText, 40), truncate(fmt.Sprint(uv), 40)))
			}
		}
	}
	if mismatches > fsVerifyIssueMax {
		*issues = append(*issues, TransferIssue{
			Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验",
			Reason: fmt.Sprintf("共 %d 格回读不一致（仅逐格列出前 %d）", mismatches, fsVerifyIssueMax),
		})
	}
	return nil
}

// boolLikeText 飞书会强转布尔的文本形态（"TRUE"/"false"，M0 实证大小写不敏感）。
func boolLikeText(s string) bool {
	u := strings.ToUpper(s)
	return u == "TRUE" || u == "FALSE"
}

// stripSheetQuotes 公式比较规范化：剥单引号（='数据B'!A1 ≡ =数据B!A1）。
func stripSheetQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "")
}

// feishuWriteCells 写入核（建副本与 M3 回写共用）：排序 → 形态计数 issue →
// 矩形窗分块 PUT（≤fsSheetWriteCells 格、列窗 ≤fsSheetWriteColSpan）。
// 返回回读期望格集（datetime formatter 与 verify 由调用方接续处理）。
func (p *FeishuProvider) feishuWriteCells(ctx context.Context, token, spToken, sheetID, sheetName string, cells []protocol.SheetCell, issues *[]TransferIssue) ([]fsOutCell, error) {
	sorted := make([]protocol.SheetCell, len(cells))
	copy(sorted, cells)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].R != sorted[j].R {
			return sorted[i].R < sorted[j].R
		}
		return sorted[i].C < sorted[j].C
	})
	var values []protocol.SheetCell
	boolCount, digitCount := 0, 0
	for _, c := range sorted {
		if c.R < 1 || c.C < 1 {
			continue
		}
		switch c.Type {
		case "bool":
			boolCount++
		case "number":
			if c.Number != nil && significantDigits(*c.Number) > 15 {
				digitCount++
			}
		}
		if c.Type != "null" {
			values = append(values, c)
		}
	}
	if boolCount > 0 {
		*issues = append(*issues, TransferIssue{
			Sheet: sheetName, Feature: "布尔值", Severity: IssueInfo, Action: ActionConverted,
			Reason: fmt.Sprintf("%d 格布尔以 =TRUE()/=FALSE() 公式形态写入（飞书 values 不收布尔字面，M0 实证）", boolCount),
		})
	}
	if digitCount > 0 {
		*issues = append(*issues, TransferIssue{
			Sheet: sheetName, Feature: "超精度数值", Severity: IssueWarning, Action: ActionConverted,
			Reason: fmt.Sprintf("%d 个数值有效数字超 15 位（飞书数值通道 float64），转文本保真", digitCount),
		})
	}
	var outs []fsOutCell
	for start := 0; start < len(values); {
		end := start + 1
		minR, maxR, minC, maxC := values[start].R, values[start].R, values[start].C, values[start].C
		for end < len(values) {
			nc := values[end]
			nMinR, nMaxR := min(minR, nc.R), max(maxR, nc.R)
			nMinC, nMaxC := min(minC, nc.C), max(maxC, nc.C)
			if end-start >= fsSheetWriteCells || nMaxC-nMinC+1 > fsSheetWriteColSpan {
				break
			}
			minR, maxR, minC, maxC = nMinR, nMaxR, nMinC, nMaxC
			end++
		}
		chunk := values[start:end]
		start = end
		if err := p.writeFeishuChunk(ctx, token, spToken, sheetID, sheetName, chunk, minR, minC, maxR, maxC, &outs, issues); err != nil {
			return outs, err
		}
	}
	return outs, nil
}

// ---- M3 回写会话 ----

// feishuTarget 已解析目标的回写会话（token/spToken/表名映射一次建立）。
type feishuTarget struct {
	p       *FeishuProvider
	token   string
	spToken string
	sheets  map[string]string
}

// OpenSheetTarget 解析目标一次（token 刷新 + sheets/query 建名→id 映射；
// 非 sheet 类型不进映射）。多维表格不是合法回写目标（导入只读）。
func (p *FeishuProvider) OpenSheetTarget(ctx context.Context, rawURL string) (SheetTargetSession, error) {
	token, err := p.ensureFresh(ctx)
	if err != nil {
		return nil, err
	}
	kind, resToken, err := feishuResourceToken(rawURL)
	if err != nil {
		return nil, err
	}
	if kind != fsKindSheet {
		return nil, &UpstreamError{Code: "bad_sheet_target",
			Message: "多维表格不支持作为回写目标（仅电子表格可更新）——请重新导出电子表格副本"}
	}
	spToken := resToken
	var query fsSheetsQueryResult
	if err := p.getJSON(ctx, token, "/open-apis/sheets/v3/spreadsheets/"+spToken+"/sheets/query", &query); err != nil {
		return nil, err
	}
	sheets := map[string]string{}
	for _, sh := range query.Data.Sheets {
		if sh.ResourceType == "sheet" {
			sheets[sh.Title] = sh.SheetID
		}
	}
	return &feishuTarget{p: p, token: token, spToken: spToken, sheets: sheets}, nil
}

func (t *feishuTarget) WriteChunk(ctx context.Context, sheetName string, cells []protocol.SheetCell) ([]TransferIssue, error) {
	sid, ok := t.sheets[sheetName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSheetNotInTarget, sheetName)
	}
	var issues []TransferIssue
	outs, err := t.p.feishuWriteCells(ctx, t.token, t.spToken, sid, sheetName, cells, &issues)
	if err != nil {
		return issues, err
	}
	// 日期 formatter（值语义保真；「样式不更新」裁定不涵盖日期显示形态）。
	var dateCells []fsOutCell
	for _, o := range outs {
		if o.expectKind == "serial" {
			dateCells = append(dateCells, o)
		}
	}
	if len(dateCells) > fsDateTimeStyleMax {
		issues = append(issues, TransferIssue{
			Sheet: sheetName, Feature: "日期时间", Severity: IssueWarning, Action: ActionDropped,
			Reason: fmt.Sprintf("日期格 %d 个超逐格设置上限 %d，formatter 跳过（值仍为序列数）", len(dateCells), fsDateTimeStyleMax),
		})
	} else {
		for _, o := range dateCells {
			fmtter := "yyyy/mm/dd"
			if o.expectNum != float64(int64(o.expectNum)) {
				fmtter = "yyyy/mm/dd hh:mm:ss"
			}
			rng := fmt.Sprintf("%s!%s%d:%s%d", sid, sheet.ColName(o.c), o.r, sheet.ColName(o.c), o.r)
			if err := t.p.putJSON(ctx, t.token, "/open-apis/sheets/v2/spreadsheets/"+t.spToken+"/style", map[string]any{
				"appendStyle": map[string]any{"range": rng, "style": map[string]any{"formatter": fmtter}},
			}, nil); err != nil {
				return issues, err
			}
		}
	}
	return issues, nil
}

func (t *feishuTarget) VerifySheet(ctx context.Context, sheetName string, expect []protocol.SheetCell) []TransferIssue {
	sid, ok := t.sheets[sheetName]
	if !ok {
		return []TransferIssue{{Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验",
			Reason: fmt.Sprintf("%v: %s", ErrSheetNotInTarget, sheetName)}}
	}
	var issues []TransferIssue
	_, outs, _ := feishuExpect(expect, sheetName)
	if err := t.p.verifyFeishuSheet(ctx, t.token, t.spToken, sid, sheetName, outs, &issues); err != nil {
		issues = append(issues, TransferIssue{Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验",
			Reason: "回读失败: " + truncate(err.Error(), 120)})
	}
	return issues
}

func (t *feishuTarget) Close() error { return nil }

// feishuExpect 期望格 → 回读期望集（复用写入形态映射——写什么比什么）。
func feishuExpect(cells []protocol.SheetCell, sheetName string) (int, []fsOutCell, []TransferIssue) {
	sorted := make([]protocol.SheetCell, len(cells))
	copy(sorted, cells)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].R != sorted[j].R {
			return sorted[i].R < sorted[j].R
		}
		return sorted[i].C < sorted[j].C
	})
	var outs []fsOutCell
	for _, c := range sorted {
		if c.R < 1 || c.C < 1 {
			continue
		}
		oc := fsOutCell{r: c.R, c: c.C}
		switch c.Type {
		case "text":
			oc.expectKind, oc.expectText = "text", c.Text
		case "number":
			if c.Number != nil && significantDigits(*c.Number) > 15 {
				oc.expectKind, oc.expectText = "text", strconv.FormatFloat(*c.Number, 'f', -1, 64)
			} else {
				oc.expectKind, oc.expectNum = "number", *c.Number
			}
		case "bool":
			oc.expectKind, oc.expectBool = "bool", c.Bool != nil && *c.Bool
		case "datetime":
			if tt, ok := parseCloudDateTime(c.Text); ok {
				oc.expectKind, oc.expectNum = "serial", timeToExcelSerial(tt)
			} else if f, err := strconv.ParseFloat(c.Text, 64); err == nil {
				oc.expectKind, oc.expectNum = "serial", f
			} else {
				oc.expectKind, oc.expectText = "text", c.Text
			}
		case "formula":
			oc.expectKind, oc.expectText = "formula", "="+c.Formula
		default:
			continue
		}
		outs = append(outs, oc)
	}
	return len(outs), outs, nil
}
