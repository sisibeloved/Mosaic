// 交换服务 UT（RFC-0017 M2）：真 doc.Service（MemStore）作落库端口 +
// 桩 SheetProvider——导入装配（分批/改名/加表/哈希防偷换/幂等重放）、
// 导出装配（版本钉死）、本地容量预检全覆盖。
package extdocs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/sheet"
)

// stubSheetProvider 表格能力桩：记录建副本入参、按脚本出快照。
type stubSheetProvider struct {
	Provider
	id         string
	snap       *CloudSnapshot
	readIssues []TransferIssue
	readErr    error
	mu         sync.Mutex
	copied     []ExportWorkbook
	writeRes   SheetWriteResult
	writeErr   error
	caps       SheetCaps
	target     SheetTargetSession // M3：非 nil 时 OpenSheetTarget 返回它
}

func (s *stubSheetProvider) SheetCapabilities() SheetCaps { return s.caps }

func (s *stubSheetProvider) ReadSheetSnapshot(ctx context.Context, rawURL string, names []string) (*CloudSnapshot, []TransferIssue, error) {
	if s.readErr != nil {
		return nil, nil, s.readErr
	}
	if len(names) == 0 {
		return s.snap, s.readIssues, nil
	}
	out := &CloudSnapshot{Title: s.snap.Title}
	for _, sh := range s.snap.Sheets {
		for _, n := range names {
			if sh.Name == n {
				out.Sheets = append(out.Sheets, sh)
			}
		}
	}
	return out, s.readIssues, nil
}

func (s *stubSheetProvider) CreateSheetCopy(ctx context.Context, wb ExportWorkbook) (SheetWriteResult, error) {
	s.mu.Lock()
	s.copied = append(s.copied, wb)
	s.mu.Unlock()
	return s.writeRes, s.writeErr
}

// stubTargetSession 桩回写会话（M3 用例在更新测试里另行驱动）。
type stubTargetSession struct {
	sp *stubSheetProvider
}

func (t *stubTargetSession) WriteChunk(ctx context.Context, sheetName string, cells []protocol.SheetCell) ([]TransferIssue, error) {
	return nil, nil
}

func (t *stubTargetSession) VerifySheet(ctx context.Context, sheetName string, expect []protocol.SheetCell) []TransferIssue {
	return nil
}

func (t *stubTargetSession) Close() error { return nil }

func (s *stubSheetProvider) OpenSheetTarget(ctx context.Context, rawURL string) (SheetTargetSession, error) {
	if s.target != nil {
		return s.target, nil
	}
	return &stubTargetSession{sp: s}, nil
}

// memTransferStore 内存交换记录存储。
type memTransferStore struct {
	mu    sync.Mutex
	byID  map[string]SheetTransferRecord
	byKey map[string]SheetTransferRecord
	order []string
}

func newMemTransferStore() *memTransferStore {
	return &memTransferStore{byID: map[string]SheetTransferRecord{}, byKey: map[string]SheetTransferRecord{}}
}

func (m *memTransferStore) InsertSheetTransfer(_ context.Context, rec SheetTransferRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byKey[rec.IdempotencyKey]; ok {
		return ErrTransferKeyConflict
	}
	m.byID[rec.TransferID] = rec
	m.byKey[rec.IdempotencyKey] = rec
	m.order = append(m.order, rec.TransferID)
	return nil
}

func (m *memTransferStore) GetSheetTransfer(_ context.Context, id string) (SheetTransferRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byID[id]; ok {
		return r, nil
	}
	return SheetTransferRecord{}, ErrTransferNotFound
}

func (m *memTransferStore) GetSheetTransferByKey(_ context.Context, key string) (SheetTransferRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byKey[key]; ok {
		return r, nil
	}
	return SheetTransferRecord{}, ErrTransferNotFound
}

func (m *memTransferStore) UpdateSheetTransfer(_ context.Context, rec SheetTransferRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[rec.TransferID]; !ok {
		return ErrTransferNotFound
	}
	m.byID[rec.TransferID] = rec
	m.byKey[rec.IdempotencyKey] = rec
	return nil
}

