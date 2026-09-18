// UT 层：doc 投影折叠与 ops 应用——四 op 语义、逐 op 拒绝、上限护栏、
// 折叠正确性（回放容错）与导出渲染。
package doc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

func blk(id, typ, text string) protocol.DocBlock {
	return protocol.DocBlock{BlockID: id, Type: typ, Text: text}
}

func statusesOK(t *testing.T, statuses []OpStatus) {
	t.Helper()
	for _, st := range statuses {
		if st.Status != "applied" {
			t.Fatalf("ops[%d] 应 applied，got %+v", st.Index, st)
		}
	}
}

func TestApplyDocOpsAllKinds(t *testing.T) {
	base := []protocol.DocBlock{blk("b1", "paragraph", "一"), blk("b2", "paragraph", "二")}
	head := blk("bh", "heading", "# 标题")
	ops := []protocol.DocOp{
		{Op: "append", Block: &head}, // 文末
		{Op: "insert_after", BlockID: strPtr("b1"), Block: &protocol.DocBlock{BlockID: "b3", Type: "quote", Text: "引"}}, // b1 之后
		{Op: "replace", BlockID: strPtr("b2"), Text: "二（改）"},
		{Op: "delete", BlockID: strPtr("b3")},
	}
	next, statuses := ApplyDocOps(base, ops, nil)
	statusesOK(t, statuses)
	if len(next) != 3 || next[0].BlockID != "b1" || next[1].Text != "二（改）" || next[2].BlockID != "bh" {
		t.Fatalf("应用结果不符：%+v", next)
	}
	// 原切片不被修改
	if base[1].Text != "二" || len(base) != 2 {
		t.Fatalf("输入被修改：%+v", base)
	}
	// 状态记录目标块
	if statuses[0].BlockID != "bh" || statuses[2].BlockID != "b2" {
		t.Fatalf("OpStatus 不符：%+v", statuses)
	}
}

func TestApplyDocOpsInsertFrontAndGeneratedID(t *testing.T) {
	base := []protocol.DocBlock{blk("b1", "paragraph", "一")}
	newBlock := protocol.DocBlock{Type: "heading", Text: "# 首"} // 空 block_id
	var n int
	gen := func() string { n++; return fmt.Sprintf("blk_gen%02d", n) }
	next, statuses := ApplyDocOps(base, []protocol.DocOp{
		{Op: "insert_after", BlockID: nil, Block: &newBlock}, // null 锚点 = 文首
	}, gen)
	statusesOK(t, statuses)
	if len(next) != 2 || next[0].Type != "heading" || next[0].BlockID == "" || next[1].BlockID != "b1" {
		t.Fatalf("文首插入/ID 分配不符：%+v", next)
	}
	// nil 生成器 → 缺 ID 插入拒绝（回放/严格校验路径）
	_, statuses = ApplyDocOps(base, []protocol.DocOp{{Op: "append", Block: &newBlock}}, nil)
	if statuses[0].Status != "rejected" {
		t.Fatalf("nil 生成器应拒绝空 block_id 插入：%+v", statuses[0])
	}
}

func TestApplyDocOpsRejections(t *testing.T) {
	base := []protocol.DocBlock{blk("b1", "paragraph", "一")}
	dup := blk("b1", "paragraph", "重")
	badType := blk("b2", "table", "x")
	cases := []struct {
		name string
		op   protocol.DocOp
	}{
		{"未知 op", protocol.DocOp{Op: "move", BlockID: strPtr("b1")}},
		{"append 缺 block", protocol.DocOp{Op: "append"}},
		{"非法 block.type", protocol.DocOp{Op: "append", Block: &badType}},
		{"block_id 重复", protocol.DocOp{Op: "append", Block: &dup}},
		{"insert_after 锚点不存在", protocol.DocOp{Op: "insert_after", BlockID: strPtr("nope"), Block: &protocol.DocBlock{BlockID: "b9", Type: "paragraph", Text: "x"}}},
		{"replace 缺锚点", protocol.DocOp{Op: "replace", Text: "x"}},
		{"replace 锚点不存在", protocol.DocOp{Op: "replace", BlockID: strPtr("nope"), Text: "x"}},
		{"delete 锚点不存在", protocol.DocOp{Op: "delete", BlockID: strPtr("nope")}},
	}
	for _, tc := range cases {
		next, statuses := ApplyDocOps(base, []protocol.DocOp{tc.op}, nil)
		if statuses[0].Status != "rejected" || statuses[0].Reason == "" {
			t.Errorf("%s：应 rejected 且附原因，got %+v", tc.name, statuses[0])
		}
		if len(next) != 1 || next[0].Text != "一" {
			t.Errorf("%s：拒绝不得改动态：%+v", tc.name, next)
		}
	}
}

