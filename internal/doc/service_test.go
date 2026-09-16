// UT 层：doc 命令处理域——幂等 receipt、乐观并发（base_version CAS）、
// 生命周期门、ops 校验、删除级联纪律、副本与导出。
package doc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// ---- 测试装置 ----

var testClock = func() string { return "2026-09-16T09:00:00.000Z" }

func newTestService(store *MemStore) *Service {
	var mu sync.Mutex
	var docN, blkN, evtN int64
	return NewService(Config{
		Store:    store,
		Searcher: store,
		Clock:    testClock,
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			evtN++
			return fmt.Sprintf("%s_test_%08d", prefix, evtN)
		},
		NewDocID: func() string {
			mu.Lock()
			defer mu.Unlock()
			docN++
			return fmt.Sprintf("doc_%012d", docN)
		},
		NewBlockID: func() string {
			mu.Lock()
			defer mu.Unlock()
			blkN++
			return fmt.Sprintf("blk_%08d", blkN)
		},
		Tenant: "ten_local",
	})
}

func docCmd(kind, docID, idem string, version int64, payload any) Command {
	raw, _ := json.Marshal(payload)
	return Command{
		DocID:              docID,
		CommandKind:        kind,
		ExpectedDocVersion: version,
		IdempotencyKey:     idem,
		IssuedAt:           "2026-09-16T08:59:59.000Z",
		Payload:            raw,
	}
}

var (
	humanActor = Actor{ParticipantID: "par_owner", Kind: "human"}
	botActor   = Actor{ParticipantID: "par_kimi", Kind: "agent"}
)

const (
	validUUIDv7  = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e6f"
	validUUIDv7b = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e70"
	validUUIDv7c = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e71"
	validUUIDv7d = "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e72"
)

// createForTest 快捷建文档（带一个 paragraph 初始块，block_id "blk_seed"）。
func createForTest(t *testing.T, svc *Service, idem string) string {
	t.Helper()
	res, err := svc.ExecuteCommand(context.Background(), humanActor, docCmd("create_doc", "", idem, 0, map[string]any{
		"title": "测试文档",
		"initial_blocks": []map[string]any{
			{"block_id": "blk_seed", "type": "paragraph", "text": "种子块"},
		},
	}))
	if err != nil {
		t.Fatalf("create_doc: %v", err)
	}
	return res.DocID
}

// ---- 命令门 ----

func TestCommandGateValidation(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()

	bad := docCmd("create_doc", "", "not-a-uuid", 0, map[string]any{"title": "x"})
	if _, err := svc.ExecuteCommand(ctx, humanActor, bad); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("非 UUIDv7 幂等键应拒收，got %v", err)
	}
	bad = docCmd("create_doc", "", validUUIDv7, 0, map[string]any{"title": "x"})
	bad.IssuedAt = "yesterday"
	if _, err := svc.ExecuteCommand(ctx, humanActor, bad); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("非法 issued_at 应拒收，got %v", err)
	}
	if _, err := svc.ExecuteCommand(ctx, botActor, docCmd("create_doc", "", validUUIDv7, 0, map[string]any{"title": "x"})); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("命令通道拒 agent actor（代写走 CommitRevision），got %v", err)
	}
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("nuke_doc", "", validUUIDv7, 0, map[string]any{})); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("未知命令应拒收，got %v", err)
	}
}

// ---- create_doc ----

func TestCreateDoc(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()

	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", "", validUUIDv7, 0, map[string]any{
		"title":             "发布清单",
		"anchor_message_id": "evt_anchor_01",
	}))
	if err != nil {
		t.Fatalf("create_doc: %v", err)
	}
	if res.DocVersion != 1 || res.Replayed {
		t.Fatalf("结果不符：%+v", res)
	}
	state, err := svc.GetDoc(ctx, res.DocID)
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	if state.Title != "发布清单" || state.Format != "markdown" || state.Status != StatusActive ||
		state.CreatedBy != "par_owner" || state.Version != 1 || len(state.Blocks) != 0 {
		t.Fatalf("state 不符：%+v", state)
	}
	// anchor 透传进 doc.created 载荷
	events := store.DocEvents(res.DocID)
	var created protocol.DocCreatedPayload
	if err := json.Unmarshal(events[0].Payload, &created); err != nil {
		t.Fatal(err)
	}
	if created.AnchorMessageID == nil || *created.AnchorMessageID != "evt_anchor_01" {
		t.Fatalf("anchor_message_id 未透传：%+v", created)
	}
}

