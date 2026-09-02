// UT 层：httpapi 开发者模式——调试端点的可见性门禁、状态检视、事件检视、trace id。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/contextx"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

// fakeOutbox 测试用 outbox 端口：固定积压条目。
type fakeOutbox struct{ entries []outbox.Entry }

func (f fakeOutbox) Pending(context.Context, int) ([]outbox.Entry, error) { return f.entries, nil }
func (f fakeOutbox) MarkDispatched(context.Context, []int64) error        { return nil }

func newDevServer(t *testing.T, dev bool) (*httptest.Server, *room.MemStore) {
	t.Helper()
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := room.NewService(room.Config{
		Store: store,
		Clock: func() string { return "2026-08-30T09:00:00.000Z" },
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return prefix + "_dbg_" + fmt.Sprintf("%08d", n)
		},
		Tenant: "ten_local",
	})
	handler := New(Deps{
		SVC:    svc,
		Reader: store,
		Hub:    sse.NewHub(),
		Actor:  room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Dev:    dev,
		Budget: contextx.Limits{MaxRounds: 10, MaxUtterances: 100, MaxTokens: 1000},
		Seats: func() []room.AgentSeat {
			return []room.AgentSeat{{ParticipantID: "par_echo"}}
		},
		Outbox: fakeOutbox{entries: []outbox.Entry{
			{ID: 7, RoomID: "room_x", EventID: "evt_a", GlobalPos: 3},
			{ID: 8, RoomID: "room_x", EventID: "evt_b", GlobalPos: 4},
		}},
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, store
}

func devPost(t *testing.T, ts *httptest.Server, path string, body any, traceID string) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if traceID != "" {
		req.Header.Set("X-Trace-Id", traceID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func createDevRoom(t *testing.T, ts *httptest.Server) (roomID string, resp *http.Response) {
	t.Helper()
	resp = devPost(t, ts, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d0001", "issued_at": "2026-08-30T09:00:00Z",
		"payload": map[string]any{"display_name": "dbg"},
	}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("create_room status=%d", resp.StatusCode)
	}
	var out struct {
		RoomID string `json:"room_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.RoomID, resp
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestDebugEndpointsHiddenWithoutDev：非开发者模式下调试端点不得存在（404 而非 403——不暴露面）。
func TestDebugEndpointsHiddenWithoutDev(t *testing.T) {
	ts, _ := newDevServer(t, false)
	roomID, _ := createDevRoom(t, ts)
	for _, path := range []string{
		"/v1/debug/rooms/" + roomID + "/state",
		"/v1/debug/rooms/" + roomID + "/events",
		"/v1/debug/rooms/" + roomID + "/waves",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404（dev 关闭时端点不注册）", path, resp.StatusCode)
		}
	}
}

// TestDebugState：状态端点汇总版本/epoch/暂停/预算水位/座位/outbox 积压。
func TestDebugState(t *testing.T) {
	ts, _ := newDevServer(t, true)
	roomID, _ := createDevRoom(t, ts)
	devPost(t, ts, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d0002", "issued_at": "2026-08-30T09:00:01Z",
		"payload": map[string]any{"body": "调试面", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	}, "")

	status, state := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/state")
	if status != 200 {
		t.Fatalf("state status=%d body=%v", status, state)
	}
	if state["room_id"] != roomID {
		t.Fatalf("room_id = %v", state["room_id"])
	}
	if state["room_version"] != float64(2) {
		t.Fatalf("room_version = %v, want 2", state["room_version"])
	}
	if state["epoch"] != float64(0) || state["paused"] != false {
		t.Fatalf("epoch/paused = %v/%v", state["epoch"], state["paused"])
	}
	seats, _ := state["seats"].([]any)
	if len(seats) != 1 || seats[0].(map[string]any)["participant_id"] != "par_echo" {
		t.Fatalf("seats = %v", seats)
	}
	budget, _ := state["budget"].(map[string]any)
	if budget["rounds"] != float64(0) || budget["level"] != float64(0) {
		t.Fatalf("budget = %v", budget)
	}
	limits, _ := budget["limits"].(map[string]any)
	if limits["max_rounds"] != float64(10) {
		t.Fatalf("budget.limits = %v", limits)
	}
	ob, _ := state["outbox"].(map[string]any)
	if ob["backlog"] != float64(2) {
		t.Fatalf("outbox = %v", ob)
	}
}

// TestDebugStateRoomNotFound：无事件房间 → 404。
func TestDebugStateRoomNotFound(t *testing.T) {
	ts, _ := newDevServer(t, true)
	status, _ := getJSON(t, ts.URL+"/v1/debug/rooms/room_nope/state")
	if status != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", status)
	}
}

// TestDebugEvents：事件检视返回权威信封（内部字段 seq/causation_id 可见），支持类型过滤与游标分页。
func TestDebugEvents(t *testing.T) {
	ts, _ := newDevServer(t, true)
	roomID, _ := createDevRoom(t, ts)
	devPost(t, ts, "/v1/rooms/"+roomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d0003", "issued_at": "2026-08-30T09:00:02Z",
		"payload": map[string]any{"body": "检视事件", "reply_to": nil, "addressed_to": []any{}, "relations": []any{}},
	}, "")

	status, doc := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/events")
	if status != 200 {
		t.Fatalf("events status=%d", status)
	}
	events, _ := doc["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	first, _ := events[0].(map[string]any)
	if first["cursor"] == "" {
		t.Fatal("事件项缺 cursor")
	}
	env, _ := first["envelope"].(map[string]any)
	if env["seq"] != float64(1) || env["type"] != "room.created" || env["tenant_id"] == "" {
		t.Fatalf("权威信封缺内部字段：%v", env)
	}

	// 类型过滤
	_, filtered := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/events?type=message.posted")
	fev, _ := filtered["events"].([]any)
	if len(fev) != 1 || fev[0].(map[string]any)["envelope"].(map[string]any)["type"] != "message.posted" {
		t.Fatalf("type 过滤结果 = %v", fev)
	}

	// 分页：limit=1 必须给出 next，且下一页能续读
	_, page1 := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/events?limit=1")
	if ev1, _ := page1["events"].([]any); len(ev1) != 1 {
		t.Fatalf("limit=1 events = %v", ev1)
	}
	next, _ := page1["next"].(string)
	if next == "" {
		t.Fatal("limit=1 时 next 不得为空")
	}
	_, page2 := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/events?limit=1&cursor="+next)
	ev2, _ := page2["events"].([]any)
	if len(ev2) != 1 || ev2[0].(map[string]any)["envelope"].(map[string]any)["seq"] != float64(2) {
		t.Fatalf("续读页 = %v", ev2)
	}

	// 非法游标 → 400（%%% 会被 URL 解析静默丢弃，用合法字符但非法编码的值）
	status, _ = getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/events?cursor=not-a-cursor")
	if status != http.StatusBadRequest {
		t.Fatalf("bad cursor status=%d, want 400", status)
	}
}

// TestDebugWaves：波链路检视（M3-1 持久化）——人类消息不开波（空链）、直接落库的
// 波事件可复盘（意图/发授/发布/结局全量投影）、limit 分页给 next 且续读更老、
// 坏 cursor 400。波事件不经引擎直接进 store（UT 装配无引擎——投影只认事件流，
// 这正是"重启后可复盘"的语义：任何写入过的事件都能重建视图）。
func TestDebugWaves(t *testing.T) {
	ts, store := newDevServer(t, true)
	roomID, _ := createDevRoom(t, ts)

	// 无波历史：只有建房 → 空链 200（与 404 房间不存在区分）
	status, doc := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/waves")
	if status != 200 {
		t.Fatalf("waves status=%d body=%v", status, doc)
	}
	if waves, _ := doc["waves"].([]any); len(waves) != 0 {
		t.Fatalf("无波历史应空链：%v", waves)
	}

	// 直接落库一波完整链路（复盘语义：只要事件在，视图就在）
	appendWave := func(roundID string, openedSeq int64) {
		envs := []room.StoredEvent{
			{Envelope: protocolEnvelope(roomID, openedSeq, protocol.EventRoundOpened, "par_system", "system",
				fmt.Sprintf(`{"round_id":%q,"stimulus_event_id":"evt_stim_%s"}`, roundID, roundID))},
			{Envelope: protocolEnvelope(roomID, openedSeq+1, protocol.EventIntentRecorded, "par_codex", "agent",
				`{"intent_id":"int_x","participant_id":"par_codex","action":"speak","type":"answer","public_rationale":"可补充","score_band":"medium","selected":true}`)},
			{Envelope: protocolEnvelope(roomID, openedSeq+2, protocol.EventFloorGranted, "par_system", "system",
				fmt.Sprintf(`{"grant_id":"g_%s","round_id":%q,"participant_id":"par_codex","rank":1}`, roundID, roundID))},
			{Envelope: protocolEnvelope(roomID, openedSeq+3, protocol.EventMessagePosted, "par_codex", "agent",
				`{"body":"复盘我"}`)},
			{Envelope: protocolEnvelope(roomID, openedSeq+4, protocol.EventRoundClosed, "par_system", "system",
				fmt.Sprintf(`{"round_id":%q,"outcome":"published","selected_count":1,"silent_count":0}`, roundID))},
		}
		if _, err := store.AppendEvents(context.Background(), envelopesOf(envs)); err != nil {
			t.Fatalf("append wave %s: %v", roundID, err)
		}
	}
	appendWave("rnd_a", 10)
	appendWave("rnd_b", 20)

	status, doc = getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/waves")
	if status != 200 {
		t.Fatalf("waves status=%d", status)
	}
	waves, _ := doc["waves"].([]any)
	if len(waves) != 2 {
		t.Fatalf("waves = %d, want 2", len(waves))
	}
	first, _ := waves[0].(map[string]any) // 页内时间正序：rnd_a 在前
	if first["round_id"] != "rnd_a" || first["outcome"] != "published" {
		t.Fatalf("波 rnd_a = %v", first)
	}
	intents, _ := first["intents"].([]any)
	if len(intents) != 1 || intents[0].(map[string]any)["participant_id"] != "par_codex" {
		t.Fatalf("rnd_a 意图 = %v", intents)
	}
	grants, _ := first["grants"].([]any)
	if len(grants) != 1 || grants[0].(map[string]any)["published"] != true {
		t.Fatalf("rnd_a 发授终态 = %v", grants)
	}

	// 分页：limit=1 → 只给最新波（rnd_b），next 指向 rnd_a 的开波 seq，续读补齐
	_, page1 := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/waves?limit=1")
	p1, _ := page1["waves"].([]any)
	if len(p1) != 1 || p1[0].(map[string]any)["round_id"] != "rnd_b" {
		t.Fatalf("limit=1 最新波 = %v", p1)
	}
	next, _ := page1["next"].(string)
	if next == "" {
		t.Fatal("limit=1 时 next 不得为空")
	}
	_, page2 := getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/waves?limit=1&cursor="+next)
	p2, _ := page2["waves"].([]any)
	if len(p2) != 1 || p2[0].(map[string]any)["round_id"] != "rnd_a" {
		t.Fatalf("续读页 = %v", p2)
	}
	if _, hasMore := page2["next"].(string); hasMore && page2["next"] != "" {
		t.Fatal("仅两波时第二页 next 应为空")
	}

	// 坏 cursor → 400
	status, _ = getJSON(t, ts.URL+"/v1/debug/rooms/"+roomID+"/waves?cursor=not-a-seq")
	if status != http.StatusBadRequest {
		t.Fatalf("bad cursor status=%d, want 400", status)
	}
}

// protocolEnvelope / envelopesOf：波事件直落 store 的构造辅助（调试面投影只认
// 事件流，不经引擎——重启后可复盘语义的 UT 面即此）。seq 由 MemStore 自派
// （appendLocked 覆盖），入参仅用于 EventID 去重。
func protocolEnvelope(roomID string, seq int64, typ, actor, kind, payload string) protocol.Envelope {
	return protocol.Envelope{
		EventID: fmt.Sprintf("evt_wv_%d", seq), TenantID: "ten_local", RoomID: roomID,
		Type: typ, SchemaVersion: 1, OccurredAt: "2026-09-01T09:00:00.000Z",
		Actor:   protocol.Actor{ParticipantID: actor, Kind: kind},
		Payload: []byte(payload),
	}
}

func envelopesOf(events []room.StoredEvent) []protocol.Envelope {
	envs := make([]protocol.Envelope, len(events))
	for i, ev := range events {
		envs[i] = ev.Envelope
	}
	return envs
}

// TestTraceIDHeader：命令响应回带 X-Trace-Id；客户端提供时透传，缺省时生成。
func TestTraceIDHeader(t *testing.T) {
	ts, _ := newDevServer(t, false) // trace id 不依赖 dev 开关（排查生产路径同样可用）
	_, resp := createDevRoom(t, ts)
	if got := resp.Header.Get("X-Trace-Id"); got == "" {
		t.Fatal("缺省应生成 X-Trace-Id 响应头")
	}
	resp2 := devPost(t, ts, "/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d0009", "issued_at": "2026-08-30T09:00:03Z",
		"payload": map[string]any{"display_name": "trace"},
	}, "trc_client_supplied")
	if got := resp2.Header.Get("X-Trace-Id"); got != "trc_client_supplied" {
		t.Fatalf("X-Trace-Id = %q, want 透传 trc_client_supplied", got)
	}
}
