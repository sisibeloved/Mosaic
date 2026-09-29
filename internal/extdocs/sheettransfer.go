// 表格手动交换服务（RFC-0017 M2）：副本导入/导出的预览-执行-记录编排。
// 记录与存储端口在此；执行编排在 SheetTransferService（同文件下半部）。
package extdocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/sheet"
)

// 交换方向 / 状态封闭枚举。update = 回写已有远端目标（M3）；partial/canceled
// 仅 update（异步分块——已写部分 + 未跑完/主动停）。
const (
	TransferImport = "import"
	TransferExport = "export"
	TransferUpdate = "update"

	TransferRunning              = "running"
	TransferSucceeded            = "succeeded"
	TransferSucceededWithWarning = "succeeded_with_warnings"
	TransferFailed               = "failed"
	TransferUnknown              = "unknown"
	TransferPartial              = "partial"
	TransferCanceled             = "canceled"
)

// 记录错误哨兵。
var (
	// ErrTransferNotFound 交换记录不存在。
	ErrTransferNotFound = errors.New("extdocs: 交换记录不存在")
	// ErrTransferKeyConflict 幂等键已被不同参数的交换占用。
	ErrTransferKeyConflict = errors.New("extdocs: 幂等键冲突")
	// ErrStalePreview 预览后源已变化（hash/版本不符），须重新预览。
	ErrStalePreview = errors.New("extdocs: 预览已过期")
)

// SheetTransferRecord sheet_transfers 行。ReportJSON 为 []TransferIssue 序列化
// （进出原样，展示层解码）。
type SheetTransferRecord struct {
	TransferID     string `json:"transfer_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Direction      string `json:"direction"`
	Provider       string `json:"provider"`
	Status         string `json:"status"`
	DocID          string `json:"doc_id,omitempty"`
	ExternalURL    string `json:"external_url,omitempty"`
	Title          string `json:"title,omitempty"`
	SourceVersion  int64  `json:"source_version,omitempty"`
	SnapshotHash   string `json:"snapshot_hash,omitempty"`
	ReportJSON     string `json:"report_json"`
	Error          string `json:"error,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	// M3 回写（update 方向）：进度与恢复状态（state_json 不出网——含目标
	// 前态与剩余计划）。
	CellsTotal   int    `json:"cells_total,omitempty"`
	CellsWritten int    `json:"cells_written,omitempty"`
	StateJSON    string `json:"-"`
}

// SheetTransferStore 交换记录存储端口（sqlite 实现）。
type SheetTransferStore interface {
	InsertSheetTransfer(ctx context.Context, rec SheetTransferRecord) error // 幂等键占用 = ErrTransferKeyConflict
	GetSheetTransfer(ctx context.Context, transferID string) (SheetTransferRecord, error)
	GetSheetTransferByKey(ctx context.Context, idempotencyKey string) (SheetTransferRecord, error)
	UpdateSheetTransfer(ctx context.Context, rec SheetTransferRecord) error // 按 transfer_id 全量更新
	ListSheetTransfers(ctx context.Context, docID, direction string, limit int) ([]SheetTransferRecord, error)
	MarkStaleRunningTransfers(ctx context.Context, now string) (int64, error) // 启动扫 running → unknown
}

// ---- 交换服务 ----

// SheetDocPort 导入落库 / 导出读取端口（*doc.Service 结构性满足）。
type SheetDocPort interface {
	CreateSheetDoc(ctx context.Context, actor doc.Actor, title string, initialCells []protocol.SheetCell, source, note string) (*doc.CommandResult, error)
	CommitSheetRevision(ctx context.Context, actor doc.Actor, docID string, baseVersion int64, ops []protocol.SheetOp, source, note string) (*doc.CommandResult, error)
	GetDoc(ctx context.Context, docID string) (doc.DocState, error)
}

// TransferPreview 预览产物（不含格数据——摘要与损失清单）。
type TransferPreview struct {
	Direction    string             `json:"direction"`
	Provider     string             `json:"provider"`
	Title        string             `json:"title,omitempty"`
	SourceURL    string             `json:"source_url,omitempty"`
	DocID        string             `json:"doc_id,omitempty"`
	SourceVer    int64              `json:"source_version,omitempty"`
	SnapshotHash string             `json:"snapshot_hash,omitempty"`
	Sheets       []PreviewSheetInfo `json:"sheets"`
	Issues       []TransferIssue    `json:"issues"`
}

