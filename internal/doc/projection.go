// 文档投影折叠（RFC-0014 §2.2/§2.3）：DocState 是 doc 事件流的纯函数——
// 可重建、可回放（ADR-0014 投影纪律）。折叠对回放容错：畸形载荷/失效锚点跳过
// 而非报错（事件为权威，回放必须总能完成）；写入侧的严格校验在 ApplyDocOps。
package doc

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// 上限与护栏（RFC-0014 §2.9 建议值）。
const (
	// MaxTitleRunes 标题字长上限（与 Schema maxLength 200 一致）。
	MaxTitleRunes = 200
	// MaxBodyBytes 单文档正文上限（正文 = 全部块 text 字节量之和）。
	MaxBodyBytes = 256 << 10
	// MaxOpsPerBatch 单条 revision 的 ops 数上限（防抖批合理上界）。
	MaxOpsPerBatch = 64
	// MaxBatchBytes 单条 revision 命令批体上限（命令体 1MiB 既有约束内）。
	MaxBatchBytes = 128 << 10
)

// 文档生命周期状态（RFC-0014 §2.2：active / archived；删除走墓碑先例——
// deleted 仅在级联清除前的折叠中可观测）。
const (
	StatusActive   = "active"
	StatusArchived = "archived"
	StatusDeleted  = "deleted"
)

// validBlockTypes 块类型封闭集（RFC-0014 §2.2 首版；表格块随表格立项另行裁定）。
var validBlockTypes = map[string]bool{
	"heading": true, "paragraph": true, "list": true,
	"code": true, "quote": true, "hr": true,
}

// DocState 文档当前态（派生态，权威为事件流；UpdatedAt/UpdatedBy 取最新事件）。
type DocState struct {
	DocID     string              `json:"doc_id"`
	Title     string              `json:"title"`
	Format    string              `json:"format"`
	CreatedBy string              `json:"created_by"`
	CreatedAt string              `json:"created_at"`
	Version   int64               `json:"version"`
	Status    string              `json:"status"` // active | archived | deleted
	Blocks    []protocol.DocBlock `json:"blocks"`
	UpdatedAt string              `json:"updated_at"`
	UpdatedBy string              `json:"updated_by"`
}

// ProjectDoc 纯折叠：doc 事件流 → 当前态。未见 doc.created 返回零值
// （调用方以 Version==0 判不存在）。回放容错：畸形载荷与失效锚点跳过。
func ProjectDoc(events []StoredDocEvent) (DocState, error) {
	st := DocState{Status: StatusActive, Blocks: []protocol.DocBlock{}}
	for _, ev := range events {
		env := ev.Envelope
		switch env.Type {
		case protocol.EventDocCreated:
			var p protocol.DocCreatedPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				st.DocID = p.DocID
				st.Title = p.Title
				st.Format = p.Format
				st.CreatedBy = p.CreatedBy
				st.CreatedAt = env.OccurredAt
				st.Status = StatusActive
			}
		case protocol.EventDocRenamed:
			var p protocol.DocRenamedPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				st.Title = p.Title
			}
		case protocol.EventDocRevisionCommitted:
			var p protocol.DocRevisionCommittedPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				// 回放应用与写入校验同一函数（不双轨）；rejected op 在回放期跳过——
				// 落库前经严格模式校验，正常流中不会出现。
				st.Blocks, _ = ApplyDocOps(st.Blocks, p.Ops, nil)
			}
		case protocol.EventDocArchived:
			st.Status = StatusArchived
		case protocol.EventDocRestored:
			st.Status = StatusActive
		case protocol.EventDocDeleted:
			st.Status = StatusDeleted
		}
		if env.Version > st.Version {
			st.Version = env.Version
		}
		st.UpdatedAt = env.OccurredAt
		st.UpdatedBy = env.Actor.ParticipantID
	}
	return st, nil
}

// OpStatus 单条 op 的应用结果（逐 op 拒绝非整批失败——agent 友好，
// ApplyCuratedOps 先例；人类命令路径由服务层把任一 rejected 升级为整批拒绝）。
type OpStatus struct {
	Index   int    `json:"index"`
	Op      string `json:"op"`
	Status  string `json:"status"` // applied | rejected
	Reason  string `json:"reason,omitempty"`
	BlockID string `json:"block_id,omitempty"` // applied 后的目标块
}

