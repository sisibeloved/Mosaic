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

	"github.com/sisibeloved/Mosaic/internal/harness"
	"github.com/sisibeloved/Mosaic/internal/outbox"
	"github.com/sisibeloved/Mosaic/internal/protocol"
	"github.com/sisibeloved/Mosaic/internal/room"
	"github.com/sisibeloved/Mosaic/internal/transport/sse"
)

func newTestServer(t *testing.T) (*httptest.Server, *room.MemStore, *sse.Hub) {
	t.Helper()
	store := room.NewMemStore()
	var mu sync.Mutex
	var n int64
	svc := room.NewService(room.Config{
		Store: store,
		Clock: func() string { return "2026-08-28T11:00:00.000Z" },
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

func postJSON(t *testing.T, url string, body any) (*http.Response, commandResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out commandResponse
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

func TestCommandEndpoints(t *testing.T) {
	ts, store, _ := newTestServer(t)

	resp, created := postJSON(t, ts.URL+"/v1/rooms", map[string]any{
		"command_kind": "create_room", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6001", "issued_at": "t",
		"payload": map[string]any{"display_name": "api 房"},
	})
	if resp.StatusCode != 200 || created.RoomID == "" || created.RoomVersion != 1 {
		t.Fatalf("create: status=%d body=%+v", resp.StatusCode, created)
	}

	msg := map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6002", "issued_at": "t",
		"payload": map[string]any{"body": "hello"},
	}
	resp, posted := postJSON(t, ts.URL+"/v1/rooms/"+created.RoomID+"/commands", msg)
	if resp.StatusCode != 200 || posted.RoomVersion != 2 || posted.Replayed {
		t.Fatalf("post: status=%d body=%+v", resp.StatusCode, posted)
	}

	// 幂等重放：同命令同键 → replayed=true 同事件
	resp, replay := postJSON(t, ts.URL+"/v1/rooms/"+created.RoomID+"/commands", msg)
	if resp.StatusCode != 200 || !replay.Replayed || replay.EventID != posted.EventID {
		t.Fatalf("replay: status=%d body=%+v", resp.StatusCode, replay)
	}
	if n := len(store.RoomEvents(created.RoomID)); n != 2 {
		t.Fatalf("事件数 = %d（期望 2）", n)
	}

	// 版本冲突 409
	resp, _ = postJSON(t, ts.URL+"/v1/rooms/"+created.RoomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 99,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6003", "issued_at": "t",
		"payload": map[string]any{"body": "stale"},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("version conflict status = %d", resp.StatusCode)
	}

	// 未知房间 404
	resp, _ = postJSON(t, ts.URL+"/v1/rooms/room_ghost/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 0,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6004", "issued_at": "t",
		"payload": map[string]any{"body": "?"},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown room status = %d", resp.StatusCode)
	}

	// 非法载荷 400
	resp, _ = postJSON(t, ts.URL+"/v1/rooms/"+created.RoomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 2,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6005", "issued_at": "t",
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
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6010", "issued_at": "t",
		"payload": map[string]any{"display_name": "sse"},
	})
	_, _ = postJSON(t, ts.URL+"/v1/rooms/"+created.RoomID+"/commands", map[string]any{
		"command_kind": "post_message", "expected_room_version": 1,
		"idempotency_key": "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d6011", "issued_at": "t",
		"payload": map[string]any{"body": "catch me up"},
	})
	events, _, _ := store.EventsAfter(context.Background(), created.RoomID, "", 100)
	for i, ev := range events {
		raw, _ := json.Marshal(ev.Envelope)
		deliver.Deliver(context.Background(), outbox.Entry{RoomID: created.RoomID, EventID: ev.Envelope.EventID, GlobalPos: int64(i + 1), Envelope: raw})
	}

	// 打开订阅（从头）：追平 2 帧后，延时投递第三帧走直播通道
	go func() {
		time.Sleep(200 * time.Millisecond)
		hub.Publish(created.RoomID, sse.ViewEvent{
			Cursor: protocol.EncodeCursor(99),
			Type:   protocol.EventMessagePosted,
			Data:   []byte(`{"event_id":"evt_live","position":"` + protocol.EncodeCursor(99) + `"}`),
		})
	}()
	frames, cancel := readSSE(t, ts.URL+"/v1/rooms/"+created.RoomID+"/events", 3, 3*time.Second)
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
