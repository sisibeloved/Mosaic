// UT 层：RFC-0014 Phase 3 房间文档集成——message.posted refs 校验与走线、
// attach_doc_to_room/detach_doc_from_room 命令链、快照 docs 段投影折叠。
package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// newDocRoomService 带文档存在性端口的房间服务（Docs = doc.MemStore；语义与
// SQLite 实现一致——删除后 DocExists 为 false）。
func newDocRoomService(store AtomicStore, docs *doc.MemStore) *Service {
	var mu sync.Mutex
	var n int64
	return NewService(Config{
		Store: store,
		Clock: testClock,
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("%s_doctest_%08d", prefix, n)
		},
		Tenant: "ten_local",
		Docs:   docs,
	})
}

// seedDoc 直落一条 doc.created（绕过 doc.Service 命令链——本测试主体是房间侧）。
func seedDoc(t *testing.T, docs *doc.MemStore, docID string) {
	t.Helper()
	payload, _ := json.Marshal(protocol.DocCreatedPayload{
		DocID: docID, Title: "seeded " + docID, Format: "markdown", CreatedBy: "par_owner",
	})
	_, err := docs.AppendDocEvents(context.Background(), []protocol.DocEnvelope{{
		EventID: "evt_docseed_" + docID, TenantID: "ten_local", DocID: docID,
		Type: protocol.EventDocCreated, SchemaVersion: 1,
		OccurredAt: testClock(), Actor: protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
		Payload: payload, Metadata: map[string]any{},
	}})
	if err != nil {
		t.Fatalf("seed doc %s: %v", docID, err)
	}
}

// docIDN / idemN 生成合法形态的测试 ID（doc_<12hex> / UUIDv7）。
func docIDN(n int) string { return fmt.Sprintf("doc_%012x", n) }
func idemN(n int) string  { return fmt.Sprintf("018f6b2e-7c1a-7b3d-9e4f-1a2b3c5e%04x", n) }

// setupDocRoom 建房（version=1）并返回所需件。
func setupDocRoom(t *testing.T, svc *Service) (roomID string) {
	t.Helper()
	created, err := svc.ExecuteCommand(context.Background(),
		Actor{ParticipantID: "par_owner", Kind: "human"},
		createCmd("create_room", idemN(1), 0, map[string]any{"display_name": "docs"}))
	if err != nil {
		t.Fatalf("create_room: %v", err)
	}
	return created.RoomID
}

func docCmd(kind, idem string, version int64, roomID, docID string) Command {
	cmd := createCmd(kind, idem, version, map[string]any{"doc_id": docID})
	cmd.RoomID = roomID
	return cmd
}

// refs 校验：kind 封闭枚举 / doc_id 形态 / ≤8 / 存在性（未知与已删均拒）。
func TestPostMessageRefsValidation(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))
	// 已删文档：doc.created 后删除级联——DocExists 转 false
	seedDoc(t, docs, docIDN(2))
	if err := docs.DeleteDoc(ctx, docIDN(2)); err != nil {
		t.Fatalf("delete doc: %v", err)
	}

	validRef := map[string]any{"kind": "doc", "doc_id": docIDN(1)}
	cases := []struct {
		name string
		refs []map[string]any
	}{
		{"kind 非 doc 拒绝", []map[string]any{{"kind": "url", "doc_id": docIDN(1)}}},
		{"doc_id 形态不符拒绝", []map[string]any{{"kind": "doc", "doc_id": "docXYZ"}}},
		{"引用未知文档拒绝", []map[string]any{{"kind": "doc", "doc_id": docIDN(99)}}},
		{"引用已删文档拒绝", []map[string]any{{"kind": "doc", "doc_id": docIDN(2)}}},
		{"超 8 条拒绝", []map[string]any{validRef, validRef, validRef, validRef, validRef, validRef, validRef, validRef, validRef}},
	}
	for i, tc := range cases {
		cmd := createCmd("post_message", idemN(100+i), 1,
			map[string]any{"body": "refs case", "refs": tc.refs})
		cmd.RoomID = roomID
		_, err := svc.ExecuteCommand(ctx, actor, cmd)
		if !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("%s：应 ErrInvalidCommand，got %v", tc.name, err)
		}
	}
	if n := len(store.RoomEvents(roomID)); n != 1 {
		t.Fatalf("全部拒绝后房间应只有 room.created，事件数 = %d", n)
	}
}

// Docs 端口 nil（旧装配）：跳过存在性校验，形态校验恒在。
func TestPostMessageRefsExistenceSkippedWithoutDocsPort(t *testing.T) {
	store := NewMemStore()
	svc := newTestService(store)
	roomID := setupDocRoom(t, svc)
	cmd := createCmd("post_message", idemN(2), 1,
		map[string]any{"body": "no docs port", "refs": []map[string]any{{"kind": "doc", "doc_id": docIDN(77)}}})
	cmd.RoomID = roomID
	if _, err := svc.ExecuteCommand(context.Background(), Actor{ParticipantID: "par_owner", Kind: "human"}, cmd); err != nil {
		t.Fatalf("Docs 端口 nil 时应跳过存在性校验，got %v", err)
	}
}

