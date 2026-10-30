// Package httpapi：HTTP 传输边界（ADR-0001：幂等命令上行 + SSE 游标订阅下行）。
// 对外只出现 EventView（无 seq/tenant，RFC-0001 P0）；错误以稳定 code 映射状态码。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/attach"
	"github.com/sisibeloved/Mosaic/internal/backup"
	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/doc"
	"github.com/sisibeloved/Mosaic/internal/extdocs"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/monitor"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/settings"
	"github.com/sisibeloved/Mosaic/internal/slot"
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
	// Identity 独立 Agent 身份域（RFC-0018 / ADR-0017，0.5.0；nil 时身份端点 503）。
	Identity *personalagent.Service
	// Slots 职责模板域（RFC-0018 §5 / ADR-0019，0.5.2；nil 时职责端点 503）。
	Slots  *slot.Service
	Logger *slog.Logger
	// 开发者模式（M1 v1.8）：Dev 开启才注册 /v1/debug 只读端点与 UI 调试面板。
	Dev    bool
	Budget contextx.Limits         // 预算上限（状态端点的水位/梯度计算基准）
	Seats  func() []room.AgentSeat // 引擎座位快照（引擎未就绪时返回空）
	Outbox outbox.Store            // outbox 积压检视（nil 时状态端点该节为 0）
	// Authority 配置的服务监听地址（host:port；四轮复审 #15）：跨源写门据此判定
	// Origin——不信请求自带的 Host（DNS rebinding 后 Origin 与 Host 会同时指向
	// 攻击者域名而相等）。空 = 测试装配退回"请求 Host + 回环 host"判定（host 仍须回环）。
	Authority string // ExtraOriginHosts 壳集成信任源（M2 桌面壳）：精确主机名匹配（如 wails.localhost）。
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
	// Searcher 按需检索端口（M3-3）：nil = 回退线性基准（读全量事件 +
	// room.SearchMessages 纯函数——语义一致）；SQLite 装配注入 FTS5 实现。
	Searcher room.MessageSearcher
	// ReceiptLister 回执流水（v1.70 展示对齐）：nil = 回执端点 404（测试装配）。
	ReceiptLister room.ReceiptLister
	// Backups 备份面（M4-0）：nil = 备份/恢复端点 404（测试装配）。
	Backups *backup.Manager
	// Attachments 附件面（RFC-0013，M4-0）：nil = 上传/下载端点 404。
	Attachments *attach.Store
	// Settings 设置族（OQ-B 首员，M4-1 切片 B）：nil = 设置端点 404（测试装配）。
	Settings *settings.Store
	// Monitors 监控面（M4-5）：nil = 监控端点 404（测试装配）。
	Monitors *monitor.Manager
	// DocSVC 文档域服务（RFC-0014 / ADR-0014）：nil = 文档端点 404（测试装配）。
	DocSVC *doc.Service
	// DocReader 文档事件读路径（doc:{id} 频道 SSE 追平）：nil = doc SSE 404。
	DocReader doc.DocEventReader
	// ExtDocs 外部云文档集成面（RFC-0016 多 provider：腾讯/飞书）：nil = extdocs
	// 端点 404（测试装配）。
	ExtDocs *extdocs.Service
	// SheetTransfers 表格手动交换服务（RFC-0017 M2）：nil = 交换端点 404。
	// 与 ExtDocs 双依赖（provider 注册表来自后者）。
	SheetTransfers *extdocs.SheetTransferService
	// Diagnostics 自诊断 bundle 构造器（M4-0）：nil = 诊断端点 404。
	// 内容纪律：不含凭据/环境变量（OQ-20）——版本/运行时/数据面统计/注册表状态/日志尾。
	Diagnostics func() (map[string]any, error)
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
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/claims", s.handleDebugClaims)
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/memory", s.handleDebugMemory)
		mux.HandleFunc("GET /v1/debug/rooms/{room_id}/export", s.handleDebugExport)
		mux.HandleFunc("GET /v1/debug/docs/{doc_id}/export", s.handleDebugDocExport)
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

