// 表格回写引擎（RFC-0017 M3）：更新已有远端目标 = 值域差异覆写（结构不动、
// 远端多出格不清除），异步分块执行 + 检查点 + 取消/续跑 + unknown 收敛。
// 恢复语义：执行前快照差异格前态（≤256KiB 持久化）；resume 校验未写区域
// 仍等于前态（目标被第三方改动 → 拒绝重新预览），已写区域不追究（用户
// 后续编辑属正常）。取消不撤销已写（partial + 已写量）。
package extdocs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/sheet"
)

// M3 错误哨兵。
var (
	// ErrTargetChanged resume 时目标未写区域已变（第三方编辑）——须重新预览。
	ErrTargetChanged = errors.New("extdocs: 目标已被修改（未写区域变化），请重新预览")
	// ErrTransferNotResumable 记录不可续跑（终态 / 无前态 / 无状态）。
	ErrTransferNotResumable = errors.New("extdocs: 该交换记录不可续跑")
	// ErrProviderMismatch URL 与指定 provider 归属不符。
	ErrProviderMismatch = errors.New("extdocs: 目标链接与 provider 归属不符")
)

// 回写常量。
const (
	// updateChunkCells 服务侧分块粒度（腾讯 set_range_value 单发建议值的
	// 保守量级；provider 内部再细分）。
	updateChunkCells = 500
	// updateMaxCells 单次回写差异格上限（对齐读面上限）。
	updateMaxCells = 10000
	// updatePrestateMaxBytes 前态持久化上限（超出 = 禁 resume，仅报告注记）。
	updatePrestateMaxBytes = 256 << 10
	// updateCancelWaitMS Cancel 端点等待落库的轮询窗。
	updateCancelWaitMS = 3000
)

// UpdateDiffSheet 单表差异摘要。
type UpdateDiffSheet struct {
	Name         string `json:"name"`
	ToWrite      int    `json:"to_write"`      // 将覆写的差异格数
	FormulaDiffs int    `json:"formula_diffs"` // 其中公式格数
	RemoteExtra  int    `json:"remote_extra"`  // 远端多出格（不清除，仅注记）
}

// UpdatePreview 回写预览产物。
type UpdatePreview struct {
	Direction   string            `json:"direction"`
	Provider    string            `json:"provider"`
	DocID       string            `json:"doc_id"`
	ExternalURL string            `json:"external_url"`
	Title       string            `json:"title"`
	SourceVer   int64             `json:"source_version"`
	TargetHash  string            `json:"target_hash"`
	CellsTotal  int               `json:"cells_total"`
	Diffs       []UpdateDiffSheet `json:"diffs"`
	Issues      []TransferIssue   `json:"issues"`
}

// UpdateRequest 回写预览/执行参数。
type UpdateRequest struct {
	DocID       string
	Provider    string
	ExternalURL string
	SheetNames  []string
	Actor       string
	// ExecuteOnly：
	TargetHash     string
	SourceVersion  int64
	IdempotencyKey string
}

// updateChunk 一块覆写（表内行列有序；块序全序确定——resume 依赖）。
type updateChunk struct {
	Sheet string               `json:"sheet"`
	Cells []protocol.SheetCell `json:"cells"`
}

// updateState 持久化执行状态（sheet_transfers.state_json；恢复数据不出网）。
type updateState struct {
	Provider    string                          `json:"provider"`
	ExternalURL string                          `json:"external_url"`
	DocID       string                          `json:"doc_id"`
	Title       string                          `json:"title"`
	SheetNames  []string                        `json:"sheet_names"`
	TargetHash  string                          `json:"target_hash"`
	Chunks      []updateChunk                   `json:"chunks"`
	Prestate    map[string]map[string]CloudCell `json:"prestate,omitempty"` // sheet → "r,c" → 覆写前远端值
	HasPrestate bool                            `json:"has_prestate"`
	Done        int                             `json:"done"`
	Issues      []TransferIssue                 `json:"issues,omitempty"`
}

// ---- diff 规划 ----