// PreviewSheetInfo 每表摘要。
type PreviewSheetInfo struct {
	Name       string `json:"name"`
	Rows       int    `json:"rows"`
	Cols       int    `json:"cols"`
	Filled     int    `json:"filled"`
	Formulas   int    `json:"formulas"`
	MergeCount int    `json:"merge_count"`
}

// PreviewRequest 预览/执行共用参数。
type TransferRequest struct {
	Direction  string   // import | export
	Provider   string   // provider id；导入可空 = 按 URL host 归属
	URL        string   // 导入：云端表格链接
	DocID      string   // 导出：本地表格 doc_id
	SheetNames []string // 可选工作表过滤（按名）
	Actor      string   // 发起者 participant id
	// ExecuteOnly：
	IdempotencyKey string
	SnapshotHash   string // 导入：execute 时远端重拉须一致
	SourceVersion  int64  // 导出：execute 时本地版本须未变
}

// SheetTransferService 交换编排。
type SheetTransferService struct {
	providers *Service
	docs      SheetDocPort
	store     SheetTransferStore
	newID     func(prefix string) string // xfr_（记录）/ sht_（add_sheet）
	Clock     func() time.Time
	Logger    *slog.Logger

	updMu   sync.Mutex               // M3 回写取消通道注册表
	cancels map[string]chan struct{} // transferID → cancel（进程内；重启由 unknown 收敛）
}

// NewSheetTransferService 构造（store 可为 nil = 无历史记录，执行仍可用）。
func NewSheetTransferService(providers *Service, docs SheetDocPort, store SheetTransferStore, newID func(prefix string) string) *SheetTransferService {
	return &SheetTransferService{
		providers: providers, docs: docs, store: store,
		newID: newID, Clock: time.Now, Logger: slog.Default(),
	}
}

// sheetProvider 按 id 或 URL host 取实现表格能力的 provider。
func (s *SheetTransferService) sheetProvider(providerID, rawURL string) (SheetProvider, error) {
	var p Provider
	if providerID != "" {
		var ok bool
		p, ok = s.providers.Provider(providerID)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
		}
	} else if rawURL != "" {
		u, err := parseURL(rawURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return nil, ErrBadLink
		}
		var ok bool
		p, ok = s.providers.ProviderByHost(u.Hostname())
		if !ok {
			return nil, ErrBadLink
		}
	} else {
		return nil, ErrBadLink
	}
	sp, ok := p.(SheetProvider)
	if !ok {
		return nil, &UpstreamError{Code: "sheets_unsupported", Message: "该外部服务不支持表格交换"}
	}
	return sp, nil
}

// ---- 预览 ----

// Preview 导入：拉远端快照出摘要；导出：读本地态出摘要。两者都产出
// snapshot_hash（导入=快照内容）或 source_version（导出=本地版本）供
// execute 防偷换。
func (s *SheetTransferService) Preview(ctx context.Context, req TransferRequest) (*TransferPreview, error) {
	switch req.Direction {
	case TransferImport:
		return s.previewImport(ctx, req)
	case TransferExport:
		return s.previewExport(ctx, req)
	default:
		return nil, fmt.Errorf("%w: direction %q", ErrBadDirection, req.Direction)
	}
}

func (s *SheetTransferService) previewImport(ctx context.Context, req TransferRequest) (*TransferPreview, error) {
	sp, err := s.sheetProvider(req.Provider, req.URL)
	if err != nil {
		return nil, err
	}
	if req.URL == "" {
		return nil, ErrBadLink
	}
	snap, issues, err := sp.ReadSheetSnapshot(ctx, req.URL, req.SheetNames)
	if err != nil {
		return nil, err
	}
	issues = append(append([]TransferIssue{}, issues...), s.planLocalCaps(snap)...)
	if hasBlocked(issues) {
		return nil, &BlockedTransferError{Issues: issues}
	}
	pv := &TransferPreview{
		Direction: TransferImport, Provider: sp.ID(),
		Title: snap.Title, SourceURL: req.URL,
		SnapshotHash: SnapshotHash(snap), Sheets: previewSheets(snap), Issues: issues,
	}
	return pv, nil
}

