// Package doc 是文档聚合域：命令校验、幂等 receipt、乐观并发、投影折叠
// （RFC-0014 / ADR-0014：文档为 workspace 级全局资产，per-doc 事件流与房间日志
// 并列为权威事实源）。存储以端口注入（架构 §8.4 依赖方向）：UT 用内存 fake，
// IT/ST 用 SQLite 实现。
package doc

import (
	"context"
	"errors"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// 域错误：调用方以 errors.Is 判别，不依赖错误文本。
var (
	// ErrInvalidCommand 命令校验失败（payload 越界、未知命令、非法幂等键、
	// ops 校验失败、生命周期状态不允许等）。
	ErrInvalidCommand = errors.New("doc: invalid command")
	// ErrVersionConflict base_version 与当前文档版本不符（乐观并发拒绝：
	// 客户端在新态上重放本地未提交 ops，RFC-0014 §2.3）。
	ErrVersionConflict = errors.New("doc: version conflict")
	// ErrIdempotencyConflict 同幂等键但请求指纹不同（禁止静默改写语义）。
	ErrIdempotencyConflict = errors.New("doc: idempotency conflict")
	// ErrDocNotFound 目标文档不存在（未见 doc.created，或已删除级联清除）。
	ErrDocNotFound = errors.New("doc: doc not found")
	// ErrDocArchived 文档已归档：归档为只读态（RFC-0014 §2.8），
	// rename/revision/archive 拒绝；duplicate/export 允许。
	ErrDocArchived = errors.New("doc: doc archived")
	// ErrDuplicateEvent 事件 ID 冲突（存储层哨兵，透传给上游判定）。
	ErrDuplicateEvent = errors.New("doc: duplicate event id")
	// ErrDuplicateReceipt 幂等键冲突（存储层哨兵：并发竞态时后到者收到）。
	ErrDuplicateReceipt = errors.New("doc: duplicate receipt")
)

// CommandReceipt 幂等回执（同 room.CommandReceipt 纪律：tenant+key+kind 唯一）。
// doc 面独立表——房间回执表 room_id NOT NULL 且假设房间信封，不可复用（ADR-0014）。
type CommandReceipt struct {
	TenantID           string
	DocID              string
	IdempotencyKey     string
	CommandKind        string
	RequestFingerprint string
	EventID            string
	// ExpectedDocVersion 乐观并发期望：存储必须在追加事务内校验
	// 当前文档版本与之相等（同 P-03：冲突判定在提交事务内）。
	ExpectedDocVersion int64
	// DocVersion 由存储在追加成功后权威回填（调用方传入值被忽略）。
	DocVersion int64
	ExecutedAt string
}

// DocStore 文档事件流写端口：事件追加与回执写入必须同事务原子完成。
type DocStore interface {
	// DeleteDoc 删除级联（RFC-0014 §3/§4，DeleteRoom 先例）：本档事件/outbox/
	// 回执/FTS 索引全清并落墓碑行——曾存在可审计、内容不可恢复；
	// 墓碑事件（doc.deleted）由命令面先落再触发本清理。
	DeleteDoc(ctx context.Context, docID string) error
	// AppendDocEvents 追加事件（version 由存储按文档分配），并同事务写 doc_outbox。
	AppendDocEvents(ctx context.Context, envelopes []protocol.DocEnvelope) ([]protocol.DocEnvelope, error)
	// AppendDocEventsWithReceipt 事件 + 幂等回执同事务落库。事务内依次：回执键已存在 →
	// ErrDuplicateReceipt；ExpectedDocVersion 与当前版本不符 → ErrVersionConflict；
	// 成功则回执 DocVersion 权威回填为追加后的版本。
	AppendDocEventsWithReceipt(ctx context.Context, envelopes []protocol.DocEnvelope, receipt CommandReceipt) ([]protocol.DocEnvelope, error)
	// LookupDocReceipt 查幂等回执；不存在返回 (nil, nil)。
	LookupDocReceipt(ctx context.Context, tenantID, idempotencyKey, commandKind string) (*CommandReceipt, error)
	// DocVersion 文档当前版本（最新 version；未见事件为 0）。
	DocVersion(ctx context.Context, docID string) (int64, error)
	// DocExists 是否已见 doc.created。
	DocExists(ctx context.Context, docID string) (bool, error)
}

// DocCASStore 乐观并发追加（可选能力；base_version 与当前 version 在
// BEGIN IMMEDIATE 临界区内判定，封死 check-then-append 竞态）。
type DocCASStore interface {
	// AppendDocEventsIf 当前文档版本 == expectedDocVersion 才追加（同事务判定，
	// 不符返回 ErrVersionConflict，整批回滚）。
	AppendDocEventsIf(ctx context.Context, envelopes []protocol.DocEnvelope, expectedDocVersion int64) ([]protocol.DocEnvelope, error)
}

// StoredDocEvent 读路径 DTO：权威信封 + opaque cursor（version 即 doc:{id} 频道
// SSE cursor；订阅续传与历史读共用）。
type StoredDocEvent struct {
	Envelope protocol.DocEnvelope
	Cursor   string
}

// DocEventReader 读端口（transport 层消费；SQLite 实现于 internal/storage/sqlite）。
type DocEventReader interface {
	// DocEventsAfter 从 cursor 之后按 version 续读；next 为空串表示已追平。
	DocEventsAfter(ctx context.Context, docID, cursor string, limit int) (events []StoredDocEvent, next string, err error)
}

// DocSearchHit 文档全文检索命中（对外视图：每文档取最新命中版本；
// position 语义由 version 承担，供编辑器/卡片跳转定位）。
type DocSearchHit struct {
	DocID      string `json:"doc_id"`
	EventID    string `json:"event_id"`
	Version    int64  `json:"version"`
	Actor      string `json:"actor"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	OccurredAt string `json:"occurred_at"`
}

// DocSearcher 全文检索端口（RFC-0014 §2.8：标题+正文派生索引；FTS5 trigram
// 生产实现，≥3 rune MATCH / <3 LIKE 回退——与 room.MessageSearcher 同规）。
type DocSearcher interface {
	SearchDocs(ctx context.Context, query string, limit int) ([]DocSearchHit, error)
}

// DocSummary 文档主页行（RFC-0014 §2.8：标题/当前版本/最近更新时间与更新者/
// 创建者——生命周期状态供"已归档"分组展示）。JSON 字段即对外契约形态。
type DocSummary struct {
	DocID     string `json:"doc_id"`
	Title     string `json:"title"`
	Version   int64  `json:"version"`
	Status    string `json:"status"` // active | archived（删除级联后不出现在列表）
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

// DocLister 文档列表读端口（RFC-0014 §2.8 文档主页：创建者筛选 = "我/各 agent"，
// createdBy 空串 = 全部；按更新时间倒序）。已删除文档随级联清除自然不出现。
type DocLister interface {
	ListDocs(ctx context.Context, createdBy string) ([]DocSummary, error)
}
