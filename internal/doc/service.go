// Package doc 的命令处理服务（RFC-0014 / ADR-0014）。
// 纪律平移 internal/room/service.go：幂等回放最先、版本预检、严格解码
// （DisallowUnknownFields）、事件 + 回执同事务原子落库（乐观并发在事务内再强制）。
// agent 写面不经本命令门（ExecuteCommand 只受理 human/system）——引擎代写走
// 导出的 CommitRevision（显式 actor/source，memory_ops 引擎代写先例平移，Phase 4 接线）。
package doc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

var (
	uuidv7Pattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	eventIDPattern  = regexp.MustCompile(`^evt_[0-9A-Za-z_-]+$`)
	docIDPattern    = regexp.MustCompile(`^doc_[0-9a-f]{12}$`)
	revisionSources = map[string]bool{"human_editor": true, "agent": true}
)

// Actor 命令行动者（命令通道：human=本地 owner / system；agent 经 CommitRevision 代写）。
type Actor struct {
	ParticipantID string
	Kind          string // human | agent | system
}

// Command 外部命令（tenant 由服务持有，actor 由调用方绑定；DocID 路径绑定——
// create_doc 不接受 DocID，其余命令必填）。
type Command struct {
	DocID              string
	CommandKind        string
	ExpectedDocVersion int64
	IdempotencyKey     string
	IssuedAt           string
	Payload            json.RawMessage
}

// CommandResult 命令执行结果（Replay 场景返回原事件标识）。
type CommandResult struct {
	DocID      string
	EventID    string
	DocVersion int64
	Replayed   bool
}

// VersionConflictError 版本冲突（结构化：transport 层据此渲染 409 与当前态，
// RFC-0014 §2.3——客户端在新态上重放本地未提交 ops）。errors.Is 命中 ErrVersionConflict。
type VersionConflictError struct {
	DocID    string
	Expected int64
	Current  int64
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("doc: version conflict: expected=%d current=%d", e.Expected, e.Current)
}

func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }

// Config 服务依赖注入。
type Config struct {
	Store  DocStore
	Reader DocEventReader // 可选：缺省回退 Store（若其实现 DocEventReader）
	// Searcher 可选：全文搜索读路径（nil 时 SearchDocs 报错）。
	Searcher DocSearcher
	Clock    func() string              // RFC3339
	NewID    func(prefix string) string // 事件 ID 生成（前缀 evt_）
	// NewDocID 文档 ID 生成（doc_<12hex>，Schema 锁定形态，服务端分配）。
	NewDocID func() string
	// NewBlockID 块 ID 生成（插入块的空 block_id 由服务端分配；可选——
	// 缺省 blk_<8hex> 随机）。注入方可保证确定性（UT）。
	NewBlockID func() string
	Tenant     string
}

// Service 命令处理服务：校验 → 幂等 → 并发检查 → 事件生产（原子落库）。
type Service struct {
	cfg Config
}

// NewService 构造服务；Config 的依赖为必填。
func NewService(cfg Config) *Service {
	if cfg.Store == nil || cfg.Clock == nil || cfg.NewID == nil || cfg.NewDocID == nil {
		panic("doc: Config.Store/Clock/NewID/NewDocID 必填")
	}
	if cfg.NewBlockID == nil {
		cfg.NewBlockID = func() string {
			var b [4]byte
			_, _ = rand.Read(b[:])
			return "blk_" + hex.EncodeToString(b[:])
		}
	}
	return &Service{cfg: cfg}
}

// ExecuteCommand 执行外部命令。幂等回放优先于一切检查（同 RFC-0001 纪律）。
func (s *Service) ExecuteCommand(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if !uuidv7Pattern.MatchString(cmd.IdempotencyKey) {
		return nil, fmt.Errorf("%w: idempotency_key 必须为 UUIDv7", ErrInvalidCommand)
	}
	// issued_at 运行时校验（Schema 只管 fixture 门禁，不管运行时）。
	if _, err := time.Parse(time.RFC3339, cmd.IssuedAt); err != nil {
		return nil, fmt.Errorf("%w: issued_at 必须为 RFC3339 时间戳", ErrInvalidCommand)
	}
	if actor.ParticipantID == "" || (actor.Kind != "human" && actor.Kind != "system") {
		return nil, fmt.Errorf("%w: 外部命令 actor 必须为具名 human/system", ErrInvalidCommand)
	}
	switch cmd.CommandKind {
	case "create_doc":
		return s.createDoc(ctx, actor, cmd)
	case "rename_doc":
		return s.renameDoc(ctx, actor, cmd)
	case "commit_doc_revision":
		return s.commitDocRevision(ctx, actor, cmd)
	case "archive_doc":
		return s.archiveDoc(ctx, actor, cmd)
	case "restore_doc":
		return s.restoreDoc(ctx, actor, cmd)
	case "delete_doc":
		return s.deleteDoc(ctx, actor, cmd)
	case "duplicate_doc":
		return s.duplicateDoc(ctx, actor, cmd)
	default:
		return nil, fmt.Errorf("%w: 未知命令 %q", ErrInvalidCommand, cmd.CommandKind)
	}
}

