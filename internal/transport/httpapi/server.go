// Package httpapi：HTTP 传输边界（ADR-0001：幂等命令上行 + SSE 游标订阅下行）。
// 对外只出现 EventView（无 seq/tenant，RFC-0001 P0）；错误以稳定 code 映射状态码。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
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
	// 开发者模式（M1 v1.8）：Dev 开启才注册 /v1/debug 只读端点与 UI 调试面板。
	Dev    bool
	Budget contextx.Limits         // 预算上限（状态端点的水位/梯度计算基准）
	Seats  func() []room.AgentSeat // 引擎座位快照（引擎未就绪时返回空）
	Outbox outbox.Store            // outbox 积压检视（nil 时状态端点该节为 0）
	// Authority 配置的服务监听地址（host:port；四轮复审 #15）：跨源写门据此判定
	// Origin——不信请求自带的 Host（DNS rebinding 后 Origin 与 Host 会同时指向
	// 攻击者域名而相等）。空 = 测试装配退回"请求 Host + 回环 host"判定（host 仍须回环）。
	Authority string
	// ExtraOriginHosts 壳集成信任源（M2 桌面壳）：精确主机名匹配（如 wails.localhost）。
	// 浏览器对 Origin 头不可伪造，且 .localhost 顶级域不落到远端——该 Origin 只能
	// 来自本应用自带 WebView 中的页面；命中即放行（豁免回环/端口判定）。
	ExtraOriginHosts []string
	// OwnerToken 写端点认证（M2，四轮复审 #15 残留收口）：非空时全部写端点要求
	// X-Owner-Token 匹配（401）。空 = 未启用（测试装配；生产由 internal/app 恒置——
	// 装配层生成并持久化，第一方客户端经 /v1/owner/bootstrap（受跨源门保护）获取）。
	OwnerToken string
	// UI 前端产物根（index.html 位于根；M2 SPA 经 apps/web 构建）。nil = 退回
	// M1 内嵌最小 webui（测试装配 / 未装配 SPA 的兜底形态）。
	UI fs.FS
}

