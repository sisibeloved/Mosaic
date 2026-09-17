// 文档面 HTTP 端点（RFC-0014 / ADR-0014）：命令上行镜像房间纪律——guardWrite
// 三件套、1MiB MaxBytesReader + DisallowUnknownFields、错误以稳定 code 映射；
// 读端点只读不设 owner token（与房间 GET 一致）。doc:{id} SSE 频道语义与房间
// 订阅一致（cursor = doc version；慢消费者断流发 resync_required）。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// docCommandRequest 文档命令 DTO（DocCommand 契约；解码留在本包手工实现——
// DisallowUnknownFields 严格拒收纪律同房间命令面）。
type docCommandRequest struct {
	CommandKind        string          `json:"command_kind"`
	ExpectedDocVersion int64           `json:"expected_doc_version"`
	IdempotencyKey     string          `json:"idempotency_key"`
	IssuedAt           string          `json:"issued_at"`
	Payload            json.RawMessage `json:"payload"`
}

// docConflictResponse 409 响应体（DocVersionConflict 契约）：error 恒在；
// version_conflict 时含当前 version 与当前态摘要（RFC-0014 §2.3——客户端在
// 新态上重放本地未提交 ops）。
type docConflictResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	DocID          string        `json:"doc_id,omitempty"`
	CurrentVersion int64         `json:"current_version,omitempty"`
	State          *doc.DocState `json:"state,omitempty"`
}

// docsAvailable 文档面装配门（nil DocSVC = 测试装配未装配文档面——404 不暴露面，
// 回执/监控端点 nil 先例平移）。
func (s *server) docsAvailable(w http.ResponseWriter) bool {
	if s.deps.DocSVC == nil {
		writeError(w, http.StatusNotFound, "docs_unavailable", "文档面未装配")
		return false
	}
	return true
}

// CreateDoc 创建文档（POST /v1/docs = create_doc 命令便捷包装——创建走集合端点，
// 与 POST /v1/rooms 同纪律）；SubmitDocCommand 其余变更命令。
func (s *server) CreateDoc(w http.ResponseWriter, r *http.Request) {
	s.executeDoc(w, r, "")
}

func (s *server) SubmitDocCommand(w http.ResponseWriter, r *http.Request, docID apigen.DocID) {
	s.executeDoc(w, r, docID)
}

func (s *server) executeDoc(w http.ResponseWriter, r *http.Request, docID string) {
	if !s.docsAvailable(w) {
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	traceID := ensureTraceID(w, r)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 命令体 1MiB 上限（二轮审校 #20 先例）
	var req docCommandRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "命令体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "命令体 JSON 不合法："+err.Error())
		return
	}
	s.debugf("httpapi: doc command received", "trace_id", traceID, "kind", req.CommandKind, "doc", docID)
	res, err := s.deps.DocSVC.ExecuteCommand(r.Context(), s.docActor(), doc.Command{
		DocID:              docID,
		CommandKind:        req.CommandKind,
		ExpectedDocVersion: req.ExpectedDocVersion,
		IdempotencyKey:     req.IdempotencyKey,
		IssuedAt:           req.IssuedAt,
		Payload:            req.Payload,
	})
	if err != nil {
		s.debugf("httpapi: doc command rejected", "trace_id", traceID, "kind", req.CommandKind, "err", err)
		s.writeDocDomainError(w, r, docID, err)
		return
	}
	s.debugf("httpapi: doc command committed", "trace_id", traceID,
		"event_id", res.EventID, "doc_version", res.DocVersion, "replayed", res.Replayed)
	writeJSON(w, http.StatusOK, apigen.DocCommandResponse{
		DocId:      res.DocID,
		EventId:    res.EventID,
		DocVersion: res.DocVersion,
		Replayed:   res.Replayed,
	})
}

// docActor 本地 owner 行动者（room.Actor 装配注入的同一人格——ADR-0009）。
func (s *server) docActor() doc.Actor {
	return doc.Actor{ParticipantID: s.deps.Actor.ParticipantID, Kind: s.deps.Actor.Kind}
}

