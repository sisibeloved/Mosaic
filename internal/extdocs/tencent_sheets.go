// 腾讯文档表格交换适配（RFC-0017 M2，能力依据 §10.1-10.4 M0 实测档案）：
// 全部工具走主网关（M2 所需 create_file/add_sheet/rename_sheet/set_range_value/
// get_cell_data/get_sheet_info/get_merged_cells 主网关均已注册——独立端点仅
// 四个 M2 不用的工具缺失，无需双端点路由）。坐标转换：腾讯 0-based ↔
// 中立/本地 1-based。
package extdocs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/sheet"
)

// 腾讯表格交换常量。
const (
	// txSheetReadCells 单次 get_cell_data 格数（上游上限 20000；压小保响应
	// 体可控——含公式原文的 5000 格已在数百 KB 量级）。
	txSheetReadCells = 5000
	// txSheetReadCapBytes 区域读响应上限（大文本单元格兜底）。
	txSheetReadCapBytes = 8 << 20
	// txSheetWriteCells 单次 set_range_value 格数（官方建议单请求 ≤1MB，保守）。
	txSheetWriteCells = 500
	// txTitleMaxRunes create_file 标题上限（官方 36 字符）。
	txTitleMaxRunes = 36
	// txSheetNameMaxRunes 子表名上限（官方 31 字符）。
	txSheetNameMaxRunes = 31
	// txVerifyIssueMax 回读差异逐格报告上限（超出汇总）。
	txVerifyIssueMax = 20
)

// SheetCapabilities M0 实证：公式原文读写 ✓、样式写静默失效 ✗、布尔字面
// 写 ✓、类型化日期写 ✗、数值通道 float64。
func (p *TencentProvider) SheetCapabilities() SheetCaps {
	return SheetCaps{
		ReadFormulas:  true,
		WriteFormulas: true,
		ReadStyles:    true,
		WriteStyles:   false,
		WriteBool:     true,
		WriteDatetime: false,
		NumberFloat64: true,
	}
}

// ---- URL → file_id ----

// txFileIDPattern file_id 形态（字母数字短串）。
var txFileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`)

// tencentFileIDCandidates 链接 token → file_id 候选：实测 URL 形如
// docs.qq.com/sheet/D<base64(file_id)>（M0 建表返回 bNbufpufnTqa ↔ URL
// DYk5idWZwdWZuVHFh）；候选序 = 去首字符解码、原样、整串解码，由
// query_file_info 逐个校验定案。
func tencentFileIDCandidates(rawURL string) []string {
	u, err := parseURL(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	seg := strings.TrimPrefix(u.Path, "/sheet/")
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return nil
	}
	var out []string
	add := func(s string) {
		if txFileIDPattern.MatchString(s) {
			for _, e := range out {
				if e == s {
					return
				}
			}
			out = append(out, s)
		}
	}
	if len(seg) > 1 { // 解码形态优先（URL 是常态输入，裸段多为无效 id）
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if raw, err := enc.DecodeString(seg[1:]); err == nil {
				add(string(raw))
			}
			if raw, err := enc.DecodeString(seg); err == nil {
				add(string(raw))
			}
		}
	}
	add(seg)
	return out
}

// txFileInfo manage.query_file_info 结果。
type txFileInfo struct {
	FileID string `json:"file_id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// resolveSheetFile 链接（或裸 file_id）→ file_id + 标题：候选逐个经
// query_file_info 校验（type=sheet 且未入回收站）——解析不赌注编码形态。
func (p *TencentProvider) resolveSheetFile(ctx context.Context, rawURL string) (fileID, title string, err error) {
	cands := tencentFileIDCandidates(rawURL)
	if len(cands) == 0 {
		return "", "", &UpstreamError{Code: "bad_sheet_link", Message: "链接不是腾讯在线表格（需 docs.qq.com/sheet/ 链接或 file_id）"}
	}
	for _, cand := range cands {
		var info txFileInfo
		if err := p.callTool(ctx, "manage.query_file_info", map[string]any{"file_id": cand}, &info, responseCap); err != nil {
			continue // 候选不成立（不存在/非本账号可见）——试下一个
		}
		if info.Type != "sheet" {
			continue
		}
		if info.Status == "trash" {
			return "", "", &UpstreamError{Code: "sheet_in_trash", Message: "该表格已在回收站，请先恢复后再导入"}
		}
		return cand, info.Title, nil
	}
	return "", "", &UpstreamError{Code: "bad_sheet_link", Message: "链接无法解析为本账号可见的在线表格"}
}

