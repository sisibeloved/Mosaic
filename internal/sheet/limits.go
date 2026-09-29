// 表格容量限额（RFC-0017 OQ-3 定稿，ADR-0015 记录）：本地编辑与 agent 面共用；
// 数值同时是协议 schema（doc.sheet_revision_committed）与服务端校验的事实源。
package sheet

// 首版容量护栏。对齐两家平台读取上限的量级（腾讯单次范围 ≤1000 行 ×200 列
// ×10000 格），本地维度上限取同量级：单表 ≤5000 行 ×200 列。
const (
	MaxSheets            = 20        // 工作簿工作表数上限
	MaxRows              = 5000      // 单表行数上限
	MaxCols              = 200       // 单表列数上限（列标最大 "GR"）
	MaxCellsPerBatch     = 1000      // 单修订批写入单元格数上限（set_cells 元素合计）
	MaxOpsPerBatch       = 64        // 单修订批 op 数上限（对齐 doc.MaxOpsPerBatch）
	MaxBatchBytes        = 256 << 10 // 单批序列化体积上限
	MaxTextBytes         = 8192      // 单格文本体积上限
	MaxFormulaRunes      = 512       // 单条公式长度上限
	MaxFormulasPerBook   = 5000      // 工作簿公式格总数上限
	MaxSheetNameRunes    = 60        // 工作表名长度上限
	MaxRangeCellsPerRead = 10000     // 单次范围读取格数上限（读端点）
	CalcBudgetNodes      = 200000    // 单次全簿重算的求值节点预算
)

// 布局护栏（像素）。
const (
	MinColWidth      = 40
	MaxColWidth      = 600
	MinRowHeight     = 20
	MaxRowHeight     = 200
	DefaultColWidth  = 96
	DefaultRowHeight = 26
)