func TestCreateDocWithInitialBlocks(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()

	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", "", validUUIDv7, 0, map[string]any{
		"title": "带初始块",
		"initial_blocks": []map[string]any{
			{"type": "heading", "text": "# 目标"},                       // 空 block_id → 服务端分配
			{"block_id": "blk_p1", "type": "paragraph", "text": "正文"}, // 客户端 ID 保留
		},
	}))
	if err != nil {
		t.Fatalf("create_doc: %v", err)
	}
	if res.DocVersion != 2 {
		t.Fatalf("created + 首条 revision 同事务：version 应为 2，got %d", res.DocVersion)
	}
	state, err := svc.GetDoc(ctx, res.DocID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Blocks) != 2 || state.Blocks[0].Type != "heading" || state.Blocks[1].BlockID != "blk_p1" {
		t.Fatalf("初始块折叠不符：%+v", state.Blocks)
	}
	if state.Blocks[0].BlockID == "" {
		t.Fatal("空 block_id 应由服务端分配")
	}
	// revision 载荷版本字段与信封一致
	events := store2Events(svc, res.DocID)
	var rev protocol.DocRevisionCommittedPayload
	if err := json.Unmarshal(events[1].Payload, &rev); err != nil {
		t.Fatal(err)
	}
	if rev.BaseVersion != 1 || rev.Version != 2 || rev.Source != "human_editor" {
		t.Fatalf("revision 载荷不符：%+v", rev)
	}
}

func TestCreateDocValidation(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	mk := func(mutate func(map[string]any)) map[string]any {
		p := map[string]any{"title": "x"}
		mutate(p)
		return p
	}
	cases := []struct {
		name    string
		payload map[string]any
		docID   string
	}{
		{"空标题", mk(func(p map[string]any) { p["title"] = "" }), ""},
		{"超长标题", mk(func(p map[string]any) { p["title"] = strings.Repeat("题", 201) }), ""},
		{"非法 format", mk(func(p map[string]any) { p["format"] = "html" }), ""},
		{"非法 anchor", mk(func(p map[string]any) { p["anchor_message_id"] = "msg_1" }), ""},
		{"create 带 doc_id", mk(func(p map[string]any) {}), "doc_0000000000aa"},
		{"初始块超 64", mk(func(p map[string]any) {
			blocks := make([]map[string]any, 65)
			for i := range blocks {
				blocks[i] = map[string]any{"type": "paragraph", "text": "x"}
			}
			p["initial_blocks"] = blocks
		}), ""},
		{"初始块非法 type", mk(func(p map[string]any) {
			p["initial_blocks"] = []map[string]any{{"type": "table", "text": "x"}}
		}), ""},
		{"初始块 block_id 重复", mk(func(p map[string]any) {
			p["initial_blocks"] = []map[string]any{
				{"block_id": "blk_d", "type": "paragraph", "text": "a"},
				{"block_id": "blk_d", "type": "paragraph", "text": "b"},
			}
		}), ""},
	}
	for i, tc := range cases {
		idem := fmt.Sprintf("018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d%04x", i)
		_, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", tc.docID, idem, 0, tc.payload))
		if !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s：应报 ErrInvalidCommand，got %v", tc.name, err)
		}
	}
}

// ---- 幂等回放 ----