// ListIdentityAgents 独立 Agent 身份列表（0.5.0 / ADR-0017；apigen.ServerInterface）。
// 只读观察面：读取前经服务内幂等同步（注册表 → 身份表）最终一致。
func (s *server) ListIdentityAgents(w http.ResponseWriter, r *http.Request) {
	if s.deps.Identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "身份注册表未配置")
		return
	}
	agents, err := s.deps.Identity.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "identity_read_failed", err.Error())
		return
	}
	if agents == nil {
		agents = []personalagent.AgentView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// GetIdentityAgent 单个 Agent 身份详情（0.5.0 / ADR-0017）。
func (s *server) GetIdentityAgent(w http.ResponseWriter, r *http.Request, agentID string) {
	if s.deps.Identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "身份注册表未配置")
		return
	}
	view, ok, err := s.deps.Identity.Get(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "identity_read_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "identity_not_found", "Agent 身份不存在")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// CreateIdentityAgent 手动建身份（0.5.1 / ADR-0018；apigen.ServerInterface）。
// source=manual、active 出生；为 0.5.2+ Slot 任职 / Worker 晋升预留主体。
func (s *server) CreateIdentityAgent(w http.ResponseWriter, r *http.Request) {
	if s.deps.Identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "身份注册表未配置")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.ManualAgentRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	adapter := ""
	if req.Adapter != nil {
		adapter = *req.Adapter
	}
	view, err := s.deps.Identity.CreateManual(r.Context(), req.DisplayName, adapter)
	if err != nil {
		if errors.Is(err, personalagent.ErrInvalidAgent) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// TransitionAgentLifecycle 生命周期转移（0.5.1 / ADR-0018）：
// suspend / resume / retire；非法转移 409（不静默幂等），未知身份 404。
func (s *server) TransitionAgentLifecycle(w http.ResponseWriter, r *http.Request, agentID string) {
	if s.deps.Identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "身份注册表未配置")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.LifecycleTransitionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	ctx := r.Context()
	var res personalagent.LifecycleResult
	var err error
	switch req.Action {
	case apigen.Suspend:
		res, err = s.deps.Identity.Suspend(ctx, agentID, reason)
	case apigen.Resume:
		res, err = s.deps.Identity.Resume(ctx, agentID, reason)
	case apigen.Retire:
		res, err = s.deps.Identity.Retire(ctx, agentID, reason)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "action 须为 suspend | resume | retire")
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, personalagent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "identity_not_found", "Agent 身份不存在")
		case errors.Is(err, personalagent.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "invalid_transition", err.Error())
		default:
			s.writeDomainError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ListIdentityAgentEvents 事件流只读审计面（0.5.1 / ADR-0018）。
func (s *server) ListIdentityAgentEvents(w http.ResponseWriter, r *http.Request, agentID string) {
	if s.deps.Identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "身份注册表未配置")
		return
	}
	evs, ok, err := s.deps.Identity.Events(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "identity_read_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "identity_not_found", "Agent 身份不存在")
		return
	}
	out := make([]apigen.AgentEvent, 0, len(evs))
	for _, ev := range evs {
		view, err := agentEventView(ev)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "identity_read_failed", err.Error())
			return
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// agentEventView personalagent.AgentEvent → apigen.AgentEvent（payload 展开为
// 对象、occurred_at 解析为时间戳——schema 形态）。
func agentEventView(ev personalagent.AgentEvent) (apigen.AgentEvent, error) {
	ts, err := time.Parse(time.RFC3339, ev.OccurredAt)
	if err != nil {
		return apigen.AgentEvent{}, fmt.Errorf("事件时间 %q: %w", ev.OccurredAt, err)
	}
	var payload map[string]interface{}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return apigen.AgentEvent{}, fmt.Errorf("事件 payload: %w", err)
		}
	}
	return apigen.AgentEvent{
		Seq:        ev.Seq,
		AgentId:    ev.AgentID,
		EventType:  apigen.AgentEventEventType(ev.EventType),
		OccurredAt: ts,
		Payload:    payload,
	}, nil
}