// planUpdateDiff 本地态 × 目标快照 → 差异覆写计划。结构不匹配（本地选中表
// 在目标不存在）→ blocked issue（整单拒绝）；远端多出格计数不清除。
func planUpdateDiff(wb ExportWorkbook, snap *CloudSnapshot) (chunks []updateChunk, prestate map[string]map[string]CloudCell, diffs []UpdateDiffSheet, issues []TransferIssue) {
	target := map[string]CloudSheet{}
	for _, cs := range snap.Sheets {
		target[cs.Name] = cs
	}
	prestate = map[string]map[string]CloudCell{}
	total := 0
	for _, es := range wb.Sheets {
		ts, ok := target[es.Name]
		if !ok {
			issues = append(issues, TransferIssue{
				Sheet: es.Name, Feature: "结构不匹配", Severity: IssueBlocked, Action: ActionDropped,
				Reason: "目标缺少同名工作表（回写不传播结构——请重新选择范围或导出新副本）",
			})
			continue
		}
		remote := map[string]CloudCell{}
		for _, cc := range ts.Cells {
			remote[coordKey(cc.R, cc.C)] = cc
		}
		var toWrite []protocol.SheetCell
		pre := map[string]CloudCell{}
		localKeys := map[string]bool{}
		formulaDiffs, remoteExtra := 0, 0
		for _, lc := range es.Cells {
			if lc.R < 1 || lc.C < 1 {
				continue
			}
			key := coordKey(lc.R, lc.C)
			localKeys[key] = true
			rc, exists := remote[key]
			if exists && updateCellEqual(lc, rc) {
				continue // 无差异
			}
			if exists {
				pre[key] = rc
			}
			if lc.Type == "formula" {
				formulaDiffs++
			}
			toWrite = append(toWrite, lc)
		}
		for key := range remote {
			if !localKeys[key] {
				remoteExtra++
			}
		}
		if len(pre) > 0 {
			prestate[es.Name] = pre
		}
		if remoteExtra > 0 {
			issues = append(issues, TransferIssue{
				Sheet: es.Name, Feature: "远端多出格", Severity: IssueInfo, Action: ActionKept,
				Reason: fmt.Sprintf("%d 格远端有值而本地为空——不清除（回写只覆写差异格）", remoteExtra),
			})
		}
		diffs = append(diffs, UpdateDiffSheet{Name: es.Name, ToWrite: len(toWrite), FormulaDiffs: formulaDiffs, RemoteExtra: remoteExtra})
		total += len(toWrite)
		// 表内分块（行列序已在 BuildExportWorkbook 排序）
		for start := 0; start < len(toWrite); start += updateChunkCells {
			end := min(start+updateChunkCells, len(toWrite))
			chunks = append(chunks, updateChunk{Sheet: es.Name, Cells: toWrite[start:end]})
		}
	}
	if total > updateMaxCells {
		issues = append(issues, TransferIssue{
			Feature: "回写范围", Severity: IssueBlocked, Action: ActionDropped,
			Reason: fmt.Sprintf("差异格 %d 超单次回写上限 %d（请缩小工作表选择）", total, updateMaxCells),
		})
	}
	return chunks, prestate, diffs, issues
}

// updateCellEqual 本地格与远端格语义相等判定（含 datetime↔text 等价——腾讯
// 副本的日期按设计降级为文本，逐次回写不得反复重写同样的降级结果）。
func updateCellEqual(lc protocol.SheetCell, rc CloudCell) bool {
	switch lc.Type {
	case "text":
		return rc.Kind == CloudCellText && rc.Text == lc.Text
	case "number":
		return rc.Kind == CloudCellNumber && lc.Number != nil && rc.Number != nil && numEqual(*rc.Number, *lc.Number)
	case "bool":
		return rc.Kind == CloudCellBool && rc.Bool != nil && *rc.Bool == (lc.Bool != nil && *lc.Bool)
	case "datetime":
		return (rc.Kind == CloudCellDatetime || rc.Kind == CloudCellText) && rc.Text == lc.Text
	case "formula":
		return rc.Kind == CloudCellFormula &&
			sheet.CanonicalFormula(stripSheetQuotes(rc.Formula)) == sheet.CanonicalFormula(stripSheetQuotes("="+lc.Formula))
	default:
		return false
	}
}

func coordKey(r, c int) string { return fmt.Sprintf("%d,%d", r, c) }

// ---- 预览 ----