func TestIdempotentReplay(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	cmd := docCmd("create_doc", "", validUUIDv7, 0, map[string]any{"title": "幂等"})

	first, err := svc.ExecuteCommand(ctx, humanActor, cmd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ExecuteCommand(ctx, humanActor, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.EventID != first.EventID || second.DocID != first.DocID || second.DocVersion != first.DocVersion {
		t.Fatalf("回放应返回原结果：first=%+v second=%+v", first, second)
	}
	if v, _ := svc.GetDoc(ctx, first.DocID); v.Version != 1 {
		t.Fatalf("回放不得重复落事件：version = %d", v.Version)
	}
	// 同键异载荷 = 幂等冲突
	other := docCmd("create_doc", "", validUUIDv7, 0, map[string]any{"title": "改过的载荷"})
	if _, err := svc.ExecuteCommand(ctx, humanActor, other); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("同键异载荷应报 ErrIdempotencyConflict，got %v", err)
	}
}

// ---- rename_doc ----

func TestRenameDoc(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7) // version 2（created + 初始 revision）

	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("rename_doc", docID, validUUIDv7b, 2, map[string]any{"title": "改名"}))
	if err != nil {
		t.Fatalf("rename_doc: %v", err)
	}
	if res.DocVersion != 3 {
		t.Fatalf("version = %d（期望 3）", res.DocVersion)
	}
	state, _ := svc.GetDoc(ctx, docID)
	if state.Title != "改名" || state.UpdatedBy != "par_owner" {
		t.Fatalf("重命名折叠不符：%+v", state)
	}

	// 版本冲突：结构化错误（transport 渲染 409 用）
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("rename_doc", docID, validUUIDv7c, 1, map[string]any{"title": "旧版本"}))
	var vc *VersionConflictError
	if !errors.As(err, &vc) || !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("应报 VersionConflictError，got %v", err)
	}
	if vc.Expected != 1 || vc.Current != 3 {
		t.Fatalf("冲突载体不符：%+v", vc)
	}
	// 不存在
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("rename_doc", "doc_aaaaaaaaaaaa", validUUIDv7d, 0, map[string]any{"title": "x"})); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("应报 ErrDocNotFound，got %v", err)
	}
}

// ---- commit_doc_revision ----

func TestCommitDocRevision(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7) // version 2，blocks: [blk_seed("种子块")]

	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7b, 2, map[string]any{
		"base_version": 2,
		"ops": []map[string]any{
			{"op": "append", "block_id": nil, "block": map[string]any{"type": "heading", "text": "# 章节"}},
			{"op": "insert_after", "block_id": "blk_seed", "block": map[string]any{"block_id": "blk_q1", "type": "quote", "text": "引文"}},
			{"op": "replace", "block_id": "blk_seed", "text": "种子块（改）"},
			{"op": "delete", "block_id": "blk_q1"},
		},
		"note": "四种 op 混合批",
	}))
	if err != nil {
		t.Fatalf("commit_doc_revision: %v", err)
	}
	if res.DocVersion != 3 {
		t.Fatalf("version = %d（期望 3）", res.DocVersion)
	}
	state, _ := svc.GetDoc(ctx, docID)
	if len(state.Blocks) != 2 {
		t.Fatalf("blocks = %+v", state.Blocks)
	}
	if state.Blocks[0].BlockID != "blk_seed" || state.Blocks[0].Text != "种子块（改）" {
		t.Fatalf("replace 未生效：%+v", state.Blocks[0])
	}
	if state.Blocks[1].Type != "heading" || state.Blocks[1].BlockID == "" {
		t.Fatalf("append 未生效/未分配 ID：%+v", state.Blocks[1])
	}
	// 载荷 version 与信封一致；note 留痕
	var rev protocol.DocRevisionCommittedPayload
	for _, e := range store2Events(svc, docID) {
		if e.Type == protocol.EventDocRevisionCommitted && e.Version == 3 {
			_ = json.Unmarshal(e.Payload, &rev)
		}
	}
	if rev.BaseVersion != 2 || rev.Version != 3 || rev.Note != "四种 op 混合批" || len(rev.Ops) != 4 {
		t.Fatalf("revision 载荷不符：%+v", rev)
	}
	// 同批内锚定前序新块（insert_after blk_q1 后 delete blk_q1 均成功）已由结果集证明
}

func store2Events(svc *Service, docID string) []protocol.DocEnvelope {
	// 测试装置同一 MemStore：经 historyOf 同源读取
	return svc.cfg.Store.(*MemStore).DocEvents(docID)
}

