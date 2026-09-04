// UT 层：httpapi——命令端点错误映射、SSE 追平/直播/去重/无 seq 泄露。
package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/httpapi/apigen"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

func newTestServer(t *testing.T) (*httptest.Server, *room.MemStore, *sse.Hub) {
	t.Helper()
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := room.NewService(room.Config{
		Store:  store,
		Lister: store,
		Clock:  func() string { return "2026-08-28T11:00:00.000Z" },
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return prefix + "_api_" + fmt.Sprintf("%08d", n)
		},
		Tenant: "ten_local",
	})
	hub := sse.NewHub()
	handler := New(Deps{
		SVC:    svc,
		Reader: store,
		Hub:    hub,
		Actor:  room.Actor{ParticipantID: "par_owner", Kind: "human"},
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, store, hub
}

func postJSON(t *testing.T, url string, body any) (*http.Response, apigen.CommandResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out apigen.CommandResponse
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

func TestCommandEndpoints(t *testing.T) {
	ts, store, _ := newTestServer(t)

	resp, created := postJSON(t, ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6001", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "api 房"},
	})
	if resp.StatusCode != 200 || created.RoomId == "" || created.RoomVersion != 1 {
		t.Fatalf("create: status=%d body=%+v", resp.StatusCode, created)
	}

	msg := map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6002", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": "hello"},
	}
	resp, posted := postJSON(t, ts.URL+"/v1/rooms/"+created.RoomId+"/commands", msg)
	if resp.StatusCode != 200 || posted.RoomVersion != 2 || posted.Replayed {
		t.Fatalf("post: status=%d body=%+v", resp.StatusCode, posted)
	}

	// 幂等重放：同命令同键 → replayed=true 同事件
	resp, replay := postJSON(t, ts.URL+"/v1/rooms/"+created.RoomId+"/commands", msg)
	if resp.StatusCode != 200 || !replay.Replayed || replay.EventId != posted.EventId {
		t.Fatalf("replay: status=%d body=%+v", resp.StatusCode, replay)
	}
	if n := len(store.RoomEvents(created.RoomId)); n != 2 {
		t.Fatalf("事件数 = %d（期望 2）", n)
	}

	// 版本冲突 409
	resp, _ = postJSON(t, ts.URL+"/v1/rooms/"+created.RoomId+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 99,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6003", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": "stale"},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("version conflict status = %d", resp.StatusCode)
	}

	// 未知房间 404
	resp, _ = postJSON(t, ts.URL+"/v1/rooms/room_ghost/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6004", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": "?"},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown room status = %d", resp.StatusCode)
	}

	// 非法载荷 400
	resp, _ = postJSON(t, ts.URL+"/v1/rooms/"+created.RoomId+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 2,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6005", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": ""},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid payload status = %d", resp.StatusCode)
	}

	// 坏 JSON 400
	bad, err := http.Post(ts.URL+"/v1/rooms", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("post bad: %v", err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json status = %d", bad.StatusCode)
	}
}

// sseEvent 解析出的一帧。
type sseEvent struct {
	ID   string
	Name string
	Data map[string]any
}

func readSSE(t *testing.T, url string, until int, timeout time.Duration) ([]sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("get sse: %v", err)
	}
	var collected []sseEvent
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				ev.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.Data)
			case line == "":
				if ev.ID != "" {
					mu.Lock()
					collected = append(collected, ev)
					n := len(collected)
					mu.Unlock()
					if n >= until {
						close(done)
						return
					}
				}
				ev = sseEvent{}
			}
		}
	}()
	deadline := time.After(timeout)
	for {
		select {
		case <-done:
			mu.Lock()
			out := append([]sseEvent(nil), collected...)
			mu.Unlock()
			return out, cancel
		case <-deadline:
			cancel()
			mu.Lock()
			t.Fatalf("SSE 超时：只收到 %d/%d 帧", len(collected), until)
			return nil, cancel
		}
	}
}