// UpdatePreview 回写预览：读目标快照 + 本地态 → 差异摘要 + 目标哈希锚点。
func (s *SheetTransferService) UpdatePreview(ctx context.Context, req UpdateRequest) (*UpdatePreview, error) {
	sp, err := s.updateProvider(req.Provider, req.ExternalURL)
	if err != nil {
		return nil, err
	}
	state, err := s.docs.GetDoc(ctx, req.DocID)
	if err != nil {
		return nil, err
	}
	if state.Format != doc.FormatSpreadsheet || state.Workbook == nil {
		return nil, ErrNotSpreadsheet
	}
	snap, _, err := sp.ReadSheetSnapshot(ctx, req.ExternalURL, req.SheetNames)
	if err != nil {
		return nil, err
	}
	wb, _ := BuildExportWorkbook(state, req.SheetNames)
	chunks, _, diffs, issues := planUpdateDiff(wb, snap)
	if hasBlocked(issues) {
		return nil, &BlockedTransferError{Issues: issues}
	}
	total := 0
	for _, ch := range chunks {
		total += len(ch.Cells)
	}
	return &UpdatePreview{
		Direction: TransferUpdate, Provider: sp.ID(),
		DocID: req.DocID, ExternalURL: req.ExternalURL, Title: state.Title,
		SourceVer: state.Version, TargetHash: SnapshotHash(snap),
		CellsTotal: total, Diffs: diffs, Issues: issues,
	}, nil
}