func TestCommitDocRevisionConflicts(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7)

	// payload.base_version 与命令期望不一致 → 拒收（两份真值不共存）
	_, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7b, 2, map[string]any{
		"base_version": 1,
		"ops":          []map[string]any{{"op": "delete", "block_id": "blk_seed"}},
	}))
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("base_version 分歧应拒收，got %v", err)
	}
	// CAS 冲突 → VersionConflictError 携带当前版本
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7b, 1, map[string]any{
		"ops": []map[string]any{{"op": "delete", "block_id": "blk_seed"}},
	}))
	var vc *VersionConflictError
	if !errors.As(err, &vc) || vc.Current != 2 {
		t.Fatalf("CAS 冲突应携带当前版本 2，got %v", err)
	}
	// 空 ops / 超限批
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7b, 2, map[string]any{"ops": []any{}}))
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("空 ops 应拒收，got %v", err)
	}
	big := make([]map[string]any, MaxOpsPerBatch+1)
	for i := range big {
		big[i] = map[string]any{"op": "delete", "block_id": "blk_seed"}
	}
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7b, 2, map[string]any{"ops": big}))
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("ops 超 64 应拒收，got %v", err)
	}
	// 批体超 128 KiB
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7b, 2, map[string]any{
		"ops":  []map[string]any{{"op": "replace", "block_id": "blk_seed", "text": "x"}},
		"note": strings.Repeat("x", MaxBatchBytes),
	}))
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("批体超 128KiB 应拒收，got %v", err)
	}
	// 失效锚点（ops 校验失败整批拒）
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7b, 2, map[string]any{
		"ops": []map[string]any{{"op": "replace", "block_id": "blk_nope", "text": "x"}},
	}))
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("失效锚点应拒收，got %v", err)
	}
	if v, _ := svc.GetDoc(ctx, docID); v.Version != 2 {
		t.Fatalf("全部拒绝路径不得落事件：version = %d", v.Version)
	}
}

// 结果态上限：replace/append 推过 256 KiB → 该 op 拒绝（人类路径升级整批拒）。
func TestCommitDocRevisionBodyLimit(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", "", validUUIDv7, 0, map[string]any{
		"title": "大体量",
		"initial_blocks": []map[string]any{
			{"block_id": "blk_big", "type": "paragraph", "text": strings.Repeat("a", 200*1024)},
			{"block_id": "blk_small", "type": "paragraph", "text": strings.Repeat("b", 50*1024)},
		},
	}))
	if err != nil {
		t.Fatalf("seed（250 KiB）: %v", err)
	}
	_, err = svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", res.DocID, validUUIDv7b, 2, map[string]any{
		"ops": []map[string]any{
			{"op": "append", "block_id": nil, "block": map[string]any{"type": "paragraph", "text": strings.Repeat("c", 10*1024)}},
		},
	}))
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("结果态超 256 KiB 应拒收，got %v", err)
	}
}

// ---- CommitRevision（引擎代写路径）----

func TestCommitRevisionAgentPath(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7)

	res, err := svc.CommitRevision(ctx, botActor, docID, 2, []protocol.DocOp{
		{Op: "replace", BlockID: strPtr("blk_seed"), Text: "agent 落笔"},
	}, "agent", "重写第一节")
	if err != nil {
		t.Fatalf("CommitRevision: %v", err)
	}
	if res.DocVersion != 3 {
		t.Fatalf("version = %d（期望 3）", res.DocVersion)
	}
	state, _ := svc.GetDoc(ctx, docID)
	if state.Blocks[0].Text != "agent 落笔" || state.UpdatedBy != "par_kimi" {
		t.Fatalf("代写未生效：%+v", state)
	}
	var rev protocol.DocRevisionCommittedPayload
	for _, e := range store2Events(svc, docID) {
		if e.Type == protocol.EventDocRevisionCommitted {
			_ = json.Unmarshal(e.Payload, &rev)
		}
	}
	if rev.Source != "agent" || rev.Actor != "par_kimi" || rev.Note != "重写第一节" {
		t.Fatalf("代写载荷不符：%+v", rev)
	}
	// CAS 冲突
	_, err = svc.CommitRevision(ctx, botActor, docID, 1, []protocol.DocOp{{Op: "delete", BlockID: strPtr("blk_seed")}}, "agent", "")
	var vc *VersionConflictError
	if !errors.As(err, &vc) || vc.Current != 3 {
		t.Fatalf("CAS 冲突应携带当前版本，got %v", err)
	}
	// 非法 source / 空 actor
	if _, err := svc.CommitRevision(ctx, botActor, docID, 3, []protocol.DocOp{{Op: "delete", BlockID: strPtr("blk_seed")}}, "robot", ""); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("非法 source 应拒收，got %v", err)
	}
	if _, err := svc.CommitRevision(ctx, Actor{}, docID, 3, []protocol.DocOp{{Op: "delete", BlockID: strPtr("blk_seed")}}, "agent", ""); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("空 actor 应拒收，got %v", err)
	}
	// 归档后代写同样被只读门拦
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("archive_doc", docID, validUUIDv7b, 3, map[string]any{})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CommitRevision(ctx, botActor, docID, 4, []protocol.DocOp{{Op: "delete", BlockID: strPtr("blk_seed")}}, "agent", ""); !errors.Is(err, ErrDocArchived) {
		t.Fatalf("归档只读门应拦代写，got %v", err)
	}
}