func (s *server) ListHarnessExecutables(w http.ResponseWriter, r *http.Request) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return
	}
	// 读前身份同步（审校 P2#2）：设置页登记/启停后只刷新本列表，不先调身份
	// API——同步必须发生在该用户流程内，agent_id 徽标才会即时出现。幂等，失败
	// 降级（身份读面另有惰性兜底）。
	if s.deps.Identity != nil {
		_, _ = s.deps.Identity.SyncFromHarness(r.Context())
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
	// Bundle 可选内嵌 bundle 路径覆盖（zcode 桌面形态非约定布局时的逃生口；
	// 空值按 exe 同级约定布局推断——AddManual 侧）。
	Bundle string `json:"bundle"`
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
		Path: req.Path, Version: req.Version, Channel: req.Channel, Bundle: req.Bundle,
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

// runtimeUpdateRequest 运行参数更新体（v1.48：模型覆盖与思考强度；全量替换
// 语义，空串 = 清除覆盖回 CLI 自身配置默认）。
type runtimeUpdateRequest struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

// UpdateHarnessExecutable PUT /v1/harness/executables/{id}：更新运行参数。
// 座位在下次 resync（≤10s）拿到新参数——设置页保存后无须重启。
func (s *server) UpdateHarnessExecutable(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req runtimeUpdateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	if err := s.deps.Harness.UpdateRuntime(id, req.Model, req.ReasoningEffort); err != nil {
		if errors.Is(err, harness.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if errors.Is(err, harness.ErrInvalidEntry) {
			writeError(w, http.StatusBadRequest, "invalid_entry", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "model": req.Model, "reasoning_effort": req.ReasoningEffort})
}

// GetHarnessExecutableModels GET /v1/harness/executables/{id}/models：模型候选
// 与思考强度档位（kimi 经 CLI 实查 provider list --json——唯一支持动态列表的
// 一家；codex 五档强度+空候选自由输入；mcode 空面。实证 2026-09-03）。
func (s *server) GetHarnessExecutableModels(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return
	}
	opts, err := s.deps.Harness.RuntimeOptionsOf(r.Context(), id, s.deps.ProbeRunner)
	if err != nil {
		if errors.Is(err, harness.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// GetHarnessExecutableUpdateCheck GET /v1/harness/executables/{id}/update-check：
// Agent CLI 版本检测（0.4.1）——注册表探测版本对照上游最新版（npm view 经登录
// shell 实查，缓存 30 分钟）。检测失败返回 200 + note（环境态不 5xx）。
func (s *server) GetHarnessExecutableUpdateCheck(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return
	}
	check, err := s.deps.Harness.CheckUpdate(r.Context(), id, s.deps.ProbeRunner)
	if err != nil {
		if errors.Is(err, harness.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, check)
}

// RunHarnessExecutableUpdate POST /v1/harness/executables/{id}/update：启动
// 更新任务（0.4.1 过程可视化）：异步执行，202 受理即返（started=本次启动 /
// running=同实例任务在途幂等），过程日志经 GET .../update/stream SSE 逐行转发。
// kimi/zcode 无自动通道 → 409 no_update_channel（note 携指引）。
func (s *server) RunHarnessExecutableUpdate(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
		return
	}
	if !s.guardWrite(w, r, false) { // 空 body：只做跨源门（enable/disable 同口径）
		return
	}
	if s.deps.ProbeRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "无探测执行面（ProbeRunner 未配置）")
		return
	}
	started, err := s.deps.Harness.StartUpdate(r.Context(), id, s.deps.ProbeRunner)
	if err != nil {
		switch {
		case errors.Is(err, harness.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, harness.ErrNoUpdateChannel):
			writeError(w, http.StatusConflict, "no_update_channel", err.Error())
		default:
			s.writeDomainError(w, err)
		}
		return
	}
	status := "running"
	if started {
		status = "started"
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": status})
}

// GetHarnessExecutableUpdateStream GET /v1/harness/executables/{id}/update/stream：
// 更新任务过程日志 SSE（0.4.1 可视化）——连接即重放已积累行，随后实时逐行转发
// （子进程 stdout/stderr 管道转发），终态（done/failed）后关流。只读观察面：
// 无写门（与房间 SSE 读面同口径；启动更新仍走 owner 门控的 POST）。无任务时
// 发 idle 事件即关。15s 注释心跳防中间层缓冲。
func (s *server) GetHarnessExecutableUpdateStream(w http.ResponseWriter, r *http.Request, id apigen.ExecutableID) {
	if s.deps.Harness == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "宿主注册表未配置")
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

	send := func(ev harness.UpdateEvent) {
		fmt.Fprintf(w, "data: %s\n\n", mustMarshalJSON(ev))
		flusher.Flush()
	}
	events, cancel, ok := s.deps.Harness.SubscribeUpdateEvents(id)
	if !ok {
		send(harness.UpdateEvent{Kind: "idle"})
		return
	}
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return
			}
			send(ev)
			if ev.Kind == "done" || ev.Kind == "failed" {
				return
			}
		}
	}
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

