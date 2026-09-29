// M3 回写引擎 UT：diff 规划矩阵、异步执行时序（取消门闩/传输错停跑/续跑）、
// stale 拒绝、结构不匹配阻断、目标被改 resume 拒绝。
package extdocs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// scriptTarget 脚本回写会话：调用计数、第 N 块失败注入、第 N 块门闩（时序）。
type scriptTarget struct {
	mu      sync.Mutex
	calls   int
	failAt  int   // 第 N 块返回 failErr（1-based；0 = 不失败）
	failErr error // UpstreamError{Code:"unknown"} = 传输类（→unknown）
	gateAt  int   // 第 N 块写前挂起（等 gate 关闭；0 = 不挂）
	gate    chan struct{}
	written []protocol.SheetCell
}

func (t *scriptTarget) WriteChunk(ctx context.Context, sheetName string, cells []protocol.SheetCell) ([]TransferIssue, error) {
	t.mu.Lock()
	t.calls++
	n := t.calls
	t.mu.Unlock()
	if t.gateAt == n && t.gate != nil {
		select {
		case <-t.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if t.failAt == n {
		return nil, t.failErr
	}
	t.mu.Lock()
	t.written = append(t.written, cells...)
	t.mu.Unlock()
	return nil, nil
}

func (t *scriptTarget) VerifySheet(ctx context.Context, sheetName string, expect []protocol.SheetCell) []TransferIssue {
	return nil
}

func (t *scriptTarget) Close() error { return nil }

func (t *scriptTarget) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// newUpdateEnv 回写环境：真 doc.Service 落库 + 桩 provider（target 注入）。
func newUpdateEnv(t *testing.T, snap *CloudSnapshot, target SheetTargetSession) (*SheetTransferService, *stubSheetProvider) {
	t.Helper()
	sp := &stubSheetProvider{id: ProviderTencent, snap: snap, target: target}
	svc, _, _ := newTransferEnv(t, sp)
	return svc, sp
}

// updLocalDoc 造本地表（两块 diff 用）：值差异 + 公式差异 + 等格格。
func updMakeDoc(t *testing.T, svc *SheetTransferService) string {
	t.Helper()
	// 借道导入建本地表（快照 → 本地），再改一格制造差异——比手搓 doc 快。
	snap := &CloudSnapshot{Title: "回写表", Sheets: []CloudSheet{{
		Name: "S1", Rows: 10, Cols: 5, Cells: []CloudCell{
			{R: 1, C: 1, Kind: CloudCellText, Text: "旧值"},
			{R: 1, C: 2, Kind: CloudCellNumber, Number: num(1)},
			{R: 1, C: 3, Kind: CloudCellFormula, Formula: "=SUM(B1:B1)"},
			{R: 2, C: 1, Kind: CloudCellText, Text: "远端独有"}, // 远端多出格
		},
	}}}
	sp := svc.providers.providers[ProviderTencent].(*stubSheetProvider)
	orig := sp.snap // 测试场景快照（导入后还原——预览/续跑读它）
	sp.snap = snap
	rec, err := svc.Execute(context.Background(), TransferRequest{
		Direction: TransferImport, Provider: ProviderTencent,
		URL: "https://docs.qq.com/sheet/abc", IdempotencyKey: fmt.Sprintf("k%d", time.Now().UnixNano()), Actor: "par_owner",
	})
	sp.snap = orig
	if err != nil {
		t.Fatal(err)
	}
	return rec.DocID
}

func waitStatus(t *testing.T, svc *SheetTransferService, id string, want ...string) *SheetTransferRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := svc.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if rec.Status == w {
				return rec
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec, _ := svc.Get(context.Background(), id)
	t.Fatalf("状态未达 %v（当前 %s）", want, rec.Status)
	return nil
}

func TestUpdateDiffPlanning(t *testing.T) {
	wb := ExportWorkbook{Title: "t", Sheets: []ExportSheet{{
		Name: "S", Cells: []protocol.SheetCell{
			{R: 1, C: 1, Type: "text", Text: "同"},                        // 等格（跳过）
			{R: 1, C: 2, Type: "text", Text: "新"},                        // 值差异
			{R: 1, C: 3, Type: "number", Number: num(2)},                 // 远端缺格
			{R: 1, C: 4, Type: "formula", Formula: "SUM(A1:A1)"},         // 公式差异（引号变体等价）
			{R: 1, C: 5, Type: "datetime", Text: "2026-09-28T00:00:00Z"}, // datetime↔text 等价
		},
	}}}
	snap := &CloudSnapshot{Sheets: []CloudSheet{{
		Name: "S", Cells: []CloudCell{
			{R: 1, C: 1, Kind: CloudCellText, Text: "同"},
			{R: 1, C: 2, Kind: CloudCellText, Text: "旧"},
			{R: 1, C: 4, Kind: CloudCellFormula, Formula: "='S'!A1"},        // stripSheetQuotes 后不等（SUM(A1:A1) 不同）
			{R: 1, C: 5, Kind: CloudCellText, Text: "2026-09-28T00:00:00Z"}, // 腾讯降级等价 → 跳过
			{R: 9, C: 9, Kind: CloudCellNumber, Number: num(9)},             // 远端多出
		},
	}}}
	chunks, pre, diffs, issues := planUpdateDiff(wb, snap)
	if len(diffs) != 1 || diffs[0].ToWrite != 3 || diffs[0].FormulaDiffs != 1 || diffs[0].RemoteExtra != 1 {
		t.Fatalf("diffs = %+v", diffs)
	}
	if len(chunks) != 1 || len(chunks[0].Cells) != 3 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if len(pre["S"]) != 2 { // 有远端前态的覆写格 = B1/D1（C1 远端缺格无前态）
		t.Fatalf("prestate = %v", pre)
	}
	var extraInfo bool
	for _, is := range issues {
		if is.Feature == "远端多出格" {
			extraInfo = true
		}
	}
	if !extraInfo {
		t.Fatalf("issues = %v", issues)
	}
	// 结构不匹配 → blocked
	wb2 := wb
	wb2.Sheets = append(wb2.Sheets, ExportSheet{Name: "新表"})
	_, _, _, issues2 := planUpdateDiff(wb2, snap)
	if !hasBlocked(issues2) {
		t.Fatalf("应 blocked: %v", issues2)
	}
}

func TestUpdatePreviewAndExecute(t *testing.T) {
	// 场景快照 = 导入快照（本地 A1 改值后 diff 应恰为 1 格）
	snap := &CloudSnapshot{Sheets: []CloudSheet{{
		Name: "S1", Rows: 10, Cols: 5, Cells: []CloudCell{
			{R: 1, C: 1, Kind: CloudCellText, Text: "旧值"},
			{R: 1, C: 2, Kind: CloudCellNumber, Number: num(1)},
			{R: 1, C: 3, Kind: CloudCellFormula, Formula: "=SUM(B1:B1)"},
			{R: 2, C: 1, Kind: CloudCellText, Text: "远端独有"},
		},
	}}}
	target := &scriptTarget{}
	svc, sp := newUpdateEnv(t, snap, target)
	docID := updMakeDoc(t, svc)
	// 本地改一格制造差异（导入后的 A1 "旧值" → "新值"）
	st, err := svc.docs.GetDoc(context.Background(), docID)
	if err != nil {
		t.Fatal(err)
	}
	sid := st.Workbook.Sheets[0].SheetID
	if _, err := svc.docs.CommitSheetRevision(context.Background(),
		docActorOf(), docID, st.Version,
		[]protocol.SheetOp{{Op: "set_cells", SheetID: sid, Cells: []protocol.SheetCell{
			{R: 1, C: 1, Type: "text", Text: "新值"},
		}}}, "human_editor", "改"); err != nil {
		t.Fatal(err)
	}
	// 预览：差异 = A1 一格
	pv, err := svc.UpdatePreview(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pv.CellsTotal != 1 || len(pv.Diffs) != 1 || pv.Diffs[0].ToWrite != 1 || pv.TargetHash == "" {
		t.Fatalf("preview = %+v", pv)
	}
	// 执行：异步 → 轮询终态
	rec, err := svc.ExecuteUpdate(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
		TargetHash: pv.TargetHash, SourceVersion: pv.SourceVer,
		IdempotencyKey: fmt.Sprintf("u%d", time.Now().UnixNano()), Actor: "par_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitStatus(t, svc, rec.TransferID, TransferSucceeded, TransferSucceededWithWarning)
	if fin.CellsWritten != 1 || fin.CellsTotal != 1 {
		t.Fatalf("fin = %+v", fin)
	}
	if target.count() != 1 || len(target.written) != 1 || target.written[0].Text != "新值" {
		t.Fatalf("written = %+v", target.written)
	}
	// 幂等重放
	again, err := svc.ExecuteUpdate(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
		IdempotencyKey: rec.IdempotencyKey, Actor: "par_owner",
	})
	if err != nil || again.TransferID != rec.TransferID {
		t.Fatalf("replay = %+v err=%v", again, err)
	}
	_ = sp
}

func TestUpdateStaleAndStructure(t *testing.T) {
	snap := &CloudSnapshot{Sheets: []CloudSheet{{Name: "S1", Rows: 5, Cols: 5}}}
	svc, _ := newUpdateEnv(t, snap, &scriptTarget{})
	docID := updMakeDoc(t, svc)
	// stale hash
	_, err := svc.ExecuteUpdate(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
		TargetHash: "deadbeef", IdempotencyKey: "stale-key-1", Actor: "par_owner",
	})
	if !errors.Is(err, ErrStalePreview) {
		t.Fatalf("err = %v（期 stale）", err)
	}
	// 结构不匹配：本地加新表（目标无）
	st, _ := svc.docs.GetDoc(context.Background(), docID)
	if _, err := svc.docs.CommitSheetRevision(context.Background(), docActorOf(), docID, st.Version,
		[]protocol.SheetOp{{Op: "add_sheet", SheetID: "sht_new00001", Name: "新表"}}, "human_editor", "加表"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.UpdatePreview(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
	})
	var blocked *BlockedTransferError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v（期 blocked）", err)
	}
	// provider/URL 归属不符
	_, err = svc.ExecuteUpdate(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://evil.com/sheet/x",
		IdempotencyKey: "host-key-001", Actor: "par_owner",
	})
	if !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("err = %v（期 mismatch）", err)
	}
}

