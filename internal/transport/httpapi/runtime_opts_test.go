// v1.48 运行参数端点 UT：PUT 更新（模型/强度校验与清除）+ GET 模型候选
// （codex 五档静态空候选；kimi 实查路径的解析在 harness 包 UT 覆盖——
// miniRunner 场景此处只验端点接线）。
package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sisibeloved/Mosaic/internal/harness"
)

func runtimeTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts, reg := newHarnessTestServer(t)
	// 手动登记一个 codex 实例（同 TestHarnessEndpoints 路径：miniRunner 探测通过）
	resp, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"codex","runtime":"native","path":"/opt/tools/codex"}`))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", resp.StatusCode)
	}
	_ = reg
	list, _ := http.Get(ts.URL + "/v1/harness/executables")
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	if len(doc.Executables) == 0 {
		t.Fatal("登记后列表为空")
	}
	return ts, doc.Executables[0].ID
}

func TestRuntimeUpdateEndpoint(t *testing.T) {
	ts, id := runtimeTestServer(t)

	put := func(target, body string) int {
		req, _ := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	base := ts.URL + "/v1/harness/executables/" + url.PathEscape(id)

	// 合法更新
	if code := put(base, `{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}`); code != http.StatusOK {
		t.Fatalf("合法更新 = %d", code)
	}
	// 非法档位 → 400
	if code := put(base, `{"model":"m","reasoning_effort":"ultra"}`); code != http.StatusBadRequest {
		t.Fatalf("非法档位 = %d", code)
	}
	// 未知字段 → 400（严格解码纪律）
	if code := put(base, `{"model":"m","reasoning_effort":"","extra":1}`); code != http.StatusBadRequest {
		t.Fatalf("未知字段 = %d", code)
	}
	// 清除覆盖 → 200
	if code := put(base, `{"model":"","reasoning_effort":""}`); code != http.StatusOK {
		t.Fatalf("清除 = %d", code)
	}
	// 不存在 → 404
	if code := put(ts.URL+"/v1/harness/executables/nope", `{}`); code != http.StatusNotFound {
		t.Fatalf("不存在 = %d", code)
	}
	// 列表透出当前值（更新 → 列表）
	if code := put(base, `{"model":"gpt-5.6-sol","reasoning_effort":"high"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	list, _ := http.Get(ts.URL + "/v1/harness/executables")
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	if doc.Executables[0].Model != "gpt-5.6-sol" || doc.Executables[0].ReasoningEffort != "high" {
		t.Fatalf("列表应透出运行参数: %+v", doc.Executables[0])
	}
}

func TestRuntimeModelsEndpoint(t *testing.T) {
	ts, id := runtimeTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/harness/executables/" + url.PathEscape(id) + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models 状态 = %d", resp.StatusCode)
	}
	var opts harness.RuntimeOptions
	_ = json.NewDecoder(resp.Body).Decode(&opts)
	if opts.Dynamic || len(opts.Models) != 0 {
		t.Fatalf("codex 应空候选非动态: %+v", opts)
	}
	if len(opts.EffortLevels) != 5 || opts.EffortLevels[4] != "xhigh" {
		t.Fatalf("codex 五档: %+v", opts.EffortLevels)
	}
	// v1.49 确定量默认：miniRunner 无配置文件 → 官方回退（强度 medium；模型为
	// CLI 内置预设，不虚构）
	if opts.DefaultModel != "" || opts.DefaultEffort != "medium" || opts.DefaultSource != "builtin" {
		t.Fatalf("codex 默认值应官方回退: %+v", opts)
	}
	// 不存在 → 404
	resp404, _ := http.Get(ts.URL + "/v1/harness/executables/nope/models")
	resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在 = %d", resp404.StatusCode)
	}
}