// refs 走线：校验后原样进事件载荷，投影 fold 进 TimelineItem.Refs（快照/SSE 同形数据源）。
func TestPostMessageRefsFlowIntoEventAndSnapshot(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))

	cmd := createCmd("post_message", idemN(3), 1, map[string]any{
		"body": "看这份文档",
		"refs": []map[string]any{{"kind": "doc", "doc_id": docIDN(1), "anchor_block_id": "blk_2"}},
	})
	cmd.RoomID = roomID
	res, err := svc.ExecuteCommand(ctx, actor, cmd)
	if err != nil {
		t.Fatalf("post_message with refs: %v", err)
	}

	events := store.RoomEvents(roomID)
	if len(events) != 2 || events[1].Type != protocol.EventMessagePosted {
		t.Fatalf("第二条应为 message.posted：%+v", events)
	}
	var p struct {
		Refs []protocol.DocRef `json:"refs"`
	}
	if err := json.Unmarshal(events[1].Payload, &p); err != nil {
		t.Fatalf("unmarshal refs: %v", err)
	}
	if len(p.Refs) != 1 || p.Refs[0].Kind != "doc" || p.Refs[0].DocID != docIDN(1) || p.Refs[0].AnchorBlockID != "blk_2" {
		t.Fatalf("refs 未原样进事件载荷：%+v", p.Refs)
	}

	stored, _, err := store.EventsAfter(ctx, roomID, "", 100)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	snap := ProjectSnapshot(roomID, stored)
	if len(snap.Timeline) != 1 || len(snap.Timeline[0].Refs) != 1 ||
		snap.Timeline[0].Refs[0].DocID != docIDN(1) || snap.Timeline[0].Refs[0].AnchorBlockID != "blk_2" {
		t.Fatalf("快照 TimelineItem.Refs 投影不符：%+v", snap.Timeline)
	}
	if snap.Timeline[0].EventID != res.EventID {
		t.Fatalf("时间线项事件 ID 不符：%s vs %s", snap.Timeline[0].EventID, res.EventID)
	}
}

// attach_doc_to_room 正常链：事件落日志（attached_by=操作者）、快照 docs 段投影、CAS 冲突。
func TestAttachDocToRoom(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))

	res, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(10), 1, roomID, docIDN(1)))
	if err != nil || res.Replayed || res.RoomVersion != 2 || res.EventID == "" {
		t.Fatalf("attach 应首发受理：%v %+v", err, res)
	}
	events := store.RoomEvents(roomID)
	if len(events) != 2 || events[1].Type != protocol.EventDocAttachedToRoom {
		t.Fatalf("第二条应为 doc.attached_to_room：%+v", events)
	}
	var p protocol.DocAttachedToRoomPayload
	if err := json.Unmarshal(events[1].Payload, &p); err != nil ||
		p.DocID != docIDN(1) || p.AttachedBy != "par_owner" {
		t.Fatalf("attach 载荷不符：%s", events[1].Payload)
	}

	stored, _, err := store.EventsAfter(ctx, roomID, "", 100)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	snap := ProjectSnapshot(roomID, stored)
	if len(snap.Docs) != 1 || snap.Docs[0].DocID != docIDN(1) ||
		snap.Docs[0].AttachedBy != "par_owner" || snap.Docs[0].AttachedAt != testClock() {
		t.Fatalf("快照 docs 段不符：%+v", snap.Docs)
	}

	// CAS：过期 expected_room_version 拒绝
	stale := docCmd("attach_doc_to_room", idemN(11), 99, roomID, docIDN(1))
	if _, err := svc.ExecuteCommand(ctx, actor, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本应 ErrVersionConflict，got %v", err)
	}
}

// attach 校验：doc_id 形态、存在性、房间存在。
func TestAttachDocValidation(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))

	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(20), 1, roomID, "docXYZ")); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("doc_id 形态不符应拒，got %v", err)
	}
	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(21), 1, roomID, docIDN(99))); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("附着未知文档应拒，got %v", err)
	}
	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(22), 0, "room_missing", docIDN(1))); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("房间不存在应 ErrRoomNotFound，got %v", err)
	}
}

// 重复 attach = 幂等空操作：成功返回、不追加事件、EventID 空串、版本不变。
func TestAttachDocReattachIsNoOp(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))

	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(30), 1, roomID, docIDN(1))); err != nil {
		t.Fatalf("首次 attach: %v", err)
	}
	res, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(31), 2, roomID, docIDN(1)))
	if err != nil {
		t.Fatalf("重复 attach 应为幂等空操作，got %v", err)
	}
	if res.Replayed || res.EventID != "" || res.RoomVersion != 2 {
		t.Fatalf("空操作结果不符（EventID 应空、版本不变）：%+v", res)
	}
	if n := len(store.RoomEvents(roomID)); n != 2 {
		t.Fatalf("空操作不得追加事件，事件数 = %d", n)
	}
}