// GetRoomMemory 记忆查看面（M3-3，apigen.ServerInterface）：编辑后胶囊 +
// edit_history + 容量水位——与语境注入同源投影（纠错生效于下次组装可证）。
// v1.70 扩展：策展条目（agent 每波评审自助沉淀）+ 合并水位。
func (s *server) GetRoomMemory(w http.ResponseWriter, r *http.Request, roomID apigen.RoomID) {
	events, err := s.readAllEvents(r, roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "memory_read_failed", err.Error())
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "room_not_found", "房间不存在或尚无事件")
		return
	}
	envs := make([]protocol.Envelope, len(events))
	for i := range events {
		envs[i] = events[i].Envelope
	}
	curated := room.CuratedEntriesOf(events)
	if curated == nil {
		curated = []room.CuratedEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":        roomID,
		"capsules":       room.MemoryCapsulesOf(events),
		"capsule_budget": room.CapsuleBudgetOf(envs),
		"curated":        curated,
		"curated_budget": room.ConstantPlaneOf(envs).Stat,
	})
}

// GetRoomContext 语境平面全景（v1.70 展示对齐，apigen.ServerInterface）：
// 与引擎组装同源的纯函数快照——"模型看得到的，开发者也能看得到"。
func (s *server) GetRoomContext(w http.ResponseWriter, r *http.Request, roomID apigen.RoomID) {
	events, err := s.readAllEvents(r, roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "context_read_failed", err.Error())
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "room_not_found", "房间不存在或尚无事件")
		return
	}
	writeJSON(w, http.StatusOK, room.ContextPanoramaOf(roomID, events))
}

// GetRoomReceipts 回执流水（v1.70 展示对齐，apigen.ServerInterface）：AR-16
// 正式面——每次运行"实际交付了什么"可查；nil Lister（测试装配）404 不暴露面。
func (s *server) GetRoomReceipts(w http.ResponseWriter, r *http.Request, roomID apigen.RoomID, params apigen.GetRoomReceiptsParams) {
	if s.deps.ReceiptLister == nil {
		writeError(w, http.StatusNotFound, "receipts_unavailable", "回执查询面未装配")
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
	receipts, err := s.deps.ReceiptLister.ReceiptsOf(r.Context(), roomID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "receipts_read_failed", err.Error())
		return
	}
	if receipts == nil {
		receipts = []contextx.Receipt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"room_id": roomID, "receipts": receipts})
}

