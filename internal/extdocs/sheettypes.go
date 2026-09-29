// 表格交换中立模型（RFC-0017 M2）：provider 适配器与交换服务之间的快照/
// 导出/报告词汇。坐标一律 1-based（与 protocol.SheetCell 同口径）；云公式
// 原文含引导 =（provider 原样进出，服务层转 protocol 时剥前导 =）。
package extdocs

import (
	"context"
	"fmt"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// CloudCell 单元格中立类型判别。
const (
	CloudCellText     = "text"
	CloudCellNumber   = "number"
	CloudCellBool     = "bool"
	CloudCellDatetime = "datetime"
	CloudCellFormula  = "formula"
)

// CloudCell 云端读回的单个单元格。Kind 取 CloudCell* 之一；内容按 Kind 携带：
// Text（text/datetime RFC3339/不可解析日期的原文回退）、Number、Bool、
// Formula（云端原文，含引导 =）。字面错误值（云端格存的就是 #DIV/0! 而非
// 公式）以 Text 携带原文——导入侧按普通文本落库并在报告注记。
type CloudCell struct {
	R       int      `json:"r"`
	C       int      `json:"c"`
	Kind    string   `json:"kind"`
	Text    string   `json:"text,omitempty"`
	Number  *float64 `json:"number,omitempty"`
	Bool    *bool    `json:"bool,omitempty"`
	Formula string   `json:"formula,omitempty"`
}

// CloudSheet 云端单张工作表快照。Rows/Cols 为云端申报网格（供本地限额比对，
// 读区截断由 provider 落 issues）；MergeCount 仅计数（合并单元格不迁移）。
type CloudSheet struct {
	SheetID    string      `json:"sheet_id"`
	Name       string      `json:"name"`
	Rows       int         `json:"rows"`
	Cols       int         `json:"cols"`
	MergeCount int         `json:"merge_count"`
	Cells      []CloudCell `json:"cells"`
}

// CloudSnapshot 导入快照（整簿）。
type CloudSnapshot struct {
	Title  string       `json:"title"`
	Sheets []CloudSheet `json:"sheets"`
}

// ExportSheet / ExportWorkbook 导出侧视图（本地表格 → 云端副本）。Cells 为
// protocol.SheetCell（Type=text|number|bool|datetime|formula；null 不携带）。
type ExportSheet struct {
	Name  string               `json:"name"`
	Rows  int                  `json:"rows"`
	Cols  int                  `json:"cols"`
	Cells []protocol.SheetCell `json:"cells"`
}

// ExportWorkbook 导出整簿。
type ExportWorkbook struct {
	Title  string        `json:"title"`
	Sheets []ExportSheet `json:"sheets"`
}

// SheetCaps 静态能力声明（取值来自 RFC-0017 §10 M0 双平台实测档案；未知
// 能力按未验证处理——接口存在不等于目标账号已获授权）。
type SheetCaps struct {
	ReadFormulas  bool `json:"read_formulas"`  // 公式原文可读
	WriteFormulas bool `json:"write_formulas"` // 公式原文可写
	ReadStyles    bool `json:"read_styles"`    // 样式可读（导入方向）
	WriteStyles   bool `json:"write_styles"`   // 样式可写（导出方向）
	WriteBool     bool `json:"write_bool"`     // 布尔字面可写（飞书 false → =TRUE() 公式形态）
	WriteDatetime bool `json:"write_datetime"` // 类型化日期可写（飞书两步写；腾讯 false）
	NumberFloat64 bool `json:"number_float64"` // 数值通道 float64（>15 位有效数字须转文本）
}

// 转换报告 severity / action 封闭枚举。
const (
	IssueInfo       = "info"      // 保真迁移注记（如布尔在云端以公式形态存在）
	IssueWarning    = "warning"   // 有损但可继续（截断/转文本/回读差异）
	IssueBlocked    = "blocked"   // 阻止该操作（全簿公式超限等）
	ActionKept      = "kept"      // 原样保留
	ActionConverted = "converted" // 显式转值/转形态
	ActionDropped   = "dropped"   // 该项不迁移
	ActionTruncated = "truncated" // 超限截断
)

// TransferIssue 转换报告项（RFC §3.6：sheet_id/range/feature/severity/action/reason）。
type TransferIssue struct {
	Sheet    string `json:"sheet,omitempty"`
	Range    string `json:"range,omitempty"`
	Feature  string `json:"feature"`
	Severity string `json:"severity"`
	Action   string `json:"action,omitempty"`
	Reason   string `json:"reason"`
}

// SheetWriteResult 建副本结果（导出方向）。
type SheetWriteResult struct {
	TargetURL string          `json:"target_url"`
	Issues    []TransferIssue `json:"issues"`
}

// ErrSheetNotInTarget 结构不匹配：本地工作表在远端目标不存在（M3 回写
// 不传播结构——须重新选择范围或导出新副本）。
var ErrSheetNotInTarget = fmt.Errorf("extdocs: 目标缺少同名工作表（结构不匹配，回写不传播结构）")

// SheetTargetSession 已有远端目标的写入会话（M3 回写）：一次解析跨块复用；
// 仅值域覆写（结构只读校验），样式不更新（日期格 formatter 例外——值语义
// 保真所需）。
type SheetTargetSession interface {
	// WriteChunk 一块类型化覆写（cells 行列有序；返回本块形态转换 issue）。
	WriteChunk(ctx context.Context, sheetName string, cells []protocol.SheetCell) ([]TransferIssue, error)
	// VerifySheet 回读校验（M2 建副本同款比对，差异成 issue 返回）。
	VerifySheet(ctx context.Context, sheetName string, expect []protocol.SheetCell) []TransferIssue
	Close() error
}

// SheetProvider 可选表格能力端口（Provider 的扩展面；type-switch 判定，未
// 实现 = 该 provider 不支持表格交换）。sheetNames 空 = 全部工作表。
type SheetProvider interface {
	Provider
	SheetCapabilities() SheetCaps
	ReadSheetSnapshot(ctx context.Context, rawURL string, sheetNames []string) (*CloudSnapshot, []TransferIssue, error)
	CreateSheetCopy(ctx context.Context, wb ExportWorkbook) (SheetWriteResult, error)
	// OpenSheetTarget 打开已有目标回写会话（M3；一次解析跨块复用）。
	OpenSheetTarget(ctx context.Context, rawURL string) (SheetTargetSession, error)
}