// ---- 上游工具薄封装 ----

// callTool tools/call 便捷封装：结果文本按 JSON 解析进 out；isError 携文本。
func (p *TencentProvider) callTool(ctx context.Context, tool string, args map[string]any, out any, respCap int64) error {
	resp, err := p.callCap(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, respCap)
	if err != nil {
		return err
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return &UpstreamError{Code: "unknown", Message: tool + " 结果不可解析: " + truncate(err.Error(), 200)}
	}
	var sb strings.Builder
	for _, c := range res.Content {
		sb.WriteString(c.Text)
		sb.WriteString("\n")
	}
	if res.IsError {
		return &UpstreamError{Code: "unknown", Message: tool + " 失败: " + truncate(sb.String(), 300)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(sb.String()), out); err != nil {
		return &UpstreamError{Code: "unknown", Message: tool + " 结果 JSON 不可解析: " + truncate(err.Error(), 200)}
	}
	return nil
}

type txSheetInfoResult struct {
	Sheets []struct {
		SheetID   string `json:"sheet_id"`
		SheetName string `json:"sheet_name"`
		SheetType string `json:"sheet_type"` // worksheet | smartsheet | smartcanvas
		RowCount  int    `json:"row_count"`
		ColCount  int    `json:"col_count"`
	} `json:"sheets"`
}

type txCell struct {
	Row         int     `json:"row"` // 0-based
	Col         int     `json:"col"`
	ValueType   string  `json:"value_type"` // NUMBER | STRING | BOOL | FORMULA | ERROR | TIME_STRING | RICH_STRING
	NumberValue float64 `json:"number_value"`
	StringValue string  `json:"string_value"`
	BoolValue   bool    `json:"bool_value"`
	Formula     string  `json:"formula"` // include_formula=true 时携带（含引导 =）
}

type txCellDataResult struct {
	Cells []txCell `json:"cells"`
}

type txMergedResult struct {
	MergedCells []string `json:"merged_cells"`
}

// ReadSheetSnapshot 读整簿快照：get_sheet_info → 逐 worksheet 分块
// get_cell_data(include_formula) → get_merged_cells 计数。读区按本地网格
// 上限截断（超出落 issues）；非 worksheet 子表丢弃并报告。
func (p *TencentProvider) ReadSheetSnapshot(ctx context.Context, rawURL string, sheetNames []string) (*CloudSnapshot, []TransferIssue, error) {
	fileID, title, err := p.resolveSheetFile(ctx, rawURL)
	if err != nil {
		return nil, nil, err
	}
	var info txSheetInfoResult
	if err := p.callTool(ctx, "sheet.get_sheet_info", map[string]any{"file_id": fileID}, &info, responseCap); err != nil {
		return nil, nil, err
	}
	if len(info.Sheets) == 0 {
		return nil, nil, &UpstreamError{Code: "empty_sheet", Message: "该表格没有工作表"}
	}
	want := map[string]bool{}
	for _, n := range sheetNames {
		want[n] = true
	}
	snap := &CloudSnapshot{Title: title}
	var issues []TransferIssue
	for _, sh := range info.Sheets {
		if sh.SheetType != "worksheet" {
			issues = append(issues, TransferIssue{
				Sheet: sh.SheetName, Feature: "子表类型 " + sh.SheetType,
				Severity: IssueInfo, Action: ActionDropped,
				Reason: "非普通工作表（智能表格/画布），M2 不迁移",
			})
			continue
		}
		if len(want) > 0 && !want[sh.SheetName] {
			continue
		}
		rows, cols := sh.RowCount, sh.ColCount
		if rows > sheet.MaxRows || cols > sheet.MaxCols {
			issues = append(issues, TransferIssue{
				Sheet: sh.SheetName, Range: fmt.Sprintf("读区截断为 %d×%d", sheet.MaxRows, sheet.MaxCols),
				Feature: "网格范围", Severity: IssueWarning, Action: ActionTruncated,
				Reason: fmt.Sprintf("云端网格 %d×%d 超出本地上限", rows, cols),
			})
			rows, cols = min(rows, sheet.MaxRows), min(cols, sheet.MaxCols)
		}
		cells, cellIssues, err := p.readTencentCells(ctx, fileID, sh.SheetID, rows, cols, sh.SheetName)
		if err != nil {
			return nil, nil, err
		}
		var merged txMergedResult
		_ = p.callTool(ctx, "sheet.get_merged_cells", map[string]any{
			"file_id": fileID, "sheet_id": sh.SheetID,
			"start_row": 0, "start_col": 0, "end_row": rows - 1, "end_col": cols - 1,
		}, &merged, responseCap)
		issues = append(issues, cellIssues...)
		snap.Sheets = append(snap.Sheets, CloudSheet{
			SheetID: sh.SheetID, Name: sh.SheetName, Rows: rows, Cols: cols,
			MergeCount: len(merged.MergedCells), Cells: cells,
		})
	}
	if len(snap.Sheets) == 0 {
		return nil, nil, &UpstreamError{Code: "no_worksheet", Message: "没有可导入的普通工作表（全部被过滤或非 worksheet）"}
	}
	return snap, issues, nil
}

// readTencentCells 分块读单元格（0-based 上游坐标 → 1-based 中立坐标），
// 输出按行列有序（快照哈希确定性）。
func (p *TencentProvider) readTencentCells(ctx context.Context, fileID, sheetID string, rows, cols int, sheetName string) ([]CloudCell, []TransferIssue, error) {
	var issues []TransferIssue
	dateCount, errCount := 0, 0
	cells := make([]CloudCell, 0)
	chunkRows := max(1, txSheetReadCells/max(cols, 1))
	for r0 := 0; r0 < rows; r0 += chunkRows {
		r1 := min(r0+chunkRows, rows) - 1
		var res txCellDataResult
		args := map[string]any{
			"file_id": fileID, "sheet_id": sheetID,
			"start_row": r0, "start_col": 0, "end_row": r1, "end_col": cols - 1,
			"return_csv": false, "include_formula": true,
		}
		if err := p.callTool(ctx, "sheet.get_cell_data", args, &res, txSheetReadCapBytes); err != nil {
			return nil, nil, err
		}
		for _, c := range res.Cells {
			if c.Row < 0 || c.Col < 0 || c.Row >= rows || c.Col >= cols {
				continue
			}
			cc := txCellToCloud(c, sheetName, &dateCount, &errCount)
			cells = append(cells, cc)
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].R != cells[j].R {
			return cells[i].R < cells[j].R
		}
		return cells[i].C < cells[j].C
	})
	if dateCount > 0 {
		issues = append(issues, TransferIssue{
			Sheet: sheetName, Feature: "日期时间", Severity: IssueInfo, Action: ActionKept,
			Reason: fmt.Sprintf("%d 格日期按 UTC 解释（云端无时区信息）", dateCount),
		})
	}
	if errCount > 0 {
		issues = append(issues, TransferIssue{
			Sheet: sheetName, Feature: "字面错误值", Severity: IssueWarning, Action: ActionConverted,
			Reason: fmt.Sprintf("%d 格字面错误值转文本（结构化读不区分错误码）", errCount),
		})
	}
	return cells, issues, nil
}