// createDoc 创建文档（doc_<12hex> 服务端分配）。initial_blocks 可选——
// 携带时同事务追加首条 revision（created + revision 两事件一批，CAS 期望 0）。
func (s *Service) createDoc(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	if cmd.DocID != "" {
		return nil, fmt.Errorf("%w: create_doc 不接受 doc_id", ErrInvalidCommand)
	}
	var payload struct {
		Title           string              `json:"title"`
		Format          string              `json:"format"`
		InitialBlocks   []protocol.DocBlock `json:"initial_blocks"`
		AnchorMessageID *string             `json:"anchor_message_id"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: create_doc payload: %v", ErrInvalidCommand, err)
	}
	if n := len([]rune(payload.Title)); n < 1 || n > MaxTitleRunes {
		return nil, fmt.Errorf("%w: title 必填 1..%d 字", ErrInvalidCommand, MaxTitleRunes)
	}
	format := payload.Format
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" {
		return nil, fmt.Errorf("%w: format 首版封闭枚举仅 markdown", ErrInvalidCommand)
	}
	if payload.AnchorMessageID != nil && !eventIDPattern.MatchString(*payload.AnchorMessageID) {
		return nil, fmt.Errorf("%w: anchor_message_id 形如 evt_*", ErrInvalidCommand)
	}
	if len(payload.InitialBlocks) > MaxOpsPerBatch {
		return nil, fmt.Errorf("%w: initial_blocks ≤ %d", ErrInvalidCommand, MaxOpsPerBatch)
	}

	docID := s.cfg.NewDocID()
	if !docIDPattern.MatchString(docID) {
		return nil, fmt.Errorf("doc: NewDocID 产物 %q 不符合 doc_<12hex>", docID)
	}
	now := s.cfg.Clock()
	mkEnv := func(typ string, payloadJSON []byte) protocol.DocEnvelope {
		return protocol.DocEnvelope{
			EventID: s.cfg.NewID("evt"), TenantID: s.cfg.Tenant, DocID: docID,
			Type: typ, SchemaVersion: 1, OccurredAt: now,
			Actor:    protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
			Payload:  payloadJSON,
			Metadata: map[string]any{},
		}
	}
	envs := []protocol.DocEnvelope{mkEnv(protocol.EventDocCreated, mustJSON(protocol.DocCreatedPayload{
		DocID: docID, Title: payload.Title, Format: format,
		CreatedBy: actor.ParticipantID, AnchorMessageID: payload.AnchorMessageID,
	}))}

	if len(payload.InitialBlocks) > 0 {
		ops := make([]protocol.DocOp, 0, len(payload.InitialBlocks))
		for _, b := range payload.InitialBlocks {
			block := b
			ops = append(ops, protocol.DocOp{Op: "append", Block: &block})
		}
		normalized, err := s.normalizeOps(nil, ops)
		if err != nil {
			return nil, err
		}
		if _, statuses := ApplyDocOps(nil, normalized, nil); firstRejected(statuses) != "" {
			return nil, fmt.Errorf("%w: initial_blocks 校验失败: %s", ErrInvalidCommand, firstRejected(statuses))
		}
		envs = append(envs, mkEnv(protocol.EventDocRevisionCommitted, mustJSON(protocol.DocRevisionCommittedPayload{
			DocID: docID, BaseVersion: 1, Version: 2, Ops: normalized,
			Actor: actor.ParticipantID, Source: "human_editor",
		})))
	}

	receipt := CommandReceipt{
		TenantID: s.cfg.Tenant, DocID: docID,
		IdempotencyKey: cmd.IdempotencyKey, CommandKind: cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor), EventID: envs[0].EventID,
		ExpectedDocVersion: cmd.ExpectedDocVersion, ExecutedAt: now,
	}
	return s.commit(ctx, envs, receipt)
}

// renameDoc 重命名（归档只读：拒绝；RFC-0014 §2.8）。
func (s *Service) renameDoc(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	state, err := s.requireState(ctx, cmd.DocID)
	if err != nil {
		return nil, err
	}
	if err := requireActive(state); err != nil {
		return nil, err
	}
	if res, err := s.checkVersion(ctx, cmd, actor, state); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		Title string `json:"title"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: rename_doc payload: %v", ErrInvalidCommand, err)
	}
	if n := len([]rune(payload.Title)); n < 1 || n > MaxTitleRunes {
		return nil, fmt.Errorf("%w: title 必填 1..%d 字", ErrInvalidCommand, MaxTitleRunes)
	}
	env := s.newEnvelope(actor, cmd.DocID, protocol.EventDocRenamed, mustJSON(protocol.DocRenamedPayload{
		DocID: cmd.DocID, Title: payload.Title,
	}))
	return s.commitWith(ctx, cmd, actor, env)
}