// ApplyDocOps 应用一批块操作：返回新块清单与逐 op 结果（原切片不被修改）。
// 逐 op 校验与应用依序进行——同批内后续 op 可锚定前面 op 新插入的块。
// newBlockID 为 insert_after/append 携带空 block_id 的块分配服务端 ID
// （blk_<8hex> 形态由注入方保证）；nil = 不分配（回放/严格校验路径——
// 缺 ID 的插入 op 拒绝）。超限检查按结果态逐 op 判定（正文 256 KiB 硬门）。
func ApplyDocOps(blocks []protocol.DocBlock, ops []protocol.DocOp, newBlockID func() string) ([]protocol.DocBlock, []OpStatus) {
	next := append([]protocol.DocBlock(nil), blocks...)
	body := bodyBytes(next)
	statuses := make([]OpStatus, 0, len(ops))
	indexOf := func(id string) int {
		for i := range next {
			if next[i].BlockID == id {
				return i
			}
		}
		return -1
	}
	for i, op := range ops {
		st := OpStatus{Index: i, Op: op.Op, Status: "applied"}
		reject := func(reason string) {
			st.Status, st.Reason = "rejected", reason
		}
		switch op.Op {
		case "insert_after", "append":
			switch {
			case op.Block == nil:
				reject(op.Op + " 缺 block")
			case !validBlockTypes[op.Block.Type]:
				reject("非法 block.type " + quote(op.Block.Type))
			default:
				b := *op.Block
				if b.BlockID == "" {
					if newBlockID == nil {
						reject("插入块缺 block_id（写入路径须先规范化分配）")
						break
					}
					b.BlockID = newBlockID()
				}
				switch {
				case indexOf(b.BlockID) >= 0:
					reject("block_id 与既有块重复 " + quote(b.BlockID))
				case body+len(b.Text) > MaxBodyBytes:
					reject("插入后超出单文档正文上限（256 KiB）")
				default:
					pos := len(next) // append
					if op.Op == "insert_after" {
						pos = 0 // null 锚点 = 插入文首
						if op.BlockID != nil {
							idx := indexOf(*op.BlockID)
							if idx < 0 {
								reject("锚点块不存在 " + quote(*op.BlockID))
								break
							}
							pos = idx + 1
						}
					}
					if st.Status == "applied" {
						next = append(next, protocol.DocBlock{})
						copy(next[pos+1:], next[pos:])
						next[pos] = b
						body += len(b.Text)
						st.BlockID = b.BlockID
					}
				}
			}
		case "replace":
			switch {
			case op.BlockID == nil || *op.BlockID == "":
				reject("replace 缺锚点 block_id")
			default:
				idx := indexOf(*op.BlockID)
				switch {
				case idx < 0:
					reject("锚点块不存在 " + quote(*op.BlockID))
				case body+len(op.Text)-len(next[idx].Text) > MaxBodyBytes:
					reject("替换后超出单文档正文上限（256 KiB）")
				default:
					body += len(op.Text) - len(next[idx].Text)
					next[idx].Text = op.Text
					st.BlockID = *op.BlockID
				}
			}
		case "delete":
			switch {
			case op.BlockID == nil || *op.BlockID == "":
				reject("delete 缺锚点 block_id")
			default:
				idx := indexOf(*op.BlockID)
				if idx < 0 {
					reject("锚点块不存在 " + quote(*op.BlockID))
				} else {
					body -= len(next[idx].Text)
					next = append(next[:idx], next[idx+1:]...)
					st.BlockID = *op.BlockID
				}
			}
		default:
			reject("未知 op " + quote(op.Op))
		}
		statuses = append(statuses, st)
	}
	return next, statuses
}

// bodyBytes 正文字节量（全部块 text 之和；护栏计量口径）。
func bodyBytes(blocks []protocol.DocBlock) int {
	n := 0
	for _, b := range blocks {
		n += len(b.Text)
	}
	return n
}

func quote(s string) string { return `"` + s + `"` }

// RenderMarkdown 导出渲染（RFC-0014 §2.8 导出 .md）：标题作 H1，块依序排列、
// 块间空行；块 text 即 markdown 行内内容原样输出（hr 空 text 渲染为 ---）。
func RenderMarkdown(st DocState) string {
	var sb strings.Builder
	sb.WriteString("# ")
	sb.WriteString(st.Title)
	sb.WriteString("\n")
	for _, b := range st.Blocks {
		sb.WriteString("\n")
		text := b.Text
		if b.Type == "hr" && strings.TrimSpace(text) == "" {
			text = "---"
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ExcerptRunes 语境注入摘录上限（RFC-0014 §2.9：每文档 8k runes——与附件摘录
// 同型护栏）。
const ExcerptRunes = 8000

// RenderExcerpt 语境注入摘录渲染（RFC-0014 §2.7 读面①：按引用注入的有界摘录）。
// 每块带 [block_id] 前缀——agent 的 insert_after 锚点与小节定位据此解析；
// 超 maxRunes 截断并如实标注（不全量灌入）。redact 为 DLP 剔除函数（秘密形状
// 整段替换；附件摘录同纪律——文档同样可能含密钥），nil = 不剔除。
func RenderExcerpt(st DocState, maxRunes int, redact func(string) string) string {
	var sb strings.Builder
	sb.WriteString("《")
	sb.WriteString(st.Title)
	sb.WriteString("》(")
	sb.WriteString(st.DocID)
	sb.WriteString(" v")
	sb.WriteString(strconv.FormatInt(st.Version, 10))
	sb.WriteString(" ")
	sb.WriteString(st.Status)
	sb.WriteString(")\n")
	for _, b := range st.Blocks {
		sb.WriteString("[")
		sb.WriteString(b.BlockID)
		sb.WriteString("] ")
		text := b.Text
		if b.Type == "hr" && strings.TrimSpace(text) == "" {
			text = "---"
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	out := sb.String()
	if redact != nil {
		out = redact(out)
	}
	if maxRunes > 0 {
		if rs := []rune(out); len(rs) > maxRunes {
			out = string(rs[:maxRunes]) + "\n…（摘录已截断，全文见文档）"
		}
	}
	return out
}