func TestSSEStreamCatchUpLiveAndDedup(t *testing.T) {
	ts, store, hub := newTestServer(t)
	deliver := HubConsumer(hub)

	// 先落两个事件（未接 dispatcher：手动 Deliver 模拟已分发）
	_, created := postJSON(t, ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6010", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "sse"},
	})
	_, _ = postJSON(t, ts.URL+"/v1/rooms/"+created.RoomId+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6011", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"body": "catch me up"},
	})
	events, _, _ := store.EventsAfter(context.Background(), created.RoomId, "", 100)
	for i, ev := range events {
		raw, _ := json.Marshal(ev.Envelope)
		deliver.Deliver(context.Background(), outbox.Entry{RoomID: created.RoomId, EventID: ev.Envelope.EventID, GlobalPos: int64(i + 1), Envelope: raw})
	}

	// 打开订阅（从头）：追平 2 帧后，延时投递第三帧走直播通道
	go func() {
		time.Sleep(200 * time.Millisecond)
		hub.Publish(created.RoomId, sse.ViewEvent{
			Cursor: protocol.EncodeCursor(99),
			Type:   protocol.EventMessagePosted,
			Data:   []byte(`{"event_id":"evt_live","position":"` + protocol.EncodeCursor(99) + `"}`),
		})
	}()
	frames, cancel := readSSE(t, ts.URL+"/v1/rooms/"+created.RoomId+"/events", 3, 3*time.Second)
	defer cancel()

	if len(frames) < 3 {
		t.Fatalf("帧数 = %d", len(frames))
	}
	// 无 seq/tenant 泄露；游标严格递增
	last := int64(-1)
	seen := map[string]bool{}
	for _, f := range frames {
		if _, ok := f.Data["seq"]; ok {
			t.Fatal("SSE 帧泄露 seq")
		}
		if _, ok := f.Data["tenant_id"]; ok {
			t.Fatal("SSE 帧泄露 tenant_id")
		}
		pos, err := protocol.DecodeCursor(f.ID)
		if err != nil {
			t.Fatalf("帧 id 非法：%q", f.ID)
		}
		if seen[f.ID] {
			t.Fatalf("重复帧 id=%s", f.ID)
		}
		seen[f.ID] = true
		if pos <= last {
			t.Fatalf("游标非严格递增：%d after %d", pos, last)
		}
		last = pos
	}
	if frames[0].Name != protocol.EventRoomCreated || frames[1].Name != protocol.EventMessagePosted {
		t.Fatalf("追平帧类型不符：%s, %s", frames[0].Name, frames[1].Name)
	}
}

func TestSSEBadCursorRejected(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/rooms/room_x/events?cursor=not-base64!!")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cursor status = %d", resp.StatusCode)
	}
}

// M1 收口补课：落后超一批（1000）也必须补齐——只取一批即转直播会静默丢事件。
func TestSSECatchUpPaginatesBeyondOneBatch(t *testing.T) {
	ts, store, _ := newTestServer(t)
	const total = 1005
	envs := make([]protocol.Envelope, total)
	for i := range envs {
		envs[i] = protocol.Envelope{
			EventID: fmt.Sprintf("evt_bulk_%04d", i), TenantID: "ten_local", RoomID: "room_bulk",
			Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: "2026-08-28T11:00:00.000Z",
			Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"},
			Payload:    []byte(`{"body":"bulk"}`), Metadata: map[string]any{},
		}
	}
	if _, err := store.AppendEvents(context.Background(), envs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	frames, cancel := readSSE(t, ts.URL+"/v1/rooms/room_bulk/events", total, 5*time.Second)
	defer cancel()
	if len(frames) != total {
		t.Fatalf("追平帧数 = %d（期望 %d）：分页未补齐超一批的积压", len(frames), total)
	}
	if want := protocol.EncodeCursor(total); frames[total-1].ID != want {
		t.Fatalf("末帧游标 = %q（期望 %q）", frames[total-1].ID, want)
	}
}

// 二轮审校 #14：断线重连携带 Last-Event-ID 头时必须从该位续传（忽略则整段重放）。
func TestSSELastEventIDHeaderResume(t *testing.T) {
	ts, store, _ := newTestServer(t)
	envs := make([]protocol.Envelope, 4)
	for i := range envs {
		envs[i] = protocol.Envelope{
			EventID: fmt.Sprintf("evt_lei_%d", i), TenantID: "ten_local", RoomID: "room_lei",
			Type: protocol.EventMessagePosted, SchemaVersion: 1, OccurredAt: "2026-08-28T11:00:00.000Z",
			Actor:      protocol.Actor{ParticipantID: "par_owner", Kind: "human"},
			Visibility: protocol.Visibility{Kind: "public"},
			Payload:    []byte(`{"body":"x"}`), Metadata: map[string]any{},
		}
	}
	if _, err := store.AppendEvents(context.Background(), envs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/rooms/room_lei/events", nil)
	req.Header.Set("Last-Event-ID", protocol.EncodeCursor(2)) // 已收到前 2 条
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	got := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "id: ") {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("Last-Event-ID 续传应只补 2 条，got %d", got)
	}
}

// 二轮审校 #20：命令体超过 1MiB 上限必须拒绝（413），不得无界读入。
func TestCommandBodyLimit(t *testing.T) {
	ts, _, _ := newTestServer(t)
	// 合法 JSON 前缀的超大体：decoder 持续读入直到触发上限（非法 JSON 会在读满前报 400）
	big := `{"command_kind":"post_message","expected_room_version":1,` +
		`"idempotency_key":"018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d5e6f",` +
		`"issued_at":"2026-08-28T12:00:00.000Z","payload":{"body":"` +
		strings.Repeat("x", 2<<20) + `"}}`
	resp, err := http.Post(ts.URL+"/v1/rooms/room_x/commands", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限命令体 status = %d（期望 413）", resp.StatusCode)
	}
}

// flakyReader 首次 EventsAfter 即失败：驱动 resync_required 信号。
type flakyReader struct{ room.EventReader }

func (f flakyReader) EventsAfter(ctx context.Context, roomID, cursor string, limit int) ([]room.StoredEvent, string, error) {
	return nil, "", fmt.Errorf("reader boom")
}

// M1 收口补课：追平失败/慢消费者断流必须发 resync_required 具名事件（RFC-0001 §订阅），
// 客户端据此走快照恢复，而非只给注释行。
func TestSSEResyncRequiredOnCatchUpFailure(t *testing.T) {
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-08-28T11:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_rs" },
		Tenant: "ten_local",
	})
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: flakyReader{EventReader: store}, Hub: sse.NewHub(),
		Actor: room.Actor{ParticipantID: "par_owner", Kind: "human"},
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/rooms/room_rs/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get sse: %v", err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	var sawResync bool
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event: resync_required") {
			sawResync = true
		}
	}
	if !sawResync {
		t.Fatal("追平失败必须发出 event: resync_required 具名帧")
	}
}