// commitDocRevision 人类编辑器路径的修订提交（source 恒 human_editor）；
// base_version = cmd.ExpectedDocVersion（payload 内 base_version 若携带必须一致——
// 不收两份可能分歧的真值）。
func (s *Service) commitDocRevision(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	state, err := s.requireState(ctx, cmd.DocID)
	if err != nil {
		return nil, err
	}
	if err := requireActive(state); err != nil {
		return nil, err
	}
	if res, err := s.checkVersion(ctx, cmd, actor, state); res != nil || err != nil {
		return res, err
	}
	if len(cmd.Payload) > MaxBatchBytes {
		return nil, fmt.Errorf("%w: 批体超 %d KiB", ErrInvalidCommand, MaxBatchBytes>>10)
	}
	var payload struct {
		BaseVersion *int64           `json:"base_version"`
		Ops         []protocol.DocOp `json:"ops"`
		Note        string           `json:"note"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: commit_doc_revision payload: %v", ErrInvalidCommand, err)
	}
	if payload.BaseVersion != nil && *payload.BaseVersion != cmd.ExpectedDocVersion {
		return nil, fmt.Errorf("%w: payload.base_version 与命令期望版本不一致", ErrInvalidCommand)
	}
	if len([]rune(payload.Note)) > 280 {
		return nil, fmt.Errorf("%w: note 超 280 字", ErrInvalidCommand)
	}
	ops, err := s.validateOps(state, payload.Ops)
	if err != nil {
		return nil, err
	}
	env := s.newEnvelope(actor, cmd.DocID, protocol.EventDocRevisionCommitted,
		mustJSON(protocol.DocRevisionCommittedPayload{
			DocID: cmd.DocID, BaseVersion: state.Version, Version: state.Version + 1,
			Ops: ops, Actor: actor.ParticipantID, Source: "human_editor", Note: payload.Note,
		}))
	return s.commitWith(ctx, cmd, actor, env)
}

// CommitRevision 引擎代写路径（RFC-0014 §2.7：agent 经 doc_ops 引擎校验代写，
// actor 记为该 bot）与命令路径共用的提交面——显式 actor（human/agent 均可）与
// source，不走幂等回执（引擎波内串行；并发安全由 CAS 追加在事务内强制）。
// ops 逐条校验：任一 rejected 整批拒绝（事件只承载已应用结果）。
func (s *Service) CommitRevision(ctx context.Context, actor Actor, docID string, baseVersion int64, ops []protocol.DocOp, source, note string) (*CommandResult, error) {
	if actor.ParticipantID == "" {
		return nil, fmt.Errorf("%w: actor 必须具名", ErrInvalidCommand)
	}
	if !revisionSources[source] {
		return nil, fmt.Errorf("%w: source 取值 human_editor | agent", ErrInvalidCommand)
	}
	if len([]rune(note)) > 280 {
		return nil, fmt.Errorf("%w: note 超 280 字", ErrInvalidCommand)
	}
	state, err := s.requireState(ctx, docID)
	if err != nil {
		return nil, err
	}
	if err := requireActive(state); err != nil {
		return nil, err
	}
	if state.Version != baseVersion {
		return nil, &VersionConflictError{DocID: docID, Expected: baseVersion, Current: state.Version}
	}
	normalized, err := s.validateOps(state, ops)
	if err != nil {
		return nil, err
	}
	env := s.newEnvelope(actor, docID, protocol.EventDocRevisionCommitted,
		mustJSON(protocol.DocRevisionCommittedPayload{
			DocID: docID, BaseVersion: baseVersion, Version: baseVersion + 1,
			Ops: normalized, Actor: actor.ParticipantID, Source: source, Note: note,
		}))
	cas, ok := s.cfg.Store.(DocCASStore)
	if !ok {
		return nil, fmt.Errorf("doc: 存储不支持 CAS 追加")
	}
	appended, err := cas.AppendDocEventsIf(ctx, []protocol.DocEnvelope{env}, baseVersion)
	if err != nil {
		return nil, fmt.Errorf("doc: append: %w", err)
	}
	return &CommandResult{DocID: docID, EventID: appended[0].EventID, DocVersion: appended[0].Version}, nil
}

// archiveDoc 归档（active → archived；归档为只读态）。
func (s *Service) archiveDoc(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	return s.lifecycleDoc(ctx, actor, cmd, protocol.EventDocArchived)
}

// restoreDoc 恢复（archived → active）。
func (s *Service) restoreDoc(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	return s.lifecycleDoc(ctx, actor, cmd, protocol.EventDocRestored)
}

func (s *Service) lifecycleDoc(ctx context.Context, actor Actor, cmd Command, eventType string) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	state, err := s.requireState(ctx, cmd.DocID)
	if err != nil {
		return nil, err
	}
	switch eventType {
	case protocol.EventDocArchived:
		if err := requireActive(state); err != nil {
			return nil, err
		}
	case protocol.EventDocRestored:
		if state.Status != StatusArchived {
			return nil, fmt.Errorf("%w: 文档未归档", ErrInvalidCommand)
		}
	}
	if res, err := s.checkVersion(ctx, cmd, actor, state); res != nil || err != nil {
		return res, err
	}
	var payload struct{}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: %s payload 应为空对象: %v", ErrInvalidCommand, cmd.CommandKind, err)
	}
	env := s.newEnvelope(actor, cmd.DocID, eventType, mustJSON(map[string]any{"doc_id": cmd.DocID}))
	return s.commitWith(ctx, cmd, actor, env)
}

// deleteDoc 删除文档（deleteRoom 先例平移）：先落墓碑事件（doc.deleted——
// 审计可回溯"曾存在"），再级联清理（事件/outbox/回执/FTS）——墓碑行独立留痕。
// 任意生命周期态可删；删除后文档不可再访问（内容不可恢复）。
func (s *Service) deleteDoc(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	state, err := s.requireState(ctx, cmd.DocID)
	if err != nil {
		return nil, err
	}
	if res, err := s.checkVersion(ctx, cmd, actor, state); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: delete_doc payload: %v", ErrInvalidCommand, err)
	}
	if n := len([]rune(payload.Reason)); n < 1 || n > 280 {
		return nil, fmt.Errorf("%w: reason 必填 1..280 字（删除不可逆，理由留痕）", ErrInvalidCommand)
	}
	env := s.newEnvelope(actor, cmd.DocID, protocol.EventDocDeleted, mustJSON(protocol.DocDeletedPayload{
		DocID: cmd.DocID, Reason: payload.Reason,
	}))
	res, err := s.commitWith(ctx, cmd, actor, env)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Store.DeleteDoc(ctx, cmd.DocID); err != nil {
		return nil, fmt.Errorf("doc: delete cascade: %w", err)
	}
	return res, nil
}

// duplicateDoc 创建副本（RFC-0014 §2.8）：新 doc_id，标题缺省为原标题 +
// "（副本）"，正文以一条 revision 整块复制（append ops，block_id 保留——
// 块 ID 作用域为单文档，外部锚点引用 doc_id+block_id 不受影响）。
// cmd.ExpectedDocVersion 为源文档版本前置断言（"复制 V 时刻的态"）；
// 新文档自身 CAS 期望恒为 0。归档文档可复制（只读约束只拦写）。
func (s *Service) duplicateDoc(ctx context.Context, actor Actor, cmd Command) (*CommandResult, error) {
	if res, err := s.replayIfReceived(ctx, cmd, actor); res != nil || err != nil {
		return res, err
	}
	src, err := s.requireState(ctx, cmd.DocID)
	if err != nil {
		return nil, err
	}
	if res, err := s.checkVersion(ctx, cmd, actor, src); res != nil || err != nil {
		return res, err
	}
	var payload struct {
		Title string `json:"title"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cmd.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: duplicate_doc payload: %v", ErrInvalidCommand, err)
	}
	title := payload.Title
	if title == "" {
		title = src.Title + "（副本）"
	}
	if n := len([]rune(title)); n < 1 || n > MaxTitleRunes {
		return nil, fmt.Errorf("%w: title 超 %d 字（缺省拼接后缀后超限——请显式提供 title）", ErrInvalidCommand, MaxTitleRunes)
	}

	docID := s.cfg.NewDocID()
	if !docIDPattern.MatchString(docID) {
		return nil, fmt.Errorf("doc: NewDocID 产物 %q 不符合 doc_<12hex>", docID)
	}
	now := s.cfg.Clock()
	mkEnv := func(typ string, payloadJSON []byte) protocol.DocEnvelope {
		return protocol.DocEnvelope{
			EventID: s.cfg.NewID("evt"), TenantID: s.cfg.Tenant, DocID: docID,
			Type: typ, SchemaVersion: 1, OccurredAt: now,
			Actor:    protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
			Payload:  payloadJSON,
			Metadata: map[string]any{},
		}
	}
	envs := []protocol.DocEnvelope{mkEnv(protocol.EventDocCreated, mustJSON(protocol.DocCreatedPayload{
		DocID: docID, Title: title, Format: src.Format, CreatedBy: actor.ParticipantID,
	}))}
	if len(src.Blocks) > 0 {
		ops := make([]protocol.DocOp, 0, len(src.Blocks))
		for _, b := range src.Blocks {
			block := b
			ops = append(ops, protocol.DocOp{Op: "append", Block: &block})
		}
		envs = append(envs, mkEnv(protocol.EventDocRevisionCommitted, mustJSON(protocol.DocRevisionCommittedPayload{
			DocID: docID, BaseVersion: 1, Version: 2, Ops: ops,
			Actor: actor.ParticipantID, Source: "human_editor", Note: "副本自 " + cmd.DocID,
		})))
	}
	receipt := CommandReceipt{
		TenantID: s.cfg.Tenant, DocID: docID,
		IdempotencyKey: cmd.IdempotencyKey, CommandKind: cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor), EventID: envs[0].EventID,
		ExpectedDocVersion: 0, ExecutedAt: now,
	}
	return s.commit(ctx, envs, receipt)
}