func strPtr(s string) *string { return &s }

// ---- 生命周期 ----

func TestArchiveRestoreGuards(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7) // version 2

	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("archive_doc", docID, validUUIDv7b, 2, map[string]any{})); err != nil {
		t.Fatalf("archive_doc: %v", err)
	}
	state, _ := svc.GetDoc(ctx, docID)
	if state.Status != StatusArchived {
		t.Fatalf("status = %s", state.Status)
	}
	// 重复归档 → 只读门
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("archive_doc", docID, validUUIDv7c, 3, map[string]any{})); !errors.Is(err, ErrDocArchived) {
		t.Fatalf("重复归档应报 ErrDocArchived，got %v", err)
	}
	// 归档态 rename / revision → 只读门
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("rename_doc", docID, validUUIDv7c, 3, map[string]any{"title": "x"})); !errors.Is(err, ErrDocArchived) {
		t.Fatalf("归档 rename 应报 ErrDocArchived，got %v", err)
	}
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7c, 3, map[string]any{
		"ops": []map[string]any{{"op": "delete", "block_id": "blk_seed"}},
	})); !errors.Is(err, ErrDocArchived) {
		t.Fatalf("归档 revision 应报 ErrDocArchived，got %v", err)
	}
	// 归档态 export / duplicate 允许
	if _, _, err := svc.ExportDoc(ctx, docID); err != nil {
		t.Fatalf("归档可导出：%v", err)
	}
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("duplicate_doc", docID, validUUIDv7c, 3, map[string]any{})); err != nil {
		t.Fatalf("归档可复制：%v", err)
	}
	// 恢复 → active；恢复后 revision 解禁
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("restore_doc", docID, validUUIDv7d, 3, map[string]any{})); err != nil {
		t.Fatalf("restore_doc: %v", err)
	}
	state, _ = svc.GetDoc(ctx, docID)
	if state.Status != StatusActive {
		t.Fatalf("恢复后 status = %s", state.Status)
	}
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("commit_doc_revision", docID, validUUIDv7c, 4, map[string]any{
		"ops": []map[string]any{{"op": "replace", "block_id": "blk_seed", "text": "恢复后落笔"}},
	})); err != nil {
		t.Fatalf("恢复后 revision 应解禁：%v", err)
	}
}

func TestRestoreActiveRejects(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7)
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("restore_doc", docID, validUUIDv7b, 2, map[string]any{})); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("active 文档 restore 应报 ErrInvalidCommand，got %v", err)
	}
}

// ---- delete_doc ----

func TestDeleteDocCascadeDiscipline(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7) // version 2

	// reason 必填
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("delete_doc", docID, validUUIDv7b, 2, map[string]any{"reason": ""})); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("空 reason 应拒收（删除不可逆，理由留痕），got %v", err)
	}
	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("delete_doc", docID, validUUIDv7b, 2, map[string]any{"reason": "重复创建"}))
	if err != nil {
		t.Fatalf("delete_doc: %v", err)
	}
	if res.DocVersion != 3 {
		t.Fatalf("墓碑事件 version = %d（期望 3）", res.DocVersion)
	}
	// 内容级联清除，墓碑留痕
	if len(store.DocEvents(docID)) != 0 {
		t.Fatal("事件应级联清除")
	}
	reason, ok := store.Tombstone(docID)
	if !ok || reason != "重复创建" {
		t.Fatalf("墓碑留痕不符：%q %v", reason, ok)
	}
	if _, err := svc.GetDoc(ctx, docID); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("删除后 GetDoc 应报 ErrDocNotFound，got %v", err)
	}
	// 回执随级联清除——同键重放不再可回，按不存在拒绝（deleteRoom 同语义）
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("delete_doc", docID, validUUIDv7b, 2, map[string]any{"reason": "重复创建"})); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("删除后重放应报 ErrDocNotFound（回执已清），got %v", err)
	}
}