// 逐 op 拒绝非整批失败：混合批内合法 op 继续应用（agent 友好；人类路径由服务升级整批拒）。
func TestApplyDocOpsMixedBatchPartial(t *testing.T) {
	base := []protocol.DocBlock{blk("b1", "paragraph", "一")}
	b := blk("b2", "paragraph", "二")
	next, statuses := ApplyDocOps(base, []protocol.DocOp{
		{Op: "delete", BlockID: strPtr("nope")}, // rejected
		{Op: "append", Block: &b},               // applied
	}, nil)
	if statuses[0].Status != "rejected" || statuses[1].Status != "applied" {
		t.Fatalf("逐 op 状态不符：%+v", statuses)
	}
	if len(next) != 2 {
		t.Fatalf("合法 op 应继续应用：%+v", next)
	}
}

// 结果态上限：replace 推过 256 KiB → 该 op 拒绝且不改写。
func TestApplyDocOpsBodyLimit(t *testing.T) {
	base := []protocol.DocBlock{blk("b1", "paragraph", strings.Repeat("a", 250*1024))}
	next, statuses := ApplyDocOps(base, []protocol.DocOp{
		{Op: "replace", BlockID: strPtr("b1"), Text: strings.Repeat("b", 260*1024)},
	}, nil)
	if statuses[0].Status != "rejected" || len(next[0].Text) != len(strings.Repeat("a", 250*1024)) {
		t.Fatalf("超限 replace 应拒绝且不改写：%+v", statuses[0])
	}
	// 边界内 replace 通过
	next, statuses = ApplyDocOps(base, []protocol.DocOp{
		{Op: "replace", BlockID: strPtr("b1"), Text: strings.Repeat("c", 100*1024)},
	}, nil)
	statusesOK(t, statuses)
	if len(next[0].Text) != 100*1024 {
		t.Fatal("边界内 replace 应生效")
	}
}

// ---- 折叠 ----

func foldEvent(version int64, typ string, payload any) StoredDocEvent {
	return StoredDocEvent{Envelope: protocol.DocEnvelope{
		EventID: "evt_fold", TenantID: "ten_local", DocID: "doc_aaaaaaaaaaaa",
		Version: version, Type: typ, SchemaVersion: 1,
		OccurredAt: "2026-09-16T10:00:00.000Z",
		Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload:    mustJSON(payload),
		Metadata:   map[string]any{},
	}}
}

func TestProjectDocFold(t *testing.T) {
	anchor := "evt_seed"
	b := protocol.DocBlock{BlockID: "b1", Type: "paragraph", Text: "一"}
	events := []StoredDocEvent{
		foldEvent(1, protocol.EventDocCreated, protocol.DocCreatedPayload{
			DocID: "doc_aaaaaaaaaaaa", Title: "原题", Format: "markdown",
			CreatedBy: "par_owner", AnchorMessageID: &anchor,
		}),
		foldEvent(2, protocol.EventDocRevisionCommitted, protocol.DocRevisionCommittedPayload{
			DocID: "doc_aaaaaaaaaaaa", BaseVersion: 1, Version: 2,
			Ops: []protocol.DocOp{{Op: "append", Block: &b}}, Actor: "par_owner", Source: "human_editor",
		}),
		foldEvent(3, protocol.EventDocRenamed, protocol.DocRenamedPayload{DocID: "doc_aaaaaaaaaaaa", Title: "新题"}),
		foldEvent(4, protocol.EventDocArchived, map[string]any{"doc_id": "doc_aaaaaaaaaaaa"}),
		foldEvent(5, protocol.EventDocRestored, map[string]any{"doc_id": "doc_aaaaaaaaaaaa"}),
		// 回放容错：畸形载荷跳过而非中断
		{Envelope: protocol.DocEnvelope{
			EventID: "evt_bad", DocID: "doc_aaaaaaaaaaaa", Version: 6,
			Type: protocol.EventDocRevisionCommitted, OccurredAt: "2026-09-16T11:00:00.000Z",
			Actor: protocol.Actor{ParticipantID: "par_kimi", Kind: "agent"}, Payload: []byte(`{broken`),
		}},
	}
	st, err := ProjectDoc(events)
	if err != nil {
		t.Fatal(err)
	}
	if st.DocID != "doc_aaaaaaaaaaaa" || st.Title != "新题" || st.Format != "markdown" ||
		st.CreatedBy != "par_owner" || st.CreatedAt == "" || st.Version != 6 ||
		st.Status != StatusActive || len(st.Blocks) != 1 || st.Blocks[0].Text != "一" ||
		st.UpdatedBy != "par_kimi" || st.UpdatedAt != "2026-09-16T11:00:00.000Z" {
		t.Fatalf("折叠结果不符：%+v", st)
	}

	// 删除事件 → deleted（级联前的折叠可观测）
	st, _ = ProjectDoc(append(events, foldEvent(7, protocol.EventDocDeleted, protocol.DocDeletedPayload{
		DocID: "doc_aaaaaaaaaaaa", Reason: "清理",
	})))
	if st.Status != StatusDeleted {
		t.Fatalf("删除折叠 status = %s", st.Status)
	}
	// 空流 → 零值（调用方判不存在）
	st, _ = ProjectDoc(nil)
	if st.Version != 0 {
		t.Fatalf("空流应为零值：%+v", st)
	}
}