// SearchRoomMessages 房内全文检索（M3-3 按需平面，apigen.ServerInterface）：
// Searcher（SQLite FTS5 trigram）优先，nil 回退线性基准（语义一致）。
func (s *server) SearchRoomMessages(w http.ResponseWriter, r *http.Request, roomID apigen.RoomID, params apigen.SearchRoomMessagesParams) {
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
	actor := ""
	if params.Actor != nil {
		actor = *params.Actor
	}
	var hits []room.SearchHit
	if s.deps.Searcher != nil {
		var err error
		hits, err = s.deps.Searcher.SearchMessages(r.Context(), roomID, q, actor, "", limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search_failed", err.Error())
			return
		}
	} else {
		events, err := s.readAllEvents(r, roomID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search_failed", err.Error())
			return
		}
		hits = room.SearchMessages(events, q, actor, "", limit)
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
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

// SeatStatusConsumer 把座位级状态通知桥到 SSE（seat.status 瞬态帧，M4-2 协作
// 状态）：eval_failed/generate_failed 携适配器错误串——配额耗尽/网络不可达/
// 超时的真实原因进房间可见面（v1.50/58 实证此类原因此前只进日志）。瞬态：
// 断线不补发；权威重建面是 intent.recorded/message.posted。
func SeatStatusConsumer(hub *sse.Hub) room.SeatStatusSink {
	return func(roomID, participantID, status, detail string) {
		data, err := json.Marshal(map[string]any{
			"room_id": roomID, "participant_id": participantID,
			"status": status, "detail": detail,
		})
		if err != nil {
			return
		}
		hub.Publish(roomID, sse.ViewEvent{Cursor: "", Type: "seat.status", Data: data})
	}
}

// ---- 职责模板面（RFC-0018 §5 / ADR-0019，0.5.2；apigen.ServerInterface）----

// ListSlotSpecs 谱系列表（只读）。
func (s *server) ListSlotSpecs(w http.ResponseWriter, r *http.Request) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	sums, err := s.deps.Slots.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if sums == nil {
		sums = []slot.SpecSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"specs": sums})
}

// CreateSlotSpec 建谱系（首版 version=1）；谱系已存在 409 spec_exists。
func (s *server) CreateSlotSpec(w http.ResponseWriter, r *http.Request) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.SlotSpecCreate
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	spec, err := s.deps.Slots.Create(r.Context(), req.SlotSpecId, specContentOf(req.Name, req.Responsibility, req.Deliverables, req.Skills, req.Tools, req.Context, req.Delegation, req.Runtime, req.Handoff))
	if err != nil {
		writeSlotError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, spec)
}

