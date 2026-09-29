// 文档投影折叠（RFC-0014 §2.2/§2.3）：DocState 是 doc 事件流的纯函数——
// 可重建、可回放（ADR-0014 投影纪律）。折叠对回放容错：畸形载荷/失效锚点跳过
// 而非报错（事件为权威，回放必须总能完成）；写入侧的严格校验在 ApplyDocOps。
package doc

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/sheet"
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

// FormatSpreadsheet 表格资产格式（RFC-0017 M1；其余取值 = markdown 块模型）。
const FormatSpreadsheet = "spreadsheet"

// validBlockTypes 块类型封闭集（RFC-0014 §2.2 首版；表格块随表格立项另行裁定）。
var validBlockTypes = map[string]bool{
	"heading": true, "paragraph": true, "list": true,
	"code": true, "quote": true, "hr": true,
}

// DocState 文档当前态（派生态，权威为事件流；UpdatedAt/UpdatedBy 取最新事件）。
// Format 分派：markdown → Blocks；spreadsheet → Workbook（表格内容模型与
// Markdown 块模型分离——禁止把表格序列化成 Markdown 充当权威内容，RFC-0017 §3.2）。
type DocState struct {
	DocID     string              `json:"doc_id"`
	Title     string              `json:"title"`
	Format    string              `json:"format"` // markdown | spreadsheet
	CreatedBy string              `json:"created_by"`
	CreatedAt string              `json:"created_at"`
	Version   int64               `json:"version"`
	Status    string              `json:"status"` // active | archived | deleted
	Blocks    []protocol.DocBlock `json:"blocks"`
	Workbook  *sheet.Workbook     `json:"workbook,omitempty"` // format=spreadsheet
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
				if p.Format == FormatSpreadsheet {
					// 默认表 ID 由 doc_id 派生（确定性回放，RFC-0017 §3.2）
					st.Workbook = sheet.NewWorkbookForDoc(p.DocID)
				}
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
		case protocol.EventDocSheetRevisionCommitted:
			var p protocol.DocSheetRevisionCommittedPayload
			if json.Unmarshal(env.Payload, &p) == nil && st.Workbook != nil {
				// 机械回放归一化 ops；畸形 op 逐条跳过（ApplyDocOps 同款容错）
				for _, op := range p.Ops {
					_ = sheet.ApplySheetOp(st.Workbook, op)
				}
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
				if err := ValidateBlockShape(*op.Block); err != nil {
					reject(err.Error())
					break
				}
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

// ValidateBlockShape 块元数据形状校验（RFC-0016 块模型增补）：level 仅
// heading 携带且 1..6（0 = 未指定）；lang 仅 code 携带且 ≤48 runes。命令面
// ops/initial_blocks（经 ApplyDocOps）与 agent 端口校验共用语义——畸形元数据
// 在写路径被拒，不映射成零值混入事件流。
func ValidateBlockShape(b protocol.DocBlock) error {
	if b.Level != 0 {
		if b.Type != "heading" {
			return fmt.Errorf("level 仅 heading 块可携带（当前 %s）", quote(b.Type))
		}
		if b.Level < 1 || b.Level > 6 {
			return fmt.Errorf("level 须在 1..6（当前 %d）", b.Level)
		}
	}
	if b.Lang != "" {
		if b.Type != "code" {
			return fmt.Errorf("lang 仅 code 块可携带（当前 %s）", quote(b.Type))
		}
		if n := len([]rune(b.Lang)); n > 48 {
			return fmt.Errorf("lang ≤48 字符（当前 %d）", n)
		}
	}
	return nil
}

func quote(s string) string { return `"` + s + `"` }

// RenderMarkdown 导出渲染（RFC-0014 §2.8 导出 .md；RFC-0016 渲染加固）：
// 标题作 H1，块依序排列、块间空行。加固裁定：块类型即语义——heading 无
// `#` 前缀自动补（#×level，缺省 2）、code 未包 fence 自动包（lang 标注）、
// quote 行缺 `>` 前缀自动补；text 已自带标记时透传优先（存量数据与模型
// 手写标记路径不受影响）。导出面向外部文档服务（腾讯智能文档 / 飞书导入），
// 裸文本行会被当普通段落——这是共享转换链的结构性兜底。
func RenderMarkdown(st DocState) string {
	var sb strings.Builder
	sb.WriteString("# ")
	sb.WriteString(st.Title)
	sb.WriteString("\n")
	for _, b := range st.Blocks {
		sb.WriteString("\n")
		text := b.Text
		switch {
		case b.Type == "hr" && strings.TrimSpace(text) == "":
			text = "---"
		case b.Type == "list":
			text = listBlockText(text)
		case b.Type == "heading" && !headingPrefixRe.MatchString(text):
			level := b.Level
			if level < 1 || level > 6 {
				level = 2
			}
			text = strings.Repeat("#", level) + " " + text
		case b.Type == "code" && !strings.HasPrefix(strings.TrimSpace(text), "```"):
			fence := "```" + b.Lang
			text = fence + "\n" + text + "\n```"
		case b.Type == "quote" && !quotePrefixRe.MatchString(text):
			lines := strings.Split(text, "\n")
			for i, ln := range lines {
				if strings.TrimSpace(ln) != "" {
					lines[i] = "> " + ln
				}
			}
			text = strings.Join(lines, "\n")
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// headingPrefixRe 已带合法标题前缀（#×1..6 + 空白）的判定——命中则导出透传。
var headingPrefixRe = regexp.MustCompile(`^#{1,6}\s`)

// quotePrefixRe 引文行前缀判定（首个非空行命中即视为已带标记）。
var quotePrefixRe = regexp.MustCompile(`^\s*>`)

// listMarkerRe 列表标记行判定（list 块文本规范化用；编辑器渲染同口径）。
var listMarkerRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s`)

// listBlockText list 块文本规范化：全文无任何列表标记时逐非空行补 "- "——
// 块类型即语义（用户/模型按直觉写纯行，导出也应是合法 markdown 列表）；
// 已含标记的照原样（agent 写 - 项 / 1. 项 的常态路径不受影响）。
func listBlockText(text string) string {
	lines := strings.Split(text, "\n")
	for _, ln := range lines {
		if listMarkerRe.MatchString(ln) {
			return text
		}
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			lines[i] = "- " + ln
		}
	}
	return strings.Join(lines, "\n")
}

// ExcerptRunes 语境注入摘录上限（RFC-0014 §2.9：每文档 8k runes——与附件摘录
// 同型护栏）。
const ExcerptRunes = 8000

// RenderExcerpt 语境注入摘录渲染（RFC-0014 §2.7 读面①：按引用注入的有界摘录）。
// 每块带 [block_id] 前缀——agent 的 insert_after 锚点与小节定位据此解析；
// 超 maxRunes 截断并如实标注（不全量灌入）。redact 为 DLP 剔除函数（秘密形状
// 整段替换；附件摘录同纪律——文档同样可能含密钥），nil = 不剔除。
// format 分派（RFC-0017 §3.4）：spreadsheet → 表格摘录（每表首个非空
// ≤10 行 ×8 列 TSV，公式格显派生值——读面有界，不灌整个工作簿）。
func RenderExcerpt(st DocState, maxRunes int, redact func(string) string) string {
	if st.Format == FormatSpreadsheet {
		head := "《" + st.Title + "》(" + st.DocID + " v" +
			strconv.FormatInt(st.Version, 10) + " " + st.Status + ")\n"
		body := head + sheet.RenderSheetExcerpt(st.Workbook, sheet.Evaluate(st.Workbook), maxRunes)
		if redact != nil {
			body = redact(body)
		}
		return body
	}
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