func (s *SheetTransferService) previewExport(ctx context.Context, req TransferRequest) (*TransferPreview, error) {
	if req.DocID == "" {
		return nil, ErrDocRequired
	}
	state, err := s.docs.GetDoc(ctx, req.DocID)
	if err != nil {
		return nil, err
	}
	if state.Format != doc.FormatSpreadsheet || state.Workbook == nil {
		return nil, ErrNotSpreadsheet
	}
	wb, issues := BuildExportWorkbook(state, req.SheetNames)
	return &TransferPreview{
		Direction: TransferExport, Provider: req.Provider, DocID: req.DocID,
		Title: state.Title, SourceVer: state.Version,
		Sheets: exportPreviewSheets(wb), Issues: issues,
	}, nil
}

// ---- 执行 ----

// BlockedTransferError 预检阻止（含全量报告项）。
type BlockedTransferError struct{ Issues []TransferIssue }

func (e *BlockedTransferError) Error() string {
	return "extdocs: 交换被转换报告阻止（含 blocked 项）"
}

// Execute 副本执行：幂等重放 → running 落库 → 导入装配 / 导出建副本 → 终态。
func (s *SheetTransferService) Execute(ctx context.Context, req TransferRequest) (*SheetTransferRecord, error) {
	if req.IdempotencyKey == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	// 幂等重放：同键既有记录直接返回
	if s.store != nil {
		if existing, err := s.store.GetSheetTransferByKey(ctx, req.IdempotencyKey); err == nil {
			return &existing, nil
		} else if !errors.Is(err, ErrTransferNotFound) {
			return nil, err
		}
	}
	now := s.Clock().Format(time.RFC3339)
	rec := SheetTransferRecord{
		TransferID: s.newID("xfr"), IdempotencyKey: req.IdempotencyKey,
		Direction: req.Direction, Status: TransferRunning,
		Title: req.Title(), CreatedAt: now, UpdatedAt: now,
		ReportJSON: "[]",
	}
	if req.Direction == TransferImport {
		rec.ExternalURL = req.URL
	} else {
		rec.DocID = req.DocID
		rec.SourceVersion = req.SourceVersion
	}
	if s.store != nil {
		if err := s.store.InsertSheetTransfer(ctx, rec); err != nil {
			return nil, err
		}
	}
	var issues []TransferIssue
	var err error
	switch req.Direction {
	case TransferImport:
		issues, err = s.executeImport(ctx, req, &rec)
	case TransferExport:
		issues, err = s.executeExport(ctx, req, &rec)
	default:
		err = fmt.Errorf("%w: direction %q", ErrBadDirection, req.Direction)
	}
	rec.ReportJSON = marshalIssues(issues)
	rec.UpdatedAt = s.Clock().Format(time.RFC3339)
	if err != nil {
		rec.Status = TransferFailed
		rec.Error = truncate(err.Error(), 500)
	} else if hasBlocked(issues) {
		rec.Status = TransferFailed
		rec.Error = "转换报告含阻止项"
	} else if hasWarning(issues) {
		rec.Status = TransferSucceededWithWarning
	} else {
		rec.Status = TransferSucceeded
	}
	if s.store != nil {
		if uerr := s.store.UpdateSheetTransfer(ctx, rec); uerr != nil {
			s.Logger.Error("extdocs: 交换终态落库失败", "transfer_id", rec.TransferID, "err", uerr.Error())
		}
	}
	if err == nil && rec.Status != TransferFailed {
		s.Logger.Info("extdocs: 表格副本交换完成", "direction", rec.Direction, "provider", rec.Provider,
			"status", rec.Status, "doc_id", rec.DocID, "url", rec.ExternalURL)
	}
	return &rec, err
}

// Title 请求侧标题（记录展示用；真实标题由执行填）。
func (r TransferRequest) Title() string {
	if r.Direction == TransferImport {
		return "导入中…"
	}
	return "导出中…"
}