// ---- 查询面（读路径，不产生事件）----

// GetDoc 当前态（折叠自事件流；不存在/已删除 → ErrDocNotFound）。
func (s *Service) GetDoc(ctx context.Context, docID string) (DocState, error) {
	return s.requireState(ctx, docID)
}

// ExportDoc 导出 markdown（RFC-0014 §2.8；归档可导出，删除后不可得）。
func (s *Service) ExportDoc(ctx context.Context, docID string) (string, DocState, error) {
	state, err := s.requireState(ctx, docID)
	if err != nil {
		return "", DocState{}, err
	}
	return RenderMarkdown(state), state, nil
}

// SearchDocs 全文搜索（委托 Searcher 端口；nil 装配如实报错）。
func (s *Service) SearchDocs(ctx context.Context, query string, limit int) ([]DocSearchHit, error) {
	if s.cfg.Searcher == nil {
		return nil, fmt.Errorf("doc: searcher 不可用")
	}
	return s.cfg.Searcher.SearchDocs(ctx, query, limit)
}

// ---- 内部 ----

// newEnvelope 构造文档事件信封（version 由存储在追加时分配）。
func (s *Service) newEnvelope(actor Actor, docID, eventType string, payloadJSON []byte) protocol.DocEnvelope {
	return protocol.DocEnvelope{
		EventID: s.cfg.NewID("evt"), TenantID: s.cfg.Tenant, DocID: docID,
		Type: eventType, SchemaVersion: 1, OccurredAt: s.cfg.Clock(),
		Actor:    protocol.Actor{ParticipantID: actor.ParticipantID, Kind: actor.Kind},
		Payload:  payloadJSON,
		Metadata: map[string]any{},
	}
}

