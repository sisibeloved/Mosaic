// curation.go —— 策展动作面（0.7.1 / ADR-0038）：接受、否决/撤回、原位
// 修订、显式替代与时效口径。状态机：proposed --accept--> active；proposed
// --reject--> superseded；active --withdraw(reject)--> superseded；supersede
// 单事务双动作；superseded 终结不可复活（ErrTerminal）。
package memory

import (
	"context"
	"strings"
	"time"
)

// 编辑原因（EditRecord.Cause 枚举）。
const (
	CauseRevise    = "revise"
	CauseReject    = "reject"
	CauseWithdraw  = "withdraw"
	CauseSupersede = "supersede"
	CauseAccept    = "accept"
)

const maxNoteRunes = 500

func validateNote(note string) error {
	if r := len([]rune(note)); r > maxNoteRunes {
		return invalidErr("note 至多 500 字")
	}
	return nil
}

func appendEdit(history []EditRecord, rec EditRecord) []EditRecord {
	out := make([]EditRecord, 0, len(history)+1)
	out = append(out, history...)
	out = append(out, rec)
	return out
}

// Accept 策展接受（proposed → active）。已 active 幂等返回（不追加留痕——
// 无变化）；superseded 终结 409。
// 一审 #2：CAS 以**读快照的状态**为基准（accept 不递增版本——纯版本 CAS
// 分辨不出"接受前后的同名版本"）；并发已被接受时重读一次按幂等收敛。
func (s *Service) Accept(ctx context.Context, memoryID string) (Item, error) {
	it, err := s.Get(ctx, memoryID)
	if err != nil {
		return Item{}, err
	}
	switch it.Status {
	case StatusActive:
		return it, nil // 幂等（策展重复确认无害）
	case StatusSuperseded, StatusDeleted:
		// 0.7.8 审校 #1：墓碑同拦——策展接口不得消除墓碑（防复活依据）。
		return Item{}, ErrTerminal
	}
	now := s.now()
	next := it
	next.Status = StatusActive
	next.UpdatedAt = now
	next.EditHistory = appendEdit(it.EditHistory, EditRecord{
		Version: it.Version, Cause: CauseAccept, EditedBy: "owner", OccurredAt: now,
	})
	got, ok, err := s.store.UpdateItem(ctx, memoryID, it.Version, it.Status, next)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		// 并发交错重读：已被并发接受 → 幂等返回；被终结 → 终结语义。
		cur, rerr := s.Get(ctx, memoryID)
		if rerr == nil && cur.Status == StatusActive {
			return cur, nil
		}
		return Item{}, ErrVersionConflict
	}
	return got, nil
}

// Reject 候选否决（proposed → superseded，cause=reject）/ active 撤回
// （cause=withdraw）。版本递增 + 留痕；superseded 终结 409。
func (s *Service) Reject(ctx context.Context, memoryID, note string) (Item, error) {
	if err := validateNote(note); err != nil {
		return Item{}, err
	}
	it, err := s.Get(ctx, memoryID)
	if err != nil {
		return Item{}, err
	}
	if it.Status == StatusSuperseded || it.Status == StatusDeleted { // 0.7.8 审校 #1：墓碑同拦
		return Item{}, ErrTerminal
	}
	cause := CauseReject
	if it.Status == StatusActive {
		cause = CauseWithdraw
	}
	now := s.now()
	next := it
	next.Version = it.Version + 1
	next.Status = StatusSuperseded
	next.UpdatedAt = now
	next.EditHistory = appendEdit(it.EditHistory, EditRecord{
		Version: next.Version, Cause: cause, Note: note, EditedBy: "owner", OccurredAt: now,
	})
	// 一审 #1/#2：读快照即 CAS 基准（版本 + 状态）。
	got, ok, err := s.store.UpdateItem(ctx, memoryID, it.Version, it.Status, next)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		return Item{}, ErrVersionConflict
	}
	return got, nil
}

// ReviseCmd 原位修订（指针字段——缺席保持原值；valid_from/valid_to 传空串
// 指针 = 清除）。
type ReviseCmd struct {
	ExpectedVersion int
	Title           *string
	Content         *string
	SourceRefs      *[]string
	ObservedAt      *string
	ValidFrom       *string
	ValidTo         *string
	Sensitivity     *Sensitivity
	Confidence      *float64
	Note            string
}

