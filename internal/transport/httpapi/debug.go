// 开发者模式调试面（M1 v1.8）：只读内部状态端点 + 事件流检视 + trace id。
// 端点仅在 -dev 下注册（非 dev 返回 404——不存在的端点比 403 更不暴露面）。
// 与对外契约的边界：调试面返回权威信封（含 seq/tenant/causation 等内部字段），
// 属内部语义，不受 RFC-0001 对外视图约束；仅供本机 owner 排障。
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
)

// newTraceID 命令链路追踪 ID（trc_ 前缀 + 随机后缀；不要求时间有序）。
func newTraceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "trc_" + hex.EncodeToString(b[:])
}

type debugSeat struct {
	ParticipantID string `json:"participant_id"`
	ProfileID     string `json:"profile_id"`
	Adapter       string `json:"adapter"`
}

type debugOutboxEntry struct {
	ID        int64  `json:"id"`
	RoomID    string `json:"room_id"`
	EventID   string `json:"event_id"`
	GlobalPos int64  `json:"global_pos"`
}

// handleDebugState 房间内部运行态：版本/epoch/暂停 + 预算水位（账本重建）+
// 引擎座位快照 + outbox 积压。全部从事件与内存快照只读重建，无副作用。
func (s *server) handleDebugState(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room_id")
	events, err := s.readAllEvents(r, roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_read_failed", err.Error())
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "room_not_found", "房间不存在或尚无事件")
		return
	}
	insp := room.InspectState(events)

	envs := make([]protocol.Envelope, len(events))
	for i := range events {
		envs[i] = events[i].Envelope
	}
	ledger := contextx.RebuildBudget(envs)
	limits := s.deps.Budget
	remaining := int64(-1) // 不限
	if limits.MaxTokens > 0 {
		remaining = limits.MaxTokens - ledger.Tokens
		if remaining < 0 {
			remaining = 0
		}
	}

	seats := []debugSeat{}
	if s.deps.Seats != nil {
		for _, seat := range s.deps.Seats() {
			seats = append(seats, debugSeat{
				ParticipantID: seat.ParticipantID,
				ProfileID:     seat.Profile.ProfileID,
				Adapter:       seat.Profile.Adapter,
			})
		}
	}

	outboxDoc := map[string]any{"backlog": 0, "pending": []debugOutboxEntry{}}
	if s.deps.Outbox != nil {
		pending, err := s.deps.Outbox.Pending(r.Context(), 100)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "debug_outbox_failed", err.Error())
			return
		}
		entries := make([]debugOutboxEntry, 0, len(pending))
		for _, e := range pending {
			entries = append(entries, debugOutboxEntry{
				ID: e.ID, RoomID: e.RoomID, EventID: e.EventID, GlobalPos: e.GlobalPos,
			})
		}
		outboxDoc = map[string]any{"backlog": len(entries), "pending": entries}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":      roomID,
		"room_version": insp.Version,
		"epoch":        insp.Epoch,
		"paused":       insp.Paused,
		"budget": map[string]any{
			"rounds":           ledger.Rounds,
			"utterances":       ledger.Utterances,
			"tokens":           ledger.Tokens,
			"level":            ledger.Level(limits),
			"remaining_tokens": remaining,
			"limits": map[string]any{
				"max_rounds":     limits.MaxRounds,
				"max_utterances": limits.MaxUtterances,
				"max_tokens":     limits.MaxTokens,
			},
		},
		"seats":  seats,
		"outbox": outboxDoc,
	})
}

// handleDebugEvents 事件流检视：权威信封（内部字段全量可见，payload 展开），
// 按房间过滤 + 可选 type 精确过滤 + 游标分页（与订阅续传同一 cursor 语义）。
func (s *server) handleDebugEvents(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room_id")
	cursor := r.URL.Query().Get("cursor")
	if _, err := protocol.DecodeCursor(cursor); err != nil {
		writeError(w, http.StatusBadRequest, "bad_cursor", err.Error())
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad_limit", "limit 须为正整数")
			return
		}
		limit = n
	}
	typeFilter := r.URL.Query().Get("type")

	events, next, err := s.deps.Reader.EventsAfter(r.Context(), roomID, cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_read_failed", err.Error())
		return
	}
	items := []map[string]any{}
	for _, ev := range events {
		if typeFilter != "" && ev.Envelope.Type != typeFilter {
			continue // 页内过滤：next 语义不变（个人版房间规模，调试面不做跨页聚合）
		}
		items = append(items, map[string]any{"cursor": ev.Cursor, "envelope": ev.Envelope})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": items, "next": next})
}