func TestUpdateCancelAndPartialResume(t *testing.T) {
	// 三块差异（600 格 → 3 块 × 500/500/100）——用脚本会话门闩停在第 2 块。
	snap := &CloudSnapshot{Sheets: []CloudSheet{{Name: "S1", Rows: 100, Cols: 10}}}
	target := &scriptTarget{gateAt: 2, gate: make(chan struct{})}
	svc, _ := newUpdateEnv(t, snap, target)
	docID := updMakeDoc(t, svc)
	// 造 1100 格差异（两笔提交——单批 ≤1000）→ 回写分块 500/500/100。
	st, _ := svc.docs.GetDoc(context.Background(), docID)
	sid := st.Workbook.Sheets[0].SheetID
	mk := func(from, to int) []protocol.SheetCell {
		cs := make([]protocol.SheetCell, 0, to-from)
		for i := from; i < to; i++ {
			cs = append(cs, protocol.SheetCell{R: i/10 + 1, C: i%10 + 1, Type: "number", Number: num(float64(i))})
		}
		return cs
	}
	res1, err := svc.docs.CommitSheetRevision(context.Background(), docActorOf(), docID, st.Version,
		[]protocol.SheetOp{{Op: "set_cells", SheetID: sid, Cells: mk(0, 600)}}, "human_editor", "600格")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.docs.CommitSheetRevision(context.Background(), docActorOf(), docID, res1.DocVersion,
		[]protocol.SheetOp{{Op: "set_cells", SheetID: sid, Cells: mk(600, 1100)}}, "human_editor", "500格"); err != nil {
		t.Fatal(err)
	}
	pv, err := svc.UpdatePreview(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pv.CellsTotal != 1100 {
		t.Fatalf("cells_total = %d", pv.CellsTotal)
	}
	rec, err := svc.ExecuteUpdate(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
		TargetHash: pv.TargetHash, SourceVersion: pv.SourceVer,
		IdempotencyKey: fmt.Sprintf("c%d", time.Now().UnixNano()), Actor: "par_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 等第 1 块写完、第 2 块挂起
	deadline := time.Now().Add(3 * time.Second)
	for target.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if target.count() < 2 {
		t.Fatalf("门闩未生效 calls=%d", target.count())
	}
	// 取消（先等取消信号真实落地再放行门闩——确定性时序）：第 2 块完成落
	// 检查点 → 第 3 块前观察到取消 → partial（已写 1000）。
	go func() { _, _ = svc.CancelUpdate(context.Background(), rec.TransferID) }()
	dl := time.Now().Add(3 * time.Second)
	canceled := false
	for time.Now().Before(dl) {
		svc.updMu.Lock()
		ch, ok := svc.cancels[rec.TransferID]
		svc.updMu.Unlock()
		if ok {
			select {
			case <-ch:
				close(target.gate)
				canceled = true
			default:
			}
		}
		if canceled {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !canceled {
		t.Fatal("取消信号未落地")
	}
	fin := waitStatus(t, svc, rec.TransferID, TransferPartial, TransferCanceled)
	if fin.Status != TransferPartial || fin.CellsWritten != 1000 {
		t.Fatalf("fin = %+v（期 partial 已写 1000）", fin)
	}
	// 续跑：无剩余块 → 直接成功
	resumed, err := svc.ResumeUpdate(context.Background(), rec.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	fin2 := waitStatus(t, svc, resumed.TransferID, TransferSucceeded, TransferSucceededWithWarning)
	if fin2.CellsWritten != 1100 {
		t.Fatalf("resume fin = %+v", fin2)
	}
}

func TestUpdateUnknownStopsAndResumeChanged(t *testing.T) {
	// 两块差异；第 2 块传输错（UpstreamError unknown）→ 状态 unknown、第 3 块不发。
	snap := &CloudSnapshot{Sheets: []CloudSheet{{Name: "S1", Rows: 100, Cols: 10}}}
	target := &scriptTarget{failAt: 2, failErr: &UpstreamError{Code: "unknown", Message: "timeout"}}
	svc, sp := newUpdateEnv(t, snap, target)
	docID := updMakeDoc(t, svc)
	st, _ := svc.docs.GetDoc(context.Background(), docID)
	sid := st.Workbook.Sheets[0].SheetID
	mk := func(from, to int) []protocol.SheetCell {
		cs := make([]protocol.SheetCell, 0, to-from)
		for i := from; i < to; i++ {
			cs = append(cs, protocol.SheetCell{R: i/10 + 1, C: i%10 + 1, Type: "number", Number: num(float64(i))})
		}
		return cs
	}
	res1, err := svc.docs.CommitSheetRevision(context.Background(), docActorOf(), docID, st.Version,
		[]protocol.SheetOp{{Op: "set_cells", SheetID: sid, Cells: mk(0, 600)}}, "human_editor", "600格")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.docs.CommitSheetRevision(context.Background(), docActorOf(), docID, res1.DocVersion,
		[]protocol.SheetOp{{Op: "set_cells", SheetID: sid, Cells: mk(600, 1100)}}, "human_editor", "500格"); err != nil {
		t.Fatal(err)
	}
	_ = err
	pv, err := svc.UpdatePreview(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := svc.ExecuteUpdate(context.Background(), UpdateRequest{
		DocID: docID, Provider: ProviderTencent, ExternalURL: "https://docs.qq.com/sheet/abc",
		TargetHash: pv.TargetHash, SourceVersion: pv.SourceVer,
		IdempotencyKey: fmt.Sprintf("k%d", time.Now().UnixNano()), Actor: "par_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitStatus(t, svc, rec.TransferID, TransferUnknown)
	if fin.CellsWritten != 500 {
		t.Fatalf("fin = %+v（期第 1 块 500 已写）", fin)
	}
	if target.count() != 2 {
		t.Fatalf("calls = %d（传输错后不得续跑）", target.count())
	}
	// resume：未写区域（第 2 块的 100 格）目标被第三方改 → 拒绝
	sp.snap = &CloudSnapshot{Sheets: []CloudSheet{{Name: "S1", Rows: 100, Cols: 10, Cells: []CloudCell{
		{R: 51, C: 1, Kind: CloudCellText, Text: "别人改了"}, // 未写区
	}}}}
	if _, err := svc.ResumeUpdate(context.Background(), rec.TransferID); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v（期 ErrTargetChanged）", err)
	}
}

func docActorOf() doc.Actor { return doc.Actor{ParticipantID: "par_owner", Kind: "human"} }