// executeImport 导入执行：重拉快照比 hash → 本地装配。
func (s *SheetTransferService) executeImport(ctx context.Context, req TransferRequest, rec *SheetTransferRecord) ([]TransferIssue, error) {
	sp, err := s.sheetProvider(req.Provider, req.URL)
	if err != nil {
		return nil, err
	}
	if req.URL == "" {
		return nil, ErrBadLink
	}
	snap, issues, err := sp.ReadSheetSnapshot(ctx, req.URL, req.SheetNames)
	if err != nil {
		return nil, err
	}
	issues = append(append([]TransferIssue{}, issues...), s.planLocalCaps(snap)...)
	if hasBlocked(issues) {
		return issues, &BlockedTransferError{Issues: issues}
	}
	if req.SnapshotHash != "" && req.SnapshotHash != SnapshotHash(snap) {
		return issues, fmt.Errorf("%w: 远端内容已变化，请重新预览", ErrStalePreview)
	}
	rec.Provider = sp.ID()
	rec.Title = sheetTitleOf(snap, sp.DisplayName())
	actor := doc.Actor{ParticipantID: req.Actor, Kind: "human"}
	note := "自 " + sp.DisplayName() + " 导入"
	res, err := s.docs.CreateSheetDoc(ctx, actor, rec.Title, nil, "import", note)
	if err != nil {
		return issues, err
	}
	rec.DocID = res.DocID
	issues, err = s.applySnapshot(ctx, actor, res.DocID, snap, issues)
	if err != nil {
		return issues, err
	}
	return issues, nil
}

// applySnapshot 快照 → 连续修订批（rename 首表 + add_sheet + set_cells 分块）。
// 返回追加装配期 issue 后的报告切片（slice 头按值传——内部追加不回写调用方）。
func (s *SheetTransferService) applySnapshot(ctx context.Context, actor doc.Actor, docID string, snap *CloudSnapshot, issues []TransferIssue) ([]TransferIssue, error) {
	wb := sheet.NewWorkbookForDoc(docID)
	defaultID := wb.Sheets[0].SheetID
	base := int64(1)
	note := ""
	// 批次组装：每批 ≤64 ops、≤1000 cells
	var ops []protocol.SheetOp
	var cellCount int
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		res, err := s.docs.CommitSheetRevision(ctx, actor, docID, base, ops, "import", note)
		if err != nil {
			return err
		}
		base = res.DocVersion
		ops, cellCount = nil, 0
		return nil
	}
	push := func(op protocol.SheetOp, cells int) error {
		if len(ops) >= sheet.MaxOpsPerBatch || cellCount+cells > sheet.MaxCellsPerBatch {
			if err := flush(); err != nil {
				return err
			}
		}
		ops = append(ops, op)
		cellCount += cells
		return nil
	}
	// 首表名对齐（默认 Sheet1 → 云端名）
	if snap.Sheets[0].Name != wb.Sheets[0].Name {
		if err := push(protocol.SheetOp{Op: sheet.OpRenameSheet, SheetID: defaultID, Name: snap.Sheets[0].Name}, 0); err != nil {
			return issues, err
		}
	}
	for i, cs := range snap.Sheets {
		sid := defaultID
		if i > 0 {
			sid = s.newID("sht")
			if err := push(protocol.SheetOp{Op: sheet.OpAddSheet, SheetID: sid, Name: cs.Name}, 0); err != nil {
				return issues, err
			}
		}
		cells, cellIssues := cloudCellsToLocal(cs, snap.Title)
		issues = append(issues, cellIssues...)
		for start := 0; start < len(cells); start += sheet.MaxCellsPerBatch {
			end := min(start+sheet.MaxCellsPerBatch, len(cells))
			if err := push(protocol.SheetOp{Op: sheet.OpSetCells, SheetID: sid, Cells: cells[start:end]}, end-start); err != nil {
				return issues, err
			}
		}
	}
	if err := flush(); err != nil {
		return issues, err
	}
	return issues, nil
}

// executeExport 导出执行：本地版本比对 → 快照 → 建副本。
func (s *SheetTransferService) executeExport(ctx context.Context, req TransferRequest, rec *SheetTransferRecord) ([]TransferIssue, error) {
	sp, err := s.sheetProvider(req.Provider, "")
	if err != nil {
		return nil, err
	}
	rec.Provider = sp.ID()
	state, err := s.docs.GetDoc(ctx, req.DocID)
	if err != nil {
		return nil, err
	}
	if state.Format != doc.FormatSpreadsheet || state.Workbook == nil {
		return nil, ErrNotSpreadsheet
	}
	if req.SourceVersion != 0 && state.Version != req.SourceVersion {
		return nil, fmt.Errorf("%w: 本地版本已到 %d（预览基于 %d），请重新预览", ErrStalePreview, state.Version, req.SourceVersion)
	}
	rec.Title = state.Title
	wb, issues := BuildExportWorkbook(state, req.SheetNames)
	if hasBlocked(issues) {
		return issues, &BlockedTransferError{Issues: issues}
	}
	res, err := sp.CreateSheetCopy(ctx, wb)
	if err != nil {
		return issues, err
	}
	rec.ExternalURL = res.TargetURL
	return append(issues, res.Issues...), nil
}