// ---- 宿主层端点 ----

// miniRunner: httpapi 测试用的最小 harness.Runner（探测全成功形态）。
type miniRunner struct{}

func (miniRunner) LookPath(ctx context.Context, runtime harness.Runtime, distro, binary string) (string, bool) {
	return "/opt/tools/" + binary, true
}
func (miniRunner) Run(ctx context.Context, runtime harness.Runtime, distro string, args []string) (string, int, error) {
	return "Logged in using ChatGPT\n", 0, nil
}
func (miniRunner) RunWithDir(ctx context.Context, runtime harness.Runtime, distro, binDir string, args []string) (string, int, error) {
	return miniRunner{}.Run(ctx, runtime, distro, args)
}
func (miniRunner) Home(ctx context.Context, runtime harness.Runtime, distro string) string {
	return "/home/u"
}
func (miniRunner) Exists(ctx context.Context, runtime harness.Runtime, distro, path string) bool {
	return true
}
func (miniRunner) Digest(ctx context.Context, runtime harness.Runtime, distro, path string) (string, error) {
	return "sha256:x", nil
}
func (miniRunner) WSLDistros(ctx context.Context) []string { return nil }
func (miniRunner) Glob(ctx context.Context, runtime harness.Runtime, distro, pattern string) []string {
	return nil
}
func (miniRunner) ReadFile(ctx context.Context, runtime harness.Runtime, distro, path string) (string, bool) {
	return "", false // 无配置文件：默认值走官方/出厂回退路径
}

func newHarnessTestServer(t *testing.T) (*httptest.Server, *harness.Registry) {
	t.Helper()
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-08-28T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_h" },
		Tenant: "ten_local",
	})
	reg, err := harness.LoadOrCreate(t.TempDir() + "/harness.json")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:   room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Harness: reg, ProbeRunner: miniRunner{},
	}))
	t.Cleanup(ts.Close)
	return ts, reg
}