// 0.4.1 版本检测与更新端点接线：GET update-check（miniRunner 输出非 semver →
// 200 + note 面如实现）；POST update（同步收口 + 版本回填注册表）；kimi manual
// 家族 409 no_update_channel；不存在 404。
func TestUpdateCheckEndpoint(t *testing.T) {
	ts, id := runtimeTestServer(t)
	base := ts.URL + "/v1/harness/executables/" + url.PathEscape(id)

	resp, err := http.Get(base + "/update-check")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("check status = %d", resp.StatusCode)
	}
	var check harness.UpdateCheck
	if err := json.NewDecoder(resp.Body).Decode(&check); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if check.Adapter != "codex" || check.Channel != "registry" || !check.CanCheck || !check.CanUpdate {
		t.Fatalf("codex 检测字段不符：%+v", check)
	}
	// miniRunner 固定输出非 semver——不可解析面如实呈现（latest 空 + note）
	if check.Latest != "" || check.Note == "" {
		t.Fatalf("查询输出不可解析应携 note 不带 latest：%+v", check)
	}

	// 不存在 → 404
	resp2, _ := http.Get(ts.URL + "/v1/harness/executables/nope/update-check")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在 = %d", resp2.StatusCode)
	}
}

func TestUpdateRunEndpoint(t *testing.T) {
	ts, id := runtimeTestServer(t)
	base := ts.URL + "/v1/harness/executables/" + url.PathEscape(id)

	// 异步受理：202 + started
	resp, err := http.Post(base+"/update", "", nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var startDoc struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&startDoc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || (startDoc.Status != "started" && startDoc.Status != "running") {
		t.Fatalf("update 受理 = %d %+v", resp.StatusCode, startDoc)
	}

	// 过程日志 SSE：重放 + 实时直至终态（miniRunner 秒回——done 后服务端关流，
	// 客户端读到 EOF）
	sreq, _ := http.NewRequest(http.MethodGet, base+"/update/stream", nil)
	sresp, err := (&http.Client{}).Do(sreq)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	body, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK || sresp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream 受理 = %d %s", sresp.StatusCode, sresp.Header.Get("Content-Type"))
	}
	text := string(body)
	if !strings.Contains(text, "\"kind\":\"line\"") || !strings.Contains(text, "\"kind\":\"done\"") {
		t.Fatalf("SSE 应含过程行与终态：%s", truncateFor(text, 200))
	}

	// 版本回填注册表（异步任务完成后；轮询容限）
	var updated string
	for i := 0; i < 50; i++ {
		list, _ := http.Get(ts.URL + "/v1/harness/executables")
		var doc struct {
			Executables []harness.Executable `json:"executables"`
		}
		_ = json.NewDecoder(list.Body).Decode(&doc)
		list.Body.Close()
		for _, exe := range doc.Executables {
			if exe.ID == id && exe.Version != "" {
				updated = exe.Version
			}
		}
		if updated != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if updated == "" {
		t.Fatal("更新后版本未回填注册表")
	}
}

func truncateFor(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func TestUpdateRunEndpointMosaicScript(t *testing.T) {
	ts, _ := runtimeTestServer(t)
	// 手动登记 zcode（mosaic 通道——miniRunner Home=/home/u 且 Exists 恒真：
	// 约定脚本 ~/.mosaic/update/zcode.sh 视为在位，更新任务受理并经脚本面收口）
	resp, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"zcode","runtime":"native","path":"/opt/tools/zcode"}`))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", resp.StatusCode)
	}
	list, _ := http.Get(ts.URL + "/v1/harness/executables")
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	var zcodeID string
	for _, exe := range doc.Executables {
		if exe.Adapter == "zcode" {
			zcodeID = exe.ID
		}
	}
	if zcodeID == "" {
		t.Fatal("zcode 未登记")
	}
	base := ts.URL + "/v1/harness/executables/" + url.PathEscape(zcodeID)
	resp2, err := http.Post(base+"/update", "", nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("mosaic 脚本在位应受理（202）= %d", resp2.StatusCode)
	}
	// 过程流收口（miniRunner 秒回——done 后关流读到 EOF）
	sreq, _ := http.NewRequest(http.MethodGet, base+"/update/stream", nil)
	sresp, err := (&http.Client{}).Do(sreq)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	body, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	if !strings.Contains(string(body), "\"kind\":\"done\"") {
		t.Fatalf("SSE 应含终态：%s", truncateFor(string(body), 200))
	}
}