// writeDocDomainError 文档域错误 → 稳定 code/状态码。版本冲突（含提交事务内的
// 竞态拒绝）尽量附当前 version 与当前态摘要（RFC-0014 §2.3——客户端在新态上
// 重放本地未提交 ops）；当前态读取失败降级为纯 error 体（不掩盖 409 本体）。
func (s *server) writeDocDomainError(w http.ResponseWriter, r *http.Request, docID string, err error) {
	code, status := "internal", http.StatusInternalServerError
	var vc *doc.VersionConflictError
	switch {
	case errors.Is(err, doc.ErrInvalidCommand):
		code, status = "invalid_command", http.StatusBadRequest
	case errors.As(err, &vc):
		s.writeDocVersionConflict(w, r, vc.DocID, vc.Current, err)
		return
	case errors.Is(err, doc.ErrVersionConflict):
		s.writeDocVersionConflict(w, r, docID, -1, err)
		return
	case errors.Is(err, doc.ErrIdempotencyConflict), errors.Is(err, doc.ErrDuplicateReceipt):
		code, status = "idempotency_conflict", http.StatusConflict
	case errors.Is(err, doc.ErrDocNotFound):
		code, status = "doc_not_found", http.StatusNotFound
	case errors.Is(err, doc.ErrDocArchived):
		code, status = "doc_archived", http.StatusConflict
	}
	if status == http.StatusInternalServerError && s.deps.Logger != nil {
		s.deps.Logger.Error("httpapi: doc command failed", "err", err)
	}
	writeError(w, status, code, err.Error())
}

// writeDocVersionConflict 409 version_conflict 响应体：doc_id + current_version +
// state（当前态可读时；版本号以刚读的权威态为准——竞态窗口内可能已推进）。
func (s *server) writeDocVersionConflict(w http.ResponseWriter, r *http.Request, docID string, current int64, err error) {
	var body docConflictResponse
	body.Error.Code = "version_conflict"
	body.Error.Message = err.Error()
	if docID != "" {
		body.DocID = docID
		if state, gerr := s.deps.DocSVC.GetDoc(r.Context(), docID); gerr == nil {
			body.State = &state
			body.CurrentVersion = state.Version
			writeJSON(w, http.StatusConflict, body)
			return
		}
	}
	if current >= 0 {
		body.CurrentVersion = current
		writeJSON(w, http.StatusConflict, body)
		return
	}
	writeError(w, http.StatusConflict, "version_conflict", err.Error())
}