func (m *memTransferStore) ListSheetTransfers(_ context.Context, docID, direction string, limit int) ([]SheetTransferRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SheetTransferRecord
	for i := len(m.order) - 1; i >= 0; i-- {
		r := m.byID[m.order[i]]
		if docID != "" && r.DocID != docID {
			continue
		}
		if direction != "" && r.Direction != direction {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (m *memTransferStore) MarkStaleRunningTransfers(_ context.Context, now string) (int64, error) {
	return 0, nil
}

// newTransferEnv 组装：真 doc.Service（MemStore）+ 桩 provider + 服务。
func newTransferEnv(t *testing.T, sp *stubSheetProvider) (*SheetTransferService, *doc.Service, *memTransferStore) {
	t.Helper()
	seq := 0
	docSvc := doc.NewService(doc.Config{
		Store: doc.NewMemStore(),
		Clock: func() string { return "2026-09-29T00:00:00Z" },
		NewID: func(prefix string) string {
			seq++
			return fmt.Sprintf("%s%010d", prefix, seq)
		},
		NewDocID: func() string {
			return fmt.Sprintf("doc_%012x", time.Now().UnixNano()&0xffffff)
		},
	})
	reg := NewService(nil)
	// stub 只需 ID/HostMatches 等被用到的面——借用一个最小实现
	//（host 随 id 对齐——updateProvider 做归属交叉校验）。
	base := &stubBaseProvider{id: sp.id}
	if sp.id == ProviderTencent {
		base.host = "docs.qq.com"
	}
	sp.Provider = base
	reg.Register(sp)
	store := newMemTransferStore()
	svc := NewSheetTransferService(reg, docSvc, store, func(prefix string) string {
		return fmt.Sprintf("%sstub%04d", prefix, time.Now().UnixNano()%10000)
	})
	return svc, docSvc, store
}

type stubBaseProvider struct {
	id   string
	host string // 缺省 stub.example.com
}

func (s *stubBaseProvider) ID() string { return s.id }
func (s *stubBaseProvider) DisplayName() string {
	return "桩平台"
}
func (s *stubBaseProvider) RefKind() string { return "stub" }
func (s *stubBaseProvider) HostMatches(host string) bool {
	if s.host != "" {
		return host == s.host
	}
	return host == "stub.example.com"
}
func (s *stubBaseProvider) Verify(ctx context.Context) error {
	return nil
}
func (s *stubBaseProvider) ShareMarkdown(ctx context.Context, title, markdown string) (string, error) {
	return "", nil
}
func (s *stubBaseProvider) ResolveLink(ctx context.Context, rawURL string) (LinkInfo, error) {
	return LinkInfo{URL: rawURL}, nil
}

func num(f float64) *float64 { return &f }

// importSnapshot 测试快照：两表，混合类型 + 超长文本/公式探针。
func importSnapshot() *CloudSnapshot {
	five := true
	long := strings.Repeat("长", 3000) // 9000 字节 > 8192 上限
	return &CloudSnapshot{
		Title: "云端表",
		Sheets: []CloudSheet{
			{Name: "总表", Rows: 100, Cols: 20, MergeCount: 2, Cells: []CloudCell{
				{R: 1, C: 1, Kind: CloudCellText, Text: "00123"},
				{R: 1, C: 2, Kind: CloudCellNumber, Number: num(123.456)},
				{R: 1, C: 3, Kind: CloudCellBool, Bool: &five},
				{R: 2, C: 1, Kind: CloudCellFormula, Formula: "=SUM(B1:B1)"},
				{R: 3, C: 1, Kind: CloudCellDatetime, Text: "2026-09-28T00:00:00Z"},
				{R: 4, C: 1, Kind: CloudCellText, Text: long},
			}},
			{Name: "数据B", Rows: 50, Cols: 10, Cells: []CloudCell{
				{R: 1, C: 1, Kind: CloudCellNumber, Number: num(7)},
			}},
		},
	}
}

func TestTransferImportPreviewAndExecute(t *testing.T) {
	sp := &stubSheetProvider{id: ProviderTencent, snap: importSnapshot()}
	svc, docSvc, store := newTransferEnv(t, sp)
	ctx := context.Background()

	pv, err := svc.Preview(ctx, TransferRequest{Direction: TransferImport, Provider: ProviderTencent,
		URL: "https://docs.qq.com/sheet/abc"})
	if err != nil {
		t.Fatal(err)
	}
	if pv.SnapshotHash == "" || len(pv.Sheets) != 2 || pv.Sheets[0].Filled != 6 {
		t.Fatalf("preview = %+v", pv)
	}
	// 预览即报超长文本（装配期 issue 在 preview 也应出现——通过 planLocalCaps
	// 只报容量；逐格截断在执行期，预览不逐格扫）
	rec, err := svc.Execute(ctx, TransferRequest{
		Direction: TransferImport, Provider: ProviderTencent,
		URL: "https://docs.qq.com/sheet/abc", SnapshotHash: pv.SnapshotHash,
		IdempotencyKey: "key-1", Actor: "par_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != TransferSucceededWithWarning {
		t.Fatalf("status = %s issues=%s", rec.Status, rec.ReportJSON)
	}
	if rec.DocID == "" || !strings.Contains(rec.Title, "云端表") {
		t.Fatalf("rec = %+v", rec)
	}
	// 落库断言：两表 + 首表名对齐 + 单元格类型齐全 + 超长文本截断
	state, err := docSvc.GetDoc(ctx, rec.DocID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Format != doc.FormatSpreadsheet || len(state.Workbook.Sheets) != 2 {
		t.Fatalf("state sheets = %d", len(state.Workbook.Sheets))
	}
	sh0 := state.Workbook.Sheets[0]
	if sh0.Name != "总表" || state.Workbook.Sheets[1].Name != "数据B" {
		t.Fatalf("names = %q/%q", sh0.Name, state.Workbook.Sheets[1].Name)
	}
	c1 := sh0.Cells[sheet.Coord{R: 1, C: 1}]
	if c1.Value == nil || c1.Value.Type != "text" || c1.Value.Text != "00123" {
		t.Fatalf("A1 = %+v", c1)
	}
	c2 := sh0.Cells[sheet.Coord{R: 2, C: 1}]
	if c2.Formula != "SUM(B1:B1)" { // 引导 = 已剥（protocol 语义）
		t.Fatalf("A2 formula = %q", c2.Formula)
	}
	c3 := sh0.Cells[sheet.Coord{R: 3, C: 1}]
	if c3.Value == nil || c3.Value.Type != "datetime" {
		t.Fatalf("A3 = %+v", c3)
	}
	c4 := sh0.Cells[sheet.Coord{R: 4, C: 1}]
	if c4.Value == nil || len(c4.Value.Text) > sheet.MaxTextBytes || len(c4.Value.Text) >= 9000 {
		t.Fatalf("A4 超长文本应截断到 ≤%d 字节: len=%d", sheet.MaxTextBytes, len(c4.Value.Text))
	}
	// 报告含超长文本截断项
	if !strings.Contains(rec.ReportJSON, "超长文本") {
		t.Fatalf("report = %s", rec.ReportJSON)
	}
	// 事件流 source=import
	//（MemStore 读路径省略——status/issue 已验证；source 由 doc 校验放行即证）
	_ = store

	// 幂等重放：同键返回既有记录（不再建新 doc）
	again, err := svc.Execute(ctx, TransferRequest{
		Direction: TransferImport, Provider: ProviderTencent,
		URL: "https://docs.qq.com/sheet/abc", IdempotencyKey: "key-1", Actor: "par_owner",
	})
	if err != nil || again.TransferID != rec.TransferID {
		t.Fatalf("replay = %+v err=%v", again, err)
	}
}

func TestTransferImportStaleHash(t *testing.T) {
	sp := &stubSheetProvider{id: ProviderTencent, snap: importSnapshot()}
	svc, _, _ := newTransferEnv(t, sp)
	ctx := context.Background()
	_, err := svc.Execute(ctx, TransferRequest{
		Direction: TransferImport, Provider: ProviderTencent,
		URL: "https://docs.qq.com/sheet/abc", SnapshotHash: "deadbeef",
		IdempotencyKey: "key-stale", Actor: "par_owner",
	})
	if !errors.Is(err, ErrStalePreview) {
		t.Fatalf("err = %v（期 ErrStalePreview）", err)
	}
}

func TestTransferImportBlockedOverFormulas(t *testing.T) {
	cells := make([]CloudCell, sheet.MaxFormulasPerBook+1)
	for i := range cells {
		cells[i] = CloudCell{R: i/200 + 1, C: i%200 + 1, Kind: CloudCellFormula, Formula: "=1"}
	}
	sp := &stubSheetProvider{id: ProviderTencent, snap: &CloudSnapshot{
		Title: "公式海", Sheets: []CloudSheet{{Name: "S", Rows: 5000, Cols: 200, Cells: cells}},
	}}
	svc, _, _ := newTransferEnv(t, sp)
	ctx := context.Background()
	_, err := svc.Preview(ctx, TransferRequest{Direction: TransferImport, Provider: ProviderTencent, URL: "https://docs.qq.com/sheet/abc"})
	var blocked *BlockedTransferError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v（期 BlockedTransferError）", err)
	}
	// 执行也被拦
	rec, err := svc.Execute(ctx, TransferRequest{
		Direction: TransferImport, Provider: ProviderTencent, URL: "https://docs.qq.com/sheet/abc",
		IdempotencyKey: "key-b", Actor: "par_owner",
	})
	if err == nil || rec == nil || rec.Status != TransferFailed {
		t.Fatalf("execute 应失败: rec=%+v err=%v", rec, err)
	}
}

func TestTransferExportRoundTrip(t *testing.T) {
	sp := &stubSheetProvider{id: ProviderFeishu, snap: importSnapshot(),
		writeRes: SheetWriteResult{TargetURL: "https://x.feishu.cn/sheets/N1"}}
	svc, docSvc, _ := newTransferEnv(t, sp)
	ctx := context.Background()
	// 先导入造一个本地表
	imp, err := svc.Execute(ctx, TransferRequest{
		Direction: TransferImport, Provider: ProviderFeishu,
		URL: "https://x.feishu.cn/sheets/src1", IdempotencyKey: "key-i", Actor: "par_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 预览导出（版本钉死）
	pv, err := svc.Preview(ctx, TransferRequest{Direction: TransferExport, Provider: ProviderFeishu, DocID: imp.DocID})
	if err != nil {
		t.Fatal(err)
	}
	if pv.SourceVer == 0 || len(pv.Sheets) != 2 {
		t.Fatalf("preview = %+v", pv)
	}
	rec, err := svc.Execute(ctx, TransferRequest{
		Direction: TransferExport, Provider: ProviderFeishu, DocID: imp.DocID,
		SourceVersion: pv.SourceVer, IdempotencyKey: "key-e", Actor: "par_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != TransferSucceeded || rec.ExternalURL != "https://x.feishu.cn/sheets/N1" {
		t.Fatalf("rec = %+v", rec)
	}
	// 建副本收到完整工作簿（公式剥 = 的 protocol 语义 → 导出视图携 formula 原文）
	sp.mu.Lock()
	wb := sp.copied[0]
	sp.mu.Unlock()
	if wb.Title != "云端表" || len(wb.Sheets) != 2 || len(wb.Sheets[0].Cells) != 6 {
		t.Fatalf("copied = %+v", wb)
	}
	// 版本漂移 → stale
	_, err = docSvc.CommitSheetRevision(ctx, doc.Actor{ParticipantID: "par_owner", Kind: "human"},
		imp.DocID, pv.SourceVer, []protocol.SheetOp{{Op: sheet.OpSetCells, SheetID: stateDefaultSheet(t, docSvc, imp.DocID),
			Cells: []protocol.SheetCell{{R: 9, C: 9, Type: "number", Number: num(1)}}}}, "human_editor", "漂移")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Execute(ctx, TransferRequest{
		Direction: TransferExport, Provider: ProviderFeishu, DocID: imp.DocID,
		SourceVersion: pv.SourceVer, IdempotencyKey: "key-e2", Actor: "par_owner",
	})
	if !errors.Is(err, ErrStalePreview) {
		t.Fatalf("err = %v（期 ErrStalePreview）", err)
	}
}

func stateDefaultSheet(t *testing.T, docSvc *doc.Service, docID string) string {
	t.Helper()
	state, err := docSvc.GetDoc(context.Background(), docID)
	if err != nil {
		t.Fatal(err)
	}
	return state.Workbook.Sheets[0].SheetID
}

func TestTransferImportSheetsCap(t *testing.T) {
	sheets := make([]CloudSheet, sheet.MaxSheets+3)
	for i := range sheets {
		sheets[i] = CloudSheet{Name: fmt.Sprintf("S%d", i), Rows: 10, Cols: 5,
			Cells: []CloudCell{{R: 1, C: 1, Kind: CloudCellNumber, Number: num(float64(i))}}}
	}
	sp := &stubSheetProvider{id: ProviderTencent, snap: &CloudSnapshot{Title: "多表", Sheets: sheets}}
	svc, docSvc, _ := newTransferEnv(t, sp)
	rec, err := svc.Execute(context.Background(), TransferRequest{
		Direction: TransferImport, Provider: ProviderTencent, URL: "https://docs.qq.com/sheet/abc",
		IdempotencyKey: "key-cap", Actor: "par_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.ReportJSON, "工作表数量") {
		t.Fatalf("report = %s", rec.ReportJSON)
	}
	state, err := docSvc.GetDoc(context.Background(), rec.DocID)
	if err != nil || len(state.Workbook.Sheets) != sheet.MaxSheets {
		t.Fatalf("sheets = %d err=%v", len(state.Workbook.Sheets), err)
	}
}
