// UT 层：ADR-0007 契约测试回填——api/http-api/openapi.yaml（权威源文件直读）
// 驱动的外部契约走查。操作集一致性已由 ServerInterface 编译期保证（漏实现即
// 编译失败）；本测试钉住三件 spec 与装配的运行期一致性：
//  1. spec 每条 path+method 都被路由（命中处理器而非 mux 默认 404）；
//  2. spec 的 command_kind 枚举与服务端实际受理集一致（漂移即红）；
//  3. spec 请求例可被受理，响应字段集与 CommandResponse 契约一致。
package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// loadSpec 读权威源 openapi.yaml（不经生成产物转手——内嵌 spec 的重序列化
// 跨环境不稳定，CI 漂移门禁实证过；契约测试只信源文件）。
func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(filepath.Join("..", "..", "..", "api", "http-api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("加载 openapi.yaml: %v", err)
	}
	return doc
}

// muxUnrouted 判定响应是否为 Go ServeMux 的默认 404（未注册路由的形态：
// text/plain 的 "404 page not found"——与业务 404 的 JSON 体区分）。
// 只读首字节：SSE 长流首帧即有内容，多读会等心跳（90s 级）。
func muxUnrouted(t *testing.T, resp *http.Response) bool {
	t.Helper()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1))
	resp.Body.Close()
	return resp.StatusCode == http.StatusNotFound &&
		resp.Header.Get("Content-Type") == "text/plain; charset=utf-8" &&
		len(body) == 1 && body[0] == '4'
}

// TestSpecRouteParity：spec 声明的每条操作都必须命中已装配的处理器。
func TestSpecRouteParity(t *testing.T) {
	ts, _, _ := newTestServer(t)
	doc := loadSpec(t)
	client := &http.Client{}
	checked := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op == nil {
				continue
			}
			var resp *http.Response
			var err error
			if method == http.MethodGet {
				resp, err = client.Get(ts.URL + path)
			} else {
				// 写面：application/json + 空 JSON——守门/校验会拒（400/415/503），
				// 但都证明路由命中；只有未注册才会落 mux 默认 404。
				resp, err = client.Post(ts.URL+path, "application/json", bytes.NewReader([]byte(`{}`)))
			}
			if err != nil {
				t.Fatalf("%s %s: 请求失败: %v", method, path, err)
			}
			// SSE 端点读取首字节即关流（长连接不等待）
			if muxUnrouted(t, resp) {
				t.Errorf("%s %s 未被路由（spec 声明了该操作，装配缺失）", method, path)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("spec 未解析出任何操作")
	}
}

// TestSpecCommandKindEnum：spec 枚举 == 服务端受理集。
func TestSpecCommandKindEnum(t *testing.T) {
	doc := loadSpec(t)
	sch, ok := doc.Components.Schemas["RoomCommand"]
	if !ok || sch.Value == nil {
		t.Fatal("spec 缺 RoomCommand schema")
	}
	prop, ok := sch.Value.Properties["command_kind"]
	if !ok || prop.Value == nil || len(prop.Value.Enum) == 0 {
		t.Fatal("RoomCommand.command_kind 缺枚举")
	}
	got := map[string]bool{}
	for _, v := range prop.Value.Enum {
		s, _ := v.(string)
		got[s] = true
	}
	want := map[string]bool{"create_room": true, "post_message": true, "pause_room": true, "resume_room": true, "set_policy": true}
	if len(got) != len(want) {
		t.Fatalf("command_kind 枚举漂移：spec=%v 服务端=%v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("command_kind 枚举漂移：spec=%v 缺 %q", got, k)
		}
	}
}

// specExample 提取操作的 application/json 请求例（example 或具名 examples）。
func specExample(t *testing.T, op any) map[string]any {
	t.Helper()
	type opLike struct {
		RequestBody any
	}
	// 经 json 往返泛化访问（kin-openapi 结构随版本变动，泛化解耦）
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal op: %v", err)
	}
	var o opLike
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("unmarshal op: %v", err)
	}
	rb, _ := json.Marshal(o.RequestBody)
	var body struct {
		Content map[string]struct {
			Example  any            `json:"example"`
			Examples map[string]any `json:"examples"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rb, &body); err != nil {
		t.Fatalf("unmarshal requestBody: %v", err)
	}
	ct, ok := body.Content["application/json"]
	if !ok {
		t.Fatal("操作缺 application/json 请求体定义")
	}
	if ct.Example != nil {
		ex, _ := ct.Example.(map[string]any)
		return ex
	}
	// 具名例取第一个
	for _, v := range ct.Examples {
		ev, _ := v.(map[string]any)
		if val, ok := ev["value"]; ok {
			ex, _ := val.(map[string]any)
			return ex
		}
	}
	return nil
}

// TestSpecRequestExamplesAccepted：spec 请求例（版本位校准后）全链受理，
// 响应字段集与 CommandResponse 契约一致（不得多键走私/少键）。
func TestSpecRequestExamplesAccepted(t *testing.T) {
	ts, _, _ := newTestServer(t)
	doc := loadSpec(t)

	post := func(path string, body map[string]any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// create_room 例：原样受理
	createEx := specExample(t, doc.Paths.Map()["/v1/rooms"].Post)
	if createEx == nil {
		t.Fatal("create_room 缺请求例")
	}
	status, created := post("/v1/rooms", createEx)
	if status != http.StatusOK {
		t.Fatalf("create_room 例被拒：%d %v", status, created)
	}
	assertCommandResponseShape(t, created)

	roomID, _ := created["room_id"].(string)
	version, _ := created["room_version"].(float64)

	// post_message 例：校准 expected_room_version（例中为示意值）
	postEx := specExample(t, doc.Paths.Map()["/v1/rooms/{room_id}/commands"].Post)
	if postEx == nil {
		t.Fatal("post_message 缺请求例")
	}
	postEx["expected_room_version"] = int64(version)
	status, posted := post("/v1/rooms/"+roomID+"/commands", postEx)
	if status != http.StatusOK {
		t.Fatalf("post_message 例被拒：%d %v", status, posted)
	}
	assertCommandResponseShape(t, posted)
	version, _ = posted["room_version"].(float64)

	// pause_room 例（同操作的第二个具名例手动构造：从 post 例改写）
	pauseEx := map[string]any{
		"command_kind":          "pause_room",
		"expected_room_version": int64(version),
		"idempotency_key":       "018f6b2e-7c1a-7b3d-9e4f-1a2b3c4d9103",
		"issued_at":             "2026-08-30T09:31:00.000Z",
		"payload":               map[string]any{"reason": "契约测试暂停"},
	}
	status, paused := post("/v1/rooms/"+roomID+"/commands", pauseEx)
	if status != http.StatusOK {
		t.Fatalf("pause_room 例被拒：%d %v", status, paused)
	}
	assertCommandResponseShape(t, paused)
}

func assertCommandResponseShape(t *testing.T, body map[string]any) {
	t.Helper()
	want := map[string]bool{"room_id": true, "event_id": true, "room_version": true, "replayed": true}
	if len(body) != len(want) {
		t.Fatalf("CommandResponse 字段集漂移：%v", body)
	}
	for k := range want {
		if _, ok := body[k]; !ok {
			t.Fatalf("CommandResponse 缺 %q：%v", k, body)
		}
	}
}
