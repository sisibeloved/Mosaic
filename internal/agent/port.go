// Package agent 定义 Harness 端口与适配器契约（RFC-0002 v0.5，Approved）。
// 端口层为域所有：任务、结果、取消、epoch、usage；适配器细节不出本包。
package agent

import (
	"context"
	"errors"
	"time"
)

// ErrStale 表示结果因 grant epoch / fencing 失配被丢弃：
// 不发布任何正文类事件，但允许提交状态事件（RFC-0002 §3.1.6）。
var ErrStale = errors.New("agent: stale grant")

// TaskKind 任务类型（RFC-0002 §3.1.2）。
type TaskKind string

const (
	KindObserve         TaskKind = "observe"
	KindEvaluateIntent  TaskKind = "evaluate_intent"
	KindGenerate        TaskKind = "generate"
	KindSummarize       TaskKind = "summarize"
	KindEvaluateClosure TaskKind = "evaluate_closure"
)

// Grant 发言许可绑定（RFC-0003 floor.granted 的任务侧投影）。
type Grant struct {
	GrantID         string `json:"grant_id"`
	Rank            int    `json:"rank"`
	RevealStrategy  string `json:"reveal_strategy"`
	ViewCursor      string `json:"view_cursor"` // opaque 视图游标（RFC-0001 v0.4：全局 seq 仅内部）
	Epoch           int64  `json:"epoch"`
}

// Context 统一讨论输入（RFC-0007 组装产物；M0 为最小占位结构）。
type Context struct {
	Inline     map[string]any `json:"inline"`
	ReceiptRef string         `json:"receipt_ref"`
	ViewCursor string         `json:"view_cursor"`
}

// Task 端口任务（域 → 适配器）。
type Task struct {
	TaskID        string    `json:"task_id"`
	Kind          TaskKind  `json:"kind"`
	ParticipantID string    `json:"participant_id"`
	RoomID        string    `json:"room_id"`
	ThreadID      string    `json:"thread_id"`
	Epoch         string    `json:"epoch"`
	Grant         *Grant    `json:"grant,omitempty"`
	Deadline      time.Time `json:"deadline"`
	Context       Context   `json:"context"`
}

// DraftUpdate 草稿流安全子集（RFC-0002 v0.5：广播前需过可见性与 DLP/secret scan）。
type DraftUpdate struct {
	Kind  string `json:"kind"`            // text_delta | stage
	Text  string `json:"text,omitempty"`  // text_delta 内容
	Stage string `json:"stage,omitempty"` // queued | evaluating | generating | validating
}

// Usage 自报用量；缺失（nil）记 unknown，不虚构（RFC-0002 §3.4.2）。
type Usage struct {
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Model        string `json:"model"`
}

// Result 端口结果（适配器 → 域）：结构化块为端口级规范。
type Result struct {
	Block      string         `json:"block"` // turn_intent | attention_assessment | public_draft | grounded_summary | closure_intent
	Data       map[string]any `json:"data"`
	Usage      *Usage         `json:"usage,omitempty"`
	StopReason string         `json:"stop_reason"`
}

// Handle 单任务句柄：Updates 为草稿流（可提前关闭），Result 阻塞至完成或 ErrStale。
type Handle interface {
	Updates() <-chan DraftUpdate
	Result() (Result, error)
	Cancel()
}

// Session 逻辑会话：与进程寿命解耦（RFC-0002 v0.5），由 supervisor 管理所有权。
type Session interface {
	Run(ctx context.Context, task Task) (Handle, error)
	Cancel(taskID string)
	Close()
}

// Capabilities 适配器能力声明（RFC-0002 §3.1.2）。
type Capabilities struct {
	Streaming      bool   `json:"streaming"`
	CancelMode     string `json:"cancel_mode"`      // notify | interrupt | none
	HistoryChannel string `json:"history_channel"` // mcp | structured_request（生产适配器禁止 none）
	Continuity     bool   `json:"continuity"`
	UsageReporting bool   `json:"usage_reporting"`
	Observe        bool   `json:"observe"`
}

// Adapter 适配器接口（M0 最小面；进程管理细节由各适配器自持）。
type Adapter interface {
	Name() string
	Capabilities() Capabilities
	Boot(ctx context.Context, profile Profile) (Session, error)
}

// Profile Agent Profile 最小内嵌形态（v0.5 双层管理的宿主层落 ADR-0010 后扩展）。
type Profile struct {
	ProfileID     string `json:"profile_id"`
	Version       int64  `json:"version"`
	Adapter       string `json:"adapter"`
	ExecutableRef string `json:"executable_ref"`
	DisplayName   string `json:"display_name"`
}