// New 构造路由。对外契约面（ADR-0007）由 apigen 生成的 ServerInterface +
// 路由模式接线：操作集与 spec 一致是编译期保证（漏实现即编译失败）；
// 内嵌 UI 与 -dev 调试端点不在对外契约内，保持手工注册。
func New(deps Deps) http.Handler {
	s := &server{deps: deps}
	mux := http.NewServeMux()
	// GET 兜底走 UI（SPA 静态产物 + 前端路由回退）；/v1/* 未匹配路径显式 404，
	// 不被 SPA 回退吞掉（非 dev 的 debug 端点 404 语义由此保持）。
	mux.HandleFunc("GET /", s.handleUI)
	if deps.Dev {
		// 开发者模式（M1 v1.8）：只读调试面——非 dev 不注册（404 而非 403，不暴露面）
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/state", s.handleDebugState)
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/events", s.handleDebugEvents)
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/waves", s.handleDebugWaves)
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/memory", s.handleDebugMemory)
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/export", s.handleDebugExport)
	}
	return apigen.HandlerWithOptions(s, apigen.StdHTTPServerOptions{BaseRouter: mux})
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

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// GetHealthz 存活探针（apigen.ServerInterface；对外契约操作）。
func (s *server) GetHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// CreateRoom / SubmitRoomCommand：命令上行（请求解码留在本包手工实现——
// DisallowUnknownFields 严格拒收是 M1 以来的契约纪律，strict-server 包装会
// 接管解码并静默放行未知字段，故边界模型只采用接口与路由层）。
func (s *server) CreateRoom(w http.ResponseWriter, r *http.Request) {
	s.execute(w, r, "")
}

func (s *server) SubmitRoomCommand(w http.ResponseWriter, r *http.Request, roomID apigen.RoomID) {
	s.execute(w, r, roomID)
}

func (s *server) execute(w http.ResponseWriter, r *http.Request, roomID string) {
	if !s.guardWrite(w, r, true) {
		return
	}
	traceID := ensureTraceID(w, r)                 // 命令→事件→outbox→适配器链路的排障口子
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 二轮审校 #20：命令体 1MiB 上限
	var req commandRequest
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
	s.debugf("httpapi: command received", "trace_id", traceID, "kind", req.CommandKind, "room", roomID)
	res, err := s.deps.SVC.ExecuteCommand(r.Context(), s.deps.Actor, room.Command{
		RoomID:              roomID,
		CommandKind:         req.CommandKind,
		ExpectedRoomVersion: req.ExpectedRoomVersion,
		IdempotencyKey:      req.IdempotencyKey,
		IssuedAt:            req.IssuedAt,
		Payload:             req.Payload,
	})
	if err != nil {
		s.debugf("httpapi: command rejected", "trace_id", traceID, "kind", req.CommandKind, "err", err)
		s.writeDomainError(w, err)
		return
	}
	s.debugf("httpapi: command committed", "trace_id", traceID,
		"event_id", res.EventID, "room_version", res.RoomVersion, "replayed", res.Replayed)
	writeJSON(w, http.StatusOK, apigen.CommandResponse{
		RoomId:      res.RoomID,
		EventId:     res.EventID,
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

// SubscribeRoomEvents：SSE 订阅（apigen.ServerInterface）。cursor 空串 = 从头；
// 断线自动重连（EventSource）携带 Last-Event-ID 头时以其续传（二轮审校 #14：忽略
// 该头会整段重放，UI 时间线重复）；先订阅后追平（间隙事件由 position 去重）；
// 慢消费者断流发 resync_required。
func (s *server) SubscribeRoomEvents(w http.ResponseWriter, r *http.Request, roomID apigen.RoomID, params apigen.SubscribeRoomEventsParams) {
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
// 永不返回错误：SSE 是 best-effort 通道（毒条目跳过即可，不能阻塞引擎侧的持久投递）。
func HubConsumer(hub *sse.Hub) outbox.Consumer {
	return outbox.ConsumerFunc(func(_ context.Context, entry outbox.Entry) error {
		var env protocol.Envelope
		if err := json.Unmarshal(entry.Envelope, &env); err != nil {
			return nil
		}
		cursor := protocol.EncodeCursor(entry.GlobalPos)
		view := protocol.ToEventView(env, cursor)
		data, err := json.Marshal(view)
		if err != nil {
			return nil
		}
		hub.Publish(entry.RoomID, sse.ViewEvent{Cursor: cursor, Type: env.Type, Data: data})
		return nil
	})
}

func writeSSE(w http.ResponseWriter, event, id string, data []byte) {
	fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, event, data)
}

// guardWrite 变更端点的写防护（复审 #18；四轮复审 #15 收紧；M2 owner token 收口）。三层：
//  1. Origin 头存在时必须通过 originAllowed——host 必须是回环（localhost/127.x/::1；
//     DNS rebinding 把域名解析到 127.0.0.1 时 Origin host 是攻击者域名，非回环即拒），
//     端口必须与配置 Authority（真实监听地址）一致；Authority 未配置（测试装配）时
//     与请求 Host 的端口比对（host 回环判定已先行，rebinding 不受影响）。
//     非浏览器客户端无 Origin，放行。
//  2. 带 body 的端点要求 Content-Type: application/json（跨站自定义头触发预检，
//     本服务无 CORS 应答 → 预检即拒；同时挡掉跨站表单的默认类型）。
//  3. OwnerToken 配置时要求 X-Owner-Token 匹配（401）——Origin 门的纵深防线：
//     即便跨源判定存在绕过面，无凭据写仍被拒（token 的读取口 bootstrap 自身过
//     跨源门，rebinding 页面同源化后也读不到）。
func (s *server) guardWrite(w http.ResponseWriter, r *http.Request, requireJSON bool) bool {
	if !s.guardOriginCT(w, r, requireJSON) {
		return false
	}
	if s.deps.OwnerToken != "" && r.Header.Get("X-Owner-Token") != s.deps.OwnerToken {
		writeError(w, http.StatusUnauthorized, "owner_token_required", "写端点需要 X-Owner-Token（经 /v1/owner/bootstrap 获取）")
		return false
	}
	return true
}

// guardOriginCT 前两层（跨源门 + Content-Type 门）——bootstrap 取凭据口自身
// 用这层（token 层对它是鸡蛋问题：取凭据前无凭据）。
func (s *server) guardOriginCT(w http.ResponseWriter, r *http.Request, requireJSON bool) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || !s.originAllowed(u, r.Host) {
			writeError(w, http.StatusForbidden, "origin_rejected", "跨源写被拒绝（本地 owner API）")
			return false
		}
	}
	if requireJSON && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type 须为 application/json")
		return false
	}
	return true
}

// GetOwnerBootstrap 第一方客户端引导（apigen.ServerInterface）：返回 owner token。
// 读取口本身过跨源门——DNS rebinding 页面把请求同源化后 Origin 仍是攻击者域名，
// 403 拒读；无 Origin 的本机客户端可达（同用户本机进程本就等效 owner——数据目录
// 与 token 文件同为 0600 owner-only）。未启用 token 的装配返回 404（路由由 apigen
// 按 spec 恒注册，语义在 handler 内收敛）。
func (s *server) GetOwnerBootstrap(w http.ResponseWriter, r *http.Request) {
	if s.deps.OwnerToken == "" {
		writeError(w, http.StatusNotFound, "owner_token_disabled", "本装配未启用 owner token")
		return
	}
	if !s.guardOriginCT(w, r, false) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": s.deps.OwnerToken})
}

// originAllowed 跨源判定：壳集成信任源（ExtraOriginHosts，M2 桌面壳）精确放行；
// 其余 host 侧恒要求回环（配置 authority 的 host 或通用回环名），端口侧与配置
// Authority 对齐（rebinding 后请求 Host 会随攻击者域名走，不可作准）。
func (s *server) originAllowed(u *url.URL, requestHost string) bool {
	host := u.Hostname()
	for _, trusted := range s.deps.ExtraOriginHosts {
		if host == trusted {
			return true
		}
	}
	ip := net.ParseIP(host)
	hostLoop := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !hostLoop {
		return false
	}
	port := u.Port()
	if s.deps.Authority != "" {
		_, authPort, _ := net.SplitHostPort(s.deps.Authority)
		return port == authPort
	}
	// 测试装配（无 Authority）：与请求 Host 的端口比对——host 回环判定已排除 rebinding
	_, reqPort, _ := net.SplitHostPort(requestHost)
	return port == reqPort
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

// ---- 宿主层可执行程序端点（RFC-0002 双层管理的宿主面；apigen.ServerInterface）----

// ListAgents 当前在席座位 + 已发现未启用项（apigen.ServerInterface）——
// 建房选择页候选集与"还有谁未启用"的如实展示（v1.24 dogfood #1）。
// 引擎未就绪（宿主扫描期）返回空列表而非阻塞。
func (s *server) ListAgents(w http.ResponseWriter, _ *http.Request) {
	agents := []map[string]any{}
	if s.deps.Seats != nil {
		for _, seat := range s.deps.Seats() {
			agents = append(agents, map[string]any{
				"participant_id": seat.ParticipantID,
				"adapter":        seat.Profile.Adapter,
				"display_name":   seat.Profile.DisplayName,
			})
		}
	}
	disabled := []map[string]any{}
	if s.deps.Harness != nil {
		for _, exe := range s.deps.Harness.List() {
			if exe.Enabled {
				continue
			}
			channel := exe.Channel
			if channel == "" {
				channel = harness.ChannelCLI // 注册表口径：空值按 cli 处理（与座位面一致）
			}
			disabled = append(disabled, map[string]any{
				"adapter": exe.Adapter,
				"channel": channel,
				"version": exe.Version,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents, "disabled": disabled})
}

func (s *server) ListHarnessExecutables(w http.ResponseWriter, _ *http.Request) {
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
	Channel string `json:"channel"` // 可选渠道覆盖（ADR-0012）：cli 或 app:<小写>
}

func (s *server) AddHarnessExecutable(w http.ResponseWriter, r *http.Request) {
	if s.deps.Harness == nil || s.deps.ProbeRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表/探测面未配置")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 二轮审校 #20：登记体 1MiB 上限
	var req manualExecutableRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) { // 复审 #21：超限是 413，不是 400
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "登记体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "登记体不合法："+err.Error())
		return
	}
	if err := s.deps.Harness.AddManual(r.Context(), s.deps.ProbeRunner, harness.Executable{
		Adapter: req.Adapter, Runtime: req.Runtime, Distro: req.Distro,
		Path: req.Path, Version: req.Version, Channel: req.Channel,
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

func (s *server) EnableHarnessExecutable(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	s.setEnabled(w, r, id, true)
}

func (s *server) DisableHarnessExecutable(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	s.setEnabled(w, r, id, false)
}

func (s *server) setEnabled(w http.ResponseWriter, r *http.Request, id string, enabled bool) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return
	}
	if !s.guardWrite(w, r, false) { // 空 body：只做跨源门，不校验 Content-Type
		return
	}
	if err := s.deps.Harness.SetEnabled(id, enabled); err != nil {
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

// GetRoomSnapshot：快照四元组（room_version + opaque watermark + 投影/算法版本 + Timeline）。
// participants 在装配层注入（本地 owner + 引擎座位；非投影产物——ADR-0011 注记）。
func (s *server) GetRoomSnapshot(w http.ResponseWriter, r *http.Request, roomID apigen.RoomID) {
	events, err := s.readAllEvents(r, roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "room_not_found", "房间不存在或尚无事件")
		return
	}
	snap := room.ProjectSnapshot(roomID, events)
	snap.Participants = s.snapshotParticipants()
	writeJSON(w, http.StatusOK, snap)
}

// snapshotParticipants 快照参与者装配（UI 重设计切片 1）：本地 owner（human）+ 引擎
// 当前座位（agent，含 harness 渠道标签）。display_name 取引擎已知名字（Owner/Echo/Codex/
// Kimi），缺省回退 participant_id。本切片 seat_status 恒 seated。
func (s *server) snapshotParticipants() []room.ParticipantView {
	parts := []room.ParticipantView{{
		ParticipantID: s.deps.Actor.ParticipantID,
		Kind:          s.deps.Actor.Kind,
		DisplayName:   "Owner",
		SeatStatus:    "seated",
	}}
	if s.deps.Seats != nil {
		for _, seat := range s.deps.Seats() {
			name := seat.Profile.DisplayName
			if name == "" {
				name = seat.ParticipantID
			}
			parts = append(parts, room.ParticipantView{
				ParticipantID: seat.ParticipantID,
				Kind:          "agent",
				DisplayName:   name,
				Adapter:       seat.Profile.Adapter,
				Channel:       seat.Profile.Channel,
				SeatStatus:    "seated",
			})
		}
	}
	return parts
}

// ListRooms 房间列表（apigen.ServerInterface；UI 重设计切片 1）。只读 GET——
// 与既有读端点一致：不设 owner token 门（形态约束见 spec 顶部说明）。
func (s *server) ListRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.deps.SVC.ListRooms(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

// handleUI：GET 兜底——SPA 静态产物 + 前端路由回退（M2 真实界面，v1.7 制度化）。
// /v1/* 未匹配路径显式 404：API 命名空间不被 SPA 回退吞掉（非 dev 的 debug
// 端点"未注册即 404 不暴露面"语义由此保持）。UI 未装配（测试/兜底）时回退
// M1 内嵌最小 webui。开发者模式注入沿用 M1 机制：MOSAIC_DEV=false → true。
func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, http.StatusNotFound, "not_found", "未知 API 路径")
		return
	}
	if s.deps.UI == nil {
		html := indexHTML
		if s.deps.Dev {
			html = strings.Replace(html, "const MOSAIC_DEV = false;", "const MOSAIC_DEV = true;", 1)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
		return
	}
	upath := strings.TrimPrefix(r.URL.Path, "/")
	if upath != "" {
		if f, err := s.deps.UI.Open(upath); err == nil {
			_ = f.Close()
			// /assets/ 下是 Vite 内容哈希资产：长缓存 + immutable；其余（如 favicon）保守 no-cache。
			if strings.HasPrefix(upath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.FileServerFS(s.deps.UI).ServeHTTP(w, r)
			return
		}
		// 前端路由回退：非资产路径一律 index.html
	}
	s.serveSpaIndex(w)
}

// serveSpaIndex 输出（按 dev 注入后的）SPA index.html。每次读取——本地单用户
// 面板频次极低，不值得为它建缓存。
func (s *server) serveSpaIndex(w http.ResponseWriter) {
	raw, err := fs.ReadFile(s.deps.UI, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ui_missing", "SPA index 缺失")
		return
	}
	if s.deps.Dev {
		raw = []byte(strings.Replace(string(raw), "const MOSAIC_DEV = false;", "const MOSAIC_DEV = true;", 1))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-cache：客户端每次校验——WebView2 持久缓存会跨进程存活（实证：重建产物后壳内
	// 仍显旧 UI），哈希资产走 immutable，入口页必须总是拿到最新。
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
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

// WaveSkipConsumer 把引擎波跳过通知桥到 SSE（wave.skipped 瞬态帧：同 draft.update
// 的瞬态语义——门控跳过不落事件日志，开发者模式据此在房间内解释静默原因）。
func WaveSkipConsumer(hub *sse.Hub) room.WaveSkipSink {
	return func(roomID, reason string) {
		data, err := json.Marshal(map[string]any{"room_id": roomID, "reason": reason})
		if err != nil {
			return
		}
		hub.Publish(roomID, sse.ViewEvent{Cursor: "", Type: "wave.skipped", Data: data})
	}
}