// updateProvider 回写 provider 解析（必须显式指定；URL 归属交叉校验）。
func (s *SheetTransferService) updateProvider(providerID, externalURL string) (SheetProvider, error) {
	if providerID == "" || externalURL == "" {
		return nil, ErrBadLink
	}
	p, ok := s.providers.Provider(providerID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}
	u, err := parseURL(externalURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || !p.HostMatches(u.Hostname()) {
		return nil, ErrProviderMismatch
	}
	sp, ok := p.(SheetProvider)
	if !ok {
		return nil, &UpstreamError{Code: "sheets_unsupported", Message: "该外部服务不支持表格交换"}
	}
	return sp, nil
}

// ---- 执行（异步） ----

// ExecuteUpdate 回写执行（同步预检 + 异步分块写入）。幂等重放同 M2。
func (s *SheetTransferService) ExecuteUpdate(ctx context.Context, req UpdateRequest) (*SheetTransferRecord, error) {
	if req.IdempotencyKey == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	if s.store != nil {
		if existing, err := s.store.GetSheetTransferByKey(ctx, req.IdempotencyKey); err == nil {
			return &existing, nil
		} else if !errors.Is(err, ErrTransferNotFound) {
			return nil, err
		}
	}
	sp, err := s.updateProvider(req.Provider, req.ExternalURL)
	if err != nil {
		return nil, err
	}
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
	snap, _, err := sp.ReadSheetSnapshot(ctx, req.ExternalURL, req.SheetNames)
	if err != nil {
		return nil, err
	}
	if req.TargetHash != "" && req.TargetHash != SnapshotHash(snap) {
		return nil, fmt.Errorf("%w: 目标内容已变化，请重新预览", ErrStalePreview)
	}
	wb, _ := BuildExportWorkbook(state, req.SheetNames)
	chunks, prestate, _, issues := planUpdateDiff(wb, snap)
	if hasBlocked(issues) {
		return nil, &BlockedTransferError{Issues: issues}
	}
	now := s.Clock().Format(time.RFC3339)
	total := 0
	for _, ch := range chunks {
		total += len(ch.Cells)
	}
	ust := updateState{
		Provider: sp.ID(), ExternalURL: req.ExternalURL, DocID: req.DocID,
		Title: state.Title, SheetNames: req.SheetNames, TargetHash: SnapshotHash(snap),
		Chunks: chunks, Issues: issues,
	}
	// 前态持久化（≤上限；超限禁 resume 仅注记）。
	hasPre := false
	if raw, merr := json.Marshal(ust); merr == nil && len(raw)+prestateBytes(prestate) <= updatePrestateMaxBytes {
		ust.Prestate, ust.HasPrestate, hasPre = prestate, true, true
	}
	if !hasPre && total > 0 {
		ust.Issues = append(ust.Issues, TransferIssue{
			Feature: "目标前态", Severity: IssueWarning, Action: ActionDropped,
			Reason: fmt.Sprintf("差异前态超 %dKiB 持久化上限——中断后不可续跑，需重新预览执行", updatePrestateMaxBytes>>10),
		})
	}
	rec := SheetTransferRecord{
		TransferID: s.newID("xfr"), IdempotencyKey: req.IdempotencyKey,
		Direction: TransferUpdate, Provider: sp.ID(), Status: TransferRunning,
		DocID: req.DocID, ExternalURL: req.ExternalURL, Title: state.Title,
		SourceVersion: state.Version, SnapshotHash: ust.TargetHash,
		CellsTotal: total, ReportJSON: "[]", CreatedAt: now, UpdatedAt: now,
	}
	rec = withUpdateState(rec, &ust)
	if s.store != nil {
		if err := s.store.InsertSheetTransfer(ctx, rec); err != nil {
			return nil, err
		}
	}
	s.launchUpdate(rec.TransferID)
	if total == 0 { // 无差异即完成（runUpdate 也会收敛，但同步给终态更友好）
		s.finishNoDiff(&rec, &ust)
		return &rec, nil
	}
	return &rec, nil
}

// launchUpdate 注册取消通道并起异步执行（resume 复用同入口）。
func (s *SheetTransferService) launchUpdate(transferID string) {
	s.updMu.Lock()
	if s.cancels == nil {
		s.cancels = map[string]chan struct{}{}
	}
	if _, ok := s.cancels[transferID]; !ok {
		s.cancels[transferID] = make(chan struct{})
	}
	ch := s.cancels[transferID]
	s.updMu.Unlock()
	go s.runUpdate(transferID, ch)
}

// runUpdate 异步主体：逐块写 → 检查点；取消/传输错按语义落终态。
func (s *SheetTransferService) runUpdate(transferID string, cancelCh chan struct{}) {
	rec, ust, err := s.loadUpdate(transferID)
	if err != nil {
		s.Logger.Error("extdocs: 回写状态装载失败", "transfer_id", transferID, "err", err.Error())
		return
	}
	defer s.dropCancel(transferID)
	sp, err := s.updateProvider(rec.Provider, rec.ExternalURL)
	if err != nil {
		s.landUpdate(rec, ust, TransferFailed, err.Error())
		return
	}
	session, err := sp.OpenSheetTarget(context.Background(), rec.ExternalURL)
	if err != nil {
		s.landUpdate(rec, ust, TransferFailed, err.Error())
		return
	}
	defer session.Close()
	expectBySheet := map[string][]protocol.SheetCell{}
	for i := ust.Done; i < len(ust.Chunks); i++ {
		select {
		case <-cancelCh:
			status := TransferCanceled
			if rec.CellsWritten > 0 {
				status = TransferPartial
			}
			s.landUpdate(rec, ust, status, "已取消（已写部分保留）")
			return
		default:
		}
		ch := ust.Chunks[i]
		ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout*2)
		_, werr := session.WriteChunk(ctx, ch.Sheet, ch.Cells)
		cancel()
		if werr != nil {
			var ue *UpstreamError
			if errors.As(werr, &ue) && ue.Code == "unknown" {
				// 传输层失败：本块结果不确定 → unknown 停（不盲目续跑）。
				s.landUpdate(rec, ust, TransferUnknown, "写入结果未知（传输中断）: "+truncate(werr.Error(), 200))
				return
			}
			status := TransferPartial
			if rec.CellsWritten == 0 && ust.Done == 0 {
				status = TransferFailed
			}
			s.landUpdate(rec, ust, status, "写入失败: "+truncate(werr.Error(), 200))
			return
		}
		expectBySheet[ch.Sheet] = append(expectBySheet[ch.Sheet], ch.Cells...)
		ust.Done = i + 1
		rec.CellsWritten += len(ch.Cells)
		rec.UpdatedAt = s.Clock().Format(time.RFC3339)
		*rec = withUpdateState(*rec, ust)
		if s.store != nil {
			if perr := s.store.UpdateSheetTransfer(context.Background(), *rec); perr != nil {
				s.Logger.Error("extdocs: 回写检查点落库失败", "transfer_id", transferID, "err", perr.Error())
			}
		}
	}
	// 全块完成 → 回读校验。
	for sheetName, expect := range expectBySheet {
		ust.Issues = append(ust.Issues, session.VerifySheet(context.Background(), sheetName, expect)...)
	}
	status := TransferSucceeded
	if hasWarning(ust.Issues) {
		status = TransferSucceededWithWarning
	}
	s.landUpdate(rec, ust, status, "")
}