// requireState 折叠当前态并要求存在（Version==0 = 未见 doc.created 或已级联清除）。
func (s *Service) requireState(ctx context.Context, docID string) (DocState, error) {
	if !docIDPattern.MatchString(docID) {
		return DocState{}, fmt.Errorf("%w: doc_id 形如 doc_<12hex>", ErrInvalidCommand)
	}
	events, err := s.historyOf(ctx, docID)
	if err != nil {
		return DocState{}, err
	}
	state, err := ProjectDoc(events)
	if err != nil {
		return DocState{}, fmt.Errorf("doc: project: %w", err)
	}
	if state.Version == 0 {
		return DocState{}, fmt.Errorf("%w: %s", ErrDocNotFound, docID)
	}
	return state, nil
}

// requireActive 归档只读门（RFC-0014 §2.8：rename/revision/archive 拒绝）。
func requireActive(state DocState) error {
	if state.Status == StatusArchived {
		return fmt.Errorf("%w: 文档已归档（只读）", ErrDocArchived)
	}
	return nil
}

// checkVersion 版本预检（命令面乐观并发）：不符返回结构化冲突（transport 渲染 409）；
// 预检与提交之间的竞态重放优先于版本冲突（复审 #22 先例平移——重查命中即回放）。
func (s *Service) checkVersion(ctx context.Context, cmd Command, actor Actor, state DocState) (*CommandResult, error) {
	if cmd.ExpectedDocVersion == state.Version {
		return nil, nil
	}
	if res, rerr := s.replayIfReceived(ctx, cmd, actor); res != nil || rerr != nil {
		return res, rerr
	}
	return nil, &VersionConflictError{DocID: cmd.DocID, Expected: cmd.ExpectedDocVersion, Current: state.Version}
}