// ListDocs 文档列表（GET /v1/docs；RFC-0014 §2.8 文档主页）。created_by 过滤
// 走存储端口；status 过滤在 handler 层（列表规模个人版，不繁衍 SQL 维度）。
func (s *server) ListDocs(w http.ResponseWriter, r *http.Request, params apigen.ListDocsParams) {
	if !s.docsAvailable(w) {
		return
	}
	createdBy := ""
	if params.CreatedBy != nil {
		createdBy = *params.CreatedBy
	}
	status := ""
	if params.Status != nil {
		if !params.Status.Valid() {
			writeError(w, http.StatusBadRequest, "bad_status", "status 取值 active | archived")
			return
		}
		status = string(*params.Status)
	}
	docs, err := s.deps.DocSVC.ListDocs(r.Context(), createdBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := []doc.DocSummary{}
	for _, d := range docs {
		if status != "" && d.Status != status {
			continue
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"docs": out})
}

// GetDoc 文档快照（GET /v1/docs/{doc_id}）：当前态含块清单（折叠自事件流）。
func (s *server) GetDoc(w http.ResponseWriter, r *http.Request, docID apigen.DocID) {
	if !s.docsAvailable(w) {
		return
	}
	state, err := s.deps.DocSVC.GetDoc(r.Context(), docID)
	if err != nil {
		s.writeDocDomainError(w, r, docID, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// ExportDoc 导出 markdown（GET /v1/docs/{doc_id}/export；归档可导出，删除后 404）。
func (s *server) ExportDoc(w http.ResponseWriter, r *http.Request, docID apigen.DocID) {
	if !s.docsAvailable(w) {
		return
	}
	md, _, err := s.deps.DocSVC.ExportDoc(r.Context(), docID)
	if err != nil {
		s.writeDocDomainError(w, r, docID, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	// doc_id 形态白名单（^doc_[0-9a-f]{12}$），直接入文件名无注入面
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", docID+".md"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(md))
}

// SearchDocs 文档全文检索（GET /v1/docs/search?q=；校验纪律同房内检索）。
func (s *server) SearchDocs(w http.ResponseWriter, r *http.Request, params apigen.SearchDocsParams) {
	if !s.docsAvailable(w) {
		return
	}
	q := strings.TrimSpace(params.Q)
	if q == "" || len([]rune(q)) > 200 {
		writeError(w, http.StatusBadRequest, "bad_query", "q 必填 1..200 字")
		return
	}
	limit := 20
	if params.Limit != nil {
		if *params.Limit < 1 || *params.Limit > 100 {
			writeError(w, http.StatusBadRequest, "bad_limit", "limit 须为 1..100")
			return
		}
		limit = *params.Limit
	}
	hits, err := s.deps.DocSVC.SearchDocs(r.Context(), q, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search_failed", err.Error())
		return
	}
	if hits == nil {
		hits = []doc.DocSearchHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

// SubscribeDocEvents doc:{id} 频道 SSE（SubscribeRoomEvents 先例平移）：
// cursor = doc version；先订阅后追平（position 去重兜底）；慢消费者断流发
// resync_required。hub 键 "doc:"+docID——与房间频道同枢纽不同命名空间。
func (s *server) SubscribeDocEvents(w http.ResponseWriter, r *http.Request, docID apigen.DocID, params apigen.SubscribeDocEventsParams) {
	if !s.docsAvailable(w) {
		return
	}
	if s.deps.DocReader == nil {
		writeError(w, http.StatusNotFound, "docs_unavailable", "文档事件读路径未装配")
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	if cursor == "" {
		cursor = r.Header.Get("Last-Event-ID") // 浏览器自动重连的标准续传位
	}
	if _, err := protocol.DecodeCursor(cursor); err != nil {
		writeError(w, http.StatusBadRequest, "bad_cursor", err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_streaming", "响应不支持流式")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	lastPos, _ := protocol.DecodeCursor(cursor)
	writeStored := func(ev doc.StoredDocEvent) {
		pos, err := protocol.DecodeCursor(ev.Cursor)
		if err != nil || pos <= lastPos {
			return
		}
		lastPos = pos
		writeSSE(w, ev.Envelope.Type, ev.Cursor, mustMarshalDocView(ev))
	}

	// 订阅先于追平：追平查询与订阅之间的事件会在两个通道各出现一次，position 去重兜底
	sub := s.deps.Hub.Subscribe("doc:"+docID, 256)
	defer sub.Close()

	writeResync := func(reason string) {
		fmt.Fprintf(w, "event: resync_required\ndata: %s\n\n", mustMarshalJSON(map[string]any{"reason": reason}))
		flusher.Flush()
	}

	// 追平分页：积压超一批（1000）也必须补齐——next 游标循环续读直至追平
	cur := cursor
	for {
		events, next, err := s.deps.DocReader.DocEventsAfter(r.Context(), docID, cur, 1000)
		if err != nil {
			writeResync("catch_up_failed") // 缺口/失败 → 具名信号，客户端走快照恢复
			return
		}
		for _, ev := range events {
			writeStored(ev)
		}
		if next == "" || len(events) == 0 {
			break
		}
		cur = next
	}
	fmt.Fprint(w, ": stream: open\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-sub.C:
			if !ok {
				writeResync("slow_consumer") // 断流必须可见：客户端携最后 id 重连或走快照
				fmt.Fprint(w, ": server: slow-consumer\n\n")
				flusher.Flush()
				return
			}
			if ev.Cursor == "" {
				continue // doc 频道无瞬态帧；防御性跳过（房间频道纪律不交叉）
			}
			pos, err := protocol.DecodeCursor(ev.Cursor)
			if err != nil || pos <= lastPos {
				continue
			}
			lastPos = pos
			fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.Cursor, ev.Type, ev.Data)
			flusher.Flush()
		}
	}
}

// DocHubConsumer 把 doc outbox 条目转成外部视图投递到 doc:{id} 频道
// （HubConsumer 先例平移；best-effort 永不返错——SSE 通道毒条目跳过即可）。
// entry.RoomID 承载 doc_id、entry.GlobalPos 承载 doc version（见 sqlite.PendingDoc）。
func DocHubConsumer(hub *sse.Hub) outbox.Consumer {
	return outbox.ConsumerFunc(func(_ context.Context, entry outbox.Entry) error {
		var env protocol.DocEnvelope
		if err := json.Unmarshal(entry.Envelope, &env); err != nil {
			return nil
		}
		cursor := protocol.EncodeCursor(entry.GlobalPos)
		view := protocol.ToDocEventView(env, cursor)
		data, err := json.Marshal(view)
		if err != nil {
			return nil
		}
		hub.Publish("doc:"+entry.RoomID, sse.ViewEvent{Cursor: cursor, Type: env.Type, Data: data})
		return nil
	})
}

func mustMarshalDocView(ev doc.StoredDocEvent) []byte {
	view := protocol.ToDocEventView(ev.Envelope, ev.Cursor)
	raw, err := json.Marshal(view)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}