// txCellToCloud 单格映射：公式原文优先（include_formula 携带）；其余按
// value_type。TIME_STRING 尽力解析为 RFC3339，失败按文本回退。
func txCellToCloud(c txCell, sheetName string, dateCount, errCount *int) CloudCell {
	cc := CloudCell{R: c.Row + 1, C: c.Col + 1}
	switch {
	case c.Formula != "":
		cc.Kind = CloudCellFormula
		cc.Formula = c.Formula
	case c.ValueType == "NUMBER":
		n := c.NumberValue
		cc.Kind, cc.Number = CloudCellNumber, &n
	case c.ValueType == "BOOL":
		b := c.BoolValue
		cc.Kind, cc.Bool = CloudCellBool, &b
	case c.ValueType == "TIME_STRING":
		if t, ok := parseCloudDateTime(c.StringValue); ok {
			cc.Kind, cc.Text = CloudCellDatetime, t.Format("2006-01-02T15:04:05Z07:00")
			*dateCount++
		} else {
			cc.Kind, cc.Text = CloudCellText, c.StringValue
		}
	case c.ValueType == "ERROR":
		*errCount++
		cc.Kind, cc.Text = CloudCellText, "#ERROR!"
	default: // STRING / RICH_STRING
		cc.Kind, cc.Text = CloudCellText, c.StringValue
	}
	return cc
}

// ---- 建副本 ----