// finishNoDiff 零差异直接终态（不进异步）。
func (s *SheetTransferService) finishNoDiff(rec *SheetTransferRecord, ust *updateState) {
	rec.Status = TransferSucceeded
	rec.ReportJSON = marshalIssues(ust.Issues)
	rec.UpdatedAt = s.Clock().Format(time.RFC3339)
	if s.store != nil {
		_ = s.store.UpdateSheetTransfer(context.Background(), *rec)
	}
	s.dropCancel(rec.TransferID)
}

// landUpdate 终态落库（报告 = 计划 issue + 块转换 issue + 回读差异）。
func (s *SheetTransferService) landUpdate(rec *SheetTransferRecord, ust *updateState, status, errMsg string) {
	rec.Status = status
	if errMsg != "" {
		rec.Error = truncate(errMsg, 500)
	}
	rec.ReportJSON = marshalIssues(ust.Issues)
	rec.UpdatedAt = s.Clock().Format(time.RFC3339)
	*rec = withUpdateState(*rec, ust) // 状态随行（resume 依赖）
	if s.store != nil {
		if err := s.store.UpdateSheetTransfer(context.Background(), *rec); err != nil {
			s.Logger.Error("extdocs: 回写终态落库失败", "transfer_id", rec.TransferID, "err", err.Error())
		}
	}
	s.Logger.Info("extdocs: 表格回写终态", "transfer_id", rec.TransferID, "status", status,
		"cells_written", rec.CellsWritten, "cells_total", rec.CellsTotal, "err", errMsg)
}

// loadUpdate 记录 + 状态装载。
func (s *SheetTransferService) loadUpdate(transferID string) (*SheetTransferRecord, *updateState, error) {
	if s.store == nil {
		return nil, nil, ErrTransferNotFound
	}
	rec, err := s.store.GetSheetTransfer(context.Background(), transferID)
	if err != nil {
		return nil, nil, err
	}
	var ust updateState
	if err := json.Unmarshal([]byte(rec.StateJSON), &ust); err != nil {
		return nil, nil, fmt.Errorf("extdocs: 回写状态不可解析: %w", err)
	}
	return &rec, &ust, nil
}

// withUpdateState 状态序列化随记录（store 持久）。
func withUpdateState(rec SheetTransferRecord, ust *updateState) SheetTransferRecord {
	raw, err := json.Marshal(ust)
	if err == nil {
		rec.StateJSON = string(raw)
	}
	return rec
}

func prestateBytes(prestate map[string]map[string]CloudCell) int {
	raw, err := json.Marshal(prestate)
	if err != nil {
		return 1 << 30
	}
	return len(raw)
}

func (s *SheetTransferService) dropCancel(transferID string) {
	s.updMu.Lock()
	delete(s.cancels, transferID)
	s.updMu.Unlock()
}

// ---- 取消 / 续跑 / 查询 ----

// CancelUpdate 取消执行中的回写（块间生效；已写部分保留 → partial）。
// 幂等：终态记录原样返回。
func (s *SheetTransferService) CancelUpdate(ctx context.Context, transferID string) (*SheetTransferRecord, error) {
	rec, ust, err := s.loadUpdate(transferID)
	if err != nil {
		return nil, err
	}
	if rec.Direction != TransferUpdate {
		return nil, ErrTransferNotResumable
	}
	if rec.Status != TransferRunning {
		return rec, nil // 已终态（含先前的 partial/canceled）——幂等返回
	}
	s.updMu.Lock()
	ch, ok := s.cancels[transferID]
	if !ok {
		ch = make(chan struct{})
		s.cancels[transferID] = ch
	}
	s.updMu.Unlock()
	close(ch)
	// 等执行侧落终态（块间检查点；有界轮询）。
	deadline := time.Now().Add(updateCancelWaitMS * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if cur, err := s.store.GetSheetTransfer(ctx, transferID); err == nil && cur.Status != TransferRunning {
			return &cur, nil
		}
	}
	_ = ust
	return rec, nil // 仍在跑（长块）——前端继续轮询
}