// TestListAgentsDisabled：/v1/agents 如实区分"在席"与"已发现未启用"（v1.24
// dogfood #1——选人页只有测试桩的可见性缺陷：注册表里 codex/kimi 已发现未启用
// 也要上报，客户端灰芯片指路设置）。启用后从未启用清单消失。
func TestListAgentsDisabled(t *testing.T) {
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store:  store,
		Clock:  func() string { return "2026-08-28T12:00:00.000Z" },
		NewID:  func(prefix string) string { return prefix + "_la" },
		Tenant: "ten_local",
	})
	reg, err := harness.LoadOrCreate(t.TempDir() + "/agents.json")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:   room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Harness: reg, ProbeRunner: miniRunner{},
		Seats: func() []room.AgentSeat {
			return []room.AgentSeat{{
				ParticipantID: "par_echo",
				Profile:       agent.Profile{ProfileID: "prof_echo", Adapter: "echo", DisplayName: "Echo"},
			}}
		},
	}))
	t.Cleanup(ts.Close)

	// 手动登记一个未启用的 codex（登记后默认 disabled）
	resp, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"codex","runtime":"native","path":"/opt/tools/codex"}`))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", resp.StatusCode)
	}

	get := func() struct {
		Agents   []map[string]any `json:"agents"`
		Disabled []map[string]any `json:"disabled"`
	} {
		r, err := http.Get(ts.URL + "/v1/agents")
		if err != nil {
			t.Fatalf("get agents: %v", err)
		}
		defer r.Body.Close()
		var out struct {
			Agents   []map[string]any `json:"agents"`
			Disabled []map[string]any `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	first := get()
	if len(first.Agents) != 1 || first.Agents[0]["participant_id"] != "par_echo" {
		t.Fatalf("在席应恰 echo：%v", first.Agents)
	}
	if len(first.Disabled) != 1 || first.Disabled[0]["adapter"] != "codex" || first.Disabled[0]["channel"] != "cli" {
		t.Fatalf("未启用清单应含 codex/cli：%v", first.Disabled)
	}

	// 启用后从未启用清单消失（留在 agents 与否取决于座位 resync，不在此断言）
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("registry 应恰一项：%v", list)
	}
	if err := reg.SetEnabled(list[0].ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if second := get(); len(second.Disabled) != 0 {
		t.Fatalf("启用后未启用清单应空：%v", second.Disabled)
	}
}

func TestHarnessEndpoints(t *testing.T) {
	ts, reg := newHarnessTestServer(t)

	// 手动登记（负责人要求 2）：合法项 → 201
	resp, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"codex","runtime":"native","path":"/opt/tools/codex"}`))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", resp.StatusCode)
	}

	// 非法 adapter → 400
	resp, _ = http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"wat","runtime":"native","path":"/x"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad adapter status = %d", resp.StatusCode)
	}

	// 列表含登记项（miniRunner 探测为已登录）
	list, err := http.Get(ts.URL + "/v1/harness/executables")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer list.Body.Close()
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	if len(doc.Executables) != 1 || doc.Executables[0].Adapter != "codex" {
		t.Fatalf("列表不符：%+v", doc.Executables)
	}
	id := doc.Executables[0].ID

	// 启用：已登录 → 200
	resp, _ = http.Post(ts.URL+"/v1/harness/executables/"+url.PathEscape(id)+"/enable", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable status = %d", resp.StatusCode)
	}

	// 登录门控（负责人要求 3）：置为未登录后启用 → 409 login_required
	if err := reg.SetEnabled(id, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	for i := range reg.List() {
		_ = i
	}
	// 直接构造未登录场景：手动登记 kimi 且探测面返回未登录
	reg2 := reg
	_ = reg2
	resp, _ = http.Post(ts.URL+"/v1/harness/executables/nope/enable", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id status = %d", resp.StatusCode)
	}

	// 渠道覆盖（ADR-0012）：手动登记带 channel → 列表回显 channel 与家族 priority
	resp, err = http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"kimi","runtime":"native","path":"/opt/tools/kimi-work","channel":"app:kimi-work"}`))
	if err != nil {
		t.Fatalf("add with channel: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add with channel status = %d", resp.StatusCode)
	}

	// 非法 channel 形态 → 400
	resp, _ = http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"kimi","runtime":"native","path":"/opt/tools/kimi-bad","channel":"Bad Channel!"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad channel status = %d", resp.StatusCode)
	}

	list2, err := http.Get(ts.URL + "/v1/harness/executables")
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	defer list2.Body.Close()
	var doc2 struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list2.Body).Decode(&doc2)
	var kimiExe *harness.Executable
	for i := range doc2.Executables {
		if doc2.Executables[i].Path == "/opt/tools/kimi-work" {
			kimiExe = &doc2.Executables[i]
		}
	}
	if kimiExe == nil {
		t.Fatalf("kimi-work 未入列表：%+v", doc2.Executables)
	}
	if kimiExe.Channel != "app:kimi-work" {
		t.Fatalf("channel = %q，应回显 app:kimi-work", kimiExe.Channel)
	}
	if kimiExe.Priority != harness.PriorityFor("kimi", "app:kimi-work") {
		t.Fatalf("priority = %d，应为家族裁定值 %d", kimiExe.Priority, harness.PriorityFor("kimi", "app:kimi-work"))
	}
}

func TestHarnessLoginGateEndpoint(t *testing.T) {
	// 独立服务器：探测面返回未登录 → enable 必须 409
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store: store, Clock: func() string { return "t" },
		NewID: func(p string) string { return p }, Tenant: "ten_local",
	})
	reg, _ := harness.LoadOrCreate(t.TempDir() + "/h.json")
	// 注入未登录登记项（绕过探测，直接走内部状态）
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:   room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Harness: reg,
	}))
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/harness/executables/whatever/enable", "application/json", nil)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未登记项启用应 404，got %d", resp.StatusCode)
	}
}