// ---- 渲染 ----

func TestRenderMarkdown(t *testing.T) {
	st := DocState{
		Title: "渲染",
		Blocks: []protocol.DocBlock{
			blk("b1", "heading", "# 节"),
			blk("b2", "paragraph", "正文 *强调*"),
			blk("b3", "list", "- 甲\n- 乙"),
			blk("b4", "code", "```go\nfmt.Println()\n```"),
			blk("b5", "quote", "> 引文"),
			blk("b6", "hr", ""),
		},
	}
	want := "# 渲染\n\n# 节\n\n正文 *强调*\n\n- 甲\n- 乙\n\n```go\nfmt.Println()\n```\n\n> 引文\n\n---\n"
	if got := RenderMarkdown(st); got != want {
		t.Fatalf("渲染不符：\nwant %q\ngot  %q", want, got)
	}
	// 空正文：仅标题
	if got := RenderMarkdown(DocState{Title: "空"}); got != "# 空\n" {
		t.Fatalf("空文档渲染 = %q", got)
	}
	// list 块规范化（2026-09-18 狗粮：纯行列表与段落渲染无别）——块类型即语义：
	// 纯行补 "- "；已有标记（含有序/缩进）原样不动。
	plain := DocState{Title: "t", Blocks: []protocol.DocBlock{
		blk("b1", "list", "甲\n乙\n\n丙"),
		blk("b2", "list", "1. 一\n2. 二"),
		blk("b3", "list", "- 甲\n  - 乙"),
	}}
	wantPlain := "# t\n\n- 甲\n- 乙\n\n- 丙\n\n1. 一\n2. 二\n\n- 甲\n  - 乙\n"
	if got := RenderMarkdown(plain); got != wantPlain {
		t.Fatalf("list 规范化不符：\nwant %q\ngot  %q", wantPlain, got)
	}
}

// TestRenderExcerpt 语境摘录渲染（RFC-0014 §2.7 读面①）：[block_id] 锚点前缀
// （agent 的 insert_after/小节定位据此解析）、8k runes 截断如实标注、DLP 秘密
// 形状剔除（附件摘录同纪律——文档同样可能含密钥）。
func TestRenderExcerpt(t *testing.T) {
	st := DocState{
		DocID: "doc_0123456789ab", Title: "设计稿", Version: 3, Status: StatusActive,
		Blocks: []protocol.DocBlock{
			blk("blk_00000001", "heading", "目标"),
			blk("blk_00000002", "paragraph", "做一个文档系统"),
			blk("blk_00000003", "hr", ""),
		},
	}
	out := RenderExcerpt(st, ExcerptRunes, nil)
	for _, want := range []string{"《设计稿》(doc_0123456789ab v3 active)", "[blk_00000001] 目标", "[blk_00000002] 做一个文档系统", "[blk_00000003] ---"} {
		if !strings.Contains(out, want) {
			t.Fatalf("摘录缺 %q：\n%s", want, out)
		}
	}

	// 截断：超 maxRunes 截断并标注（截断后总长 ≈ maxRunes + 标注）
	long := DocState{DocID: "doc_0123456789ab", Title: "长", Version: 1, Status: StatusActive,
		Blocks: []protocol.DocBlock{blk("b1", "paragraph", strings.Repeat("文", ExcerptRunes+100))}}
	truncated := RenderExcerpt(long, ExcerptRunes, nil)
	if !strings.Contains(truncated, "摘录已截断") {
		t.Fatal("超限应标注截断")
	}
	if n := len([]rune(truncated)); n > ExcerptRunes+20 {
		t.Fatalf("截断后应 ≈ %d runes，got %d", ExcerptRunes, n)
	}

	// DLP：秘密形状整段替换（与生产装配同一组合：agent.RedactSecrets 注入）
	secret := DocState{DocID: "doc_0123456789ab", Title: "密钥", Version: 1, Status: StatusActive,
		Blocks: []protocol.DocBlock{blk("b1", "paragraph", "token 是 sk-abcdefghijklmnop1234 请保密")}}
	scrubbed := RenderExcerpt(secret, ExcerptRunes, agent.RedactSecrets)
	if strings.Contains(scrubbed, "sk-abcdefghijklmnop1234") {
		t.Fatalf("秘密形状未剔除：\n%s", scrubbed)
	}
	if !strings.Contains(scrubbed, "[REDACTED]") {
		t.Fatalf("应替换为 [REDACTED]：\n%s", scrubbed)
	}
}