// ResumeUpdate 续跑 partial/canceled/unknown：校验未写区域仍等于前态 +
// 本地版本未变，任何不符 → 拒绝（重新预览）。同 transfer_id 续写。
func (s *SheetTransferService) ResumeUpdate(ctx context.Context, transferID string) (*SheetTransferRecord, error) {
	rec, ust, err := s.loadUpdate(transferID)
	if err != nil {
		return nil, err
	}
	if rec.Direction != TransferUpdate {
		return nil, ErrTransferNotResumable
	}
	switch rec.Status {
	case TransferPartial, TransferCanceled, TransferUnknown:
	default:
		return nil, fmt.Errorf("%w: 当前状态 %s", ErrTransferNotResumable, rec.Status)
	}
	if !ust.HasPrestate || len(ust.Chunks) == 0 {
		return nil, fmt.Errorf("%w: 无前态或无剩余计划（请重新预览执行）", ErrTransferNotResumable)
	}
	// 本地源未变（变了 = 计划失效，须重新预览）。
	state, err := s.docs.GetDoc(ctx, ust.DocID)
	if err != nil {
		return nil, err
	}
	if state.Version != rec.SourceVersion {
		return nil, fmt.Errorf("%w: 本地版本已到 %d（计划基于 %d），请重新预览", ErrStalePreview, state.Version, rec.SourceVersion)
	}
	// 未写区域目标仍等于前态（第三方编辑 → 拒绝）。
	sp, err := s.updateProvider(ust.Provider, ust.ExternalURL)
	if err != nil {
		return nil, err
	}
	snap, _, err := sp.ReadSheetSnapshot(ctx, ust.ExternalURL, ust.SheetNames)
	if err != nil {
		return nil, err
	}
	current := map[string]map[string]CloudCell{}
	for _, cs := range snap.Sheets {
		m := map[string]CloudCell{}
		for _, cc := range cs.Cells {
			m[coordKey(cc.R, cc.C)] = cc
		}
		current[cs.Name] = m
	}
	for i := ust.Done; i < len(ust.Chunks); i++ {
		ch := ust.Chunks[i]
		pre := ust.Prestate[ch.Sheet]
		cur := current[ch.Sheet]
		if cur == nil {
			return nil, fmt.Errorf("%w: 目标缺少工作表 %s", ErrTargetChanged, ch.Sheet)
		}
		for _, lc := range ch.Cells {
			key := coordKey(lc.R, lc.C)
			before, hadValue := pre[key]
			nowCell, hasNow := cur[key]
			if hadValue != hasNow {
				return nil, fmt.Errorf("%w: %s!%s", ErrTargetChanged, ch.Sheet, key)
			}
			if hadValue && !cloudCellEqual(before, nowCell) {
				return nil, fmt.Errorf("%w: %s!%s", ErrTargetChanged, ch.Sheet, key)
			}
		}
	}
	rec.Status = TransferRunning
	rec.Error = ""
	rec.UpdatedAt = s.Clock().Format(time.RFC3339)
	if s.store != nil {
		if err := s.store.UpdateSheetTransfer(ctx, *rec); err != nil {
			return nil, err
		}
	}
	s.launchUpdate(transferID)
	return rec, nil
}

// cloudCellEqual 远端格原值相等（resume 前态校验用——不做本地等价转换）。
func cloudCellEqual(a, b CloudCell) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case CloudCellText, CloudCellDatetime:
		return a.Text == b.Text
	case CloudCellNumber:
		return a.Number != nil && b.Number != nil && numEqual(*a.Number, *b.Number)
	case CloudCellBool:
		return a.Bool != nil && b.Bool != nil && *a.Bool == *b.Bool
	case CloudCellFormula:
		return sheet.CanonicalFormula(stripSheetQuotes(a.Formula)) == sheet.CanonicalFormula(stripSheetQuotes(b.Formula))
	default:
		return false
	}
}

// Get 单条记录（前端轮询）。
func (s *SheetTransferService) Get(ctx context.Context, transferID string) (*SheetTransferRecord, error) {
	if s.store == nil {
		return nil, ErrTransferNotFound
	}
	rec, err := s.store.GetSheetTransfer(ctx, transferID)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// URL 归属辅助（HTTP 层复用）。
func updateURLHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
