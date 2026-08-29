// Package httpapi：HTTP 传输边界（ADR-0001：幂等命令上行 + SSE 游标订阅下行）。
// 对外只出现 EventView（无 seq/tenant，RFC-0001 P0）；错误以稳定 code 映射状态码。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// Deps 传输层依赖。
type Deps struct {
	SVC    *room.Service
	Reader room.EventReader
	Hub    *sse.Hub
	Actor  room.Actor // 个人版：本地 owner（ADR-0009）
	// Harness 宿主层可执行程序注册表（nil 时相关端点返回 503）。
	Harness     *harness.Registry
	ProbeRunner harness.Runner // 手动登记时的探测执行面（nil 时禁止手动登记）
	Logger      *slog.Logger
}

// New 构造路由（含 healthz）。
func New(deps Deps) http.Handler {
	s := &server{deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("POST /v1/rooms", s.handleCreateRoom)
	mux.HandleFunc("POST /v1/rooms/{room_id}/commands", s.handleCommand)
	mux.HandleFunc("GET /v1/rooms/{room_id}/events", s.handleEvents)
	mux.HandleFunc("GET /v1/rooms/{room_id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /v1/harness/executables", s.handleListExecutables)
	mux.HandleFunc("POST /v1/harness/executables", s.handleAddExecutable)
	mux.HandleFunc("POST /v1/harness/executables/{id}/enable", s.handleEnableExecutable(true))
	mux.HandleFunc("POST /v1/harness/executables/{id}/disable", s.handleEnableExecutable(false))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

type server struct {
	deps Deps
}

// commandRequest 对外命令 DTO（command.schema.json 契约）。
type commandRequest struct {
	CommandKind         string          `json:"command_kind"`
	ExpectedRoomVersion int64           `json:"expected_room_version"`
	IdempotencyKey      string          `json:"idempotency_key"`
	IssuedAt            string          `json:"issued_at"`
	Payload             json.RawMessage `json:"payload"`
}

type commandResponse struct {
	RoomID      string `json:"room_id"`
	EventID     string `json:"event_id"`
	RoomVersion int64  `json:"room_version"`
	Replayed    bool   `json:"replayed"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	s.execute(w, r, "")
}

func (s *server) handleCommand(w http.ResponseWriter, r *http.Request) {
	s.execute(w, r, r.PathValue("room_id"))
}

func (s *server) execute(w http.ResponseWriter, r *http.Request, roomID string) {
	var req commandRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "命令体 JSON 不合法："+err.Error())
		return
	}
	res, err := s.deps.SVC.ExecuteCommand(r.Context(), s.deps.Actor, room.Command{
		RoomID:              roomID,
		CommandKind:         req.CommandKind,
		ExpectedRoomVersion: req.ExpectedRoomVersion,
		IdempotencyKey:      req.IdempotencyKey,
		IssuedAt:            req.IssuedAt,
		Payload:             req.Payload,
	})
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commandResponse{
		RoomID:      res.RoomID,
		EventID:     res.EventID,
		RoomVersion: res.RoomVersion,
		Replayed:    res.Replayed,
	})
}

func (s *server) writeDomainError(w http.ResponseWriter, err error) {
	code, status := "internal", http.StatusInternalServerError
	switch {
	case errors.Is(err, room.ErrInvalidCommand):
		code, status = "invalid_command", http.StatusBadRequest
	case errors.Is(err, room.ErrVersionConflict):
		code, status = "version_conflict", http.StatusConflict
	case errors.Is(err, room.ErrIdempotencyConflict):
		code, status = "idempotency_conflict", http.StatusConflict
	case errors.Is(err, room.ErrRoomNotFound):
		code, status = "room_not_found", http.StatusNotFound
	}
	if status == http.StatusInternalServerError && s.deps.Logger != nil {
		s.deps.Logger.Error("httpapi: command failed", "err", err)
	}
	writeError(w, status, code, err.Error())
}

// handleEvents：SSE 订阅。cursor 空串 = 从头；先订阅后追平（间隙事件由 position 去重）。
// 慢消费者被断流时以注释行提示并结束响应，客户端携最后 id 重连追平。
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room_id")
	cursor := r.URL.Query().Get("cursor")
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
	writeStored := func(ev room.StoredEvent) {
		pos, err := protocol.DecodeCursor(ev.Cursor)
		if err != nil || pos <= lastPos {
			return
		}
		lastPos = pos
		writeSSE(w, ev.Envelope.Type, ev.Cursor, mustMarshalView(ev))
	}

	// 订阅先于追平：追平查询与订阅之间的事件会在两个通道各出现一次，position 去重兜底
	sub := s.deps.Hub.Subscribe(roomID, 256)
	defer sub.Close()

	writeResync := func(reason string) {
		fmt.Fprintf(w, "event: resync_required\ndata: %s\n\n", mustMarshalJSON(map[string]any{"reason": reason}))
		flusher.Flush()
	}

	// 追平分页：积压超一批（1000）也必须补齐——next 游标循环续读直至追平
	cur := cursor
	for {
		events, next, err := s.deps.Reader.EventsAfter(r.Context(), roomID, cur, 1000)
		if err != nil {
			writeResync("catch_up_failed") // RFC-0001 §订阅：缺口/失败 → 具名信号，客户端走快照恢复
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
				// 瞬态帧（draft.update）：无 id、不入去重序——断线不补发，正式内容以事件流为准
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, ev.Data)
				flusher.Flush()
				continue
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

// HubConsumer 把 outbox 条目转成外部视图投递给 Hub（main 接线用）。
func HubConsumer(hub *sse.Hub) outbox.Consumer {
	return outbox.ConsumerFunc(func(_ context.Context, entry outbox.Entry) {
		var env protocol.Envelope
		if err := json.Unmarshal(entry.Envelope, &env); err != nil {
			return
		}
		cursor := protocol.EncodeCursor(entry.GlobalPos)
		view := protocol.ToEventView(env, cursor)
		data, err := json.Marshal(view)
		if err != nil {
			return
		}
		hub.Publish(entry.RoomID, sse.ViewEvent{Cursor: cursor, Type: env.Type, Data: data})
	})
}

func writeSSE(w http.ResponseWriter, event, id string, data []byte) {
	fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, event, data)
}

func mustMarshalJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

func mustMarshalView(ev room.StoredEvent) []byte {
	view := protocol.ToEventView(ev.Envelope, ev.Cursor)
	raw, err := json.Marshal(view)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorResponse
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

// ---- 宿主层可执行程序端点（RFC-0002 双层管理的宿主面）----

func (s *server) handleListExecutables(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"executables": s.deps.Harness.List()})
}

type manualExecutableRequest struct {
	Adapter string `json:"adapter"`
	Runtime string `json:"runtime"`
	Distro  string `json:"distro"`
	Path    string `json:"path"`
	Version string `json:"version"`
}

func (s *server) handleAddExecutable(w http.ResponseWriter, r *http.Request) {
	if s.deps.Harness == nil || s.deps.ProbeRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表/探测面未配置")
		return
	}
	var req manualExecutableRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "登记体不合法："+err.Error())
		return
	}
	if err := s.deps.Harness.AddManual(r.Context(), s.deps.ProbeRunner, harness.Executable{
		Adapter: req.Adapter, Runtime: req.Runtime, Distro: req.Distro,
		Path: req.Path, Version: req.Version,
	}); err != nil {
		if errors.Is(err, harness.ErrInvalidEntry) {
			writeError(w, http.StatusBadRequest, "invalid_entry", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "registered"})
}

func (s *server) handleEnableExecutable(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Harness == nil {
			writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
			return
		}
		if err := s.deps.Harness.SetEnabled(r.PathValue("id"), enabled); err != nil {
			switch {
			case errors.Is(err, harness.ErrLoginRequired):
				writeError(w, http.StatusConflict, "login_required", err.Error())
			case errors.Is(err, harness.ErrNotFound):
				writeError(w, http.StatusNotFound, "not_found", err.Error())
			default:
				s.writeDomainError(w, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": enabled})
	}
}

// handleSnapshot：快照四元组（room_version + opaque watermark + 投影/算法版本 + Timeline）。
func (s *server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room_id")
	var events []room.StoredEvent
	cursor := ""
	for {
		batch, next, err := s.deps.Reader.EventsAfter(r.Context(), roomID, cursor, 1000)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
			return
		}
		events = append(events, batch...)
		if next == "" || len(batch) == 0 {
			break
		}
		cursor = next
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "room_not_found", "房间不存在或尚无事件")
		return
	}
	writeJSON(w, http.StatusOK, room.ProjectSnapshot(roomID, events))
}

// handleIndex：Timeline 最小 UI（内嵌单页；React/Vite SPA 随 M2 接入）。
func (s *server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

// DraftConsumer 把引擎草稿流桥到 SSE（draft.update 瞬态帧：无 id、不入事件日志、
// 断线不补发——客户端以最新草稿状态渲染，正式消息以 message.posted 为准）。
func DraftConsumer(hub *sse.Hub) room.DraftSink {
	return func(roomID, participantID string, u agent.DraftUpdate) {
		data, err := json.Marshal(map[string]any{
			"room_id": roomID, "participant_id": participantID,
			"kind": u.Kind, "text": u.Text, "stage": u.Stage,
		})
		if err != nil {
			return
		}
		hub.Publish(roomID, sse.ViewEvent{Cursor: "", Type: "draft.update", Data: data})
	}
}