// 附着上限 32（RFC-0014 §2.9）：第 33 份拒绝。
func TestAttachDocCap32(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)

	version := int64(1)
	for i := 0; i < maxAttachedDocsPerRoom; i++ {
		seedDoc(t, docs, docIDN(i))
		res, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(1000+i), version, roomID, docIDN(i)))
		if err != nil {
			t.Fatalf("第 %d 份 attach: %v", i+1, err)
		}
		version = res.RoomVersion
	}
	seedDoc(t, docs, docIDN(32))
	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(2000), version, roomID, docIDN(32))); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("第 33 份附着应拒（上限 32），got %v", err)
	}
}

// detach 正常链：事件落日志（detached_by）、快照 docs 段出集；
// detach 未附着文档 = 幂等空操作。
func TestDetachDocFromRoom(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))

	// 未附着即 detach → 幂等空操作（版本 1 不变、无新事件）
	noop, err := svc.ExecuteCommand(ctx, actor, docCmd("detach_doc_from_room", idemN(40), 1, roomID, docIDN(1)))
	if err != nil {
		t.Fatalf("detach 未附着文档应为幂等空操作，got %v", err)
	}
	if noop.EventID != "" || noop.RoomVersion != 1 || noop.Replayed {
		t.Fatalf("空操作结果不符：%+v", noop)
	}
	if n := len(store.RoomEvents(roomID)); n != 1 {
		t.Fatalf("空操作不得追加事件，事件数 = %d", n)
	}

	// attach → detach 正常链
	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(41), 1, roomID, docIDN(1))); err != nil {
		t.Fatalf("attach: %v", err)
	}
	res, err := svc.ExecuteCommand(ctx, actor, docCmd("detach_doc_from_room", idemN(42), 2, roomID, docIDN(1)))
	if err != nil || res.RoomVersion != 3 || res.EventID == "" {
		t.Fatalf("detach 应受理：%v %+v", err, res)
	}
	events := store.RoomEvents(roomID)
	if len(events) != 3 || events[2].Type != protocol.EventDocDetachedFromRoom {
		t.Fatalf("第三条应为 doc.detached_from_room：%+v", events)
	}
	var p protocol.DocDetachedFromRoomPayload
	if err := json.Unmarshal(events[2].Payload, &p); err != nil ||
		p.DocID != docIDN(1) || p.DetachedBy != "par_owner" {
		t.Fatalf("detach 载荷不符：%s", events[2].Payload)
	}
	stored, _, err := store.EventsAfter(ctx, roomID, "", 100)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	if snap := ProjectSnapshot(roomID, stored); len(snap.Docs) != 0 {
		t.Fatalf("detach 后 docs 段应为空：%+v", snap.Docs)
	}
}

// detach 免存在性校验：文档已删也可解除（关联清理由房间日志自治）。
func TestDetachDocDeletedDocAllowed(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))

	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(50), 1, roomID, docIDN(1))); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := docs.DeleteDoc(ctx, docIDN(1)); err != nil {
		t.Fatalf("delete doc: %v", err)
	}
	res, err := svc.ExecuteCommand(ctx, actor, docCmd("detach_doc_from_room", idemN(51), 2, roomID, docIDN(1)))
	if err != nil || res.RoomVersion != 3 {
		t.Fatalf("文档已删也应可 detach：%v %+v", err, res)
	}
}

// 折叠序内后者胜：attach A、attach B、detach A → docs=[B]；再 attach A → docs=[B,A]。
func TestAttachDetachFoldOrdering(t *testing.T) {
	ctx := context.Background()
	actor := Actor{ParticipantID: "par_owner", Kind: "human"}
	store := NewMemStore()
	docs := doc.NewMemStore()
	svc := newDocRoomService(store, docs)
	roomID := setupDocRoom(t, svc)
	seedDoc(t, docs, docIDN(1))
	seedDoc(t, docs, docIDN(2))

	steps := []Command{
		docCmd("attach_doc_to_room", idemN(60), 1, roomID, docIDN(1)),
		docCmd("attach_doc_to_room", idemN(61), 2, roomID, docIDN(2)),
		docCmd("detach_doc_from_room", idemN(62), 3, roomID, docIDN(1)),
	}
	for i, cmd := range steps {
		if _, err := svc.ExecuteCommand(ctx, actor, cmd); err != nil {
			t.Fatalf("步骤 %d: %v", i, err)
		}
	}
	stored, _, err := store.EventsAfter(ctx, roomID, "", 100)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	snap := ProjectSnapshot(roomID, stored)
	if len(snap.Docs) != 1 || snap.Docs[0].DocID != docIDN(2) {
		t.Fatalf("detach A 后应只剩 B：%+v", snap.Docs)
	}

	if _, err := svc.ExecuteCommand(ctx, actor, docCmd("attach_doc_to_room", idemN(63), 4, roomID, docIDN(1))); err != nil {
		t.Fatalf("再 attach A: %v", err)
	}
	stored, _, err = store.EventsAfter(ctx, roomID, "", 100)
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	snap = ProjectSnapshot(roomID, stored)
	if len(snap.Docs) != 2 || snap.Docs[0].DocID != docIDN(2) || snap.Docs[1].DocID != docIDN(1) {
		t.Fatalf("再附着 A 后应为 [B, A]：%+v", snap.Docs)
	}
}