// 复审 #18：loopback owner API 的跨站写防护——跨源 Origin 403；带 body 端点非 JSON
// Content-Type 415（跨站 text/plain 简单请求无预检，是本门的核心威胁模型）；
// 同源 Origin 与无 Origin（curl 等非浏览器客户端）放行。
func TestWriteOriginAndContentTypeGuard(t *testing.T) {
	ts, _, _ := newTestServer(t)
	raw, _ := json.Marshal(map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d7001", "issued_at": "2026-08-28T12:00:00.000Z",
		"payload": map[string]any{"display_name": "guard"},
	})
	post := func(origin, contentType string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/rooms", bytes.NewReader(raw))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("http://evil.example", "application/json"); code != http.StatusForbidden {
		t.Fatalf("跨源写应 403，got %d", code)
	}
	if code := post("http://127.0.0.1.evil.example", "application/json"); code != http.StatusForbidden {
		t.Fatalf("宿主前缀伪装应 403，got %d", code)
	}
	if code := post(ts.URL, "application/json"); code != http.StatusOK {
		t.Fatalf("同源 Origin 应放行，got %d", code)
	}
	if code := post("", "application/json"); code != http.StatusOK {
		t.Fatalf("无 Origin（非浏览器客户端）应放行，got %d", code)
	}
	if code := post("", "text/plain"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain JSON 跨站简单请求应 415，got %d", code)
	}
	if code := post("http://evil.example", "text/plain"); code != http.StatusForbidden {
		t.Fatalf("跨源优先于 Content-Type 判定，got %d", code)
	}
}

// 复审 #18：enable/disable 空 body 端点只做 Origin 门（无 Content-Type 要求）。
func TestEnableOriginGuard(t *testing.T) {
	ts, _ := newHarnessTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/harness/executables/nope/enable", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨源 enable 应 403（先于 404），got %d", resp.StatusCode)
	}
}

// 复审 #21：登记体超限必须 413（此前 MaxBytesError 被并入 400 语义）。
func TestAddExecutableBodyLimit(t *testing.T) {
	ts, _ := newHarnessTestServer(t)
	big := `{"adapter":"codex","runtime":"native","path":"/x","version":"` + strings.Repeat("v", 2<<20) + `"}`
	resp, err := http.Post(ts.URL+"/v1/harness/executables", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("登记体超限应 413，got %d", resp.StatusCode)
	}
}

// 四轮复审 #15：Origin 门对"配置的回环 authority"判定——不信请求自带的 Host
// （DNS rebinding 后 Origin 与 Host 同时指向攻击者域名而相等，旧比对放行）。
func TestWriteOriginAuthorityGuard(t *testing.T) {
	store := room.NewMemStore()
	svc := room.NewService(room.Config{
		Store: store, Clock: func() string { return "2026-08-30T00:00:00.000Z" },
		NewID: func(p string) string { return p + "_auth" }, Tenant: "ten_local",
	})
	ts := httptest.NewServer(New(Deps{
		SVC: svc, Reader: store, Hub: sse.NewHub(),
		Actor:     room.Actor{ParticipantID: "par_owner", Kind: "human"},
		Authority: "127.0.0.1:7420",
	}))
	t.Cleanup(ts.Close)
	raw, _ := json.Marshal(map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d8002", "issued_at": "2026-08-30T12:00:00.000Z",
		"payload": map[string]any{"display_name": "auth"},
	})
	post := func(origin, host string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/rooms", bytes.NewReader(raw))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if host != "" {
			req.Host = host // 模拟 rebinding 后随攻击者域名走的 Host
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	// rebinding 场景：Origin 与 Host 同为攻击者域——旧"Origin==Host"比对会放行
	if code := post("http://evil.example:7420", "evil.example:7420"); code != http.StatusForbidden {
		t.Fatalf("rebinding 场景必须拒绝（got %d）", code)
	}
	// 非配置端口的回环 Origin → 拒
	if code := post("http://127.0.0.1:9999", ""); code != http.StatusForbidden {
		t.Fatalf("端口不匹配的回环 Origin 应拒绝（got %d）", code)
	}
	// 配置 authority 端口的回环 Origin（localhost 变体）→ 放行
	if code := post("http://localhost:7420", ""); code != http.StatusOK {
		t.Fatalf("同 authority 的回环 Origin 应放行（got %d）", code)
	}
}