// validateOps ops 校验与规范化：结构上限 → 空 block_id 服务端分配 → 严格应用
// （任一 rejected 整批拒绝——事件只承载已应用结果；agent 侧的逐 op 容错由引擎
// 在调用前自行过滤，ApplyDocOps 导出可用）。
func (s *Service) validateOps(state DocState, ops []protocol.DocOp) ([]protocol.DocOp, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: ops 至少 1 条", ErrInvalidCommand)
	}
	if len(ops) > MaxOpsPerBatch {
		return nil, fmt.Errorf("%w: ops ≤ %d 条/批", ErrInvalidCommand, MaxOpsPerBatch)
	}
	normalized, err := s.normalizeOps(state.Blocks, ops)
	if err != nil {
		return nil, err
	}
	if _, statuses := ApplyDocOps(state.Blocks, normalized, nil); firstRejected(statuses) != "" {
		return nil, fmt.Errorf("%w: ops 校验失败: %s", ErrInvalidCommand, firstRejected(statuses))
	}
	return normalized, nil
}

// normalizeOps 为插入块分配缺省 block_id（与既有块及本批已分配 ID 均不撞车）。
func (s *Service) normalizeOps(blocks []protocol.DocBlock, ops []protocol.DocOp) ([]protocol.DocOp, error) {
	taken := map[string]bool{}
	for _, b := range blocks {
		taken[b.BlockID] = true
	}
	out := make([]protocol.DocOp, len(ops))
	for i, op := range ops {
		out[i] = op
		if (op.Op == "insert_after" || op.Op == "append") && op.Block != nil && op.Block.BlockID == "" {
			for range 16 {
				id := s.cfg.NewBlockID()
				if !taken[id] {
					block := *op.Block
					block.BlockID = id
					out[i].Block = &block
					taken[id] = true
					break
				}
			}
			if out[i].Block.BlockID == "" {
				return nil, fmt.Errorf("doc: block_id 分配失败（生成器持续撞车）")
			}
		} else if op.Block != nil {
			taken[op.Block.BlockID] = true
		}
	}
	return out, nil
}

// firstRejected 汇总首个被拒绝的 op（错误面只报首个——逐条明细可经 ApplyDocOps 复算）。
func firstRejected(statuses []OpStatus) string {
	for _, st := range statuses {
		if st.Status == "rejected" {
			return fmt.Sprintf("ops[%d] %s: %s", st.Index, st.Op, st.Reason)
		}
	}
	return ""
}