// ---- duplicate_doc ----

func TestDuplicateDoc(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	docID := createForTest(t, svc, validUUIDv7) // version 2，blocks: [blk_seed("种子块")]

	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("duplicate_doc", docID, validUUIDv7b, 2, map[string]any{}))
	if err != nil {
		t.Fatalf("duplicate_doc: %v", err)
	}
	if res.DocID == docID || res.DocVersion != 2 {
		t.Fatalf("副本应有新 doc_id 且 version=2（created+revision）：%+v", res)
	}
	src, _ := svc.GetDoc(ctx, docID)
	dup, err := svc.GetDoc(ctx, res.DocID)
	if err != nil {
		t.Fatal(err)
	}
	if dup.Title != "测试文档（副本）" || dup.Format != src.Format || dup.CreatedBy != "par_owner" {
		t.Fatalf("副本元数据不符：%+v", dup)
	}
	if len(dup.Blocks) != len(src.Blocks) || dup.Blocks[0].BlockID != "blk_seed" || dup.Blocks[0].Text != "种子块" {
		t.Fatalf("副本正文不符（block_id 保留）：%+v", dup.Blocks)
	}
	// 显式标题 + 源版本前置断言
	if _, err := svc.ExecuteCommand(ctx, humanActor, docCmd("duplicate_doc", docID, validUUIDv7c, 99, map[string]any{"title": "v99"})); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("源版本断言不符应报冲突，got %v", err)
	}
	res2, err := svc.ExecuteCommand(ctx, humanActor, docCmd("duplicate_doc", docID, validUUIDv7c, 2, map[string]any{"title": "显式副本名"}))
	if err != nil {
		t.Fatal(err)
	}
	dup2, _ := svc.GetDoc(ctx, res2.DocID)
	if dup2.Title != "显式副本名" {
		t.Fatalf("显式标题不符：%+v", dup2)
	}
	// 源文档不受影响
	if v, _ := svc.GetDoc(ctx, docID); v.Version != 2 {
		t.Fatalf("源文档不得被副本命令推进：version = %d", v.Version)
	}
}

// ---- export / search ----

func TestExportDoc(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	res, err := svc.ExecuteCommand(ctx, humanActor, docCmd("create_doc", "", validUUIDv7, 0, map[string]any{
		"title": "导出",
		"initial_blocks": []map[string]any{
			{"block_id": "blk_h", "type": "heading", "text": "# 一"},
			{"block_id": "blk_p", "type": "paragraph", "text": "正文"},
			{"block_id": "blk_r", "type": "hr", "text": ""},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	md, state, err := svc.ExportDoc(ctx, res.DocID)
	if err != nil {
		t.Fatal(err)
	}
	want := "# 导出\n\n# 一\n\n正文\n\n---\n"
	if md != want {
		t.Fatalf("导出渲染不符：\nwant %q\ngot  %q", want, md)
	}
	if state.Version != 2 {
		t.Fatalf("state 随行：%+v", state)
	}
	if _, _, err := svc.ExportDoc(ctx, "doc_ffffffffffff"); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("不存在应报 ErrDocNotFound，got %v", err)
	}
}

func TestSearchDocs(t *testing.T) {
	svc := newTestService(NewMemStore())
	ctx := context.Background()
	createForTest(t, svc, validUUIDv7)

	hits, err := svc.SearchDocs(ctx, "种子", 20)
	if err != nil || len(hits) != 1 {
		t.Fatalf("搜索命中 = %+v, %v", hits, err)
	}
	// 未装配 Searcher 如实报错
	bare := NewService(Config{
		Store: NewMemStore(), Clock: testClock,
		NewID: func(prefix string) string { return prefix + "_x" },
		NewDocID: func() string {
			return "doc_aaaaaaaaaaaa"
		},
		Tenant: "ten_local",
	})
	if _, err := bare.SearchDocs(ctx, "x", 20); err == nil {
		t.Fatal("nil Searcher 应报错")
	}
}