// GetSlotSpecLatest 最新版本全量内容。
func (s *server) GetSlotSpecLatest(w http.ResponseWriter, r *http.Request, slotSpecID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	spec, ok, err := s.deps.Slots.Latest(r.Context(), slotSpecID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "spec_not_found", "职责模板谱系不存在")
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

// ListSlotSpecVersions 全版本升序（含内容）。
func (s *server) ListSlotSpecVersions(w http.ResponseWriter, r *http.Request, slotSpecID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	versions, ok, err := s.deps.Slots.Versions(r.Context(), slotSpecID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "spec_not_found", "职责模板谱系不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// AppendSlotSpecVersion 追加下一不可变版本（version=latest+1）。
func (s *server) AppendSlotSpecVersion(w http.ResponseWriter, r *http.Request, slotSpecID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.SlotSpecContent
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	spec, err := s.deps.Slots.AppendVersion(r.Context(), slotSpecID,
		specContentOf(req.Name, req.Responsibility, req.Deliverables, req.Skills, req.Tools, req.Context, req.Delegation, req.Runtime, req.Handoff))
	if err != nil {
		writeSlotError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, spec)
}

// writeSlotError 职责域错误映射。
func writeSlotError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, slot.ErrInvalidSpec):
		writeError(w, http.StatusBadRequest, "invalid_spec", err.Error())
	case errors.Is(err, slot.ErrSpecExists):
		writeError(w, http.StatusConflict, "spec_exists", err.Error())
	case errors.Is(err, slot.ErrNotFound):
		writeError(w, http.StatusNotFound, "spec_not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "slot_write_failed", err.Error())
	}
}

// specContentOf apigen 请求体 → 域内容（字段平铺的两请求体共用）。
func specContentOf(name, responsibility string, deliverables []string,
	skills *[]apigen.SkillRef, tools, ctxList *apigen.CapabilityList,
	delegation *apigen.DelegationPolicy, runtime *apigen.RuntimeRequirements,
	handoff *apigen.HandoffRequirements) slot.SpecContent {
	c := slot.SpecContent{
		Name: name, Responsibility: responsibility, Deliverables: deliverables,
	}
	if skills != nil {
		for _, sk := range *skills {
			c.Skills = append(c.Skills, slot.SkillRef{Ref: sk.Ref, Version: sk.Version})
		}
	}
	c.Tools = capsOf(tools)
	c.Context = capsOf(ctxList)
	if delegation != nil {
		d := &slot.DelegationPolicy{MaxDepth: delegation.MaxDepth, Allowed: []string{}}
		for _, a := range delegation.Allowed {
			d.Allowed = append(d.Allowed, string(a))
		}
		c.Delegation = d
	}
	if runtime != nil {
		rt := &slot.RuntimeRequirements{WorkspaceAccess: string(runtime.WorkspaceAccess), Required: []string{}}
		for _, n := range runtime.Required {
			rt.Required = append(rt.Required, n)
		}
		c.Runtime = rt
	}
	if handoff != nil {
		h := &slot.HandoffRequirements{Required: []string{}}
		for _, n := range handoff.Required {
			h.Required = append(h.Required, n)
		}
		c.Handoff = h
	}
	return c
}

// capsOf 能力分层转换（nil 透传——缺省段不落库）。
func capsOf(cl *apigen.CapabilityList) *slot.CapabilityList {
	if cl == nil {
		return nil
	}
	out := &slot.CapabilityList{Required: []string{}, Optional: []string{}}
	out.Required = append(out.Required, cl.Required...)
	out.Optional = append(out.Optional, cl.Optional...)
	return out
}

// ---- 职责实例面（RFC-0018 §3.1/§5.2 / ADR-0020，0.5.3）----

// ListSlotInstances 实例列表（创建序，模板现势连接）。
func (s *server) ListSlotInstances(w http.ResponseWriter, r *http.Request) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	instances, err := s.deps.Slots.ListInstances(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if instances == nil {
		instances = []slot.SlotInstanceView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": instances})
}

// CreateSlotInstance 实例化职责（si_ ID + slotns_ 命名空间）；谱系未知 404。
func (s *server) CreateSlotInstance(w http.ResponseWriter, r *http.Request) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.SlotInstanceCreate
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	// 标题三态（审校 P2#3）：省略（nil）→ 缺省取模板名；显式空串/纯空白 →
	// 400（契约：minLength 1——显式提供即须有效，不与省略合流）
	title := ""
	if req.Title != nil {
		if strings.TrimSpace(*req.Title) == "" {
			writeError(w, http.StatusBadRequest, "invalid_instance", "title 显式提供时须 1–64 字（省略则取模板名）")
			return
		}
		title = *req.Title
	}
	inst, err := s.deps.Slots.CreateInstance(r.Context(), req.SlotSpecId, title)
	if err != nil {
		switch {
		case errors.Is(err, slot.ErrInvalidInstance):
			writeError(w, http.StatusBadRequest, "invalid_instance", err.Error())
		case errors.Is(err, slot.ErrNotFound):
			writeError(w, http.StatusNotFound, "spec_not_found", "职责模板谱系不存在")
		default:
			s.writeDomainError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, inst)
}

// GetSlotInstance 实例详情 / 404。
func (s *server) GetSlotInstance(w http.ResponseWriter, r *http.Request, slotInstanceID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	inst, ok, err := s.deps.Slots.GetInstance(r.Context(), slotInstanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "instance_not_found", "职责实例不存在")
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

// ---- 任职面（RFC-0018 §3.2/§5.2 / ADR-0021，0.5.4）----

// ListSlotBindings 任职列表（创建序；可按实例过滤）。
func (s *server) ListSlotBindings(w http.ResponseWriter, r *http.Request, params apigen.ListSlotBindingsParams) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	instanceID := ""
	if params.SlotInstanceId != nil {
		instanceID = *params.SlotInstanceId
	}
	bindings, err := s.deps.Slots.ListBindings(r.Context(), instanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if bindings == nil {
		bindings = []slot.Binding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

// PrepareSlotBinding 建任职（preparing → readiness 门 → ready）。
func (s *server) PrepareSlotBinding(w http.ResponseWriter, r *http.Request) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.BindingPrepare
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	// 版本三态（审校 P2#5）：省略（nil）→ 钉谱系最新；显式 <1 → 400（不静默
	// 换成最新版——可能绑定用户未指定的模板）
	version := 0
	if req.SlotSpecVersion != nil {
		if *req.SlotSpecVersion < 1 {
			writeError(w, http.StatusBadRequest, "invalid_binding", "slot_spec_version 显式提供时须 ≥1（省略则钉最新）")
			return
		}
		version = int(*req.SlotSpecVersion)
	}
	b, err := s.deps.Slots.PrepareBinding(r.Context(), req.SlotInstanceId, req.AgentId, version)
	if err != nil {
		writeBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// ActivateSlotBinding 激活任职（ready → active；单 active）。
func (s *server) ActivateSlotBinding(w http.ResponseWriter, r *http.Request, bindingID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	b, err := s.deps.Slots.ActivateBinding(r.Context(), bindingID)
	if err != nil {
		writeBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ReleaseSlotBinding 释放任职（排空门 + 交接备注）。
func (s *server) ReleaseSlotBinding(w http.ResponseWriter, r *http.Request, bindingID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	if !s.guardWrite(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req apigen.BindingRelease
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体超过 1MiB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不合法："+err.Error())
		return
	}
	note := ""
	if req.HandoffNote != nil {
		note = *req.HandoffNote
	}
	res, err := s.deps.Slots.ReleaseBinding(r.Context(), bindingID, note)
	if err != nil {
		writeBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// GetSlotBinding 任职详情 / 404。
func (s *server) GetSlotBinding(w http.ResponseWriter, r *http.Request, bindingID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	b, ok, err := s.deps.Slots.GetBinding(r.Context(), bindingID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "binding_not_found", "任职记录不存在")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ListSlotBindingEvents 任职事件流（只读审计面）。
func (s *server) ListSlotBindingEvents(w http.ResponseWriter, r *http.Request, bindingID string) {
	if s.deps.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slot_unavailable", "职责域未装配")
		return
	}
	evs, ok, err := s.deps.Slots.BindingEvents(r.Context(), bindingID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "binding_not_found", "任职记录不存在")
		return
	}
	out := make([]apigen.BindingEvent, 0, len(evs))
	for _, ev := range evs {
		view, err := bindingEventView(ev)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "slot_read_failed", err.Error())
			return
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// writeBindingError 任职域错误映射。
func writeBindingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, slot.ErrBindingNotFound):
		writeError(w, http.StatusNotFound, "binding_not_found", err.Error())
	case errors.Is(err, slot.ErrInstanceNotFound):
		writeError(w, http.StatusNotFound, "instance_not_found", err.Error())
	case errors.Is(err, slot.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "agent_not_found", err.Error())
	case errors.Is(err, slot.ErrNotFound):
		writeError(w, http.StatusNotFound, "spec_not_found", err.Error())
	case errors.Is(err, slot.ErrAgentNotActive):
		writeError(w, http.StatusConflict, "agent_not_active", err.Error())
	case errors.Is(err, slot.ErrInvalidSpecVersion):
		writeError(w, http.StatusConflict, "invalid_spec_version", err.Error())
	case errors.Is(err, slot.ErrActiveBindingExists):
		writeError(w, http.StatusConflict, "active_binding_exists", err.Error())
	case errors.Is(err, slot.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, slot.ErrInvalidBinding):
		writeError(w, http.StatusBadRequest, "invalid_binding", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "slot_write_failed", err.Error())
	}
}

// bindingEventView slot.BindingEvent → apigen.BindingEvent（审校 P2#6：主体
// 字段为 binding_id——任职事件不得借 AgentEvent 的 agent_id 承载）。
func bindingEventView(ev slot.BindingEvent) (apigen.BindingEvent, error) {
	ts, err := time.Parse(time.RFC3339, ev.OccurredAt)
	if err != nil {
		return apigen.BindingEvent{}, fmt.Errorf("事件时间 %q: %w", ev.OccurredAt, err)
	}
	var payload map[string]interface{}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return apigen.BindingEvent{}, fmt.Errorf("事件 payload: %w", err)
		}
	}
	return apigen.BindingEvent{
		Seq:        ev.Seq,
		BindingId:  ev.BindingID,
		EventType:  apigen.BindingEventEventType(ev.EventType),
		OccurredAt: ts,
		Payload:    payload,
	}, nil
}