type txCreateFileResult struct {
	FileID string `json:"file_id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Type   string `json:"type"`
	Error  string `json:"error"`
}

type txAddSheetResult struct {
	SheetID string `json:"sheet_id"`
}

// CreateSheetCopy 建远端副本：create_file → rename 默认表 + add_sheet →
// set_range_value 类型化分块写 → 回读校验。失败不删远端（用户自行清理）。
func (p *TencentProvider) CreateSheetCopy(ctx context.Context, wb ExportWorkbook) (SheetWriteResult, error) {
	var issues []TransferIssue
	title := []rune(wb.Title)
	if len(title) > txTitleMaxRunes {
		issues = append(issues, TransferIssue{
			Feature: "文档标题", Severity: IssueWarning, Action: ActionTruncated,
			Reason: fmt.Sprintf("腾讯标题上限 %d 字符，已截断", txTitleMaxRunes),
		})
		title = title[:txTitleMaxRunes]
	}
	if len(title) == 0 {
		title = []rune("Mosaic 表格")
	}
	var created txCreateFileResult
	if err := p.callTool(ctx, "manage.create_file", map[string]any{
		"title": string(title), "file_type": "sheet",
	}, &created, responseCap); err != nil {
		return SheetWriteResult{}, err
	}
	if created.FileID == "" {
		return SheetWriteResult{}, &UpstreamError{Code: "unknown", Message: "创建表格未返回 file_id: " + truncate(firstNonEmpty(created.Error, created.Title), 200)}
	}
	if created.URL == "" {
		created.URL = "https://docs.qq.com/sheet/" + created.FileID
	}

	// 默认表改名对齐首表，其余 add_sheet（显式 index 保持顺序）。
	var info txSheetInfoResult
	if err := p.callTool(ctx, "sheet.get_sheet_info", map[string]any{"file_id": created.FileID}, &info, responseCap); err != nil {
		return SheetWriteResult{TargetURL: created.URL, Issues: issues}, err
	}
	if len(info.Sheets) == 0 {
		return SheetWriteResult{TargetURL: created.URL, Issues: issues}, &UpstreamError{Code: "unknown", Message: "新建表格无默认工作表"}
	}
	type target struct {
		sheetID string
		es      ExportSheet
	}
	targets := make([]target, 0, len(wb.Sheets))
	firstID := info.Sheets[0].SheetID
	for i, es := range wb.Sheets {
		name := []rune(es.Name)
		if len(name) > txSheetNameMaxRunes {
			issues = append(issues, TransferIssue{
				Sheet: es.Name, Feature: "工作表名", Severity: IssueWarning, Action: ActionTruncated,
				Reason: fmt.Sprintf("腾讯子表名上限 %d 字符，已截断", txSheetNameMaxRunes),
			})
			name = name[:txSheetNameMaxRunes]
		}
		if i == 0 {
			if err := p.callTool(ctx, "sheet.rename_sheet", map[string]any{
				"file_id": created.FileID, "sheet_id": firstID, "name": string(name),
			}, nil, responseCap); err != nil {
				return SheetWriteResult{TargetURL: created.URL, Issues: issues}, err
			}
			targets = append(targets, target{sheetID: firstID, es: es})
			continue
		}
		var added txAddSheetResult
		if err := p.callTool(ctx, "sheet.add_sheet", map[string]any{
			"file_id": created.FileID, "name": string(name), "index": i, "append_index": false,
		}, &added, responseCap); err != nil {
			return SheetWriteResult{TargetURL: created.URL, Issues: issues}, err
		}
		if added.SheetID == "" {
			return SheetWriteResult{TargetURL: created.URL, Issues: issues}, &UpstreamError{Code: "unknown", Message: "add_sheet 未返回 sheet_id"}
		}
		targets = append(targets, target{sheetID: added.SheetID, es: es})
	}

	for _, t := range targets {
		if err := p.writeTencentSheet(ctx, created.FileID, t.sheetID, t.es, &issues); err != nil {
			return SheetWriteResult{TargetURL: created.URL, Issues: issues}, err
		}
	}
	return SheetWriteResult{TargetURL: created.URL, Issues: issues}, nil
}

// txOutCell 写入期望（回读比对用；row/col 0-based）。
type txOutCell struct {
	row, col   int
	expectKind string // text | number | bool | formula
	expectNum  float64
	expectText string
	expectBool bool
}

// tencentMapCells 本地格 → 腾讯类型化写值 + 回读期望（建副本与 M3 回写共用）。
func tencentMapCells(cells []protocol.SheetCell, sheetName string) (toWrite []map[string]any, outs []txOutCell, convIssues []TransferIssue) {
	digits, dates := 0, 0
	for _, c := range cells {
		if c.R < 1 || c.C < 1 {
			continue
		}
		oc := txOutCell{row: c.R - 1, col: c.C - 1}
		var val map[string]any
		switch c.Type {
		case "text":
			oc.expectKind, oc.expectText = "text", c.Text
			val = map[string]any{"value_type": "STRING", "string_value": c.Text}
		case "number":
			if n := c.Number; n != nil && significantDigits(*n) > 15 {
				s := strconv.FormatFloat(*n, 'f', -1, 64)
				digits++
				oc.expectKind, oc.expectText = "text", s
				val = map[string]any{"value_type": "STRING", "string_value": s}
			} else {
				oc.expectKind, oc.expectNum = "number", *c.Number
				val = map[string]any{"value_type": "NUMBER", "number_value": *c.Number}
			}
		case "bool":
			b := c.Bool != nil && *c.Bool
			oc.expectKind, oc.expectBool = "bool", b
			val = map[string]any{"value_type": "BOOL", "bool_value": b}
		case "datetime":
			dates++
			oc.expectKind, oc.expectText = "text", c.Text
			val = map[string]any{"value_type": "STRING", "string_value": c.Text}
		case "formula":
			oc.expectKind, oc.expectText = "formula", "="+c.Formula
			val = map[string]any{"value_type": "FORMULA", "formula": "=" + c.Formula}
		default:
			continue
		}
		val["row"], val["col"] = oc.row, oc.col
		toWrite = append(toWrite, val)
		outs = append(outs, oc)
	}
	if digits > 0 {
		convIssues = append(convIssues, TransferIssue{
			Sheet: sheetName, Feature: "超精度数值", Severity: IssueWarning, Action: ActionConverted,
			Reason: fmt.Sprintf("%d 个数值有效数字超 15 位（腾讯数值通道 float64），转文本保真", digits),
		})
	}
	if dates > 0 {
		convIssues = append(convIssues, TransferIssue{
			Sheet: sheetName, Feature: "日期时间", Severity: IssueWarning, Action: ActionConverted,
			Reason: fmt.Sprintf("%d 格日期转 RFC3339 文本（腾讯 MCP 无类型化日期写）", dates),
		})
	}
	return toWrite, outs, convIssues
}

// tencentWriteChunked 类型化值分块下发（≤txSheetWriteCells/次）。
func (p *TencentProvider) tencentWriteChunked(ctx context.Context, fileID, sheetID string, toWrite []map[string]any) error {
	for i := 0; i < len(toWrite); i += txSheetWriteCells {
		end := min(i+txSheetWriteCells, len(toWrite))
		if err := p.callTool(ctx, "sheet.set_range_value", map[string]any{
			"file_id": fileID, "sheet_id": sheetID, "values": toWrite[i:end],
		}, nil, responseCap); err != nil {
			return err
		}
	}
	return nil
}

// writeTencentSheet 单表写入 + 回读校验（建副本路径）。
func (p *TencentProvider) writeTencentSheet(ctx context.Context, fileID, sheetID string, es ExportSheet, issues *[]TransferIssue) error {
	toWrite, outs, conv := tencentMapCells(es.Cells, es.Name)
	*issues = append(*issues, conv...)
	if err := p.tencentWriteChunked(ctx, fileID, sheetID, toWrite); err != nil {
		return err
	}
	return p.verifyTencentSheet(ctx, fileID, sheetID, es.Name, outs, issues)
}

// ---- M3 回写会话 ----

// tencentTarget 已解析目标的回写会话（fileID + 表名→ID 一次建立）。
type tencentTarget struct {
	p      *TencentProvider
	fileID string
	sheets map[string]string // 表名 → sheet_id（仅 worksheet）
}

// OpenSheetTarget 解析目标一次；非 worksheet 子表不进映射（按名写入即结构
// 不匹配错误）。
func (p *TencentProvider) OpenSheetTarget(ctx context.Context, rawURL string) (SheetTargetSession, error) {
	fileID, _, err := p.resolveSheetFile(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	var info txSheetInfoResult
	if err := p.callTool(ctx, "sheet.get_sheet_info", map[string]any{"file_id": fileID}, &info, responseCap); err != nil {
		return nil, err
	}
	sheets := map[string]string{}
	for _, sh := range info.Sheets {
		if sh.SheetType == "worksheet" {
			sheets[sh.SheetName] = sh.SheetID
		}
	}
	return &tencentTarget{p: p, fileID: fileID, sheets: sheets}, nil
}

func (t *tencentTarget) WriteChunk(ctx context.Context, sheetName string, cells []protocol.SheetCell) ([]TransferIssue, error) {
	sid, ok := t.sheets[sheetName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSheetNotInTarget, sheetName)
	}
	toWrite, _, conv := tencentMapCells(cells, sheetName)
	if err := t.p.tencentWriteChunked(ctx, t.fileID, sid, toWrite); err != nil {
		return nil, err
	}
	return conv, nil
}

func (t *tencentTarget) VerifySheet(ctx context.Context, sheetName string, expect []protocol.SheetCell) []TransferIssue {
	sid, ok := t.sheets[sheetName]
	if !ok {
		return []TransferIssue{{Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验",
			Reason: fmt.Sprintf("%v: %s", ErrSheetNotInTarget, sheetName)}}
	}
	_, outs, _ := tencentMapCells(expect, sheetName)
	var issues []TransferIssue
	if err := t.p.verifyTencentSheet(ctx, t.fileID, sid, sheetName, outs, &issues); err != nil {
		issues = append(issues, TransferIssue{Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验",
			Reason: "回读失败: " + truncate(err.Error(), 120)})
	}
	return issues
}

func (t *tencentTarget) Close() error { return nil }

// verifyTencentSheet 回读比对（数值 1e-9 相对容差；公式比原文）。
func (p *TencentProvider) verifyTencentSheet(ctx context.Context, fileID, sheetID, sheetName string, outs []txOutCell, issues *[]TransferIssue) error {
	if len(outs) == 0 {
		return nil
	}
	maxR, maxC := 0, 0
	for _, o := range outs {
		maxR, maxC = max(maxR, o.row), max(maxC, o.col)
	}
	var mismatches int
	emit := func(reason string) {
		mismatches++
		if mismatches <= txVerifyIssueMax {
			*issues = append(*issues, TransferIssue{
				Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验", Reason: reason,
			})
		}
	}
	var res txCellDataResult
	if err := p.callTool(ctx, "sheet.get_cell_data", map[string]any{
		"file_id": fileID, "sheet_id": sheetID,
		"start_row": 0, "start_col": 0, "end_row": maxR, "end_col": maxC,
		"return_csv": false, "include_formula": true,
	}, &res, txSheetReadCapBytes); err != nil {
		return err
	}
	got := map[int]txCell{}
	for _, c := range res.Cells {
		got[c.Row*10000+c.Col] = c
	}
	for _, o := range outs {
		c, ok := got[o.row*10000+o.col]
		if !ok {
			emit(fmt.Sprintf("回读缺少单元格 r%dc%d", o.row+1, o.col+1))
			continue
		}
		switch o.expectKind {
		case "formula":
			if sheet.CanonicalFormula(c.Formula) != sheet.CanonicalFormula(o.expectText) {
				emit(fmt.Sprintf("公式不一致 r%dc%d：写 %q 读 %q", o.row+1, o.col+1, o.expectText, c.Formula))
			}
		case "number":
			if c.ValueType != "NUMBER" || !numEqual(c.NumberValue, o.expectNum) {
				emit(fmt.Sprintf("数值不一致 r%dc%d：写 %v 读 %v", o.row+1, o.col+1, o.expectNum, c.NumberValue))
			}
		case "bool":
			if c.ValueType != "BOOL" || c.BoolValue != o.expectBool {
				emit(fmt.Sprintf("布尔不一致 r%dc%d", o.row+1, o.col+1))
			}
		default: // text / datetime_text
			if c.ValueType != "STRING" || c.StringValue != o.expectText {
				emit(fmt.Sprintf("文本不一致 r%dc%d：写 %q 读 %q", o.row+1, o.col+1, truncate(o.expectText, 40), truncate(c.StringValue, 40)))
			}
		}
	}
	if mismatches > txVerifyIssueMax {
		*issues = append(*issues, TransferIssue{
			Sheet: sheetName, Severity: IssueWarning, Feature: "回读校验",
			Reason: fmt.Sprintf("共 %d 格回读不一致（仅逐格列出前 %d）", mismatches, txVerifyIssueMax),
		})
	}
	return nil
}

// significantDigits 十进制最短表示的有效数字位数（>15 判 float64 有损风险）。
func significantDigits(f float64) int {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		s = s[:i]
	}
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

// numEqual 浮点比较（绝对 + 相对容差）。
func numEqual(a, b float64) bool {
	if a == b {
		return true
	}
	diff := a - b
	return (diff < 0 && -diff < 1e-9) || (diff >= 0 && diff < 1e-9) ||
		(diff/(a+b) < 1e-12 && diff/(a+b) > -1e-12)
}