// handleDebugWaves 波链路检视（M3-1 开发者模式持久化）：自事件流重建反应波全貌
// （意图全记录/发授终态/收波结局），重启后历史波完整可复盘——[dev] 内联时间线为
// 瞬态 SSE 内存态，本端点是持久化的事实源视图。分页：按开波 seq 降序取最新 N 波；
// cursor = 下一页最老开波 seq（exclusive），空 = 从最新开始。
func (s *server) handleDebugWaves(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room_id")
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "bad_limit", "limit 须为 1..100")
			return
		}
		limit = n
	}
	before := int64(0)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad_cursor", "cursor 须为正整数 seq")
			return
		}
		before = n
	}

	events, err := s.readAllEvents(r, roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_read_failed", err.Error())
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "room_not_found", "房间不存在或尚无事件")
		return
	}
	chain := room.WaveChainOf(events)

	// 降序取页：chain 按开波 seq 升序，cursor = exclusive 上界（该 seq 的波不含本页）；
	// 页内保持时间正序（复盘阅读序），页间 newest-first。
	hi := len(chain)
	if before > 0 {
		hi = sort.Search(len(chain), func(i int) bool { return chain[i].OpenedSeq >= before })
	}
	lo := hi - limit
	if lo < 0 {
		lo = 0
	}
	page := chain[lo:hi]
	var next string
	if lo > 0 {
		next = strconv.FormatInt(chain[lo].OpenedSeq, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"waves": page, "next": next})
}

// handleDebugMemory 记忆查询面（M3-3）：胶囊记忆 + 证据需求单 + 漂移签名 +
// 逐座重复风险——记忆侧可观测（查看面；编辑不适用：胶囊不可变，登记 RFC-0007）。
func (s *server) handleDebugMemory(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room_id")
	events, err := s.readAllEvents(r, roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_read_failed", err.Error())
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
	seats := map[string]float64{}
	if s.deps.Seats != nil {
		for _, seat := range s.deps.Seats() {
			seats[seat.ParticipantID] = room.RepetitionRiskOf(envs, seat.ParticipantID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":           roomID,
		"capsules":          room.AcceptedCapsulesOf(events),
		"evidence_requests": room.EvidenceRequestsOf(events),
		"drift_signature":   room.DriftSignature(envs, 20),
		"repetition_risk":   seats,
	})
}

// handleDebugExport 导出（M3-6，RFC-0010 个人版）：NDJSON 事件流 + 首行 manifest——
// 干净环境按序重放即可重建全部投影（幂等事件流即权威账本）。
func (s *server) handleDebugExport(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room_id")
	events, err := s.readAllEvents(r, roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_read_failed", err.Error())
		return
	}
	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "room_not_found", "房间不存在或尚无事件")
		return
	}
	manifest := map[string]any{
		"kind": "mosaic.room.export", "version": 1, "room_id": roomID,
		"event_count": len(events),
		"watermark":   events[len(events)-1].Envelope.Seq,
		"exported_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	first, _ := json.Marshal(manifest)
	w.Write(append(first, '\n'))
	for _, ev := range events {
		line, _ := json.Marshal(ev.Envelope)
		w.Write(append(line, '\n'))
	}
}

// readAllEvents 分页拉取房间全量事件（快照端点与调试面共用）。
func (s *server) readAllEvents(r *http.Request, roomID string) ([]room.StoredEvent, error) {
	var events []room.StoredEvent
	cursor := ""
	for {
		batch, next, err := s.deps.Reader.EventsAfter(r.Context(), roomID, cursor, 1000)
		if err != nil {
			return nil, err
		}
		events = append(events, batch...)
		if next == "" || len(batch) == 0 {
			return events, nil
		}
		cursor = next
	}
}

// debugf 调试级日志（Logger 为 nil 的测试装配下静默）。
func (s *server) debugf(msg string, args ...any) {
	if s.deps.Logger != nil {
		s.deps.Logger.Debug(msg, args...)
	}
}

// ensureTraceID 命令链路的 trace id：客户端 X-Trace-Id 优先，缺省生成；
// 响应回带同名头（排查时从客户端即可拿到链路口子）。不依赖 dev 开关——
// 响应头与 debug 日志在常规模式下同样有效（日志受级别门控）。
func ensureTraceID(w http.ResponseWriter, r *http.Request) string {
	traceID := r.Header.Get("X-Trace-Id")
	if traceID == "" {
		traceID = newTraceID()
	}
	w.Header().Set("X-Trace-Id", traceID)
	return traceID
}