// historyOf 全量事件（折叠输入；游标翻页至追平）。
func (s *Service) historyOf(ctx context.Context, docID string) ([]StoredDocEvent, error) {
	reader := s.cfg.Reader
	if reader == nil {
		if r, ok := s.cfg.Store.(DocEventReader); ok {
			reader = r
		}
	}
	if reader == nil {
		return nil, fmt.Errorf("doc: reader 不可用")
	}
	var all []StoredDocEvent
	cursor := ""
	for {
		events, next, err := reader.DocEventsAfter(ctx, docID, cursor, 1000)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
		if next == "" || len(events) == 0 {
			return all, nil
		}
		cursor = next
	}
}

// commit 事件 + 回执同事务落库；回执撞车按竞态回放处理（room commit 先例）。
func (s *Service) commit(ctx context.Context, envs []protocol.DocEnvelope, receipt CommandReceipt) (*CommandResult, error) {
	appended, err := s.cfg.Store.AppendDocEventsWithReceipt(ctx, envs, receipt)
	if err != nil {
		if errors.Is(err, ErrDuplicateReceipt) { // 并发同键竞态：后到者按回放处理
			// 指纹比对不可跳过：同键异载荷是幂等冲突，静默回放等于吞掉冲突
			if res, rerr := s.replayByReceipt(ctx, receipt); res != nil || rerr != nil {
				return res, rerr
			}
			// 回放未果（非回执撞车而是其他唯一约束）：按原错误上抛，绝不 (nil,nil)
			return nil, fmt.Errorf("doc: append: %w", err)
		}
		return nil, fmt.Errorf("doc: append: %w", err)
	}
	return &CommandResult{
		DocID:      appended[0].DocID,
		EventID:    appended[0].EventID,
		DocVersion: appended[len(appended)-1].Version,
	}, nil
}

// commitWith 以命令要素构造回执并提交（事件携带的 DocID 即回执 DocID）。
func (s *Service) commitWith(ctx context.Context, cmd Command, actor Actor, env protocol.DocEnvelope) (*CommandResult, error) {
	receipt := CommandReceipt{
		TenantID: s.cfg.Tenant, DocID: env.DocID,
		IdempotencyKey: cmd.IdempotencyKey, CommandKind: cmd.CommandKind,
		RequestFingerprint: fingerprint(cmd, actor), EventID: env.EventID,
		ExpectedDocVersion: cmd.ExpectedDocVersion, ExecutedAt: s.cfg.Clock(),
	}
	return s.commit(ctx, []protocol.DocEnvelope{env}, receipt)
}

// replayIfReceived 已受理则回放；同键不同指纹报冲突；未受理返回 (nil, nil)。
func (s *Service) replayIfReceived(ctx context.Context, cmd Command, actor Actor) (*CommandResult, error) {
	return s.replayCore(ctx, cmd.IdempotencyKey, cmd.CommandKind, fingerprint(cmd, actor))
}

// replayByReceipt 竞态路径回放：以本方回执携带的指纹比对（commit 内唯一调用点）。
func (s *Service) replayByReceipt(ctx context.Context, receipt CommandReceipt) (*CommandResult, error) {
	return s.replayCore(ctx, receipt.IdempotencyKey, receipt.CommandKind, receipt.RequestFingerprint)
}

func (s *Service) replayCore(ctx context.Context, idemKey, kind, wantFingerprint string) (*CommandResult, error) {
	rc, err := s.cfg.Store.LookupDocReceipt(ctx, s.cfg.Tenant, idemKey, kind)
	if err != nil {
		return nil, fmt.Errorf("doc: lookup receipt: %w", err)
	}
	if rc == nil {
		return nil, nil
	}
	if rc.RequestFingerprint != wantFingerprint {
		return nil, fmt.Errorf("%w: 同幂等键不同请求指纹", ErrIdempotencyConflict)
	}
	return &CommandResult{
		DocID:      rc.DocID,
		EventID:    rc.EventID,
		DocVersion: rc.DocVersion,
		Replayed:   true,
	}, nil
}

// fingerprint 请求指纹：命令身份要素的规范化哈希（版本不入指纹——回放与文档演进解耦）。
func fingerprint(cmd Command, actor Actor) string {
	h := sha256.New()
	for _, part := range []string{cmd.CommandKind, cmd.DocID, actor.ParticipantID, actor.Kind, string(cmd.Payload)} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("doc: marshal: " + err.Error())
	}
	return raw
}