// Revise 原位修订（ADR-0038 §2）：归属与状态不可改（另有动作面）；版本
// 递增 + edit_history 恰一条。
func (s *Service) Revise(ctx context.Context, memoryID string, cmd ReviseCmd) (Item, error) {
	if err := validateNote(cmd.Note); err != nil {
		return Item{}, err
	}
	it, err := s.Get(ctx, memoryID)
	if err != nil {
		return Item{}, err
	}
	if it.Status == StatusSuperseded || it.Status == StatusDeleted { // 0.7.8 审校 #1：墓碑同拦
		return Item{}, ErrTerminal
	}
	// 一审 #1：请求版本必须与读取快照一致（绑定快照提交）——快照已落后
	// 于请求预期即拒绝，不留"读到旧值却以新版本号过 CAS"的窗口。
	if it.Version != cmd.ExpectedVersion {
		return Item{}, ErrVersionConflict
	}
	next := it
	if cmd.Title != nil {
		next.Title = strings.TrimSpace(*cmd.Title)
	}
	if cmd.Content != nil {
		next.Content = *cmd.Content
	}
	if cmd.SourceRefs != nil {
		next.SourceRefs = append([]string{}, *cmd.SourceRefs...)
	}
	if cmd.ObservedAt != nil {
		next.ObservedAt = normTime(*cmd.ObservedAt) // 一审 #4：UTC 规范化
	}
	if cmd.ValidFrom != nil {
		next.ValidFrom = normTime(*cmd.ValidFrom) // 空串 = 清除
	}
	if cmd.ValidTo != nil {
		next.ValidTo = normTime(*cmd.ValidTo)
	}
	if cmd.Sensitivity != nil {
		next.Sensitivity = *cmd.Sensitivity
	}
	var confidence *float64
	if cmd.Confidence != nil {
		v := *cmd.Confidence
		confidence = &v
		next.Confidence = v
	} else {
		c := it.Confidence
		confidence = &c
	}
	if err := validateFields(next.Title, next.Content, next.SourceRefs,
		next.ObservedAt, next.ValidFrom, next.ValidTo, confidence); err != nil {
		return Item{}, err
	}
	if next.Sensitivity != SensNormal && next.Sensitivity != SensSensitive {
		return Item{}, invalidErr("sensitivity 未知：" + string(next.Sensitivity))
	}
	now := s.now()
	next.Version = it.Version + 1
	next.UpdatedAt = now
	next.EditHistory = appendEdit(it.EditHistory, EditRecord{
		Version: next.Version, Cause: CauseRevise, Note: cmd.Note, EditedBy: "owner", OccurredAt: now,
	})
	// 一审 #2：状态基准 CAS（纯版本 CAS 分辨不出状态已并发的同名版本）。
	got, ok, err := s.store.UpdateItem(ctx, memoryID, cmd.ExpectedVersion, it.Status, next)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		return Item{}, ErrVersionConflict
	}
	return got, nil
}

// Supersede 显式替代（单事务双动作）：旧 → superseded（version+1，cause=
// supersede，留痕）+ 新条目创建（supersedes=[旧 id]）。replacement 归属可
// 与旧条目不同（跨域迁移纠错是合法替代）。
func (s *Service) Supersede(ctx context.Context, oldID string, expectedVersion int, note string, replacement CreateCmd) (Item, Item, error) {
	if err := validateNote(note); err != nil {
		return Item{}, Item{}, err
	}
	if err := replacement.validate(); err != nil {
		return Item{}, Item{}, err
	}
	if err := s.checkAnchor(ctx, replacement.Namespace, strings.TrimSpace(replacement.ScopeID)); err != nil {
		return Item{}, Item{}, err
	}
	old, err := s.Get(ctx, oldID)
	if err != nil {
		return Item{}, Item{}, err
	}
	if old.Status == StatusSuperseded || old.Status == StatusDeleted { // 0.7.8 审校 #1：墓碑同拦
		return Item{}, Item{}, ErrTerminal
	}
	// 一审 #1：请求版本与读取快照一致才提交（绑定快照）。
	if old.Version != expectedVersion {
		return Item{}, Item{}, ErrVersionConflict
	}
	now := s.now()
	oldNext := old
	oldNext.Version = old.Version + 1
	oldNext.Status = StatusSuperseded
	oldNext.UpdatedAt = now
	oldNext.EditHistory = appendEdit(old.EditHistory, EditRecord{
		Version: oldNext.Version, Cause: CauseSupersede, Note: note, EditedBy: "owner", OccurredAt: now,
	})
	confidence := 1.0
	if replacement.Confidence != nil {
		confidence = *replacement.Confidence
	}
	sensitivity := replacement.Sensitivity
	if sensitivity == "" {
		sensitivity = SensNormal
	}
	status := replacement.Status
	if status == "" {
		status = StatusActive
	}
	newItem := Item{
		MemoryID:    "mem_" + newToken(),
		Version:     1,
		Namespace:   replacement.Namespace,
		ScopeID:     strings.TrimSpace(replacement.ScopeID),
		Kind:        replacement.Kind,
		Title:       strings.TrimSpace(replacement.Title),
		Content:     replacement.Content,
		SourceRefs:  append([]string{}, replacement.SourceRefs...),
		ObservedAt:  normTime(replacement.ObservedAt), // 一审 #4：UTC 规范化
		ValidFrom:   normTime(replacement.ValidFrom),
		ValidTo:     normTime(replacement.ValidTo),
		Confidence:  confidence,
		Status:      status,
		Visibility:  "namespace",
		Sensitivity: sensitivity,
		Supersedes:  []string{oldID},
		EditHistory: []EditRecord{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// 一审 #2：状态基准 CAS（同 Revise——接受/撤回与替代并发交错全部拦）。
	oldGot, newGot, ok, err := s.store.SupersedeItem(ctx, oldID, expectedVersion, old.Status, oldNext, newItem)
	if err != nil {
		return Item{}, Item{}, err
	}
	if !ok {
		return Item{}, Item{}, ErrVersionConflict
	}
	return oldGot, newGot, nil
}

// expired valid_to < now（RFC3339 **时刻**比较——0.6.13 审校 #15 同教训：
// 字典序在可变小数位下会漏判）。
func expired(validTo, now string) bool {
	if validTo == "" {
		return false
	}
	to, err1 := time.Parse(time.RFC3339, validTo)
	n, err2 := time.Parse(time.RFC3339, now)
	if err1 != nil || err2 != nil {
		return false // 解析失败不参与过滤（写入面已校验格式）
	}
	return to.Before(n)
}