// List 历史。
func (s *SheetTransferService) List(ctx context.Context, docID, direction string, limit int) ([]SheetTransferRecord, error) {
	if s.store == nil {
		return nil, ErrTransferNotFound
	}
	return s.store.ListSheetTransfers(ctx, docID, direction, limit)
}

// ---- 共享装配辅助 ----

// 快照哈希：canonical JSON（键序确定——结构体字段序即确定序）。
func SnapshotHash(snap *CloudSnapshot) string {
	raw, err := json.Marshal(snap)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// sheetTitleOf 导入标题（云端标题截到本地上限）。
func sheetTitleOf(snap *CloudSnapshot, providerName string) string {
	t := strings.TrimSpace(snap.Title)
	if t == "" {
		return "自" + providerName + "导入的表格"
	}
	r := []rune(t)
	if len(r) > 180 {
		return string(r[:180])
	}
	return t
}

// planLocalCaps 本地容量预检（进报告；blocked 项阻止执行）。
func (s *SheetTransferService) planLocalCaps(snap *CloudSnapshot) []TransferIssue {
	var issues []TransferIssue
	if len(snap.Sheets) > sheet.MaxSheets {
		issues = append(issues, TransferIssue{
			Feature: "工作表数量", Severity: IssueWarning, Action: ActionTruncated,
			Reason: fmt.Sprintf("云端 %d 张，本地只保留前 %d 张", len(snap.Sheets), sheet.MaxSheets),
		})
		snap.Sheets = snap.Sheets[:sheet.MaxSheets]
	}
	totalFormulas := 0
	for _, sh := range snap.Sheets {
		totalFormulas += countKind(sh.Cells, CloudCellFormula)
	}
	if totalFormulas > sheet.MaxFormulasPerBook {
		issues = append(issues, TransferIssue{
			Feature: "公式总数", Severity: IssueBlocked, Action: ActionDropped,
			Reason: fmt.Sprintf("全簿公式 %d 超本地上限 %d，无法完整导入", totalFormulas, sheet.MaxFormulasPerBook),
		})
	}
	return issues
}

// cloudCellsToLocal 云格 → protocol 格（文本截断/超长公式转文本进报告）。
func cloudCellsToLocal(cs CloudSheet, title string) ([]protocol.SheetCell, []TransferIssue) {
	var cells []protocol.SheetCell
	var issues []TransferIssue
	longText, longFormula := 0, 0
	for _, cc := range cs.Cells {
		pc := protocol.SheetCell{R: cc.R, C: cc.C}
		switch cc.Kind {
		case CloudCellText:
			if len(cc.Text) > sheet.MaxTextBytes {
				r := []rune(cc.Text)
				// 字节超限按 rune 截到限内
				for len([]byte(string(r))) > sheet.MaxTextBytes && len(r) > 0 {
					r = r[:len(r)-1]
				}
				pc.Type, pc.Text = "text", string(r)
				longText++
			} else {
				pc.Type, pc.Text = "text", cc.Text
			}
		case CloudCellNumber:
			n := *cc.Number
			pc.Type, pc.Number = "number", &n
		case CloudCellBool:
			b := *cc.Bool
			pc.Type, pc.Bool = "bool", &b
		case CloudCellDatetime:
			pc.Type, pc.Text = "datetime", cc.Text
		case CloudCellFormula:
			f := strings.TrimPrefix(cc.Formula, "=")
			if len([]rune(f)) > sheet.MaxFormulaRunes {
				pc.Type, pc.Text = "text", cc.Formula
				longFormula++
			} else {
				pc.Type, pc.Formula = "formula", f
			}
		default:
			continue
		}
		cells = append(cells, pc)
	}
	if longText > 0 {
		issues = append(issues, TransferIssue{
			Sheet: cs.Name, Feature: "超长文本", Severity: IssueWarning, Action: ActionTruncated,
			Reason: fmt.Sprintf("%d 格文本超 %d 字节上限，已截断", longText, sheet.MaxTextBytes),
		})
	}
	if longFormula > 0 {
		issues = append(issues, TransferIssue{
			Sheet: cs.Name, Feature: "超长公式", Severity: IssueWarning, Action: ActionConverted,
			Reason: fmt.Sprintf("%d 格公式超 %d 字符上限，转文本保留原文", longFormula, sheet.MaxFormulaRunes),
		})
	}
	return cells, issues
}

// BuildExportWorkbook 本地态 → 导出视图（表名过滤 + 汇总注记）。
func BuildExportWorkbook(state doc.DocState, sheetNames []string) (ExportWorkbook, []TransferIssue) {
	want := map[string]bool{}
	for _, n := range sheetNames {
		want[n] = true
	}
	wb := ExportWorkbook{Title: state.Title}
	var issues []TransferIssue
	for _, sh := range state.Workbook.Sheets {
		if len(want) > 0 && !want[sh.Name] {
			continue
		}
		es := ExportSheet{Name: sh.Name, Rows: sh.Rows, Cols: sh.Cols}
		var coords []sheet.Coord
		for c := range sh.Cells {
			coords = append(coords, c)
		}
		sort.Slice(coords, func(i, j int) bool {
			if coords[i].R != coords[j].R {
				return coords[i].R < coords[j].R
			}
			return coords[i].C < coords[j].C
		})
		for _, cd := range coords {
			cell := sh.Cells[cd]
			pc := protocol.SheetCell{R: cd.R, C: cd.C}
			switch {
			case cell.Formula != "":
				pc.Type, pc.Formula = "formula", cell.Formula
			case cell.Value != nil:
				pc.Type = cell.Value.Type
				pc.Text = cell.Value.Text
				pc.Number = cell.Value.Number
				pc.Bool = cell.Value.Bool
			default:
				continue // 纯样式格不迁移
			}
			es.Cells = append(es.Cells, pc)
		}
		if sh.Rows > sheet.MaxRows || sh.Cols > sheet.MaxCols {
			// 本地态本受同限，此处仅防御性记录
			issues = append(issues, TransferIssue{
				Sheet: sh.Name, Feature: "网格范围", Severity: IssueWarning, Action: ActionTruncated,
				Reason: "超出导出网格上限，仅导出上限内内容",
			})
		}
		wb.Sheets = append(wb.Sheets, es)
	}
	return wb, issues
}

// ---- 小工具 ----

// ErrBadDirection / ErrDocRequired / ErrNotSpreadsheet / ErrIdempotencyKeyRequired
var (
	ErrBadDirection           = errors.New("extdocs: 未知交换方向")
	ErrDocRequired            = errors.New("extdocs: 导出需指定 doc_id")
	ErrNotSpreadsheet         = errors.New("extdocs: 目标文档不是表格")
	ErrIdempotencyKeyRequired = errors.New("extdocs: 缺少幂等键")
)

func hasBlocked(issues []TransferIssue) bool {
	for _, is := range issues {
		if is.Severity == IssueBlocked {
			return true
		}
	}
	return false
}

func hasWarning(issues []TransferIssue) bool {
	for _, is := range issues {
		if is.Severity == IssueWarning || is.Severity == IssueBlocked {
			return true
		}
	}
	return false
}

func marshalIssues(issues []TransferIssue) string {
	if len(issues) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(issues)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func previewSheets(snap *CloudSnapshot) []PreviewSheetInfo {
	out := make([]PreviewSheetInfo, 0, len(snap.Sheets))
	for _, sh := range snap.Sheets {
		out = append(out, PreviewSheetInfo{
			Name: sh.Name, Rows: sh.Rows, Cols: sh.Cols,
			Filled: len(sh.Cells), Formulas: countKind(sh.Cells, CloudCellFormula),
			MergeCount: sh.MergeCount,
		})
	}
	return out
}

func exportPreviewSheets(wb ExportWorkbook) []PreviewSheetInfo {
	out := make([]PreviewSheetInfo, 0, len(wb.Sheets))
	for _, es := range wb.Sheets {
		formulas := 0
		for _, c := range es.Cells {
			if c.Type == "formula" {
				formulas++
			}
		}
		out = append(out, PreviewSheetInfo{Name: es.Name, Rows: es.Rows, Cols: es.Cols, Filled: len(es.Cells), Formulas: formulas})
	}
	return out
}

func countKind(cells []CloudCell, kind string) int {
	n := 0
	for _, c := range cells {
		if c.Kind == kind {
			n++
		}
	}
	return n
}
